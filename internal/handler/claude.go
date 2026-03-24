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

	log.Debugf("收到 Claude Messages 请求: model=%s, stream=%v", model, stream)

	rc := h.buildRetryConfig()

	if stream {
		if execErr := h.executeClaudeStream(ctx, rc, openaiBody, model); execErr != nil {
			handleClaudeExecutorError(ctx, execErr)
		} else {
			RecordRequest()
		}
	} else {
		if execErr := h.executeClaudeNonStream(ctx, rc, openaiBody, model); execErr != nil {
			handleClaudeExecutorError(ctx, execErr)
		} else {
			RecordRequest()
		}
	}
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
func (h *ProxyHandler) executeClaudeStream(ctx *fasthttp.RequestCtx, rc executor.RetryConfig, openaiBody []byte, model string) error {
	rawResp, account, err := h.executor.ExecuteRawCodexStream(ctx, rc, openaiBody, model)
	if err != nil {
		return err
	}

	/* 只有到这里才开始写 SSE 头（重试在 executor 内部已完成）；Body 须在 StreamWriter 内关闭，避免 defer 早于异步写协程执行 */
	ctx.Response.Header.Set("Content-Type", "text/event-stream")
	ctx.Response.Header.Set("Cache-Control", "no-cache")
	ctx.Response.Header.Set("Connection", "keep-alive")
	ctx.SetStatusCode(fasthttp.StatusOK)

	/* StreamWriter 内勿使用 RequestCtx（fasthttp 文档）；翻译用 Background，取消/断连由上游 Body 读错误体现 */
	ctx.SetBodyStreamWriter(func(w *bufio.Writer) {
		defer func() {
			if rawResp.Body != nil {
				_ = rawResp.Body.Close()
			}
		}()
		sw := newStreamBufWriter(w)
		flush := func() { _ = w.Flush() }
		state := translator.NewClaudeStreamState(model)

		scanner := bufio.NewScanner(rawResp.Body)
		scanner.Buffer(make([]byte, scannerInitSize), scannerMaxSize)

		for scanner.Scan() {
			line := scanner.Bytes()
			events := translator.ConvertCodexStreamToClaudeEvents(line, state)
			for _, event := range events {
				_, _ = io.WriteString(sw, event)
				flush()
			}
			if state.Completed {
				break
			}
		}

		if scanErr := scanner.Err(); scanErr != nil {
			if errors.Is(scanErr, context.Canceled) {
				return
			}
			log.Errorf("Claude 读取流式响应失败: %v", scanErr)
			return
		}
		/*
		 * 无正文/工具/思考但已完成：response.completed 已发 message_stop，须记账。
		 * 仅有 message_start 而上游提前 EOF：补发 message_delta + message_stop，避免客户端看到空 SSE。
		 */
		if !state.HasText && !state.HasToolUse && !state.HasThinking {
			if !state.Completed && state.MessageStartEmitted {
				closeEvents := translator.GenerateClaudeCloseEvents(state)
				for _, event := range closeEvents {
					_, _ = io.WriteString(sw, event)
				}
				flush()
			}
			if state.MessageStartEmitted || state.Completed {
				account.RecordSuccess()
			}
			return
		}
		if !state.Completed {
			closeEvents := translator.GenerateClaudeCloseEvents(state)
			for _, event := range closeEvents {
				_, _ = io.WriteString(sw, event)
			}
			flush()
		}
		account.RecordSuccess()
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
func (h *ProxyHandler) executeClaudeNonStream(ctx *fasthttp.RequestCtx, rc executor.RetryConfig, openaiBody []byte, model string) error {
	rawResp, account, err := h.executor.ExecuteRawCodexStream(ctx, rc, openaiBody, model)
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

	result := translator.ConvertCodexFullSSEToClaudeResponseWithMeta(ctx, data, model)
	if !result.FoundCompleted || result.JSON == "" {
		return fmt.Errorf("未收到 response.completed 事件")
	}
	if !result.HasText && !result.HasToolUse && !result.HasThinking {
		return executor.ErrEmptyResponse
	}

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
