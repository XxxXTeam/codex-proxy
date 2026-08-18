package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"codex-proxy/internal/auth"
	"github.com/tidwall/gjson"
)

const testResponsesSSE = "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_test\",\"created_at\":1710000000,\"model\":\"gpt-5.6-sol\"}}\n\n" +
	"data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n" +
	"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_test\",\"object\":\"response\",\"created_at\":1710000000,\"model\":\"gpt-5.6-sol\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"hello\"}]}],\"usage\":{\"input_tokens\":3,\"output_tokens\":2,\"total_tokens\":5}}}\n\n" +
	"data: [DONE]\n\n"

func newStreamTransportTestExecutor(t *testing.T, handler http.Handler) (*Executor, *auth.Account, func()) {
	t.Helper()
	server := httptest.NewServer(handler)
	account := &auth.Account{
		Token: auth.TokenData{
			AccessToken: "test-access-token",
			AccountID:   "test-account-id",
		},
	}
	exec := NewExecutor(server.URL, "", HTTPPoolConfig{})
	return exec, account, server.Close
}

func streamTransportRetryConfig(account *auth.Account) RetryConfig {
	return RetryConfig{
		PickFn: func(string, map[string]bool) (*auth.Account, error) {
			return account, nil
		},
		MaxRetry:      0,
		EmptyRetryMax: 0,
	}
}

func TestExecuteNonStreamUsesUpstreamSSEAndReturnsChatJSON(t *testing.T) {
	exec, account, closeServer := newStreamTransportTestExecutor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Fatalf("path = %q, want /responses", r.URL.Path)
		}
		if got := r.Header.Get("Accept"); got != "text/event-stream" {
			t.Errorf("Accept = %q, want text/event-stream", got)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		if got := gjson.GetBytes(body, "stream").Bool(); !got {
			t.Errorf("upstream stream = false, want true; body=%s", body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(testResponsesSSE))
	}))
	defer closeServer()

	result, err := exec.ExecuteNonStream(
		context.Background(),
		streamTransportRetryConfig(account),
		[]byte(`{"model":"gpt-5.6-sol","messages":[{"role":"user","content":"hi"}],"stream":false}`),
		"gpt-5.6-sol",
	)
	if err != nil {
		t.Fatalf("ExecuteNonStream() error = %v", err)
	}
	if got := gjson.GetBytes(result, "object").String(); got != "chat.completion" {
		t.Errorf("object = %q, want chat.completion", got)
	}
	if got := gjson.GetBytes(result, "choices.0.message.content").String(); got != "hello" {
		t.Errorf("content = %q, want hello", got)
	}
	if got := account.GetStats().Usage.TotalTokens; got != 5 {
		t.Errorf("total tokens = %d, want 5", got)
	}
}

func TestExecuteResponsesNonStreamUsesUpstreamSSEAndReturnsResponseJSON(t *testing.T) {
	exec, account, closeServer := newStreamTransportTestExecutor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Fatalf("path = %q, want /responses", r.URL.Path)
		}
		if got := r.Header.Get("Accept"); got != "text/event-stream" {
			t.Errorf("Accept = %q, want text/event-stream", got)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		if got := gjson.GetBytes(body, "stream").Bool(); !got {
			t.Errorf("upstream stream = false, want true; body=%s", body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(testResponsesSSE))
	}))
	defer closeServer()

	result, err := exec.ExecuteResponsesNonStream(
		context.Background(),
		streamTransportRetryConfig(account),
		[]byte(`{"model":"gpt-5.6-sol","input":"hi","stream":false}`),
		"gpt-5.6-sol",
	)
	if err != nil {
		t.Fatalf("ExecuteResponsesNonStream() error = %v", err)
	}
	if got := gjson.GetBytes(result, "object").String(); got != "response" {
		t.Errorf("object = %q, want response", got)
	}
	if got := gjson.GetBytes(result, "output.0.content.0.text").String(); got != "hello" {
		t.Errorf("text = %q, want hello", got)
	}
	if got := account.GetStats().Usage.TotalTokens; got != 5 {
		t.Errorf("total tokens = %d, want 5", got)
	}
}
