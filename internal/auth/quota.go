/**
 * 账号额度查询模块
 * 通过 wham/usage API 获取每个账号的剩余额度信息
 * 支持并发查询和结果缓存
 */
package auth

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"codex-proxy/internal/netutil"

	log "github.com/sirupsen/logrus"
)

/**
 * QuotaChecker 账号额度查询器
 * @field httpClient - 共享的 HTTP 客户端
 * @field concurrency - 并发查询数
 * @field proxyURL - 代理地址
 */
type QuotaChecker struct {
	httpClient  *http.Client
	concurrency int
	usageURL    string
}

/**
 * QuotaCheckResult 额度查询的汇总结果
 * @field Total - 总查询数
 * @field Valid - 有效账号数
 * @field Invalid - 无效账号数（已删除）
 * @field Failed - 查询失败数
 * @field Duration - 查询耗时
 */
type QuotaCheckResult struct {
	Total    int    `json:"total"`
	Valid    int    `json:"valid"`
	Invalid  int    `json:"invalid"`
	Failed   int    `json:"failed"`
	Duration string `json:"duration"`
}

/**
 * NewQuotaChecker 创建新的额度查询器
 * @param proxyURL - 代理地址
 * @param concurrency - 并发查询数
 * @returns *QuotaChecker - 额度查询器实例
 */
func NewQuotaChecker(baseURL, proxyURL string, concurrency int, enableHTTP2 bool, backendDomain, resolveAddress string) *QuotaChecker {
	if concurrency <= 0 {
		concurrency = 50
	}
	if backendDomain == "" {
		backendDomain = "chatgpt.com"
	}
	usageURL := "https://" + backendDomain + "/backend-api/wham/usage"
	if baseURL != "" {
		if u, err := url.Parse(baseURL); err == nil && u.Host != "" {
			usageURL = u.Scheme + "://" + u.Host + "/backend-api/wham/usage"
		}
	}

	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 60 * time.Second}
	dialCtx := netutil.BuildUpstreamDialContext(dialer, proxyURL, backendDomain, resolveAddress)
	log.Debugf("quota checker dial config backend_domain=%s resolve_address=%s usage_url=%s", backendDomain, netutil.NormalizeResolveAddress(resolveAddress), usageURL)

	transport := netutil.NewUpstreamTransport(netutil.UpstreamTransportConfig{
		DialContext:           dialCtx,
		ProxyURL:              proxyURL,
		MaxIdleConns:          concurrency * 2,
		MaxIdleConnsPerHost:   concurrency * 2,
		MaxConnsPerHost:       concurrency * 2,
		EnableHTTP2:           enableHTTP2,
		ResponseHeaderTimeout: 15 * time.Second,
		IdleConnTimeout:       120 * time.Second,
		DisableCompression:    true,
	})

	return &QuotaChecker{
		httpClient: &http.Client{
			Transport: transport,
			Timeout:   20 * time.Second,
		},
		concurrency: concurrency,
		usageURL:    strings.TrimSpace(usageURL),
	}
}

/**
 * CheckAllStream 并发查询所有账号的剩余额度（SSE 流式返回进度）
 * 每查询完一个账号就通过 channel 发送一个 ProgressEvent
 * 无效账号（API 返回非 200）从号池中删除
 * @param ctx - 上下文
 * @param manager - 账号管理器
 * @returns <-chan ProgressEvent - 进度事件 channel
 */
func (qc *QuotaChecker) CheckAllStream(ctx context.Context, manager *Manager) <-chan ProgressEvent {
	ch := make(chan ProgressEvent, 100)

	go func() {
		defer close(ch)

		accounts := manager.GetAccounts()
		total := len(accounts)
		if total == 0 {
			ch <- ProgressEvent{Type: "done", Message: "无账号", Duration: "0s"}
			return
		}

		start := time.Now()
		log.Infof("开始查询 %d 个账号的剩余额度（并发 %d）", total, qc.concurrency)

		sem := make(chan struct{}, qc.concurrency)
		var wg sync.WaitGroup
		var validCount, invalidCount, failCount, currentIdx atomic.Int64

		for _, acc := range accounts {
			if ctx.Err() != nil {
				break
			}

			wg.Add(1)
			sem <- struct{}{}

			go func(a *Account) {
				defer wg.Done()
				defer func() { <-sem }()

				v, st := qc.checkAccount(ctx, a)
				email := a.GetEmail()

				cur := int(currentIdx.Add(1))
				var ok bool
				switch v {
				case 1:
					validCount.Add(1)
					ok = true
				case 0:
					failCount.Add(1)
				default:
					manager.ApplyQuotaUsageHTTPOutcome(ctx, qc, a, st, v)
					if manager.AccountInPool(a) {
						failCount.Add(1)
					} else {
						invalidCount.Add(1)
					}
				}

				ch <- ProgressEvent{
					Type:    "item",
					Email:   email,
					Success: &ok,
					Current: cur,
					Total:   total,
				}
			}(acc)
		}

		wg.Wait()

		vc := validCount.Load()
		ic := invalidCount.Load()
		fc := failCount.Load()
		elapsed := time.Since(start).Round(time.Millisecond)
		log.Infof("额度查询完成: 有效 %d, 无效 %d, 失败 %d, 耗时 %v",
			vc, ic, fc, elapsed)

		ch <- ProgressEvent{
			Type:         "done",
			Message:      "额度查询完成",
			Total:        total,
			SuccessCount: int(vc),
			FailedCount:  int(ic + fc),
			Remaining:    manager.AccountCount(),
			Duration:     elapsed.String(),
		}
	}()

	return ch
}

/**
 * CheckOne 查询单个账号的额度（公开方法）
 * @param ctx - 上下文
 * @param acc - 账号
 */
func (qc *QuotaChecker) CheckOne(ctx context.Context, acc *Account) {
	_, _ = qc.checkAccount(ctx, acc)
}

/**
 * CheckAccountResult 查询额度并返回结果码：1=有效，-1=无效 4xx，0=失败/5xx 暂态，2=HTTP 429
 */
func (qc *QuotaChecker) CheckAccountResult(ctx context.Context, acc *Account) int {
	v, _ := qc.checkAccount(ctx, acc)
	return v
}

/**
 * CheckAccountResultWithStatus 同 CheckAccountResult，并返回上游 HTTP 状态码（网络失败时为 0）
 */
func (qc *QuotaChecker) CheckAccountResultWithStatus(ctx context.Context, acc *Account) (verdict int, httpStatus int) {
	return qc.checkAccount(ctx, acc)
}

/**
 * checkAccount 查询单个账号的额度
 * @returns verdict: 1=有效, -1=无效 4xx（非 429）, 0=网络或 5xx 暂态, 2=HTTP 429
 * @returns httpStatus 响应状态码，错误时为 0
 */
func (qc *QuotaChecker) checkAccount(ctx context.Context, acc *Account) (verdict int, httpStatus int) {
	acc.mu.RLock()
	accessToken := acc.Token.AccessToken
	accountID := acc.Token.AccountID
	email := acc.Token.Email
	acc.mu.RUnlock()

	if accessToken == "" {
		return -1, 0
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, qc.usageURL, nil)
	if err != nil {
		return 0, 0
	}

	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "codex_cli_rs/0.101.0 (Mac OS 26.0.1; arm64) Apple_Terminal/464")
	req.Header.Set("Origin", "https://chatgpt.com")
	req.Header.Set("Referer", "https://chatgpt.com/")
	if accountID != "" {
		req.Header.Set("Chatgpt-Account-Id", accountID)
	}

	resp, err := qc.httpClient.Do(req)
	if err != nil {
		log.Debugf("账号 [%s] 额度查询网络错误: %v", email, err)
		return 0, 0
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	now := time.Now()
	info := &QuotaInfo{
		StatusCode: resp.StatusCode,
		CheckedAt:  now,
	}

	if resp.StatusCode == 200 {
		info.Valid = true
		/* 存储原始 JSON 响应 */
		if json.Valid(body) {
			info.RawData = body
		}
		acc.mu.Lock()
		acc.QuotaInfo = info
		acc.QuotaCheckedAt = now
		acc.mu.Unlock()

		log.Debugf("账号 [%s] 额度查询成功", email)
		return 1, 200
	}

	/* 非 200 */
	info.Valid = false
	if json.Valid(body) {
		info.RawData = body
	} else if len(body) > 0 {
		/* 非 JSON 响应截断后用 json.Marshal 安全转义 */
		truncated := string(body)
		if len(truncated) > 200 {
			truncated = truncated[:200]
		}
		if escaped, err := json.Marshal(truncated); err == nil {
			info.RawData = escaped
		}
	}

	acc.mu.Lock()
	acc.QuotaInfo = info
	acc.QuotaCheckedAt = now
	acc.mu.Unlock()

	log.Warnf("账号 [%s] 额度查询异常 [%d]", email, resp.StatusCode)
	st := resp.StatusCode
	switch {
	case st == 429:
		return 2, st
	case st >= 500:
		return 0, st
	default:
		return -1, st
	}
}
