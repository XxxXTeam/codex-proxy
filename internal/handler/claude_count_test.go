package handler

import (
	"testing"

	"github.com/tidwall/gjson"
	"github.com/valyala/fasthttp"
)

func TestHandleMessageCountTokens(t *testing.T) {
	h := &ProxyHandler{}
	var ctx fasthttp.RequestCtx
	ctx.Request.SetBodyString(`{
		"model":"claude-opus-5",
		"messages":[
			{"role":"user","content":[{"type":"text","text":"hello world"}]},
			{"role":"assistant","content":[{"type":"thinking","thinking":"internal note"}]}
		],
		"system":[{"type":"text","text":"follow repo rules"}],
		"tools":[{"name":"lookup","description":"query data","input_schema":{"type":"object","properties":{"id":{"type":"string"}}}}],
		"thinking":{"type":"enabled","budget_tokens":1024},
		"output_config":{"effort":"high"},
		"speed":"fast",
		"context_management":{"edits":[{"type":"clear_thinking_20251015","keep":"all"}]}
	}`)

	h.handleMessageCountTokens(&ctx)

	if got := ctx.Response.StatusCode(); got != fasthttp.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", got, fasthttp.StatusOK, ctx.Response.Body())
	}
	body := ctx.Response.Body()
	if got := gjson.GetBytes(body, "input_tokens").Int(); got <= 0 {
		t.Fatalf("input_tokens = %d, want positive; body=%s", got, body)
	}
	if got := gjson.GetBytes(body, "context_management.original_input_tokens").Int(); got != 0 {
		t.Fatalf("original_input_tokens = %d, want 0; body=%s", got, body)
	}
}

func TestHandleMessageCountTokensValidationError(t *testing.T) {
	h := &ProxyHandler{}
	var ctx fasthttp.RequestCtx
	ctx.Request.SetBodyString(`{"messages":[{"role":"user","content":"hello"}]}`)

	h.handleMessageCountTokens(&ctx)

	if got := ctx.Response.StatusCode(); got != fasthttp.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body=%s", got, fasthttp.StatusBadRequest, ctx.Response.Body())
	}
	if got := gjson.GetBytes(ctx.Response.Body(), "error.message").String(); got != "缺少 model 字段" {
		t.Fatalf("error.message = %q, want %q", got, "缺少 model 字段")
	}
}
