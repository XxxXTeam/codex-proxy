package thinking

import "testing"

func TestParseModelSuffixRecognizesSuffixesInEitherOrder(t *testing.T) {
	for _, model := range []string{
		"gpt-5.3-codex-fast-1m",
		"gpt-5.3-codex-1m-fast",
	} {
		got := ParseModelSuffix(model)
		if got.ModelName != "gpt-5.3-codex" {
			t.Fatalf("ParseModelSuffix(%q).ModelName = %q, want gpt-5.3-codex", model, got.ModelName)
		}
		if !got.IsFast || !got.Is1M {
			t.Fatalf("ParseModelSuffix(%q) = %#v, want fast and 1m", model, got)
		}
	}
}
