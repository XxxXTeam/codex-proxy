package translator

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/tidwall/gjson"
	"github.com/tiktoken-go/tokenizer"
)

var (
	claudeInputTokenizerOnce  sync.Once
	claudeInputTokenizerCodec tokenizer.Codec
	claudeInputTokenizerErr   error
)

/**
 * ValidateClaudeCountTokensRequest 校验 Claude count_tokens 请求的最小结构
 * @param payload - 原始请求 JSON
 * @returns error - 结构不合法时返回错误
 */
func ValidateClaudeCountTokensRequest(payload []byte) error {
	if !gjson.ValidBytes(payload) {
		return fmt.Errorf("请求体不是合法 JSON")
	}

	root := gjson.ParseBytes(payload)
	if !root.IsObject() {
		return fmt.Errorf("请求体必须是 JSON 对象")
	}
	if strings.TrimSpace(root.Get("model").String()) == "" {
		return fmt.Errorf("缺少 model 字段")
	}

	messages := root.Get("messages")
	if !messages.IsArray() || len(messages.Array()) == 0 {
		return fmt.Errorf("messages 必须是非空数组")
	}

	for _, message := range messages.Array() {
		if !message.IsObject() {
			return fmt.Errorf("messages 数组中的元素必须是对象")
		}
		role := message.Get("role").String()
		if role != "user" && role != "assistant" {
			return fmt.Errorf("message.role 仅支持 user 或 assistant")
		}
		content := message.Get("content")
		if content.Type == gjson.String {
			continue
		}
		if !content.IsArray() {
			return fmt.Errorf("message.content 必须是字符串或数组")
		}
		for _, block := range content.Array() {
			if !block.IsObject() || strings.TrimSpace(block.Get("type").String()) == "" {
				return fmt.Errorf("message.content 数组中的元素必须是带 type 的对象")
			}
		}
	}
	return nil
}

/**
 * CountClaudeInputTokens 按 Claude 请求结构估算输入 token
 * 当前实现与参考项目一致，使用 O200kBase 对文本段进行本地估算
 * @param payload - 原始 Claude 请求 JSON
 * @returns int64 - 估算得到的输入 token
 * @returns error - 计数失败时返回错误
 */
func CountClaudeInputTokens(payload []byte) (int64, error) {
	enc, err := claudeInputTokenizer()
	if err != nil {
		return 0, fmt.Errorf("初始化 O200kBase tokenizer 失败: %w", err)
	}
	count, err := countClaudeInputTokens(enc, payload)
	if err != nil {
		return 0, fmt.Errorf("计算 Claude 输入 token 失败: %w", err)
	}
	return count, nil
}

func claudeInputTokenizer() (tokenizer.Codec, error) {
	claudeInputTokenizerOnce.Do(func() {
		claudeInputTokenizerCodec, claudeInputTokenizerErr = tokenizer.Get(tokenizer.O200kBase)
	})
	return claudeInputTokenizerCodec, claudeInputTokenizerErr
}

func countClaudeInputTokens(enc tokenizer.Codec, payload []byte) (int64, error) {
	if enc == nil {
		return 0, fmt.Errorf("encoder is nil")
	}
	segments, err := collectClaudeInputTokenSegments(payload)
	if err != nil {
		return 0, err
	}
	if len(segments) == 0 {
		return 0, nil
	}
	count, err := enc.Count(strings.Join(segments, "\n"))
	if err != nil {
		return 0, err
	}
	return int64(count), nil
}

func collectClaudeInputTokenSegments(payload []byte) ([]string, error) {
	if len(bytes.TrimSpace(payload)) == 0 {
		return nil, nil
	}
	if !gjson.ValidBytes(payload) {
		return nil, fmt.Errorf("invalid Claude request JSON")
	}

	root := gjson.ParseBytes(payload)
	segments := make([]string, 0, 32)
	collectClaudeSystemTokenSegments(root.Get("system"), &segments)
	collectClaudeMessageTokenSegments(root.Get("messages"), &segments)
	collectClaudeToolTokenSegments(root.Get("tools"), &segments)
	collectClaudeToolChoiceTokenSegments(root.Get("tool_choice"), &segments)
	return segments, nil
}

func collectClaudeSystemTokenSegments(system gjson.Result, segments *[]string) {
	if system.Type == gjson.String {
		appendClaudeTokenString(segments, system.String())
		return
	}
	if !system.IsArray() {
		return
	}
	system.ForEach(func(_, part gjson.Result) bool {
		if part.Type == gjson.String {
			appendClaudeTokenString(segments, part.String())
		} else if part.Get("type").String() == "text" {
			appendClaudeTokenString(segments, part.Get("text").String())
		}
		return true
	})
}

func collectClaudeMessageTokenSegments(messages gjson.Result, segments *[]string) {
	if !messages.IsArray() {
		return
	}
	messages.ForEach(func(_, message gjson.Result) bool {
		appendClaudeTokenString(segments, message.Get("role").String())
		collectClaudeContentTokenSegments(message.Get("content"), segments)
		return true
	})
}

func collectClaudeContentTokenSegments(content gjson.Result, segments *[]string) {
	if !content.Exists() {
		return
	}
	if content.Type == gjson.String {
		appendClaudeTokenString(segments, content.String())
		return
	}
	if content.IsArray() {
		content.ForEach(func(_, part gjson.Result) bool {
			collectClaudeContentTokenSegments(part, segments)
			return true
		})
		return
	}
	if !content.IsObject() {
		return
	}

	switch content.Get("type").String() {
	case "text":
		appendClaudeTokenString(segments, content.Get("text").String())
	case "thinking":
		appendClaudeTokenString(segments, content.Get("thinking").String())
	case "document":
		collectClaudeDocumentTokenSegments(content, segments)
	case "tool_use", "server_tool_use", "mcp_tool_use":
		appendClaudeTokenString(segments, content.Get("id").String())
		appendClaudeTokenString(segments, content.Get("name").String())
		appendClaudeTokenJSON(segments, content.Get("input"))
	case "tool_result", "mcp_tool_result", "web_search_tool_result", "web_fetch_tool_result", "code_execution_tool_result", "bash_code_execution_tool_result", "text_editor_code_execution_tool_result":
		appendClaudeTokenString(segments, content.Get("tool_use_id").String())
		appendClaudeTokenString(segments, content.Get("tool_call_id").String())
		collectClaudeContentTokenSegments(content.Get("content"), segments)
	case "web_search_result", "search_result":
		if source := content.Get("source"); source.Type == gjson.String {
			appendClaudeTokenString(segments, source.String())
		}
		appendClaudeTokenString(segments, content.Get("title").String())
		appendClaudeTokenString(segments, content.Get("url").String())
		appendClaudeTokenString(segments, content.Get("page_age").String())
		collectClaudeContentTokenSegments(content.Get("content"), segments)
	case "web_fetch_result":
		appendClaudeTokenString(segments, content.Get("url").String())
		appendClaudeTokenString(segments, content.Get("retrieved_at").String())
		collectClaudeContentTokenSegments(content.Get("content"), segments)
	case "code_execution_result", "bash_code_execution_result", "text_editor_code_execution_result":
		appendClaudeTokenString(segments, content.Get("stdout").String())
		appendClaudeTokenString(segments, content.Get("stderr").String())
		appendClaudeTokenString(segments, content.Get("return_code").String())
		collectClaudeContentTokenSegments(content.Get("content"), segments)
		collectClaudeContentTokenSegments(content.Get("output"), segments)
	case "tool_reference":
		appendClaudeTokenString(segments, content.Get("tool_name").String())
	case "image", "input_audio", "audio", "video", "redacted_thinking":
		return
	case "":
		appendClaudeTokenJSON(segments, content)
	default:
		appendClaudeTokenString(segments, content.Get("text").String())
	}
}

func collectClaudeDocumentTokenSegments(document gjson.Result, segments *[]string) {
	source := document.Get("source")
	if source.Get("type").String() != "text" {
		return
	}
	appendClaudeTokenString(segments, document.Get("title").String())
	appendClaudeTokenString(segments, document.Get("context").String())
	appendClaudeTokenString(segments, source.Get("data").String())
	appendClaudeTokenString(segments, source.Get("content").String())
}

func collectClaudeToolTokenSegments(tools gjson.Result, segments *[]string) {
	if !tools.IsArray() {
		return
	}
	tools.ForEach(func(_, tool gjson.Result) bool {
		appendClaudeTokenString(segments, tool.Get("type").String())
		appendClaudeTokenString(segments, tool.Get("name").String())
		appendClaudeTokenString(segments, tool.Get("description").String())
		appendClaudeTokenJSON(segments, tool.Get("input_schema"))
		return true
	})
}

func collectClaudeToolChoiceTokenSegments(toolChoice gjson.Result, segments *[]string) {
	if !toolChoice.Exists() {
		return
	}
	if toolChoice.Type == gjson.String {
		appendClaudeTokenString(segments, toolChoice.String())
		return
	}
	appendClaudeTokenString(segments, toolChoice.Get("type").String())
	appendClaudeTokenString(segments, toolChoice.Get("name").String())
}

func appendClaudeTokenString(segments *[]string, value string) {
	if segments == nil {
		return
	}
	if trimmed := strings.TrimSpace(value); trimmed != "" {
		*segments = append(*segments, trimmed)
	}
}

func appendClaudeTokenJSON(segments *[]string, value gjson.Result) {
	if !value.Exists() {
		return
	}
	if value.Type == gjson.String {
		appendClaudeTokenString(segments, value.String())
		return
	}
	raw := strings.TrimSpace(value.Raw)
	if raw == "" {
		return
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, []byte(raw)); err == nil {
		appendClaudeTokenString(segments, compact.String())
		return
	}
	appendClaudeTokenString(segments, raw)
}
