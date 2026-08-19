package config

import "testing"

func TestValidateCodexFingerprintOffOrEmpty(t *testing.T) {
	cases := []string{"", "off", "OFF", "  off  "}
	for _, mode := range cases {
		if err := validateCodexFingerprint(mode, ""); err != nil {
			t.Errorf("mode=%q 不应报错: %v", mode, err)
		}
	}
}

func TestValidateCodexFingerprintInvalidMode(t *testing.T) {
	if err := validateCodexFingerprint("random", "00000000-0000-4000-8000-000000000000"); err == nil {
		t.Error("非法 mode 应报错")
	}
}

func TestValidateCodexFingerprintMissingSeed(t *testing.T) {
	for _, mode := range []string{"device", "session", "full"} {
		if err := validateCodexFingerprint(mode, ""); err == nil {
			t.Errorf("mode=%q 缺 seed 应报错", mode)
		}
	}
}

func TestValidateCodexFingerprintInvalidSeed(t *testing.T) {
	if err := validateCodexFingerprint("session", "not-a-uuid"); err == nil {
		t.Error("非法 seed 应报错")
	}
}

func TestValidateCodexFingerprintValid(t *testing.T) {
	for _, mode := range []string{"device", "session", "full"} {
		if err := validateCodexFingerprint(mode, "00000000-0000-4000-8000-000000000000"); err != nil {
			t.Errorf("mode=%q 合法 seed 不应报错: %v", mode, err)
		}
	}
}

func TestValidatePromptCache(t *testing.T) {
	if err := validatePromptCache("stable-v1", []PromptCacheReplacement{{Pattern: `run=\d+`, Replace: "run=<stable>"}}); err != nil {
		t.Fatalf("合法缓存配置不应报错: %v", err)
	}
	if err := validatePromptCache("bad\nlabel", nil); err == nil {
		t.Fatal("缓存标签换行应报错")
	}
	if err := validatePromptCache("stable-v1", []PromptCacheReplacement{{Pattern: "[", Replace: ""}}); err == nil {
		t.Fatal("非法替换正则应报错")
	}
}
