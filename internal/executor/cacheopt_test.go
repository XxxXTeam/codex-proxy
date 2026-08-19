package executor

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func promptCacheTestBody(t *testing.T, root map[string]any) []byte {
	t.Helper()
	body, err := json.Marshal(root)
	if err != nil {
		t.Fatalf("序列化测试请求失败: %v", err)
	}
	return body
}

func TestOptimizeCacheForCodexDisabledKeepsBody(t *testing.T) {
	body := []byte(`{"instructions":"base","input":[{"type":"message","role":"developer","content":"rules"}]}`)
	fps := &RequestsFingerprint{promptCache: NewPromptCacheOptions(false, "stable-v1", true, true, nil)}
	if got := fps.OptimizeCacheForCodex(body, "gpt-5.2-codex"); string(got) != string(body) {
		t.Fatalf("关闭时必须逐字保留请求体: got=%s", got)
	}
}

func TestOptimizeCacheForCodexPromotesStableDeveloperPrefix(t *testing.T) {
	stable := strings.Repeat("fixed policy text ", 24) + "run=20260819\r\n\r\n"
	body := promptCacheTestBody(t, map[string]any{
		"instructions": "\r\nbase instruction  \r\n\r\n",
		"input": []any{
			map[string]any{
				"type":    "message",
				"role":    "developer",
				"content": stable,
			},
			map[string]any{
				"type":    "message",
				"role":    "developer",
				"content": stable,
			},
			map[string]any{
				"type":    "message",
				"role":    "user",
				"content": "implement this",
			},
		},
	})
	fps := &RequestsFingerprint{promptCache: NewPromptCacheOptions(
		true,
		"stable-v1",
		true,
		true,
		[]PromptCacheReplacement{{Pattern: `run=\d+`, Replace: "run=<stable>"}},
	)}

	out := fps.OptimizeCacheForCodex(body, "gpt-5.2-codex")
	instructions := gjson.GetBytes(out, "instructions").String()
	expected := "<!-- codex-proxy-prompt-cache:stable-v1 -->\n\nbase instruction\n\n" + strings.TrimSuffix(strings.Repeat("fixed policy text ", 24)+"run=<stable>\n", "\n")
	if instructions != expected {
		t.Fatalf("稳定 instructions 不符:\ngot:  %q\nwant: %q", instructions, expected)
	}
	input := gjson.GetBytes(out, "input")
	if !input.IsArray() || len(input.Array()) != 1 {
		t.Fatalf("提升后仅应保留 user message: %s", input.Raw)
	}
	if role := input.Get("0.role").String(); role != "user" {
		t.Fatalf("剩余 input 顺序异常: role=%s", role)
	}
	if strings.Count(instructions, "run=<stable>") != 1 {
		t.Fatalf("大块重复 developer 提示词应合并一次: %q", instructions)
	}
	if strings.Contains(instructions, "20260819") {
		t.Fatalf("显式替换规则未应用: %q", instructions)
	}
}

func TestOptimizeCacheForCodexKeepsDeveloperMessageForHistory(t *testing.T) {
	body := promptCacheTestBody(t, map[string]any{
		"instructions":         "base",
		"previous_response_id": "resp_previous",
		"input": []any{
			map[string]any{
				"type":    "message",
				"role":    "developer",
				"content": "new instruction run=100",
			},
		},
	})
	fps := &RequestsFingerprint{promptCache: NewPromptCacheOptions(
		true,
		"stable-v1",
		false,
		false,
		[]PromptCacheReplacement{{Pattern: `run=\d+`, Replace: "run=<stable>"}},
	)}

	out := fps.OptimizeCacheForCodex(body, "gpt-5.2-codex")
	if instructions := gjson.GetBytes(out, "instructions").String(); instructions != "<!-- codex-proxy-prompt-cache:stable-v1 -->\n\nbase" {
		t.Fatalf("连续对话仅应注入稳定标签: %q", instructions)
	}
	content := gjson.GetBytes(out, "input.0.content").String()
	if content != "new instruction run=100" {
		t.Fatalf("连续对话 developer message 不应提升或替换: %q", content)
	}
}

func TestOptimizeCacheForCodexKeepsStructuredDeveloperMessage(t *testing.T) {
	body := promptCacheTestBody(t, map[string]any{
		"input": []any{
			map[string]any{
				"type": "message",
				"role": "developer",
				"content": []any{
					map[string]any{"type": "input_text", "text": "rules"},
					map[string]any{"type": "input_file", "file_id": "file_1"},
				},
			},
		},
	})
	fps := &RequestsFingerprint{promptCache: NewPromptCacheOptions(true, "stable-v1", true, true, nil)}
	out := fps.OptimizeCacheForCodex(body, "gpt-5.2-codex")
	if count := len(gjson.GetBytes(out, "input").Array()); count != 1 {
		t.Fatalf("结构化 developer content 不应被移除: count=%d", count)
	}
	if instructions := gjson.GetBytes(out, "instructions").String(); instructions != "<!-- codex-proxy-prompt-cache:stable-v1 -->" {
		t.Fatalf("缺少稳定标签: %q", instructions)
	}
}
