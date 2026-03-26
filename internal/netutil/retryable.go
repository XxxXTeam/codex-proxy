package netutil

import (
	"context"
	"errors"
	"strings"
)

// IsRetryableUpstreamNetError 判断出站 RoundTrip 阶段错误是否适合换号/重试（连接被掐、GOAWAY、h2 等）。
func IsRetryableUpstreamNetError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	/* 整请求阶段的 deadline（不含客户端主动 Cancel）通常可换连接/账号再试 */
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}

	s := strings.ToLower(err.Error())
	switch {
	case strings.Contains(s, "goaway"):
		return true
	case strings.Contains(s, "enhance_your_calm"):
		return true
	case strings.Contains(s, "connection closed"):
		return true
	case strings.Contains(s, "connection reset"):
		return true
	case strings.Contains(s, "broken pipe"):
		return true
	case strings.Contains(s, "unexpected eof"):
		return true
	case strings.Contains(s, "server closed idle connection"):
		return true
	case strings.Contains(s, "use of closed network connection"):
		return true
	case strings.Contains(s, "transport connection broken"):
		return true
	case strings.Contains(s, "tls handshake timeout"):
		return true
	case strings.Contains(s, "i/o timeout"):
		return true
	case strings.Contains(s, "timeout"):
		return true
	}
	return false
}
