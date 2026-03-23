package auth

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"
)

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
	saveTokenStmt           *sql.Stmt
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
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
			if acc.refreshing.Load() == 0 {
				return true
			}
		}
	}
}

/**
 * SetupDB 初始化数据库表结构
 */
func SetupDB(db *sql.DB) error {
	if db == nil {
		return nil
	}
	_, err := db.Exec(`
CREATE TABLE IF NOT EXISTS codex_accounts (
	id SERIAL PRIMARY KEY,
	account_id TEXT UNIQUE,
	email TEXT UNIQUE,
	id_token TEXT,
	access_token TEXT,
	refresh_token TEXT,
	expire TEXT,
	plan_type TEXT,
	last_refresh TIMESTAMPTZ,
	updated_at TIMESTAMPTZ DEFAULT NOW()
)
`)
	if err != nil {
		return err
	}
	_, err = db.Exec(`CREATE INDEX IF NOT EXISTS idx_codex_accounts_updated_at ON codex_accounts(updated_at)`)
	if err != nil {
		return err
	}
	_, err = db.Exec(`CREATE INDEX IF NOT EXISTS idx_codex_accounts_refresh_token ON codex_accounts(refresh_token)`)
	return err
}

func (m *Manager) prepareDBStatements() error {
	if m.db == nil {
		return nil
	}
	stmt, err := m.db.Prepare(`
INSERT INTO codex_accounts (account_id,email,id_token,access_token,refresh_token,expire,plan_type,last_refresh,updated_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,NOW())
ON CONFLICT (account_id) DO UPDATE SET
	email = EXCLUDED.email,
	id_token = EXCLUDED.id_token,
	access_token = EXCLUDED.access_token,
	refresh_token = EXCLUDED.refresh_token,
	expire = EXCLUDED.expire,
	plan_type = EXCLUDED.plan_type,
	last_refresh = EXCLUDED.last_refresh,
	updated_at = NOW()
`)
	if err != nil {
		return err
	}
	m.saveTokenStmt = stmt
	return nil
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
 * LoadAccounts 从账号目录加载所有 JSON 账号文件
 * @returns error - 加载失败时返回错误
 */
func (m *Manager) LoadAccounts() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.db != nil {
		/* 始终检查是否有新 JSON 文件需要导入到数据库 */
		if m.authDir != "" {
			if _, err := m.importAccountsFromFilesToDB(); err != nil {
				log.Warnf("从磁盘迁移账号到数据库失败: %v", err)
			}
		}

		if err := m.loadAccountsFromDB(); err != nil {
			return fmt.Errorf("加载数据库账号失败: %w", err)
		}

		if len(m.accounts) == 0 {
			return fmt.Errorf("数据库中未找到有效账号")
		}
		m.publishSnapshot()
		log.Infof("共加载 %d 个 Codex 账号（PostgreSQL）", len(m.accounts))
		return nil
	}

	entries, err := os.ReadDir(m.authDir)
	if err != nil {
		return fmt.Errorf("读取账号目录失败: %w", err)
	}

	filePaths := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".json") {
			continue
		}
		filePaths = append(filePaths, filepath.Join(m.authDir, entry.Name()))
	}

	type loadResult struct {
		path string
		acc  *Account
		err  error
	}

	workerCount := runtime.GOMAXPROCS(0) * 8 // 增加并发度以提升启动速度
	if workerCount < 16 {                    // 提高最小工作器数量
		workerCount = 16
	}
	if workerCount > 256 { // 允许更高的最大并发度
		workerCount = 256
	}
	if workerCount > len(filePaths) && len(filePaths) > 0 {
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

	accounts := make([]*Account, 0, len(filePaths))
	index := make(map[string]*Account, len(filePaths))
	for r := range results {
		if r.err != nil {
			log.Warnf("加载账号文件失败 [%s]: %v", filepath.Base(r.path), r.err)
			continue
		}
		accounts = append(accounts, r.acc)
		index[r.path] = r.acc
	}

	if len(accounts) == 0 {
		return fmt.Errorf("在目录 %s 中未找到有效的账号文件", m.authDir)
	}

	m.accounts = accounts
	m.accountIndex = index
	m.publishSnapshot()
	log.Infof("共加载 %d 个 Codex 账号", len(accounts))
	return nil
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

	if tf.RefreshToken == "" {
		return nil, fmt.Errorf("文件中缺少 refresh_token")
	}

	/* 从 ID Token 中补充解析 AccountID、Email、PlanType */
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
		FilePath: filePath,
		Token: TokenData{
			IDToken:      tf.IDToken,
			AccessToken:  tf.AccessToken,
			RefreshToken: tf.RefreshToken,
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

func (m *Manager) loadAccountsFromDB() error {
	if m.db == nil {
		return nil
	}

	rows, err := m.db.Query(`SELECT account_id,email,id_token,access_token,refresh_token,expire,plan_type,last_refresh FROM codex_accounts`)
	if err != nil {
		return err
	}
	defer rows.Close()

	accounts := make([]*Account, 0)
	index := make(map[string]*Account)

	for rows.Next() {
		var accountID, email, idToken, accessToken, refreshToken, expire, planType sql.NullString
		var lastRefresh sql.NullTime
		if err := rows.Scan(&accountID, &email, &idToken, &accessToken, &refreshToken, &expire, &planType, &lastRefresh); err != nil {
			log.Warnf("读取数据库账号失败: %v", err)
			continue
		}
		if refreshToken.String == "" {
			continue
		}
		key := "db:" + accountID.String
		if accountID.String == "" {
			key = "db:" + email.String
		}
		if key == "db:" {
			key = fmt.Sprintf("db:%d", len(accounts)+1)
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
		acc.SyncAccessExpireFromToken()

		accounts = append(accounts, acc)
		index[key] = acc
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
	defer acc.mu.RUnlock()

	if m.saveTokenStmt != nil {
		_, err := m.saveTokenStmt.Exec(acc.Token.AccountID, acc.Token.Email, acc.Token.IDToken, acc.Token.AccessToken, acc.Token.RefreshToken, acc.Token.Expire, acc.Token.PlanType, acc.LastRefreshedAt)
		return err
	}

	_, err := m.db.Exec(`
INSERT INTO codex_accounts (account_id,email,id_token,access_token,refresh_token,expire,plan_type,last_refresh,updated_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,NOW())
ON CONFLICT (account_id) DO UPDATE SET
	email = EXCLUDED.email,
	id_token = EXCLUDED.id_token,
	access_token = EXCLUDED.access_token,
	refresh_token = EXCLUDED.refresh_token,
	expire = EXCLUDED.expire,
	plan_type = EXCLUDED.plan_type,
	last_refresh = EXCLUDED.last_refresh,
	updated_at = NOW()
`, acc.Token.AccountID, acc.Token.Email, acc.Token.IDToken, acc.Token.AccessToken, acc.Token.RefreshToken, acc.Token.Expire, acc.Token.PlanType, acc.LastRefreshedAt)

	return err
}

func (m *Manager) accountExists(acc *Account) (bool, error) {
	if m.db == nil {
		return false, nil
	}

	var count int
	if err := m.db.QueryRow(`SELECT COUNT(1) FROM codex_accounts WHERE email=$1 OR account_id=$2`, acc.GetEmail(), acc.GetAccountID()).Scan(&count); err != nil {
		return false, err
	}
	return count > 0, nil
}

func (m *Manager) deleteAccountFromDB(acc *Account) error {
	if m.db == nil {
		return nil
	}

	result, err := m.db.Exec(`DELETE FROM codex_accounts WHERE email=$1 OR account_id=$2`, acc.GetEmail(), acc.GetAccountID())
	if err != nil {
		return err
	}
	_, _ = result.RowsAffected()
	// 重复删除也视为成功
	return nil
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
 * PickExcluding 选择下一个可用账号，排除已用过的账号
 * 用于错误重试时切换到不同的账号
 * @param model - 请求的模型名称
 * @param excluded - 已排除的账号文件路径集合
 * @returns *Account - 选中的账号
 * @returns error - 没有可用账号时返回错误
 */
func (m *Manager) PickExcluding(model string, excluded map[string]bool) (*Account, error) {
	/* 原子指针读取，零锁 */
	allAccounts := *m.accountsPtr.Load()
	if len(excluded) == 0 {
		return m.selector.Pick(model, allAccounts)
	}

	filtered := make([]*Account, 0, len(allAccounts)-len(excluded))
	for _, acc := range allAccounts {
		if !excluded[acc.FilePath] {
			filtered = append(filtered, acc)
		}
	}

	if len(filtered) == 0 {
		return nil, fmt.Errorf("没有更多可用账号（已排除 %d 个）", len(excluded))
	}

	return m.selector.Pick(model, filtered)
}

/**
 * PickRecentlySuccessful 在换号重试仍失败时作为回退：选取最近一次成功完成请求的账号（LastUsedAt 最新）。
 * 优先选择本轮尚未尝试过的账号；若均已尝试则仍取全局最近成功者。
 */
func (m *Manager) PickRecentlySuccessful(model string, excluded map[string]bool) (*Account, error) {
	_ = model
	allAccounts := *m.accountsPtr.Load()
	type cand struct {
		acc *Account
		t   time.Time
	}
	var list []cand
	for _, acc := range allAccounts {
		st := AccountStatus(acc.atomicStatus.Load())
		if st == StatusDisabled || st == StatusCooldown {
			continue
		}
		t := acc.GetLastUsedAt()
		if t.IsZero() {
			continue
		}
		list = append(list, cand{acc: acc, t: t})
	}
	if len(list) == 0 {
		return nil, fmt.Errorf("没有可用于回退的最近成功账号")
	}
	sort.Slice(list, func(i, j int) bool {
		if !list[i].t.Equal(list[j].t) {
			return list[i].t.After(list[j].t)
		}
		return list[i].acc.FilePath < list[j].acc.FilePath
	})
	for _, c := range list {
		if excluded == nil || !excluded[c.acc.FilePath] {
			return c.acc, nil
		}
	}
	return list[0].acc, nil
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
		if noPolicyRemove {
			m.RemoveAccount(acc, ReasonRefreshFailed)
			return false, QuotaApplyRemoved
		}
		m.DisableAccountByRenamingFile(acc, ReasonAuth401Disabled)
		return false, QuotaApplyDisabled
	}
	pol, has := m.refreshHTTPPolicy[status]
	if !has {
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

				/* 刷新成功后同时查询额度 */
				if ok && quotaChecker != nil {
					quotaChecker.CheckOne(ctx, a)
					a.RefreshUsedPercent()
				}

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
	log.Infof("账号 [%s] 刷新成功", td.Email)
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
	default:
		r.Status = Auth401RecoverCooldown429OK
	}
	if refreshErr != nil {
		r.Detail = refreshErr.Error()
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
 * ctx 为外层上限；单次 OAuth 另受 refresh-single-timeout-sec 约束（非对话 API）
 */
func (m *Manager) RecoverAuth401(ctx context.Context, acc *Account, qc *QuotaChecker) Auth401RecoverResult {
	if ctx == nil {
		ctx = context.Background()
	}
	if acc == nil {
		return Auth401RecoverResult{Status: Auth401RecoverInvalid, Detail: "account is nil"}
	}
	email := acc.GetEmail()
	fp := acc.FilePath
	out := Auth401RecoverResult{Email: email, FilePath: fp}

	if !acc.refreshing.CompareAndSwap(0, 1) {
		/* 与后台批量刷新并发时：等对方写完 Token 后本请求同号重试，避免刷新已成功却仍走换号/空解析 */
		log.Debugf("账号 [%s] 正在他处刷新 Token，401 恢复等待其完成…", email)
		if m.waitAccountRefreshIdle(ctx, acc) {
			out.Status = Auth401RecoverRefreshed
			out.Detail = "waited_peer_refresh"
			log.Infof("账号 [%s] 401 恢复：已等待进行中的刷新结束，将用当前凭据重试上游", email)
			return out
		}
		out.Status = Auth401RecoverSkippedBusy
		out.Detail = "账号正在刷新中，等待超时"
		log.Warnf("账号 [%s] 401 恢复：等待他处刷新超时，跳过同号重试", email)
		return out
	}
	defer acc.refreshing.Store(0)

	acc.mu.RLock()
	refreshToken := acc.Token.RefreshToken
	acc.mu.RUnlock()

	if refreshToken == "" {
		log.Warnf("账号 [%s] 无 refresh_token，禁用凭据", email)
		m.DisableAccountByRenamingFile(acc, ReasonAuth401Disabled)
		out.Status = Auth401RecoverDisabled
		out.ReasonCode = ReasonAuth401Disabled
		out.Detail = "missing refresh_token"
		return out
	}

	log.Warnf("账号 [%s] 401 恢复：正在同步刷新 Token...", email)
	rctx, rcancel := m.refreshRequestContext(ctx)
	defer rcancel()
	td, err := m.refresher.RefreshTokenWithRetry(rctx, refreshToken, 3)
	if err == nil {
		acc.UpdateToken(*td)
		if err := m.saveTokenToFile(acc); err != nil {
			log.Errorf("账号 [%s] 401 刷新成功但持久化失败: %v", td.Email, err)
			out.Detail = "persist error: " + err.Error()
		}
		m.enqueueSave(acc)
		acc.SetActive()
		m.InvalidateSelectorCache()
		log.Infof("账号 [%s] 401 后刷新成功，已恢复可用", td.Email)
		out.Status = Auth401RecoverRefreshed
		return out
	}

	recovered, qOut := m.handleRefreshHTTPError(rctx, acc, email, err, false)
	if recovered {
		acc.SetActive()
		m.InvalidateSelectorCache()
		out.Status = Auth401RecoverRefreshed
		return out
	}
	return mapRecover401FromRefreshOutcome(qOut, email, fp, err)
}

/**
 * HandleAuth401 处理请求返回 401 的账号（委托 RecoverAuth401）
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
	return m.RecoverAuth401(hctx, acc, qc)
}

/**
 * ScheduleUpstream429Recovery 上游 429 后异步：先查额度，未通过则等待 1 小时、刷新 token 再查，仍失败则删号
 */
func (m *Manager) ScheduleUpstream429Recovery(_ context.Context, acc *Account, qc *QuotaChecker) {
	if qc == nil || acc == nil {
		return
	}
	if !acc.upstream429Recovering.CompareAndSwap(0, 1) {
		return
	}
	go func() {
		defer acc.upstream429Recovering.Store(0)
		email := acc.GetEmail()

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

		log.Warnf("账号 [%s] 上游 429 后额度查询暂态失败，1 小时后刷新凭证并重试", email)

		select {
		case <-time.After(1 * time.Hour):
		case <-m.stopCh:
			return
		}

		if !m.AccountInPool(acc) {
			return
		}

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

		qctx2, cancel2 := context.WithTimeout(context.Background(), 25*time.Second)
		v2, st2 := qc.CheckAccountResultWithStatus(qctx2, acc)
		cancel2()
		if v2 == 1 {
			acc.SetActive()
			m.InvalidateSelectorCache()
			log.Infof("账号 [%s] 429 恢复：额度查询已通过，已恢复可用", acc.GetEmail())
			return
		}
		m.ApplyQuotaUsageHTTPOutcome(context.Background(), qc, acc, st2, v2)
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
	log.Infof("账号 [%s] 刷新成功", td.Email)
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
