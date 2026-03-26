/**
 * Codex 执行器模块
 * 负责向 Codex API 发送请求并处理响应
 * 支持流式和非流式两种模式，处理认证头注入、错误处理和重试
 */
package executor

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"codex-proxy/internal/auth"
	"codex-proxy/internal/netutil"
	"codex-proxy/internal/thinking"
	"codex-proxy/internal/translator"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

/* Codex 客户端版本常量，用于请求头 */
const (
	codexClientVersion = "0.101.0"
	codexUserAgent     = "codex_cli_rs/0.101.0 (Mac OS 26.0.1; arm64) Apple_Terminal/464"
)

/* 预分配 SSE 输出字节片段，避免每次事件的内存分配 */
var (
	sseDataPrefix    = []byte("data: ")
	sseDataSuffix    = []byte("\n\n")
	sseDoneMarker    = []byte("data: [DONE]\n\n")
	ErrEmptyResponse = errors.New("empty response")
)

/* 与 handler 一致的缓冲与扫描器大小，便于统一调优 */
const (
	httpBufferSize              = 32 * 1024
	scannerInitSize             = 4 * 1024
	scannerMaxSize              = 50 * 1024 * 1024
	defaultKeepaliveIntervalSec = 60
)

type HTTPPoolConfig struct {
	MaxIdleConns         int
	MaxIdleConnsPerHost  int
	MaxConnsPerHost      int
	EnableHTTP2          bool
	BackendDomain        string
	ResolveAddress       string
	KeepaliveIntervalSec int /* 连接保活间隔（秒），0 使用默认 60 */
}

/**
 * Executor Codex 请求执行器
 * 使用全局共享连接池提升高并发性能
 * @field baseURL - Codex API 基础 URL
 * @field httpClient - 共享的 HTTP 客户端（连接池复用）
 */
type Executor struct {
	baseURL              string
	httpClient           *http.Client
	keepAliveOnce        sync.Once
	resolveAddr          string
	keepaliveIntervalSec int
}

/**
 * NewExecutor 创建新的 Codex 执行器（仅用于 /responses 对话转发）
 * 出站 Dial/Transport/Client 不设超时，避免用户长对话或 SSE 被掐断；健康检查、刷新、额度等走 auth 包独立 Client。
 */
func NewExecutor(baseURL, proxyURL string, poolCfg HTTPPoolConfig) *Executor {
	if baseURL == "" {
		baseURL = "https://chatgpt.com/backend-api/codex"
	}
	maxIdleConns := poolCfg.MaxIdleConns
	if maxIdleConns < 0 {
		maxIdleConns = 0
	}
	maxIdleConnsPerHost := poolCfg.MaxIdleConnsPerHost
	if maxIdleConnsPerHost < 0 {
		maxIdleConnsPerHost = 0
	}
	maxConnsPerHost := poolCfg.MaxConnsPerHost
	if maxConnsPerHost < 0 {
		maxConnsPerHost = 0
	}
	enableHTTP2 := poolCfg.EnableHTTP2
	dialer := &net.Dialer{
		Timeout:   0,
		KeepAlive: 60 * time.Second,
	}
	dialCtx := netutil.BuildUpstreamDialContext(dialer, proxyURL, poolCfg.BackendDomain, poolCfg.ResolveAddress)

	transport := netutil.NewUpstreamTransport(netutil.UpstreamTransportConfig{
		DialContext:         dialCtx,
		ProxyURL:            proxyURL,
		MaxIdleConns:        maxIdleConns,
		MaxIdleConnsPerHost: maxIdleConnsPerHost,
		MaxConnsPerHost:     maxConnsPerHost,
		EnableHTTP2:         enableHTTP2,
		WriteBufferSize:     httpBufferSize,
		ReadBufferSize:      httpBufferSize,
		DisableCompression:  true,
	})
	if proxyURL != "" {
		if proxyScheme, err := netutil.ParseProxyScheme(proxyURL); err == nil {
			switch proxyScheme {
			case "socks5", "socks5h":
				log.Infof("已启用 SOCKS5 代理: %s", proxyURL)
			case "http", "https":
				log.Infof("已启用 HTTP/HTTPS 代理: %s", proxyURL)
			}
		}
	}

	keepaliveSec := poolCfg.KeepaliveIntervalSec
	if keepaliveSec <= 0 {
		keepaliveSec = defaultKeepaliveIntervalSec
	}
	return &Executor{
		baseURL: strings.TrimSuffix(baseURL, "/"),
		httpClient: &http.Client{
			Transport: transport,
			Timeout:   0, /* 对话 API：不在 Client 层限制整段请求+读 body */
		},
		resolveAddr:          strings.TrimSpace(poolCfg.ResolveAddress),
		keepaliveIntervalSec: keepaliveSec,
	}
}

/**
 * RetryConfig 内部重试配置
 * @field EmptyRetryMax - 空返回时最多再重试次数
 * @field QuotaCheckFn - 选号后检查额度的回调（可选）；返回 false 表示额度无效/未知，需继续换号
 */
type RetryConfig struct {
	PickFn                func(model string, excluded map[string]bool) (*auth.Account, error)
	HealthyPickFn         func(model string, excluded map[string]bool) (*auth.Account, error)
	HealthyPickMinAttempt int /* 从第几次尝试起（0-based）改用 HealthyPickFn；0 表示主循环中不用；通常为 max-retry-1 */
	FallbackRecentPickFn  func(model string, excluded map[string]bool) (*auth.Account, error)
	/* On401Fn 返回 true 表示已换发新 access_token（或 429 后额度已恢复），调用方应对同一账号立即重发上游请求 */
	On401Fn              func(acc *auth.Account) bool
	On429RecoveryFn      func(ctx context.Context, acc *auth.Account)
	OnAfterUpstreamErrFn func(acc *auth.Account, statusCode int)
	QuotaCheckFn         func(ctx context.Context, acc *auth.Account) bool /* 返回 true 表示额度有效，false 表示无效需换号 */
	MaxRetry             int
	EmptyRetryMax        int
}

/**
 * MergeRetryConfigExcluded 将 extra 中的账号路径并入每次选号时的 excluded 映射。
 * 用于空响应等场景下换号重试：须与 PickFn 同样作用于 HealthyPickFn / FallbackRecentPickFn，否则会再次选到应排除的号。
 */
func MergeRetryConfigExcluded(rc RetryConfig, extra map[string]bool) RetryConfig {
	if len(extra) == 0 {
		return rc
	}
	merge := func(excl map[string]bool) {
		for k := range extra {
			excl[k] = true
		}
	}
	out := rc
	out.PickFn = func(m string, excl map[string]bool) (*auth.Account, error) {
		merge(excl)
		return rc.PickFn(m, excl)
	}
	if rc.HealthyPickFn != nil {
		h := rc.HealthyPickFn
		out.HealthyPickFn = func(m string, excl map[string]bool) (*auth.Account, error) {
			merge(excl)
			return h(m, excl)
		}
	}
	if rc.FallbackRecentPickFn != nil {
		f := rc.FallbackRecentPickFn
		out.FallbackRecentPickFn = func(m string, excl map[string]bool) (*auth.Account, error) {
			merge(excl)
			return f(m, excl)
		}
	}
	return out
}

/* 上游 HTTP/2 GOAWAY ENHANCE_YOUR_CALM 时的错误特征，用于日志与提示 */
const enhanceYourCalmHint = "（上游限流：可调低 max-conns-per-host / max-idle-conns-per-host 或关闭 enable-http2）"

/* errCodexBuildRequest 标记创建上游 HTTP 请求失败，调用方不再换号重试 */
var errCodexBuildRequest = errors.New("codex: build http request")

/**
 * wrapReadErr 若为 HTTP/2 GOAWAY ENHANCE_YOUR_CALM，附加说明便于排查
 */
func wrapReadErr(err error) error {
	if err == nil {
		return nil
	}
	s := err.Error()
	if strings.Contains(s, "GOAWAY") && strings.Contains(s, "ENHANCE_YOUR_CALM") {
		return fmt.Errorf("%w %s", err, enhanceYourCalmHint)
	}
	return err
}

/* isRetryableUpstreamReadErr 响应体读取阶段遇连接被掐、GOAWAY 等，可换号/重建连接再试（Do() 已成功时 sendWithRetry 无法覆盖） */
func isRetryableUpstreamReadErr(err error) bool {
	if err == nil {
		return false
	}
	for e := err; e != nil; e = errors.Unwrap(e) {
		if netutil.IsRetryableUpstreamNetError(e) {
			return true
		}
	}
	s := err.Error()
	return strings.Contains(s, "GOAWAY") || strings.Contains(s, "ENHANCE_YOUR_CALM")
}

/**
 * IsRetryableStatus 判断 HTTP 状态码是否可重试（切换账号重试）
 * 403（地域封锁 / Cloudflare 拦截）换账号也无法解决，不重试
 * 400（参数/模型错误）也不重试
 * 401（认证失效）、429（限频）、5xx 均可切换账号重试
 * @param code - HTTP 状态码
 * @returns bool - 是否可重试
 */
func IsRetryableStatus(code int) bool {
	if code >= 200 && code < 300 {
		return false
	}
	switch code {
	case 400, 403:
		return false
	}
	return true
}

/**
 * sendWithRetry 带内部重试的请求发送
 * 在 executor 内部循环切换账号，直到获得 2xx 响应或耗尽重试次数
 * 成功时返回打开的 *http.Response（调用方负责关闭 Body）和对应的账号
 * 失败时返回 StatusError 或网络错误
 *
 * @param ctx - 上下文
 * @param rc - 重试配置
 * @param model - 模型名（传递给 PickFn）
 * @param apiURL - 请求 URL
 * @param codexBody - 请求体字节（每次重试自动创建新 Reader）
 * @param stream - 是否流式（影响 Accept 头）
 * @returns *http.Response - 成功的上游响应（调用方负责关闭）
 * @returns *auth.Account - 使用的账号
 * @returns error - 所有重试均失败时返回错误
 */
func (e *Executor) sendWithRetry(ctx context.Context, rc RetryConfig, model string, apiURL string, codexBody []byte, stream bool) (*http.Response, *auth.Account, int, error) {
	excluded := make(map[string]bool)
	maxAttempts := rc.MaxRetry + 1
	var lastErr error

	trySend := func(account *auth.Account, attemptOneBased, maxLabel int, pickDur time.Duration) (*http.Response, error) {
		startAttempt := time.Now()
		log.Debugf("send attempt %d/%d account=%s model=%s stream=%v", attemptOneBased, maxLabel, account.GetEmail(), model, stream)

		const maxSameAccount401Rounds = 3
		var lastStatus *StatusError
		/* 每个选号尝试内最多做一次 OAuth 401 恢复；刷新后仍 401 则直接换号，避免与「换号提速」冲突 */
		did401Refresh := false
		for round := 0; round < maxSameAccount401Rounds; round++ {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}

			buildStart := time.Now()
			httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(codexBody))
			if err != nil {
				return nil, fmt.Errorf("%w: %w", errCodexBuildRequest, err)
			}
			applyCodexHeaders(httpReq, account, stream)
			buildDur := time.Since(buildStart)
			dialTarget := effectiveDialTarget(httpReq.URL, e.resolveAddr)
			log.Debugf("upstream request model=%s stream=%v account=%s attempt=%d/%d method=%s url=%s dial_target=%s", model, stream, account.GetEmail(), attemptOneBased, maxLabel, httpReq.Method, httpReq.URL.String(), dialTarget)

			doStart := time.Now()
			httpResp, err := e.httpClient.Do(httpReq)
			doDur := time.Since(doStart)
			if err != nil {
				account.RecordFailure()
				netErr := fmt.Errorf("请求发送失败: %w", err)
				log.Debugf("send stage model=%s account=%s attempt=%d/%d pick=%v build=%v upstream_wait=%v total=%v status=ERR err=%v", model, account.GetEmail(), attemptOneBased, maxLabel, pickDur, buildDur, doDur, time.Since(startAttempt), err)
				return nil, netErr
			}

			if httpResp.StatusCode >= 200 && httpResp.StatusCode < 300 {
				log.Debugf("send stage model=%s account=%s attempt=%d/%d pick=%v build=%v upstream_wait=%v total=%v status=%d", model, account.GetEmail(), attemptOneBased, maxLabel, pickDur, buildDur, doDur, time.Since(startAttempt), httpResp.StatusCode)
				log.Debugf("send attempt success status=%d account=%s elapsed=%v", httpResp.StatusCode, account.GetEmail(), time.Since(startAttempt).Round(time.Millisecond))
				return httpResp, nil
			}

			errBody, _ := io.ReadAll(io.LimitReader(httpResp.Body, 1<<20))
			_ = httpResp.Body.Close()

			handleAccountError(account, httpResp.StatusCode, errBody)

			if rc.OnAfterUpstreamErrFn != nil {
				rc.OnAfterUpstreamErrFn(account, httpResp.StatusCode)
			}

			if httpResp.StatusCode == 429 && rc.On429RecoveryFn != nil {
				go rc.On429RecoveryFn(context.Background(), account)
			}

			lastStatus = &StatusError{Code: httpResp.StatusCode, Body: errBody}
			log.Debugf("send stage model=%s account=%s attempt=%d/%d pick=%v build=%v upstream_wait=%v total=%v status=%d", model, account.GetEmail(), attemptOneBased, maxLabel, pickDur, buildDur, doDur, time.Since(startAttempt), httpResp.StatusCode)

			if httpResp.StatusCode == 401 && rc.On401Fn != nil {
				if did401Refresh {
					log.Debugf("账号 [%s] 已刷新后仍 401，结束同号重试并换号", account.GetEmail())
					return nil, lastStatus
				}
				if rc.On401Fn(account) {
					did401Refresh = true
					log.Debugf("账号 [%s] 401 恢复成功，同请求内立即重试上游", account.GetEmail())
					continue
				}
			}

			return nil, lastStatus
		}
		if lastStatus != nil {
			return nil, lastStatus
		}
		return nil, fmt.Errorf("请求失败")
	}

	handleSendErr := func(account *auth.Account, attempt int, err2 error) (done bool, fatal error) {
		if err2 == nil {
			return false, nil
		}
		if errors.Is(err2, errCodexBuildRequest) {
			return true, err2
		}
		var se *StatusError
		if errors.As(err2, &se) {
			lastErr = err2
			if !IsRetryableStatus(se.Code) {
				log.Debugf("send attempt non-retryable status=%d account=%s", se.Code, account.GetEmail())
				return true, se
			}
			if attempt < maxAttempts-1 {
				log.Warnf("账号 [%s] [%d] 切换重试", account.GetEmail(), se.Code)
				return false, nil
			}
			return false, nil
		}
		lastErr = err2
		if attempt < maxAttempts-1 {
			if netutil.IsRetryableUpstreamNetError(err2) {
				log.Warnf("账号 [%s] 上游网络错误（可重试），切换账号: %v", account.GetEmail(), err2)
			} else {
				log.Warnf("账号 [%s] 网络错误，切换账号重试: %v", account.GetEmail(), err2)
			}
			return false, nil
		}
		return false, nil
	}

	for attempt := 0; attempt < maxAttempts; attempt++ {
		if ctx.Err() != nil {
			break
		}

		pickStart := time.Now()
		var account *auth.Account
		var err error
		if rc.HealthyPickMinAttempt > 0 && attempt >= rc.HealthyPickMinAttempt && rc.HealthyPickFn != nil {
			account, err = rc.HealthyPickFn(model, excluded)
			if err != nil {
				account, err = rc.PickFn(model, excluded)
			} else {
				log.Debugf("选号: 尝试 %d/%d 使用最近成功账号策略 account=%s", attempt+1, maxAttempts, account.GetEmail())
			}
		} else {
			account, err = rc.PickFn(model, excluded)
		}
		pickDur := time.Since(pickStart)
		if err != nil {
			if attempt == 0 {
				return nil, nil, attempt + 1, err
			}
			break
		}

		/* 选号后检查额度（如果配置了 QuotaCheckFn）；仅在确认无效时排除，不确定时继续尝试发送 */
		if rc.QuotaCheckFn != nil {
			if !rc.QuotaCheckFn(ctx, account) {
				/* QuotaCheckFn 返回 false 表示额度检查失败
				 * 但我们不立即排除，而是继续发送请求
				 * 让上游 API 的响应来最终决定是否需要换号
				 * 这样可以避免因为额度检查网络问题导致的误判 */
				log.Debugf("账号 [%s] 额度检查失败，但继续尝试发送请求", account.GetEmail())
				/* 不加入 excluded，继续使用该账号 */
			}
		}

		httpResp, err2 := trySend(account, attempt+1, maxAttempts, pickDur)
		if err2 == nil {
			return httpResp, account, attempt + 1, nil
		}
		excluded[account.FilePath] = true
		done, fatal := handleSendErr(account, attempt, err2)
		if done {
			return nil, nil, attempt + 1, fatal
		}
	}

	if lastErr != nil && rc.FallbackRecentPickFn != nil && ctx.Err() == nil {
		if se, ok := lastErr.(*StatusError); ok && !IsRetryableStatus(se.Code) {
			// 400/403 等不重试、不回退
		} else {
			fallbackAcc, perr := rc.FallbackRecentPickFn(model, excluded)
			if perr == nil && fallbackAcc != nil {
				log.Warnf("换号均失败，回退最近成功账号再试: %s", fallbackAcc.GetEmail())
				fbLabel := maxAttempts + 1
				httpResp, err2 := trySend(fallbackAcc, fbLabel, fbLabel, 0)
				if err2 == nil {
					return httpResp, fallbackAcc, fbLabel, nil
				}
				if errors.Is(err2, errCodexBuildRequest) {
					return nil, nil, fbLabel, err2
				}
				var se2 *StatusError
				if errors.As(err2, &se2) {
					lastErr = err2
					if !IsRetryableStatus(se2.Code) {
						return nil, nil, fbLabel, se2
					}
				} else {
					lastErr = err2
				}
			}
		}
	}

	if lastErr != nil {
		return nil, nil, maxAttempts, lastErr
	}
	return nil, nil, maxAttempts, fmt.Errorf("请求失败")
}

/**
 * ExecuteStream 执行流式请求（内部重试）
 * 将 OpenAI 格式请求转换为 Codex 格式，在内部切换账号重试直到获得 2xx 响应
 * SSE 头只在成功后才写给客户端，客户端不感知重试过程
 *
 * @param ctx - 上下文
 * @param rc - 内部重试配置
 * @param requestBody - 原始 OpenAI Chat Completions 请求体
 * @param model - 模型名称（可能含思考后缀）
 * @param writer - HTTP 响应写入器
 * @returns error - 执行失败时返回错误
 */
func (e *Executor) ExecuteStream(ctx context.Context, rc RetryConfig, requestBody []byte, model string, writer http.ResponseWriter) error {
	s, err := e.OpenCodexResponsesStream(ctx, rc, requestBody, model)
	if err != nil {
		return err
	}
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("Cache-Control", "no-cache")
	writer.Header().Set("Connection", "keep-alive")
	writer.WriteHeader(http.StatusOK)
	flusher, ok := writer.(http.Flusher)
	flush := func() {
		if ok {
			flusher.Flush()
		}
	}
	return s.PumpChatCompletion(writer, flush)
}

/**
 * ExecuteNonStream 执行非流式请求（内部重试）
 * 将 OpenAI 格式请求转换为 Codex 格式，在内部切换账号重试直到获得 2xx 响应
 *
 * @param ctx - 上下文
 * @param rc - 内部重试配置
 * @param requestBody - 原始 OpenAI Chat Completions 请求体
 * @param model - 模型名称（可能含思考后缀）
 * @returns []byte - OpenAI Chat Completions 格式的响应 JSON
 * @returns error - 执行失败时返回错误
 */
func (e *Executor) ExecuteNonStream(ctx context.Context, rc RetryConfig, requestBody []byte, model string) ([]byte, error) {
	startTotal := time.Now()
	convertStart := time.Now()
	body, baseModel := thinking.ApplyThinking(requestBody, model)
	codexBody := translator.ConvertOpenAIRequestToCodex(baseModel, body, true)
	convertDur := time.Since(convertStart)
	apiURL := e.baseURL + "/responses"
	reverseToolMap := translator.BuildReverseToolNameMap(requestBody)
	emptyRetryMax := rc.EmptyRetryMax
	if emptyRetryMax < 0 {
		emptyRetryMax = 0
	}
	excludedForEmpty := make(map[string]bool)

	for emptyAttempt := 0; emptyAttempt <= emptyRetryMax; emptyAttempt++ {
		rcExcl := MergeRetryConfigExcluded(rc, excludedForEmpty)
		sendStart := time.Now()
		httpResp, account, attempts, err := e.sendWithRetry(ctx, rcExcl, model, apiURL, codexBody, true)
		sendDur := time.Since(sendStart)
		if err != nil {
			return nil, err
		}

		scanner := bufio.NewScanner(httpResp.Body)
		scanner.Buffer(make([]byte, scannerInitSize), scannerMaxSize)
		var result []byte
		gotValid := false
		for scanner.Scan() {
			line := scanner.Bytes()
			if !bytes.HasPrefix(line, []byte("data:")) {
				continue
			}
			jsonData := bytes.TrimSpace(line[5:])
			if gjson.GetBytes(jsonData, "type").String() != "response.completed" {
				continue
			}
			resStr, hasOutput := translator.ConvertNonStreamResponse(ctx, jsonData, reverseToolMap)
			if !hasOutput {
				break
			}
			/* 仅在有有效输出时才记录 usage */
			usage := gjson.GetBytes(jsonData, "response.usage")
			if usage.Exists() {
				account.RecordUsage(
					usage.Get("input_tokens").Int(),
					usage.Get("output_tokens").Int(),
					usage.Get("total_tokens").Int(),
				)
			}
			if resStr != "" {
				result = []byte(resStr)
				gotValid = true
				break
			}
		}
		_ = httpResp.Body.Close()

		if gotValid && len(result) > 0 {
			account.RecordSuccess()
			log.Infof("req summary nonstream model=%s account=%s attempts=%d convert=%v upstream=%v total=%v", baseModel, account.GetEmail(), attempts, convertDur, sendDur, time.Since(startTotal))
			return result, nil
		}
		/* 空回答时标记账号失败，防止下一个请求继续选择该账号 */
		account.RecordFailure()
		excludedForEmpty[account.FilePath] = true
		if emptyAttempt < emptyRetryMax {
			log.Warnf("非流式空返回，换号重试 (account=%s attempt=%d/%d)", account.GetEmail(), emptyAttempt+1, emptyRetryMax+1)
		}
	}
	log.Infof("req summary nonstream (empty after %d tries) total=%v", emptyRetryMax+1, time.Since(startTotal))
	return nil, ErrEmptyResponse
}

/**
 * ExecuteResponsesStream 执行 Responses API 流式请求（内部重试）
 * 直接透传 Codex SSE 事件到客户端，不做 Chat Completions 格式转换
 *
 * @param ctx - 上下文
 * @param rc - 内部重试配置
 * @param requestBody - Responses API 格式的请求体
 * @param model - 模型名称（可能含思考后缀）
 * @param writer - HTTP 响应写入器
 * @returns error - 执行失败时返回错误
 */
func (e *Executor) ExecuteResponsesStream(ctx context.Context, rc RetryConfig, requestBody []byte, model string, writer http.ResponseWriter) error {
	s, err := e.OpenCodexResponsesStream(ctx, rc, requestBody, model)
	if err != nil {
		return err
	}
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("Cache-Control", "no-cache")
	writer.Header().Set("Connection", "keep-alive")
	writer.WriteHeader(http.StatusOK)
	flusher, ok := writer.(http.Flusher)
	flush := func() {
		if ok {
			flusher.Flush()
		}
	}
	return s.PumpRawSSE(writer, flush)
}

/**
 * ExecuteResponsesNonStream 执行 Responses API 非流式请求（内部重试）
 * 从 Codex SSE 响应中提取 response.completed 事件，返回原生 response 对象
 *
 * @param ctx - 上下文
 * @param rc - 内部重试配置
 * @param requestBody - Responses API 格式的请求体
 * @param model - 模型名称（可能含思考后缀）
 * @returns []byte - Codex Responses API 格式的完整响应 JSON
 * @returns error - 执行失败时返回错误
 */
func (e *Executor) ExecuteResponsesNonStream(ctx context.Context, rc RetryConfig, requestBody []byte, model string) ([]byte, error) {
	startTotal := time.Now()
	convertStart := time.Now()
	body, baseModel := thinking.ApplyThinking(requestBody, model)
	codexBody := translator.ConvertOpenAIRequestToCodex(baseModel, body, true)
	convertDur := time.Since(convertStart)
	apiURL := e.baseURL + "/responses"

	readRounds := 1 + rc.EmptyRetryMax
	if readRounds < 2 {
		readRounds = 2
	}
	if rc.EmptyRetryMax < 0 {
		readRounds = 2
	}
	excluded := make(map[string]bool)
	sendStart := time.Now()

	for round := 0; round < readRounds; round++ {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		rcExcl := MergeRetryConfigExcluded(rc, excluded)
		httpResp, account, attempts, err := e.sendWithRetry(ctx, rcExcl, model, apiURL, codexBody, true)
		if err != nil {
			return nil, err
		}
		sendDur := time.Since(sendStart)

		scanner := bufio.NewScanner(httpResp.Body)
		scanner.Buffer(make([]byte, scannerInitSize), scannerMaxSize)

		for scanner.Scan() {
			line := scanner.Bytes()
			if !bytes.HasPrefix(line, []byte("data:")) {
				continue
			}
			jsonData := bytes.TrimSpace(line[5:])
			if gjson.GetBytes(jsonData, "type").String() != "response.completed" {
				continue
			}
			if resp := gjson.GetBytes(jsonData, "response"); resp.Exists() {
				_ = httpResp.Body.Close()
				usage := gjson.GetBytes(jsonData, "response.usage")
				if usage.Exists() {
					account.RecordUsage(
						usage.Get("input_tokens").Int(),
						usage.Get("output_tokens").Int(),
						usage.Get("total_tokens").Int(),
					)
				}
				account.RecordSuccess()
				log.Infof("req summary responses-nonstream model=%s account=%s attempts=%d convert=%v upstream=%v total=%v", baseModel, account.GetEmail(), attempts, convertDur, sendDur, time.Since(startTotal))
				return []byte(resp.Raw), nil
			}
		}

		if err := scanner.Err(); err != nil {
			_ = httpResp.Body.Close()
			account.RecordFailure()
			if isRetryableUpstreamReadErr(err) && round+1 < readRounds {
				excluded[account.FilePath] = true
				log.Warnf("responses-nonstream 读 SSE 失败，换号重试 (%d/%d): %v", round+1, readRounds, wrapReadErr(err))
				continue
			}
			log.Infof("req summary responses-nonstream model=%s account=%s attempts=%d convert=%v upstream=%v total=%v (ERR)", baseModel, account.GetEmail(), attempts, convertDur, sendDur, time.Since(startTotal))
			return nil, fmt.Errorf("读取响应失败: %w", wrapReadErr(err))
		}

		_ = httpResp.Body.Close()
		account.RecordFailure()
		log.Infof("req summary responses-nonstream model=%s account=%s attempts=%d convert=%v upstream=%v total=%v (no completed)", baseModel, account.GetEmail(), attempts, convertDur, sendDur, time.Since(startTotal))
		return nil, fmt.Errorf("未收到 response.completed 事件")
	}
	return nil, fmt.Errorf("读取响应失败")
}

func (e *Executor) OpenResponsesStream(ctx context.Context, rc RetryConfig, requestBody []byte, model string) (*RawResponse, *auth.Account, int, string, time.Duration, time.Duration, error) {
	convertStart := time.Now()
	body, baseModel := thinking.ApplyThinking(requestBody, model)
	codexBody := translator.ConvertOpenAIRequestToCodex(baseModel, body, true)
	convertDur := time.Since(convertStart)
	apiURL := e.baseURL + "/responses"

	sendStart := time.Now()
	httpResp, account, attempts, err := e.sendWithRetry(ctx, rc, model, apiURL, codexBody, true)
	if err != nil {
		return nil, nil, 0, "", 0, 0, err
	}
	sendDur := time.Since(sendStart)

	return &RawResponse{StatusCode: httpResp.StatusCode, Body: httpResp.Body}, account, attempts, baseModel, convertDur, sendDur, nil
}

/**
 * ExecuteResponsesCompactStream 执行 Responses Compact API 流式请求（内部重试）
 * 使用 /responses/compact 端点，直接透传 Codex SSE 事件到客户端
 *
 * @param ctx - 上下文
 * @param rc - 内部重试配置
 * @param requestBody - Responses API 格式的请求体
 * @param model - 模型名称（可能含思考后缀）
 * @param writer - HTTP 响应写入器
 * @returns error - 执行失败时返回错误
 */
func (e *Executor) ExecuteResponsesCompactStream(ctx context.Context, rc RetryConfig, requestBody []byte, model string, writer http.ResponseWriter) error {
	startTotal := time.Now()
	compact, err := e.OpenCodexCompactStream(ctx, rc, requestBody, model)
	if err != nil {
		return err
	}
	for k, vs := range compact.Resp.Header {
		for _, v := range vs {
			writer.Header().Add(k, v)
		}
	}
	writer.WriteHeader(http.StatusOK)
	flusher, ok := writer.(http.Flusher)
	flush := func() {
		if ok {
			flusher.Flush()
		}
	}
	if err := compact.PumpBody(writer, flush); err != nil {
		return err
	}
	compact.Account.RecordSuccess()
	log.Infof("req summary responses-compact-stream model=%s account=%s attempts=%d convert=%v upstream=%v total=%v", compact.BaseModel, compact.Account.GetEmail(), compact.Attempts, compact.ConvertDur, compact.SendDur, time.Since(startTotal))
	return nil
}

/**
 * ExecuteResponsesCompactNonStream 执行 Responses Compact API 非流式请求（内部重试）
 * 使用 /responses/compact 端点，返回 compact 格式的完整响应
 *
 * @param ctx - 上下文
 * @param rc - 内部重试配置
 * @param requestBody - Responses API 格式的请求体
 * @param model - 模型名称（可能含思考后缀）
 * @returns []byte - compact 格式的完整响应
 * @returns error - 执行失败时返回错误
 */
func (e *Executor) ExecuteResponsesCompactNonStream(ctx context.Context, rc RetryConfig, requestBody []byte, model string) ([]byte, error) {
	startTotal := time.Now()
	convertStart := time.Now()
	body, baseModel := thinking.ApplyThinking(requestBody, model)
	codexBody := cleanCompactBody(body, baseModel)
	convertDur := time.Since(convertStart)
	apiURL := e.baseURL + "/responses/compact"

	sendStart := time.Now()
	httpResp, account, attempts, err := e.sendWithRetry(ctx, rc, model, apiURL, codexBody, false)
	if err != nil {
		return nil, err
	}
	sendDur := time.Since(sendStart)
	defer func() {
		_ = httpResp.Body.Close()
	}()

	data, err := io.ReadAll(httpResp.Body)
	if err != nil {
		log.Infof("req summary responses-compact-nonstream model=%s account=%s attempts=%d convert=%v upstream=%v total=%v (ERR)", baseModel, account.GetEmail(), attempts, convertDur, sendDur, time.Since(startTotal))
		return nil, fmt.Errorf("读取响应失败: %w", wrapReadErr(err))
	}

	account.RecordSuccess()
	log.Infof("req summary responses-compact-nonstream model=%s account=%s attempts=%d convert=%v upstream=%v total=%v", baseModel, account.GetEmail(), attempts, convertDur, sendDur, time.Since(startTotal))
	return data, nil
}

// OpenCodexCompactStream 打开 /responses/compact 流式上游连接（2xx 后返回，Body 由 PumpBody 关闭）。
func (e *Executor) OpenCodexCompactStream(ctx context.Context, rc RetryConfig, requestBody []byte, model string) (*CodexCompactStream, error) {
	convertStart := time.Now()
	body, baseModel := thinking.ApplyThinking(requestBody, model)
	codexBody := cleanCompactBody(body, baseModel)
	convertDur := time.Since(convertStart)
	apiURL := e.baseURL + "/responses/compact"
	sendStart := time.Now()
	httpResp, account, attempts, err := e.sendWithRetry(ctx, rc, model, apiURL, codexBody, true)
	if err != nil {
		return nil, err
	}
	return &CodexCompactStream{
		Resp:       httpResp,
		Account:    account,
		Attempts:   attempts,
		BaseModel:  baseModel,
		ConvertDur: convertDur,
		SendDur:    time.Since(sendStart),
	}, nil
}

/**
 * cleanCompactBody 为 Compact 端点清理请求体
 * 不使用通用转换器，直接透传原始请求体
 * 只做模型名替换 + 删除 Compact 端点不支持的参数
 * @param body - 原始请求体（已应用思考配置）
 * @param baseModel - 解析后的基础模型名
 * @returns []byte - 清理后的请求体
 */
func cleanCompactBody(body []byte, baseModel string) []byte {
	/* sjson 操作会返回新切片，无需手动 copy */
	result, _ := sjson.SetBytes(body, "model", baseModel)

	/* 删除 Compact 端点不支持的参数 */
	result, _ = sjson.DeleteBytes(result, "stream")
	result, _ = sjson.DeleteBytes(result, "stream_options")
	result, _ = sjson.DeleteBytes(result, "parallel_tool_calls")
	result, _ = sjson.DeleteBytes(result, "reasoning")
	result, _ = sjson.DeleteBytes(result, "include")
	result, _ = sjson.DeleteBytes(result, "previous_response_id")
	result, _ = sjson.DeleteBytes(result, "prompt_cache_retention")
	result, _ = sjson.DeleteBytes(result, "safety_identifier")
	result, _ = sjson.DeleteBytes(result, "generate")
	result, _ = sjson.DeleteBytes(result, "store")
	result, _ = sjson.DeleteBytes(result, "reasoning_effort")
	result, _ = sjson.DeleteBytes(result, "max_output_tokens")
	result, _ = sjson.DeleteBytes(result, "max_completion_tokens")
	result, _ = sjson.DeleteBytes(result, "temperature")
	result, _ = sjson.DeleteBytes(result, "top_p")
	result, _ = sjson.DeleteBytes(result, "truncation")
	result, _ = sjson.DeleteBytes(result, "context_management")
	result, _ = sjson.DeleteBytes(result, "user")
	result, _ = sjson.DeleteBytes(result, "service_tier")

	/* Compact 端点要求 instructions 字段必须存在 */
	if !gjson.GetBytes(result, "instructions").Exists() {
		result, _ = sjson.SetBytes(result, "instructions", "")
	}

	return result
}

/**
 * RawResponse 原始上游响应封装
 * @field StatusCode - HTTP 状态码
 * @field Body - 响应体（调用方负责关闭）
 * @field ErrBody - 错误时的响应体（StatusCode >= 300 时有值）
 */
type RawResponse struct {
	StatusCode int
	Body       io.ReadCloser
	ErrBody    []byte
}

/**
 * ExecuteRawCodexStream 发送请求到 Codex 并返回原始上游响应（内部重试）
 * 不做任何格式转换，由调用方自行处理响应体
 * 用于 Claude API 等需要自定义响应格式的场景
 *
 * @param ctx - 上下文
 * @param rc - 内部重试配置
 * @param requestBody - OpenAI Chat Completions 格式的请求体
 * @param model - 模型名称（可能含思考后缀）
 * @returns *RawResponse - 原始响应（成功时调用方需关闭 Body）
 * @returns *auth.Account - 使用的账号（调用方用于 RecordSuccess）
 * @returns error - 请求发送失败时返回错误
 */
func (e *Executor) ExecuteRawCodexStream(ctx context.Context, rc RetryConfig, requestBody []byte, model string) (*RawResponse, *auth.Account, error) {
	bodyRC, account, _, _, _, _, _, err := e.openCodexResponsesBody(ctx, rc, requestBody, model)
	if err != nil {
		return nil, nil, err
	}
	return &RawResponse{StatusCode: http.StatusOK, Body: bodyRC}, account, nil
}

/**
 * applyCodexHeaders 设置 Codex API 请求头
 * @param r - HTTP 请求
 * @param account - 账号（提供 access_token 和 account_id）
 * @param stream - 是否为流式请求
 */
func applyCodexHeaders(r *http.Request, account *auth.Account, stream bool) {
	token := account.GetAccessToken()
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Version", codexClientVersion)
	r.Header.Set("Session_id", uuid.NewString())
	r.Header.Set("User-Agent", codexUserAgent)
	r.Header.Set("Origin", "https://chatgpt.com")
	r.Header.Set("Referer", "https://chatgpt.com/")
	r.Header.Set("Originator", "codex_cli_rs")
	r.Header.Set("Connection", "Keep-Alive")

	if stream {
		r.Header.Set("Accept", "text/event-stream")
	} else {
		r.Header.Set("Accept", "application/json")
	}

	accountID := account.GetAccountID()
	if accountID != "" {
		r.Header.Set("Chatgpt-Account-Id", accountID)
	}
}

/**
 * handleAccountError 根据 HTTP 错误状态码记录账号失败
 * handler 层会根据 ShouldRemoveAccount 决定是否删号
 * @param account - 账号
 * @param statusCode - HTTP 状态码
 * @param body - 错误响应体
 */
func handleAccountError(account *auth.Account, statusCode int, body []byte) {
	account.RecordFailure()

	switch {
	case statusCode == 429:
		cooldown := parseRetryAfter(body)
		if cooldown > 0 {
			account.SetQuotaCooldown(cooldown)
		}
	case statusCode == 403:
		account.SetCooldown(5 * time.Minute)
	}
}

/**
 * StatusError HTTP 状态错误
 * @field Code - HTTP 状态码
 * @field Body - 错误响应体
 */
type StatusError struct {
	Code int
	Body []byte
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("Codex API 错误 [%d]: %s", e.Code, summarizeError(e.Body))
}

func effectiveDialTarget(u *url.URL, resolveAddr string) string {
	if u == nil {
		return ""
	}
	host := u.Hostname()
	port := u.Port()
	if port == "" {
		if strings.EqualFold(u.Scheme, "http") {
			port = "80"
		} else {
			port = "443"
		}
	}

	resolveAddr = netutil.NormalizeResolveAddress(resolveAddr)
	if resolveAddr != "" {
		if _, _, err := net.SplitHostPort(resolveAddr); err == nil {
			return resolveAddr
		}
		return net.JoinHostPort(resolveAddr, port)
	}
	if host == "" {
		return u.Host
	}
	return net.JoinHostPort(host, port)
}

/**
 * summarizeError 提取错误响应的摘要信息
 * @param body - 错误响应体
 * @returns string - 错误摘要
 */
func summarizeError(body []byte) string {
	if len(body) == 0 {
		return "(空响应)"
	}
	if msg := gjson.GetBytes(body, "error.message").String(); msg != "" {
		return msg
	}
	if len(body) > 100 {
		return string(body[:100]) + "..."
	}
	return string(body)
}

/**
 * parseRetryAfter 从 429 错误响应中解析冷却时间
 * @param body - 错误响应体
 * @returns time.Duration - 冷却持续时间
 */
func parseRetryAfter(body []byte) time.Duration {
	if len(body) == 0 {
		return 60 * time.Second
	}

	/* 尝试从 resets_at 字段解析 */
	if resetsAt := gjson.GetBytes(body, "error.resets_at").Int(); resetsAt > 0 {
		resetTime := time.Unix(resetsAt, 0)
		if resetTime.After(time.Now()) {
			return time.Until(resetTime)
		}
	}

	/* 尝试从 resets_in_seconds 字段解析 */
	if seconds := gjson.GetBytes(body, "error.resets_in_seconds").Int(); seconds > 0 {
		return time.Duration(seconds) * time.Second
	}

	/* 默认冷却 60 秒 */
	return 60 * time.Second
}

/**
 * StartKeepAlive 启动连接池保活循环
 * 每隔固定时间向上游发送轻量级 HEAD 请求，防止空闲连接被回收
 * 解决长时间无请求后首次请求因重建 TCP+TLS 连接而耗时过长的问题
 * 使用 sync.Once 保证只启动一次
 * @param ctx - 上下文，用于控制生命周期
 */
func (e *Executor) StartKeepAlive(ctx context.Context) {
	e.keepAliveOnce.Do(func() {
		go func() {
			sec := e.keepaliveIntervalSec
			if sec < 1 {
				sec = defaultKeepaliveIntervalSec
			}
			ticker := time.NewTicker(time.Duration(sec) * time.Second)
			defer ticker.Stop()

			pingURL := strings.TrimSuffix(e.baseURL, "/codex")
			if pingURL == "" {
				pingURL = "https://chatgpt.com"
			}

			log.Infof("连接保活已启动，每 %d 秒 ping %s", sec, pingURL)

			for {
				select {
				case <-ctx.Done():
					log.Debug("连接保活循环已停止")
					return
				case <-ticker.C:
					e.pingUpstream(pingURL)
				}
			}
		}()
	})
}

/**
 * pingUpstream 向上游发送轻量级 HEAD 请求保持连接池活跃
 * 忽略响应结果，仅为维持 TCP+TLS 连接
 * @param targetURL - 目标 URL
 */
func (e *Executor) pingUpstream(targetURL string) {
	/* 保活 ping 非用户对话路径，设短超时避免后台 ticker 堆积 */
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, targetURL, nil)
	if err != nil {
		return
	}
	req.Header.Set("User-Agent", codexUserAgent)
	req.Header.Set("Connection", "Keep-Alive")

	resp, err := e.httpClient.Do(req)
	if err != nil {
		log.Debugf("连接保活 ping 失败: %v", err)
		return
	}
	_ = resp.Body.Close()
	log.Debugf("连接保活 ping 成功: %d", resp.StatusCode)
}
