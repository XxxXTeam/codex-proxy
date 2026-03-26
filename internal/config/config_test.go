package config

import "testing"

func TestValidateProxyURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		proxyURL string
		wantErr  bool
	}{
		{name: "empty", proxyURL: "", wantErr: false},
		{name: "http", proxyURL: "http://127.0.0.1:7890", wantErr: false},
		{name: "https", proxyURL: "https://user:pass@127.0.0.1:7890", wantErr: false},
		{name: "socks5", proxyURL: "socks5://127.0.0.1:1080", wantErr: false},
		{name: "socks5h", proxyURL: "socks5h://user:pass@127.0.0.1:1080", wantErr: false},
		{name: "bad scheme", proxyURL: "socks4://127.0.0.1:1080", wantErr: true},
		{name: "missing host", proxyURL: "http://", wantErr: true},
		{name: "socks missing port", proxyURL: "socks5://127.0.0.1", wantErr: true},
		{name: "query not allowed", proxyURL: "http://127.0.0.1:7890?x=1", wantErr: true},
		{name: "path not allowed", proxyURL: "http://127.0.0.1:7890/proxy", wantErr: true},
		{name: "trailing slash allowed after sanitize", proxyURL: "http://127.0.0.1:7890/", wantErr: false},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := &Config{ProxyURL: tt.proxyURL}
			cfg.Sanitize()
			err := cfg.Validate()
			if tt.wantErr && err == nil {
				t.Fatalf("expected error for %q", tt.proxyURL)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error for %q: %v", tt.proxyURL, err)
			}
		})
	}
}
