package auth

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	codexdb "codex-proxy/internal/db"

	log "github.com/sirupsen/logrus"
	"golang.org/x/sync/singleflight"
)

/* defaultStartupLoadBatch 异步启动时单批并入号池的 JSON 文件数上限 */
const defaultStartupLoadBatch = 8000

/* 并发刷新与运维默认配置 */
const (
	defaultRefreshConcurrency      = 50
	defaultScanInterval            = 30
	defaultSaveWorkers             = 4
	defaultCooldown401Sec          = 30
	defaultCooldown429Sec          = 60
	defaultRefreshSingleTimeoutSec = 30
)

/**
 * ManagerOptions 账号管理器的可选配置（由 config 传入，零值使用默认）
 */
type ManagerOptions struct {
	AuthScanInterval        int /* 热加载扫描间隔（秒） */
	SaveWorkers             int /* 异步写入协程数 */
	Cooldown401Sec          int /* 401 后冷却（秒） */
	Cooldown429Sec          int /* 429 后冷却（秒） */
	RefreshSingleTimeoutSec int /* 后台 OAuth 刷新单次超时（秒），不影响 Codex 对话 SSE */
	RefreshBatchSize        int /* 刷新批大小，0=不分批；>0 每批完成后再启下一批以控内存 */
	/* RefreshHTTP429Action 未在 refresh-http-status-policy 中配置 429 时的默认 final（phase=none） */
	RefreshHTTP429Action   string
	QuotaHTTP429Action     string
	QuotaHTTPStatusActions map[string]string
	/* RefreshHTTPStatusPolicy 刷新 token 接口按状态码：phase=none|refresh_once|cooldown_then_retry，final=remove|disable|cooldown */
	RefreshHTTPStatusPolicy map[string]map[string]string
	/* QuotaHTTPStatusPolicy 额度 wham/usage 同上；未出现在表中的状态码走简单逻辑（一般 4xx 直接 remove） */
	QuotaHTTPStatusPolicy map[string]map[string]string
	/* Auth401SyncRefreshConcurrency 请求路径上「401→同步 OAuth」的全局并发上限；0 表示不限制。
	   高并发时打满 OAuth 易 429，槽满则直接换号（后台周期刷新仍会修 Token），减少无效刷新与 WARN 刷屏。 */
	Auth401SyncRefreshConcurrency int
	/* DBDialect 持久化时使用，与 internal/db 方言一致；零值等价于 PostgreSQL */
	DBDialect codexdb.Dialect
	/* QuotaPrecheck 为 true 时，401 恢复路径在 OAuth 成功后仍跑 wham/usage；false 时仅刷新 Token，交周期刷新/上游错误处置 */
	QuotaPrecheck bool
	/* DisableAuth401Remove true 时 401 刷新失败只冷却不删号/禁用，保留账号等周期刷新再尝试 */
	DisableAuth401Remove bool
}

/**
 * Manager 账号管理器
 * @field mu - 并发保护锁
 * @field accounts - 已加载的账号列表
 * @field accountIndex - 文件路径 → 账号索引（O(1) 查找）
 * @field refresher - Token 刷新器
 * @field selector - 账号选择器
 * @field authDir - 账号文件目录
 * @field refreshInterval - 刷新间隔（秒）
 * @field refreshConcurrency - 并发刷新数
 * @field stopCh - 停止信号通道
 */
type Manager struct {
	mu                      sync.RWMutex
	accounts                []*Account
	accountIndex            map[string]*Account
	accountsPtr             atomic.Pointer[[]*Account] /* 原子快照，Pick 热路径零锁读取 */
	refresher               *Refresher
	selector                Selector
	authDir                 string
	db                      *sql.DB
	dbDialect               codexdb.Dialect
	saveTokenStmt           *sql.Stmt
	saveTokenStmtByEmail    *sql.Stmt
	refreshInterval         int
	refreshConcurrency      int
	scanIntervalSec         int
	saveWorkers             int
	cooldown401Sec          int
	cooldown429Sec          int
	refreshSingleTimeoutSec int
	refreshBatchSize        int
	saveQueue               chan *Account /* 异步磁盘写入队列 */
	stopCh                  chan struct{}
	importMu                sync.Mutex /* 防止并发导入账号文件到数据库 */
	refreshHTTPPolicy       map[int]httpStatusPolicy
	quotaHTTPPolicy         map[int]httpStatusPolicy
	/* auth401SF 合并同一凭据文件的并发 401 恢复，避免多请求重复打 OAuth */
	auth401SF singleflight.Group
	/* auth401SyncSem 非 nil 时限制全局同步 OAuth 并发（recoverAuth401Once 内 acquire） */
	auth401SyncSem chan struct{}
	/* postRefreshQuota OAuth 刷新成功后用于立刻校验 wham/usage；nil 表示不校验（由 SetPostRefreshQuotaChecker 注入） */
	postRefreshQuota atomic.Pointer[QuotaChecker]
	/* quotaPrecheck 见 ManagerOptions.QuotaPrecheck；false 时 recoverAuth401Once 不在刷新后查 wham */
	quotaPrecheck bool
	/* disableAuth401Remove true 时 401 刷新失败不删号/不禁用，只冷却 */
	disableAuth401Remove bool
}

/**
 * NewManager 创建新的账号管理器
 * @param authDir - 账号文件目录
 * @param proxyURL - 代理地址
 * @param refreshInterval - 刷新间隔（秒）
 * @param selector - 账号选择器
 * @param opts - 可选配置，nil 时使用默认值
 * @returns *Manager - 账号管理器实例
 */
func NewManager(authDir string, db *sql.DB, proxyURL string, refreshInterval int, selector Selector, enableHTTP2 bool, opts *ManagerOptions) *Manager {
	if selector == nil {
		selector = NewRoundRobinSelector()
	}
	m := &Manager{
		db:                      db,
		accounts:                make([]*Account, 0, 1024),
		accountIndex:            make(map[string]*Account, 1024),
		refresher:               NewRefresher(proxyURL, enableHTTP2),
		selector:                selector,
		authDir:                 authDir,
		refreshInterval:         refreshInterval,
		refreshConcurrency:      defaultRefreshConcurrency,
		scanIntervalSec:         defaultScanInterval,
		saveWorkers:             defaultSaveWorkers,
		cooldown401Sec:          defaultCooldown401Sec,
		cooldown429Sec:          defaultCooldown429Sec,
		refreshSingleTimeoutSec: defaultRefreshSingleTimeoutSec,
		saveQueue:               make(chan *Account, 4096),
		stopCh:                  make(chan struct{}),
	}
	if opts != nil {
		m.dbDialect = opts.DBDialect
		if opts.AuthScanInterval > 0 {
			m.scanIntervalSec = opts.AuthScanInterval
		}
		if opts.SaveWorkers > 0 {
			m.saveWorkers = opts.SaveWorkers
		}
		if opts.Cooldown401Sec > 0 {
			m.cooldown401Sec = opts.Cooldown401Sec
		}
		if opts.Cooldown429Sec > 0 {
			m.cooldown429Sec = opts.Cooldown429Sec
		}
		if opts.RefreshSingleTimeoutSec > 0 {
			m.refreshSingleTimeoutSec = opts.RefreshSingleTimeoutSec
		}
		if opts.RefreshBatchSize > 0 {
			m.refreshBatchSize = opts.RefreshBatchSize
		}
		if opts.Auth401SyncRefreshConcurrency > 0 {
			m.auth401SyncSem = make(chan struct{}, opts.Auth401SyncRefreshConcurrency)
		}
		m.quotaPrecheck = opts.QuotaPrecheck
		m.disableAuth401Remove = opts.DisableAuth401Remove
	}
	m.refreshHTTPPolicy = mergeRefreshHTTPPolicies(opts)
	m.quotaHTTPPolicy = mergeQuotaHTTPPolicies(opts)
	empty := make([]*Account, 0)
	m.accountsPtr.Store(&empty)

	if m.db != nil {
		if err := m.prepareDBStatements(); err != nil {
			log.Fatalf("准备数据库语句失败: %v", err)
		}
	}

	return m
}

/* refreshRequestContext 用于 OAuth 刷新 HTTP：超时仅基于 Background，不继承调用方 context 的 deadline，避免长对话/批处理把 parent 掐死导致误报 context deadline exceeded */
func (m *Manager) refreshRequestContext(_ context.Context) (context.Context, context.CancelFunc) {
	sec := m.refreshSingleTimeoutSec
	if sec < 1 {
		sec = defaultRefreshSingleTimeoutSec
	}
	return context.WithTimeout(context.Background(), time.Duration(sec)*time.Second)
}

/* waitAccountRefreshIdle 等待他处（如后台刷新）释放 refreshing 标志，避免 401 时误判 skipped_busy 导致不换 token 就换号/空返回 */
func (m *Manager) waitAccountRefreshIdle(ctx context.Context, acc *Account) bool {
	if acc == nil {
		return false
	}
	if acc.refreshing.Load() == 0 {
		return true
	}
	delay := 5 * time.Millisecond
	const maxDelay = 50 * time.Millisecond
	for {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(delay):
			if acc.refreshing.Load() == 0 {
				return true
			}
			if delay < maxDelay {
				delay *= 2
				if delay > maxDelay {
					delay = maxDelay
				}
			}
		}
	}
}

/**
 * EnsureTokenFresh 发送上游请求前调用：若账号 access_token 即将过期则尝试内联刷新。
 * 其他 goroutine 已在刷新时等待其完成；刷新失败时不阻止请求（由上游 401 触发换号）。
 * 返回 true 表示 token 已可用（无论是否刷新过），false 表示账号已经不可用。
 */
func (m *Manager) EnsureTokenFresh(ctx context.Context, acc *Account) bool {
	if acc == nil {
		return false
	}
	if !acc.IsTokenExpiringSoon() {
		return true
	}
	if !acc.HasRefreshToken() {
		return true /* 无 refresh_token，交上游 401 换号 */
	}

	/* CAS 抢占刷新权 */
	if !acc.refreshing.CompareAndSwap(0, 1) {
		/* 他处已在刷新，短等待完成 */
		m.waitAccountRefreshIdle(ctx, acc)
		return acc.IsAvailable()
	}
	defer acc.refreshing.Store(0)

	acc.mu.RLock()
	rt := acc.Token.RefreshToken
	email := acc.Token.Email
	acc.mu.RUnlock()

	log.Debugf("账号 [%s] token 即将过期，内联刷新…", email)

	rctx, rcancel := m.refreshRequestContext(ctx)
	defer rcancel()
	td, err := m.refresher.RefreshTokenWithRetry(rctx, rt, 2)
	if err != nil {
		log.Warnf("账号 [%s] 内联刷新失败: %v，仍尝试上游请求", email, err)
		return true /* 不阻止，让上游 401 触发换号 */
	}

	acc.UpdateToken(*td)
	m.enqueueSave(acc)
	m.InvalidateSelectorCache()
	log.Infof("账号 [%s] 内联刷新成功", acc.GetEmail())
	return true
}

func (m *Manager) prepareDBStatements() error {
	if m.db == nil {
		return nil
	}
	switch m.dbDialect {
	case codexdb.DialectMySQL:
		s := `
INSERT INTO codex_accounts (account_id,email,id_token,access_token,refresh_token,expire,plan_type,last_refresh,status,cooldown_until,disable_reason,last_used_at,updated_at)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,UTC_TIMESTAMP(6))
ON DUPLICATE KEY UPDATE
	email = VALUES(email),
	account_id = VALUES(account_id),
	id_token = VALUES(id_token),
	access_token = VALUES(access_token),
	refresh_token = VALUES(refresh_token),
	expire = VALUES(expire),
	plan_type = VALUES(plan_type),
	last_refresh = VALUES(last_refresh),
	status = VALUES(status),
	cooldown_until = VALUES(cooldown_until),
	disable_reason = VALUES(disable_reason),
	last_used_at = VALUES(last_used_at),
	updated_at = VALUES(updated_at)`
		stmt, err := m.db.Prepare(s)
		if err != nil {
			return err
		}
		m.saveTokenStmt = stmt
		m.saveTokenStmtByEmail = nil
		return nil
	case codexdb.DialectSQLite:
		s1 := `
INSERT INTO codex_accounts (account_id,email,id_token,access_token,refresh_token,expire,plan_type,last_refresh,status,cooldown_until,disable_reason,last_used_at,updated_at)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,CURRENT_TIMESTAMP)
ON CONFLICT(account_id) DO UPDATE SET
	email = excluded.email,
	id_token = excluded.id_token,
	access_token = excluded.access_token,
	refresh_token = excluded.refresh_token,
	expire = excluded.expire,
	plan_type = excluded.plan_type,
	last_refresh = excluded.last_refresh,
	status = excluded.status,
	cooldown_until = excluded.cooldown_until,
	disable_reason = excluded.disable_reason,
	last_used_at = excluded.last_used_at,
	updated_at = CURRENT_TIMESTAMP`
		stmt, err := m.db.Prepare(s1)
		if err != nil {
			return err
		}
		m.saveTokenStmt = stmt
		s2 := `
INSERT INTO codex_accounts (account_id,email,id_token,access_token,refresh_token,expire,plan_type,last_refresh,status,cooldown_until,disable_reason,last_used_at,updated_at)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,CURRENT_TIMESTAMP)
ON CONFLICT(email) DO UPDATE SET
	account_id = excluded.account_id,
	id_token = excluded.id_token,
	access_token = excluded.access_token,
	refresh_token = excluded.refresh_token,
	expire = excluded.expire,
	plan_type = excluded.plan_type,
	last_refresh = excluded.last_refresh,
	status = excluded.status,
	cooldown_until = excluded.cooldown_until,
	disable_reason = excluded.disable_reason,
	last_used_at = excluded.last_used_at,
	updated_at = CURRENT_TIMESTAMP`
		stmtEm, err := m.db.Prepare(s2)
		if err != nil {
			return err
		}
		m.saveTokenStmtByEmail = stmtEm
		return nil
	default:
		s1 := `
INSERT INTO codex_accounts (account_id,email,id_token,access_token,refresh_token,expire,plan_type,last_refresh,status,cooldown_until,disable_reason,last_used_at,updated_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,NOW())
ON CONFLICT (account_id) DO UPDATE SET
	email = EXCLUDED.email,
	id_token = EXCLUDED.id_token,
	access_token = EXCLUDED.access_token,
	refresh_token = EXCLUDED.refresh_token,
	expire = EXCLUDED.expire,
	plan_type = EXCLUDED.plan_type,
	last_refresh = EXCLUDED.last_refresh,
	status = EXCLUDED.status,
	cooldown_until = EXCLUDED.cooldown_until,
	disable_reason = EXCLUDED.disable_reason,
	last_used_at = EXCLUDED.last_used_at,
	updated_at = NOW()`
		stmt, err := m.db.Prepare(s1)
		if err != nil {
			return err
		}
		m.saveTokenStmt = stmt
		s2 := `
INSERT INTO codex_accounts (account_id,email,id_token,access_token,refresh_token,expire,plan_type,last_refresh,status,cooldown_until,disable_reason,last_used_at,updated_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,NOW())
ON CONFLICT (email) DO UPDATE SET
	account_id = EXCLUDED.account_id,
	id_token = EXCLUDED.id_token,
	access_token = EXCLUDED.access_token,
	refresh_token = EXCLUDED.refresh_token,
	expire = EXCLUDED.expire,
	plan_type = EXCLUDED.plan_type,
	last_refresh = EXCLUDED.last_refresh,
	status = EXCLUDED.status,
	cooldown_until = EXCLUDED.cooldown_until,
	disable_reason = EXCLUDED.disable_reason,
	last_used_at = EXCLUDED.last_used_at,
	updated_at = NOW()`
		stmtEm, err := m.db.Prepare(s2)
		if err != nil {
			return err
		}
		m.saveTokenStmtByEmail = stmtEm
		return nil
	}
}

/**
 * SetRefreshConcurrency 设置并发刷新数
 * @param n - 并发数，默认 50
 */
func (m *Manager) SetRefreshConcurrency(n int) {
	if n > 0 {
		m.refreshConcurrency = n
	}
}

/**
 * loadAccountsFromPathsParallel 并发解析路径列表（持锁外执行，供全量/分批加载共用）
 */
func loadAccountsFromPathsParallel(filePaths []string) []*Account {
	if len(filePaths) == 0 {
		return nil
	}
	type loadResult struct {
		path string
		acc  *Account
		err  error
	}
	workerCount := runtime.GOMAXPROCS(0) * 8
	if workerCount < 16 {
		workerCount = 16
	}
	if workerCount > 256 {
		workerCount = 256
	}
	if workerCount > len(filePaths) {
		workerCount = len(filePaths)
	}
	jobs := make(chan string, workerCount*2)
	results := make(chan loadResult, workerCount*2)
	var wg sync.WaitGroup
	for i := 0; i < workerCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for p := range jobs {
				acc, loadErr := loadAccountFromFile(p)
				results <- loadResult{path: p, acc: acc, err: loadErr}
			}
		}()
	}
	go func() {
		for _, p := range filePaths {
			jobs <- p
		}
		close(jobs)
		wg.Wait()
		close(results)
	}()
	out := make([]*Account, 0, len(filePaths))
	for r := range results {
		if r.err != nil {
			log.Warnf("加载账号文件失败 [%s]: %v", filepath.Base(r.path), r.err)
			continue
		}
		out = append(out, r.acc)
	}
	return out
}

/**
 * mergeAppendAccounts 将新解析的账号并入号池（按 FilePath 去重），并发布快照
 */
func (m *Manager) mergeAppendAccounts(newAccs []*Account) (added int) {
	if len(newAccs) == 0 {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, acc := range newAccs {
		if acc == nil {
			continue
		}
		if _, exists := m.accountIndex[acc.FilePath]; exists {
			continue
		}
		m.accounts = append(m.accounts, acc)
		m.accountIndex[acc.FilePath] = acc
		added++
	}
	if added > 0 {
		m.publishSnapshot()
	}
	return added
}

/**
 * LoadAccountsProgressive 按批并入号池：磁盘模式解析 JSON；数据库模式按行分页查询（与 mergeAppendAccounts 复用）
 */
func (m *Manager) LoadAccountsProgressive(ctx context.Context, batchSize int) error {
	m.mu.Lock()
	if m.db != nil {
		m.mu.Unlock()
		if batchSize < 1 {
			batchSize = defaultStartupLoadBatch
		}
		if m.authDir != "" {
			if _, err := m.importAccountsFromFilesToDB(); err != nil {
				log.Warnf("从磁盘迁移账号到数据库失败: %v", err)
			}
		}
		offset := 0
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
			batch, rowCount, err := m.loadAccountsFromDBSlice(ctx, offset, batchSize)
			if err != nil {
				return fmt.Errorf("分批加载数据库账号失败: %w", err)
			}
			if rowCount == 0 {
				break
			}
			added := m.mergeAppendAccounts(batch)
			log.Infof("启动分批加载(DB): 本批从库读 %d 行、纳入 %d 个账号，号池累计 %d 个，OFFSET %d～%d",
				rowCount, added, m.AccountCount(), offset, offset+rowCount-1)
			offset += rowCount
			if rowCount < batchSize {
				break
			}
		}
		if m.AccountCount() == 0 {
			log.Warnf("数据库中暂无有效账号，号池为空（可后续导入 JSON 或写入库）")
		}
		return nil
	}
	authDir := m.authDir
	m.mu.Unlock()

	if authDir == "" {
		return fmt.Errorf("未配置账号目录")
	}
	if batchSize < 1 {
		batchSize = defaultStartupLoadBatch
	}
	entries, err := os.ReadDir(authDir)
	if err != nil {
		return fmt.Errorf("读取账号目录失败: %w", err)
	}
	filePaths := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".json") {
			continue
		}
		filePaths = append(filePaths, filepath.Join(authDir, entry.Name()))
	}
	if len(filePaths) == 0 {
		log.Warnf("在目录 %s 中未找到 .json 账号文件，号池为空", authDir)
		return nil
	}
	for i := 0; i < len(filePaths); i += batchSize {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		end := i + batchSize
		if end > len(filePaths) {
			end = len(filePaths)
		}
		batch := filePaths[i:end]
		loaded := loadAccountsFromPathsParallel(batch)
		added := m.mergeAppendAccounts(loaded)
		log.Infof("启动分批加载: 本批纳入 %d 个有效账号，号池累计 %d 个，已处理 %d/%d 个文件",
			added, m.AccountCount(), end, len(filePaths))
	}
	if m.AccountCount() == 0 {
		log.Warnf("在目录 %s 中未解析出有效账号，号池为空", authDir)
	}
	return nil
}

/**
 * LoadAccounts 从账号目录加载所有 JSON 账号文件（全量替换号池）；或从数据库一次性加载
 * @returns error - 加载失败时返回错误
 */
func (m *Manager) LoadAccounts() error {
	m.mu.Lock()
	if m.db != nil {
		/* 始终检查是否有新 JSON 文件需要导入到数据库 */
		if m.authDir != "" {
			if _, err := m.importAccountsFromFilesToDB(); err != nil {
				log.Warnf("从磁盘迁移账号到数据库失败: %v", err)
			}
		}

		if err := m.loadAccountsFromDB(); err != nil {
			m.mu.Unlock()
			return fmt.Errorf("加载数据库账号失败: %w", err)
		}

		m.publishSnapshot()
		n := len(m.accounts)
		m.mu.Unlock()
		if n == 0 {
			log.Warnf("数据库中暂无有效账号，号池为空（可后续导入 JSON 或写入库）")
		} else {
			log.Infof("共加载 %d 个 Codex 账号（%s）", n, m.dbDialect.String())
		}
		return nil
	}
	authDir := m.authDir
	m.mu.Unlock()

	if authDir == "" {
		return fmt.Errorf("未配置账号目录")
	}
	entries, err := os.ReadDir(authDir)
	if err != nil {
		return fmt.Errorf("读取账号目录失败: %w", err)
	}
	filePaths := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".json") {
			continue
		}
		filePaths = append(filePaths, filepath.Join(authDir, entry.Name()))
	}
	accounts := loadAccountsFromPathsParallel(filePaths)
	index := make(map[string]*Account)
	if len(accounts) > 0 {
		index = make(map[string]*Account, len(accounts))
		for _, a := range accounts {
			index[a.FilePath] = a
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.accounts = accounts
	m.accountIndex = index
	m.publishSnapshot()
	if len(accounts) == 0 {
		if len(filePaths) == 0 {
			log.Warnf("在目录 %s 中未找到 .json 账号文件，号池为空", authDir)
		} else {
			log.Warnf("在目录 %s 中未解析出有效账号，号池为空", authDir)
		}
	} else {
		log.Infof("共加载 %d 个 Codex 账号", len(accounts))
	}
	return nil
}

/**
 * accountFromTokenFile 由 TokenFile 构造账号（与磁盘 JSON、导入 DB 共用）
 */
func accountFromTokenFile(tf *TokenFile, logicalPath string) (*Account, error) {
	rt := strings.TrimSpace(tf.RefreshToken)
	if rt == "" {
		rt = strings.TrimSpace(tf.RK)
	}
	if rt == "" && strings.TrimSpace(tf.AccessToken) == "" && strings.TrimSpace(tf.IDToken) == "" {
		return nil, fmt.Errorf("缺少 refresh_token/rk、access_token 与 id_token，至少需一项")
	}
	accountID := tf.AccountID
	email := tf.Email
	var planType string
	if tf.IDToken != "" {
		jwtAccountID, jwtEmail, jwtPlan := parseIDTokenClaims(tf.IDToken)
		if accountID == "" {
			accountID = jwtAccountID
		}
		if email == "" {
			email = jwtEmail
		}
		planType = jwtPlan
	}
	acc := &Account{
		FilePath: logicalPath,
		Token: TokenData{
			IDToken:      tf.IDToken,
			AccessToken:  tf.AccessToken,
			RefreshToken: rt,
			AccountID:    accountID,
			Email:        email,
			Expire:       tf.Expire,
			PlanType:     planType,
		},
		Status: StatusActive,
	}
	acc.SyncAccessExpireFromToken()
	return acc, nil
}

/**
 * loadAccountFromFile 从单个 JSON 文件加载账号
 * @param filePath - 文件路径
 * @returns *Account - 账号对象
 * @returns error - 加载失败时返回错误
 */
func loadAccountFromFile(filePath string) (*Account, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("读取文件失败: %w", err)
	}
	var tf TokenFile
	if err = json.Unmarshal(data, &tf); err != nil {
		return nil, fmt.Errorf("解析 JSON 失败: %w", err)
	}
	return accountFromTokenFile(&tf, filePath)
}

func accountFromDBRow(id int64, accountID, email, idToken, accessToken, refreshToken, expire, planType sql.NullString, lastRefresh sql.NullTime, status sql.NullInt32, cooldownUntil sql.NullTime, disableReason sql.NullString, lastUsedAt sql.NullTime) (*Account, bool) {
	if strings.TrimSpace(refreshToken.String) == "" && strings.TrimSpace(accessToken.String) == "" && strings.TrimSpace(idToken.String) == "" {
		return nil, false
	}
	key := "db:" + accountID.String
	if accountID.String == "" {
		key = "db:" + email.String
	}
	if key == "db:" {
		key = fmt.Sprintf("db:id:%d", id)
	}
	acc := &Account{
		FilePath: key,
		Token: TokenData{
			IDToken:      idToken.String,
			AccessToken:  accessToken.String,
			RefreshToken: refreshToken.String,
			AccountID:    accountID.String,
			Email:        email.String,
			Expire:       expire.String,
			PlanType:     planType.String,
		},
		Status:          StatusActive,
		LastRefreshedAt: lastRefresh.Time,
	}
	if lastRefresh.Valid {
		acc.lastRefreshMs.Store(lastRefresh.Time.UnixMilli())
	}
	/* 恢复运行时状态 */
	if status.Valid {
		acc.Status = AccountStatus(status.Int32)
		acc.atomicStatus.Store(status.Int32)
	}
	if cooldownUntil.Valid {
		acc.CooldownUntil = cooldownUntil.Time
		acc.atomicCooldownMs.Store(cooldownUntil.Time.UnixMilli())
	}
	if disableReason.Valid {
		acc.DisableReason = disableReason.String
	}
	if lastUsedAt.Valid {
		acc.LastUsedAt = lastUsedAt.Time
		acc.lastSuccessUnixMs.Store(lastUsedAt.Time.UnixMilli())
	}
	acc.SyncAccessExpireFromToken()
	return acc, true
}

/* loadAccountsFromDBSlice 返回有效账号与本次从库迭代的行数（含无 refresh_token 等被跳过的行），供 OFFSET 与磁盘「按批处理文件数」语义对齐 */
func (m *Manager) loadAccountsFromDBSlice(ctx context.Context, offset, limit int) ([]*Account, int, error) {
	if m.db == nil {
		return nil, 0, nil
	}
	base := `SELECT id, account_id,email,id_token,access_token,refresh_token,expire,plan_type,last_refresh,status,cooldown_until,disable_reason,last_used_at FROM codex_accounts ORDER BY id`
	var rows *sql.Rows
	var err error
	switch m.dbDialect {
	case codexdb.DialectPostgres:
		rows, err = m.db.QueryContext(ctx, base+` LIMIT $1 OFFSET $2`, limit, offset)
	default:
		rows, err = m.db.QueryContext(ctx, base+` LIMIT ? OFFSET ?`, limit, offset)
	}
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := make([]*Account, 0, limit)
	rowCount := 0
	for rows.Next() {
		rowCount++
		var id int64
		var accountID, email, idToken, accessToken, refreshToken, expire, planType, disableReason sql.NullString
		var lastRefresh, cooldownUntil, lastUsedAt sql.NullTime
		var status sql.NullInt32
		if err := rows.Scan(&id, &accountID, &email, &idToken, &accessToken, &refreshToken, &expire, &planType, &lastRefresh, &status, &cooldownUntil, &disableReason, &lastUsedAt); err != nil {
			log.Warnf("读取数据库账号失败: %v", err)
			continue
		}
		if acc, ok := accountFromDBRow(id, accountID, email, idToken, accessToken, refreshToken, expire, planType, lastRefresh, status, cooldownUntil, disableReason, lastUsedAt); ok {
			out = append(out, acc)
		}
	}
	return out, rowCount, rows.Err()
}

func (m *Manager) loadAccountsFromDB() error {
	if m.db == nil {
		return nil
	}
	rows, err := m.db.Query(`SELECT id, account_id,email,id_token,access_token,refresh_token,expire,plan_type,last_refresh,status,cooldown_until,disable_reason,last_used_at FROM codex_accounts ORDER BY id`)
	if err != nil {
		return err
	}
	defer rows.Close()

	accounts := make([]*Account, 0)
	index := make(map[string]*Account)

	for rows.Next() {
		var id int64
		var accountID, email, idToken, accessToken, refreshToken, expire, planType, disableReason sql.NullString
		var lastRefresh, cooldownUntil, lastUsedAt sql.NullTime
		var status sql.NullInt32
		if err := rows.Scan(&id, &accountID, &email, &idToken, &accessToken, &refreshToken, &expire, &planType, &lastRefresh, &status, &cooldownUntil, &disableReason, &lastUsedAt); err != nil {
			log.Warnf("读取数据库账号失败: %v", err)
			continue
		}
		acc, ok := accountFromDBRow(id, accountID, email, idToken, accessToken, refreshToken, expire, planType, lastRefresh, status, cooldownUntil, disableReason, lastUsedAt)
		if !ok {
			continue
		}
		accounts = append(accounts, acc)
		index[acc.FilePath] = acc
	}

	if err := rows.Err(); err != nil {
		return err
	}

	m.accounts = accounts
	m.accountIndex = index
	return nil
}

func (m *Manager) importAccountsFromFilesToDB() (int, error) {
	if m.db == nil {
		return 0, nil
	}

	m.importMu.Lock()
	defer m.importMu.Unlock()

	entries, err := os.ReadDir(m.authDir)
	if err != nil {
		return 0, err
	}

	importedCount := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".json") {
			continue
		}
		filePath := filepath.Join(m.authDir, entry.Name())
		acc, loadErr := loadAccountFromFile(filePath)
		if loadErr != nil {
			log.Warnf("导入账号文件失败 [%s]: %v", entry.Name(), loadErr)
			continue
		}

		exists, err := m.accountExists(acc)
		if err != nil {
			log.Warnf("检查账号是否存在失败 [%s]: %v", acc.GetEmail(), err)
			continue
		}
		if exists {
			log.Infof("账号已存在，跳过导入: %s", acc.GetEmail())
			// 同样清理原文件，避免重复导入
			if err := os.Remove(filePath); err != nil {
				log.Warnf("删除重复 JSON账号文件失败 [%s]: %v", filePath, err)
			}
			continue
		}

		if err := m.saveTokenToDB(acc); err != nil {
			log.Warnf("导入账号到 DB 失败 [%s]: %v", acc.GetEmail(), err)
			continue
		}
		importedCount++
		// 成功写入数据库后删除本地 JSON 文件
		if err := os.Remove(filePath); err != nil {
			log.Warnf("删除已导入 JSON账号文件失败 [%s]: %v", filePath, err)
		} else {
			log.Infof("已删除已导入 JSON账号文件: %s", filePath)
		}
	}
	return importedCount, nil
}

func (m *Manager) saveTokenToDB(acc *Account) error {
	if m.db == nil {
		return nil
	}

	acc.mu.RLock()
	/* 读取运行时状态：Status、CooldownUntil、DisableReason、LastUsedAt */
	status := int(acc.Status)
	cooldownUntil := acc.CooldownUntil
	disableReason := acc.DisableReason
	lastUsedAt := acc.LastUsedAt
	args := []any{
		acc.Token.AccountID,
		acc.Token.Email,
		acc.Token.IDToken,
		acc.Token.AccessToken,
		acc.Token.RefreshToken,
		acc.Token.Expire,
		acc.Token.PlanType,
		acc.LastRefreshedAt,
		status,
		cooldownUntil,
		disableReason,
		lastUsedAt,
	}
	acc.mu.RUnlock()

	aid := strings.TrimSpace(acc.Token.AccountID)
	em := strings.TrimSpace(acc.Token.Email)

	if m.dbDialect == codexdb.DialectMySQL && m.saveTokenStmt != nil {
		_, err := m.saveTokenStmt.Exec(args...)
		return err
	}
	if aid != "" && m.saveTokenStmt != nil {
		_, err := m.saveTokenStmt.Exec(args...)
		return err
	}
	if aid == "" && em != "" && m.saveTokenStmtByEmail != nil {
		_, err := m.saveTokenStmtByEmail.Exec(args...)
		return err
	}
	if m.saveTokenStmt != nil {
		_, err := m.saveTokenStmt.Exec(args...)
		return err
	}

	switch m.dbDialect {
	case codexdb.DialectMySQL:
		_, err := m.db.Exec(`
INSERT INTO codex_accounts (account_id,email,id_token,access_token,refresh_token,expire,plan_type,last_refresh,status,cooldown_until,disable_reason,last_used_at,updated_at)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,UTC_TIMESTAMP(6))
ON DUPLICATE KEY UPDATE
	email = VALUES(email),
	account_id = VALUES(account_id),
	id_token = VALUES(id_token),
	access_token = VALUES(access_token),
	refresh_token = VALUES(refresh_token),
	expire = VALUES(expire),
	plan_type = VALUES(plan_type),
	last_refresh = VALUES(last_refresh),
	status = VALUES(status),
	cooldown_until = VALUES(cooldown_until),
	disable_reason = VALUES(disable_reason),
	last_used_at = VALUES(last_used_at),
	updated_at = VALUES(updated_at)`, args...)
		return err
	case codexdb.DialectSQLite:
		_, err := m.db.Exec(`
INSERT INTO codex_accounts (account_id,email,id_token,access_token,refresh_token,expire,plan_type,last_refresh,status,cooldown_until,disable_reason,last_used_at,updated_at)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,CURRENT_TIMESTAMP)
ON CONFLICT(account_id) DO UPDATE SET
	email = excluded.email,
	id_token = excluded.id_token,
	access_token = excluded.access_token,
	refresh_token = excluded.refresh_token,
	expire = excluded.expire,
	plan_type = excluded.plan_type,
	last_refresh = excluded.last_refresh,
	status = excluded.status,
	cooldown_until = excluded.cooldown_until,
	disable_reason = excluded.disable_reason,
	last_used_at = excluded.last_used_at,
	updated_at = CURRENT_TIMESTAMP`, args...)
		return err
	default:
		_, err := m.db.Exec(`
INSERT INTO codex_accounts (account_id,email,id_token,access_token,refresh_token,expire,plan_type,last_refresh,status,cooldown_until,disable_reason,last_used_at,updated_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,NOW())
ON CONFLICT (account_id) DO UPDATE SET
	email = EXCLUDED.email,
	id_token = EXCLUDED.id_token,
	access_token = EXCLUDED.access_token,
	refresh_token = EXCLUDED.refresh_token,
	expire = EXCLUDED.expire,
	plan_type = EXCLUDED.plan_type,
	last_refresh = EXCLUDED.last_refresh,
	status = EXCLUDED.status,
	cooldown_until = EXCLUDED.cooldown_until,
	disable_reason = EXCLUDED.disable_reason,
	last_used_at = EXCLUDED.last_used_at,
	updated_at = NOW()`, args...)
		return err
	}
}

func (m *Manager) accountExists(acc *Account) (bool, error) {
	if m.db == nil {
		return false, nil
	}
	email := strings.TrimSpace(acc.GetEmail())
	aid := strings.TrimSpace(acc.GetAccountID())
	if email != "" {
		var one int
		var err error
		if m.dbDialect == codexdb.DialectPostgres {
			err = m.db.QueryRow(`SELECT 1 FROM codex_accounts WHERE email=$1 LIMIT 1`, email).Scan(&one)
		} else {
			err = m.db.QueryRow(`SELECT 1 FROM codex_accounts WHERE email=? LIMIT 1`, email).Scan(&one)
		}
		if err == nil {
			return true, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return false, err
		}
	}
	if aid != "" {
		var one int
		var err error
		if m.dbDialect == codexdb.DialectPostgres {
			err = m.db.QueryRow(`SELECT 1 FROM codex_accounts WHERE account_id=$1 LIMIT 1`, aid).Scan(&one)
		} else {
			err = m.db.QueryRow(`SELECT 1 FROM codex_accounts WHERE account_id=? LIMIT 1`, aid).Scan(&one)
		}
		if err == nil {
			return true, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return false, err
		}
	}
	return false, nil
}

func (m *Manager) deleteAccountFromDB(acc *Account) error {
	if m.db == nil {
		return nil
	}
	var err error
	if m.dbDialect == codexdb.DialectPostgres {
		_, err = m.db.Exec(`DELETE FROM codex_accounts WHERE email=$1 OR account_id=$2`, acc.GetEmail(), acc.GetAccountID())
	} else {
		_, err = m.db.Exec(`DELETE FROM codex_accounts WHERE email=? OR account_id=?`, acc.GetEmail(), acc.GetAccountID())
	}
	return err
}

/**
 * Pick 选择下一个可用账号（委托给选择器）
 * @param model - 请求的模型名称
 * @returns *Account - 选中的账号
 * @returns error - 没有可用账号时返回错误
 */
func (m *Manager) Pick(model string) (*Account, error) {
	/* 原子指针读取，零锁 */
	accounts := *m.accountsPtr.Load()
	return m.selector.Pick(model, accounts)
}

/**
 * PickExcluding 选择下一个可用账号，排除已用过的账号；优先在活跃账号子集中选号，失败再回退全量排除列表。
 */
func (m *Manager) PickExcluding(model string, excluded map[string]bool) (*Account, error) {
	/* 原子指针读取，零锁 */
	allAccounts := *m.accountsPtr.Load()
	if len(excluded) == 0 {
		return m.selector.Pick(model, allAccounts)
	}

	capGuess := len(allAccounts) - len(excluded)
	if capGuess < 0 {
		capGuess = 0
	}
	nowMs := time.Now().UnixMilli()
	filtered := make([]*Account, 0, capGuess)
	activeOnly := make([]*Account, 0, capGuess)
	for _, acc := range allAccounts {
		if excluded[acc.FilePath] {
			continue
		}
		filtered = append(filtered, acc)
		if accountPickableAt(nowMs, acc) {
			activeOnly = append(activeOnly, acc)
		}
	}

	if len(filtered) == 0 {
		return nil, fmt.Errorf("没有更多可用账号（已排除 %d 个）", len(excluded))
	}

	if len(activeOnly) > 0 {
		return m.selector.Pick(model, activeOnly)
	}
	return m.selector.Pick(model, filtered)
}

/**
 * PickRecentlySuccessful 回退选择：优先选最近成功且非排除的账号；所有都排除时清空排除列表重选正常账号
 */
func (m *Manager) PickRecentlySuccessful(model string, excluded map[string]bool) (*Account, error) {
	_ = model
	allAccounts := *m.accountsPtr.Load()
	/* 按 lastSuccessUnixMs 线性扫描，等价于原 sort 后取最近，避免 O(N log N) */
	var bestAll *Account
	var bestAllMs int64 = -1
	var bestOpen *Account
	var bestOpenMs int64 = -1
	for _, acc := range allAccounts {
		st := AccountStatus(acc.atomicStatus.Load())
		if st == StatusDisabled || st == StatusCooldown {
			continue
		}
		ms := acc.lastSuccessUnixMs.Load()
		if ms <= 0 {
			continue
		}
		fp := acc.FilePath
		if betterRecentSuccess(ms, fp, bestAllMs, bestAll) {
			bestAllMs = ms
			bestAll = acc
		}
		if excluded != nil && excluded[fp] {
			continue
		}
		if betterRecentSuccess(ms, fp, bestOpenMs, bestOpen) {
			bestOpenMs = ms
			bestOpen = acc
		}
	}

	if bestAll == nil {
		for _, acc := range allAccounts {
			st := AccountStatus(acc.atomicStatus.Load())
			if st == StatusDisabled || st == StatusCooldown {
				continue
			}
			return acc, nil
		}
		return nil, fmt.Errorf("没有可用账号")
	}
	if bestOpen != nil {
		return bestOpen, nil
	}
	return bestAll, nil
}

/* betterRecentSuccess 比较「更近的成功时间」，时间相同则 FilePath 较小者优先（与原 sort 稳定序一致） */
func betterRecentSuccess(ms int64, fp string, bestMs int64, best *Account) bool {
	if best == nil {
		return true
	}
	if ms != bestMs {
		return ms > bestMs
	}
	return fp < best.FilePath
}

/**
 * PickIgnoringCooldown 选号时忽略冷却状态，仅排除 Disabled 与 excluded 列表。
 * 用于 429 并发重试场景：所有账号可能均处于冷却，仍尝试发送。
 */
func (m *Manager) PickIgnoringCooldown(model string, excluded map[string]bool) (*Account, error) {
	allAccounts := *m.accountsPtr.Load()
	var candidates []*Account
	for _, acc := range allAccounts {
		if excluded[acc.FilePath] {
			continue
		}
		st := AccountStatus(acc.atomicStatus.Load())
		if st == StatusDisabled {
			continue
		}
		candidates = append(candidates, acc)
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("没有可用账号（忽略冷却）")
	}
	/* 轮询选取，简单取第一个候选 */
	return candidates[0], nil
}

/**
 * GetAccounts 获取所有账号的只读快照
 * @returns []*Account - 账号列表
 */
func (m *Manager) GetAccounts() []*Account {
	/* 原子快照是不可变的，可安全直接返回 */
	snap := *m.accountsPtr.Load()
	result := make([]*Account, len(snap))
	copy(result, snap)
	return result
}

/**
 * AccountCount 返回已加载的账号数量
 * @returns int - 账号数量
 */
func (m *Manager) AccountCount() int {
	return len(*m.accountsPtr.Load())
}

/**
 * AccountInPool 判断账号是否仍在号池（用于异步任务中途检测是否已移除）
 */
func (m *Manager) AccountInPool(acc *Account) bool {
	if acc == nil {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.accountIndex[acc.FilePath]
	return ok
}

type selectorCacheInvalidator interface {
	InvalidateAvailableCache()
}

func (m *Manager) InvalidateSelectorCache() {
	if inv, ok := m.selector.(selectorCacheInvalidator); ok {
		inv.InvalidateAvailableCache()
	}
}

/**
 * RemoveAccount 从号池和磁盘同时删除异常账号
 * 内存中移除 + 删除磁盘上的 JSON 文件，彻底清理
 * @param acc - 要移除的账号
 * @param reason - 移除原因
 */
func (m *Manager) RemoveAccount(acc *Account, reason string) {
	m.mu.Lock()

	filePath := acc.FilePath
	email := acc.GetEmail()

	if _, exists := m.accountIndex[filePath]; !exists {
		m.mu.Unlock()
		return
	}

	delete(m.accountIndex, filePath)

	/* 从切片中删除，用末尾覆盖法避免移动大量元素 */
	for i, a := range m.accounts {
		if a.FilePath == filePath {
			last := len(m.accounts) - 1
			m.accounts[i] = m.accounts[last]
			m.accounts = m.accounts[:last]
			break
		}
	}

	remaining := len(m.accounts)
	m.publishSnapshot()
	m.mu.Unlock()

	m.InvalidateSelectorCache()

	/* 删除持久化存储 */
	if m.db != nil {
		if err := m.deleteAccountFromDB(acc); err != nil {
			log.Errorf("账号 [%s] 数据库删除失败: %v", email, err)
		} else {
			log.Warnf("账号 [%s] 已删除（内存+数据库），原因: %s，剩余 %d 个", email, reason, remaining)
		}
	} else {
		if err := os.Remove(filePath); err != nil && !os.IsNotExist(err) {
			log.Errorf("账号 [%s] 磁盘文件删除失败: %v", email, err)
		} else {
			log.Warnf("账号 [%s] 已删除（内存+磁盘），原因: %s，剩余 %d 个", email, reason, remaining)
		}
	}
}

/**
 * DisableAccountByRenamingFile 从号池移除账号；磁盘模式将 JSON 重命名为 *.json.disabled（不再加载），数据库模式等同删除库记录
 */
func (m *Manager) DisableAccountByRenamingFile(acc *Account, reason string) {
	if acc == nil {
		return
	}
	m.mu.Lock()
	filePath := acc.FilePath
	email := acc.GetEmail()
	if _, exists := m.accountIndex[filePath]; !exists {
		m.mu.Unlock()
		return
	}
	delete(m.accountIndex, filePath)
	for i, a := range m.accounts {
		if a.FilePath == filePath {
			last := len(m.accounts) - 1
			m.accounts[i] = m.accounts[last]
			m.accounts = m.accounts[:last]
			break
		}
	}
	remaining := len(m.accounts)
	m.publishSnapshot()
	m.mu.Unlock()
	m.InvalidateSelectorCache()

	if m.db != nil {
		if err := m.deleteAccountFromDB(acc); err != nil {
			log.Errorf("账号 [%s] 禁用（数据库删除）失败: %v", email, err)
		} else {
			log.Warnf("账号 [%s] 已从号池移除（数据库），原因: %s，剩余 %d 个", email, reason, remaining)
		}
		return
	}

	dest, err := nextDisabledRenamePath(filePath)
	if err != nil {
		log.Errorf("账号 [%s] 生成禁用文件名失败: %v，改为删除原文件", email, err)
		if err := os.Remove(filePath); err != nil && !os.IsNotExist(err) {
			log.Errorf("账号 [%s] 删除凭据文件失败: %v", email, err)
		}
		return
	}
	if err := os.Rename(filePath, dest); err != nil {
		log.Errorf("账号 [%s] 禁用重命名失败: %v，尝试删除", email, err)
		if err := os.Remove(filePath); err != nil && !os.IsNotExist(err) {
			log.Errorf("账号 [%s] 删除凭据文件失败: %v", email, err)
		}
		return
	}
	log.Warnf("账号 [%s] 已禁用: %s -> %s，原因: %s，剩余 %d 个", email, filePath, dest, reason, remaining)
}

func nextDisabledRenamePath(filePath string) (string, error) {
	base := filePath + ".disabled"
	for i := 0; i < 256; i++ {
		candidate := base
		if i > 0 {
			candidate = fmt.Sprintf("%s.%d", base, i)
		}
		_, err := os.Stat(candidate)
		if os.IsNotExist(err) {
			return candidate, nil
		}
		if err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("exhausted disabled rename suffixes")
}

func (m *Manager) cooldownDurationForHTTPStatus(code int) time.Duration {
	switch code {
	case 429:
		return time.Duration(m.cooldown429Sec) * time.Second
	case 401:
		return time.Duration(m.cooldown401Sec) * time.Second
	default:
		return time.Duration(m.cooldown429Sec) * time.Second
	}
}

func refreshFailureReasonCode(code int) string {
	if code == 429 {
		return ReasonRefresh429
	}
	return fmt.Sprintf("refresh_http_%d", code)
}

func quotaFailureReasonCode(code int) string {
	if code == 429 {
		return ReasonQuotaHTTP429
	}
	return fmt.Sprintf("quota_http_%d", code)
}

func (m *Manager) trySingleRefreshToken(ctx context.Context, acc *Account) (*TokenData, error) {
	acc.mu.RLock()
	rt := acc.Token.RefreshToken
	acc.mu.RUnlock()
	if rt == "" {
		return nil, fmt.Errorf("missing refresh_token")
	}
	return m.refresher.RefreshToken(ctx, rt)
}

func (m *Manager) applyFinalHTTPRefresh(acc *Account, email, reason string, final HTTPErrorAction, httpStatus int) QuotaApplyOutcome {
	switch final {
	case HTTPErrorActionRemove:
		m.RemoveAccount(acc, reason)
		return QuotaApplyRemoved
	case HTTPErrorActionDisable:
		m.DisableAccountByRenamingFile(acc, reason)
		return QuotaApplyDisabled
	default:
		if httpStatus == 429 {
			acc.SetCooldown(time.Duration(m.cooldown429Sec) * time.Second)
		} else {
			acc.SetCooldown(time.Duration(m.cooldown401Sec) * time.Second)
		}
		m.InvalidateSelectorCache()
		/* 将冷却状态保存到数据库（数据库模式） */
		if m.db != nil {
			m.enqueueSave(acc)
		}
		log.Warnf("账号 [%s] 刷新 HTTP %d 按策略冷却", email, httpStatus)
		return QuotaApplyCooldown
	}
}

func (m *Manager) applyFinalHTTPQuota(acc *Account, reason string, final HTTPErrorAction, httpStatus int) QuotaApplyOutcome {
	switch final {
	case HTTPErrorActionRemove:
		m.RemoveAccount(acc, reason)
		return QuotaApplyRemoved
	case HTTPErrorActionDisable:
		m.DisableAccountByRenamingFile(acc, reason)
		return QuotaApplyDisabled
	default:
		if httpStatus == 429 {
			acc.SetQuotaCooldown(time.Duration(m.cooldown429Sec) * time.Second)
		} else {
			acc.SetCooldown(time.Duration(m.cooldown401Sec) * time.Second)
		}
		m.InvalidateSelectorCache()
		/* 将冷却状态保存到数据库（数据库模式） */
		if m.db != nil {
			m.enqueueSave(acc)
		}
		return QuotaApplyCooldown
	}
}

func (m *Manager) runRefreshStatusPolicy(ctx context.Context, acc *Account, email string, httpStatus int, pol httpStatusPolicy) (recovered bool, outcome QuotaApplyOutcome) {
	reason := refreshFailureReasonCode(httpStatus)
	doFinal := func() QuotaApplyOutcome {
		return m.applyFinalHTTPRefresh(acc, email, reason, pol.final, httpStatus)
	}
	switch pol.phase {
	case policyPhaseNone:
		return false, doFinal()
	case policyPhaseRefreshOnce:
		td, err := m.trySingleRefreshToken(ctx, acc)
		if err == nil {
			acc.UpdateToken(*td)
			if err := m.saveTokenToFile(acc); err != nil {
				log.Errorf("账号 [%s] 刷新重试成功后持久化失败: %v", email, err)
			}
			m.enqueueSave(acc)
			m.InvalidateSelectorCache()
			log.Infof("账号 [%s] 刷新遇 %d 后立刻重试 token 成功", email, httpStatus)
			return true, QuotaApplyNone
		}
		st2, ok := RefreshHTTPStatusFromErr(err)
		if ok && st2 == httpStatus {
			return false, doFinal()
		}
		log.Warnf("账号 [%s] 刷新重试后错误变化 (%d): %v", email, httpStatus, err)
		m.RemoveAccount(acc, ReasonRefreshFailed)
		return false, QuotaApplyRemoved
	case policyPhaseCooldownThenRetry:
		cd := m.cooldownDurationForHTTPStatus(httpStatus)
		log.Warnf("账号 [%s] 刷新 HTTP %d，等待 %v 后再试一次", email, httpStatus, cd)
		select {
		case <-time.After(cd):
		case <-ctx.Done():
			return false, QuotaApplyNone
		}
		td, err := m.trySingleRefreshToken(ctx, acc)
		if err == nil {
			acc.UpdateToken(*td)
			if err := m.saveTokenToFile(acc); err != nil {
				log.Errorf("账号 [%s] 冷却后再刷新成功但持久化失败: %v", email, err)
			}
			m.enqueueSave(acc)
			m.InvalidateSelectorCache()
			log.Infof("账号 [%s] 冷却后再刷新成功", email)
			return true, QuotaApplyNone
		}
		st2, ok := RefreshHTTPStatusFromErr(err)
		if ok && st2 == httpStatus {
			return false, doFinal()
		}
		log.Warnf("账号 [%s] 冷却后再刷新仍失败: %v", email, err)
		m.RemoveAccount(acc, ReasonRefreshFailed)
		return false, QuotaApplyRemoved
	default:
		return false, doFinal()
	}
}

/**
 * handleRefreshHTTPError 处理 OAuth 刷新失败（带 RefreshError 状态码时走 refresh-http-status-policy）
 * noPolicyRemove：未配置该状态码时 true=删号（强制刷新周期），false=禁用凭据（401 恢复路径）
 */
func (m *Manager) handleRefreshHTTPError(ctx context.Context, acc *Account, email string, err error, noPolicyRemove bool) (recovered bool, outcome QuotaApplyOutcome) {
	status, ok := RefreshHTTPStatusFromErr(err)
	if !ok {
		log.Warnf("账号 [%s] 刷新失败: %v", email, err)
		if m.disableAuth401Remove {
			log.Warnf("账号 [%s] 已配置不删号/不冷却，保留原状态", email)
			return false, QuotaApplyCooldown
		}
		if noPolicyRemove {
			m.RemoveAccount(acc, ReasonRefreshFailed)
			return false, QuotaApplyRemoved
		}
		m.DisableAccountByRenamingFile(acc, ReasonAuth401Disabled)
		return false, QuotaApplyDisabled
	}
	pol, has := m.refreshHTTPPolicy[status]
	if !has {
		if m.disableAuth401Remove {
			log.Warnf("账号 [%s] 刷新 HTTP %d 无策略，已配置不删号/不冷却，保留原状态", email, status)
			return false, QuotaApplyCooldown
		}
		if noPolicyRemove {
			log.Warnf("账号 [%s] 刷新 HTTP %d 无策略，移除", email, status)
			m.RemoveAccount(acc, ReasonRefreshFailed)
			return false, QuotaApplyRemoved
		}
		log.Warnf("账号 [%s] 刷新 HTTP %d 无策略，禁用凭据", email, status)
		m.DisableAccountByRenamingFile(acc, ReasonAuth401Disabled)
		return false, QuotaApplyDisabled
	}
	return m.runRefreshStatusPolicy(ctx, acc, email, status, pol)
}

func (m *Manager) runQuotaStatusPolicy(ctx context.Context, qc *QuotaChecker, acc *Account, httpStatus int, verdict int, pol httpStatusPolicy) QuotaApplyOutcome {
	if qc == nil {
		return m.applyQuotaUsageHTTPLegacy(acc, httpStatus, verdict)
	}
	reason := quotaFailureReasonCode(httpStatus)
	doFinal := func() QuotaApplyOutcome {
		return m.applyFinalHTTPQuota(acc, reason, pol.final, httpStatus)
	}
	switch pol.phase {
	case policyPhaseNone:
		return doFinal()
	case policyPhaseRefreshOnce:
		td, err := m.trySingleRefreshToken(ctx, acc)
		if err == nil {
			acc.UpdateToken(*td)
			if err := m.saveTokenToFile(acc); err != nil {
				log.Errorf("账号 [%s] 额度侧刷新重试成功后持久化失败: %v", acc.GetEmail(), err)
			}
			m.enqueueSave(acc)
		}
		v2, st2 := qc.checkAccount(ctx, acc)
		if v2 == 1 {
			m.InvalidateSelectorCache()
			return QuotaApplyNone
		}
		if st2 == httpStatus {
			return doFinal()
		}
		return m.applyQuotaUsageHTTPLegacy(acc, st2, v2)
	case policyPhaseCooldownThenRetry:
		cd := m.cooldownDurationForHTTPStatus(httpStatus)
		log.Warnf("账号 [%s] 额度查询 HTTP %d，等待 %v 后再查一次", acc.GetEmail(), httpStatus, cd)
		select {
		case <-time.After(cd):
		case <-ctx.Done():
			return QuotaApplyNone
		}
		v2, st2 := qc.checkAccount(ctx, acc)
		if v2 == 1 {
			m.InvalidateSelectorCache()
			return QuotaApplyNone
		}
		if st2 == httpStatus {
			return doFinal()
		}
		return m.applyQuotaUsageHTTPLegacy(acc, st2, v2)
	default:
		return doFinal()
	}
}

/**
 * applyQuotaUsageHTTPLegacy 未在 quota-http-status-policy 中配置的状态码：429→冷却兜底，其它 4xx→删号
 */
func (m *Manager) applyQuotaUsageHTTPLegacy(acc *Account, httpStatus int, verdict int) QuotaApplyOutcome {
	if acc == nil || verdict == 1 || verdict == 0 {
		return QuotaApplyNone
	}
	reason := quotaFailureReasonCode(httpStatus)
	if verdict == 2 {
		return m.applyFinalHTTPQuota(acc, reason, HTTPErrorActionCooldown, httpStatus)
	}
	m.RemoveAccount(acc, reason)
	return QuotaApplyRemoved
}

/**
 * ApplyQuotaUsageHTTPOutcome 按状态码策略处理额度 wham/usage 非成功响应（ctx 用于取消等待；qc 可为 nil 则退化为 legacy）
 * verdict：1=成功，0=暂态，-1=其它 4xx，2=HTTP 429
 */
func (m *Manager) ApplyQuotaUsageHTTPOutcome(ctx context.Context, qc *QuotaChecker, acc *Account, httpStatus int, verdict int) QuotaApplyOutcome {
	if ctx == nil {
		ctx = context.Background()
	}
	if acc == nil || verdict == 1 || verdict == 0 {
		return QuotaApplyNone
	}
	if pol, ok := m.quotaHTTPPolicy[httpStatus]; ok {
		return m.runQuotaStatusPolicy(ctx, qc, acc, httpStatus, verdict, pol)
	}
	return m.applyQuotaUsageHTTPLegacy(acc, httpStatus, verdict)
}

/**
 * SetPostRefreshQuotaChecker 设置「刷新成功后」用于校验 wham/usage 的查询器；nil 表示关闭该校验
 */
func (m *Manager) SetPostRefreshQuotaChecker(qc *QuotaChecker) {
	if m == nil {
		return
	}
	m.postRefreshQuota.Store(qc)
}

func (m *Manager) effectiveQuotaAfterRefresh(qcArg *QuotaChecker) *QuotaChecker {
	if qcArg != nil {
		return qcArg
	}
	if m == nil {
		return nil
	}
	return m.postRefreshQuota.Load()
}

/**
 * afterRefreshValidateQuota OAuth 刷新成功后同步查额度（401 恢复路径）；4xx 无效（非 429）删号。返回 false 表示已 RemoveAccount。
 */
func (m *Manager) afterRefreshValidateQuota(ctx context.Context, qc *QuotaChecker, acc *Account) bool {
	if qc == nil || acc == nil {
		return true
	}
	if !acc.HasRefreshToken() {
		return true
	}
	return m.applyPostRefreshQuotaOutcome(ctx, qc, acc, false)
}

/* applyPostRefreshQuotaOutcome 根据 wham/usage 结果处理账号；asyncLog 为 true 时用「异步」文案 */
func (m *Manager) applyPostRefreshQuotaOutcome(ctx context.Context, qc *QuotaChecker, acc *Account, asyncLog bool) bool {
	verdict, st := qc.CheckAccountResultWithStatus(ctx, acc)
	switch verdict {
	case 1:
		acc.RefreshUsedPercent()
		return true
	case 2:
		_ = m.ApplyQuotaUsageHTTPOutcome(ctx, qc, acc, st, verdict)
		return true
	case -1:
		if asyncLog {
			log.Warnf("账号 [%s] OAuth 刷新成功但异步额度查询无效 (HTTP %d)，视为凭据无效已删除", acc.GetEmail(), st)
		} else {
			log.Warnf("账号 [%s] OAuth 刷新成功但额度查询无效 (HTTP %d)，视为凭据无效已删除", acc.GetEmail(), st)
		}
		m.RemoveAccount(acc, ReasonQuotaInvalidAfterRefresh)
		return false
	default:
		if asyncLog {
			log.Debugf("账号 [%s] 刷新后异步额度查询暂态失败 (HTTP %d)，保留账号", acc.GetEmail(), st)
		} else {
			log.Debugf("账号 [%s] 刷新后额度查询暂态失败 (HTTP %d)，保留账号", acc.GetEmail(), st)
		}
		return true
	}
}

/**
 * StartRefreshLoop 启动后台 Token 刷新循环
 * 每个周期：先扫描新增文件 → 再并发刷新所有账号
 * @param ctx - 上下文，用于控制生命周期
 */
func (m *Manager) StartRefreshLoop(ctx context.Context) {
	refreshInterval := time.Duration(m.refreshInterval) * time.Second
	refreshTicker := time.NewTicker(refreshInterval)
	defer refreshTicker.Stop()

	/* 热加载扫描间隔（比刷新更频繁） */
	scanInterval := time.Duration(m.scanIntervalSec) * time.Second
	if scanInterval > refreshInterval {
		scanInterval = refreshInterval
	}
	scanTicker := time.NewTicker(scanInterval)
	defer scanTicker.Stop()

	/* 启动时立即执行一次刷新 */
	m.refreshAllAccountsConcurrent(ctx)

	for {
		select {
		case <-ctx.Done():
			log.Info("账号刷新循环已停止")
			return
		case <-m.stopCh:
			log.Info("账号刷新循环已停止")
			return
		case <-scanTicker.C:
			/* 定时扫描 auth 目录，热加载新增文件 */
			m.scanNewFiles()
		case <-refreshTicker.C:
			m.scanNewFiles()
			m.refreshAllAccountsConcurrent(ctx)
		}
	}
}

/**
 * Stop 停止刷新循环
 */
func (m *Manager) Stop() {
	close(m.stopCh)
}

/**
 * publishSnapshot 将当前 accounts 切片发布为原子快照
 * 必须在持有 m.mu 写锁时调用
 */
func (m *Manager) publishSnapshot() {
	snap := make([]*Account, len(m.accounts))
	copy(snap, m.accounts)
	m.accountsPtr.Store(&snap)
}

/**
 * StartSaveWorker 启动异步磁盘写入工作器
 * 从 saveQueue 中消费账号，批量将 Token 写入磁盘
 * 将磁盘 IO 从刷新 goroutine 中解耦，避免阻塞并发刷新
 * @param ctx - 上下文，用于控制生命周期
 */
func (m *Manager) StartSaveWorker(ctx context.Context) {
	/* 启动多个写入 goroutine 并行消费队列，加速 2w+ 账号的磁盘写入 */
	n := m.saveWorkers
	if n < 1 {
		n = defaultSaveWorkers
	}
	for i := 0; i < n; i++ {
		go func() {
			for {
				select {
				case <-ctx.Done():
					/* 退出前排空队列 */
					for {
						select {
						case acc := <-m.saveQueue:
							_ = m.saveTokenToFile(acc)
						default:
							return
						}
					}
				case acc := <-m.saveQueue:
					if err := m.saveTokenToFile(acc); err != nil {
						log.Errorf("异步保存 Token 失败 [%s]: %v", acc.GetEmail(), err)
					}
				}
			}
		}()
	}
}

/**
 * enqueueSave 将账号加入异步磁盘写入队列
 * 非阻塞：队列满时丢弃（下次刷新会重新写入）
 * @param acc - 要保存的账号
 */
func (m *Manager) enqueueSave(acc *Account) {
	select {
	case m.saveQueue <- acc:
	default:
		/* 队列满，跳过此次写入，不阻塞刷新 goroutine */
		log.Debugf("磁盘写入队列已满，跳过 [%s]", acc.GetEmail())
	}
}

/**
 * scanNewFiles 扫描 auth 目录，加载新增的账号文件到号池
 * 已存在的文件不会重复加载，已被移除的也不会重新加入（直到文件变更）
 */
func (m *Manager) scanNewFiles() {
	if m.db != nil {
		if m.authDir == "" {
			return
		}
		// 数据库模式下，也要扫描目录并将 JSON 导入数据库
		// 导入过程涉及磁盘和数据库 IO，在锁外执行
		count, err := m.importAccountsFromFilesToDB()
		if err != nil {
			log.Warnf("热加载: 导入 JSON 文件到数据库失败: %v", err)
			return
		}
		if count > 0 {
			m.mu.Lock()
			if err := m.loadAccountsFromDB(); err != nil {
				m.mu.Unlock()
				log.Warnf("热加载: 重新加载数据库账号失败: %v", err)
				return
			}
			m.publishSnapshot()
			m.mu.Unlock()
			log.Infof("热加载: 已将 %d 个新增 JSON 文件导入数据库，当前总计 %d 个", count, m.AccountCount())
		}
		return
	}
	entries, err := os.ReadDir(m.authDir)
	if err != nil {
		log.Warnf("扫描账号目录失败: %v", err)
		return
	}

	/* 第一阶段：在读锁下快速过滤出未加载的文件路径 */
	m.mu.RLock()
	var candidates []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".json") {
			continue
		}
		filePath := filepath.Join(m.authDir, entry.Name())
		if _, exists := m.accountIndex[filePath]; !exists {
			candidates = append(candidates, filePath)
		}
	}
	m.mu.RUnlock()

	if len(candidates) == 0 {
		return
	}

	/* 第二阶段：在锁外加载所有新文件（IO 密集，不持锁） */
	type newEntry struct {
		path string
		acc  *Account
	}
	loaded := make([]newEntry, 0, len(candidates))
	for _, filePath := range candidates {
		acc, loadErr := loadAccountFromFile(filePath)
		if loadErr != nil {
			continue
		}
		loaded = append(loaded, newEntry{path: filePath, acc: acc})
	}

	if len(loaded) == 0 {
		return
	}

	/* 第三阶段：一次性写锁批量写入（双检查防并发） */
	m.mu.Lock()
	newCount := 0
	for _, entry := range loaded {
		if _, exists := m.accountIndex[entry.path]; !exists {
			m.accounts = append(m.accounts, entry.acc)
			m.accountIndex[entry.path] = entry.acc
			newCount++
		}
	}
	if newCount > 0 {
		m.publishSnapshot()
	}
	m.mu.Unlock()

	if newCount > 0 {
		log.Infof("热加载: 新增 %d 个账号，当前总计 %d 个", newCount, m.AccountCount())
	}
}

/**
 * refreshAllAccountsConcurrent 并发刷新所有账号的 Token
 * 使用 goroutine pool 控制并发数，支持 2w+ 账号高效刷新
 * @param ctx - 上下文
 */
func (m *Manager) refreshAllAccountsConcurrent(ctx context.Context) {
	/* 使用原子快照，零锁 */
	accounts := *m.accountsPtr.Load()
	if len(accounts) == 0 {
		return
	}

	/* 先过滤出需要刷新的账号，避免为不需要刷新的账号创建 goroutine */
	needRefresh := m.filterNeedRefresh(accounts)

	start := time.Now()
	log.Infof("开始并发刷新: 总 %d 个账号，需刷新 %d 个（并发 %d）",
		len(accounts), len(needRefresh), m.refreshConcurrency)

	if len(needRefresh) == 0 {
		log.Info("所有账号 Token 均有效，跳过刷新")
		return
	}

	batchSize := m.refreshBatchSize
	if batchSize <= 0 {
		batchSize = len(needRefresh)
	}
	sem := make(chan struct{}, m.refreshConcurrency)
	var wg sync.WaitGroup

	for i := 0; i < len(needRefresh); i += batchSize {
		if ctx.Err() != nil {
			break
		}
		end := i + batchSize
		if end > len(needRefresh) {
			end = len(needRefresh)
		}
		batch := needRefresh[i:end]
		for _, acc := range batch {
			if ctx.Err() != nil {
				break
			}
			wg.Add(1)
			sem <- struct{}{}
			go func(a *Account) {
				defer wg.Done()
				defer func() { <-sem }()
				m.refreshAccount(ctx, a)
			}(acc)
		}
		wg.Wait()
	}
	log.Infof("刷新完成: 刷新 %d 个账号，耗时 %v，剩余 %d 个",
		len(needRefresh), time.Since(start).Round(time.Millisecond), m.AccountCount())
}

/**
 * filterNeedRefresh 过滤出需要刷新的账号
 * 跳过条件：
 *   - Token 还有 5 分钟以上有效期
 *   - 最近 60 秒内已经刷新过
 *   - 正在被其他 goroutine 刷新中
 * @param accounts - 全部账号列表
 * @returns []*Account - 需要刷新的账号列表
 */
func (m *Manager) filterNeedRefresh(accounts []*Account) []*Account {
	nowMs := time.Now().UnixMilli()
	result := make([]*Account, 0, len(accounts)/2)
	intervalMs := int64(m.refreshInterval) * 1000
	if intervalMs < 60_000 {
		intervalMs = 60_000
	}

	for _, acc := range accounts {
		/* 正在刷新中，跳过 */
		if acc.refreshing.Load() != 0 {
			continue
		}

		/* 最近 60 秒内已刷新过，跳过 */
		if lastMs := acc.lastRefreshMs.Load(); lastMs > 0 && (nowMs-lastMs) < 60_000 {
			continue
		}

		/* 检查 Token 过期时间 */
		acc.mu.RLock()
		expire := acc.Token.Expire
		refreshToken := acc.Token.RefreshToken
		acc.mu.RUnlock()

		if refreshToken == "" {
			continue
		}

		tokenOK := false
		if expire != "" {
			if expireTime, parseErr := time.Parse(time.RFC3339, expire); parseErr == nil {
				tokenOK = time.Until(expireTime) > 5*time.Minute
			}
		}

		/* 即使 JWT 仍显示有效，超过 refreshInterval 未成功刷新也要纳入本轮，避免长期不刷导致调用时 401 */
		staleByInterval := false
		if lastMs := acc.lastRefreshMs.Load(); lastMs == 0 || (nowMs-lastMs) >= intervalMs {
			staleByInterval = true
		}

		if tokenOK && !staleByInterval {
			continue
		}

		result = append(result, acc)
	}

	return result
}

/**
 * ProgressEvent SSE 流式进度事件
 * @field Type - 事件类型：item（单条进度）/ done（完成汇总）
 * @field Email - 账号邮箱（item 类型时有值）
 * @field Success - 该条操作是否成功（item 类型时有值）
 * @field Message - 描述信息
 * @field Total - 总数（done 类型时有值）
 * @field SuccessCount - 成功数（done 类型时有值）
 * @field FailedCount - 失败数（done 类型时有值）
 * @field Remaining - 剩余数（done 类型时有值）
 * @field Duration - 耗时（done 类型时有值）
 * @field Current - 当前进度序号
 */
type ProgressEvent struct {
	Type         string `json:"type"`
	Email        string `json:"email,omitempty"`
	Success      *bool  `json:"success,omitempty"`
	Message      string `json:"message,omitempty"`
	Total        int    `json:"total,omitempty"`
	SuccessCount int    `json:"success_count,omitempty"`
	FailedCount  int    `json:"failed_count,omitempty"`
	Remaining    int    `json:"remaining,omitempty"`
	Duration     string `json:"duration,omitempty"`
	Current      int    `json:"current,omitempty"`
}

/**
 * ForceRefreshAllStream 强制刷新所有账号的 Token（SSE 流式返回进度）
 * 每刷新完一个账号就通过 channel 发送一个 ProgressEvent
 * @param ctx - 上下文
 * @returns <-chan ProgressEvent - 进度事件 channel
 */
func (m *Manager) ForceRefreshAllStream(ctx context.Context, quotaChecker *QuotaChecker) <-chan ProgressEvent {
	ch := make(chan ProgressEvent, 100)

	go func() {
		defer close(ch)

		/* 原子快照读取，零锁 */
		accounts := *m.accountsPtr.Load()

		total := len(accounts)
		if total == 0 {
			ch <- ProgressEvent{Type: "done", Message: "无账号", Duration: "0s"}
			return
		}

		start := time.Now()
		log.Infof("开始手动强制刷新 %d 个账号（并发 %d）", total, m.refreshConcurrency)

		for _, acc := range accounts {
			acc.SetActive()
			if m.db != nil {
				m.enqueueSave(acc)
			}
		}

		sem := make(chan struct{}, m.refreshConcurrency)
		var wg sync.WaitGroup
		var successCount, failCount, currentIdx atomic.Int64

		for _, acc := range accounts {
			if ctx.Err() != nil {
				break
			}

			wg.Add(1)
			sem <- struct{}{}

			go func(a *Account) {
				defer wg.Done()
				defer func() { <-sem }()

				ok := m.forceRefreshAccount(ctx, a)

				email := a.GetEmail()
				cur := int(currentIdx.Add(1))
				if ok {
					successCount.Add(1)
				} else {
					failCount.Add(1)
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

		remaining := m.AccountCount()
		sc := successCount.Load()
		fc := failCount.Load()
		elapsed := time.Since(start).Round(time.Millisecond)
		log.Infof("手动刷新完成: 成功 %d, 失败 %d, 耗时 %v, 剩余 %d 个",
			sc, fc, elapsed, remaining)

		ch <- ProgressEvent{
			Type:         "done",
			Message:      "刷新完成",
			Total:        total,
			SuccessCount: int(sc),
			FailedCount:  int(fc),
			Remaining:    remaining,
			Duration:     elapsed.String(),
		}
	}()

	return ch
}

/**
 * forceRefreshAccount 强制刷新单个账号的 Token（跳过过期检查）
 * @param ctx - 上下文
 * @param acc - 要刷新的账号
 * @returns bool - 刷新是否成功
 */
func (m *Manager) forceRefreshAccount(ctx context.Context, acc *Account) bool {
	/* CAS 去重：防止同一账号被多个刷新源同时刷新 */
	if !acc.refreshing.CompareAndSwap(0, 1) {
		log.Debugf("账号 [%s] 正在刷新中，跳过强制刷新", acc.GetEmail())
		return true /* 正在刷新中视为成功 */
	}
	defer acc.refreshing.Store(0)

	acc.mu.RLock()
	refreshToken := acc.Token.RefreshToken
	email := acc.Token.Email
	acc.mu.RUnlock()

	if refreshToken == "" {
		log.Warnf("账号 [%s] 缺少 refresh_token，移除", email)
		m.RemoveAccount(acc, "missing_refresh_token")
		return false
	}

	rctx, rcancel := m.refreshRequestContext(ctx)
	defer rcancel()
	td, err := m.refresher.RefreshTokenWithRetry(rctx, refreshToken, 3)
	if err != nil {
		rec, _ := m.handleRefreshHTTPError(rctx, acc, email, err, true)
		return rec
	}

	acc.UpdateToken(*td)
	m.enqueueSave(acc)
	log.Infof("账号 [%s] 刷新成功", acc.GetEmail())
	if qcEff := m.effectiveQuotaAfterRefresh(nil); qcEff != nil {
		qctx, qcancel := context.WithTimeout(ctx, 30*time.Second)
		if !m.afterRefreshValidateQuota(qctx, qcEff, acc) {
			qcancel()
			return false
		}
		qcancel()
	}
	return true
}

func mapRecover401FromRefreshOutcome(q QuotaApplyOutcome, email, fp string, refreshErr error) Auth401RecoverResult {
	r := Auth401RecoverResult{Email: email, FilePath: fp}
	switch q {
	case QuotaApplyRemoved:
		r.Status = Auth401RecoverRemoved
	case QuotaApplyDisabled:
		r.Status = Auth401RecoverDisabled
		r.ReasonCode = ReasonAuth401Disabled
	case QuotaApplyCooldown:
		r.Status = Auth401RecoverCooldown429OK
	default: /* QuotaApplyNone：如 ctx 取消导致策略未落地，勿误报为 429 额度正常 */
		r.Status = Auth401RecoverSkippedBusy
	}
	if refreshErr != nil {
		r.Detail = refreshErr.Error()
	} else if q == QuotaApplyNone {
		r.Detail = "refresh_outcome_none"
	}
	return r
}

/**
 * FindAccountByIdentifier 按邮箱或凭据文件路径（完整路径或仅文件名）查找号池中的账号
 */
func (m *Manager) FindAccountByIdentifier(email, filePath string) *Account {
	email = strings.TrimSpace(email)
	filePath = strings.TrimSpace(filePath)
	if email == "" && filePath == "" {
		return nil
	}
	accounts := m.GetAccounts()
	wantBase := ""
	if filePath != "" {
		wantBase = filepath.Base(filePath)
	}
	wantEmail := strings.ToLower(email)
	for _, a := range accounts {
		if filePath != "" {
			if a.FilePath == filePath || filepath.Base(a.FilePath) == wantBase {
				return a
			}
		}
		if email != "" && strings.ToLower(strings.TrimSpace(a.GetEmail())) == wantEmail {
			return a
		}
	}
	return nil
}

/**
 * RecoverAuth401 对指定账号执行 401 恢复：同步刷新 → 若 429 则查额度（qc 非空）→ 仍失败则禁用凭据文件
 * 同一凭据文件上并发的 RecoverAuth401 会合并为单次 OAuth（singleflight），避免多连接重复刷新刷屏。
 * ctx 为外层上限；单次 OAuth 另受 refresh-single-timeout-sec 约束（非对话 API）
 */
func (m *Manager) RecoverAuth401(ctx context.Context, acc *Account, qc *QuotaChecker) Auth401RecoverResult {
	if ctx == nil {
		ctx = context.Background()
	}
	if acc == nil {
		return Auth401RecoverResult{Status: Auth401RecoverInvalid, Detail: "account is nil"}
	}
	key := acc.FilePath
	if key == "" {
		key = acc.GetEmail()
	}
	v, _, _ := m.auth401SF.Do(key, func() (interface{}, error) {
		return m.recoverAuth401Once(ctx, acc, qc), nil
	})
	out := v.(Auth401RecoverResult)
	/* singleflight 只执行首个闭包的 recover；等待方 *Account 可能与执行方不同指针，需把已刷新的凭据同步到当前 acc */
	if out.Status == Auth401RecoverRefreshed {
		m.syncAccountTokenAfter401Flight(acc)
	}
	return out
}

/* syncAccountTokenAfter401Flight 若索引中的 canonical 账号已含更新后的 Token，合并到 caller（caller 非 canonical 时） */
func (m *Manager) syncAccountTokenAfter401Flight(caller *Account) {
	if caller == nil {
		return
	}
	fp := caller.FilePath
	if fp == "" {
		return
	}
	m.mu.RLock()
	src := m.accountIndex[fp]
	m.mu.RUnlock()
	if src == nil || src == caller {
		return
	}
	caller.UpdateToken(src.TokenSnapshot())
}

func (m *Manager) recoverAuth401Once(ctx context.Context, acc *Account, qc *QuotaChecker) Auth401RecoverResult {
	email := acc.GetEmail()
	fp := acc.FilePath
	out := Auth401RecoverResult{Email: email, FilePath: fp}

	if !acc.refreshing.CompareAndSwap(0, 1) {
		/* 与后台批量刷新并发时：等对方写完 Token 后本请求同号重试，避免刷新已成功却仍走换号/空解析 */
		log.Debugf("账号 [%s] 正在他处刷新 Token，401 恢复等待其完成…", email)
		if m.waitAccountRefreshIdle(ctx, acc) {
			out.Status = Auth401RecoverRefreshed
			out.Detail = "waited_peer_refresh"
			log.Debugf("账号 [%s] 401 恢复：已等待进行中的刷新结束，将用当前凭据重试上游", email)
			return out
		}
		out.Status = Auth401RecoverSkippedBusy
		out.Detail = "账号正在刷新中，等待超时"
		log.Warnf("账号 [%s] 401 恢复：等待他处刷新超时，跳过同号重试", email)
		return out
	}
	defer acc.refreshing.Store(0)

	acc.mu.RLock()
	refreshToken := strings.TrimSpace(acc.Token.RefreshToken)
	acc.mu.RUnlock()

	if refreshToken == "" {
		if m.disableAuth401Remove {
			log.Warnf("账号 [%s] 无 refresh_token，已配置不删号/不冷却，保留原状态", email)
			out.Status = Auth401RecoverSkippedBusy
			out.Detail = "missing refresh_token, kept (disable-auth-401-remove)"
			return out
		}
		log.Warnf("账号 [%s] 无 refresh_token（空或 null），401 触发已从号池删除", email)
		m.RemoveAccount(acc, ReasonAuth401NoRefreshToken)
		out.Status = Auth401RecoverRemoved
		out.ReasonCode = ReasonAuth401NoRefreshToken
		out.Detail = "missing refresh_token"
		return out
	}

	if m.auth401SyncSem != nil {
		select {
		case m.auth401SyncSem <- struct{}{}:
			defer func() { <-m.auth401SyncSem }()
		default:
			out.Status = Auth401RecoverSkippedBusy
			out.Detail = "sync_oauth_concurrency_full"
			log.Debugf("账号 [%s] 401 恢复：同步 OAuth 并发已满，换号重试（后台刷新仍会更新 Token）", email)
			return out
		}
	}

	log.Warnf("账号 [%s] 401 恢复：正在同步刷新 Token...", email)
	rctx, rcancel := m.refreshRequestContext(ctx)
	defer rcancel()
	td, err := m.refresher.RefreshTokenWithRetry(rctx, refreshToken, 3)
	if err == nil {
		acc.UpdateToken(*td)
		qcEff := m.effectiveQuotaAfterRefresh(qc)
		if !m.quotaPrecheck {
			qcEff = nil
		}
		if qcEff != nil && !m.afterRefreshValidateQuota(rctx, qcEff, acc) {
			out.Status = Auth401RecoverRemoved
			out.ReasonCode = ReasonQuotaInvalidAfterRefresh
			out.Detail = "刷新成功但额度接口判无效，已删号"
			m.InvalidateSelectorCache()
			return out
		}
		if err := m.saveTokenToFile(acc); err != nil {
			log.Errorf("账号 [%s] 401 刷新成功但持久化失败: %v", acc.GetEmail(), err)
			out.Detail = "persist error: " + err.Error()
		}
		acc.SetActive()
		m.enqueueSave(acc)
		m.InvalidateSelectorCache()
		log.Infof("账号 [%s] 401 后刷新成功，已恢复可用", acc.GetEmail())
		out.Status = Auth401RecoverRefreshed
		return out
	}

	recovered, qOut := m.handleRefreshHTTPError(rctx, acc, email, err, false)
	if recovered {
		qcEff := m.effectiveQuotaAfterRefresh(qc)
		if !m.quotaPrecheck {
			qcEff = nil
		}
		if qcEff != nil && !m.afterRefreshValidateQuota(rctx, qcEff, acc) {
			out.Status = Auth401RecoverRemoved
			out.ReasonCode = ReasonQuotaInvalidAfterRefresh
			out.Detail = "刷新恢复后额度接口判无效，已删号"
			m.InvalidateSelectorCache()
			if m.db != nil {
				m.enqueueSave(acc)
			}
			return out
		}
		acc.SetActive()
		m.InvalidateSelectorCache()
		out.Status = Auth401RecoverRefreshed
		return out
	}
	return mapRecover401FromRefreshOutcome(qOut, email, fp, err)
}

/**
 * ScheduleRecoverAfterAuth401 在后台对曾返回 401 的账号执行与 RecoverAuth401 相同的刷新与额度流程，不阻塞当前请求。
 * 供对话代理「先换号继续、异步救号」策略使用；与 RecoverAuth401 共享 singleflight，重复提交会合并。
 */
func (m *Manager) ScheduleRecoverAfterAuth401(acc *Account, qc *QuotaChecker) {
	if m == nil || acc == nil {
		return
	}
	a := acc
	q := qc
	go func() {
		timeoutSec := m.refreshSingleTimeoutSec
		if timeoutSec < 1 {
			timeoutSec = defaultRefreshSingleTimeoutSec
		}
		hctx, hcancel := context.WithTimeout(context.Background(), time.Duration(timeoutSec+35)*time.Second)
		defer hcancel()
		out := m.RecoverAuth401(hctx, a, q)
		switch out.Status {
		case Auth401RecoverRefreshed, Auth401RecoverCooldown429OK:
			log.Infof("异步 401 恢复成功: %s", a.GetEmail())
		default:
			log.Debugf("异步 401 恢复结束 [%s]: status=%s detail=%s", a.GetEmail(), out.Status, out.Detail)
		}
	}()
}

/**
 * HandleAuth401 同步刷新 401 账号（管理接口等需等待结果时使用）；对话路径请用 ScheduleRecoverAfterAuth401。
 * @param acc - 返回 401 的账号
 * @param qc - 额度查询器，可为 nil（此时刷新 429 视为无法复核，直接禁用）
 */
func (m *Manager) HandleAuth401(acc *Account, qc *QuotaChecker) Auth401RecoverResult {
	if acc == nil {
		return Auth401RecoverResult{Status: Auth401RecoverInvalid, Detail: "account is nil"}
	}

	timeoutSec := m.refreshSingleTimeoutSec
	if timeoutSec < 1 {
		timeoutSec = defaultRefreshSingleTimeoutSec
	}
	hctx, hcancel := context.WithTimeout(context.Background(), time.Duration(timeoutSec+35)*time.Second)
	defer hcancel()

	/* 同步等待 Token 刷新完成，获取最新状态 */
	return m.RecoverAuth401(hctx, acc, qc)
}

/**
 * ScheduleUpstream429Recovery 上游 429 后异步恢复：先立即查额度，未通过则指数退避重试（2m→5m→10m），每轮先刷新 token 再查额度
 */
func (m *Manager) ScheduleUpstream429Recovery(_ context.Context, acc *Account, qc *QuotaChecker) {
	if qc == nil || acc == nil {
		return
	}
	if !acc.HasRefreshToken() {
		return
	}
	if !acc.upstream429Recovering.CompareAndSwap(0, 1) {
		return
	}
	go func() {
		defer acc.upstream429Recovering.Store(0)
		email := acc.GetEmail()

		/* 立即查一次额度 */
		qctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		v1, st1 := qc.CheckAccountResultWithStatus(qctx, acc)
		cancel()
		if v1 == 1 {
			return
		}
		m.ApplyQuotaUsageHTTPOutcome(context.Background(), qc, acc, st1, v1)
		if !m.AccountInPool(acc) {
			return
		}
		if v1 != 0 {
			return
		}

		/* 指数退避重试：2min, 5min, 10min（取代原来的 1 小时固定等待） */
		backoffs := []time.Duration{2 * time.Minute, 5 * time.Minute, 10 * time.Minute}
		for round, wait := range backoffs {
			log.Warnf("账号 [%s] 上游 429 恢复：第 %d 轮等待 %v 后刷新凭证并重试", email, round+1, wait)

			select {
			case <-time.After(wait):
			case <-m.stopCh:
				return
			}

			if !m.AccountInPool(acc) {
				return
			}

			/* 刷新 token */
			if acc.refreshing.CompareAndSwap(0, 1) {
				func() {
					defer acc.refreshing.Store(0)
					acc.mu.RLock()
					rt := acc.Token.RefreshToken
					acc.mu.RUnlock()
					if rt == "" {
						m.RemoveAccount(acc, ReasonQuotaRecheckFailed)
						return
					}
					rctx, rcancel := m.refreshRequestContext(context.Background())
					defer rcancel()
					td, err := m.refresher.RefreshTokenWithRetry(rctx, rt, 3)
					if err != nil {
						_, _ = m.handleRefreshHTTPError(rctx, acc, acc.GetEmail(), err, true)
						return
					}
					acc.UpdateToken(*td)
					if err := m.saveTokenToFile(acc); err != nil {
						log.Errorf("账号 [%s] 429 恢复刷新后持久化失败: %v", acc.GetEmail(), err)
					}
					m.enqueueSave(acc)
				}()
			} else {
				log.Debugf("账号 [%s] 429 恢复：跳过刷新（他处正在刷新）", email)
			}

			if !m.AccountInPool(acc) {
				return
			}

			/* 查额度 */
			qctx2, cancel2 := context.WithTimeout(context.Background(), 25*time.Second)
			v2, st2 := qc.CheckAccountResultWithStatus(qctx2, acc)
			cancel2()
			if v2 == 1 {
				acc.SetActive()
				if m.db != nil {
					m.enqueueSave(acc)
				}
				m.InvalidateSelectorCache()
				log.Infof("账号 [%s] 429 恢复（第 %d 轮）：额度查询已通过，已恢复可用", acc.GetEmail(), round+1)
				return
			}
			if v2 != 0 {
				/* 非暂态失败（4xx 等），按策略处理后不再重试 */
				m.ApplyQuotaUsageHTTPOutcome(context.Background(), qc, acc, st2, v2)
				return
			}
		}
		/* 所有退避轮次耗尽 */
		log.Warnf("账号 [%s] 429 恢复：%d 轮退避后仍无法恢复", email, len(backoffs))
		qctxF, cancelF := context.WithTimeout(context.Background(), 25*time.Second)
		vF, stF := qc.CheckAccountResultWithStatus(qctxF, acc)
		cancelF()
		m.ApplyQuotaUsageHTTPOutcome(context.Background(), qc, acc, stF, vF)
	}()
}

/**
 * refreshAccount 刷新单个账号的 Token
 * 刷新失败时直接从号池移除该账号
 * 保存时使用原子写入，防止写入失败损坏原文件
 * @param ctx - 上下文
 * @param acc - 要刷新的账号
 */
func (m *Manager) refreshAccount(ctx context.Context, acc *Account) {
	/* CAS 去重：防止同一账号被多个刷新源同时刷新 */
	if !acc.refreshing.CompareAndSwap(0, 1) {
		log.Debugf("账号 [%s] 正在刷新中，跳过", acc.GetEmail())
		return
	}
	defer acc.refreshing.Store(0)

	acc.mu.RLock()
	refreshToken := acc.Token.RefreshToken
	email := acc.Token.Email
	acc.mu.RUnlock()

	if refreshToken == "" {
		log.Warnf("账号 [%s] 缺少 refresh_token，移除", email)
		m.RemoveAccount(acc, "missing_refresh_token")
		return
	}

	log.Debugf("正在刷新账号 [%s]", email)

	rctx, rcancel := m.refreshRequestContext(ctx)
	defer rcancel()
	td, err := m.refresher.RefreshTokenWithRetry(rctx, refreshToken, 3)
	if err != nil {
		_, _ = m.handleRefreshHTTPError(rctx, acc, email, err, true)
		return
	}

	acc.UpdateToken(*td)
	m.enqueueSave(acc)
	log.Infof("账号 [%s] 刷新成功", acc.GetEmail())
	if qcEff := m.effectiveQuotaAfterRefresh(nil); qcEff != nil {
		qctx, qcancel := context.WithTimeout(ctx, 30*time.Second)
		if !m.afterRefreshValidateQuota(qctx, qcEff, acc) {
			qcancel()
			return
		}
		qcancel()
	}
}

/**
 * saveTokenToFile 将更新后的 Token 原子写入磁盘文件
 * 使用先写临时文件再重命名的方式，防止写入失败时损坏原文件
 * @param acc - 要保存的账号
 * @returns error - 保存失败时返回错误（原文件不受影响）
 */
func (m *Manager) saveTokenToFile(acc *Account) error {
	if m.db != nil {
		return m.saveTokenToDB(acc)
	}
	acc.mu.RLock()
	tf := TokenFile{
		IDToken:      acc.Token.IDToken,
		AccessToken:  acc.Token.AccessToken,
		RefreshToken: acc.Token.RefreshToken,
		AccountID:    acc.Token.AccountID,
		LastRefresh:  acc.LastRefreshedAt.Format(time.RFC3339),
		Email:        acc.Token.Email,
		Type:         "codex",
		Expire:       acc.Token.Expire,
	}
	filePath := acc.FilePath
	acc.mu.RUnlock()

	data, err := json.MarshalIndent(tf, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化 Token 失败: %w", err)
	}

	if err = os.MkdirAll(filepath.Dir(filePath), 0700); err != nil {
		return fmt.Errorf("创建目录失败: %w", err)
	}

	/* 原子写入：先写临时文件，成功后再重命名，避免写入失败损坏原文件 */
	tmpPath := filePath + ".tmp"
	if err = os.WriteFile(tmpPath, data, 0600); err != nil {
		return fmt.Errorf("写入临时文件失败: %w", err)
	}

	if err = os.Rename(tmpPath, filePath); err != nil {
		/* 重命名失败时清理临时文件 */
		_ = os.Remove(tmpPath)
		return fmt.Errorf("重命名文件失败: %w", err)
	}

	return nil
}
