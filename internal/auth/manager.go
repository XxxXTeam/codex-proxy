package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"
)

/* 并发刷新默认配置 */
const (
	defaultRefreshConcurrency = 50
	defaultScanInterval       = 30
)

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
	mu                 sync.RWMutex
	accounts           []*Account
	accountIndex       map[string]*Account
	accountsPtr        atomic.Pointer[[]*Account] /* 原子快照，Pick 热路径零锁读取 */
	refresher          *Refresher
	selector           Selector
	authDir            string
	refreshInterval    int
	refreshConcurrency int
	saveQueue          chan *Account /* 异步磁盘写入队列 */
	stopCh             chan struct{}
}

/**
 * NewManager 创建新的账号管理器
 * @param authDir - 账号文件目录
 * @param proxyURL - 代理地址
 * @param refreshInterval - 刷新间隔（秒）
 * @param selector - 账号选择器
 * @returns *Manager - 账号管理器实例
 */
func NewManager(authDir, proxyURL string, refreshInterval int, selector Selector) *Manager {
	if selector == nil {
		selector = NewRoundRobinSelector()
	}
	m := &Manager{
		accounts:           make([]*Account, 0, 1024),
		accountIndex:       make(map[string]*Account, 1024),
		refresher:          NewRefresher(proxyURL),
		selector:           selector,
		authDir:            authDir,
		refreshInterval:    refreshInterval,
		refreshConcurrency: defaultRefreshConcurrency,
		saveQueue:          make(chan *Account, 4096),
		stopCh:             make(chan struct{}),
	}
	empty := make([]*Account, 0)
	m.accountsPtr.Store(&empty)
	return m
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

	entries, err := os.ReadDir(m.authDir)
	if err != nil {
		return fmt.Errorf("读取账号目录失败: %w", err)
	}

	accounts := make([]*Account, 0, len(entries))
	index := make(map[string]*Account, len(entries))

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".json") {
			continue
		}

		filePath := filepath.Join(m.authDir, entry.Name())
		acc, loadErr := loadAccountFromFile(filePath)
		if loadErr != nil {
			log.Warnf("加载账号文件失败 [%s]: %v", entry.Name(), loadErr)
			continue
		}

		accounts = append(accounts, acc)
		index[filePath] = acc
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

	return &Account{
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
	}, nil
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

	/* 删除磁盘文件 */
	if err := os.Remove(filePath); err != nil && !os.IsNotExist(err) {
		log.Errorf("账号 [%s] 磁盘文件删除失败: %v", email, err)
	} else {
		log.Warnf("账号 [%s] 已删除（内存+磁盘），原因: %s，剩余 %d 个", email, reason, remaining)
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
	scanInterval := time.Duration(defaultScanInterval) * time.Second
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
	entries, err := os.ReadDir(m.authDir)
	if err != nil {
		log.Warnf("扫描账号目录失败: %v", err)
		return
	}

	newCount := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".json") {
			continue
		}

		filePath := filepath.Join(m.authDir, entry.Name())

		/* 检查是否已在号池中 */
		m.mu.RLock()
		_, exists := m.accountIndex[filePath]
		m.mu.RUnlock()

		if exists {
			continue
		}

		acc, loadErr := loadAccountFromFile(filePath)
		if loadErr != nil {
			continue
		}

		m.mu.Lock()
		/* 双检查防止并发 */
		if _, exists2 := m.accountIndex[filePath]; !exists2 {
			m.accounts = append(m.accounts, acc)
			m.accountIndex[filePath] = acc
			newCount++
		}
		m.mu.Unlock()
	}

	if newCount > 0 {
		/* 发布新快照 */
		m.mu.Lock()
		m.publishSnapshot()
		m.mu.Unlock()
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

	sem := make(chan struct{}, m.refreshConcurrency)
	var wg sync.WaitGroup

	for _, acc := range needRefresh {
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

		/* Token 还有 5 分钟以上有效期，跳过 */
		if expire != "" {
			if expireTime, parseErr := time.Parse(time.RFC3339, expire); parseErr == nil {
				if time.Until(expireTime) > 5*time.Minute {
					continue
				}
			}
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

	td, err := m.refresher.RefreshTokenWithRetry(ctx, refreshToken, 3)
	if err != nil {
		log.Errorf("账号 [%s] Token 刷新失败，从号池移除: %v", email, err)
		m.RemoveAccount(acc, ReasonRefreshFailed)
		return false
	}

	acc.UpdateToken(*td)

	/* 异步写盘，不阻塞刷新 goroutine */
	m.enqueueSave(acc)
	log.Infof("账号 [%s] Token 刷新成功", td.Email)
	return true
}

/**
 * HandleAuth401 处理请求返回 401 的账号
 * 先将账号设为短暂冷却（立即从可用池中排除），然后后台异步刷新 Token
 * 刷新成功：恢复为 active 状态，账号重新可用
 * 刷新失败：从号池和磁盘中彻底删除该账号
 * 该方法是非阻塞的，不影响当前请求的响应速度
 * @param acc - 返回 401 的账号
 */
func (m *Manager) HandleAuth401(acc *Account) {
	email := acc.GetEmail()

	/* 立即设为冷却状态，防止后续请求继续使用该账号 */
	acc.SetCooldown(30 * time.Second)
	log.Warnf("账号 [%s] 遇到 401，已临时冷却，后台刷新中...", email)

	/* CAS 去重：防止同一账号被多个刷新源同时刷新 */
	if !acc.refreshing.CompareAndSwap(0, 1) {
		log.Debugf("账号 [%s] 已在刷新中，跳过 401 后台刷新", email)
		return
	}

	/* 后台异步刷新 Token */
	go func() {
		defer acc.refreshing.Store(0)

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		acc.mu.RLock()
		refreshToken := acc.Token.RefreshToken
		acc.mu.RUnlock()

		if refreshToken == "" {
			log.Warnf("账号 [%s] 无 refresh_token，直接删除", email)
			m.RemoveAccount(acc, ReasonAuth401)
			return
		}

		td, err := m.refresher.RefreshTokenWithRetry(ctx, refreshToken, 2)
		if err != nil {
			log.Errorf("账号 [%s] 后台刷新失败，从号池删除: %v", email, err)
			m.RemoveAccount(acc, ReasonAuth401)
			return
		}

		/* 刷新成功，更新 Token 并恢复为 active */
		acc.UpdateToken(*td)
		m.enqueueSave(acc)
		log.Infof("账号 [%s] 后台刷新成功，已恢复可用", td.Email)
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

	td, err := m.refresher.RefreshTokenWithRetry(ctx, refreshToken, 3)
	if err != nil {
		log.Errorf("账号 [%s] Token 刷新失败，从号池移除: %v", email, err)
		m.RemoveAccount(acc, ReasonRefreshFailed)
		return
	}

	/* 更新内存中的 Token */
	acc.UpdateToken(*td)

	/* 异步写盘，不阻塞刷新 goroutine */
	m.enqueueSave(acc)
	log.Infof("账号 [%s] Token 刷新成功", td.Email)
}

/**
 * saveTokenToFile 将更新后的 Token 原子写入磁盘文件
 * 使用先写临时文件再重命名的方式，防止写入失败时损坏原文件
 * @param acc - 要保存的账号
 * @returns error - 保存失败时返回错误（原文件不受影响）
 */
func (m *Manager) saveTokenToFile(acc *Account) error {
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
