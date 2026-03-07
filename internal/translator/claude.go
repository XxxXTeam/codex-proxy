/**
 * Claude API 协议兼容模块
 * 将 Claude Messages API 格式的请求转换为 OpenAI Chat Completions 格式（复用现有转换链）
 * 将 OpenAI Chat Completions 格式的响应转换为 Claude Messages API 格式
 * 支持流式和非流式两种模式
 *
 * Claude Messages API 格式：
 *   请求：{"model":"...", "max_tokens":1024, "messages":[...], "system":"...", "stream":true}
 *   非流式响应：{"id":"msg_xxx", "type":"message", "role":"assistant", "content":[{"type":"text","text":"..."}], ...}
 *   流式响应：SSE 事件序列 (message_start → content_block_start → content_block_delta → ... → message_stop)
 */
package translator

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

/**
 * ConvertClaudeRequestToOpenAI 将 Claude Messages API 请求转换为 OpenAI Chat Completions 格式
 * 转换后可复用现有的 OpenAI → Codex 转换链
 *
 * 转换规则：
 *   - claude.system → openai.messages[0].role=system
 *   - claude.messages → openai.messages（role 映射一致）
 *   - claude.max_tokens → openai.max_tokens
 *   - claude.temperature → openai.temperature
 *   - claude.top_p → openai.top_p
 *   - claude.stream → openai.stream
 *   - claude.tools → openai.tools（Claude 格式 → OpenAI 格式）
 *   - claude.tool_choice → openai.tool_choice
 *
 * @param claudeBody - Claude Messages API 格式的请求 JSON
 * @returns []byte - OpenAI Chat Completions 格式的请求 JSON
 * @returns string - 模型名称
 * @returns bool - 是否流式
 */
func ConvertClaudeRequestToOpenAI(claudeBody []byte) ([]byte, string, bool) {
	out := `{}`

	/* 模型名 */
	model := gjson.GetBytes(claudeBody, "model").String()
	out, _ = sjson.Set(out, "model", model)

	/* 流式标志 */
	stream := gjson.GetBytes(claudeBody, "stream").Bool()
	out, _ = sjson.Set(out, "stream", stream)

	/* max_tokens */
	if v := gjson.GetBytes(claudeBody, "max_tokens"); v.Exists() {
		out, _ = sjson.Set(out, "max_tokens", v.Int())
	}

	/* temperature */
	if v := gjson.GetBytes(claudeBody, "temperature"); v.Exists() {
		out, _ = sjson.Set(out, "temperature", v.Float())
	}

	/* top_p */
	if v := gjson.GetBytes(claudeBody, "top_p"); v.Exists() {
		out, _ = sjson.Set(out, "top_p", v.Float())
	}

	/* 构建 messages 数组 */
	out, _ = sjson.SetRaw(out, "messages", `[]`)

	/* system → messages[0] role=system */
	if sys := gjson.GetBytes(claudeBody, "system"); sys.Exists() {
		sysMsg := `{}`
		sysMsg, _ = sjson.Set(sysMsg, "role", "system")
		if sys.Type == gjson.String {
			sysMsg, _ = sjson.Set(sysMsg, "content", sys.String())
		} else if sys.IsArray() {
			/* Claude 支持 system 为数组格式 [{type:"text", text:"..."}] */
			var text string
			for _, item := range sys.Array() {
				if item.Get("type").String() == "text" {
					text += item.Get("text").String()
				}
			}
			sysMsg, _ = sjson.Set(sysMsg, "content", text)
		}
		out, _ = sjson.SetRaw(out, "messages.-1", sysMsg)
	}

	/* 转换 messages */
	messages := gjson.GetBytes(claudeBody, "messages")
	if messages.IsArray() {
		for _, m := range messages.Array() {
			role := m.Get("role").String()
			content := m.Get("content")

			msg := `{}`
			msg, _ = sjson.Set(msg, "role", role)

			if content.Type == gjson.String {
				/* 简单字符串内容 */
				msg, _ = sjson.Set(msg, "content", content.String())
			} else if content.IsArray() {
				/* 数组内容：文本 / 图片 / tool_use / tool_result */
				msg, _ = sjson.SetRaw(msg, "content", `[]`)
				var toolCalls []string

				for _, block := range content.Array() {
					blockType := block.Get("type").String()

					switch blockType {
					case "text":
						part := `{}`
						part, _ = sjson.Set(part, "type", "text")
						part, _ = sjson.Set(part, "text", block.Get("text").String())
						msg, _ = sjson.SetRaw(msg, "content.-1", part)

					case "image":
						/* Claude image → OpenAI image_url */
						src := block.Get("source")
						if src.Exists() && src.Get("type").String() == "base64" {
							dataURL := fmt.Sprintf("data:%s;base64,%s",
								src.Get("media_type").String(), src.Get("data").String())
							part := `{}`
							part, _ = sjson.Set(part, "type", "image_url")
							part, _ = sjson.Set(part, "image_url.url", dataURL)
							msg, _ = sjson.SetRaw(msg, "content.-1", part)
						}

					case "tool_use":
						/* Claude tool_use → OpenAI tool_calls（放到 message 的 tool_calls 字段） */
						tc := `{}`
						tc, _ = sjson.Set(tc, "id", block.Get("id").String())
						tc, _ = sjson.Set(tc, "type", "function")
						tc, _ = sjson.Set(tc, "function.name", block.Get("name").String())
						if input := block.Get("input"); input.Exists() {
							tc, _ = sjson.Set(tc, "function.arguments", input.Raw)
						} else {
							tc, _ = sjson.Set(tc, "function.arguments", "{}")
						}
						toolCalls = append(toolCalls, tc)

					case "tool_result":
						/* Claude tool_result → OpenAI tool message（单独消息） */
						toolMsg := `{}`
						toolMsg, _ = sjson.Set(toolMsg, "role", "tool")
						toolMsg, _ = sjson.Set(toolMsg, "tool_call_id", block.Get("tool_use_id").String())
						resultContent := block.Get("content")
						if resultContent.Type == gjson.String {
							toolMsg, _ = sjson.Set(toolMsg, "content", resultContent.String())
						} else if resultContent.IsArray() {
							var text string
							for _, rc := range resultContent.Array() {
								if rc.Get("type").String() == "text" {
									text += rc.Get("text").String()
								}
							}
							toolMsg, _ = sjson.Set(toolMsg, "content", text)
						}
						out, _ = sjson.SetRaw(out, "messages.-1", toolMsg)
					}
				}

				/* 如果有 tool_calls，设到消息上 */
				if len(toolCalls) > 0 {
					msg, _ = sjson.SetRaw(msg, "tool_calls", `[]`)
					for _, tc := range toolCalls {
						msg, _ = sjson.SetRaw(msg, "tool_calls.-1", tc)
					}
					/* assistant + tool_calls 时 content 可以是纯文本或 null */
					contentArr := gjson.Get(msg, "content")
					if contentArr.IsArray() && len(contentArr.Array()) == 0 {
						msg, _ = sjson.Set(msg, "content", nil)
					}
				}
			}

			/* tool_result 已经作为单独消息添加了，跳过原消息中仅包含 tool_result 的情况 */
			if role == "user" {
				/* 检查是否有非 tool_result 的内容 */
				hasNonToolResult := false
				if content.Type == gjson.String {
					hasNonToolResult = true
				} else if content.IsArray() {
					for _, block := range content.Array() {
						if block.Get("type").String() != "tool_result" {
							hasNonToolResult = true
							break
						}
					}
				}
				if !hasNonToolResult {
					continue
				}
			}

			out, _ = sjson.SetRaw(out, "messages.-1", msg)
		}
	}

	/* 转换 tools：Claude 格式 → OpenAI 格式 */
	if tools := gjson.GetBytes(claudeBody, "tools"); tools.IsArray() && len(tools.Array()) > 0 {
		out, _ = sjson.SetRaw(out, "tools", `[]`)
		for _, t := range tools.Array() {
			tool := `{}`
			tool, _ = sjson.Set(tool, "type", "function")
			tool, _ = sjson.Set(tool, "function.name", t.Get("name").String())
			if desc := t.Get("description"); desc.Exists() {
				tool, _ = sjson.Set(tool, "function.description", desc.String())
			}
			if schema := t.Get("input_schema"); schema.Exists() {
				tool, _ = sjson.SetRaw(tool, "function.parameters", schema.Raw)
			}
			out, _ = sjson.SetRaw(out, "tools.-1", tool)
		}
	}

	/* tool_choice */
	if tc := gjson.GetBytes(claudeBody, "tool_choice"); tc.Exists() {
		tcType := tc.Get("type").String()
		switch tcType {
		case "auto":
			out, _ = sjson.Set(out, "tool_choice", "auto")
		case "any":
			out, _ = sjson.Set(out, "tool_choice", "required")
		case "tool":
			choice := `{}`
			choice, _ = sjson.Set(choice, "type", "function")
			choice, _ = sjson.Set(choice, "function.name", tc.Get("name").String())
			out, _ = sjson.SetRaw(out, "tool_choice", choice)
		}
	}

	return []byte(out), model, stream
}

/**
 * ClaudeStreamState Claude 流式响应转换的状态对象
 * @field MessageID - 消息 ID
 * @field Model - 模型名称
 * @field InputTokens - 输入 token 数
 * @field ContentBlockIndex - 当前内容块索引
 * @field HasStartedContent - 是否已开始输出内容
 * @field HasToolUse - 是否包含工具调用
 */
type ClaudeStreamState struct {
	MessageID         string
	Model             string
	InputTokens       int64
	ContentBlockIndex int
	HasStartedContent bool
	HasToolUse        bool
}

/**
 * NewClaudeStreamState 创建新的 Claude 流式状态对象
 * @param model - 模型名称
 * @returns *ClaudeStreamState - 流式状态实例
 */
func NewClaudeStreamState(model string) *ClaudeStreamState {
	return &ClaudeStreamState{
		MessageID:         "msg_" + uuid.NewString()[:24],
		Model:             model,
		ContentBlockIndex: -1,
	}
}

/**
 * ConvertCodexStreamToClaudeEvents 将 Codex SSE 事件转换为 Claude Messages 流式格式
 * 每个 Codex 事件可能产生 0~N 个 Claude SSE 事件
 *
 * @param ctx - 上下文
 * @param rawLine - 原始 SSE 行数据
 * @param state - Claude 流式状态对象
 * @returns []string - "event: xxx\ndata: {...}\n\n" 格式的 SSE 事件列表
 */
func ConvertCodexStreamToClaudeEvents(_ context.Context, rawLine []byte, state *ClaudeStreamState) []string {
	if !bytes.HasPrefix(rawLine, dataPrefix) {
		return nil
	}
	rawJSON := bytes.TrimSpace(rawLine[5:])
	if len(rawJSON) == 0 {
		return nil
	}

	root := gjson.ParseBytes(rawJSON)
	dataType := root.Get("type").String()
	var events []string

	switch dataType {
	case "response.created":
		if m := root.Get("response.model").String(); m != "" {
			state.Model = m
		}
		if usage := root.Get("response.usage"); usage.Exists() {
			state.InputTokens = usage.Get("input_tokens").Int()
		}

		/* 发送 message_start 事件 */
		msgStart := `{}`
		msgStart, _ = sjson.Set(msgStart, "type", "message_start")
		msg := `{}`
		msg, _ = sjson.Set(msg, "id", state.MessageID)
		msg, _ = sjson.Set(msg, "type", "message")
		msg, _ = sjson.Set(msg, "role", "assistant")
		msg, _ = sjson.SetRaw(msg, "content", `[]`)
		msg, _ = sjson.Set(msg, "model", state.Model)
		msg, _ = sjson.Set(msg, "stop_reason", nil)
		msg, _ = sjson.Set(msg, "usage.input_tokens", state.InputTokens)
		msg, _ = sjson.Set(msg, "usage.output_tokens", 0)
		msgStart, _ = sjson.SetRaw(msgStart, "message", msg)
		events = append(events, formatClaudeSSE("message_start", msgStart))

	case "response.output_text.delta":
		delta := root.Get("delta").String()
		if delta == "" {
			return nil
		}

		/* 如果还没开始内容块，先发 content_block_start */
		if !state.HasStartedContent {
			state.ContentBlockIndex++
			state.HasStartedContent = true
			blockStart := `{}`
			blockStart, _ = sjson.Set(blockStart, "type", "content_block_start")
			blockStart, _ = sjson.Set(blockStart, "index", state.ContentBlockIndex)
			block := `{}`
			block, _ = sjson.Set(block, "type", "text")
			block, _ = sjson.Set(block, "text", "")
			blockStart, _ = sjson.SetRaw(blockStart, "content_block", block)
			events = append(events, formatClaudeSSE("content_block_start", blockStart))
		}

		/* content_block_delta */
		blockDelta := `{}`
		blockDelta, _ = sjson.Set(blockDelta, "type", "content_block_delta")
		blockDelta, _ = sjson.Set(blockDelta, "index", state.ContentBlockIndex)
		d := `{}`
		d, _ = sjson.Set(d, "type", "text_delta")
		d, _ = sjson.Set(d, "text", delta)
		blockDelta, _ = sjson.SetRaw(blockDelta, "delta", d)
		events = append(events, formatClaudeSSE("content_block_delta", blockDelta))

	case "response.output_item.added":
		item := root.Get("item")
		if !item.Exists() || item.Get("type").String() != "function_call" {
			return nil
		}
		state.HasToolUse = true

		/* 先关闭之前的文本内容块（如果有） */
		if state.HasStartedContent {
			blockStop := `{}`
			blockStop, _ = sjson.Set(blockStop, "type", "content_block_stop")
			blockStop, _ = sjson.Set(blockStop, "index", state.ContentBlockIndex)
			events = append(events, formatClaudeSSE("content_block_stop", blockStop))
			state.HasStartedContent = false
		}

		/* 开始新的 tool_use 内容块 */
		state.ContentBlockIndex++
		blockStart := `{}`
		blockStart, _ = sjson.Set(blockStart, "type", "content_block_start")
		blockStart, _ = sjson.Set(blockStart, "index", state.ContentBlockIndex)
		block := `{}`
		block, _ = sjson.Set(block, "type", "tool_use")
		block, _ = sjson.Set(block, "id", item.Get("call_id").String())
		block, _ = sjson.Set(block, "name", item.Get("name").String())
		block, _ = sjson.SetRaw(block, "input", `{}`)
		blockStart, _ = sjson.SetRaw(blockStart, "content_block", block)
		events = append(events, formatClaudeSSE("content_block_start", blockStart))

	case "response.function_call_arguments.delta":
		delta := root.Get("delta").String()
		if delta == "" {
			return nil
		}
		blockDelta := `{}`
		blockDelta, _ = sjson.Set(blockDelta, "type", "content_block_delta")
		blockDelta, _ = sjson.Set(blockDelta, "index", state.ContentBlockIndex)
		d := `{}`
		d, _ = sjson.Set(d, "type", "input_json_delta")
		d, _ = sjson.Set(d, "partial_json", delta)
		blockDelta, _ = sjson.SetRaw(blockDelta, "delta", d)
		events = append(events, formatClaudeSSE("content_block_delta", blockDelta))

	case "response.function_call_arguments.done":
		/* 关闭 tool_use 内容块 */
		blockStop := `{}`
		blockStop, _ = sjson.Set(blockStop, "type", "content_block_stop")
		blockStop, _ = sjson.Set(blockStop, "index", state.ContentBlockIndex)
		events = append(events, formatClaudeSSE("content_block_stop", blockStop))

	case "response.completed":
		/* 关闭最后一个内容块（如果还未关闭） */
		if state.HasStartedContent {
			blockStop := `{}`
			blockStop, _ = sjson.Set(blockStop, "type", "content_block_stop")
			blockStop, _ = sjson.Set(blockStop, "index", state.ContentBlockIndex)
			events = append(events, formatClaudeSSE("content_block_stop", blockStop))
		}

		/* message_delta：设置 stop_reason 和 usage */
		stopReason := "end_turn"
		if state.HasToolUse {
			stopReason = "tool_use"
		}
		outputTokens := root.Get("response.usage.output_tokens").Int()

		msgDelta := `{}`
		msgDelta, _ = sjson.Set(msgDelta, "type", "message_delta")
		delta := `{}`
		delta, _ = sjson.Set(delta, "stop_reason", stopReason)
		msgDelta, _ = sjson.SetRaw(msgDelta, "delta", delta)
		msgDelta, _ = sjson.Set(msgDelta, "usage.output_tokens", outputTokens)
		events = append(events, formatClaudeSSE("message_delta", msgDelta))

		/* message_stop */
		msgStop := `{}`
		msgStop, _ = sjson.Set(msgStop, "type", "message_stop")
		events = append(events, formatClaudeSSE("message_stop", msgStop))
	}

	return events
}

/**
 * ConvertCodexNonStreamToClaudeResponse 将 Codex 非流式响应转换为 Claude Messages API 格式
 *
 * @param ctx - 上下文
 * @param rawJSON - Codex response.completed 事件的 data JSON
 * @param model - 请求的模型名称
 * @returns string - Claude Messages API 格式的 JSON 字符串
 */
func ConvertCodexNonStreamToClaudeResponse(_ context.Context, rawJSON []byte, model string) string {
	root := gjson.ParseBytes(rawJSON)
	if root.Get("type").String() != "response.completed" {
		return ""
	}

	resp := root.Get("response")
	msgID := "msg_" + uuid.NewString()[:24]

	out := `{}`
	out, _ = sjson.Set(out, "id", msgID)
	out, _ = sjson.Set(out, "type", "message")
	out, _ = sjson.Set(out, "role", "assistant")
	out, _ = sjson.SetRaw(out, "content", `[]`)

	if m := resp.Get("model").String(); m != "" {
		out, _ = sjson.Set(out, "model", m)
	} else {
		out, _ = sjson.Set(out, "model", model)
	}

	/* 处理 output 数组 */
	stopReason := "end_turn"
	output := resp.Get("output")
	if output.IsArray() {
		for _, item := range output.Array() {
			switch item.Get("type").String() {
			case "message":
				if content := item.Get("content"); content.IsArray() {
					for _, ci := range content.Array() {
						if ci.Get("type").String() == "output_text" {
							block := `{}`
							block, _ = sjson.Set(block, "type", "text")
							block, _ = sjson.Set(block, "text", ci.Get("text").String())
							out, _ = sjson.SetRaw(out, "content.-1", block)
						}
					}
				}
			case "function_call":
				stopReason = "tool_use"
				block := `{}`
				block, _ = sjson.Set(block, "type", "tool_use")
				block, _ = sjson.Set(block, "id", item.Get("call_id").String())
				block, _ = sjson.Set(block, "name", item.Get("name").String())
				if args := item.Get("arguments"); args.Exists() {
					out, _ = sjson.SetRaw(out, "content.-1.input", args.Raw)
				} else {
					block, _ = sjson.SetRaw(block, "input", `{}`)
				}
				out, _ = sjson.SetRaw(out, "content.-1", block)
			}
		}
	}

	out, _ = sjson.Set(out, "stop_reason", stopReason)
	out, _ = sjson.Set(out, "stop_sequence", nil)

	/* usage */
	if usage := resp.Get("usage"); usage.Exists() {
		out, _ = sjson.Set(out, "usage.input_tokens", usage.Get("input_tokens").Int())
		out, _ = sjson.Set(out, "usage.output_tokens", usage.Get("output_tokens").Int())
	} else {
		out, _ = sjson.Set(out, "usage.input_tokens", 0)
		out, _ = sjson.Set(out, "usage.output_tokens", 0)
	}

	return out
}

/**
 * ConvertCodexFullSSEToClaudeResponse 从完整 SSE 数据中提取 response.completed 并转为 Claude 格式
 * 用于非流式场景（Codex 始终返回 SSE，需要从中提取最终结果）
 *
 * @param ctx - 上下文
 * @param data - 完整 SSE 响应数据
 * @param model - 请求的模型名称
 * @returns string - Claude Messages API 格式的 JSON 字符串
 */
func ConvertCodexFullSSEToClaudeResponse(ctx context.Context, data []byte, model string) string {
	lines := bytes.Split(data, []byte("\n"))
	for _, line := range lines {
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		jsonData := bytes.TrimSpace(line[5:])
		if gjson.GetBytes(jsonData, "type").String() != "response.completed" {
			continue
		}
		return ConvertCodexNonStreamToClaudeResponse(ctx, jsonData, model)
	}
	return ""
}

/**
 * formatClaudeSSE 格式化单个 Claude SSE 事件
 * @param eventType - 事件类型
 * @param data - 事件数据 JSON
 * @returns string - 格式化的 SSE 事件字符串
 */
func formatClaudeSSE(eventType, data string) string {
	return fmt.Sprintf("event: %s\ndata: %s\n\n", eventType, data)
}

/**
 * SendClaudeError 生成 Claude 格式的错误响应 JSON
 * @param errType - 错误类型
 * @param message - 错误消息
 * @returns string - Claude 格式的错误 JSON
 */
func SendClaudeError(errType, message string) string {
	out := `{}`
	out, _ = sjson.Set(out, "type", "error")
	out, _ = sjson.Set(out, "error.type", errType)
	out, _ = sjson.Set(out, "error.message", message)
	return out
}

/* 确保 time 和 uuid 包在编译时被引用 */
var _ = time.Now
var _ = uuid.NewString
