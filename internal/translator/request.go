/**
 * 请求转换模块
 * 将 OpenAI Chat Completions 格式的请求转换为 Codex (OpenAI Responses API) 格式
 * 处理消息、工具调用、多模态内容、结构化输出等的格式映射
 */
package translator

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

/**
 * ConvertOpenAIRequestToCodex 将 OpenAI Chat Completions 请求转为 Codex Responses API 格式
 *
 * 转换规则：
 *   - messages → input 数组（message/function_call/function_call_output）
 *   - system role → developer role
 *   - tools 中 function 类型展平为顶层 function 定义
 *   - response_format → text.format
 *   - 工具名超过 64 字符自动缩短
 *
 * @param modelName - 模型名称
 * @param rawJSON - 原始 OpenAI Chat Completions 请求 JSON
 * @param stream - 是否为流式请求
 * @returns []byte - 转换后的 Codex Responses API 请求 JSON
 */
func ConvertOpenAIRequestToCodex(modelName string, rawJSON []byte, stream bool) []byte {
	out := `{"instructions":""}`

	out, _ = sjson.Set(out, "stream", stream)

	/*
	 * 映射 reasoning 参数（修复降智问题）
	 * Chat Completions 格式: "reasoning_effort": "high"
	 * Responses API 格式:   "reasoning": {"effort": "high"}
	 * 优先使用请求体中已有的值，不再强制设为 medium
	 */
	if v := gjson.GetBytes(rawJSON, "reasoning_effort"); v.Exists() {
		/* Chat Completions 格式 → 转为 Codex 嵌套格式 */
		out, _ = sjson.Set(out, "reasoning.effort", v.Value())
	} else if v := gjson.GetBytes(rawJSON, "reasoning.effort"); v.Exists() {
		/* Responses API 格式 → 直接透传 */
		out, _ = sjson.Set(out, "reasoning.effort", v.Value())
	} else if v := gjson.GetBytes(rawJSON, "variant"); v.Exists() {
		/* OpenWork 等客户端使用 variant 代替 reasoning_effort（issue #258） */
		out, _ = sjson.Set(out, "reasoning.effort", v.Value())
	}
	out, _ = sjson.Set(out, "parallel_tool_calls", true)
	out, _ = sjson.Set(out, "reasoning.summary", "auto")
	out, _ = sjson.Set(out, "include", []string{"reasoning.encrypted_content"})
	out, _ = sjson.Set(out, "model", modelName)

	/* 构建工具名缩短映射 */
	originalToolNameMap := buildToolNameMap(rawJSON)

	/*
	 * 构建 input 数组
	 * Responses API 格式已有 input 字段，走快速路径
	 * Chat Completions 格式只有 messages 字段，需要转换为 input
	 */
	existingInput := gjson.GetBytes(rawJSON, "input")
	if existingInput.Exists() {
		/*
		 * Responses API 快速路径：直接在原始 JSON 上原地修改
		 * 不重建 JSON，大幅减少序列化开销和内存分配
		 */
		result := make([]byte, len(rawJSON))
		copy(result, rawJSON)

		/* input 为字符串时，转为标准消息数组格式 */
		if existingInput.Type == gjson.String {
			inputArr, _ := sjson.Set(
				`[{"type":"message","role":"user","content":[{"type":"input_text","text":""}]}]`,
				"0.content.0.text", existingInput.String(),
			)
			result, _ = sjson.SetRawBytes(result, "input", []byte(inputArr))
		}

		result, _ = sjson.SetBytes(result, "model", modelName)
		result, _ = sjson.SetBytes(result, "stream", stream)
		result, _ = sjson.SetBytes(result, "store", false)
		result, _ = sjson.SetBytes(result, "parallel_tool_calls", true)
		result, _ = sjson.SetBytes(result, "include", []string{"reasoning.encrypted_content"})

		if v := gjson.GetBytes(rawJSON, "reasoning_effort"); v.Exists() {
			result, _ = sjson.SetBytes(result, "reasoning.effort", v.Value())
			result, _ = sjson.DeleteBytes(result, "reasoning_effort")
		} else if v := gjson.GetBytes(rawJSON, "variant"); v.Exists() {
			/* OpenWork 等客户端使用 variant 代替 reasoning_effort（issue #258） */
			result, _ = sjson.SetBytes(result, "reasoning.effort", v.Value())
		}

		/* 确保 instructions 存在 */
		if !gjson.GetBytes(result, "instructions").Exists() {
			result, _ = sjson.SetBytes(result, "instructions", "")
		}

		/* 删除上游不支持的参数 */
		result, _ = sjson.DeleteBytes(result, "previous_response_id")
		result, _ = sjson.DeleteBytes(result, "stream_options")
		result, _ = sjson.DeleteBytes(result, "prompt_cache_retention")
		result, _ = sjson.DeleteBytes(result, "safety_identifier")
		result, _ = sjson.DeleteBytes(result, "generate")
		result, _ = sjson.DeleteBytes(result, "max_output_tokens")
		result, _ = sjson.DeleteBytes(result, "max_completion_tokens")
		result, _ = sjson.DeleteBytes(result, "temperature")
		result, _ = sjson.DeleteBytes(result, "top_p")
		result, _ = sjson.DeleteBytes(result, "truncation")
		result, _ = sjson.DeleteBytes(result, "context_management")
		result, _ = sjson.DeleteBytes(result, "user")
		result, _ = sjson.DeleteBytes(result, "variant")

		/* service_tier：-fast 时 ApplyThinking 写入 priority，其余透传客户端原值 */

		/* 修复 tools 中 array 类型缺少 items 的 schema 问题 */
		result = fixToolsArraySchema(result)

		/* system role → developer 转换（Codex 不接受 system role） */
		result = convertSystemRoleToDeveloper(result)

		/* 使用 json_object/json_schema 时上游要求 input 含 "json" */
		result = ensureInputContainsJSON(result)

		return result
	}

	/* Chat Completions 格式：messages → input 转换 */
	messages := gjson.GetBytes(rawJSON, "messages")
	out, _ = sjson.SetRaw(out, "input", `[]`)

	if messages.IsArray() {
		arr := messages.Array()
		for i := 0; i < len(arr); i++ {
			m := arr[i]
			role := m.Get("role").String()

			switch role {
			case "tool":
				/* tool 消息转为 function_call_output */
				funcOutput := `{}`
				funcOutput, _ = sjson.Set(funcOutput, "type", "function_call_output")
				funcOutput, _ = sjson.Set(funcOutput, "call_id", m.Get("tool_call_id").String())
				funcOutput, _ = sjson.Set(funcOutput, "output", m.Get("content").String())
				out, _ = sjson.SetRaw(out, "input.-1", funcOutput)

			default:
				/* 常规消息 */
				msg := `{}`
				msg, _ = sjson.Set(msg, "type", "message")
				if role == "system" {
					msg, _ = sjson.Set(msg, "role", "developer")
				} else {
					msg, _ = sjson.Set(msg, "role", role)
				}
				msg, _ = sjson.SetRaw(msg, "content", `[]`)

				/* 处理内容 */
				c := m.Get("content")
				if c.Exists() && c.Type == gjson.String && c.String() != "" {
					partType := "input_text"
					if role == "assistant" {
						partType = "output_text"
					}
					part := `{}`
					part, _ = sjson.Set(part, "type", partType)
					part, _ = sjson.Set(part, "text", c.String())
					msg, _ = sjson.SetRaw(msg, "content.-1", part)
				} else if c.Exists() && c.IsArray() {
					items := c.Array()
					for j := 0; j < len(items); j++ {
						it := items[j]
						t := it.Get("type").String()
						switch t {
						case "text":
							partType := "input_text"
							if role == "assistant" {
								partType = "output_text"
							}
							part := `{}`
							part, _ = sjson.Set(part, "type", partType)
							part, _ = sjson.Set(part, "text", it.Get("text").String())
							msg, _ = sjson.SetRaw(msg, "content.-1", part)
						case "image_url":
							if role == "user" {
								part := `{}`
								part, _ = sjson.Set(part, "type", "input_image")
								if u := it.Get("image_url.url"); u.Exists() {
									part, _ = sjson.Set(part, "image_url", u.String())
								}
								msg, _ = sjson.SetRaw(msg, "content.-1", part)
							}
						case "file":
							if role == "user" {
								fileData := it.Get("file.file_data").String()
								filename := it.Get("file.filename").String()
								if fileData != "" {
									part := `{}`
									part, _ = sjson.Set(part, "type", "input_file")
									part, _ = sjson.Set(part, "file_data", fileData)
									if filename != "" {
										part, _ = sjson.Set(part, "filename", filename)
									}
									msg, _ = sjson.SetRaw(msg, "content.-1", part)
								}
							}
						}
					}
				}
				out, _ = sjson.SetRaw(out, "input.-1", msg)

				/* assistant 消息的 tool_calls 转为独立的 function_call 对象 */
				if role == "assistant" {
					toolCalls := m.Get("tool_calls")
					if toolCalls.Exists() && toolCalls.IsArray() {
						tcArr := toolCalls.Array()
						for j := 0; j < len(tcArr); j++ {
							tc := tcArr[j]
							if tc.Get("type").String() == "function" {
								funcCall := `{}`
								funcCall, _ = sjson.Set(funcCall, "type", "function_call")
								funcCall, _ = sjson.Set(funcCall, "call_id", tc.Get("id").String())
								name := tc.Get("function.name").String()
								if short, ok := originalToolNameMap[name]; ok {
									name = short
								} else {
									name = shortenNameIfNeeded(name)
								}
								funcCall, _ = sjson.Set(funcCall, "name", name)
								funcCall, _ = sjson.Set(funcCall, "arguments", tc.Get("function.arguments").String())
								out, _ = sjson.SetRaw(out, "input.-1", funcCall)
							}
						}
					}
				}
			}
		}
	}

	/* 映射 response_format 到 text.format */
	rf := gjson.GetBytes(rawJSON, "response_format")
	if rf.Exists() {
		if !gjson.Get(out, "text").Exists() {
			out, _ = sjson.SetRaw(out, "text", `{}`)
		}
		rft := rf.Get("type").String()
		switch rft {
		case "text":
			out, _ = sjson.Set(out, "text.format.type", "text")
		case "json_object":
			out, _ = sjson.Set(out, "text.format.type", "json_object")
		case "json_schema":
			js := rf.Get("json_schema")
			if js.Exists() {
				out, _ = sjson.Set(out, "text.format.type", "json_schema")
				if v := js.Get("name"); v.Exists() {
					out, _ = sjson.Set(out, "text.format.name", v.Value())
				}
				if v := js.Get("strict"); v.Exists() {
					out, _ = sjson.Set(out, "text.format.strict", v.Value())
				}
				if v := js.Get("schema"); v.Exists() {
					out, _ = sjson.SetRaw(out, "text.format.schema", v.Raw)
				}
			}
		}
	}

	/* 映射 tools */
	tools := gjson.GetBytes(rawJSON, "tools")
	if tools.IsArray() && len(tools.Array()) > 0 {
		out, _ = sjson.SetRaw(out, "tools", `[]`)
		arr := tools.Array()
		for i := 0; i < len(arr); i++ {
			t := arr[i]
			toolType := t.Get("type").String()
			if toolType == "custom" {
				item := `{}`
				item, _ = sjson.Set(item, "type", "function")
				if v := t.Get("name"); v.Exists() {
					name := v.String()
					if short, ok := originalToolNameMap[name]; ok {
						name = short
					} else {
						name = shortenNameIfNeeded(name)
					}
					item, _ = sjson.Set(item, "name", name)
				}
				if v := t.Get("description"); v.Exists() {
					item, _ = sjson.Set(item, "description", v.Value())
				}
				/* 将 format 信息编码到 parameters 的 description 中 */
				if format := t.Get("format"); format.Exists() {
					paramSchema := `{"type":"object","properties":{"patch":{"type":"string","description":"The patch content"}}}`
					item, _ = sjson.SetRaw(item, "parameters", paramSchema)
				} else {
					item, _ = sjson.SetRaw(item, "parameters", `{"type":"object","properties":{}}`)
				}
				out, _ = sjson.SetRaw(out, "tools.-1", item)
				continue
			}

			/* 非 function/custom 类型直接透传 */
			if toolType != "" && toolType != "function" && t.IsObject() {
				out, _ = sjson.SetRaw(out, "tools.-1", t.Raw)
				continue
			}

			if toolType == "function" {
				item := `{}`
				item, _ = sjson.Set(item, "type", "function")
				fn := t.Get("function")
				if fn.Exists() {
					if v := fn.Get("name"); v.Exists() {
						name := v.String()
						if short, ok := originalToolNameMap[name]; ok {
							name = short
						} else {
							name = shortenNameIfNeeded(name)
						}
						item, _ = sjson.Set(item, "name", name)
					}
					if v := fn.Get("description"); v.Exists() {
						item, _ = sjson.Set(item, "description", v.Value())
					}
					if v := fn.Get("parameters"); v.Exists() {
						/* 修复 array 类型缺少 items 的 schema 问题 */
						item, _ = sjson.SetRaw(item, "parameters", fixArraySchemaInParams(v.Raw))
					}
					if v := fn.Get("strict"); v.Exists() {
						item, _ = sjson.Set(item, "strict", v.Value())
					}
				}
				out, _ = sjson.SetRaw(out, "tools.-1", item)
			}
		}
	}

	/* 映射 tool_choice */
	if tc := gjson.GetBytes(rawJSON, "tool_choice"); tc.Exists() {
		switch {
		case tc.Type == gjson.String:
			out, _ = sjson.Set(out, "tool_choice", tc.String())
		case tc.IsObject():
			tcType := tc.Get("type").String()
			if tcType == "function" {
				name := tc.Get("function.name").String()
				if name != "" {
					if short, ok := originalToolNameMap[name]; ok {
						name = short
					} else {
						name = shortenNameIfNeeded(name)
					}
				}
				choice := `{}`
				choice, _ = sjson.Set(choice, "type", "function")
				if name != "" {
					choice, _ = sjson.Set(choice, "name", name)
				}
				out, _ = sjson.SetRaw(out, "tool_choice", choice)
			} else if tcType != "" {
				out, _ = sjson.SetRaw(out, "tool_choice", tc.Raw)
			}
		}
	}

	/* -fast 时为 priority；其余透传客户端自带的 service_tier（若有） */
	if v := gjson.GetBytes(rawJSON, "service_tier"); v.Exists() {
		if st := strings.TrimSpace(v.String()); st != "" {
			out, _ = sjson.Set(out, "service_tier", st)
		}
	}

	out, _ = sjson.Set(out, "store", false)
	outBytes := ensureInputContainsJSON([]byte(out))
	return outBytes
}

/**
 * buildToolNameMap 构建工具名缩短映射表
 * @param rawJSON - 原始请求 JSON
 * @returns map[string]string - 原始名 → 缩短名的映射
 */
func buildToolNameMap(rawJSON []byte) map[string]string {
	m := map[string]string{}
	tools := gjson.GetBytes(rawJSON, "tools")
	if !tools.IsArray() || len(tools.Array()) == 0 {
		return m
	}

	var names []string
	arr := tools.Array()
	for i := 0; i < len(arr); i++ {
		t := arr[i]
		if t.Get("type").String() == "function" {
			fn := t.Get("function")
			if fn.Exists() {
				if v := fn.Get("name"); v.Exists() {
					names = append(names, v.String())
				}
			}
		}
	}

	if len(names) > 0 {
		return buildShortNameMap(names)
	}
	return m
}

/**
 * shortenNameIfNeeded 对单个工具名进行缩短处理
 * 如果名称超过 64 字符，尝试保留 mcp__ 前缀和最后一段
 * @param name - 工具名
 * @returns string - 缩短后的工具名
 */
func shortenNameIfNeeded(name string) string {
	const limit = 64
	if len(name) <= limit {
		return name
	}
	if strings.HasPrefix(name, "mcp__") {
		idx := strings.LastIndex(name, "__")
		if idx > 0 {
			candidate := "mcp__" + name[idx+2:]
			if len(candidate) > limit {
				return candidate[:limit]
			}
			return candidate
		}
	}
	return name[:limit]
}

/**
 * buildShortNameMap 构建唯一的短工具名映射
 * 保留 mcp__ 前缀，使用 _1/_2 后缀确保唯一性
 * @param names - 原始工具名列表
 * @returns map[string]string - 原始名 → 唯一短名的映射
 */
func buildShortNameMap(names []string) map[string]string {
	const limit = 64
	used := map[string]struct{}{}
	m := map[string]string{}

	baseCandidate := func(n string) string {
		if len(n) <= limit {
			return n
		}
		if strings.HasPrefix(n, "mcp__") {
			idx := strings.LastIndex(n, "__")
			if idx > 0 {
				cand := "mcp__" + n[idx+2:]
				if len(cand) > limit {
					cand = cand[:limit]
				}
				return cand
			}
		}
		return n[:limit]
	}

	makeUnique := func(cand string) string {
		if _, ok := used[cand]; !ok {
			return cand
		}
		base := cand
		for i := 1; ; i++ {
			suffix := "_" + strconv.Itoa(i)
			allowed := limit - len(suffix)
			if allowed < 0 {
				allowed = 0
			}
			tmp := base
			if len(tmp) > allowed {
				tmp = tmp[:allowed]
			}
			tmp = tmp + suffix
			if _, ok := used[tmp]; !ok {
				return tmp
			}
		}
	}

	for _, n := range names {
		cand := baseCandidate(n)
		uniq := makeUnique(cand)
		used[uniq] = struct{}{}
		m[n] = uniq
	}
	return m
}
func ensureInputContainsJSON(body []byte) []byte {
	ft := strings.ToLower(gjson.GetBytes(body, "text.format.type").String())
	if ft != "json_object" && ft != "json_schema" {
		return body
	}
	needle := "json"
	hasJSON := func(s string) bool { return strings.Contains(strings.ToLower(s), needle) }

	inputNode := gjson.GetBytes(body, "input")
	if inputNode.Exists() && inputNode.IsArray() {
		for _, it := range inputNode.Array() {
			if it.Get("type").String() != "message" {
				continue
			}
			for _, c := range it.Get("content").Array() {
				if hasJSON(c.Get("text").String()) {
					return body
				}
			}
		}
	}

	synth := json.RawMessage(`{"type":"message","role":"developer","content":[{"type":"input_text","text":"Respond in JSON format."}]}`)

	if !inputNode.Exists() || !inputNode.IsArray() {
		wrapped, err := json.Marshal([]json.RawMessage{synth})
		if err != nil {
			return body
		}
		out, err := sjson.SetRawBytes(body, "input", wrapped)
		if err != nil {
			return body
		}
		return out
	}

	var items []json.RawMessage
	if err := json.Unmarshal([]byte(inputNode.Raw), &items); err != nil {
		return body
	}
	newItems := append([]json.RawMessage{synth}, items...)
	newInput, err := json.Marshal(newItems)
	if err != nil {
		return body
	}
	out, err := sjson.SetRawBytes(body, "input", newInput)
	if err != nil {
		return body
	}
	return out
}

/**
 * convertSystemRoleToDeveloper 遍历 input 数组，将 role="system" 转为 role="developer"
 * Codex API 不接受 "system" role，必须使用 "developer"
 * @param rawJSON - 请求体 JSON
 * @returns []byte - 转换后的 JSON
 */
func convertSystemRoleToDeveloper(rawJSON []byte) []byte {
	inputResult := gjson.GetBytes(rawJSON, "input")
	if !inputResult.IsArray() {
		return rawJSON
	}

	inputArray := inputResult.Array()
	result := rawJSON

	for i := 0; i < len(inputArray); i++ {
		rolePath := fmt.Sprintf("input.%d.role", i)
		if gjson.GetBytes(result, rolePath).String() == "system" {
			result, _ = sjson.SetBytes(result, rolePath, "developer")
		}
	}

	return result
}

/**
 * BuildReverseToolNameMap 从原始 OpenAI 请求构建反向工具名映射
 * 用于将 Codex 响应中缩短的工具名还原为原始名
 * @param originalJSON - 原始 OpenAI 请求 JSON
 * @returns map[string]string - 缩短名 → 原始名的映射
 */
func BuildReverseToolNameMap(originalJSON []byte) map[string]string {
	tools := gjson.GetBytes(originalJSON, "tools")
	rev := map[string]string{}
	if !tools.IsArray() || len(tools.Array()) == 0 {
		return rev
	}

	var names []string
	arr := tools.Array()
	for i := 0; i < len(arr); i++ {
		t := arr[i]
		if t.Get("type").String() != "function" {
			continue
		}
		fn := t.Get("function")
		if !fn.Exists() {
			continue
		}
		if v := fn.Get("name"); v.Exists() {
			names = append(names, v.String())
		}
	}

	if len(names) > 0 {
		m := buildShortNameMap(names)
		for orig, short := range m {
			rev[short] = orig
		}
	}
	return rev
}

/**
 * fixToolsArraySchema 遍历请求体中的 tools 数组，修复每个 function 的 parameters schema
 * 用于 Responses API 快速路径（[]byte 格式的原始 JSON）
 * @param rawJSON - 请求体 JSON
 * @returns []byte - 修复后的 JSON
 */
func fixToolsArraySchema(rawJSON []byte) []byte {
	tools := gjson.GetBytes(rawJSON, "tools")
	if !tools.IsArray() {
		return rawJSON
	}
	result := rawJSON
	for i, t := range tools.Array() {
		var paramsPath string
		if t.Get("type").String() == "function" && t.Get("function.parameters").Exists() {
			paramsPath = fmt.Sprintf("tools.%d.function.parameters", i)
		} else if t.Get("parameters").Exists() {
			paramsPath = fmt.Sprintf("tools.%d.parameters", i)
		}
		if paramsPath != "" {
			raw := gjson.GetBytes(result, paramsPath).Raw
			fixed := fixArraySchemaInParams(raw)
			if fixed != raw {
				result, _ = sjson.SetRawBytes(result, paramsPath, []byte(fixed))
			}
		}
	}
	return result
}

/**
 * fixArraySchemaInParams 修复 JSON Schema 中 type=array 但缺少 items 的节点
 * 上游 Codex API 要求所有 array 类型必须有 items 字段，否则返回 400
 * 递归遍历 schema 的 properties/items/oneOf/anyOf/allOf 等所有嵌套层级
 * @param raw - tool parameters 的原始 JSON
 * @returns string - 修复后的 JSON（如果无需修复则原样返回）
 */
func fixArraySchemaInParams(raw string) string {
	if raw == "" || raw == "{}" {
		return raw
	}
	var schema map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &schema); err != nil {
		return raw
	}
	if fixSchemaNode(schema) {
		if fixed, err := json.Marshal(schema); err == nil {
			return string(fixed)
		}
	}
	return raw
}

/**
 * fixSchemaNode 递归修复单个 schema 节点
 * @param node - schema 节点（map）
 * @returns bool - 是否做了修复
 */
func fixSchemaNode(node map[string]interface{}) bool {
	changed := false

	/* 当前节点是 array 类型但缺少 items → 补上空 items */
	if t, ok := node["type"]; ok {
		if ts, isStr := t.(string); isStr && ts == "array" {
			if _, hasItems := node["items"]; !hasItems {
				node["items"] = map[string]interface{}{}
				changed = true
			}
		}
	}

	/* 递归处理 properties 中的每个属性 */
	if props, ok := node["properties"].(map[string]interface{}); ok {
		for _, v := range props {
			if sub, ok := v.(map[string]interface{}); ok {
				if fixSchemaNode(sub) {
					changed = true
				}
			}
		}
	}

	/* 递归处理 items */
	if items, ok := node["items"].(map[string]interface{}); ok {
		if fixSchemaNode(items) {
			changed = true
		}
	}

	/* 递归处理 oneOf / anyOf / allOf / prefixItems */
	for _, key := range []string{"oneOf", "anyOf", "allOf", "prefixItems"} {
		if arr, ok := node[key].([]interface{}); ok {
			for _, elem := range arr {
				if sub, ok := elem.(map[string]interface{}); ok {
					if fixSchemaNode(sub) {
						changed = true
					}
				}
			}
		}
	}

	/* 递归处理 additionalProperties（如果是 schema 对象） */
	if ap, ok := node["additionalProperties"].(map[string]interface{}); ok {
		if fixSchemaNode(ap) {
			changed = true
		}
	}

	return changed
}
