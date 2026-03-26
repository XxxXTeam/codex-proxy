/**
 * 配置加载模块
 * 负责解析 YAML 配置文件，定义 Codex 代理服务所需的全部配置结构
 */
package config

import (
	"fmt"
	"net/url"
	"os"
	"strings"

	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
)

/**
 * Config 是 Codex 代理服务的顶层配置结构
 * @field Listen - 监听地址，格式为 host:port
 * @field AuthDir - 账号文件目录路径
 * @field ProxyURL - 全局 HTTP/SOCKS 代理地址
 * @field BaseURL - Codex API 基础 URL
 * @field LogLevel - 日志级别 (debug/info/warn/error)
 * @field RefreshInterval - Token 自动刷新间隔（秒）
 * @field Accounts - 账号文件列表（可选，不指定则自动扫描 AuthDir）
 * @field APIKeys - 可选的 API 访问密钥，用于保护代理服务
 */
type Config struct {
	Listen     string `yaml:"listen"`
	AuthDir    string `yaml:"auth-dir"`
	DBEnabled  bool   `yaml:"db-enabled"`
	DBDriver   string `yaml:"db-driver"`
	DBHost     string `yaml:"db-host"`
	DBPort     int    `yaml:"db-port"`
	DBUser     string `yaml:"db-user"`
	DBPassword string `yaml:"db-password"`
	DBName     string `yaml:"db-name"`
	DBSSLMode  string `yaml:"db-sslmode"`
	DBDSN      string `yaml:"db-dsn"`
	/* DBMaxOpenConns / DBMaxIdleConns 为 0 时按 refresh-concurrency 自动估算 */
	DBMaxOpenConns         int    `yaml:"db-max-open-conns"`
	DBMaxIdleConns         int    `yaml:"db-max-idle-conns"`
	DBConnMaxLifetimeSec   int    `yaml:"db-conn-max-lifetime-sec"` /* 0 默认 1800（30m） */
	ProxyURL               string `yaml:"proxy-url"`
	BackendDomain          string `yaml:"backend-domain"`
	BackendResolveAddress  string `yaml:"backend-resolve-address"`
	BaseURL                string `yaml:"base-url"`
	LogLevel               string `yaml:"log-level"`
	RefreshInterval        int    `yaml:"refresh-interval"`
	MaxRetry               int    `yaml:"max-retry"`
	EnableHealthyRetry     bool   `yaml:"enable-healthy-retry"`
	HealthCheckInterval    int    `yaml:"health-check-interval"`
	HealthCheckMaxFailures int    `yaml:"health-check-max-failures"`
	HealthCheckConcurrency int    `yaml:"health-check-concurrency"`
	HealthCheckStartDelay  int    `yaml:"health-check-start-delay"`
	HealthCheckBatchSize   int    `yaml:"health-check-batch-size"`
	/* HealthCheckReqTimeout 定时健康检查单次请求超时（秒），与对话转发无关 */
	HealthCheckReqTimeout int `yaml:"health-check-request-timeout"`
	/* DisabledRecoveryIntervalSec 仅磁盘凭据：周期性将 *.json.disabled 还原为 .json，OAuth+额度探测，失败则删文件；0 关闭 */
	DisabledRecoveryIntervalSec int  `yaml:"disabled-recovery-interval-sec"`
	RefreshConcurrency          int  `yaml:"refresh-concurrency"`
	MaxConnsPerHost             int  `yaml:"max-conns-per-host"`
	MaxIdleConns                int  `yaml:"max-idle-conns"`
	MaxIdleConnsPerHost         int  `yaml:"max-idle-conns-per-host"`
	EnableHTTP2                 bool `yaml:"enable-http2"`
	StartupAsyncLoad            bool `yaml:"startup-async-load"`
	StartupLoadRetryInterval    int  `yaml:"startup-load-retry-interval"`
	/* StartupLoadBatchSize startup-async-load：磁盘 JSON 每批解析文件数，或 db-enabled 时每批从库读取行数；0 表示内置默认（8000） */
	StartupLoadBatchSize int `yaml:"startup-load-batch-size"`
	ShutdownTimeout      int `yaml:"shutdown-timeout"`
	AuthScanInterval     int `yaml:"auth-scan-interval"`
	SaveWorkers          int `yaml:"save-workers"`
	Cooldown401Sec       int `yaml:"cooldown-401-sec"`
	Cooldown429Sec       int `yaml:"cooldown-429-sec"`
	/* RefreshSingleTimeoutSec 后台 Token 刷新 / 401 恢复等单次 OAuth 请求超时（秒），与对话 SSE 无关 */
	RefreshSingleTimeoutSec int `yaml:"refresh-single-timeout-sec"`
	/* Auth401SyncRefreshConcurrency 请求路径 401→同步 OAuth 的全局并发；0 不限制；>0 时槽满直接换号，减轻 OAuth 429 */
	Auth401SyncRefreshConcurrency int `yaml:"auth-401-sync-refresh-concurrency"`
	/* RefreshHTTP429Action 刷新 token 遇 HTTP 429：cooldown | remove | disable（默认 cooldown） */
	RefreshHTTP429Action string `yaml:"refresh-http-429-action"`
	/* QuotaHTTP429Action 额度 wham/usage 遇 HTTP 429：cooldown | remove | disable（默认 cooldown） */
	QuotaHTTP429Action string `yaml:"quota-http-429-action"`
	/* QuotaHTTPStatusActions 旧式：等价于 quota-http-status-policy 中 phase=none */
	QuotaHTTPStatusActions map[string]string `yaml:"quota-http-status-actions"`
	/* RefreshHTTPStatusPolicy 刷新 token 按 HTTP 状态：phase=none|refresh_once|cooldown_then_retry，final=remove|disable|cooldown */
	RefreshHTTPStatusPolicy map[string]map[string]string `yaml:"refresh-http-status-policy"`
	QuotaHTTPStatusPolicy   map[string]map[string]string `yaml:"quota-http-status-policy"`
	QuotaCheckConcurrency   int                          `yaml:"quota-check-concurrency"`
	KeepaliveInterval       int                          `yaml:"keepalive-interval"`
	/* EmptyRetryMax 非流式空结果时的换号次数；亦用于 responses 在读到 GOAWAY/断连等时的读阶段换号重试（至少 2 轮） */
	EmptyRetryMax    int      `yaml:"empty-retry-max"`
	Selector         string   `yaml:"selector"`
	RefreshBatchSize int      `yaml:"refresh-batch-size"`
	Accounts         []string `yaml:"accounts"`
	APIKeys          []string `yaml:"api-keys"`

	/* 入站 HTTP/2 (h2c) 等 */
	EnableListenH2C            bool `yaml:"enable-listen-h2c"`
	ListenReadHeaderTimeoutSec int  `yaml:"listen-read-header-timeout-sec"`
	ListenIdleTimeoutSec       int  `yaml:"listen-idle-timeout-sec"`
	ListenTCPKeepaliveSec      int  `yaml:"listen-tcp-keepalive-sec"`
	ListenMaxHeaderBytes       int  `yaml:"listen-max-header-bytes"`
	H2MaxConcurrentStreams     int  `yaml:"h2-max-concurrent-streams"`
}

/**
 * LoadConfig 从指定路径加载 YAML 配置文件
 * @param path - 配置文件路径
 * @returns *Config - 解析后的配置对象
 * @returns error - 加载或解析失败时返回错误
 */
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置文件失败: %w", err)
	}

	cfg := &Config{
		Listen:                      ":8080",
		AuthDir:                     "./auths",
		DBEnabled:                   false,
		DBDriver:                    "postgres",
		DBHost:                      "127.0.0.1",
		DBPort:                      5432,
		DBUser:                      "",
		DBPassword:                  "",
		DBName:                      "codex_proxy",
		DBSSLMode:                   "disable",
		DBDSN:                       "",
		BackendDomain:               "",
		BaseURL:                     "",
		LogLevel:                    "info",
		RefreshInterval:             3000,
		MaxRetry:                    2,
		EnableHealthyRetry:          true,
		HealthCheckInterval:         300,
		HealthCheckMaxFailures:      3,
		HealthCheckConcurrency:      5,
		HealthCheckStartDelay:       45,
		HealthCheckBatchSize:        20,
		HealthCheckReqTimeout:       8,
		DisabledRecoveryIntervalSec: 0,
		RefreshConcurrency:          50,
		MaxConnsPerHost:             12, /* 配合 HTTP/2 降低 GOAWAY ENHANCE_YOUR_CALM 概率 */
		MaxIdleConns:                48,
		MaxIdleConnsPerHost:         8,
		EnableHTTP2:                 false, /* 默认 HTTP/1.1，多连接复用更稳；需 h2 可显式开启 */
		StartupAsyncLoad:            true,
		StartupLoadRetryInterval:    10,
		ShutdownTimeout:             5,
		AuthScanInterval:            30,
		SaveWorkers:                 4,
		Cooldown401Sec:              30,
		Cooldown429Sec:              60,
		RefreshSingleTimeoutSec:     30,
		QuotaCheckConcurrency:       0, /* 0 表示使用 refresh-concurrency */
		KeepaliveInterval:           60,
		EmptyRetryMax:               2,
		Selector:                    "round-robin",
		RefreshBatchSize:            0,
		EnableListenH2C:             true,
		ListenReadHeaderTimeoutSec:  60,
		ListenIdleTimeoutSec:        180,
		ListenTCPKeepaliveSec:       30,
		ListenMaxHeaderBytes:        1 << 20,
		H2MaxConcurrentStreams:      1000,
	}

	if err = yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("解析配置文件失败: %w", err)
	}

	cfg.Sanitize()
	if err = cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

/**
 * Sanitize 清理和规范化配置值
 * 去除多余空白、设置默认值等
 */
func (c *Config) Sanitize() {
	c.Listen = strings.TrimSpace(c.Listen)
	c.AuthDir = strings.TrimSpace(c.AuthDir)
	c.ProxyURL = strings.TrimSpace(c.ProxyURL)
	c.BackendDomain = strings.TrimSpace(c.BackendDomain)
	c.BackendResolveAddress = strings.TrimSpace(c.BackendResolveAddress)
	c.BaseURL = strings.TrimSpace(c.BaseURL)
	c.LogLevel = strings.TrimSpace(strings.ToLower(c.LogLevel))

	if c.Listen == "" {
		c.Listen = ":8080"
	}
	if c.AuthDir == "" && !c.DBEnabled {
		c.AuthDir = "./auths"
	}
	if c.DBDriver == "" {
		c.DBDriver = "postgres"
	}
	c.DBDriver = normalizeDBDriver(c.DBDriver)
	if c.DBPort == 0 {
		switch c.DBDriver {
		case "mysql":
			c.DBPort = 3306
		case "sqlite":
			/* 无 TCP 端口 */
		default:
			c.DBPort = 5432
		}
	}
	if c.DBSSLMode == "" {
		c.DBSSLMode = "disable"
	}
	if c.DBMaxOpenConns < 0 {
		c.DBMaxOpenConns = 0
	}
	if c.DBMaxOpenConns > 512 {
		c.DBMaxOpenConns = 512
	}
	if c.DBMaxIdleConns < 0 {
		c.DBMaxIdleConns = 0
	}
	if c.DBMaxIdleConns > 512 {
		c.DBMaxIdleConns = 512
	}
	if c.DBConnMaxLifetimeSec < 0 {
		c.DBConnMaxLifetimeSec = 0
	}
	if c.DBConnMaxLifetimeSec > 7200 {
		c.DBConnMaxLifetimeSec = 7200
	}
	if c.ProxyURL != "" {
		if u, err := url.Parse(c.ProxyURL); err == nil {
			if u.Path == "/" && u.RawQuery == "" && u.Fragment == "" {
				u.Path = ""
				c.ProxyURL = u.String()
			}
		}
	}
	/* 优先级：base-url（若配置） > backend-domain（自动拼接） */
	if c.BaseURL != "" {
		if !strings.HasPrefix(strings.ToLower(c.BaseURL), "http://") && !strings.HasPrefix(strings.ToLower(c.BaseURL), "https://") {
			c.BaseURL = "https://" + c.BaseURL
		}
		if u, err := url.Parse(c.BaseURL); err == nil && u.Hostname() != "" {
			/* 与请求主机保持一致，避免解析地址映射目标与 URL 主机不一致 */
			c.BackendDomain = u.Hostname()
		}
	} else {
		if c.BackendDomain == "" {
			c.BackendDomain = "chatgpt.com"
		}
		c.BaseURL = "https://" + c.BackendDomain + "/backend-api/codex"
	}
	if c.RefreshInterval <= 0 {
		c.RefreshInterval = 3000
	}
	if c.MaxRetry < 0 {
		c.MaxRetry = 0
	}
	if c.HealthCheckInterval < 0 {
		c.HealthCheckInterval = 0
	}
	if c.HealthCheckMaxFailures <= 0 {
		c.HealthCheckMaxFailures = 3
	}
	if c.HealthCheckConcurrency <= 0 {
		c.HealthCheckConcurrency = 5
	}
	if c.HealthCheckConcurrency > 128 {
		c.HealthCheckConcurrency = 128
	}
	if c.HealthCheckStartDelay < 0 {
		c.HealthCheckStartDelay = 0
	}
	if c.HealthCheckBatchSize < 0 {
		c.HealthCheckBatchSize = 0
	}
	if c.HealthCheckBatchSize > 0 && c.HealthCheckConcurrency > c.HealthCheckBatchSize {
		c.HealthCheckConcurrency = c.HealthCheckBatchSize
	}
	if c.HealthCheckReqTimeout <= 0 {
		c.HealthCheckReqTimeout = 8
	}
	if c.DisabledRecoveryIntervalSec < 0 {
		c.DisabledRecoveryIntervalSec = 0
	}
	if c.RefreshConcurrency <= 0 {
		c.RefreshConcurrency = 50
	}
	if c.MaxConnsPerHost < 0 {
		c.MaxConnsPerHost = 0
	}
	if c.MaxIdleConns < 0 {
		c.MaxIdleConns = 0
	}
	if c.MaxIdleConnsPerHost < 0 {
		c.MaxIdleConnsPerHost = 0
	}
	if c.StartupLoadRetryInterval <= 0 {
		c.StartupLoadRetryInterval = 10
	}
	if c.StartupLoadBatchSize < 0 {
		c.StartupLoadBatchSize = 0
	}
	if c.ShutdownTimeout < 1 {
		c.ShutdownTimeout = 5
	}
	if c.ShutdownTimeout > 60 {
		c.ShutdownTimeout = 60
	}
	if c.AuthScanInterval <= 0 {
		c.AuthScanInterval = 30
	}
	if c.SaveWorkers < 1 {
		c.SaveWorkers = 4
	}
	if c.SaveWorkers > 32 {
		c.SaveWorkers = 32
	}
	if c.Cooldown401Sec <= 0 {
		c.Cooldown401Sec = 30
	}
	if c.Cooldown429Sec <= 0 {
		c.Cooldown429Sec = 60
	}
	if c.RefreshSingleTimeoutSec <= 0 {
		c.RefreshSingleTimeoutSec = 30
	}
	if c.Auth401SyncRefreshConcurrency < 0 {
		c.Auth401SyncRefreshConcurrency = 0
	}
	if c.QuotaCheckConcurrency < 0 {
		c.QuotaCheckConcurrency = 0
	}
	if c.QuotaCheckConcurrency == 0 {
		c.QuotaCheckConcurrency = c.RefreshConcurrency
	}
	if c.KeepaliveInterval <= 0 {
		c.KeepaliveInterval = 60
	}
	if c.EmptyRetryMax < 0 {
		c.EmptyRetryMax = 0
	}
	if c.RefreshBatchSize < 0 {
		c.RefreshBatchSize = 0
	}
	c.Selector = strings.TrimSpace(strings.ToLower(c.Selector))
	if c.Selector != "quota-first" {
		c.Selector = "round-robin"
	}
	c.RefreshHTTP429Action = strings.TrimSpace(c.RefreshHTTP429Action)
	c.QuotaHTTP429Action = strings.TrimSpace(c.QuotaHTTP429Action)
	if len(c.QuotaHTTPStatusActions) > 0 {
		norm := make(map[string]string, len(c.QuotaHTTPStatusActions))
		for k, v := range c.QuotaHTTPStatusActions {
			kk := strings.TrimSpace(k)
			if kk == "" {
				continue
			}
			norm[kk] = strings.TrimSpace(v)
		}
		c.QuotaHTTPStatusActions = norm
	}
	c.RefreshHTTPStatusPolicy = normalizeNestedStringMap(c.RefreshHTTPStatusPolicy)
	c.QuotaHTTPStatusPolicy = normalizeNestedStringMap(c.QuotaHTTPStatusPolicy)
	if c.ListenReadHeaderTimeoutSec < 1 {
		c.ListenReadHeaderTimeoutSec = 60
	}
	if c.ListenReadHeaderTimeoutSec > 600 {
		c.ListenReadHeaderTimeoutSec = 600
	}
	if c.ListenIdleTimeoutSec < 0 {
		c.ListenIdleTimeoutSec = 180
	}
	if c.ListenIdleTimeoutSec > 0 && c.ListenIdleTimeoutSec < 30 {
		c.ListenIdleTimeoutSec = 30
	}
	if c.ListenTCPKeepaliveSec < 0 {
		c.ListenTCPKeepaliveSec = 30
	}
	if c.ListenMaxHeaderBytes < 4096 && c.ListenMaxHeaderBytes != 0 {
		c.ListenMaxHeaderBytes = 4096
	}
	if c.H2MaxConcurrentStreams < 0 {
		c.H2MaxConcurrentStreams = 1000
	}
	if c.H2MaxConcurrentStreams > 0 && c.H2MaxConcurrentStreams < 100 {
		c.H2MaxConcurrentStreams = 100
	}
	if c.H2MaxConcurrentStreams > 10000 {
		c.H2MaxConcurrentStreams = 10000
	}

	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		c.LogLevel = "info"
	}

	level, err := log.ParseLevel(c.LogLevel)
	if err == nil {
		log.SetLevel(level)
	}
}

func (c *Config) Validate() error {
	if err := validateProxyURL(c.ProxyURL); err != nil {
		return fmt.Errorf("proxy-url 配置无效: %w", err)
	}
	return nil
}

func validateProxyURL(proxyURL string) error {
	if proxyURL == "" {
		return nil
	}

	u, err := url.Parse(proxyURL)
	if err != nil {
		return fmt.Errorf("解析失败: %w", err)
	}

	scheme := strings.ToLower(strings.TrimSpace(u.Scheme))
	switch scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return fmt.Errorf("不支持的代理协议 %q，仅支持 http/https/socks5/socks5h", u.Scheme)
	}

	if strings.TrimSpace(u.Hostname()) == "" {
		return fmt.Errorf("缺少代理主机")
	}

	if (scheme == "socks5" || scheme == "socks5h") && strings.TrimSpace(u.Port()) == "" {
		return fmt.Errorf("%s 代理必须显式指定端口", scheme)
	}

	if u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("不支持 query 或 fragment")
	}

	if u.Path != "" && u.Path != "/" {
		return fmt.Errorf("不支持路径后缀 %q", u.Path)
	}

	return nil
}

func normalizeNestedStringMap(m map[string]map[string]string) map[string]map[string]string {
	if len(m) == 0 {
		return m
	}
	out := make(map[string]map[string]string, len(m))
	for k, v := range m {
		kk := strings.TrimSpace(k)
		if kk == "" || v == nil {
			continue
		}
		inner := make(map[string]string, len(v))
		for ik, iv := range v {
			ikk := strings.TrimSpace(ik)
			if ikk == "" {
				continue
			}
			inner[ikk] = strings.TrimSpace(iv)
		}
		if len(inner) > 0 {
			out[kk] = inner
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func normalizeDBDriver(s string) string {
	d := strings.ToLower(strings.TrimSpace(s))
	switch d {
	case "pg", "postgresql":
		return "postgres"
	case "mariadb":
		return "mysql"
	case "sqlite3":
		return "sqlite"
	default:
		return d
	}
}
