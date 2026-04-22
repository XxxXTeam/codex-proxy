package handler

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"codex-proxy/internal/auth"
	"codex-proxy/internal/auth/codex"
	"codex-proxy/internal/browser"

	"github.com/fasthttp/router"
	log "github.com/sirupsen/logrus"
	"github.com/valyala/fasthttp"
)

const (
	deviceUserCodeURL     = "https://auth.openai.com/api/accounts/deviceauth/usercode"
	deviceTokenURL        = "https://auth.openai.com/api/accounts/deviceauth/token"
	verificationURL       = "https://auth.openai.com/codex/device"
	deviceAuthRedirectURI = "https://auth.openai.com/deviceauth/callback"
)

// SetupLoginRoutes 注册 Codex 登录路由
func SetupLoginRoutes(r *router.Router, authDir string, callbackPort int, noBrowser bool, enableCodexLogin bool, manager *auth.Manager) {
	if !enableCodexLogin {
		return
	}
	r.POST("/auth/codex/login", func(ctx *fasthttp.RequestCtx) {
		if err := HandleCodexLogin(authDir, callbackPort, noBrowser, manager); err != nil {
			writeJSON(ctx, fasthttp.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(ctx, fasthttp.StatusOK, map[string]string{"status": "success", "message": "login successful"})
	})
	r.POST("/auth/codex/device-login", func(ctx *fasthttp.RequestCtx) {
		if err := HandleCodexDeviceLogin(authDir, noBrowser, manager); err != nil {
			writeJSON(ctx, fasthttp.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(ctx, fasthttp.StatusOK, map[string]string{"status": "success", "message": "device login successful"})
	})
}

// HandleCodexLogin 执行标准 OAuth 回调登录流程
func HandleCodexLogin(authDir string, callbackPort int, noBrowser bool, manager *auth.Manager) error {
	pkceCodes, err := codex.GeneratePKCECodes()
	if err != nil {
		return fmt.Errorf("生成 PKCE 码失败: %w", err)
	}

	state, err := generateRandomState()
	if err != nil {
		return fmt.Errorf("生成 state 失败: %w", err)
	}

	oauthServer := codex.NewOAuthServer(callbackPort)
	if err := oauthServer.Start(); err != nil {
		return fmt.Errorf("启动 OAuth 回调服务器失败: %w", err)
	}
	defer func() {
		_ = oauthServer.Stop(context.Background())
	}()

	redirectURI := codex.BuildRedirectURI(callbackPort)

	authClient := codex.NewCodexAuth()
	authURL, err := authClient.GenerateAuthURL(state, redirectURI, pkceCodes)
	if err != nil {
		return fmt.Errorf("生成授权 URL 失败: %w", err)
	}

	if noBrowser {
		fmt.Printf("请在浏览器中打开以下链接进行登录:\n%s\n", authURL)
	} else {
		if browser.IsAvailable() {
			if err := browser.OpenURL(authURL); err != nil {
				fmt.Printf("打开浏览器失败，请手动打开以下链接:\n%s\n", authURL)
			}
		} else {
			fmt.Printf("未检测到浏览器，请手动打开以下链接:\n%s\n", authURL)
		}
	}

	result, err := oauthServer.WaitForCallback(5 * time.Minute)
	if err != nil {
		return fmt.Errorf("等待 OAuth 回调超时: %w", err)
	}
	if result.Error != "" {
		return fmt.Errorf("OAuth 错误: %s", result.Error)
	}
	if result.State != state {
		return fmt.Errorf("state 验证失败")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	bundle, err := authClient.ExchangeCodeForTokens(ctx, result.Code, redirectURI, pkceCodes)
	if err != nil {
		return fmt.Errorf("交换令牌失败: %w", err)
	}

	if err := saveCodexAuthBundle(authDir, bundle, manager); err != nil {
		return fmt.Errorf("保存凭据失败: %w", err)
	}

	return nil
}

// HandleCodexDeviceLogin 执行设备代码流登录
func HandleCodexDeviceLogin(authDir string, noBrowser bool, manager *auth.Manager) error {
	deviceCodeResp, err := requestDeviceCode()
	if err != nil {
		return fmt.Errorf("请求设备码失败: %w", err)
	}

	fmt.Printf("设备码: %s\n", deviceCodeResp.UserCode)
	fmt.Printf("请在浏览器中打开 %s 并输入设备码\n", verificationURL)

	if !noBrowser && browser.IsAvailable() {
		if err := browser.OpenURL(verificationURL); err != nil {
			log.Debugf("打开验证页面失败: %v", err)
		}
	}

	authCode, codeVerifier, err := pollDeviceAuthorization(deviceCodeResp.DeviceAuthID, deviceCodeResp.UserCode)
	if err != nil {
		return fmt.Errorf("设备授权轮询失败: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	authClient := codex.NewCodexAuth()
	pkceCodes := &codex.PKCECodes{CodeVerifier: codeVerifier}
	bundle, err := authClient.ExchangeCodeForTokens(ctx, authCode, deviceAuthRedirectURI, pkceCodes)
	if err != nil {
		return fmt.Errorf("交换令牌失败: %w", err)
	}

	if err := saveCodexAuthBundle(authDir, bundle, manager); err != nil {
		return fmt.Errorf("保存凭据失败: %w", err)
	}

	return nil
}

func saveCodexAuthBundle(authDir string, bundle *codex.CodexAuthBundle, manager *auth.Manager) error {
	email := bundle.TokenData.Email
	accountID := bundle.TokenData.AccountID
	planType := ""

	if bundle.TokenData.IDToken != "" {
		claims, err := codex.ParseJWTToken(bundle.TokenData.IDToken)
		if err != nil {
			log.Warnf("解析 JWT 失败: %v", err)
		} else if claims != nil {
			if email == "" {
				email = claims.GetUserEmail()
			}
			if accountID == "" {
				accountID = claims.GetAccountID()
			}
			planType = claims.CodexAuthInfo.ChatgptPlanType
		}
	}

	authClient := codex.NewCodexAuth()
	storage := authClient.CreateTokenStorage(bundle)
	if email != "" {
		storage.Email = email
	}

	if authDir == "" {
		authDir = "./auths"
	}
	if err := os.MkdirAll(authDir, 0700); err != nil {
		return fmt.Errorf("创建目录失败: %w", err)
	}

	fileName := credentialFileName(email, planType, accountID)
	filePath := filepath.Join(authDir, fileName)

	if err := storage.SaveTokenToFile(filePath); err != nil {
		return fmt.Errorf("保存凭据文件失败: %w", err)
	}

	log.Infof("凭据已保存到 %s (%s)", filePath, email)

	if manager != nil {
		if err := manager.AddAccountFromFile(filePath); err != nil {
			log.Warnf("加载新账号到号池失败: %v", err)
		} else {
			log.Infof("新账号已加入号池: %s", email)
		}
	}

	return nil
}

func credentialFileName(email, planType, accountID string) string {
	email = strings.TrimSpace(email)
	planType = strings.TrimSpace(planType)
	accountID = strings.TrimSpace(accountID)

	base := email
	if base == "" {
		base = accountID
	}
	if base == "" {
		base = fmt.Sprintf("unknown-%d", time.Now().Unix())
	}

	if planType != "" {
		return fmt.Sprintf("codex-%s-%s.json", base, planType)
	}
	return fmt.Sprintf("codex-%s.json", base)
}

func generateRandomState() (string, error) {
	b := make([]byte, 16)
	_, err := rand.Read(b)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", b), nil
}

type deviceCodeResponse struct {
	DeviceAuthID string `json:"device_auth_id"`
	UserCode     string `json:"user_code"`
}

type deviceTokenResponse struct {
	AuthorizationCode string `json:"authorization_code"`
	CodeVerifier      string `json:"code_verifier"`
}

func requestDeviceCode() (*deviceCodeResponse, error) {
	body, _ := json.Marshal(map[string]string{"client_id": codex.ClientID})
	resp, err := http.Post(deviceUserCodeURL, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result deviceCodeResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return &result, nil
}

func pollDeviceAuthorization(deviceAuthID, userCode string) (string, string, error) {
	interval := 5 * time.Second
	timeout := 15 * time.Minute
	deadline := time.Now().Add(timeout)

	body := map[string]string{
		"device_auth_id": deviceAuthID,
		"user_code":      userCode,
	}
	jsonBody, _ := json.Marshal(body)

	for time.Now().Before(deadline) {
		time.Sleep(interval)

		resp, err := http.Post(deviceTokenURL, "application/json", bytes.NewReader(jsonBody))
		if err != nil {
			log.Debugf("设备授权轮询请求失败: %v", err)
			continue
		}

		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		switch resp.StatusCode {
		case http.StatusOK:
			var result deviceTokenResponse
			if err := json.Unmarshal(respBody, &result); err != nil {
				log.Debugf("解析设备授权响应失败: %v", err)
				continue
			}
			if result.AuthorizationCode != "" {
				return result.AuthorizationCode, result.CodeVerifier, nil
			}
			continue
		case http.StatusForbidden, http.StatusNotFound:
			continue
		default:
			log.Debugf("设备授权轮询收到状态码 %d: %s", resp.StatusCode, string(respBody))
			continue
		}
	}

	return "", "", fmt.Errorf("设备授权轮询超时（%v）", timeout)
}
