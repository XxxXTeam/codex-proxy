/**
 * 响应转换模块
 * 将 Codex (OpenAI Responses API) 的流式和非流式响应转换为 OpenAI Chat Completions 格式
 * 处理文本内容、工具调用、推理内容、用量元数据等的格式映射
 */
package translator

import (
	"bytes"
	"context"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

var dataPrefix = []byte("data:")

/**
 * StreamState 流式响应转换的状态对象
 * 在多次调用之间维护上下文（如 response ID、函数调用索引等）
 * @field ResponseID - Codex 响应 ID
 * @field CreatedAt - 创建时间戳
 * @field Model - 模型名称
 * @field FunctionCallIndex - 当前函数调用的索引
 * @field HasReceivedArgsDelta - 是否已接收到函数参数增量
 * @field HasToolCallAnnounced - 是否已发送过工具调用通知
 * @field baseTpl - 预构建的基础 JSON 模板（id/created/model 已设置，避免每事件重复构建）
 * @field UsageInput - response.completed 时提取的 input_tokens
 * @field UsageOutput - response.completed 时提取的 output_tokens
 * @field UsageTotal - response.completed 时提取的 total_tokens
 */
type StreamState struct {
	ResponseID           string
	CreatedAt            int64
	Model                string
	FunctionCallIndex    int
	HasReceivedArgsDelta bool
	HasToolCallAnnounced bool
	baseTpl              string
	UsageInput           int64
	UsageOutput          int64
	UsageTotal           int64
}

/**
 * NewStreamState 创建新的流式状态对象
 * @param model - 模型名称
 * @returns *StreamState - 流式状态实例
 */
func NewStreamState(model string) *StreamState {
	return &StreamState{
		Model:             model,
		FunctionCallIndex: -1,
	}
}

/**
 * ConvertStreamChunk 将单个 Codex SSE 事件转换为 OpenAI Chat Completions 流式格式
 *
 * 支持的事件类型映射：
 *   - response.created → 缓存 ID/时间戳（不输出）
 *   - response.output_text.delta → choices[0].delta.content
 *   - response.reasoning_summary_text.delta → choices[0].delta.reasoning_content
 *   - response.output_item.added (function_call) → choices[0].delta.tool_calls
 *   - response.function_call_arguments.delta → tool_calls arguments
 *   - response.completed → finish_reason
 *
 * @param ctx - 上下文
 * @param rawLine - 原始 SSE 行数据
 * @param state - 流式状态对象
 * @param reverseToolMap - 缩短名→原始名的工具名映射
 * @returns []string - 转换后的 OpenAI 格式 JSON 字符串列表
 */
func ConvertStreamChunk(_ context.Context, rawLine []byte, state *StreamState, reverseToolMap map[string]string) []string {
	if !bytes.HasPrefix(rawLine, dataPrefix) {
		return nil
	}
	rawJSON := bytes.TrimSpace(rawLine[5:])
	if len(rawJSON) == 0 {
		return nil
	}

	root := gjson.ParseBytes(rawJSON)
	dataType := root.Get("type").String()
	if dataType == "response.created" {
		state.ResponseID = root.Get("response.id").String()
		state.CreatedAt = root.Get("response.created_at").Int()
		if m := root.Get("response.model").String(); m != "" {
			state.Model = m
		}
		tpl := `{"id":"","object":"chat.completion.chunk","created":0,"model":"","choices":[{"index":0,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":null},"finish_reason":null}]}`
		tpl, _ = sjson.Set(tpl, "id", state.ResponseID)
		tpl, _ = sjson.Set(tpl, "created", state.CreatedAt)
		tpl, _ = sjson.Set(tpl, "model", state.Model)
		state.baseTpl = tpl
		return nil
	}

	/*
	 * 复用预构建的基础模板（id/created/model 已设置好）
	 * Go 字符串不可变，sjson.Set 返回新字符串，不会污染 baseTpl
	 * 每个 delta 事件省去 3 次 sjson.Set
	 */
	tpl := state.baseTpl
	if tpl == "" {
		tpl = `{"id":"","object":"chat.completion.chunk","created":0,"model":"","choices":[{"index":0,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":null},"finish_reason":null}]}`
		tpl, _ = sjson.Set(tpl, "id", state.ResponseID)
		tpl, _ = sjson.Set(tpl, "created", state.CreatedAt)
		tpl, _ = sjson.Set(tpl, "model", state.Model)
	}

	switch dataType {
	case "response.reasoning_summary_text.delta":
		if delta := root.Get("delta"); delta.Exists() {
			tpl, _ = sjson.Set(tpl, "choices.0.delta.role", "assistant")
			tpl, _ = sjson.Set(tpl, "choices.0.delta.reasoning_content", delta.String())
		}

	case "response.reasoning_summary_text.done":
		tpl, _ = sjson.Set(tpl, "choices.0.delta.role", "assistant")
		tpl, _ = sjson.Set(tpl, "choices.0.delta.reasoning_content", "\n\n")

	case "response.output_text.delta":
		if delta := root.Get("delta"); delta.Exists() {
			tpl, _ = sjson.Set(tpl, "choices.0.delta.role", "assistant")
			tpl, _ = sjson.Set(tpl, "choices.0.delta.content", delta.String())
		}

	case "response.completed":
		finishReason := "stop"
		if state.FunctionCallIndex != -1 {
			finishReason = "tool_calls"
		}
		tpl, _ = sjson.Set(tpl, "choices.0.finish_reason", finishReason)

		/* usage 只在 response.completed 事件中存在，提取并存入 state */
		if usage := root.Get("response.usage"); usage.Exists() {
			state.UsageInput = usage.Get("input_tokens").Int()
			state.UsageOutput = usage.Get("output_tokens").Int()
			state.UsageTotal = usage.Get("total_tokens").Int()
			if v := usage.Get("output_tokens"); v.Exists() {
				tpl, _ = sjson.Set(tpl, "usage.completion_tokens", v.Int())
			}
			if v := usage.Get("total_tokens"); v.Exists() {
				tpl, _ = sjson.Set(tpl, "usage.total_tokens", v.Int())
			}
			if v := usage.Get("input_tokens"); v.Exists() {
				tpl, _ = sjson.Set(tpl, "usage.prompt_tokens", v.Int())
			}
			if v := usage.Get("input_tokens_details.cached_tokens"); v.Exists() {
				tpl, _ = sjson.Set(tpl, "usage.prompt_tokens_details.cached_tokens", v.Int())
			}
			if v := usage.Get("output_tokens_details.reasoning_tokens"); v.Exists() {
				tpl, _ = sjson.Set(tpl, "usage.completion_tokens_details.reasoning_tokens", v.Int())
			}
		}

	case "response.output_item.added":
		item := root.Get("item")
		if !item.Exists() || item.Get("type").String() != "function_call" {
			return nil
		}
		state.FunctionCallIndex++
		state.HasReceivedArgsDelta = false
		state.HasToolCallAnnounced = true

		fc := `{"index":0,"id":"","type":"function","function":{"name":"","arguments":""}}`
		fc, _ = sjson.Set(fc, "index", state.FunctionCallIndex)
		fc, _ = sjson.Set(fc, "id", item.Get("call_id").String())
		name := item.Get("name").String()
		if orig, ok := reverseToolMap[name]; ok {
			name = orig
		}
		fc, _ = sjson.Set(fc, "function.name", name)
		fc, _ = sjson.Set(fc, "function.arguments", "")

		tpl, _ = sjson.Set(tpl, "choices.0.delta.role", "assistant")
		tpl, _ = sjson.SetRaw(tpl, "choices.0.delta.tool_calls", `[]`)
		tpl, _ = sjson.SetRaw(tpl, "choices.0.delta.tool_calls.-1", fc)

	case "response.function_call_arguments.delta":
		state.HasReceivedArgsDelta = true
		fc := `{"index":0,"function":{"arguments":""}}`
		fc, _ = sjson.Set(fc, "index", state.FunctionCallIndex)
		fc, _ = sjson.Set(fc, "function.arguments", root.Get("delta").String())
		tpl, _ = sjson.SetRaw(tpl, "choices.0.delta.tool_calls", `[]`)
		tpl, _ = sjson.SetRaw(tpl, "choices.0.delta.tool_calls.-1", fc)

	case "response.function_call_arguments.done":
		if state.HasReceivedArgsDelta {
			return nil
		}
		fc := `{"index":0,"function":{"arguments":""}}`
		fc, _ = sjson.Set(fc, "index", state.FunctionCallIndex)
		fc, _ = sjson.Set(fc, "function.arguments", root.Get("arguments").String())
		tpl, _ = sjson.SetRaw(tpl, "choices.0.delta.tool_calls", `[]`)
		tpl, _ = sjson.SetRaw(tpl, "choices.0.delta.tool_calls.-1", fc)

	case "response.output_item.done":
		item := root.Get("item")
		if !item.Exists() || item.Get("type").String() != "function_call" {
			return nil
		}
		if state.HasToolCallAnnounced {
			state.HasToolCallAnnounced = false
			return nil
		}
		state.FunctionCallIndex++
		fc := `{"index":0,"id":"","type":"function","function":{"name":"","arguments":""}}`
		fc, _ = sjson.Set(fc, "index", state.FunctionCallIndex)
		fc, _ = sjson.Set(fc, "id", item.Get("call_id").String())
		name := item.Get("name").String()
		if orig, ok := reverseToolMap[name]; ok {
			name = orig
		}
		fc, _ = sjson.Set(fc, "function.name", name)
		fc, _ = sjson.Set(fc, "function.arguments", item.Get("arguments").String())
		tpl, _ = sjson.Set(tpl, "choices.0.delta.role", "assistant")
		tpl, _ = sjson.SetRaw(tpl, "choices.0.delta.tool_calls", `[]`)
		tpl, _ = sjson.SetRaw(tpl, "choices.0.delta.tool_calls.-1", fc)

	default:
		return nil
	}

	return []string{tpl}
}

/**
 * ConvertNonStreamResponse 将 Codex 非流式响应转换为 OpenAI Chat Completions 格式
 *
 * @param ctx - 上下文
 * @param rawJSON - Codex 完整响应 JSON（response.completed 事件的 data 部分）
 * @param reverseToolMap - 缩短名→原始名的工具名映射
 * @returns string - OpenAI Chat Completions 格式的 JSON 字符串
 */
func ConvertNonStreamResponse(_ context.Context, rawJSON []byte, reverseToolMap map[string]string) string {
	root := gjson.ParseBytes(rawJSON)
	if root.Get("type").String() != "response.completed" {
		return ""
	}

	resp := root.Get("response")
	tpl := `{"id":"","object":"chat.completion","created":0,"model":"","choices":[{"index":0,"message":{"role":"assistant","content":null,"reasoning_content":null,"tool_calls":null},"finish_reason":null}]}`

	if v := resp.Get("model"); v.Exists() {
		tpl, _ = sjson.Set(tpl, "model", v.String())
	}
	if v := resp.Get("created_at"); v.Exists() {
		tpl, _ = sjson.Set(tpl, "created", v.Int())
	} else {
		tpl, _ = sjson.Set(tpl, "created", time.Now().Unix())
	}
	if v := resp.Get("id"); v.Exists() {
		tpl, _ = sjson.Set(tpl, "id", v.String())
	}

	/* usage */
	if usage := resp.Get("usage"); usage.Exists() {
		if v := usage.Get("output_tokens"); v.Exists() {
			tpl, _ = sjson.Set(tpl, "usage.completion_tokens", v.Int())
		}
		if v := usage.Get("total_tokens"); v.Exists() {
			tpl, _ = sjson.Set(tpl, "usage.total_tokens", v.Int())
		}
		if v := usage.Get("input_tokens"); v.Exists() {
			tpl, _ = sjson.Set(tpl, "usage.prompt_tokens", v.Int())
		}
		/* 透传 cached_tokens 和 reasoning_tokens 细分信息（issue #391） */
		if v := usage.Get("input_tokens_details.cached_tokens"); v.Exists() {
			tpl, _ = sjson.Set(tpl, "usage.prompt_tokens_details.cached_tokens", v.Int())
		}
		if v := usage.Get("output_tokens_details.reasoning_tokens"); v.Exists() {
			tpl, _ = sjson.Set(tpl, "usage.completion_tokens_details.reasoning_tokens", v.Int())
		}
	}

	/* 处理 output 数组 */
	output := resp.Get("output")
	if output.IsArray() {
		var contentText, reasoningText string
		var toolCalls []string

		for _, item := range output.Array() {
			switch item.Get("type").String() {
			case "reasoning":
				if summary := item.Get("summary"); summary.IsArray() {
					for _, si := range summary.Array() {
						if si.Get("type").String() == "summary_text" {
							reasoningText = si.Get("text").String()
							break
						}
					}
				}
			case "message":
				if content := item.Get("content"); content.IsArray() {
					for _, ci := range content.Array() {
						if ci.Get("type").String() == "output_text" {
							contentText = ci.Get("text").String()
							break
						}
					}
				}
			case "function_call":
				fc := `{"id":"","type":"function","function":{"name":"","arguments":""}}`
				if v := item.Get("call_id"); v.Exists() {
					fc, _ = sjson.Set(fc, "id", v.String())
				}
				if v := item.Get("name"); v.Exists() {
					n := v.String()
					if orig, ok := reverseToolMap[n]; ok {
						n = orig
					}
					fc, _ = sjson.Set(fc, "function.name", n)
				}
				if v := item.Get("arguments"); v.Exists() {
					fc, _ = sjson.Set(fc, "function.arguments", v.String())
				}
				toolCalls = append(toolCalls, fc)
			}
		}

		if contentText != "" {
			tpl, _ = sjson.Set(tpl, "choices.0.message.content", contentText)
		}
		if reasoningText != "" {
			tpl, _ = sjson.Set(tpl, "choices.0.message.reasoning_content", reasoningText)
		}
		if len(toolCalls) > 0 {
			tpl, _ = sjson.SetRaw(tpl, "choices.0.message.tool_calls", `[]`)
			for _, tc := range toolCalls {
				tpl, _ = sjson.SetRaw(tpl, "choices.0.message.tool_calls.-1", tc)
			}
		}
	}

	/* finish_reason */
	if resp.Get("status").String() == "completed" {
		tpl, _ = sjson.Set(tpl, "choices.0.finish_reason", "stop")
	}

	return tpl
}
