/**
 * Claude Messages API 兼容处理器
 * 提供 /v1/messages 端点，接收 Claude 格式请求，转换为 OpenAI 格式后通过 Codex 执行器转发
 * 支持流式和非流式响应，响应结果转换回 Claude Messages API 格式
 */
package handler

import (
	"bufio"
	"fmt"
	"io"
	"net/http"

	"codex-proxy/internal/auth"
	"codex-proxy/internal/executor"
	"codex-proxy/internal/translator"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

/**
 * handleMessages 处理 Claude Messages API 请求（/v1/messages）
 * 将 Claude 格式请求转换为 OpenAI 格式 → Codex 执行 → 响应转回 Claude 格式
 * 支持流式（SSE）和非流式两种模式，带重试的账号切换
 */
func (h *ProxyHandler) handleMessages(c *gin.Context) {
	/* 读取请求体 */
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		sendClaudeError(c, http.StatusBadRequest, "invalid_request_error", "读取请求体失败")
		return
	}

	/* 将 Claude 请求转换为 OpenAI 格式 */
	openaiBody, model, stream := translator.ConvertClaudeRequestToOpenAI(body)

	if model == "" {
		sendClaudeError(c, http.StatusBadRequest, "invalid_request_error", "缺少 model 字段")
		return
	}

	log.Infof("收到 Claude Messages 请求: model=%s, stream=%v", model, stream)

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
				sendClaudeError(c, http.StatusServiceUnavailable, "api_error", fmt.Sprintf("没有可用账号: %v", pickErr))
				return
			}
			break
		}

		usedAccounts[account.FilePath] = true
		log.Debugf("Claude 使用账号: %s (尝试 %d/%d)", account.GetEmail(), attempt+1, maxAttempts)

		var execErr error

		if stream {
			execErr = h.executeClaudeStream(c, account, openaiBody, model)
		} else {
			execErr = h.executeClaudeNonStream(c, account, openaiBody, model)
		}

		/* 成功 */
		if execErr == nil {
			account.RecordSuccess()
			return
		}

		lastErr = execErr

		/* 检查是否可重试 */
		if statusErr, ok := execErr.(*executor.StatusError); ok {
			if statusErr.Code == 401 {
				h.manager.HandleAuth401(account)
			}

			if isRetryableStatus(statusErr.Code) && attempt < maxAttempts-1 {
				log.Warnf("账号 [%s] Claude 请求失败 [%d]，切换账号重试", account.GetEmail(), statusErr.Code)
				continue
			}

			sendClaudeError(c, statusErr.Code, "api_error", string(statusErr.Body))
			return
		}

		/* 非 StatusError（网络错误/读取失败等）也切换账号重试 */
		if attempt < maxAttempts-1 {
			log.Warnf("账号 [%s] Claude 上游错误，切换账号重试: %v", account.GetEmail(), execErr)
			continue
		}
		break
	}

	/* 所有重试都失败 */
	if lastErr != nil {
		log.Errorf("Claude 所有重试均失败: %v", lastErr)
		if statusErr, ok := lastErr.(*executor.StatusError); ok {
			sendClaudeError(c, statusErr.Code, "api_error", string(statusErr.Body))
			return
		}
		sendClaudeError(c, http.StatusInternalServerError, "api_error", lastErr.Error())
		return
	}
	sendClaudeError(c, http.StatusServiceUnavailable, "api_error", "请求失败")
}

/**
 * executeClaudeStream 执行 Claude 流式请求
 * 通过 ExecuteRawCodexStream 获取原始 Codex SSE 流，逐行转换为 Claude SSE 事件写回客户端
 *
 * @param c - Gin 上下文
 * @param account - 使用的账号
 * @param openaiBody - 已转换为 OpenAI 格式的请求体
 * @param model - 模型名称
 * @returns error - 执行失败时返回错误
 */
func (h *ProxyHandler) executeClaudeStream(c *gin.Context, account *auth.Account, openaiBody []byte, model string) error {
	rawResp, err := h.executor.ExecuteRawCodexStream(c.Request.Context(), account, openaiBody, model)
	if err != nil {
		return err
	}
	defer func() {
		if rawResp.Body != nil {
			_ = rawResp.Body.Close()
		}
	}()

	/* 设置 Claude SSE 响应头 */
	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.WriteHeader(http.StatusOK)

	flusher, canFlush := c.Writer.(http.Flusher)
	state := translator.NewClaudeStreamState(model)

	/* 逐行读取 Codex SSE 并转换为 Claude SSE 事件 */
	scanner := bufio.NewScanner(rawResp.Body)
	scanner.Buffer(nil, 52_428_800)

	for scanner.Scan() {
		line := scanner.Bytes()
		events := translator.ConvertCodexStreamToClaudeEvents(c.Request.Context(), line, state)
		for _, event := range events {
			_, _ = fmt.Fprint(c.Writer, event)
			if canFlush {
				flusher.Flush()
			}
		}
	}

	if scanErr := scanner.Err(); scanErr != nil {
		log.Errorf("Claude 读取流式响应失败: %v", scanErr)
		return scanErr
	}

	return nil
}

/**
 * executeClaudeNonStream 执行 Claude 非流式请求
 * 通过 ExecuteRawCodexStream 获取原始 Codex SSE 数据，从中提取结果并转换为 Claude Messages 格式
 *
 * @param c - Gin 上下文
 * @param account - 使用的账号
 * @param openaiBody - 已转换为 OpenAI 格式的请求体
 * @param model - 模型名称
 * @returns error - 执行失败时返回错误
 */
func (h *ProxyHandler) executeClaudeNonStream(c *gin.Context, account *auth.Account, openaiBody []byte, model string) error {
	rawResp, err := h.executor.ExecuteRawCodexStream(c.Request.Context(), account, openaiBody, model)
	if err != nil {
		return err
	}
	defer func() {
		if rawResp.Body != nil {
			_ = rawResp.Body.Close()
		}
	}()

	/* 读取完整 SSE 数据 */
	data, err := io.ReadAll(rawResp.Body)
	if err != nil {
		return fmt.Errorf("读取响应失败: %w", err)
	}

	/* 从 SSE 数据中提取 response.completed 并转为 Claude 格式 */
	result := translator.ConvertCodexFullSSEToClaudeResponse(c.Request.Context(), data, model)
	if result == "" {
		return fmt.Errorf("未收到 response.completed 事件")
	}

	c.Data(http.StatusOK, "application/json", []byte(result))
	return nil
}

/**
 * sendClaudeError 发送 Claude 格式的错误响应
 * @param c - Gin 上下文
 * @param status - HTTP 状态码
 * @param errType - 错误类型
 * @param message - 错误消息
 */
func sendClaudeError(c *gin.Context, status int, errType, message string) {
	c.JSON(status, gin.H{
		"type": "error",
		"error": gin.H{
			"type":    errType,
			"message": message,
		},
	})
}
