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
	"strconv"
	"strings"
	"time"

	"codex-proxy/internal/auth"
	"codex-proxy/internal/executor"

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
 */
type ProxyHandler struct {
	manager            *auth.Manager
	executor           *executor.Executor
	apiKeys            []string
	maxRetry           int
	enableHealthyRetry bool
	quotaChecker       *auth.QuotaChecker
	indexHTML          []byte
	emptyRetryMax      int
}

/**
 * NewProxyHandler 创建新的代理处理器
 * @param manager - 账号管理器
 * @param exec - Codex 执行器
 * @param apiKeys - API Key 列表
 * @param maxRetry - 最大重试次数（0 表示不重试）
 * @param quotaCheckConcurrency - 额度查询并发数（来自 config）
 * @returns *ProxyHandler - 代理处理器实例
 */
func NewProxyHandler(manager *auth.Manager, exec *executor.Executor, apiKeys []string, maxRetry int, enableHealthyRetry bool, proxyURL string, baseURL string, enableHTTP2 bool, backendDomain string, backendResolveAddress string, quotaCheckConcurrency int, emptyRetryMax int, indexHTML []byte) *ProxyHandler {
	if maxRetry < 0 {
		maxRetry = 0
	}
	if quotaCheckConcurrency <= 0 {
		quotaCheckConcurrency = 50
	}
	return &ProxyHandler{
		manager:            manager,
		executor:           exec,
		apiKeys:            apiKeys,
		maxRetry:           maxRetry,
		enableHealthyRetry: enableHealthyRetry,
		quotaChecker:       auth.NewQuotaChecker(baseURL, proxyURL, quotaCheckConcurrency, enableHTTP2, backendDomain, backendResolveAddress),
		indexHTML:          indexHTML,
		emptyRetryMax:      emptyRetryMax,
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
	// r.POST("/v1/responses/compact", ... ) 原接口暂不支持

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

/**
 * handleModels 模型列表接口
 * 按 README 表格生成：每个基础模型 + 其支持的思考等级 + 均可加 -fast
 */
func (h *ProxyHandler) handleModels(ctx *fasthttp.RequestCtx) {
	models := make([]map[string]interface{}, 0, 50*20)
	for _, e := range modelList {
		models = append(models, map[string]interface{}{"id": e.base, "object": "model", "owned_by": "openai"})
		models = append(models, map[string]interface{}{"id": e.base + "-fast", "object": "model", "owned_by": "openai"})
		for _, s := range e.suffixes {
			models = append(models, map[string]interface{}{"id": e.base + "-" + s, "object": "model", "owned_by": "openai"})
			models = append(models, map[string]interface{}{"id": e.base + "-" + s + "-fast", "object": "model", "owned_by": "openai"})
		}
	}

	writeJSON(ctx, fasthttp.StatusOK, map[string]interface{}{
		"object": "list",
		"data":   models,
	})
}

/**
 * buildRetryConfig 构建 executor 内部重试配置
 * 将 handler 的账号选择和 401 处理逻辑封装为回调传给 executor
 * @returns executor.RetryConfig - 重试配置
 */
func (h *ProxyHandler) buildRetryConfig() executor.RetryConfig {
	healthyPick := func(model string, excluded map[string]bool) (*auth.Account, error) {
		return h.manager.PickRecentlySuccessful(model, excluded)
	}
	rc := executor.RetryConfig{
		PickFn: func(model string, excluded map[string]bool) (*auth.Account, error) {
			return h.manager.PickExcluding(model, excluded)
		},
		On401Fn: func(acc *auth.Account) bool {
			r := h.manager.HandleAuth401(acc, h.quotaChecker)
			return r.Status == auth.Auth401RecoverRefreshed || r.Status == auth.Auth401RecoverCooldown429OK
		},
		On429RecoveryFn: func(ctx context.Context, acc *auth.Account) {
			h.manager.ScheduleUpstream429Recovery(ctx, acc, h.quotaChecker)
		},
		OnAfterUpstreamErrFn: func(acc *auth.Account, statusCode int) {
			if statusCode >= 200 && statusCode < 300 {
				return
			}
			h.manager.InvalidateSelectorCache()
		},
		MaxRetry:      h.maxRetry,
		EmptyRetryMax: h.emptyRetryMax,
	}
	if h.enableHealthyRetry {
		rc.HealthyPickFn = healthyPick
		if h.maxRetry >= 2 {
			/* 前 max-retry-1 次用常规换号，之后用最近成功账号，减少无效轮询 */
			rc.HealthyPickMinAttempt = h.maxRetry - 1
		}
		/* 常规尝试用尽后，sendWithRetry 末尾再保底一次「最近成功账号」（可重试已排除的号，见 PickRecentlySuccessful） */
		rc.FallbackRecentPickFn = healthyPick
	}
	return rc
}

/**
 * handleExecutorError 统一处理 executor 返回的错误
 * @param ctx - FastHTTP 上下文
 * @param err - executor 返回的错误
 */
func handleExecutorError(ctx *fasthttp.RequestCtx, err error) {
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
	stream := gjson.GetBytes(body, "stream").Bool()

	log.Debugf("收到请求: model=%s, stream=%v", model, stream)

	rc := h.buildRetryConfig()

	if stream {
		up, openErr := h.executor.OpenCodexResponsesStream(ctx, rc, body, model)
		if openErr != nil {
			handleExecutorError(ctx, openErr)
			return
		}
		ctx.Response.Header.Set("Content-Type", "text/event-stream")
		ctx.Response.Header.Set("Cache-Control", "no-cache")
		ctx.Response.Header.Set("Connection", "keep-alive")
		ctx.SetStatusCode(fasthttp.StatusOK)
		ctx.SetBodyStreamWriter(func(w *bufio.Writer) {
			flush := func() { _ = w.Flush() }
			if execErr := up.PumpChatCompletion(newStreamBufWriter(w), flush); execErr != nil {
				log.Errorf("chat stream pump: %v", execErr)
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
	if isWebSocketUpgradeRequest(ctx) {
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
	stream := gjson.GetBytes(body, "stream").Bool()

	log.Debugf("收到 Responses 请求: model=%s, stream=%v", model, stream)

	rc := h.buildRetryConfig()

	if stream {
		up, openErr := h.executor.OpenCodexResponsesStream(ctx, rc, body, model)
		if openErr != nil {
			handleExecutorError(ctx, openErr)
			return
		}
		ctx.Response.Header.Set("Content-Type", "text/event-stream")
		ctx.Response.Header.Set("Cache-Control", "no-cache")
		ctx.Response.Header.Set("Connection", "keep-alive")
		ctx.SetStatusCode(fasthttp.StatusOK)
		ctx.SetBodyStreamWriter(func(w *bufio.Writer) {
			flush := func() { _ = w.Flush() }
			if execErr := up.PumpRawSSE(newStreamBufWriter(w), flush); execErr != nil {
				log.Errorf("responses stream pump: %v", execErr)
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

func (h *ProxyHandler) handleResponsesWS(ctx *fasthttp.RequestCtx) {
	err := responsesWSUpgrader.Upgrade(ctx, func(conn *websocket.Conn) {
		defer func() {
			_ = conn.Close()
		}()

		for {
			msgType, message, readErr := conn.ReadMessage()
			if readErr != nil {
				return
			}
			if msgType != websocket.TextMessage {
				h.writeWSError(conn, "invalid_request_error", "仅支持文本帧")
				continue
			}

			eventType := gjson.GetBytes(message, "type").String()
			switch eventType {
			case "response.create":
				respObj := gjson.GetBytes(message, "response")
				if !respObj.Exists() {
					h.writeWSError(conn, "invalid_request_error", "缺少 response 字段")
					continue
				}

				requestBody := []byte(respObj.Raw)
				requestBody, _ = sjson.SetBytes(requestBody, "stream", true)

				model := gjson.GetBytes(requestBody, "model").String()
				if model == "" {
					h.writeWSError(conn, "invalid_request_error", "缺少 model 字段")
					continue
				}

				log.Debugf("responses ws: model=%s", model)
				rc := h.buildRetryConfig()
				streamErr := h.forwardResponsesSSEAsWS(ctx, conn, rc, requestBody, model)
				if streamErr == nil {
					RecordRequest()
				} else if errors.Is(streamErr, executor.ErrEmptyResponse) {
					h.writeWSError(conn, "invalid_response", "empty response")
				} else if statusErr, ok := streamErr.(*executor.StatusError); ok {
					h.writeWSError(conn, "api_error", summarizeUpstreamError(statusErr.Body))
				} else {
					h.writeWSError(conn, "api_error", streamErr.Error())
				}

			case "response.cancel", "response.close":
				_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "closed"), time.Now().Add(2*time.Second))
				return

			default:
				h.writeWSError(conn, "invalid_request_error", "不支持的事件类型: "+eventType)
			}
		}
	})
	if err != nil {
		log.Warnf("responses ws upgrade 失败: %v", err)
	}
}

func (h *ProxyHandler) forwardResponsesSSEAsWS(ctx context.Context, conn *websocket.Conn, rc executor.RetryConfig, requestBody []byte, model string) error {
	startTotal := time.Now()
	emptyRetryMax := h.emptyRetryMax
	if emptyRetryMax < 0 {
		emptyRetryMax = 0
	}
	excludedForEmpty := make(map[string]bool)

	for emptyAttempt := 0; emptyAttempt <= emptyRetryMax; emptyAttempt++ {
		rcExcl := executor.MergeRetryConfigExcluded(rc, excludedForEmpty)

		rawResp, account, attempts, baseModel, convertDur, sendDur, err := h.executor.OpenResponsesStream(ctx, rcExcl, requestBody, model)
		if err != nil {
			return err
		}

		hasContent, streamErr := h.pipeSSEToWS(conn, rawResp, ctx)
		if streamErr != nil {
			log.Infof("req summary responses-ws model=%s account=%s attempts=%d convert=%v upstream=%v total=%v (ERR)", baseModel, account.GetEmail(), attempts, convertDur, sendDur, time.Since(startTotal))
			return streamErr
		}

		if hasContent {
			account.RecordSuccess()
			log.Infof("req summary responses-ws model=%s account=%s attempts=%d convert=%v upstream=%v total=%v", baseModel, account.GetEmail(), attempts, convertDur, sendDur, time.Since(startTotal))
			return nil
		}

		excludedForEmpty[account.FilePath] = true
		if emptyAttempt < emptyRetryMax {
			log.Warnf("WS 空返回，换号重试 (account=%s attempt=%d/%d)", account.GetEmail(), emptyAttempt+1, emptyRetryMax+1)
		}
	}

	log.Infof("req summary responses-ws (empty after %d tries) total=%v", emptyRetryMax+1, time.Since(startTotal))
	return executor.ErrEmptyResponse
}

func (h *ProxyHandler) pipeSSEToWS(conn *websocket.Conn, rawResp *executor.RawResponse, ctx context.Context) (bool, error) {
	defer func() {
		if rawResp.Body != nil {
			_ = rawResp.Body.Close()
		}
	}()

	hasContent := false
	flushed := false
	var buffer [][]byte

	scanner := bufio.NewScanner(rawResp.Body)
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
				if writeErr := conn.WriteMessage(websocket.TextMessage, buf); writeErr != nil {
					return true, writeErr
				}
			}
			buffer = nil
			flushed = true
		}

		if flushed {
			if writeErr := conn.WriteMessage(websocket.TextMessage, payload); writeErr != nil {
				return hasContent, writeErr
			}
		} else {
			payloadCopy := make([]byte, len(payload))
			copy(payloadCopy, payload)
			buffer = append(buffer, payloadCopy)
		}
	}

	if scanErr := scanner.Err(); scanErr != nil {
		if errors.Is(scanErr, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
			return hasContent, nil
		}
		return hasContent, scanErr
	}

	return hasContent, nil
}

func (h *ProxyHandler) writeWSError(conn *websocket.Conn, errType, message string) {
	errBody := `{"type":"error","error":{"type":"","message":""}}`
	errBody, _ = sjson.Set(errBody, "error.type", errType)
	errBody, _ = sjson.Set(errBody, "error.message", message)
	_ = conn.WriteMessage(websocket.TextMessage, []byte(errBody))
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
