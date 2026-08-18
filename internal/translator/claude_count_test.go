package translator

import (
	"fmt"
	"strings"
	"testing"

	"github.com/tiktoken-go/tokenizer"
)

func TestValidateClaudeCountTokensRequest(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		wantErr string
	}{
		{
			name:    "missing model",
			payload: `{"messages":[{"role":"user","content":"hello"}]}`,
			wantErr: "缺少 model 字段",
		},
		{
			name:    "empty messages",
			payload: `{"model":"claude-opus-5","messages":[]}`,
			wantErr: "messages 必须是非空数组",
		},
		{
			name:    "invalid role",
			payload: `{"model":"claude-opus-5","messages":[{"role":"system","content":"hello"}]}`,
			wantErr: "message.role 仅支持 user 或 assistant",
		},
		{
			name:    "invalid block type",
			payload: `{"model":"claude-opus-5","messages":[{"role":"user","content":[{"text":"hello"}]}]}`,
			wantErr: "message.content 数组中的元素必须是带 type 的对象",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateClaudeCountTokensRequest([]byte(test.payload))
			if err == nil || err.Error() != test.wantErr {
				t.Fatalf("ValidateClaudeCountTokensRequest() error = %v, want %q", err, test.wantErr)
			}
		})
	}
}

func TestCollectClaudeInputTokenSegments(t *testing.T) {
	payload := []byte(`{
		"model":"claude-test",
		"system":[
			{"type":"text","text":"Follow repository rules.","cache_control":{"type":"ephemeral"}},
			{"type":"image","source":{"type":"base64","media_type":"image/png","data":"ignored-system-image"}}
		],
		"messages":[
			{"role":"user","content":[
				{"type":"text","text":"Review the implementation."},
				{"type":"document","source":{"type":"text","data":"Reference document text."}},
				{"type":"image","source":{"type":"base64","media_type":"image/png","data":"ignored-image"}}
			]},
			{"role":"assistant","content":[
				{"type":"thinking","thinking":"Inspect the relevant files.","signature":"ignored-signature"},
				{"type":"tool_use","id":"toolu_1","name":"read_file","input":{"path":"main.go"}}
			]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"toolu_1","content":[
					{"type":"text","text":"package main"},
					{"type":"image","source":{"type":"base64","data":"ignored-tool-image"}}
				]}
			]}
		],
		"tools":[{
			"name":"read_file",
			"description":"Reads a repository file.",
			"input_schema":{"type":"object","properties":{"path":{"type":"string"}}},
			"cache_control":{"type":"ephemeral"}
		}],
		"tool_choice":{"type":"tool","name":"read_file"},
		"metadata":{"user_id":"ignored-metadata"},
		"max_tokens":4096,
		"stream":true
	}`)

	got, err := collectClaudeInputTokenSegments(payload)
	if err != nil {
		t.Fatalf("collectClaudeInputTokenSegments() error = %v", err)
	}
	want := []string{
		"Follow repository rules.",
		"user",
		"Review the implementation.",
		"Reference document text.",
		"assistant",
		"Inspect the relevant files.",
		"toolu_1",
		"read_file",
		`{"path":"main.go"}`,
		"user",
		"toolu_1",
		"package main",
		"read_file",
		"Reads a repository file.",
		`{"type":"object","properties":{"path":{"type":"string"}}}`,
		"tool",
		"read_file",
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("segments = %#v, want %#v", got, want)
	}
}

func TestCountClaudeInputTokensExcludesMultimediaAndControlFields(t *testing.T) {
	enc, err := tokenizer.Get(tokenizer.O200kBase)
	if err != nil {
		t.Fatalf("tokenizer.Get() error = %v", err)
	}

	base := []byte(`{
		"system":"System text.",
		"messages":[{"role":"user","content":[{"type":"text","text":"User text."}]}],
		"tools":[{"name":"lookup","description":"Looks up data.","input_schema":{"type":"object"}}]
	}`)
	withExcludedFields := []byte(`{
		"model":"claude-test",
		"system":"System text.",
		"messages":[{"role":"user","content":[
			{"type":"text","text":"User text."},
			{"type":"image","source":{"type":"base64","media_type":"image/png","data":"very-large-image-data"}},
			{"type":"input_audio","source":{"type":"base64","data":"very-large-audio-data"}},
			{"type":"video","source":{"type":"url","url":"https://example.com/video.mp4"}},
			{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"very-large-pdf-data"}}
		]}],
		"tools":[{"name":"lookup","description":"Looks up data.","input_schema":{"type":"object"},"cache_control":{"type":"ephemeral"}}],
		"metadata":{"large_wrapper":"ignored"},
		"max_tokens":8192,
		"temperature":0.8,
		"top_p":0.9,
		"thinking":{"type":"enabled","budget_tokens":4096},
		"output_config":{"effort":"high"},
		"speed":"fast",
		"context_management":{"edits":[{"type":"clear_thinking_20251015","keep":"all"}]},
		"stream":true
	}`)

	baseCount, errBase := countClaudeInputTokens(enc, base)
	if errBase != nil {
		t.Fatalf("countClaudeInputTokens(base) error = %v", errBase)
	}
	excludedCount, errExcluded := countClaudeInputTokens(enc, withExcludedFields)
	if errExcluded != nil {
		t.Fatalf("countClaudeInputTokens(withExcludedFields) error = %v", errExcluded)
	}
	if excludedCount != baseCount {
		t.Fatalf("count with excluded fields = %d, want %d", excludedCount, baseCount)
	}
}

func TestCollectClaudeInputTokenSegmentsIncludesKnownToolResults(t *testing.T) {
	payload := []byte(`{
		"messages":[{"role":"user","content":[
			{"type":"web_search_tool_result","tool_use_id":"ws_tool_1","content":[
				{"type":"web_search_result","source":"Search source","title":"Search result title","url":"https://search.example/result","page_age":"1 day","encrypted_content":"ignored-secret"}
			]},
			{"type":"web_fetch_tool_result","tool_use_id":"fetch_tool_1","content":{
				"type":"web_fetch_result","url":"https://docs.example/page","retrieved_at":"2026-07-22T00:00:00Z","content":{
					"type":"document","title":"Fetched document","source":{"type":"text","data":"Fetched body"}
				}
			}},
			{"type":"bash_code_execution_tool_result","tool_use_id":"bash_tool_1","content":{
				"type":"bash_code_execution_result","stdout":"command output","stderr":"command error","return_code":1,
				"content":[{"type":"text","text":"additional output"}]
			}},
			{"type":"tool_result","tool_use_id":"toolu_1","content":[
				{"type":"tool_reference","tool_name":"proxy_mcp__nia__manage_resource"}
			]}
		]}]
	}`)

	segments, err := collectClaudeInputTokenSegments(payload)
	if err != nil {
		t.Fatalf("collectClaudeInputTokenSegments() error = %v", err)
	}
	joined := "\n" + strings.Join(segments, "\n") + "\n"
	for _, want := range []string{
		"ws_tool_1",
		"Search source",
		"Search result title",
		"https://search.example/result",
		"1 day",
		"fetch_tool_1",
		"https://docs.example/page",
		"2026-07-22T00:00:00Z",
		"Fetched document",
		"Fetched body",
		"bash_tool_1",
		"command output",
		"command error",
		"1",
		"additional output",
		"toolu_1",
		"proxy_mcp__nia__manage_resource",
	} {
		if !strings.Contains(joined, "\n"+want+"\n") {
			t.Errorf("segments do not contain %q: %#v", want, segments)
		}
	}
	if strings.Contains(joined, "ignored-secret") {
		t.Fatalf("segments contain encrypted content: %#v", segments)
	}
}
