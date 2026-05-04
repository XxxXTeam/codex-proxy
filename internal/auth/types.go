/**
 * 账号认证类型定义模块
 * 定义 Codex Token 数据结构、账号文件存储格式和运行时认证状态
 */
package auth

import (
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tidwall/gjson"
)

/**
 * TokenData 保存从 OpenAI OAuth 获取的 Token 信息
 * @field IDToken - JWT ID Token，包含用户声明
 * @field AccessToken - OAuth2 访问令牌
 * @field RefreshToken - 用于获取新访问令牌的刷新令牌
 * @field AccountID - OpenAI 账号标识符
 * @field Email - 账号邮箱
 * @field Expire - 访问令牌过期时间戳（RFC3339格式）
 */
type TokenData struct {
	IDToken      string `json:"id_token"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	AccountID    string `json:"account_id"`
	Email        string `json:"email"`
	Expire       string `json:"expired"`
	PlanType     string `json:"plan_type,omitempty"`
}

/**
 * TokenFile 表示磁盘上的账号文件结构
 * @field IDToken - JWT ID Token
 * @field AccessToken - 访问令牌
 * @field RefreshToken - 刷新令牌
 * @field AccountID - 账号ID
 * @field LastRefresh - 上次刷新时间戳
 * @field Email - 邮箱
 * @field Type - 认证类型，固定为 "codex"
 * @field Expire - Token 过期时间
 */
type TokenFile struct {
	IDToken      string `json:"id_token"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	/* RK 与 refresh_token 等价；JSON 中 null 与省略 unmarsh 后均为空串 */
	RK          string `json:"rk"`
	AccountID   string `json:"account_id"`
	LastRefresh string `json:"last_refresh"`
	Email       string `json:"email"`
	Type        string `json:"type"`
	Expire      string `json:"expired"`
}

/**
 * Account 表示运行时的单个 Codex 账号状态
 * @field mu - 并发保护锁
 * @field FilePath - 账号文件路径
 * @field Token - 当前 Token 数据
 * @field Status - 账号状态（active/cooldown/disabled）
 * @field LastError - 最近一次错误
 * @field LastRefreshedAt - 上次成功刷新时间
 * @field NextRetryAfter - 下次允许重试的时间
 * @field CooldownUntil - 冷却结束时间
 * @field ConsecutiveFailures - 连续失败次数
 * @field LastUsedAt - 最后一次使用时间
 * @field TotalRequests - 总请求数（原子操作）
 * @field TotalErrors - 总错误数（原子操作）
 * @field DisableReason - 禁用原因编码
 */
type Account struct {
	mu                  sync.RWMutex
	FilePath            string
	Token               TokenData
	Status              AccountStatus
	LastError           error
	LastRefreshedAt     time.Time
	NextRetryAfter      time.Time
	CooldownUntil       time.Time
	ConsecutiveFailures int
	LastUsedAt          time.Time
	TotalRequests       atomic.Int64
	TotalErrors         atomic.Int64
	DisableReason       string
	QuotaResetsAt       time.Time
	QuotaExhausted      bool
	TotalInputTokens    atomic.Int64
	TotalOutputTokens   atomic.Int64
	TotalTokens         atomic.Int64
	TotalCompletions    atomic.Int64
	QuotaInfo           *QuotaInfo
	QuotaCheckedAt      time.Time

	/* 原子状态字段（热路径无锁读取） */
	atomicStatus     atomic.Int32 /* 存储 AccountStatus 枚举值 */
	atomicCooldownMs atomic.Int64 /* 存储 CooldownUntil 的 UnixMilli */
	atomicUsedPct    atomic.Int64 /* 存储 usedPercent * 100（定点数），-100 表示未知 */

	/* 刷新去重字段 */
	refreshing    atomic.Int32 /* CAS 标志：0=空闲，1=正在刷新。防止同一账号被重复刷新 */
	lastRefreshMs atomic.Int64 /* 上次刷新完成时间戳（UnixMilli），用于快速判断是否需要刷新 */

	/* access_token 过期时刻（UnixMilli），0 表示未知；选号时用于排除即将过期的账号 */
	accessExpireUnixMs atomic.Int64

	/* RecordSuccess 时写入，供 PickRecentlySuccessful O(N) 取最近成功号，无需全表排序 */
	lastSuccessUnixMs atomic.Int64

	/* 上游 429 后的额度恢复流程，防止同一账号堆积多个恢复 goroutine */
	upstream429Recovering atomic.Int32
}

/**
 * AccountStatus 账号状态枚举
 */
type AccountStatus int

const (
	/* StatusActive 账号正常可用 */
	StatusActive AccountStatus = iota
	/* StatusCooldown 账号冷却中（限频等） */
	StatusCooldown
	/* StatusDisabled 账号已禁用（刷新失败等） */
	StatusDisabled
)

/* 禁用原因编码 */
const (
	ReasonNone    = ""
	ReasonAuth401 = "auth_401"
	/* ReasonAuth401NoRefreshToken 上游 401 且无 refresh_token，已从号池删除 */
	ReasonAuth401NoRefreshToken = "auth_401_no_refresh_token"
	/* ReasonAuth401Disabled 上游 401 且刷新/额度复核均失败，凭据文件已重命名禁用 */
	ReasonAuth401Disabled    = "auth_401_disabled"
	ReasonAuth403            = "auth_403"
	ReasonQuotaExhausted     = "quota_exhausted"
	ReasonRefreshFailed      = "refresh_failed"
	ReasonHealthCheck        = "health_check_failed"
	ReasonQuotaRecheckFailed = "quota_recheck_failed"
	/* ReasonRefresh429 token 刷新接口返回 HTTP 429 */
	ReasonRefresh429 = "refresh_http_429"
	/* ReasonQuotaHTTP429 额度查询接口返回 HTTP 429 */
	ReasonQuotaHTTP429 = "quota_http_429"
	/* ReasonQuotaInvalidAfterRefresh OAuth 刷新成功但 wham/usage 返回无效（非 200 且非 429），视为废号 */
	ReasonQuotaInvalidAfterRefresh = "quota_invalid_after_refresh"
	/* ReasonRestoreProbeFailed 周期性「禁用凭据恢复」探测中 OAuth/额度不通过，已删除凭据文件 */
	ReasonRestoreProbeFailed = "restore_probe_failed"
)

/**
 * Auth401RecoverStatus POST /recover-auth 等「401 恢复」流程的终端状态
 */
type Auth401RecoverStatus string

const (
	Auth401RecoverInvalid            Auth401RecoverStatus = "invalid_input"
	Auth401RecoverSkippedBusy        Auth401RecoverStatus = "skipped_busy"
	Auth401RecoverRefreshed          Auth401RecoverStatus = "refreshed"
	Auth401RecoverWaitedRefreshIdle  Auth401RecoverStatus = "waited_refresh_idle"
	/* Auth401RecoverRefreshFailedCooldown 刷新失败且策略落地为冷却（含 401/429 等），并非 Token 已恢复 */
	Auth401RecoverRefreshFailedCooldown Auth401RecoverStatus = "refresh_failed_cooldown"
	/* Auth401RecoverCooldown429OK 历史兼容；新逻辑请用 refresh_failed_cooldown 或 refreshed */
	Auth401RecoverCooldown429OK      Auth401RecoverStatus = "cooldown_429_quota_ok"
	Auth401RecoverDisabled            Auth401RecoverStatus = "disabled"
	Auth401RecoverRemoved             Auth401RecoverStatus = "removed"
)

/**
 * Auth401RecoverResult 单次账号 401 恢复（同步刷新 → 429 则查额度 → 失败则禁用凭据）的结果
 */
type Auth401RecoverResult struct {
	Email      string               `json:"email"`
	FilePath   string               `json:"file_path,omitempty"`
	Status     Auth401RecoverStatus `json:"status"`
	ReasonCode string               `json:"reason_code,omitempty"`
	Detail     string               `json:"detail,omitempty"`
}

/**
 * AccountStats 账号统计信息（只读快照）
 * @field Email - 账号邮箱
 * @field Status - 当前状态
 * @field DisableReason - 禁用原因
 * @field TotalRequests - 总请求数
 * @field TotalErrors - 总错误数
 * @field ConsecutiveFailures - 连续失败次数
 * @field LastUsedAt - 最后使用时间
 * @field CooldownUntil - 冷却结束时间
 */
type AccountStats struct {
	Email               string     `json:"email"`
	Status              string     `json:"status"`
	PlanType            string     `json:"plan_type,omitempty"`
	DisableReason       string     `json:"disable_reason,omitempty"`
	TotalRequests       int64      `json:"total_requests"`
	TotalErrors         int64      `json:"total_errors"`
	ConsecutiveFailures int        `json:"consecutive_failures"`
	LastUsedAt          time.Time  `json:"last_used_at,omitempty"`
	LastRefreshedAt     time.Time  `json:"last_refreshed_at,omitempty"`
	CooldownUntil       time.Time  `json:"cooldown_until,omitempty"`
	QuotaExhausted      bool       `json:"quota_exhausted"`
	QuotaResetsAt       time.Time  `json:"quota_resets_at,omitempty"`
	TokenExpire         string     `json:"token_expire,omitempty"`
	Usage               UsageStats `json:"usage"`
	Quota               *QuotaInfo `json:"quota,omitempty"`
}

/**
 * UsageStats token 使用量统计
 * @field TotalCompletions - 总补全次数
 * @field InputTokens - 输入 token 总量
 * @field OutputTokens - 输出 token 总量
 * @field TotalTokens - token 总量
 */
type UsageStats struct {
	TotalCompletions int64 `json:"total_completions"`
	InputTokens      int64 `json:"input_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}

/**
 * QuotaInfo 账号额度信息（来自 wham/usage API）
 * @field Valid - 账号是否有效（API 返回 200）
 * @field RawJSON - 原始响应 JSON（透传展示）
 */
type QuotaInfo struct {
	Valid      bool            `json:"valid"`
	StatusCode int             `json:"status_code"`
	RawData    json.RawMessage `json:"raw_data,omitempty"`
	CheckedAt  time.Time       `json:"checked_at"`
}

/**
 * IsAvailable 检查账号当前是否可用
 * @returns bool - 如果账号状态为 active 或冷却已过则返回 true
 */
func (a *Account) IsAvailable() bool {
	/* 使用原子字段无锁判断，避免热路径上的锁竞争 */
	status := AccountStatus(a.atomicStatus.Load())
	if status == StatusDisabled {
		return false
	}
	if status == StatusCooldown {
		cooldownMs := a.atomicCooldownMs.Load()
		if time.Now().UnixMilli() < cooldownMs {
			return false
		}
	}
	return true
}

/**
 * GetAccessToken 安全获取当前的 AccessToken
 * @returns string - 当前 AccessToken
 */
func (a *Account) GetAccessToken() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.Token.AccessToken
}

/**
 * HasRefreshToken 是否具备非空的 refresh_token（含上传 JSON 中 rk→refresh_token 合并后；字段为 null 或 "" 均视为无 rk）
 */
func (a *Account) HasRefreshToken() bool {
	if a == nil {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return strings.TrimSpace(a.Token.RefreshToken) != ""
}

/**
 * GetAccountID 安全获取当前的 AccountID
 * @returns string - 当前 AccountID
 */
func (a *Account) GetAccountID() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.Token.AccountID
}

/**
 * GetEmail 安全获取当前的 Email
 * @returns string - 当前 Email
 */
func (a *Account) GetEmail() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.Token.Email
}

/**
 * GetLastUsedAt 最近一次成功完成请求的时间（RecordSuccess 更新），零值表示尚无成功记录
 */
func (a *Account) GetLastUsedAt() time.Time {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.LastUsedAt
}

/**
 * TokenSnapshot 返回当前 Token 副本（持锁短读，供合并或跨账号同步）
 */
func (a *Account) TokenSnapshot() TokenData {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.Token
}

/**
 * UpdateToken 安全更新 Token 数据
 * OAuth 刷新响应常省略 refresh_token / id_token；缺省时保留旧值，避免清空 Chatgpt-Account-Id 等导致刷新成功仍连续 401
 * @param td - 新的 Token 数据
 */
func (a *Account) UpdateToken(td TokenData) {
	now := time.Now()
	a.mu.Lock()
	prev := a.Token
	if td.RefreshToken == "" {
		td.RefreshToken = prev.RefreshToken
	}
	if strings.TrimSpace(td.AccountID) == "" {
		td.AccountID = prev.AccountID
	}
	if strings.TrimSpace(td.Email) == "" {
		td.Email = prev.Email
	}
	if td.IDToken == "" {
		td.IDToken = prev.IDToken
	}
	var expMs int64
	if td.Expire != "" {
		if t, err := time.Parse(time.RFC3339, td.Expire); err == nil {
			expMs = t.UnixMilli()
		}
	}
	a.Token = td
	a.LastRefreshedAt = now
	a.Status = StatusActive
	a.LastError = nil
	a.mu.Unlock()

	/* 同步更新原子状态 */
	a.atomicStatus.Store(int32(StatusActive))
	a.lastRefreshMs.Store(now.UnixMilli())
	a.accessExpireUnixMs.Store(expMs)
}

/**
 * IsTokenExpiringSoon 检查 access_token 是否即将过期（5 分钟内）或已过期
 * 用于发送请求前主动触发刷新，避免发送过期 token 到上游产生 401/429
 * @returns bool - 如果 token 即将过期或已过期返回 true
 */
func (a *Account) IsTokenExpiringSoon() bool {
	expMs := a.accessExpireUnixMs.Load()
	if expMs == 0 {
		/* 未知过期时间，检查 Token.Expire 字符串 */
		a.mu.RLock()
		expire := a.Token.Expire
		a.mu.RUnlock()
		if expire == "" {
			return false /* 无过期信息，不阻止 */
		}
		if t, err := time.Parse(time.RFC3339, expire); err == nil {
			return time.Until(t) < 5*time.Minute
		}
		return false
	}
	return time.Now().UnixMilli() >= expMs-int64(5*60*1000)
}

/**
 * SyncAccessExpireFromToken 根据当前 Token.Expire 刷新 accessExpireUnixMs（加载账号后调用）
 */
func (a *Account) SyncAccessExpireFromToken() {
	a.mu.RLock()
	defer a.mu.RUnlock()
	var expMs int64
	if a.Token.Expire != "" {
		if t, err := time.Parse(time.RFC3339, a.Token.Expire); err == nil {
			expMs = t.UnixMilli()
		}
	}
	a.accessExpireUnixMs.Store(expMs)
}

/**
 * SetCooldown 将账号设为冷却状态
 * @param duration - 冷却持续时间
 */
func (a *Account) SetCooldown(duration time.Duration) {
	until := time.Now().Add(duration)
	a.mu.Lock()
	a.Status = StatusCooldown
	a.CooldownUntil = until
	a.mu.Unlock()

	/* 同步更新原子状态 */
	a.atomicStatus.Store(int32(StatusCooldown))
	a.atomicCooldownMs.Store(until.UnixMilli())
}

/**
 * SetQuotaCooldown 设置配额耗尽冷却（429 限频）
 * @param duration - 冷却持续时间
 */
func (a *Account) SetQuotaCooldown(duration time.Duration) {
	until := time.Now().Add(duration)
	a.mu.Lock()
	a.Status = StatusCooldown
	a.CooldownUntil = until
	a.QuotaExhausted = true
	a.QuotaResetsAt = until
	a.mu.Unlock()

	/* 同步更新原子状态 */
	a.atomicStatus.Store(int32(StatusCooldown))
	a.atomicCooldownMs.Store(until.UnixMilli())
	a.atomicUsedPct.Store(10000) /* 100.00% */
}

/**
 * SetDisabled 将账号标记为禁用
 * @param err - 禁用原因
 */
func (a *Account) SetDisabled(err error) {
	a.mu.Lock()
	a.Status = StatusDisabled
	a.LastError = err
	a.mu.Unlock()

	a.atomicStatus.Store(int32(StatusDisabled))
}

/**
 * SetDisabledWithReason 将账号标记为禁用，并记录原因编码
 * @param err - 禁用原因
 * @param reason - 原因编码
 */
func (a *Account) SetDisabledWithReason(err error, reason string) {
	a.mu.Lock()
	a.Status = StatusDisabled
	a.LastError = err
	a.DisableReason = reason
	a.mu.Unlock()

	a.atomicStatus.Store(int32(StatusDisabled))
}

/**
 * SetActive 恢复账号为可用状态
 */
func (a *Account) SetActive() {
	a.mu.Lock()
	a.Status = StatusActive
	a.LastError = nil
	a.ConsecutiveFailures = 0
	a.DisableReason = ReasonNone
	a.QuotaExhausted = false
	a.QuotaResetsAt = time.Time{}
	a.mu.Unlock()

	/* 同步更新原子状态 */
	a.atomicStatus.Store(int32(StatusActive))
	a.atomicCooldownMs.Store(0)
}

/**
 * RecordSuccess 记录一次成功请求
 */
func (a *Account) RecordSuccess() {
	a.TotalRequests.Add(1)
	a.mu.Lock()
	a.ConsecutiveFailures = 0
	a.LastUsedAt = time.Now()
	a.mu.Unlock()
	a.lastSuccessUnixMs.Store(time.Now().UnixMilli())
}

/**
 * RecordUsage 记录一次请求的 token 使用量
 * @param inputTokens - 输入 token 数
 * @param outputTokens - 输出 token 数
 * @param totalTokens - 总 token 数
 */
func (a *Account) RecordUsage(inputTokens, outputTokens, totalTokens int64) {
	a.TotalCompletions.Add(1)
	if inputTokens > 0 {
		a.TotalInputTokens.Add(inputTokens)
	}
	if outputTokens > 0 {
		a.TotalOutputTokens.Add(outputTokens)
	}
	if totalTokens > 0 {
		a.TotalTokens.Add(totalTokens)
	} else if inputTokens+outputTokens > 0 {
		a.TotalTokens.Add(inputTokens + outputTokens)
	}
}

/**
 * GetUsedPercent 获取账号的额度使用率百分比
 * 从 QuotaInfo.RawData 中提取 rate_limit.primary_window.used_percent
 * 未查询过额度的账号返回 -1（排到最后）
 * 配额耗尽的账号返回 100
 * @returns float64 - 使用率（0-100），-1 表示未知
 */
func (a *Account) GetUsedPercent() float64 {
	/* 直接从原子字段读取，无锁 */
	v := a.atomicUsedPct.Load()
	return float64(v) / 100.0
}

/**
 * RefreshUsedPercent 从 QuotaInfo 重新计算并更新原子缓存的 usedPercent
 * 在额度查询完成后调用，避免排序时逐个加锁解析 JSON
 */
func (a *Account) RefreshUsedPercent() {
	a.mu.RLock()
	exhausted := a.QuotaExhausted
	qi := a.QuotaInfo
	a.mu.RUnlock()

	if exhausted {
		a.atomicUsedPct.Store(10000) /* 100.00% */
		return
	}
	if qi == nil || !qi.Valid || len(qi.RawData) == 0 {
		a.atomicUsedPct.Store(-100) /* -1.00 → 未知 */
		return
	}

	result := gjson.GetBytes(qi.RawData, "rate_limit.primary_window.used_percent")
	if !result.Exists() {
		a.atomicUsedPct.Store(-100)
		return
	}
	/* 存储为定点数：percent * 100 */
	a.atomicUsedPct.Store(int64(result.Float() * 100))
}

/**
 * RecordFailure 记录一次失败请求
 * @returns int - 当前连续失败次数
 */
func (a *Account) RecordFailure() int {
	a.TotalRequests.Add(1)
	a.TotalErrors.Add(1)
	a.mu.Lock()
	a.ConsecutiveFailures++
	a.LastUsedAt = time.Now()
	failures := a.ConsecutiveFailures
	a.mu.Unlock()
	return failures
}

/**
 * GetStats 获取账号统计信息快照
 * @returns AccountStats - 统计快照
 */
func (a *Account) GetStats() AccountStats {
	a.mu.RLock()
	defer a.mu.RUnlock()

	statusStr := "active"
	switch a.Status {
	case StatusCooldown:
		statusStr = "cooldown"
	case StatusDisabled:
		statusStr = "disabled"
	}

	/* 配额状态：如果已过期则自动恢复 */
	quotaExhausted := a.QuotaExhausted
	quotaResetsAt := a.QuotaResetsAt
	if quotaExhausted && !quotaResetsAt.IsZero() && time.Now().After(quotaResetsAt) {
		quotaExhausted = false
	}

	return AccountStats{
		Email:               a.Token.Email,
		Status:              statusStr,
		PlanType:            a.Token.PlanType,
		DisableReason:       a.DisableReason,
		TotalRequests:       a.TotalRequests.Load(),
		TotalErrors:         a.TotalErrors.Load(),
		ConsecutiveFailures: a.ConsecutiveFailures,
		LastUsedAt:          a.LastUsedAt,
		LastRefreshedAt:     a.LastRefreshedAt,
		CooldownUntil:       a.CooldownUntil,
		QuotaExhausted:      quotaExhausted,
		QuotaResetsAt:       quotaResetsAt,
		TokenExpire:         a.Token.Expire,
		Usage: UsageStats{
			TotalCompletions: a.TotalCompletions.Load(),
			InputTokens:      a.TotalInputTokens.Load(),
			OutputTokens:     a.TotalOutputTokens.Load(),
			TotalTokens:      a.TotalTokens.Load(),
		},
		Quota: a.QuotaInfo,
	}
}
