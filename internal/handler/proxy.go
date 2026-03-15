/**
 * HTTP 代理处理器模块
 * 提供 OpenAI 兼容的 API 端点，接收请求后通过 Codex 执行器转发
 * 支持流式和非流式响应、API Key 鉴权、模型列表接口
 */
package handler

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"codex-proxy/internal/auth"
	"codex-proxy/internal/executor"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

/**
 * ProxyHandler 代理处理器
 * @field manager - 账号管理器
 * @field executor - Codex 执行器
 * @field apiKeys - 允许访问的 API Key 列表（为空则不鉴权）
 * @field maxRetry - 请求失败最大重试次数（切换账号重试）
 */
type ProxyHandler struct {
	manager      *auth.Manager
	executor     *executor.Executor
	apiKeys      []string
	maxRetry     int
	quotaChecker *auth.QuotaChecker
	indexHTML    []byte
}

/**
 * NewProxyHandler 创建新的代理处理器
 * @param manager - 账号管理器
 * @param exec - Codex 执行器
 * @param apiKeys - API Key 列表
 * @param maxRetry - 最大重试次数（0 表示不重试）
 * @returns *ProxyHandler - 代理处理器实例
 */
func NewProxyHandler(manager *auth.Manager, exec *executor.Executor, apiKeys []string, maxRetry int, proxyURL string, indexHTML []byte) *ProxyHandler {
	if maxRetry < 0 {
		maxRetry = 0
	}
	return &ProxyHandler{
		manager:      manager,
		executor:     exec,
		apiKeys:      apiKeys,
		maxRetry:     maxRetry,
		quotaChecker: auth.NewQuotaChecker(proxyURL, 50),
		indexHTML:    indexHTML,
	}
}

/**
 * RegisterRoutes 注册所有 HTTP 路由
 * @param r - Gin 引擎实例
 */
func (h *ProxyHandler) RegisterRoutes(r *gin.Engine) {
	/* 首页 */
	r.GET("/", h.handleIndex)

	/* 健康检查 */
	r.GET("/health", h.handleHealth)

	/* OpenAI 兼容接口 */
	api := r.Group("/v1")
	if len(h.apiKeys) > 0 {
		api.Use(h.authMiddleware())
	}
	api.POST("/chat/completions", h.handleChatCompletions)
	api.POST("/responses", h.handleResponses)
	api.POST("/responses/compact", h.handleResponsesCompact)
	api.POST("/messages", h.handleMessages)
	api.GET("/models", h.handleModels)

	/* 管理接口（配置了 API Key 时需要鉴权） */
	mgmt := r.Group("")
	if len(h.apiKeys) > 0 {
		mgmt.Use(h.authMiddleware())
	}
	mgmt.GET("/stats", h.handleStats)
	mgmt.POST("/refresh", h.handleRefresh)
	mgmt.POST("/check-quota", h.handleCheckQuota)
}

/**
 * authMiddleware API Key 鉴权中间件
 * @returns gin.HandlerFunc - Gin 中间件
 */
func (h *ProxyHandler) authMiddleware() gin.HandlerFunc {
	keySet := make(map[string]struct{}, len(h.apiKeys))
	for _, k := range h.apiKeys {
		k = strings.TrimSpace(k)
		if k != "" {
			keySet[k] = struct{}{}
		}
	}

	return func(c *gin.Context) {
		if len(keySet) == 0 {
			c.Next()
			return
		}

		authHeader := c.GetHeader("Authorization")
		token := strings.TrimPrefix(authHeader, "Bearer ")
		token = strings.TrimSpace(token)

		if _, ok := keySet[token]; !ok {
			c.JSON(http.StatusUnauthorized, gin.H{
				"error": gin.H{
					"message": "无效的 API Key",
					"type":    "invalid_request_error",
					"code":    "invalid_api_key",
				},
			})
			c.Abort()
			return
		}
		c.Next()
	}
}

/**
 * handleHealth 健康检查接口
 */
func (h *ProxyHandler) handleHealth(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"status":   "ok",
		"accounts": h.manager.AccountCount(),
	})
}

/**
 * baseModelList 基础模型名列表（与 thinking/suffix.go 中 knownBaseModels 保持一致）
 */
var baseModelList = []string{
	"gpt-5", "gpt-5-codex", "gpt-5-codex-mini",
	"gpt-5.1", "gpt-5.1-codex", "gpt-5.1-codex-mini", "gpt-5.1-codex-max",
	"gpt-5.2", "gpt-5.2-codex",
	"gpt-5.3-codex", "gpt-5.3-codex-spark",
	"gpt-5.4",
	"codex-mini",
}

/**
 * thinkingSuffixes 所有可用的思考等级后缀
 */
var thinkingSuffixes = []string{
	"low", "medium", "high", "xhigh", "max", "none", "auto",
}

/**
 * handleModels 模型列表接口
 * 为每个基础模型自动生成全部思考等级后缀变体和 fast 模式变体
 * 格式：model、model-level、model-fast、model-level-fast
 */
func (h *ProxyHandler) handleModels(c *gin.Context) {
	models := make([]gin.H, 0, len(baseModelList)*(2+len(thinkingSuffixes)*2))

	for _, base := range baseModelList {
		/* 基础模型（无后缀） */
		models = append(models, gin.H{"id": base, "object": "model", "owned_by": "openai"})
		/* 基础模型 + fast */
		models = append(models, gin.H{"id": base + "-fast", "object": "model", "owned_by": "openai"})
		/* 生成全部思考等级变体 */
		for _, suffix := range thinkingSuffixes {
			models = append(models, gin.H{
				"id":       base + "-" + suffix,
				"object":   "model",
				"owned_by": "openai",
			})
			/* 思考等级 + fast 组合 */
			models = append(models, gin.H{
				"id":       base + "-" + suffix + "-fast",
				"object":   "model",
				"owned_by": "openai",
			})
		}
	}

	c.JSON(http.StatusOK, gin.H{
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
	return executor.RetryConfig{
		PickFn: func(model string, excluded map[string]bool) (*auth.Account, error) {
			return h.manager.PickExcluding(model, excluded)
		},
		On401Fn: func(acc *auth.Account) {
			h.manager.HandleAuth401(acc)
		},
		MaxRetry: h.maxRetry,
	}
}

/**
 * handleExecutorError 统一处理 executor 返回的错误
 * @param c - Gin 上下文
 * @param err - executor 返回的错误
 */
func handleExecutorError(c *gin.Context, err error) {
	if statusErr, ok := err.(*executor.StatusError); ok {
		c.JSON(statusErr.Code, gin.H{
			"error": gin.H{
				"message": string(statusErr.Body),
				"type":    "api_error",
				"code":    fmt.Sprintf("upstream_%d", statusErr.Code),
			},
		})
		return
	}
	sendError(c, http.StatusInternalServerError, err.Error(), "server_error")
}

/**
 * handleChatCompletions 处理 Chat Completions 请求
 * 解析请求 → executor 内部选择账号/重试 → 返回响应
 * 重试逻辑在 executor 内部完成，流式请求的 SSE 头只在成功后才写给客户端
 */
func (h *ProxyHandler) handleChatCompletions(c *gin.Context) {
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		sendError(c, http.StatusBadRequest, "读取请求体失败", "invalid_request_error")
		return
	}

	model := gjson.GetBytes(body, "model").String()
	if model == "" {
		sendError(c, http.StatusBadRequest, "缺少 model 字段", "invalid_request_error")
		return
	}
	stream := gjson.GetBytes(body, "stream").Bool()

	log.Infof("收到请求: model=%s, stream=%v", model, stream)

	rc := h.buildRetryConfig()

	if stream {
		if execErr := h.executor.ExecuteStream(c.Request.Context(), rc, body, model, c.Writer); execErr != nil {
			handleExecutorError(c, execErr)
		}
	} else {
		result, execErr := h.executor.ExecuteNonStream(c.Request.Context(), rc, body, model)
		if execErr != nil {
			handleExecutorError(c, execErr)
			return
		}
		c.Data(http.StatusOK, "application/json", result)
	}
}

/**
 * handleStats 账号统计接口
 * 返回所有账号的状态、请求数、错误数等统计信息
 */
func (h *ProxyHandler) handleStats(c *gin.Context) {
	accounts := h.manager.GetAccounts()
	stats := make([]auth.AccountStats, 0, len(accounts))
	active, cooldown, disabled := 0, 0, 0

	for _, acc := range accounts {
		s := acc.GetStats()
		stats = append(stats, s)
		switch s.Status {
		case "active":
			active++
		case "cooldown":
			cooldown++
		case "disabled":
			disabled++
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"summary": gin.H{
			"total":    len(accounts),
			"active":   active,
			"cooldown": cooldown,
			"disabled": disabled,
		},
		"accounts": stats,
	})
}

/**
 * handleRefresh 手动刷新所有账号的 Token（SSE 流式返回进度）
 * 每刷新完一个账号就推送一条 SSE 事件，防止大量账号时超时
 * POST /refresh
 */
func (h *ProxyHandler) handleRefresh(c *gin.Context) {
	ch := h.manager.ForceRefreshAllStream(c.Request.Context(), h.quotaChecker)
	writeSSEProgress(c, ch)
}

/**
 * handleCheckQuota 查询所有账号的剩余额度（SSE 流式返回进度）
 * 每查询完一个账号就推送一条 SSE 事件，防止大量账号时超时
 * POST /check-quota
 */
func (h *ProxyHandler) handleCheckQuota(c *gin.Context) {
	ch := h.quotaChecker.CheckAllStream(c.Request.Context(), h.manager)
	writeSSEProgress(c, ch)
}

/**
 * writeSSEProgress 将 ProgressEvent channel 以 SSE 格式写入 HTTP 响应
 * @param c - Gin 上下文
 * @param ch - 进度事件 channel
 */
func writeSSEProgress(c *gin.Context, ch <-chan auth.ProgressEvent) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Status(http.StatusOK)

	flusher, canFlush := c.Writer.(http.Flusher)

	for event := range ch {
		data, err := json.Marshal(event)
		if err != nil {
			continue
		}
		_, _ = fmt.Fprintf(c.Writer, "event: %s\ndata: %s\n\n", event.Type, data)
		if canFlush {
			flusher.Flush()
		}
	}
}

/**
 * handleResponses 处理 Responses API 请求
 * 直接透传 Codex 原生 SSE 事件或 response 对象，不做 Chat Completions 格式转换
 * 重试逻辑在 executor 内部完成
 */
func (h *ProxyHandler) handleResponses(c *gin.Context) {
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		sendError(c, http.StatusBadRequest, "读取请求体失败", "invalid_request_error")
		return
	}

	model := gjson.GetBytes(body, "model").String()
	if model == "" {
		sendError(c, http.StatusBadRequest, "缺少 model 字段", "invalid_request_error")
		return
	}
	stream := gjson.GetBytes(body, "stream").Bool()

	log.Infof("收到 Responses 请求: model=%s, stream=%v", model, stream)

	rc := h.buildRetryConfig()

	if stream {
		if execErr := h.executor.ExecuteResponsesStream(c.Request.Context(), rc, body, model, c.Writer); execErr != nil {
			handleExecutorError(c, execErr)
		}
	} else {
		result, execErr := h.executor.ExecuteResponsesNonStream(c.Request.Context(), rc, body, model)
		if execErr != nil {
			handleExecutorError(c, execErr)
			return
		}
		c.Data(http.StatusOK, "application/json", result)
	}
}

/**
 * handleResponsesCompact 处理 Responses Compact API 请求
 * 使用 /responses/compact 端点，直接透传 compact 格式（CBOR/SSE）响应
 * 重试逻辑在 executor 内部完成
 */
func (h *ProxyHandler) handleResponsesCompact(c *gin.Context) {
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		sendError(c, http.StatusBadRequest, "读取请求体失败", "invalid_request_error")
		return
	}

	model := gjson.GetBytes(body, "model").String()
	if model == "" {
		sendError(c, http.StatusBadRequest, "缺少 model 字段", "invalid_request_error")
		return
	}
	stream := gjson.GetBytes(body, "stream").Bool()

	log.Infof("收到 Responses Compact 请求: model=%s, stream=%v", model, stream)

	rc := h.buildRetryConfig()

	if stream {
		if execErr := h.executor.ExecuteResponsesCompactStream(c.Request.Context(), rc, body, model, c.Writer); execErr != nil {
			handleExecutorError(c, execErr)
		}
	} else {
		result, execErr := h.executor.ExecuteResponsesCompactNonStream(c.Request.Context(), rc, body, model)
		if execErr != nil {
			handleExecutorError(c, execErr)
			return
		}
		c.Data(http.StatusOK, "application/json", result)
	}
}

/**
 * sendError 发送 OpenAI 格式的错误响应
 * @param c - Gin 上下文
 * @param status - HTTP 状态码
 * @param message - 错误消息
 * @param errType - 错误类型
 */
func sendError(c *gin.Context, status int, message, errType string) {
	c.JSON(status, gin.H{
		"error": gin.H{
			"message": message,
			"type":    errType,
		},
	})
}
