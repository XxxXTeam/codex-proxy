/**
 * Token 刷新模块
 * 负责使用 refresh_token 向 OpenAI 认证端点获取新的 access_token
 * 支持带重试的刷新机制和指数退避策略
 */
package auth

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

/* OpenAI OAuth 配置常量 */
const (
	TokenURL = "https://auth.openai.com/oauth/token"
	ClientID = "app_EMoamEEZ73f0CkXaXp7hrann"
)

/**
 * Refresher 负责 Codex Token 的刷新操作
 * @field httpClient - 用于发送刷新请求的 HTTP 客户端
 */
type Refresher struct {
	httpClient *http.Client
}

/**
 * NewRefresher 创建新的 Token 刷新器
 * @param proxyURL - 可选的代理地址，为空则直连
 * @returns *Refresher - 刷新器实例
 */
func NewRefresher(proxyURL string) *Refresher {
	transport := &http.Transport{
		MaxIdleConns:          200,
		MaxIdleConnsPerHost:   200, /* auth.openai.com 单主机，需要大连接池 */
		MaxConnsPerHost:       200,
		IdleConnTimeout:       120 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 20 * time.Second,
		WriteBufferSize:       4 * 1024,
		ReadBufferSize:        8 * 1024,
		ForceAttemptHTTP2:     true,
		DisableCompression:    true,
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: false},
	}

	if proxyURL != "" {
		proxyParsed, err := url.Parse(proxyURL)
		if err == nil {
			transport.Proxy = http.ProxyURL(proxyParsed)
		} else {
			log.Warnf("代理地址解析失败: %v", err)
		}
	}

	return &Refresher{
		httpClient: &http.Client{
			Transport: transport,
			Timeout:   30 * time.Second,
		},
	}
}

/**
 * tokenResponse 是 OpenAI Token 端点的响应结构
 */
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
}

/**
 * RefreshToken 使用 refresh_token 获取新的 Token
 * @param ctx - 上下文，用于取消和超时控制
 * @param refreshToken - 当前的刷新令牌
 * @returns *TokenData - 新的 Token 数据
 * @returns error - 刷新失败时返回错误
 */
func (r *Refresher) RefreshToken(ctx context.Context, refreshToken string) (*TokenData, error) {
	if refreshToken == "" {
		return nil, fmt.Errorf("refresh_token 不能为空")
	}

	data := url.Values{
		"client_id":     {ClientID},
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"scope":         {"openid profile email"},
	}

	req, err := http.NewRequestWithContext(ctx, "POST", TokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, fmt.Errorf("创建刷新请求失败: %w", err)
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := r.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("刷新请求发送失败: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取刷新响应失败: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("刷新失败，状态码 %d: %s", resp.StatusCode, string(body))
	}

	var tokenResp tokenResponse
	if err = json.Unmarshal(body, &tokenResp); err != nil {
		return nil, fmt.Errorf("解析刷新响应失败: %w", err)
	}

	/* 从 ID Token 中提取账号信息 */
	accountID, email, planType := parseIDTokenClaims(tokenResp.IDToken)

	return &TokenData{
		IDToken:      tokenResp.IDToken,
		AccessToken:  tokenResp.AccessToken,
		RefreshToken: tokenResp.RefreshToken,
		AccountID:    accountID,
		Email:        email,
		Expire:       time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second).Format(time.RFC3339),
		PlanType:     planType,
	}, nil
}

/**
 * RefreshTokenWithRetry 带重试机制的 Token 刷新
 * 使用指数退避策略，遇到 refresh_token_reused 错误时立即停止
 * @param ctx - 上下文
 * @param refreshToken - 刷新令牌
 * @param maxRetries - 最大重试次数
 * @returns *TokenData - 新的 Token 数据
 * @returns error - 所有重试均失败时返回错误
 */
func (r *Refresher) RefreshTokenWithRetry(ctx context.Context, refreshToken string, maxRetries int) (*TokenData, error) {
	var lastErr error

	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt) * time.Second):
			}
		}

		tokenData, err := r.RefreshToken(ctx, refreshToken)
		if err == nil {
			return tokenData, nil
		}

		/* refresh_token_reused 错误不可重试 */
		if isNonRetryableErr(err) {
			log.Warnf("Token 刷新失败（不可重试）: %v", err)
			return nil, err
		}

		lastErr = err
		log.Warnf("Token 刷新第 %d 次尝试失败: %v", attempt+1, err)
	}

	return nil, fmt.Errorf("Token 刷新在 %d 次尝试后失败: %w", maxRetries, lastErr)
}

/**
 * isNonRetryableErr 判断错误是否不可重试
 * @param err - 错误对象
 * @returns bool - 如果是不可重试的错误返回 true
 */
func isNonRetryableErr(err error) bool {
	if err == nil {
		return false
	}
	raw := strings.ToLower(err.Error())
	return strings.Contains(raw, "refresh_token_reused")
}

/**
 * parseIDTokenClaims 解析 JWT ID Token 中的 AccountID、Email 和 PlanType
 * 使用 base64 解码 JWT payload，不验证签名
 * @param idToken - JWT ID Token 字符串
 * @returns string - AccountID
 * @returns string - Email
 * @returns string - PlanType (free/plus/pro)
 */
func parseIDTokenClaims(idToken string) (string, string, string) {
	if idToken == "" {
		return "", "", ""
	}

	parts := strings.Split(idToken, ".")
	if len(parts) < 2 {
		return "", "", ""
	}

	/* 使用 base64 RawURLEncoding 解码 JWT payload */
	decoded, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", "", ""
	}

	var claims struct {
		Sub   string `json:"sub"`
		Email string `json:"email"`
		Auth  struct {
			ChatGPTAccountID string `json:"chatgpt_account_id"`
			ChatGPTUserID    string `json:"chatgpt_user_id"`
			ChatGPTPlanType  string `json:"chatgpt_plan_type"`
			OrganizationID   string `json:"organization_id"`
			Organizations    []struct {
				ID string `json:"id"`
			} `json:"organizations"`
		} `json:"https://api.openai.com/auth"`
	}

	/* 宽松解析：即使部分字段类型不匹配也不放弃 */
	_ = json.Unmarshal(decoded, &claims)
	accountID := claims.Auth.ChatGPTAccountID
	if accountID == "" && claims.Auth.OrganizationID != "" {
		accountID = claims.Auth.OrganizationID
	}
	if accountID == "" && len(claims.Auth.Organizations) > 0 {
		accountID = claims.Auth.Organizations[0].ID
	}

	return accountID, claims.Email, claims.Auth.ChatGPTPlanType
}
