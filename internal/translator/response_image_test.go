package translator

import (
	"context"
	"testing"

	"github.com/tidwall/gjson"
)

func TestMimeTypeFromCodexOutputFormat(t *testing.T) {
	tests := map[string]string{
		"":          "image/png",
		"png":       "image/png",
		"jpg":       "image/jpeg",
		"jpeg":      "image/jpeg",
		"webp":      "image/webp",
		"gif":       "image/gif",
		"image/png": "image/png",
	}
	for outputFormat, want := range tests {
		if got := mimeTypeFromCodexOutputFormat(outputFormat); got != want {
			t.Errorf("mimeTypeFromCodexOutputFormat(%q) = %q, want %q", outputFormat, got, want)
		}
	}
}

func TestConvertNonStreamResponseImageGeneration(t *testing.T) {
	raw := []byte(`{"type":"response.completed","response":{"id":"resp_image","model":"gpt-5.6-sol","created_at":1710000000,"status":"completed","output":[{"type":"image_generation_call","result":"aGVsbG8=","output_format":"png"}]}}`)

	got, hasOutput := ConvertNonStreamResponse(raw, nil)
	if !hasOutput {
		t.Fatal("ConvertNonStreamResponse() hasOutput = false, want true")
	}
	if imageURL := gjson.Get(got, "choices.0.message.images.0.image_url.url").String(); imageURL != "data:image/png;base64,aGVsbG8=" {
		t.Fatalf("image URL = %q", imageURL)
	}
	if got := gjson.Get(got, "choices.0.finish_reason").String(); got != "stop" {
		t.Fatalf("finish_reason = %q, want stop", got)
	}
}

func TestConvertStreamChunkImageGenerationDeduplicatesFinalImage(t *testing.T) {
	state := NewStreamState("gpt-5.6-sol")
	created := []byte(`data:{"type":"response.created","response":{"id":"resp_image","created_at":1710000000,"model":"gpt-5.6-sol"}}`)
	if events := ConvertStreamChunk(context.Background(), created, state, nil, false); len(events) != 0 {
		t.Fatalf("response.created events = %d, want 0", len(events))
	}

	partial := []byte(`data:{"type":"response.image_generation_call.partial_image","item_id":"img_1","partial_image_b64":"aGVsbG8=","output_format":"jpeg"}`)
	events := ConvertStreamChunk(context.Background(), partial, state, nil, false)
	if len(events) != 1 {
		t.Fatalf("partial image events = %d, want 1", len(events))
	}
	if imageURL := gjson.Get(events[0], "choices.0.delta.images.0.image_url.url").String(); imageURL != "data:image/jpeg;base64,aGVsbG8=" {
		t.Fatalf("partial image URL = %q", imageURL)
	}

	final := []byte(`data:{"type":"response.output_item.done","item":{"type":"image_generation_call","item_id":"img_1","result":"aGVsbG8="}}`)
	if events := ConvertStreamChunk(context.Background(), final, state, nil, false); len(events) != 0 {
		t.Fatalf("duplicate final image events = %d, want 0", len(events))
	}
	completed := []byte(`data:{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}`)
	if events := ConvertStreamChunk(context.Background(), completed, state, nil, false); len(events) != 1 {
		t.Fatalf("completed events = %d, want 1", len(events))
	}
	if !state.HasImage {
		t.Fatal("HasImage = false, want true")
	}
}
