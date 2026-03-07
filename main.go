/**
 * Codex Proxy 独立服务入口
 * 提供 OpenAI 兼容的 API 接口，将请求转发至 Codex (OpenAI Responses API)
 * 支持多账号轮询、Token 自动刷新、思考配置（连字符格式）
 */
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"codex-proxy/internal/auth"
	"codex-proxy/internal/config"
	"codex-proxy/internal/executor"
	"codex-proxy/internal/handler"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
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
	flag.Parse()

	/* 加载配置 */
	cfg, err := config.LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}

	log.Infof("%s⚡ Codex Proxy 启动中...%s", colorCyan, colorReset)
	log.Infof("监听地址: %s%s%s", colorGreen, cfg.Listen, colorReset)
	log.Infof("账号目录: %s", cfg.AuthDir)
	log.Infof("API 基础 URL: %s", cfg.BaseURL)
	log.Infof("刷新间隔: %d 秒", cfg.RefreshInterval)
	log.Infof("最大重试: %d 次", cfg.MaxRetry)
	if cfg.HealthCheckInterval > 0 {
		log.Infof("健康检查: 每 %d 秒, 并发 %d, 连续失败 %d 次禁用",
			cfg.HealthCheckInterval, cfg.HealthCheckConcurrency, cfg.HealthCheckMaxFailures)
	}

	/* 初始化账号管理器 */
	selector := auth.NewRoundRobinSelector()
	manager := auth.NewManager(cfg.AuthDir, cfg.ProxyURL, cfg.RefreshInterval, selector)

	if err = manager.LoadAccounts(); err != nil {
		log.Fatalf("加载账号失败: %v", err)
	}
	manager.SetRefreshConcurrency(cfg.RefreshConcurrency)

	/* 启动后台任务 */
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	/* 启动异步磁盘写入工作器（将 Token 写盘从刷新 goroutine 解耦） */
	manager.StartSaveWorker(ctx)

	/* 启动后台 Token 刷新 */
	go manager.StartRefreshLoop(ctx)

	/* 启动健康检查（如果配置了检查间隔） */
	if cfg.HealthCheckInterval > 0 {
		healthChecker := auth.NewHealthChecker(
			cfg.BaseURL, cfg.ProxyURL,
			cfg.HealthCheckInterval,
			cfg.HealthCheckMaxFailures,
			cfg.HealthCheckConcurrency,
		)
		go healthChecker.StartLoop(ctx, manager)
	}

	/* 初始化执行器 */
	exec := executor.NewExecutor(cfg.BaseURL, cfg.ProxyURL)

	/* 启动连接池保活（防止长时间无请求后首次请求耗时过长） */
	exec.StartKeepAlive(ctx)

	/* 初始化 HTTP 服务 */
	if cfg.LogLevel != "debug" {
		gin.SetMode(gin.ReleaseMode)
	}
	r := gin.New()
	r.Use(gin.Recovery())
	r.Use(ginLogger())

	/* 注册路由 */
	proxyHandler := handler.NewProxyHandler(manager, exec, cfg.APIKeys, cfg.MaxRetry, cfg.ProxyURL)
	proxyHandler.RegisterRoutes(r)

	/* 使用 http.Server 以支持优雅关闭 */
	srv := &http.Server{
		Addr:    cfg.Listen,
		Handler: r,
	}

	/* 在 goroutine 中启动 HTTP 服务 */
	go func() {
		log.Infof("%s⚡ Codex Proxy 已启动%s，共 %s%d%s 个账号，监听 %s%s%s",
			colorCyan, colorReset,
			colorGreen, manager.AccountCount(), colorReset,
			colorGreen, cfg.Listen, colorReset)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP 服务启动失败: %v", err)
		}
	}()

	/* 等待关闭信号 */
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Infof("%s收到关闭信号，正在停止...%s", colorYellow, colorReset)

	/* 优雅关闭 HTTP 服务器（最多等 5 秒） */
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Errorf("HTTP 服务关闭异常: %v", err)
	}

	/* 停止后台任务 */
	cancel()
	manager.Stop()

	log.Infof("%s✅ Codex Proxy 已停止%s", colorGreen, colorReset)
}

/**
 * ginLogger 自定义 Gin 日志中间件（彩色输出）
 * @returns gin.HandlerFunc - Gin 中间件函数
 */
func ginLogger() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()

		status := c.Writer.Status()
		latency := time.Since(start)
		method := c.Request.Method
		path := c.Request.URL.Path
		client := c.ClientIP()

		/* 状态码着色 */
		statusColor := colorGreen
		switch {
		case status >= 500:
			statusColor = colorRed
		case status >= 400:
			statusColor = colorYellow
		case status >= 300:
			statusColor = colorCyan
		}

		/* 方法着色 */
		methodColor := colorBlue
		switch method {
		case "POST":
			methodColor = colorCyan
		case "DELETE":
			methodColor = colorRed
		case "PUT", "PATCH":
			methodColor = colorYellow
		}

		log.Infof("%s%s%s %s%d%s %s%s%s %s%v%s %s",
			methodColor, method, colorReset,
			statusColor, status, colorReset,
			colorWhite, path, colorReset,
			colorGray, latency.Round(time.Millisecond), colorReset,
			fmt.Sprintf("%s%s%s", colorGray, client, colorReset),
		)
	}
}
