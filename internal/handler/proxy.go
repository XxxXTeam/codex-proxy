/**
 * HTTP 代理处理器模块
 * 提供 OpenAI 兼容的 API 端点，接收请求后通过 Codex 执行器转发
 * 支持流式和非流式响应、API Key 鉴权、模型列表接口
 */
package handler

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"codex-proxy/internal/auth"
	"codex-proxy/internal/executor"
	"codex-proxy/internal/thinking"

	fasthttprouter "github.com/fasthttp/router"
	"github.com/fasthttp/websocket"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"github.com/valyala/fasthttp"
)

/* 与 executor 一致的缓冲与扫描器大小，便于统一调优 */
const (
	wsBufferSize     = 32 * 1024
	scannerInitSize  = 4 * 1024
	scannerMaxSize   = 50 * 1024 * 1024
	statsMaxPageSize = 200
)

type statsPagination struct {
	Page          int    `json:"page"`
	PageSize      int    `json:"page_size"`
	Total         int    `json:"total"`
	FilteredTotal int    `json:"filtered_total"`
	TotalPages    int    `json:"total_pages"`
	Returned      int    `json:"returned"`
	HasPrev       bool   `json:"has_prev"`
	HasNext       bool   `json:"has_next"`
	Query         string `json:"query,omitempty"`
}

var responsesWSUpgrader = websocket.FastHTTPUpgrader{
	ReadBufferSize:  wsBufferSize,
	WriteBufferSize: wsBufferSize,
	CheckOrigin: func(ctx *fasthttp.RequestCtx) bool {
		return true
	},
}

/**
 * ProxyHandler 代理处理器
 * @field manager - 账号管理器
 * @field executor - Codex 执行器
 * @field apiKeys - 允许访问的 API Key 列表（为空则不鉴权）
 * @field maxRetry - 请求失败最大重试次数（切换账号重试）
 * @field auth401RecoverTracks - 追踪账号 401 恢复的次数和时间，防止陷入快速循环
 */
type ProxyHandler struct {
	manager                   *auth.Manager
	executor                  *executor.Executor
	apiKeys                   []string
	maxRetry                  int
	enableHealthyRetry        bool
	quotaChecker              *auth.QuotaChecker
	quotaPrecheck             bool /* true：选号后 wham 预检；false：直发上游，401 换号+异步 OAuth */
	indexHTML                 []byte
	emptyRetryMax             int
	debugUpstreamStream       bool          /* 配置 debug-upstream-stream：打印上游 SSE 原文 */
	enableModelFast           bool          /* 是否允许模型名携带 -fast */
	enableModel1M             bool          /* 是否允许模型名携带 -1m */
	enableWebSocket           bool          /* 是否允许 /v1/responses 走 WebSocket */
	debugWSStream             bool          /* WS 转发时是否打印每帧 debug 日志 */
	concurrentRetry429        bool          /* 遇 429 时并发重试 */
	concurrentRetry429Timeout time.Duration /* 并发重试最大等待时间 */
	auth401RecoverTracks      sync.Map      /* key: filePath, value: *auth401RecoverTrack */
	/* retryCfg 在首请求时构建一次，避免每条对话重复分配闭包与 RetryConfig */
	retryCfgOnce sync.Once
	retryCfg     executor.RetryConfig
}

/* auth401RecoverTrack 追踪单个账号的 401 恢复情况 */
type auth401RecoverTrack struct {
	count     int       /* 在当前时间窗口内的恢复次数 */
	startTime time.Time /* 时间窗口开始时间 */
}

/**
 * NewProxyHandler 创建新的代理处理器
 * @param manager - 账号管理器
 * @param exec - Codex 执行器
 * @param apiKeys - API Key 列表
 * @param maxRetry - 最大重试次数（0 表示不重试）
 * @param quotaCheckConcurrency - 额度查询并发数（来自 config；quotaChecker 为 nil 新建 checker 时用）
 * @param quotaCheckCacheTTLSec - wham 预检本地复用秒数（quotaChecker 为 nil 时传给 NewQuotaChecker；0 关闭）
 * @param quotaChecker - 与 main 注入 Manager 的同一实例（wham/usage）；nil 时内部新建
 * @param quotaPrecheck - true 时选号后 wham 预检；false 时直发上游（401 换号 + 异步 OAuth，见 quota-precheck 配置）
 * @param debugUpstreamStream - 是否 Info 打印上游 Codex SSE 原文（对应配置 debug-upstream-stream）
 * @returns *ProxyHandler - 代理处理器实例
 */
func NewProxyHandler(manager *auth.Manager, exec *executor.Executor, apiKeys []string, maxRetry int, enableHealthyRetry bool, proxyURL string, baseURL string, enableHTTP2 bool, backendDomain string, backendResolveAddress string, quotaCheckConcurrency int, quotaCheckCacheTTLSec int, quotaChecker *auth.QuotaChecker, quotaPrecheck bool, emptyRetryMax int, debugUpstreamStream bool, enableModelFast bool, enableModel1M bool, enableWebSocket bool, debugWSStream bool, concurrentRetry429 bool, concurrentRetry429TimeoutSec int, indexHTML []byte) *ProxyHandler {
	if maxRetry < 0 {
		maxRetry = 0
	}
	if quotaCheckConcurrency <= 0 {
		quotaCheckConcurrency = 50
	}
	if quotaChecker == nil {
		quotaChecker = auth.NewQuotaChecker(baseURL, proxyURL, quotaCheckConcurrency, enableHTTP2, backendDomain, backendResolveAddress, time.Duration(quotaCheckCacheTTLSec)*time.Second)
	}
	return &ProxyHandler{
		manager:             manager,
		executor:            exec,
		apiKeys:             apiKeys,
		maxRetry:            maxRetry,
		enableHealthyRetry:  enableHealthyRetry,
		quotaChecker:        quotaChecker,
		quotaPrecheck:       quotaPrecheck,
		indexHTML:           indexHTML,
		emptyRetryMax:       emptyRetryMax,
		debugUpstreamStream: debugUpstreamStream,
		enableModelFast:     enableModelFast,
		enableModel1M:       enableModel1M,
		enableWebSocket:     enableWebSocket,
		debugWSStream:       debugWSStream,
		concurrentRetry429:  concurrentRetry429,
		concurrentRetry429Timeout: func() time.Duration {
			if concurrentRetry429TimeoutSec > 0 {
				return time.Duration(concurrentRetry429TimeoutSec) * time.Second
			}
			return 30 * time.Second
		}(),
	}
}

/**
 * RegisterRoutes 注册所有 HTTP 路由
 * @param r - FastHTTP 路由实例
 */
func (h *ProxyHandler) RegisterRoutes(r *fasthttprouter.Router) {
	/* 首页 */
	r.GET("/", h.handleIndex)

	/* 健康检查 */
	r.GET("/health", h.handleHealth)

	/* OpenAI 兼容接口 */
	apiAuth := h.handleChatCompletions
	if len(h.apiKeys) > 0 {
		apiAuth = h.authMiddleware(h.handleChatCompletions)
	}
	r.POST("/v1/chat/completions", apiAuth)

	apiResponses := h.handleResponses
	if len(h.apiKeys) > 0 {
		apiResponses = h.authMiddleware(h.handleResponses)
	}
	r.POST("/v1/responses", apiResponses)

	apiResponsesCompact := h.handleResponsesCompact
	if len(h.apiKeys) > 0 {
		apiResponsesCompact = h.authMiddleware(h.handleResponsesCompact)
	}
	r.POST("/v1/responses/compact", apiResponsesCompact)

	apiMessages := h.handleMessages
	if len(h.apiKeys) > 0 {
		apiMessages = h.authMiddleware(h.handleMessages)
	}
	r.POST("/v1/messages", apiMessages)

	apiModels := h.handleModels
	if len(h.apiKeys) > 0 {
		apiModels = h.authMiddleware(h.handleModels)
	}
	r.GET("/v1/models", apiModels)

	/* 管理接口 */
	statsHandler := h.handleStats
	refreshHandler := h.handleRefresh
	checkQuotaHandler := h.handleCheckQuota
	recoverAuthHandler := h.handleRecoverAuth
	if len(h.apiKeys) > 0 {
		statsHandler = h.authMiddleware(h.handleStats)
		refreshHandler = h.authMiddleware(h.handleRefresh)
		checkQuotaHandler = h.authMiddleware(h.handleCheckQuota)
		recoverAuthHandler = h.authMiddleware(h.handleRecoverAuth)
	}
	r.GET("/stats", statsHandler)
	r.POST("/refresh", refreshHandler)
	r.POST("/check-quota", checkQuotaHandler)
	r.POST("/recover-auth", recoverAuthHandler)

	accountsIngestHandler := h.handleAccountsIngest
	if len(h.apiKeys) > 0 {
		accountsIngestHandler = h.authMiddleware(h.handleAccountsIngest)
	}
	r.POST("/admin/accounts/ingest", accountsIngestHandler)
	r.GET("/admin/accounts/ingest", accountsIngestHandler)
}

/**
 * authMiddleware API Key 鉴权中间件
 */
func (h *ProxyHandler) authMiddleware(next fasthttp.RequestHandler) fasthttp.RequestHandler {
	keySet := make(map[string]struct{}, len(h.apiKeys))
	for _, k := range h.apiKeys {
		k = strings.TrimSpace(k)
		if k != "" {
			keySet[k] = struct{}{}
		}
	}

	return func(ctx *fasthttp.RequestCtx) {
		if len(keySet) == 0 {
			next(ctx)
			return
		}

		token := ""
		tokenSource := "none"

		authHeader := strings.TrimSpace(string(ctx.Request.Header.Peek("Authorization")))
		if authHeader != "" {
			parts := strings.Fields(authHeader)
			if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
				token = strings.TrimSpace(parts[1])
				tokenSource = "authorization_bearer"
			}
		}

		if token == "" {
			token = strings.TrimSpace(string(ctx.Request.Header.Peek("x-api-key")))
			if token != "" {
				tokenSource = "x-api-key"
			}
		}
		if token == "" {
			token = strings.TrimSpace(string(ctx.Request.Header.Peek("api-key")))
			if token != "" {
				tokenSource = "api-key"
			}
		}

		if _, ok := keySet[token]; !ok {
			log.Debugf("鉴权失败: path=%s source=%s api_key_len=%d", string(ctx.Path()), tokenSource, len(token))
			writeJSON(ctx, fasthttp.StatusUnauthorized, map[string]any{
				"error": map[string]any{
					"message": "无效的 API Key",
					"type":    "invalid_request_error",
					"code":    "invalid_api_key",
				},
			})
			return
		}

		log.Debugf("鉴权成功: path=%s source=%s token_len=%d", string(ctx.Path()), tokenSource, len(token))
		next(ctx)
	}
}

/**
 * handleHealth 健康检查接口
 */
func (h *ProxyHandler) handleHealth(ctx *fasthttp.RequestCtx) {
	writeJSON(ctx, fasthttp.StatusOK, map[string]any{
		"status":   "ok",
		"accounts": h.manager.AccountCount(),
	})
}

type modelListEntry struct {
	base     string
	suffixes []string
}

var modelList = []modelListEntry{
	{base: "gpt-5", suffixes: []string{"low", "medium", "high", "auto"}},
	{base: "gpt-5-codex", suffixes: []string{"low", "medium", "high", "auto"}},
	{base: "gpt-5-codex-mini", suffixes: []string{"low", "medium", "high", "auto"}},
	{base: "gpt-5.1", suffixes: []string{"low", "medium", "high", "none", "auto"}},
	{base: "gpt-5.1-codex", suffixes: []string{"low", "medium", "high", "max", "auto"}},
	{base: "gpt-5.1-codex-mini", suffixes: []string{"low", "medium", "high", "auto"}},
	{base: "gpt-5.1-codex-max", suffixes: []string{"low", "medium", "high", "xhigh", "auto"}},
	{base: "gpt-5.2", suffixes: []string{"low", "medium", "high", "xhigh", "none", "auto"}},
	{base: "gpt-5.2-codex", suffixes: []string{"low", "medium", "high", "xhigh", "auto"}},
	{base: "gpt-5.3-codex", suffixes: []string{"low", "medium", "high", "xhigh", "none", "auto"}},
	{base: "gpt-5.4", suffixes: []string{"low", "medium", "high", "xhigh", "none", "auto"}},
	{base: "gpt-5.4-mini", suffixes: []string{"low", "medium", "high", "xhigh", "none", "auto"}},
}

func expandModelSubvariantIDs(id string, enableFast bool, enable1M bool) []string {
	out := []string{id}
	if enable1M {
		out = append(out, id+"-1m")
	}
	if enableFast {
		out = append(out, id+"-fast")
	}
	if enableFast && enable1M {
		out = append(out, id+"-1m-fast", id+"-fast-1m")
	}
	return out
}

func (h *ProxyHandler) handleModels(ctx *fasthttp.RequestCtx) {
	models := make([]map[string]interface{}, 0, 800)
	for _, e := range modelList {
		ids := make([]string, 0, 1+len(e.suffixes))
		ids = append(ids, e.base)
		for _, s := range e.suffixes {
			ids = append(ids, e.base+"-"+s)
		}
		for _, id := range ids {
			for _, mid := range expandModelSubvariantIDs(id, h.enableModelFast, h.enableModel1M) {
				models = append(models, map[string]interface{}{"id": mid, "object": "model", "owned_by": "openai"})
			}
		}
	}

	writeJSON(ctx, fasthttp.StatusOK, map[string]interface{}{
		"object": "list",
		"data":   models,
	})
}

func (h *ProxyHandler) validateModelSuffixOptions(model string) error {
	parsed := thinking.ParseModelSuffix(model)
	if parsed.IsFast && !h.enableModelFast {
		return fmt.Errorf("模型后缀 -fast 已禁用")
	}
	if parsed.Is1M && !h.enableModel1M {
		return fmt.Errorf("模型后缀 -1m 已禁用")
	}
	return nil
}

/**
 * buildRetryConfig 返回 executor 内部重试配置（进程内缓存，字段不变时勿改 handler 相关配置）
 */
func (h *ProxyHandler) buildRetryConfig() executor.RetryConfig {
	h.retryCfgOnce.Do(func() { h.retryCfg = h.buildRetryConfigOnce() })
	return h.retryCfg
}

func (h *ProxyHandler) buildRetryConfigOnce() executor.RetryConfig {
	healthyPick := func(model string, excluded map[string]bool) (*auth.Account, error) {
		return h.manager.PickRecentlySuccessful(model, excluded)
	}
	rc := executor.RetryConfig{
		PickFn: func(model string, excluded map[string]bool) (*auth.Account, error) {
			return h.manager.PickExcluding(model, excluded)
		},
		EnsureTokenFreshFn: func(ctx context.Context, acc *auth.Account) bool {
			return h.manager.EnsureTokenFresh(ctx, acc)
		},
		On401Fn: func(acc *auth.Account) bool {
			/* 先换号让当前请求立即继续；对 401 账号在后台提交 OAuth+额度恢复（异步，不阻塞） */
			if acc == nil {
				return false
			}
			if h.canPerformAuth401Recover(acc) {
				h.recordAuth401Recover(acc)
				h.manager.ScheduleRecoverAfterAuth401(acc, h.quotaChecker)
			} else {
				log.Warnf("账号 [%s] 在 30 秒内异步恢复次数过多（>2 次），跳过后台刷新", acc.GetEmail())
			}
			return false
		},
		On429RecoveryFn: func(ctx context.Context, acc *auth.Account) {
			h.manager.ScheduleUpstream429Recovery(ctx, acc, h.quotaChecker)
		},
		OnAfterUpstreamErrFn: func(_ *auth.Account, statusCode int) {
			if statusCode >= 200 && statusCode < 300 {
				return
			}
			/* 冷却或限频后失效选号缓存；502/503/504 同步失效，避免大量请求继续撞同一批刚失败的号 */
			if statusCode == 429 || statusCode == 403 || statusCode == 502 || statusCode == 503 || statusCode == 504 {
				h.manager.InvalidateSelectorCache()
			}
		},
		MaxRetry:                  h.maxRetry,
		EmptyRetryMax:             h.emptyRetryMax,
		DebugUpstreamStream:       h.debugUpstreamStream,
		ConcurrentRetry429:        h.concurrentRetry429,
		ConcurrentRetry429Timeout: h.concurrentRetry429Timeout,
		PickIgnoringCooldownFn: func(model string, excluded map[string]bool) (*auth.Account, error) {
			return h.manager.PickIgnoringCooldown(model, excluded)
		},
	}
	if h.quotaPrecheck && h.quotaChecker != nil {
		rc.QuotaCheckFn = func(ctx context.Context, acc *auth.Account) bool {
			if acc != nil && !acc.HasRefreshToken() {
				return true
			}
			verdict := h.quotaChecker.CheckAccountResult(ctx, acc)
			switch verdict {
			case 1:
				return true
			case -1:
				log.Warnf("账号 [%s] 额度接口判定无效，跳过发送", acc.GetEmail())
				return false
			case 0:
				log.Debugf("账号 [%s] 额度查询网络/5xx 暂态，仍尝试上游", acc.GetEmail())
				return true
			case 2:
				log.Debugf("账号 [%s] 额度查询 429，仍尝试上游", acc.GetEmail())
				return true
			default:
				return true
			}
		}
	}
	if h.enableHealthyRetry {
		rc.HealthyPickFn = healthyPick
		if h.maxRetry >= 2 {
			/* 前 max-retry-1 次用常规换号，之后用最近成功账号，减少无效轮询 */
			rc.HealthyPickMinAttempt = h.maxRetry - 1
		}
		/* 常规尝试用尽后，sendWithRetry 末尾再保底一次「最近成功账号」（可重试已排除的号，见 PickRecentlySuccessful） */
		rc.FallbackRecentPickFn = healthyPick
		/* 最后一格选号：仅快速取最近成功号，不阻塞 OAuth（刷新由周期任务/401 异步恢复完成） */
		rc.LastAttemptPickFn = func(_ context.Context, model string, excluded map[string]bool) (*auth.Account, error) {
			acc, err := h.manager.PickRecentlySuccessful(model, excluded)
			if err != nil {
				return h.manager.PickExcluding(model, excluded)
			}
			return acc, nil
		}
	}
	return rc
}

/**
 * canPerformAuth401Recover 检查账号是否可以进行 401 恢复
 * 30 秒内最多允许 2 次刷新，防止陷入快速循环
 */
func (h *ProxyHandler) canPerformAuth401Recover(acc *auth.Account) bool {
	if acc == nil {
		return true
	}
	fp := acc.FilePath
	if fp == "" {
		return true
	}

	now := time.Now()
	const timeWindow = 30 * time.Second
	const maxRecoverPerWindow = 2

	val, _ := h.auth401RecoverTracks.LoadOrStore(fp, &auth401RecoverTrack{
		count:     0,
		startTime: now,
	})
	track := val.(*auth401RecoverTrack)

	/* 检查时间窗口是否过期 */
	if now.Sub(track.startTime) > timeWindow {
		/* 新窗口开始 */
		track.count = 0
		track.startTime = now
	}

	/* 检查是否超过限制 */
	if track.count >= maxRecoverPerWindow {
		return false
	}

	return true
}

/**
 * recordAuth401Recover 记录账号的一次 401 恢复
 */
func (h *ProxyHandler) recordAuth401Recover(acc *auth.Account) {
	if acc == nil {
		return
	}
	fp := acc.FilePath
	if fp == "" {
		return
	}

	const timeWindow = 30 * time.Second
	now := time.Now()

	val, _ := h.auth401RecoverTracks.LoadOrStore(fp, &auth401RecoverTrack{
		count:     0,
		startTime: now,
	})
	track := val.(*auth401RecoverTrack)

	/* 检查是否超出时间窗口 */
	if now.Sub(track.startTime) > timeWindow {
		/* 新窗口开始 */
		track.count = 1
		track.startTime = now
	} else {
		/* 同一窗口内计数增加 */
		track.count++
	}
}

/* chatStreamPumpErrorMeta 将 Pump 错误映射为 SSE data 内 OpenAI 风格 error.type/message */
func chatStreamPumpErrorMeta(execErr error) (message, typ string) {
	if errors.Is(execErr, executor.ErrEmptyResponse) {
		return "上游未返回可解析的流式内容（空响应）", "bad_gateway"
	}
	if errors.Is(execErr, context.Canceled) {
		return "请求已取消或上游连接中断", "request_cancelled"
	}
	return execErr.Error(), "api_error"
}

/**
 * handleExecutorError 统一处理 executor 返回的错误
 * @param ctx - FastHTTP 上下文
 * @param err - executor 返回的错误
 */
func handleExecutorError(ctx *fasthttp.RequestCtx, err error) {
	if errors.Is(err, context.Canceled) {
		sendError(ctx, fasthttp.StatusBadGateway, "请求已取消或客户端断开", "request_cancelled")
		return
	}
	if errors.Is(err, context.DeadlineExceeded) {
		sendError(ctx, fasthttp.StatusGatewayTimeout, "请求处理超时", "timeout")
		return
	}
	if errors.Is(err, executor.ErrEmptyResponse) {
		sendError(ctx, fasthttp.StatusBadGateway, "上游未返回有效内容（空响应）", "bad_gateway")
		return
	}
	if statusErr, ok := err.(*executor.StatusError); ok {
		if gjson.ValidBytes(statusErr.Body) {
			if gjson.GetBytes(statusErr.Body, "error").Exists() {
				ctx.SetContentType("application/json")
				ctx.SetStatusCode(statusErr.Code)
				ctx.SetBody(statusErr.Body)
				return
			}
		}
		msg := summarizeUpstreamError(statusErr.Body)
		writeJSON(ctx, statusErr.Code, map[string]any{
			"error": map[string]any{
				"message": msg,
				"type":    "api_error",
				"code":    fmt.Sprintf("upstream_%d", statusErr.Code),
			},
		})
		return
	}
	sendError(ctx, fasthttp.StatusInternalServerError, err.Error(), "server_error")
}

func summarizeUpstreamError(body []byte) string {
	if len(body) == 0 {
		return "(empty upstream response)"
	}
	if gjson.ValidBytes(body) {
		if msg := gjson.GetBytes(body, "detail").String(); msg != "" {
			return msg
		}
	}
	if len(body) > 200 {
		return string(body[:200]) + "..."
	}
	return string(body)
}

/**
 * sendError 发送 OpenAI 格式的错误响应
 */
func sendError(ctx *fasthttp.RequestCtx, status int, message, errType string) {
	writeJSON(ctx, status, map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    errType,
		},
	})
}

/**
 * handleChatCompletions 处理 Chat Completions 请求
 * 解析请求 → executor 内部选择账号/重试 → 返回响应
 * 重试逻辑在 executor 内部完成，流式请求的 SSE 头只在成功后才写给客户端
 */
func (h *ProxyHandler) handleChatCompletions(ctx *fasthttp.RequestCtx) {
	body := ctx.PostBody()
	if len(body) == 0 {
		writeJSON(ctx, fasthttp.StatusBadRequest, map[string]any{"error": map[string]any{"message": "读取请求体失败", "type": "invalid_request_error"}})
		return
	}

	model := gjson.GetBytes(body, "model").String()
	if model == "" {
		writeJSON(ctx, fasthttp.StatusBadRequest, map[string]any{"error": map[string]any{"message": "缺少 model 字段", "type": "invalid_request_error"}})
		return
	}
	if err := h.validateModelSuffixOptions(model); err != nil {
		writeJSON(ctx, fasthttp.StatusBadRequest, map[string]any{"error": map[string]any{"message": err.Error(), "type": "invalid_request_error"}})
		return
	}
	stream := gjson.GetBytes(body, "stream").Bool()

	log.Debugf("收到请求: model=%s, stream=%v", model, stream)

	rc := h.buildRetryConfig()

	if stream {
		/* 头与状态在 StreamWriter 外发送；Open+Pump 在 Writer 内完成，上游断连等在响应体尚无字节时可内部多轮全量重连，最后再向客户端写 SSE 错误 */
		ctx.Response.Header.Set("Content-Type", "text/event-stream")
		ctx.Response.Header.Set("Cache-Control", "no-cache")
		ctx.Response.Header.Set("Connection", "keep-alive")
		ctx.SetStatusCode(fasthttp.StatusOK)
		ctx.SetBodyStreamWriter(func(w *bufio.Writer) {
			/* 立即 flush 推送 SSE 头到客户端，避免上游思考期间客户端无响应超时 */
			_, _ = io.WriteString(w, ": ping\n\n")
			_ = w.Flush()
			flush := func() { _ = w.Flush() }
			sw := newStreamBufWriter(w)
			bridges := executor.CodexStreamOpenBridgeMax(h.maxRetry)
			execErr := h.executor.RunCodexStreamWithOpenBridges(context.Background(), rc, body, model, sw, flush, bridges, func(s *executor.CodexResponsesStream, w2 io.Writer, fl func()) error {
				return s.PumpChatCompletion(w2, fl)
			})
			if execErr != nil {
				log.Errorf("chat stream: %v", execErr)
				msg, typ := chatStreamPumpErrorMeta(execErr)
				writeOpenAIChatCompletionSSEError(w, msg, typ, true)
				return
			}
			RecordRequest()
		})
		return
	}

	result, execErr := h.executor.ExecuteNonStream(ctx, rc, body, model)
	if execErr != nil {
		handleExecutorError(ctx, execErr)
		return
	}
	RecordRequest()
	ctx.Response.Header.Set("Content-Type", "application/json")
	ctx.SetStatusCode(fasthttp.StatusOK)
	ctx.SetBody(result)
}

/**
 * handleStats 账号统计接口
 * 返回所有账号的状态、请求数、错误数等统计信息
 */
func (h *ProxyHandler) handleStats(ctx *fasthttp.RequestCtx) {
	args := ctx.QueryArgs()
	pageMode := len(args.Peek("page")) > 0 || len(args.Peek("page_size")) > 0 || len(args.Peek("q")) > 0 || len(args.Peek("include_quota")) > 0
	query := strings.ToLower(strings.TrimSpace(string(args.Peek("q"))))
	includeQuota := queryBoolArg(args, "include_quota")
	accounts := h.manager.GetAccounts()
	active, cooldown, disabled := 0, 0, 0
	var totalInputTokens, totalOutputTokens int64

	if !pageMode {
		stats := make([]auth.AccountStats, 0, len(accounts))
		for _, acc := range accounts {
			s := acc.GetStats()
			stats = append(stats, s)
			totalInputTokens += s.Usage.InputTokens
			totalOutputTokens += s.Usage.OutputTokens
			switch s.Status {
			case "active":
				active++
			case "cooldown":
				cooldown++
			case "disabled":
				disabled++
			}
		}

		writeJSON(ctx, fasthttp.StatusOK, map[string]any{
			"summary": map[string]any{
				"total":               len(accounts),
				"active":              active,
				"cooldown":            cooldown,
				"disabled":            disabled,
				"rpm":                 GetRPM(),
				"total_input_tokens":  totalInputTokens,
				"total_output_tokens": totalOutputTokens,
			},
			"accounts": stats,
		})
		return
	}

	page := parsePositiveIntArg(args, "page", 1, 0)
	pageSize := parsePositiveIntArg(args, "page_size", 100, statsMaxPageSize)
	pageStart := (page - 1) * pageSize
	pageEnd := pageStart + pageSize
	stats := make([]auth.AccountStats, 0, pageSize)
	filteredTotal := 0

	for _, acc := range accounts {
		s := acc.GetStats()
		totalInputTokens += s.Usage.InputTokens
		totalOutputTokens += s.Usage.OutputTokens
		switch s.Status {
		case "active":
			active++
		case "cooldown":
			cooldown++
		case "disabled":
			disabled++
		}

		if query != "" && !strings.Contains(strings.ToLower(s.Email), query) {
			continue
		}

		idx := filteredTotal
		filteredTotal++
		if idx < pageStart || idx >= pageEnd {
			continue
		}
		if !includeQuota {
			s.Quota = nil
		}
		stats = append(stats, s)
	}

	totalPages := 1
	if filteredTotal > 0 {
		totalPages = (filteredTotal + pageSize - 1) / pageSize
	}

	writeJSON(ctx, fasthttp.StatusOK, map[string]any{
		"summary": map[string]any{
			"total":               len(accounts),
			"active":              active,
			"cooldown":            cooldown,
			"disabled":            disabled,
			"rpm":                 GetRPM(),
			"total_input_tokens":  totalInputTokens,
			"total_output_tokens": totalOutputTokens,
		},
		"accounts": stats,
		"pagination": statsPagination{
			Page:          page,
			PageSize:      pageSize,
			Total:         len(accounts),
			FilteredTotal: filteredTotal,
			TotalPages:    totalPages,
			Returned:      len(stats),
			HasPrev:       page > 1 && filteredTotal > 0,
			HasNext:       page < totalPages,
			Query:         query,
		},
	})
}

func parsePositiveIntArg(args *fasthttp.Args, key string, defaultValue, maxValue int) int {
	raw := strings.TrimSpace(string(args.Peek(key)))
	if raw == "" {
		return defaultValue
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return defaultValue
	}
	if maxValue > 0 && value > maxValue {
		return maxValue
	}
	return value
}

func queryBoolArg(args *fasthttp.Args, key string) bool {
	switch strings.ToLower(strings.TrimSpace(string(args.Peek(key)))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

/**
 * handleRecoverAuth POST /recover-auth
 * 对指定账号或全部账号执行与上游 401 相同的恢复流程：同步刷新 token；遇 429 则查额度；仍失败则禁用凭据（JSON 重命名为 *.disabled）
 * 请求体 JSON：{ "email":"..." } 或 { "file_path":"..." } 指定其一；{ "all": true } 遍历当前号池全部账号（顺序执行，账号多时会较慢）
 */
func (h *ProxyHandler) handleRecoverAuth(ctx *fasthttp.RequestCtx) {
	start := time.Now()
	body := ctx.PostBody()
	if len(body) == 0 {
		writeJSON(ctx, fasthttp.StatusBadRequest, map[string]any{
			"error": map[string]any{"message": "请求体不能为空", "type": "invalid_request_error"},
		})
		return
	}
	var req struct {
		Email    string `json:"email"`
		FilePath string `json:"file_path"`
		All      bool   `json:"all"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(ctx, fasthttp.StatusBadRequest, map[string]any{
			"error": map[string]any{"message": "JSON 解析失败", "type": "invalid_request_error"},
		})
		return
	}

	/* 管理接口批量恢复：设上限避免协程永久挂起；与 /v1/chat 等对话流无关 */
	baseCtx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()

	if req.All {
		list := h.manager.GetAccounts()
		results := make([]auth.Auth401RecoverResult, 0, len(list))
		for _, acc := range list {
			results = append(results, h.manager.RecoverAuth401(baseCtx, acc, h.quotaChecker))
		}
		writeJSON(ctx, fasthttp.StatusOK, map[string]any{
			"object":      "list",
			"results":     results,
			"count":       len(results),
			"duration_ms": time.Since(start).Milliseconds(),
		})
		return
	}

	acc := h.manager.FindAccountByIdentifier(req.Email, req.FilePath)
	if acc == nil {
		writeJSON(ctx, fasthttp.StatusNotFound, map[string]any{
			"error": map[string]any{
				"message": "未找到账号，请提供 email 或 file_path，或设置 all 为 true",
				"type":    "invalid_request_error",
			},
		})
		return
	}

	r := h.manager.RecoverAuth401(baseCtx, acc, h.quotaChecker)
	writeJSON(ctx, fasthttp.StatusOK, map[string]any{
		"object":      "auth401_recover_result",
		"result":      r,
		"duration_ms": time.Since(start).Milliseconds(),
	})
}

/**
 * handleRefresh 手动刷新所有账号的 Token（SSE 流式返回进度）
 * 每刷新完一个账号就推送一条 SSE 事件，防止大量账号时超时
 * POST /refresh
 */
func (h *ProxyHandler) handleRefresh(ctx *fasthttp.RequestCtx) {
	ch := h.manager.ForceRefreshAllStream(ctx, h.quotaChecker)
	writeSSEProgress(ctx, ch)
}

/**
 * handleCheckQuota 查询所有账号的剩余额度（SSE 流式返回进度）
 * 每查询完一个账号就推送一条 SSE 事件，防止大量账号时超时
 * POST /check-quota
 */
func (h *ProxyHandler) handleCheckQuota(ctx *fasthttp.RequestCtx) {
	ch := h.quotaChecker.CheckAllStream(ctx, h.manager)
	writeSSEProgress(ctx, ch)
}

/**
 * writeSSEProgress 将 ProgressEvent channel 以 SSE 格式写入 HTTP 响应
 * @param ctx - FastHTTP 上下文
 * @param ch - 进度事件 channel
 */
func writeSSEProgress(ctx *fasthttp.RequestCtx, ch <-chan auth.ProgressEvent) {
	ctx.Response.Header.Set("Content-Type", "text/event-stream")
	ctx.Response.Header.Set("Cache-Control", "no-cache")
	ctx.Response.Header.Set("Connection", "keep-alive")
	ctx.SetStatusCode(fasthttp.StatusOK)

	/* fasthttp：StreamWriter 内禁止访问 RequestCtx（见 SetBodyStreamWriter 文档） */
	ctx.SetBodyStreamWriter(func(w *bufio.Writer) {
		for event := range ch {
			data, err := json.Marshal(event)
			if err != nil {
				continue
			}
			_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event.Type, data)
			_ = w.Flush()
		}
	})
}

/**
 * handleResponses 处理 Responses API 请求
 * 直接透传 Codex 原生 SSE 事件或 response 对象，不做 Chat Completions 格式转换
 * 重试逻辑在 executor 内部完成
 */
func (h *ProxyHandler) handleResponses(ctx *fasthttp.RequestCtx) {
	if h.enableWebSocket && isWebSocketUpgradeRequest(ctx) {
		h.handleResponsesWS(ctx)
		return
	}

	body := ctx.PostBody()
	if len(body) == 0 {
		sendError(ctx, fasthttp.StatusBadRequest, "读取请求体失败", "invalid_request_error")
		return
	}

	model := gjson.GetBytes(body, "model").String()
	if model == "" {
		sendError(ctx, fasthttp.StatusBadRequest, "缺少 model 字段", "invalid_request_error")
		return
	}
	if err := h.validateModelSuffixOptions(model); err != nil {
		sendError(ctx, fasthttp.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	stream := gjson.GetBytes(body, "stream").Bool()

	log.Debugf("收到 Responses 请求: model=%s, stream=%v", model, stream)

	rc := h.buildRetryConfig()

	if stream {
		/* 头与状态在 StreamWriter 外发送；Open+Pump 在 Writer 内完成，connection closed 等在体尚无字节时可内部多轮全量重连 */
		ctx.Response.Header.Set("Content-Type", "text/event-stream")
		ctx.Response.Header.Set("Cache-Control", "no-cache")
		ctx.Response.Header.Set("Connection", "keep-alive")
		ctx.SetStatusCode(fasthttp.StatusOK)
		ctx.SetBodyStreamWriter(func(w *bufio.Writer) {
			/* 立即 flush 推送 SSE 头到客户端，避免上游思考期间客户端无响应超时 */
			_, _ = io.WriteString(w, ": ping\n\n")
			_ = w.Flush()
			flush := func() { _ = w.Flush() }
			sw := newStreamBufWriter(w)
			bridges := executor.CodexStreamOpenBridgeMax(h.maxRetry)
			execErr := h.executor.RunCodexStreamWithOpenBridges(context.Background(), rc, body, model, sw, flush, bridges, func(s *executor.CodexResponsesStream, w2 io.Writer, fl func()) error {
				return s.PumpRawSSE(w2, fl)
			})
			if execErr != nil {
				log.Errorf("responses stream: %v", execErr)
				msg, typ := chatStreamPumpErrorMeta(execErr)
				writeOpenAIChatCompletionSSEError(w, msg, typ, true)
				return
			}
			RecordRequest()
		})
		return
	}

	result, execErr := h.executor.ExecuteResponsesNonStream(ctx, rc, body, model)
	if execErr != nil {
		handleExecutorError(ctx, execErr)
		return
	}
	RecordRequest()
	ctx.Response.Header.Set("Content-Type", "application/json")
	ctx.SetStatusCode(fasthttp.StatusOK)
	ctx.SetBody(result)
}

/* wsWriteTimeout WebSocket 写超时 */
const wsWriteTimeout = 10 * time.Second

/* wsReadTimeout WebSocket 读超时（心跳周期内收不到任何消息则关闭） */
const wsReadTimeout = 65 * time.Second

/* wsHeartbeatInterval 心跳间隔，需小于 wsReadTimeout */
const wsHeartbeatInterval = 30 * time.Second

/* wsSession 管理单个 WebSocket 连接的读写、心跳 */
type wsSession struct {
	conn      *websocket.Conn
	writeMu   sync.Mutex
	closed    chan struct{}
	closeOnce sync.Once
}

func newWSSession(conn *websocket.Conn) *wsSession {
	s := &wsSession{conn: conn, closed: make(chan struct{})}
	conn.SetReadLimit(64 << 20) // 64 MiB
	_ = conn.SetReadDeadline(time.Now().Add(wsReadTimeout))
	conn.SetPongHandler(func(string) error {
		_ = conn.SetReadDeadline(time.Now().Add(wsReadTimeout))
		return nil
	})
	s.startHeartbeat()
	return s
}

func (s *wsSession) startHeartbeat() {
	ticker := time.NewTicker(wsHeartbeatInterval)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-s.closed:
				return
			case <-ticker.C:
				s.writeMu.Lock()
				err := s.conn.WriteControl(websocket.PingMessage, []byte("ping"), time.Now().Add(wsWriteTimeout))
				s.writeMu.Unlock()
				if err != nil {
					s.close()
					return
				}
			}
		}
	}()
}

func (s *wsSession) writeMessage(msgType int, data []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_ = s.conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
	return s.conn.WriteMessage(msgType, data)
}

func (s *wsSession) close() {
	s.closeOnce.Do(func() {
		close(s.closed)
		_ = s.conn.Close()
	})
}

func (h *ProxyHandler) handleResponsesWS(ctx *fasthttp.RequestCtx) {
	log.Debugf("responses ws: 升级请求 remote=%s", ctx.RemoteAddr())
	err := responsesWSUpgrader.Upgrade(ctx, func(conn *websocket.Conn) {
		sess := newWSSession(conn)
		defer func() {
			sess.close()
			log.Debugf("responses ws: 连接关闭 remote=%s", conn.RemoteAddr())
		}()
		log.Debugf("responses ws: 连接已建立 remote=%s", conn.RemoteAddr())

		for {
			msgType, message, readErr := conn.ReadMessage()
			if readErr != nil {
				log.Debugf("responses ws: 读取错误 remote=%s err=%v", conn.RemoteAddr(), readErr)
				return
			}
			if h.debugWSStream {
				log.Debugf("ws-frame-in: type=%d len=%d payload=%s", msgType, len(message), message)
			}
			if msgType != websocket.TextMessage {
				h.writeWSErrorSession(sess, "invalid_request_error", "仅支持文本帧")
				continue
			}

			eventType := gjson.GetBytes(message, "type").String()
			switch eventType {
			case "response.create":
				respObj := gjson.GetBytes(message, "response")
				if !respObj.Exists() {
					h.writeWSErrorSession(sess, "invalid_request_error", "缺少 response 字段")
					continue
				}

				requestBody := []byte(respObj.Raw)
				requestBody, _ = sjson.SetBytes(requestBody, "stream", true)

				model := gjson.GetBytes(requestBody, "model").String()
				if model == "" {
					h.writeWSErrorSession(sess, "invalid_request_error", "缺少 model 字段")
					continue
				}
				if err := h.validateModelSuffixOptions(model); err != nil {
					h.writeWSErrorSession(sess, "invalid_request_error", err.Error())
					continue
				}

				log.Debugf("responses ws: model=%s", model)
				rc := h.buildRetryConfig()
				streamErr := h.forwardResponsesSSEAsWSSession(ctx, sess, rc, requestBody, model)
				if streamErr == nil {
					RecordRequest()
				} else if errors.Is(streamErr, executor.ErrEmptyResponse) {
					h.writeWSErrorSession(sess, "invalid_response", "empty response")
				} else if statusErr, ok := streamErr.(*executor.StatusError); ok {
					h.writeWSErrorSession(sess, "api_error", summarizeUpstreamError(statusErr.Body))
				} else {
					h.writeWSErrorSession(sess, "api_error", streamErr.Error())
				}

			case "response.cancel", "response.close":
				sess.writeMu.Lock()
				_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "closed"), time.Now().Add(2*time.Second))
				sess.writeMu.Unlock()
				return

			default:
				h.writeWSErrorSession(sess, "invalid_request_error", "不支持的事件类型: "+eventType)
			}
		}
	})
	if err != nil {
		log.Warnf("responses ws upgrade 失败: %v", err)
	}
}

func (h *ProxyHandler) forwardResponsesSSEAsWSSession(ctx context.Context, sess *wsSession, rc executor.RetryConfig, requestBody []byte, model string) error {
	bridges := executor.CodexStreamOpenBridgeMax(h.maxRetry)
	/* wsNopWriter 仅负责计数，实际 WS 写入在 pump 内完成 */
	return h.executor.RunCodexStreamWithOpenBridges(ctx, rc, requestBody, model,
		&wsNopWriter{}, func() {}, bridges,
		func(s *executor.CodexResponsesStream, w io.Writer, flush func()) error {
			return h.pumpSSEToWSSession(s, sess, w, ctx)
		})
}

/* wsNopWriter 仅用于 RunCodexStreamWithOpenBridges 的 countingWriter 计数，实际写入走 sess.writeMessage */
type wsNopWriter struct{}

func (w *wsNopWriter) Write(p []byte) (int, error) { return len(p), nil }

func (h *ProxyHandler) pumpSSEToWSSession(s *executor.CodexResponsesStream, sess *wsSession, countW io.Writer, ctx context.Context) error {
	hasContent := false
	flushed := false
	var buffer [][]byte

	scanner := bufio.NewScanner(s.Body())
	scanner.Buffer(make([]byte, scannerInitSize), scannerMaxSize)

	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(line[5:])
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}

		if h.debugWSStream {
			log.Debugf("ws-frame-out: %s", payload)
		}

		if !hasContent {
			typ := gjson.GetBytes(payload, "type").String()
			switch typ {
			case "response.output_text.delta":
				if gjson.GetBytes(payload, "delta").String() != "" {
					hasContent = true
				}
			case "response.output_item.added", "response.function_call_arguments.delta",
				"response.function_call_arguments.done", "response.output_item.done":
				hasContent = true
			case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
				hasContent = true
			}
		}

		if !flushed && hasContent {
			for _, buf := range buffer {
				if writeErr := sess.writeMessage(websocket.TextMessage, buf); writeErr != nil {
					return writeErr
				}
				/* 向 countW 写入以便 bridge 计数 */
				_, _ = countW.Write(buf)
			}
			buffer = nil
			flushed = true
		}

		if flushed {
			if writeErr := sess.writeMessage(websocket.TextMessage, payload); writeErr != nil {
				return writeErr
			}
			_, _ = countW.Write(payload)
		} else {
			payloadCopy := make([]byte, len(payload))
			copy(payloadCopy, payload)
			buffer = append(buffer, payloadCopy)
		}
	}

	if scanErr := scanner.Err(); scanErr != nil {
		if errors.Is(scanErr, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
			if hasContent {
				return nil
			}
			return scanErr
		}
		return scanErr
	}

	if !hasContent {
		return executor.ErrEmptyResponse
	}
	s.Account().RecordSuccess()
	return nil
}

func (h *ProxyHandler) writeWSError(conn *websocket.Conn, errType, message string) {
	errBody := `{"type":"error","error":{"type":"","message":""}}`
	errBody, _ = sjson.Set(errBody, "error.type", errType)
	errBody, _ = sjson.Set(errBody, "error.message", message)
	_ = conn.WriteMessage(websocket.TextMessage, []byte(errBody))
}

func (h *ProxyHandler) writeWSErrorSession(sess *wsSession, errType, message string) {
	errBody := `{"type":"error","error":{"type":"","message":""}}`
	errBody, _ = sjson.Set(errBody, "error.type", errType)
	errBody, _ = sjson.Set(errBody, "error.message", message)
	_ = sess.writeMessage(websocket.TextMessage, []byte(errBody))
}

/**
 * handleResponsesCompact 处理 Responses Compact API 请求
 * 使用 /responses/compact 端点，直接透传 compact 格式（CBOR/SSE）响应
 * 重试逻辑在 executor 内部完成
 */
func (h *ProxyHandler) handleResponsesCompact(ctx *fasthttp.RequestCtx) {
	body := ctx.PostBody()
	if len(body) == 0 {
		sendError(ctx, fasthttp.StatusBadRequest, "读取请求体失败", "invalid_request_error")
		return
	}

	model := gjson.GetBytes(body, "model").String()
	if model == "" {
		sendError(ctx, fasthttp.StatusBadRequest, "缺少 model 字段", "invalid_request_error")
		return
	}
	if err := h.validateModelSuffixOptions(model); err != nil {
		sendError(ctx, fasthttp.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	stream := gjson.GetBytes(body, "stream").Bool()

	log.Debugf("收到 Responses Compact 请求: model=%s, stream=%v", model, stream)

	rc := h.buildRetryConfig()

	if stream {
		compact, openErr := h.executor.OpenCodexCompactStream(ctx, rc, body, model)
		if openErr != nil {
			handleExecutorError(ctx, openErr)
			return
		}
		for k, vs := range compact.Resp.Header {
			for _, v := range vs {
				ctx.Response.Header.Add(k, v)
			}
		}
		ctx.SetStatusCode(fasthttp.StatusOK)
		ctx.SetBodyStreamWriter(func(w *bufio.Writer) {
			flush := func() { _ = w.Flush() }
			if execErr := compact.PumpBody(newStreamBufWriter(w), flush); execErr != nil {
				log.Errorf("compact stream pump: %v", execErr)
				return
			}
			compact.Account.RecordSuccess()
			RecordRequest()
		})
		return
	}

	result, execErr := h.executor.ExecuteResponsesCompactNonStream(ctx, rc, body, model)
	if execErr != nil {
		handleExecutorError(ctx, execErr)
		return
	}
	RecordRequest()
	ctx.Response.Header.Set("Content-Type", "application/json")
	ctx.SetStatusCode(fasthttp.StatusOK)
	ctx.SetBody(result)
}
