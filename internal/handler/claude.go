/**
 * Claude Messages API 兼容处理器
 * 提供 /v1/messages 端点，接收 Claude 格式请求，转换为 OpenAI 格式后通过 Codex 执行器转发
 * 支持流式和非流式响应，响应结果转换回 Claude Messages API 格式
 * 重试逻辑在 executor 内部完成，客户端不感知账号切换
 */
package handler

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"

	"codex-proxy/internal/executor"
	"codex-proxy/internal/translator"

	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/valyala/fasthttp"
)

/*
 * pumpClaudeCodexSSE 将 Codex /responses SSE 译为 Claude Messages 流式事件写入 w。
 * 须关闭 s.body（与 PumpChatCompletion 一致）；若尚未向客户端写出任何事件且上游无有效内容，返回 ErrEmptyResponse 以触发换号/全量重连。
 */
func pumpClaudeCodexSSE(s *executor.CodexResponsesStream, w io.Writer, flush func(), model string, debugUpstream bool) error {
	body := s.UpstreamBody()
	defer func() { _ = body.Close() }()
	state := translator.NewClaudeStreamState(model)
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, scannerInitSize), scannerMaxSize)

	for scanner.Scan() {
		line := scanner.Bytes()
		if debugUpstream {
			ae := ""
			if acc := s.StreamAccount(); acc != nil {
				ae = acc.GetEmail()
			}
			executor.LogUpstreamStreamChunk("claude_sse_line", model, ae, line)
		}
		events := translator.ConvertCodexStreamToClaudeEvents(line, state, s.CacheSpoofEnabled())
		for _, event := range events {
			_, _ = io.WriteString(w, event)
			if flush != nil {
				flush()
			}
		}
		if state.Completed {
			break
		}
	}

	if scanErr := scanner.Err(); scanErr != nil {
		if errors.Is(scanErr, context.Canceled) {
			return fmt.Errorf("读取流式响应中断: %w", scanErr)
		}
		return fmt.Errorf("读取 Claude 上游 SSE 失败: %w", scanErr)
	}

	if !state.HasText && !state.HasToolUse && !state.HasThinking {
		if !state.Completed && state.MessageStartEmitted {
			closeEvents := translator.GenerateClaudeCloseEvents(state)
			for _, event := range closeEvents {
				_, _ = io.WriteString(w, event)
				if flush != nil {
					flush()
				}
			}
		}
		if state.MessageStartEmitted || state.Completed {
			if acc := s.StreamAccount(); acc != nil {
				acc.RecordUsageDetailed(
					s.BaseModel,
					state.InputTokens,
					state.OutputTokens,
					state.TotalTokens,
					state.CacheReadTokens,
					state.CacheWriteTokens,
					state.ReasoningTokens,
				)
				acc.RecordSuccess()
			}
			return nil
		}
		return executor.ErrEmptyResponse
	}
	if !state.Completed {
		closeEvents := translator.GenerateClaudeCloseEvents(state)
		for _, event := range closeEvents {
			_, _ = io.WriteString(w, event)
			if flush != nil {
				flush()
			}
		}
	}
	if acc := s.StreamAccount(); acc != nil {
		acc.RecordUsageDetailed(
			s.BaseModel,
			state.InputTokens,
			state.OutputTokens,
			state.TotalTokens,
			state.CacheReadTokens,
			state.CacheWriteTokens,
			state.ReasoningTokens,
		)
		acc.RecordSuccess()
	}
	return nil
}

/**
 * handleMessages 处理 Claude Messages API 请求（/v1/messages）
 * 将 Claude 格式请求转换为 OpenAI 格式 → executor 内部选择账号/重试 → 响应转回 Claude 格式
 */
func (h *ProxyHandler) handleMessages(ctx *fasthttp.RequestCtx) {
	body := ctx.PostBody()
	if len(body) == 0 {
		sendClaudeError(ctx, fasthttp.StatusBadRequest, "invalid_request_error", "读取请求体失败")
		return
	}

	openaiBody, model, stream := translator.ConvertClaudeRequestToOpenAI(body)
	if model == "" {
		sendClaudeError(ctx, fasthttp.StatusBadRequest, "invalid_request_error", "缺少 model 字段")
		return
	}
	if err := h.validateModelRequestOptions(model, body); err != nil {
		sendClaudeError(ctx, fasthttp.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	log.Debugf("收到 Claude Messages 请求: model=%s, stream=%v", model, stream)

	rc := h.buildRetryConfig()
	/* Claude 与其它 API 共用同一指纹收敛上下文；全局 session/thread 由配置种子派生。 */
	fps := h.newFingerPrintForRequest(ctx, openaiBody, "")
	bodyWithFP := openaiBody
	if fps != nil {
		if b, _ := fps.ApplyClientMetadata(openaiBody); b != nil {
			bodyWithFP = b
		}
	}

	if stream {
		if execErr := h.executeClaudeStream(ctx, rc, bodyWithFP, model, fps); execErr != nil {
			handleClaudeExecutorError(ctx, execErr)
		} else {
			RecordRequest()
		}
	} else {
		if execErr := h.executeClaudeNonStream(ctx, rc, bodyWithFP, model, fps); execErr != nil {
			handleClaudeExecutorError(ctx, execErr)
		} else {
			RecordRequest()
		}
	}
}

/**
 * handleMessageCountTokens 处理 Claude Messages Token Count 请求（/v1/messages/count_tokens）
 * 本地按 Claude 请求结构估算输入 token，不创建消息响应
 */
func (h *ProxyHandler) handleMessageCountTokens(ctx *fasthttp.RequestCtx) {
	body := ctx.PostBody()
	if len(body) == 0 {
		sendClaudeError(ctx, fasthttp.StatusBadRequest, "invalid_request_error", "读取请求体失败")
		return
	}

	if err := translator.ValidateClaudeCountTokensRequest(body); err != nil {
		sendClaudeError(ctx, fasthttp.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	model := gjson.GetBytes(body, "model").String()
	if err := h.validateModelRequestOptions(model, body); err != nil {
		sendClaudeError(ctx, fasthttp.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	count, err := translator.CountClaudeInputTokens(body)
	if err != nil {
		sendClaudeError(ctx, fasthttp.StatusInternalServerError, "api_error", fmt.Sprintf("计算输入 token 失败: %v", err))
		return
	}

	payload := map[string]any{
		"input_tokens": count,
	}
	if gjson.GetBytes(body, "context_management").Exists() {
		payload["context_management"] = map[string]any{
			"original_input_tokens": 0,
		}
	}

	writeJSON(ctx, fasthttp.StatusOK, payload)
	RecordRequest()
}

/**
 * executeClaudeStream 执行 Claude 流式请求
 * 通过 ExecuteRawCodexStream 获取原始 Codex SSE 流（内部已完成重试）
 * 逐行转换为 Claude SSE 事件写回客户端
 *
 * @param ctx - FastHTTP 上下文
 * @param rc - 内部重试配置
 * @param openaiBody - 已转换为 OpenAI 格式的请求体
 * @param model - 模型名称
 * @returns error - 执行失败时返回错误
 */
func (h *ProxyHandler) executeClaudeStream(ctx *fasthttp.RequestCtx, rc executor.RetryConfig, openaiBody []byte, model string, fps *executor.RequestsFingerprint) error {
	/* 只有到这里才开始写 SSE 头；Open+Pump 在 StreamWriter 内完成，响应体尚无字节时可内部换号与全量重连 */
	ctx.Response.Header.Set("Content-Type", "text/event-stream")
	ctx.Response.Header.Set("Cache-Control", "no-cache")
	ctx.Response.Header.Set("Connection", "keep-alive")
	ctx.SetStatusCode(fasthttp.StatusOK)

	ctx.SetBodyStreamWriter(func(w *bufio.Writer) {
		sw := newStreamBufWriter(w)
		flush := func() { _ = w.Flush() }
		bridges := executor.CodexStreamOpenBridgeMax(h.maxRetry)
		execErr := h.executor.RunCodexStreamWithOpenBridges(context.Background(), rc, openaiBody, model, sw, flush, bridges, func(s *executor.CodexResponsesStream, w2 io.Writer, fl func()) error {
			return pumpClaudeCodexSSE(s, w2, fl, model, h.debugUpstreamStream)
		}, fps)
		if execErr != nil {
			log.Errorf("Claude stream: %v", execErr)
			msg := execErr.Error()
			if errors.Is(execErr, executor.ErrEmptyResponse) {
				msg = "上游未返回有效内容（空响应）"
			}
			_, _ = fmt.Fprintf(w, "event: error\ndata: {\"type\":\"error\",\"message\":%q}\n\n", msg)
			_ = w.Flush()
		}
	})

	if !ctx.Response.IsBodyStream() {
		return executor.ErrEmptyResponse
	}
	return nil
}

/**
 * executeClaudeNonStream 执行 Claude 非流式请求
 * 通过 ExecuteRawCodexStream 获取原始 Codex SSE 数据（内部已完成重试）
 * 从中提取结果并转换为 Claude Messages 格式
 *
 * @param c - Gin 上下文
 * @param rc - 内部重试配置
 * @param openaiBody - 已转换为 OpenAI 格式的请求体
 * @param model - 模型名称
 * @returns error - 执行失败时返回错误
 */
func (h *ProxyHandler) executeClaudeNonStream(ctx *fasthttp.RequestCtx, rc executor.RetryConfig, openaiBody []byte, model string, fps *executor.RequestsFingerprint) error {
	rawResp, account, err := h.executor.ExecuteRawCodexStream(ctx, rc, openaiBody, model, fps)
	if err != nil {
		return err
	}
	defer func() {
		if rawResp.Body != nil {
			_ = rawResp.Body.Close()
		}
	}()

	data, err := io.ReadAll(rawResp.Body)
	if err != nil {
		return fmt.Errorf("读取响应失败: %w", err)
	}

	result := translator.ConvertCodexFullSSEToClaudeResponseWithMeta(ctx, data, model, h.cacheSpoofEnabled)
	if !result.FoundCompleted || result.JSON == "" {
		return fmt.Errorf("未收到 response.completed 事件")
	}
	if !result.HasText && !result.HasToolUse && !result.HasThinking {
		return executor.ErrEmptyResponse
	}

	account.RecordUsageDetailed(
		model,
		result.InputTokens,
		result.OutputTokens,
		result.TotalTokens,
		result.CacheReadTokens,
		result.CacheWriteTokens,
		result.ReasoningTokens,
	)
	account.RecordSuccess()
	ctx.Response.Header.Set("Content-Type", "application/json")
	ctx.SetStatusCode(fasthttp.StatusOK)
	ctx.SetBody([]byte(result.JSON))
	return nil
}

/**
 * handleClaudeExecutorError 处理 Claude 格式的 executor 错误
 * @param c - Gin 上下文
 * @param err - executor 返回的错误
 */
func handleClaudeExecutorError(ctx *fasthttp.RequestCtx, err error) {
	if errors.Is(err, executor.ErrEmptyResponse) {
		sendClaudeError(ctx, fasthttp.StatusBadGateway, "bad_gateway", "上游未返回有效内容（空响应）")
		return
	}
	if statusErr, ok := err.(*executor.StatusError); ok {
		msg := string(statusErr.Body)
		if gjson.ValidBytes(statusErr.Body) {
			if errMsg := gjson.GetBytes(statusErr.Body, "error.message"); errMsg.Exists() {
				msg = errMsg.String()
			} else if detail := gjson.GetBytes(statusErr.Body, "detail"); detail.Exists() {
				msg = detail.String()
			}
		}
		sendClaudeError(ctx, statusErr.Code, "api_error", msg)
		return
	}
	sendClaudeError(ctx, fasthttp.StatusInternalServerError, "api_error", err.Error())
}

/**
 * sendClaudeError 发送 Claude 格式的错误响应
 * @param ctx - FastHTTP 上下文
 * @param status - HTTP 状态码
 * @param errType - 错误类型
 * @param message - 错误消息
 */
func sendClaudeError(ctx *fasthttp.RequestCtx, status int, errType, message string) {
	writeJSON(ctx, status, map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    errType,
			"message": message,
		},
	})
}
