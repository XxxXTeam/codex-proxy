/**
 * 账号健康检查模块
 * 定时向 Codex API 发送轻量级请求检测账号状态
 * 自动识别 401（Token无效）、403（账号封禁）、429（配额耗尽）等异常
 * 支持并发检查和连续失败自动禁用
 */
package auth

import (
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

/**
 * HealthChecker 账号健康检查器
 * @field httpClient - 共享的 HTTP 客户端（连接池复用）
 * @field baseURL - Codex API 基础 URL
 * @field checkInterval - 检查间隔
 * @field maxConsecutiveFailures - 连续失败多少次后禁用账号
 * @field concurrency - 并发检查数量
 */
type HealthChecker struct {
	httpClient             *http.Client
	baseURL                string
	checkInterval          time.Duration
	maxConsecutiveFailures int
	concurrency            int
}

/**
 * NewHealthChecker 创建新的健康检查器
 * @param baseURL - Codex API 基础 URL
 * @param proxyURL - 代理地址
 * @param checkInterval - 检查间隔（秒）
 * @param maxFailures - 连续失败禁用阈值
 * @param concurrency - 并发检查数
 * @returns *HealthChecker - 健康检查器实例
 */
func NewHealthChecker(baseURL, proxyURL string, checkInterval int, maxFailures int, concurrency int) *HealthChecker {
	if baseURL == "" {
		baseURL = "https://chatgpt.com/backend-api/codex"
	}
	if checkInterval <= 0 {
		checkInterval = 300
	}
	if maxFailures <= 0 {
		maxFailures = 3
	}
	if concurrency <= 0 {
		concurrency = 5
	}

	transport := &http.Transport{
		MaxIdleConns:          concurrency * 2,
		MaxIdleConnsPerHost:   concurrency * 2,
		MaxConnsPerHost:       concurrency * 2,
		IdleConnTimeout:       120 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
		ForceAttemptHTTP2:     true,
		DisableCompression:    true,
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: false},
	}

	if proxyURL != "" {
		proxyParsed, err := url.Parse(proxyURL)
		if err == nil {
			transport.Proxy = http.ProxyURL(proxyParsed)
		}
	}

	return &HealthChecker{
		httpClient: &http.Client{
			Transport: transport,
			Timeout:   15 * time.Second,
		},
		baseURL:                strings.TrimSuffix(baseURL, "/"),
		checkInterval:          time.Duration(checkInterval) * time.Second,
		maxConsecutiveFailures: maxFailures,
		concurrency:            concurrency,
	}
}

/**
 * StartLoop 启动健康检查循环
 * @param ctx - 上下文
 * @param manager - 账号管理器
 */
func (hc *HealthChecker) StartLoop(ctx context.Context, manager *Manager) {
	ticker := time.NewTicker(hc.checkInterval)
	defer ticker.Stop()

	/* 启动后等待一个检查周期再开始（让刷新先完成） */
	log.Infof("健康检查已启动，间隔 %v，并发 %d，连续失败 %d 次禁用",
		hc.checkInterval, hc.concurrency, hc.maxConsecutiveFailures)

	for {
		select {
		case <-ctx.Done():
			log.Info("健康检查循环已停止")
			return
		case <-ticker.C:
			hc.checkAll(ctx, manager)
		}
	}
}

/**
 * checkAll 并发检查所有账号
 * @param ctx - 上下文
 * @param manager - 账号管理器
 */
func (hc *HealthChecker) checkAll(ctx context.Context, manager *Manager) {
	accounts := manager.GetAccounts()
	if len(accounts) == 0 {
		return
	}

	log.Debugf("开始健康检查，共 %d 个账号", len(accounts))

	/* 使用信号量控制并发 */
	sem := make(chan struct{}, hc.concurrency)
	var wg sync.WaitGroup

	for _, acc := range accounts {
		if ctx.Err() != nil {
			break
		}

		wg.Add(1)
		sem <- struct{}{}

		go func(account *Account) {
			defer wg.Done()
			defer func() { <-sem }()

			hc.checkAccount(ctx, manager, account)
		}(acc)
	}

	wg.Wait()

	remaining := manager.AccountCount()
	removed := len(accounts) - remaining
	if removed > 0 {
		log.Warnf("健康检查完成: 移除 %d 个异常账号，剩余 %d 个", removed, remaining)
	} else {
		log.Infof("健康检查完成: 全部正常，共 %d 个账号", remaining)
	}
}

/**
 * checkAccount 检查单个账号的健康状态
 * 发现异常直接从号池移除（而非禁用）
 * @param ctx - 上下文
 * @param manager - 账号管理器，用于移除异常账号
 * @param acc - 要检查的账号
 */
func (hc *HealthChecker) checkAccount(ctx context.Context, manager *Manager, acc *Account) {
	acc.mu.RLock()
	accessToken := acc.Token.AccessToken
	accountID := acc.Token.AccountID
	email := acc.Token.Email
	acc.mu.RUnlock()

	if accessToken == "" {
		manager.RemoveAccount(acc, "empty_access_token")
		return
	}

	/* 使用 responses 端点发送一个最小化的探测请求 */
	checkURL := hc.baseURL + "/responses"
	reqBody := `{"model":"gpt-5","input":[{"role":"user","content":"hi"}],"stream":false,"max_output_tokens":1}`

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, checkURL, strings.NewReader(reqBody))
	if err != nil {
		return
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	if accountID != "" {
		req.Header.Set("Chatgpt-Account-Id", accountID)
	}

	resp, err := hc.httpClient.Do(req)
	if err != nil {
		/* 网络错误不移除（可能是代理问题） */
		log.Debugf("健康检查网络错误 [%s]: %v", email, err)
		return
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	body, _ := io.ReadAll(resp.Body)

	switch {
	case resp.StatusCode == 401:
		/* Token 无效，连续失败达到阈值则移除 */
		failures := acc.RecordFailure()
		if failures >= hc.maxConsecutiveFailures {
			manager.RemoveAccount(acc, ReasonAuth401)
		} else {
			log.Debugf("账号 [%s] 健康检查 401 (第 %d/%d 次)", email, failures, hc.maxConsecutiveFailures)
		}

	case resp.StatusCode == 403:
		/* 403 通常是地域封锁 / Cloudflare 拦截，设置冷却（不删除） */
		acc.SetCooldown(5 * time.Minute)

	case resp.StatusCode == 400:

	case resp.StatusCode == 429:
		cooldown := parseHealthCheckRetryAfter(body)
		acc.SetQuotaCooldown(cooldown)
		log.Infof("账号 [%s] 健康检查 429（配额耗尽），冷却 %v", email, cooldown)

	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		/* 正常，重置失败计数 */
		acc.RecordSuccess()

	case resp.StatusCode >= 500:
		/* 服务端错误，不计入账号失败 */
		log.Debugf("账号 [%s] 健康检查服务端错误 %d", email, resp.StatusCode)

	default:
		/* 其他错误码，仅记录日志，不删除账号 */
		log.Warnf("账号 [%s] 健康检查异常状态码 %d", email, resp.StatusCode)
	}
}

/**
 * parseHealthCheckRetryAfter 从 429 响应中解析冷却时间
 * @param body - 响应体
 * @returns time.Duration - 冷却时间
 */
func parseHealthCheckRetryAfter(body []byte) time.Duration {
	if len(body) == 0 {
		return 60 * time.Second
	}
	if seconds := gjson.GetBytes(body, "error.resets_in_seconds").Int(); seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if resetsAt := gjson.GetBytes(body, "error.resets_at").Int(); resetsAt > 0 {
		resetTime := time.Unix(resetsAt, 0)
		if resetTime.After(time.Now()) {
			return time.Until(resetTime)
		}
	}
	return 60 * time.Second
}
