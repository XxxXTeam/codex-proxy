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
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"codex-proxy/internal/auth"
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
	sseDataPrefix = []byte("data: ")
	sseDataSuffix = []byte("\n\n")
	sseDoneMarker = []byte("data: [DONE]\n\n")
)

/**
 * Executor Codex 请求执行器
 * 使用全局共享连接池提升高并发性能
 * @field baseURL - Codex API 基础 URL
 * @field httpClient - 共享的 HTTP 客户端（连接池复用）
 */
type Executor struct {
	baseURL       string
	httpClient    *http.Client
	keepAliveOnce sync.Once
}

/**
 * NewExecutor 创建新的 Codex 执行器
 * 初始化全局连接池，支持高并发场景
 * @param baseURL - API 基础 URL
 * @param proxyURL - 代理地址
 * @returns *Executor - 执行器实例
 */
func NewExecutor(baseURL, proxyURL string) *Executor {
	if baseURL == "" {
		baseURL = "https://chatgpt.com/backend-api/codex"
	}

	transport := &http.Transport{
		MaxIdleConns:          200,
		MaxIdleConnsPerHost:   50,
		MaxConnsPerHost:       100,
		IdleConnTimeout:       300 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		WriteBufferSize:       32 * 1024,
		ReadBufferSize:        32 * 1024,
		ForceAttemptHTTP2:     true,
		DisableCompression:    true, /* SSE 流不需要 gzip 解压开销 */
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: false},
	}

	if proxyURL != "" {
		proxyParsed, err := url.Parse(proxyURL)
		if err == nil {
			transport.Proxy = http.ProxyURL(proxyParsed)
		}
	}

	return &Executor{
		baseURL: strings.TrimSuffix(baseURL, "/"),
		httpClient: &http.Client{
			Transport: transport,
			Timeout:   5 * time.Minute,
		},
	}
}

/**
 * RetryConfig 内部重试配置
 * 将重试逻辑封装在 executor 内部，在写响应头之前完成账号切换重试
 * 客户端完全不感知重试过程，流不会被切断
 * @field PickFn - 账号选择函数，接收模型名和已排除账号集合
 * @field On401Fn - 401 错误回调（用于触发后台 Token 刷新）
 * @field MaxRetry - 最大重试次数（0 表示不重试）
 */
type RetryConfig struct {
	PickFn   func(model string, excluded map[string]bool) (*auth.Account, error)
	On401Fn  func(acc *auth.Account)
	MaxRetry int
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
func (e *Executor) sendWithRetry(ctx context.Context, rc RetryConfig, model string, apiURL string, codexBody []byte, stream bool) (*http.Response, *auth.Account, error) {
	excluded := make(map[string]bool)
	maxAttempts := rc.MaxRetry + 1
	var lastErr error

	for attempt := 0; attempt < maxAttempts; attempt++ {
		if ctx.Err() != nil {
			break
		}

		account, err := rc.PickFn(model, excluded)
		if err != nil {
			if attempt == 0 {
				return nil, nil, err
			}
			break
		}
		excluded[account.FilePath] = true

		log.Debugf("使用账号: %s (尝试 %d/%d)", account.GetEmail(), attempt+1, maxAttempts)

		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(codexBody))
		if err != nil {
			return nil, nil, fmt.Errorf("创建请求失败: %w", err)
		}
		applyCodexHeaders(httpReq, account, stream)

		httpResp, err := e.httpClient.Do(httpReq)
		if err != nil {
			account.RecordFailure()
			lastErr = fmt.Errorf("请求发送失败: %w", err)
			if attempt < maxAttempts-1 {
				log.Warnf("账号 [%s] 网络错误，切换账号重试: %v", account.GetEmail(), err)
				continue
			}
			break
		}

		/* 2xx 成功 */
		if httpResp.StatusCode >= 200 && httpResp.StatusCode < 300 {
			return httpResp, account, nil
		}

		/* 错误状态码：读取错误体、处理账号状态、判断是否可重试 */
		errBody, _ := io.ReadAll(io.LimitReader(httpResp.Body, 1<<20))
		_ = httpResp.Body.Close()

		handleAccountError(account, httpResp.StatusCode, errBody)

		if httpResp.StatusCode == 401 && rc.On401Fn != nil {
			rc.On401Fn(account)
		}

		statusErr := &StatusError{Code: httpResp.StatusCode, Body: errBody}
		lastErr = statusErr

		if !IsRetryableStatus(httpResp.StatusCode) {
			return nil, nil, statusErr
		}

		if attempt < maxAttempts-1 {
			log.Warnf("账号 [%s] 请求失败 [%d]，切换账号重试", account.GetEmail(), httpResp.StatusCode)
			continue
		}
	}

	if lastErr != nil {
		return nil, nil, lastErr
	}
	return nil, nil, fmt.Errorf("请求失败")
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
	body, baseModel := thinking.ApplyThinking(requestBody, model)
	codexBody := translator.ConvertOpenAIRequestToCodex(baseModel, body, true)
	apiURL := e.baseURL + "/responses"

	httpResp, account, err := e.sendWithRetry(ctx, rc, model, apiURL, codexBody, true)
	if err != nil {
		return err
	}
	defer func() {
		_ = httpResp.Body.Close()
	}()

	/* 只有到这里才开始写 SSE 头（重试在上面已完成） */
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("Cache-Control", "no-cache")
	writer.Header().Set("Connection", "keep-alive")
	writer.WriteHeader(http.StatusOK)

	flusher, canFlush := writer.(http.Flusher)
	reverseToolMap := translator.BuildReverseToolNameMap(requestBody)
	state := translator.NewStreamState(baseModel)
	scanner := bufio.NewScanner(httpResp.Body)
	scanner.Buffer(make([]byte, 4*1024), 50*1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		/* 不再调用 extractUsageFromStreamLine，ConvertStreamChunk 内部已提取 usage 到 state */
		chunks := translator.ConvertStreamChunk(ctx, line, state, reverseToolMap)
		for _, chunk := range chunks {
			_, _ = writer.Write(sseDataPrefix)
			_, _ = io.WriteString(writer, chunk)
			_, _ = writer.Write(sseDataSuffix)
			if canFlush {
				flusher.Flush()
			}
		}
	}

	if err = scanner.Err(); err != nil {
		log.Errorf("读取流式响应失败: %v", err)
		return err
	}

	_, _ = writer.Write(sseDoneMarker)
	if canFlush {
		flusher.Flush()
	}

	/* 从 state 中读取 usage（ConvertStreamChunk 在 response.completed 时已提取） */
	if state.UsageInput > 0 || state.UsageOutput > 0 {
		account.RecordUsage(state.UsageInput, state.UsageOutput, state.UsageTotal)
	}
	account.RecordSuccess()
	return nil
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
	body, baseModel := thinking.ApplyThinking(requestBody, model)
	codexBody := translator.ConvertOpenAIRequestToCodex(baseModel, body, true)
	apiURL := e.baseURL + "/responses"

	httpResp, account, err := e.sendWithRetry(ctx, rc, model, apiURL, codexBody, true)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = httpResp.Body.Close()
	}()

	reverseToolMap := translator.BuildReverseToolNameMap(requestBody)
	scanner := bufio.NewScanner(httpResp.Body)
	scanner.Buffer(make([]byte, 4*1024), 50*1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		jsonData := bytes.TrimSpace(line[5:])
		if gjson.GetBytes(jsonData, "type").String() != "response.completed" {
			continue
		}
		/* 提取 usage 并记录 */
		usage := gjson.GetBytes(jsonData, "response.usage")
		if usage.Exists() {
			account.RecordUsage(
				usage.Get("input_tokens").Int(),
				usage.Get("output_tokens").Int(),
				usage.Get("total_tokens").Int(),
			)
		}
		result := translator.ConvertNonStreamResponse(ctx, jsonData, reverseToolMap)
		if result != "" {
			account.RecordSuccess()
			return []byte(result), nil
		}
	}

	if err = scanner.Err(); err != nil {
		return nil, fmt.Errorf("读取响应失败: %w", err)
	}

	return nil, fmt.Errorf("未收到 response.completed 事件")
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
	body, baseModel := thinking.ApplyThinking(requestBody, model)
	codexBody := translator.ConvertOpenAIRequestToCodex(baseModel, body, true)
	apiURL := e.baseURL + "/responses"

	httpResp, account, err := e.sendWithRetry(ctx, rc, model, apiURL, codexBody, true)
	if err != nil {
		return err
	}
	defer func() {
		_ = httpResp.Body.Close()
	}()

	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("Cache-Control", "no-cache")
	writer.Header().Set("Connection", "keep-alive")
	writer.WriteHeader(http.StatusOK)

	flusher, canFlush := writer.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, readErr := httpResp.Body.Read(buf)
		if n > 0 {
			_, _ = writer.Write(buf[:n])
			if canFlush {
				flusher.Flush()
			}
		}
		if readErr != nil {
			if readErr != io.EOF {
				log.Errorf("读取流式响应失败: %v", readErr)
				return readErr
			}
			break
		}
	}

	account.RecordSuccess()
	return nil
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
	body, baseModel := thinking.ApplyThinking(requestBody, model)
	codexBody := translator.ConvertOpenAIRequestToCodex(baseModel, body, true)
	apiURL := e.baseURL + "/responses"

	httpResp, account, err := e.sendWithRetry(ctx, rc, model, apiURL, codexBody, true)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = httpResp.Body.Close()
	}()

	scanner := bufio.NewScanner(httpResp.Body)
	scanner.Buffer(make([]byte, 4*1024), 50*1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		jsonData := bytes.TrimSpace(line[5:])
		if gjson.GetBytes(jsonData, "type").String() != "response.completed" {
			continue
		}
		/* 提取 usage 并记录 */
		usage := gjson.GetBytes(jsonData, "response.usage")
		if usage.Exists() {
			account.RecordUsage(
				usage.Get("input_tokens").Int(),
				usage.Get("output_tokens").Int(),
				usage.Get("total_tokens").Int(),
			)
		}
		if resp := gjson.GetBytes(jsonData, "response"); resp.Exists() {
			account.RecordSuccess()
			return []byte(resp.Raw), nil
		}
	}

	if err = scanner.Err(); err != nil {
		return nil, fmt.Errorf("读取响应失败: %w", err)
	}

	return nil, fmt.Errorf("未收到 response.completed 事件")
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
	body, baseModel := thinking.ApplyThinking(requestBody, model)
	codexBody := cleanCompactBody(body, baseModel)
	apiURL := e.baseURL + "/responses/compact"

	httpResp, account, err := e.sendWithRetry(ctx, rc, model, apiURL, codexBody, true)
	if err != nil {
		return err
	}
	defer func() {
		_ = httpResp.Body.Close()
	}()

	/* 透传响应头 */
	for k, vs := range httpResp.Header {
		for _, v := range vs {
			writer.Header().Add(k, v)
		}
	}
	writer.WriteHeader(http.StatusOK)

	flusher, canFlush := writer.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, readErr := httpResp.Body.Read(buf)
		if n > 0 {
			_, _ = writer.Write(buf[:n])
			if canFlush {
				flusher.Flush()
			}
		}
		if readErr != nil {
			break
		}
	}

	account.RecordSuccess()
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
	body, baseModel := thinking.ApplyThinking(requestBody, model)
	codexBody := cleanCompactBody(body, baseModel)
	apiURL := e.baseURL + "/responses/compact"

	httpResp, account, err := e.sendWithRetry(ctx, rc, model, apiURL, codexBody, false)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = httpResp.Body.Close()
	}()

	data, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取响应失败: %w", err)
	}

	account.RecordSuccess()
	return data, nil
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
	body, baseModel := thinking.ApplyThinking(requestBody, model)
	codexBody := translator.ConvertOpenAIRequestToCodex(baseModel, body, true)
	apiURL := e.baseURL + "/responses"

	httpResp, account, err := e.sendWithRetry(ctx, rc, model, apiURL, codexBody, true)
	if err != nil {
		return nil, nil, err
	}

	return &RawResponse{StatusCode: httpResp.StatusCode, Body: httpResp.Body}, account, nil
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
		log.Debugf("账号 [%s] 请求 429，冷却 %v", account.GetEmail(), cooldown)
	case statusCode == 403:
		account.SetCooldown(5 * time.Minute)
		log.Debugf("账号 [%s] 请求 403，冷却 5m", account.GetEmail())
	default:
		log.Warnf("账号 [%s] 请求失败 [%d]: %s", account.GetEmail(), statusCode, summarizeError(body))
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
			/* 每 60 秒 ping 一次上游，保持连接池热度 */
			ticker := time.NewTicker(60 * time.Second)
			defer ticker.Stop()

			pingURL := strings.TrimSuffix(e.baseURL, "/codex")
			if pingURL == "" {
				pingURL = "https://chatgpt.com"
			}

			log.Infof("连接保活已启动，每 60 秒 ping %s", pingURL)

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
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
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
