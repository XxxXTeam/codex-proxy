package executor

import (
	"testing"

	"codex-proxy/internal/auth"
)

func TestRecordCompactUsage(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{
			name: "top-level usage",
			body: `{"object":"response.compaction","usage":{"input_tokens":12,"output_tokens":8,"total_tokens":20,"input_tokens_details":{"cached_tokens":5,"cache_write_tokens":4},"output_tokens_details":{"reasoning_tokens":3}}}`,
		},
		{
			name: "wrapped response usage",
			body: `{"response":{"usage":{"input_tokens":12,"output_tokens":8,"total_tokens":20,"input_tokens_details":{"cached_tokens":5,"cache_write_tokens":4},"output_tokens_details":{"reasoning_tokens":3}}}}`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			account := &auth.Account{}
			recordCompactUsage(account, "gpt-5.6-sol-openai-compact", []byte(test.body), false)

			usage := account.GetStats().Usage
			if usage.TotalCompletions != 1 || usage.InputTokens != 12 || usage.OutputTokens != 8 || usage.TotalTokens != 20 {
				t.Fatalf("usage = %#v", usage)
			}
			if usage.CacheReadTokens != 5 || usage.CacheWriteTokens != 4 || usage.ReasoningTokens != 3 {
				t.Fatalf("detailed usage = %#v", usage)
			}
		})
	}
}
