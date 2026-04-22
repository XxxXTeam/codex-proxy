/**
 * Codex Proxy 独立服务入口
 * 提供 OpenAI 兼容的 API 接口，将请求转发至 Codex (OpenAI Responses API)
 * 支持多账号轮询、Token 自动刷新、思考配置（连字符格式）
 */
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"codex-proxy/internal/auth"
	"codex-proxy/internal/config"
	codexdb "codex-proxy/internal/db"
	"codex-proxy/internal/executor"
	"codex-proxy/internal/handler"
	"codex-proxy/internal/static"

	"github.com/fasthttp/router"
	log "github.com/sirupsen/logrus"
	"github.com/valyala/fasthttp"
)

/* ANSI 颜色代码 */
const (
	colorReset  = "\033[0m"
	colorRed    = "\033[31m"
	colorGreen  = "\033[32m"
	colorYellow = "\033[33m"
	colorBlue   = "\033[34m"
	colorCyan   = "\033[36m"
	colorGray   = "\033[90m"
	colorWhite  = "\033[97m"
)

func main() {
	/* 配置 logrus 彩色日志格式 */
	log.SetFormatter(&log.TextFormatter{
		ForceColors:     true,
		FullTimestamp:   true,
		TimestampFormat: "15:04:05",
	})

	configPath := flag.String("config", "config.yaml", "配置文件路径")
	toJSON := flag.Bool("tojson", false, "将数据库账号导出为 JSON 文件到 auth-dir 目录（需配置数据库连接）")
	flag.Parse()

	/* 加载配置 */
	cfg, err := config.LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}

	/* 处理子命令 */
	args := flag.Args()
	if len(args) > 0 {
		switch args[0] {
		case "login":
			loginFlags := flag.NewFlagSet("login", flag.ExitOnError)
			device := loginFlags.Bool("device", false, "使用设备代码流登录")
			_ = loginFlags.Parse(args[1:])

			cfg, err := config.LoadConfig(*configPath)
			if err != nil {
				log.Fatalf("加载配置失败: %v", err)
			}
			if !cfg.EnableCodexLogin {
				log.Fatal("Codex 登录已在配置中禁用")
			}

			var loginErr error
			if *device {
				loginErr = handler.HandleCodexDeviceLogin(cfg.AuthDir, cfg.OAuthNoBrowser, nil)
			} else {
				loginErr = handler.HandleCodexLogin(cfg.AuthDir, cfg.OAuthCallbackPort, cfg.OAuthNoBrowser, nil)
			}
			if loginErr != nil {
				log.Fatalf("登录失败: %v", loginErr)
			}
			log.Info("登录成功")
			return
		}
	}

	/* 处理 --tojson 导出功能 */
	if *toJSON {
		if err := exportAccountsToJSON(cfg); err != nil {
			log.Fatalf("导出账号失败: %v", err)
		}
		return
	}

	log.Infof("%s⚡ Codex Proxy 启动中...%s", colorCyan, colorReset)
	log.Infof("监听地址: %s%s%s", colorGreen, cfg.Listen, colorReset)
	if cfg.DBEnabled {
		switch cfg.DBDriver {
		case "mysql":
			log.Infof("%s持久化: MySQL（Token 直写数据库）%s — %s:%d/%s",
				colorCyan, colorReset, cfg.DBHost, cfg.DBPort, strings.TrimSpace(cfg.DBName))
		case "sqlite":
			dsn := strings.TrimSpace(cfg.DBDSN)
			if dsn == "" {
				dsn = strings.TrimSpace(cfg.DBName)
				if dsn == "" {
					dsn = "codex_proxy.db"
				}
			}
			log.Infof("%s持久化: SQLite（Token 直写数据库）%s — %s", colorCyan, colorReset, dsn)
		default:
			log.Infof("%s持久化: PostgreSQL（Token 直写数据库）%s — %s:%d/%s sslmode=%s",
				colorCyan, colorReset, cfg.DBHost, cfg.DBPort, strings.TrimSpace(cfg.DBName), cfg.DBSSLMode)
		}
		if strings.TrimSpace(cfg.AuthDir) != "" {
			log.Infof("账号目录（JSON 可导入 DB）: %s", cfg.AuthDir)
		} else {
			log.Infof("账号目录: （未配置，仅从数据库加载）")
		}
	} else {
		log.Infof("账号目录: %s", cfg.AuthDir)
	}
	log.Infof("API 基础 URL: %s", cfg.BaseURL)
	if cfg.ProxyURL != "" {
		log.Infof("代理地址: %s%s%s", colorGreen, cfg.ProxyURL, colorReset)
	}
	log.Infof("刷新间隔: %d 秒", cfg.RefreshInterval)
	log.Infof("最大重试: %d 次", cfg.MaxRetry)
	if cfg.UpstreamPoolAutoScale {
		log.Infof("出站连接池(自适应): max-conns-per-host=%d max-idle-per-host=%d max-idle=%d tcp_nodelay=on",
			cfg.MaxConnsPerHost, cfg.MaxIdleConnsPerHost, cfg.MaxIdleConns)
	} else {
		log.Infof("出站连接池(手动): max-conns-per-host=%d max-idle-per-host=%d max-idle=%d tcp_nodelay=on",
			cfg.MaxConnsPerHost, cfg.MaxIdleConnsPerHost, cfg.MaxIdleConns)
	}
	if cfg.ListenConcurrency > 0 {
		log.Infof("入站 fasthttp 最大并发连接: %d", cfg.ListenConcurrency)
	}
	if cfg.UpstreamResponseHeaderTimeoutSec > 0 {
		log.Infof("出站响应头超时: %d 秒", cfg.UpstreamResponseHeaderTimeoutSec)
	}
	if cfg.HealthCheckInterval > 0 {
		log.Infof("健康检查: 每 %d 秒, 并发 %d, 连续失败 %d 次禁用",
			cfg.HealthCheckInterval, cfg.HealthCheckConcurrency, cfg.HealthCheckMaxFailures)
	}
	if cfg.DisabledRecoveryIntervalSec > 0 && !cfg.DBEnabled {
		log.Infof("禁用凭据恢复: 每 %d 秒将 *.json.disabled 还原并探测 OAuth/额度，失败则删文件",
			cfg.DisabledRecoveryIntervalSec)
	}

	/* 数据库连接（可选）- 异步初始化以提升启动速度 */
	var db *sql.DB
	var dbDialect codexdb.Dialect
	var dbInitDone = make(chan struct{})
	if cfg.DBEnabled {
		go func() {
			defer close(dbInitDone)
			var err error
			db, dbDialect, err = codexdb.Open(cfg)
			if err != nil {
				log.Fatalf("数据库无法就绪: %v", err)
			}
			log.Infof("已连接数据库 (%s)", dbDialect.String())

			if err = codexdb.SetupSchema(db, dbDialect); err != nil {
				log.Fatalf("数据库初始化失败: %v", err)
			}
		}()
	} else {
		close(dbInitDone)
	}

	/* 等待数据库初始化完成（如果启用） */
	<-dbInitDone

	/* 初始化账号管理器 */
	var selector auth.Selector
	if cfg.Selector == "quota-first" {
		selector = auth.NewQuotaFirstSelector()
	} else {
		selector = auth.NewRoundRobinSelector()
	}
	managerOpts := &auth.ManagerOptions{
		AuthScanInterval:              cfg.AuthScanInterval,
		SaveWorkers:                   cfg.SaveWorkers,
		Cooldown401Sec:                cfg.Cooldown401Sec,
		Cooldown429Sec:                cfg.Cooldown429Sec,
		RefreshSingleTimeoutSec:       cfg.RefreshSingleTimeoutSec,
		RefreshBatchSize:              cfg.RefreshBatchSize,
		RefreshHTTP429Action:          cfg.RefreshHTTP429Action,
		QuotaHTTP429Action:            cfg.QuotaHTTP429Action,
		QuotaHTTPStatusActions:        cfg.QuotaHTTPStatusActions,
		RefreshHTTPStatusPolicy:       cfg.RefreshHTTPStatusPolicy,
		QuotaHTTPStatusPolicy:         cfg.QuotaHTTPStatusPolicy,
		Auth401SyncRefreshConcurrency: cfg.Auth401SyncRefreshConcurrency,
		DBDialect:                     dbDialect,
		QuotaPrecheck:                 cfg.QuotaPrecheck,
		DisableAuth401Remove:          cfg.DisableAuth401Remove,
	}
	manager := auth.NewManager(cfg.AuthDir, db, cfg.ProxyURL, cfg.RefreshInterval, selector, cfg.EnableHTTP2, managerOpts)
	manager.SetRefreshConcurrency(cfg.RefreshConcurrency)
	quotaChecker := auth.NewQuotaChecker(cfg.BaseURL, cfg.ProxyURL, cfg.QuotaCheckConcurrency, cfg.EnableHTTP2, cfg.BackendDomain, cfg.BackendResolveAddress, time.Duration(cfg.QuotaCheckCacheTTLSec)*time.Second)
	manager.SetPostRefreshQuotaChecker(quotaChecker)

	/* 启动后台任务 */
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if cfg.StartupAsyncLoad {
		log.Infof("启动即服务可用: 已启用后台账号加载模式")
		go func() {
			start := time.Now()
			batch := cfg.StartupLoadBatchSize
			if batch < 1 {
				batch = 8000
			}
			for {
				if ctx.Err() != nil {
					return
				}
				loadErr := manager.LoadAccountsProgressive(ctx, batch)
				if loadErr != nil {
					if errors.Is(loadErr, context.Canceled) || errors.Is(loadErr, context.DeadlineExceeded) {
						return
					}
					retrySec := cfg.StartupLoadRetryInterval
					if retrySec < 1 {
						retrySec = 10
					}
					n := manager.AccountCount()
					if n > 0 {
						log.Warnf("启动分批加载报错: %v（号池当前 %d 个，选号直接读号池；%d 秒后重试本流程）", loadErr, n, retrySec)
					} else {
						log.Warnf("后台加载账号失败: %v，%d 秒后重试", loadErr, retrySec)
					}
					select {
					case <-ctx.Done():
						return
					case <-time.After(time.Duration(retrySec) * time.Second):
					}
					continue
				}
				log.Infof("后台加载账号完成: 共 %d 个，耗时 %v", manager.AccountCount(), time.Since(start).Round(time.Millisecond))
				return
			}
		}()
	} else {
		loadStart := time.Now()
		if err = manager.LoadAccounts(); err != nil {
			log.Fatalf("加载账号失败: %v", err)
		}
		log.Infof("账号加载完成: 共 %d 个，耗时 %v", manager.AccountCount(), time.Since(loadStart).Round(time.Millisecond))
	}

	/* 启动异步磁盘写入工作器（将 Token 写盘从刷新 goroutine 解耦） */
	manager.StartSaveWorker(ctx)

	/* 启动后台 Token 刷新 */
	go manager.StartRefreshLoop(ctx)

	/* 延迟启动健康检查（在服务启动后异步进行，避免影响启动速度） */
	if cfg.HealthCheckInterval > 0 {
		go func() {
			// 等待服务完全启动
			time.Sleep(2 * time.Second)
			healthChecker := auth.NewHealthChecker(
				cfg.BaseURL, cfg.ProxyURL,
				cfg.HealthCheckInterval,
				cfg.HealthCheckMaxFailures,
				cfg.HealthCheckConcurrency,
				cfg.HealthCheckStartDelay,
				cfg.HealthCheckBatchSize,
				cfg.HealthCheckReqTimeout,
				cfg.EnableHTTP2,
				cfg.BackendDomain,
				cfg.BackendResolveAddress,
			)
			healthChecker.StartLoop(ctx, manager)
		}()
	}

	if cfg.DisabledRecoveryIntervalSec > 0 && !cfg.DBEnabled {
		go func() {
			iv := time.Duration(cfg.DisabledRecoveryIntervalSec) * time.Second
			select {
			case <-ctx.Done():
				return
			case <-time.After(90 * time.Second):
			}
			for {
				if ctx.Err() != nil {
					return
				}
				manager.RunDisabledCredentialRecovery(ctx, quotaChecker)
				select {
				case <-ctx.Done():
					return
				case <-time.After(iv):
				}
			}
		}()
	}

	/* 初始化执行器 */
	exec := executor.NewExecutor(cfg.BaseURL, cfg.ProxyURL, executor.HTTPPoolConfig{
		MaxConnsPerHost:          cfg.MaxConnsPerHost,
		MaxIdleConns:             cfg.MaxIdleConns,
		MaxIdleConnsPerHost:      cfg.MaxIdleConnsPerHost,
		EnableHTTP2:              cfg.EnableHTTP2,
		BackendDomain:            cfg.BackendDomain,
		ResolveAddress:           cfg.BackendResolveAddress,
		KeepaliveIntervalSec:     cfg.KeepaliveInterval,
		IdleConnTimeoutSec:       cfg.UpstreamIdleConnTimeoutSec,
		TLSHandshakeTimeoutSec:   cfg.UpstreamTLSHandshakeTimeoutSec,
		HTTP2MaxConnsPerHostCap:  cfg.HTTP2MaxConnsPerHostCap,
		ResponseHeaderTimeoutSec: cfg.UpstreamResponseHeaderTimeoutSec,
	})

	/* 延迟启动连接池保活（在服务启动后异步进行） */
	go func() {
		// 短暂延迟，让HTTP服务先启动
		time.Sleep(100 * time.Millisecond)
		exec.StartKeepAlive(ctx)
	}()

	/* 初始化 HTTP 服务 */
	r := router.New()
	proxyHandler := handler.NewProxyHandler(manager, exec, cfg.APIKeys, cfg.MaxRetry, cfg.EnableHealthyRetry, cfg.ProxyURL, cfg.BaseURL, cfg.EnableHTTP2, cfg.BackendDomain, cfg.BackendResolveAddress, cfg.QuotaCheckConcurrency, cfg.QuotaCheckCacheTTLSec, quotaChecker, cfg.QuotaPrecheck, cfg.EmptyRetryMax, cfg.DebugUpstreamStream, cfg.EnableModelSuffixFast, cfg.EnableModelSuffix1M, cfg.EnableModelSuffixImage, cfg.EnableWebSocket, cfg.DebugWSStream, cfg.Enable429ConcurrentRetry, cfg.ConcurrentRetry429TimeoutSec, static.IndexHTML)
	proxyHandler.RegisterRoutes(r)
	handler.SetupLoginRoutes(r, cfg.AuthDir, cfg.OAuthCallbackPort, cfg.OAuthNoBrowser, cfg.EnableCodexLogin, manager)

	appHandler := r.Handler
	appHandler = handler.OptionsBypass(appHandler)
	appHandler = handler.CORSAllowOrigin(appHandler)
	appHandler = handler.GzipIfAccepted(appHandler)
	appHandler = fasthttpLogger(appHandler)

	/* Read/WriteTimeout=0：长 SSE 对话不在服务端掐写回；IdleTimeout 用 listen-idle-timeout-sec，勿与 shutdown-timeout 混用 */
	srv := &fasthttp.Server{
		Handler:          appHandler,
		Name:             "Codex Proxy",
		DisableKeepalive: false,
		Concurrency:      cfg.ListenConcurrency,
		IdleTimeout:      time.Duration(cfg.ListenIdleTimeoutSec) * time.Second,
		ReadTimeout:      0,
		WriteTimeout:     0,
		/* fasthttp 无单独「读头」超时；此处按配置限制单次请求的读（含 body），与 listen-read-header-timeout-sec 语义接近 */
		HeaderReceived: func(_ *fasthttp.RequestHeader) fasthttp.RequestConfig {
			return fasthttp.RequestConfig{
				ReadTimeout: time.Duration(cfg.ListenReadHeaderTimeoutSec) * time.Second,
			}
		},
		TCPKeepalive:       cfg.ListenTCPKeepaliveSec > 0,
		TCPKeepalivePeriod: time.Duration(cfg.ListenTCPKeepaliveSec) * time.Second,
		ReadBufferSize:     cfg.ListenMaxHeaderBytes,
		MaxConnsPerIP:      0,
		MaxRequestsPerConn: 0,
	}

	/* 在 goroutine 中启动 HTTP 服务 */
	go func() {
		log.Infof("%s⚡ Codex Proxy 已启动%s，共 %s%d%s 个账号，监听 %s%s%s",
			colorCyan, colorReset,
			colorGreen, manager.AccountCount(), colorReset,
			colorGreen, cfg.Listen, colorReset)
		if err := srv.ListenAndServe(cfg.Listen); err != nil {
			log.Fatalf("HTTP 服务启动失败: %v", err)
		}
	}()

	/* 等待关闭信号 */
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Infof("%s收到关闭信号，正在停止...%s", colorYellow, colorReset)

	/* 优雅关闭 HTTP 服务器 */
	shutdownSec := cfg.ShutdownTimeout
	if shutdownSec < 1 {
		shutdownSec = 5
	}
	if err := srv.Shutdown(); err != nil {
		log.Errorf("HTTP 服务关闭异常: %v", err)
	}

	/* 停止后台任务 */
	cancel()
	manager.Stop()

	log.Infof("%s✅ Codex Proxy 已停止%s", colorGreen, colorReset)
}

/**
 * fasthttpLogger 自定义 FastHTTP 日志中间件（彩色输出）
 */
func fasthttpLogger(next fasthttp.RequestHandler) fasthttp.RequestHandler {
	return func(ctx *fasthttp.RequestCtx) {
		start := time.Now()
		next(ctx)

		status := ctx.Response.StatusCode()
		latency := time.Since(start)
		method := string(ctx.Method())
		path := string(ctx.Path())
		client := ctx.RemoteAddr().String()

		statusColor := colorGreen
		switch {
		case status >= 500:
			statusColor = colorRed
		case status >= 400:
			statusColor = colorYellow
		case status >= 300:
			statusColor = colorCyan
		}

		methodColor := colorBlue
		switch method {
		case "POST":
			methodColor = colorCyan
		case "DELETE":
			methodColor = colorRed
		case "PUT", "PATCH":
			methodColor = colorYellow
		}

		if status >= 400 {
			log.Warnf("%s%s%s %s%d%s %s%s%s %s%v%s %s",
				methodColor, method, colorReset,
				statusColor, status, colorReset,
				colorWhite, path, colorReset,
				colorGray, latency.Round(time.Millisecond), colorReset,
				fmt.Sprintf("%s%s%s", colorGray, client, colorReset),
			)
		} else {
			log.Debugf("%s%s%s %s%d%s %s%s%s %s%v%s %s",
				methodColor, method, colorReset,
				statusColor, status, colorReset,
				colorWhite, path, colorReset,
				colorGray, latency.Round(time.Millisecond), colorReset,
				fmt.Sprintf("%s%s%s", colorGray, client, colorReset),
			)
		}
	}
}

/**
 * exportAccountsToJSON 将数据库中的账号导出为 JSON 文件到 auth-dir 目录
 * @param cfg - 配置对象
 * @return error - 导出出错则返回错误
 */
func exportAccountsToJSON(cfg *config.Config) error {
	authDir := strings.TrimSpace(cfg.AuthDir)
	if authDir == "" {
		return fmt.Errorf("auth-dir 未配置，无法导出账号")
	}

	/* 创建 auth-dir 目录（如果不存在） */
	if err := os.MkdirAll(authDir, 0755); err != nil {
		return fmt.Errorf("创建 auth-dir 目录失败: %v", err)
	}

	log.Infof("%s📤 开始导出账号到 JSON...%s", colorCyan, colorReset)

	/* 连接数据库 */
	db, dialect, err := codexdb.Open(cfg)
	if err != nil {
		return fmt.Errorf("数据库连接失败: %v", err)
	}
	defer db.Close()

	/* 从数据库读取所有账号 */
	accounts, err := loadAllAccountsFromDB(db, dialect)
	if err != nil {
		return fmt.Errorf("从数据库读取账号失败: %v", err)
	}

	if len(accounts) == 0 {
		log.Warnf("数据库中没有账号数据")
		return nil
	}

	/* 遍历账号，转换成 JSON 并写入文件 */
	successCount := 0
	failCount := 0
	for _, acc := range accounts {
		/* 生成文件名：优先用邮箱，其次用 account_id，都没有则用 filepath 中提取的名称 */
		var filename string
		email := strings.TrimSpace(acc.Token.Email)
		accountID := strings.TrimSpace(acc.Token.AccountID)

		if email != "" {
			filename = email + ".json"
		} else if accountID != "" {
			filename = accountID + ".json"
		} else if acc.FilePath != "" {
			filename = acc.FilePath
		} else {
			log.Warnf("账号信息不完整（无邮箱、ID 或路径），跳过")
			failCount++
			continue
		}

		/* 转换成 TokenFile 格式供导出 */
		tokenFile := auth.TokenFile{
			IDToken:      acc.Token.IDToken,
			AccessToken:  acc.Token.AccessToken,
			RefreshToken: acc.Token.RefreshToken,
			AccountID:    acc.Token.AccountID,
			Email:        acc.Token.Email,
			Type:         "codex",
			Expire:       acc.Token.Expire,
		}
		tokenFile.LastRefresh = acc.LastRefreshedAt.Format(time.RFC3339)

		/* 序列化为 JSON */
		jsonData, err := json.MarshalIndent(tokenFile, "", "  ")
		if err != nil {
			log.Warnf("序列化账号 %s 失败: %v", email, err)
			failCount++
			continue
		}

		/* 写入文件 */
		filepath := authDir + "/" + filename
		if err := os.WriteFile(filepath, jsonData, 0600); err != nil {
			log.Warnf("写入文件 %s 失败: %v", filepath, err)
			failCount++
			continue
		}

		log.Infof("已导出: %s (%s)", filename, email)
		successCount++
	}

	log.Infof("%s✅ 导出完成: 成功 %d 个，失败 %d 个%s", colorGreen, successCount, failCount, colorReset)
	return nil
}

/**
 * loadAllAccountsFromDB 从数据库读取所有账号
 * 这是一个轻量级版本，仅用于导出，不初始化选择器等复杂逻辑
 *
 * @param db - 数据库连接
 * @param dialect - 数据库方言
 * @return []*auth.Account - 账号列表
 * @return error - 读取出错
 */
func loadAllAccountsFromDB(db *sql.DB, dialect codexdb.Dialect) ([]*auth.Account, error) {
	/* 使用 account_id 和 email 作为唯一标识，而非 file_path（数据库中无此列） */
	rows, err := db.Query(`
		SELECT id, account_id, email, id_token, access_token, refresh_token, 
		       expire, last_refresh, status, cooldown_until, disable_reason, last_used_at
		FROM codex_accounts
		ORDER BY id DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("数据库查询失败: %v", err)
	}
	defer rows.Close()

	var accounts []*auth.Account
	for rows.Next() {
		var (
			id                                                                          int64
			accountID, email, idToken, accessToken, refreshToken, expire, disableReason sql.NullString
			lastRefresh, cooldownUntil, lastUsedAt                                      sql.NullTime
			status                                                                      sql.NullInt32
		)

		if err := rows.Scan(
			&id, &accountID, &email, &idToken, &accessToken, &refreshToken,
			&expire, &lastRefresh, &status, &cooldownUntil, &disableReason, &lastUsedAt,
		); err != nil {
			log.Warnf("扫描数据库行失败: %v", err)
			continue
		}

		/* 构建 Account 对象 */
		acc := &auth.Account{
			FilePath: "", /* 数据库中无此列，导出时用邮箱或 account_id 作文件名 */
			Token: auth.TokenData{
				IDToken:      idToken.String,
				AccessToken:  accessToken.String,
				RefreshToken: refreshToken.String,
				AccountID:    accountID.String,
				Email:        email.String,
				Expire:       expire.String,
			},
		}

		if lastRefresh.Valid {
			acc.LastRefreshedAt = lastRefresh.Time
		}
		if status.Valid {
			acc.Status = auth.AccountStatus(status.Int32)
		}
		if cooldownUntil.Valid {
			acc.CooldownUntil = cooldownUntil.Time
		}
		if lastUsedAt.Valid {
			acc.LastUsedAt = lastUsedAt.Time
		}
		if disableReason.Valid {
			acc.DisableReason = disableReason.String
		}

		accounts = append(accounts, acc)
	}

	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("行迭代错误: %v", err)
	}

	return accounts, nil
}
