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
}

/**
 * NewProxyHandler 创建新的代理处理器
 * @param manager - 账号管理器
 * @param exec - Codex 执行器
 * @param apiKeys - API Key 列表
 * @param maxRetry - 最大重试次数（0 表示不重试）
 * @returns *ProxyHandler - 代理处理器实例
 */
func NewProxyHandler(manager *auth.Manager, exec *executor.Executor, apiKeys []string, maxRetry int, proxyURL string) *ProxyHandler {
	if maxRetry < 0 {
		maxRetry = 0
	}
	return &ProxyHandler{
		manager:      manager,
		executor:     exec,
		apiKeys:      apiKeys,
		maxRetry:     maxRetry,
		quotaChecker: auth.NewQuotaChecker(proxyURL, 50),
	}
}

/**
 * RegisterRoutes 注册所有 HTTP 路由
 * @param r - Gin 引擎实例
 */
func (h *ProxyHandler) RegisterRoutes(r *gin.Engine) {
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
	"gpt-4.1", "gpt-4.1-mini", "gpt-4.1-nano",
	"o3", "o4-mini",
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
 * 为每个基础模型自动生成全部思考等级后缀变体
 */
func (h *ProxyHandler) handleModels(c *gin.Context) {
	models := make([]gin.H, 0, len(baseModelList)*(1+len(thinkingSuffixes)))

	for _, base := range baseModelList {
		/* 基础模型（无后缀） */
		models = append(models, gin.H{"id": base, "object": "model", "owned_by": "openai"})
		/* 生成全部思考等级变体 */
		for _, suffix := range thinkingSuffixes {
			models = append(models, gin.H{
				"id":       base + "-" + suffix,
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
 * isRetryableStatus 判断 HTTP 状态码是否可重试（切换账号重试）
 * 403（地域封锁 / Cloudflare 拦截）换账号也无法解决，不重试
 * 400（参数/模型错误）、401（认证失效）、429（限频）、5xx 均可切换账号重试
 * @param code - HTTP 状态码
 * @returns bool - 是否可重试
 */
func isRetryableStatus(code int) bool {
	if code >= 200 && code < 300 {
		return false
	}
	/* 403 地域封锁 / Cloudflare 拦截，换账号也没用 */
	if code == 403 {
		return false
	}
	return true
}

/**
 * handleChatCompletions 处理 Chat Completions 请求
 * 解析请求 → 选择账号 → 转换格式 → 执行 → 失败则切换账号重试 → 返回响应
 */
func (h *ProxyHandler) handleChatCompletions(c *gin.Context) {
	/* 读取请求体 */
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		sendError(c, http.StatusBadRequest, "读取请求体失败", "invalid_request_error")
		return
	}

	/* 解析模型名和流式标志 */
	model := gjson.GetBytes(body, "model").String()
	if model == "" {
		sendError(c, http.StatusBadRequest, "缺少 model 字段", "invalid_request_error")
		return
	}
	stream := gjson.GetBytes(body, "stream").Bool()

	log.Infof("收到请求: model=%s, stream=%v", model, stream)

	/* 带重试的请求执行 */
	maxAttempts := h.maxRetry + 1
	var lastErr error
	var usedAccounts = make(map[string]bool)

	for attempt := 0; attempt < maxAttempts; attempt++ {
		if c.Request.Context().Err() != nil {
			break
		}

		/* 选择账号（排除已用过的） */
		account, pickErr := h.manager.PickExcluding(model, usedAccounts)
		if pickErr != nil {
			if attempt == 0 {
				log.Errorf("选择账号失败: %v", pickErr)
				sendError(c, http.StatusServiceUnavailable, fmt.Sprintf("没有可用账号: %v", pickErr), "server_error")
				return
			}
			/* 重试时无更多可用账号，返回上次错误 */
			break
		}

		usedAccounts[account.FilePath] = true
		log.Debugf("使用账号: %s (尝试 %d/%d)", account.GetEmail(), attempt+1, maxAttempts)

		var execErr error
		var result []byte

		if stream {
			execErr = h.executor.ExecuteStream(c.Request.Context(), account, body, model, c.Writer)
		} else {
			result, execErr = h.executor.ExecuteNonStream(c.Request.Context(), account, body, model)
		}

		/* 成功 */
		if execErr == nil {
			account.RecordSuccess()
			if !stream {
				var jsonResult interface{}
				if unmarshalErr := json.Unmarshal(result, &jsonResult); unmarshalErr != nil {
					c.Data(http.StatusOK, "application/json", result)
				} else {
					c.JSON(http.StatusOK, jsonResult)
				}
			}
			return
		}

		lastErr = execErr

		/* 检查是否可重试 */
		if statusErr, ok := execErr.(*executor.StatusError); ok {
			/* 401: 先冷却+切换，后台异步刷新，刷新失败再删除 */
			if statusErr.Code == 401 {
				h.manager.HandleAuth401(account)
			} else if executor.ShouldRemoveAccount(statusErr.Code) {
				h.manager.RemoveAccount(account, fmt.Sprintf("request_%d", statusErr.Code))
			}

			if isRetryableStatus(statusErr.Code) && attempt < maxAttempts-1 {
				log.Warnf("账号 [%s] 请求失败 [%d]，切换账号重试", account.GetEmail(), statusErr.Code)
				continue
			}
			c.JSON(statusErr.Code, gin.H{
				"error": gin.H{
					"message": string(statusErr.Body),
					"type":    "api_error",
					"code":    fmt.Sprintf("upstream_%d", statusErr.Code),
				},
			})
			return
		}

		/* 非 StatusError（网络错误/读取失败等）也切换账号重试 */
		if attempt < maxAttempts-1 {
			log.Warnf("账号 [%s] 上游错误，切换账号重试: %v", account.GetEmail(), execErr)
			continue
		}
		break
	}

	/* 所有重试都失败 */
	if lastErr != nil {
		log.Errorf("所有重试均失败: %v", lastErr)
		if statusErr, ok := lastErr.(*executor.StatusError); ok {
			c.JSON(statusErr.Code, gin.H{
				"error": gin.H{
					"message": string(statusErr.Body),
					"type":    "api_error",
				},
			})
			return
		}
		sendError(c, http.StatusInternalServerError, lastErr.Error(), "server_error")
		return
	}
	sendError(c, http.StatusServiceUnavailable, "请求失败", "server_error")
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
 * 独立处理 Responses API 格式（input 数组 + reasoning.effort），
 * 直接透传 Codex 原生 SSE 事件或 response 对象，不做 Chat Completions 格式转换
 * 支持流式和非流式响应、带重试的账号切换
 */
func (h *ProxyHandler) handleResponses(c *gin.Context) {
	/* 读取请求体 */
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		sendError(c, http.StatusBadRequest, "读取请求体失败", "invalid_request_error")
		return
	}

	/* 解析模型名和流式标志 */
	model := gjson.GetBytes(body, "model").String()
	if model == "" {
		sendError(c, http.StatusBadRequest, "缺少 model 字段", "invalid_request_error")
		return
	}
	stream := gjson.GetBytes(body, "stream").Bool()

	log.Infof("收到 Responses 请求: model=%s, stream=%v", model, stream)

	/* 带重试的请求执行 */
	maxAttempts := h.maxRetry + 1
	var lastErr error
	var usedAccounts = make(map[string]bool)

	for attempt := 0; attempt < maxAttempts; attempt++ {
		if c.Request.Context().Err() != nil {
			break
		}

		/* 选择账号（排除已用过的） */
		account, pickErr := h.manager.PickExcluding(model, usedAccounts)
		if pickErr != nil {
			if attempt == 0 {
				log.Errorf("选择账号失败: %v", pickErr)
				sendError(c, http.StatusServiceUnavailable, fmt.Sprintf("没有可用账号: %v", pickErr), "server_error")
				return
			}
			break
		}

		usedAccounts[account.FilePath] = true
		log.Debugf("Responses 使用账号: %s (尝试 %d/%d)", account.GetEmail(), attempt+1, maxAttempts)

		var execErr error
		var result []byte

		if stream {
			execErr = h.executor.ExecuteResponsesStream(c.Request.Context(), account, body, model, c.Writer)
		} else {
			result, execErr = h.executor.ExecuteResponsesNonStream(c.Request.Context(), account, body, model)
		}

		/* 成功 */
		if execErr == nil {
			account.RecordSuccess()
			if !stream {
				c.Data(http.StatusOK, "application/json", result)
			}
			return
		}

		lastErr = execErr

		/* 检查是否可重试 */
		if statusErr, ok := execErr.(*executor.StatusError); ok {
			/* 401: 先冷却+切换，后台异步刷新，刷新失败再删除 */
			if statusErr.Code == 401 {
				h.manager.HandleAuth401(account)
			} else if executor.ShouldRemoveAccount(statusErr.Code) {
				h.manager.RemoveAccount(account, fmt.Sprintf("request_%d", statusErr.Code))
			}

			if isRetryableStatus(statusErr.Code) && attempt < maxAttempts-1 {
				log.Warnf("账号 [%s] Responses 请求失败 [%d]，切换账号重试", account.GetEmail(), statusErr.Code)
				continue
			}
			c.JSON(statusErr.Code, gin.H{
				"error": gin.H{
					"message": string(statusErr.Body),
					"type":    "api_error",
					"code":    fmt.Sprintf("upstream_%d", statusErr.Code),
				},
			})
			return
		}

		/* 非 StatusError（网络错误/读取失败等）也切换账号重试 */
		if attempt < maxAttempts-1 {
			log.Warnf("账号 [%s] Responses 上游错误，切换账号重试: %v", account.GetEmail(), execErr)
			continue
		}
		break
	}

	/* 所有重试都失败 */
	if lastErr != nil {
		log.Errorf("Responses 所有重试均失败: %v", lastErr)
		if statusErr, ok := lastErr.(*executor.StatusError); ok {
			c.JSON(statusErr.Code, gin.H{
				"error": gin.H{
					"message": string(statusErr.Body),
					"type":    "api_error",
				},
			})
			return
		}
		sendError(c, http.StatusInternalServerError, lastErr.Error(), "server_error")
		return
	}
	sendError(c, http.StatusServiceUnavailable, "请求失败", "server_error")
}

/**
 * handleResponsesCompact 处理 Responses Compact API 请求
 * 使用 /responses/compact 端点，直接透传 compact 格式（CBOR/SSE）响应
 * 支持流式和非流式响应、带重试的账号切换
 */
func (h *ProxyHandler) handleResponsesCompact(c *gin.Context) {
	/* 读取请求体 */
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		sendError(c, http.StatusBadRequest, "读取请求体失败", "invalid_request_error")
		return
	}

	/* 解析模型名和流式标志 */
	model := gjson.GetBytes(body, "model").String()
	if model == "" {
		sendError(c, http.StatusBadRequest, "缺少 model 字段", "invalid_request_error")
		return
	}
	stream := gjson.GetBytes(body, "stream").Bool()

	log.Infof("收到 Responses Compact 请求: model=%s, stream=%v", model, stream)

	/* 带重试的请求执行 */
	maxAttempts := h.maxRetry + 1
	var lastErr error
	var usedAccounts = make(map[string]bool)

	for attempt := 0; attempt < maxAttempts; attempt++ {
		if c.Request.Context().Err() != nil {
			break
		}

		/* 选择账号（排除已用过的） */
		account, pickErr := h.manager.PickExcluding(model, usedAccounts)
		if pickErr != nil {
			if attempt == 0 {
				log.Errorf("选择账号失败: %v", pickErr)
				sendError(c, http.StatusServiceUnavailable, fmt.Sprintf("没有可用账号: %v", pickErr), "server_error")
				return
			}
			break
		}

		usedAccounts[account.FilePath] = true
		log.Debugf("Compact 使用账号: %s (尝试 %d/%d)", account.GetEmail(), attempt+1, maxAttempts)

		var execErr error
		var result []byte

		if stream {
			execErr = h.executor.ExecuteResponsesCompactStream(c.Request.Context(), account, body, model, c.Writer)
		} else {
			result, execErr = h.executor.ExecuteResponsesCompactNonStream(c.Request.Context(), account, body, model)
		}

		/* 成功 */
		if execErr == nil {
			account.RecordSuccess()
			if !stream {
				/* compact 格式透传，使用原始 Content-Type */
				c.Data(http.StatusOK, "application/json", result)
			}
			return
		}

		lastErr = execErr

		/* 检查是否可重试 */
		if statusErr, ok := execErr.(*executor.StatusError); ok {
			if statusErr.Code == 401 {
				h.manager.HandleAuth401(account)
			} else if executor.ShouldRemoveAccount(statusErr.Code) {
				h.manager.RemoveAccount(account, fmt.Sprintf("request_%d", statusErr.Code))
			}

			if isRetryableStatus(statusErr.Code) && attempt < maxAttempts-1 {
				log.Warnf("账号 [%s] Compact 请求失败 [%d]，切换账号重试", account.GetEmail(), statusErr.Code)
				continue
			}
			c.JSON(statusErr.Code, gin.H{
				"error": gin.H{
					"message": string(statusErr.Body),
					"type":    "api_error",
					"code":    fmt.Sprintf("upstream_%d", statusErr.Code),
				},
			})
			return
		}

		/* 非 StatusError（网络错误/读取失败等）也切换账号重试 */
		if attempt < maxAttempts-1 {
			log.Warnf("账号 [%s] Compact 上游错误，切换账号重试: %v", account.GetEmail(), execErr)
			continue
		}
		break
	}

	/* 所有重试都失败 */
	if lastErr != nil {
		log.Errorf("Compact 所有重试均失败: %v", lastErr)
		if statusErr, ok := lastErr.(*executor.StatusError); ok {
			c.JSON(statusErr.Code, gin.H{
				"error": gin.H{
					"message": string(statusErr.Body),
					"type":    "api_error",
				},
			})
			return
		}
		sendError(c, http.StatusInternalServerError, lastErr.Error(), "server_error")
		return
	}
	sendError(c, http.StatusServiceUnavailable, "请求失败", "server_error")
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
