package codex

import "net/http"

const (
	ClientVersion = "0.144.0"
	UserAgent     = "codex-tui/" + ClientVersion + " (Mac OS 26.5.1; arm64) iTerm.app/3.6.11 (codex-tui; " + ClientVersion + ")"
	Origin        = "https://chatgpt.com"
	Referer       = "https://chatgpt.com/"
	Originator    = "codex-tui"
)

func ApplyClientHeaders(header http.Header, accountID string) {
	header.Set("Version", ClientVersion)
	header.Set("User-Agent", UserAgent)
	header.Set("Origin", Origin)
	header.Set("Referer", Referer)
	header.Set("Originator", Originator)
	if accountID != "" {
		header.Set("Chatgpt-Account-Id", accountID)
	}
}
