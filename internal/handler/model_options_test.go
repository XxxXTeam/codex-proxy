package handler

import (
	"testing"

	"github.com/tidwall/gjson"
	"github.com/valyala/fasthttp"
)

func TestParseModelRequestOptionsRecognizesAllImageConflicts(t *testing.T) {
	got := parseModelRequestOptions("codex-auto-review-high-image-fast-1m")
	if got.entry == nil || got.entry.base != "codex-auto-review" {
		t.Fatalf("entry = %#v, want codex-auto-review", got.entry)
	}
	if !got.isImage || !got.isFast || !got.is1M || !got.hasThinking {
		t.Fatalf("options = %#v, want image, fast, 1m, thinking", got)
	}
}

func TestValidateModelRequestOptionsRejectsRestrictedModels(t *testing.T) {
	h := &ProxyHandler{enableModelFast: true, enableModel1M: true, enableModelImage: true}
	tests := []struct {
		name  string
		model string
		body  string
	}{
		{name: "auto review image", model: "codex-auto-review-image"},
		{name: "auto review fast", model: "codex-auto-review-fast"},
		{name: "auto review thinking", model: "codex-auto-review-high"},
		{name: "auto review unknown suffix", model: "codex-auto-review-custom"},
		{name: "image with thinking", model: "gpt-5.3-codex-high-image"},
		{name: "unknown image fast", model: "unlisted-fast-image"},
		{name: "image with reasoning body", model: "gpt-5.3-codex-image", body: `{"reasoning":{"effort":"high"}}`},
		{name: "compact with fast body", model: "gpt-5.6-sol-openai-compact", body: `{"service_tier":"priority"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := h.validateModelRequestOptions(test.model, []byte(test.body)); err == nil {
				t.Fatalf("validateModelRequestOptions(%q) returned nil", test.model)
			}
		})
	}
}

func TestHandleModelsIncludesRestrictedBaseModelsOnly(t *testing.T) {
	h := &ProxyHandler{enableModelFast: true, enableModel1M: true, enableModelImage: true}
	var ctx fasthttp.RequestCtx
	h.handleModels(&ctx)

	if got := gjson.GetBytes(ctx.Response.Body(), `data.#(id=="gpt-5.3-codex-spark").id`).String(); got != "gpt-5.3-codex-spark" {
		t.Fatalf("gpt-5.3-codex-spark is missing from model list")
	}
	if got := gjson.GetBytes(ctx.Response.Body(), `data.#(id=="codex-auto-review").id`).String(); got != "codex-auto-review" {
		t.Fatalf("codex-auto-review is missing from model list")
	}
	for _, id := range []string{
		"codex-auto-review-fast",
		"codex-auto-review-image",
		"codex-auto-review-high",
		"gpt-5.3-codex-high-image",
		"gpt-5.3-codex-fast-image",
	} {
		if gjson.GetBytes(ctx.Response.Body(), `data.#(id=="`+id+`")`).Exists() {
			t.Fatalf("model list contains invalid variant %q", id)
		}
	}
}

func TestExpandModelSubvariantIDsDoesNotCombineImage(t *testing.T) {
	for _, base := range []string{"gpt-5.3-codex", "gpt-5.3-codex-high"} {
		ids := expandModelSubvariantIDs(base, true, true, true, true, true, true)
		for _, id := range ids {
			if id == base+"-fast-image" || id == base+"-image-fast" || id == base+"-image-1m" {
				t.Fatalf("found invalid image combination %q", id)
			}
		}
	}
}
