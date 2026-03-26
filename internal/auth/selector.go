/**
 * 多号轮询选择器模块
 * 实现 Round-Robin 和 Fill-First 两种账号选择策略
 * 全部热路径均使用原子操作，零锁竞争
 * 可用账号缓存 + 原子游标实现高并发快速选号
 */
package auth

import (
	"fmt"
	"sort"
	"sync/atomic"
	"time"
)

/* 可用账号缓存有效期（毫秒），超过此时间自动刷新（略短以减少选到已冷却/刚 401 的号） */
const availableCacheTTLMs = 200

/* 选号时距 access_token 过期不足该窗口则视为不可用（与后台刷新阈值一致） */
const pickAvoidTokenExpireMarginMs = int64(5 * 60 * 1000)

/**
 * Selector 定义账号选择器接口
 * @method Pick - 从可用账号中选择一个
 */
type Selector interface {
	Pick(model string, accounts []*Account) (*Account, error)
}

/**
 * cachedAvailable 可用账号缓存条目（不可变，通过原子指针替换）
 * @field accounts - 已排序的可用账号列表
 * @field updatedAt - 缓存更新时间戳（毫秒）
 */
type cachedAvailable struct {
	accounts  []*Account
	updatedAt int64
}

/**
 * RoundRobinSelector 轮询选择器
 * 全部操作无锁：原子指针存储缓存 + 原子游标选号
 * @field cursor - 全局原子游标
 * @field cachePtr - 原子指针存储可用账号缓存（完全无锁读取）
 * @field refreshing - CAS 标志防止多 goroutine 同时刷新缓存
 */
type RoundRobinSelector struct {
	cursor     atomic.Uint64
	cachePtr   atomic.Pointer[cachedAvailable]
	refreshing atomic.Int32
}

/**
 * NewRoundRobinSelector 创建新的轮询选择器
 * @returns *RoundRobinSelector - 轮询选择器实例
 */
func NewRoundRobinSelector() *RoundRobinSelector {
	return &RoundRobinSelector{}
}

/**
 * InvalidateAvailableCache 使可用账号缓存失效（账号状态变更时由 Manager 调用）
 */
func (s *RoundRobinSelector) InvalidateAvailableCache() {
	s.cachePtr.Store(nil)
}

/**
 * Pick 使用轮询策略选择下一个可用账号
 * 全部无锁操作：原子指针读缓存 + 原子游标选号
 * @param model - 请求的模型名称
 * @param accounts - 全部账号列表
 * @returns *Account - 选中的账号
 * @returns error - 没有可用账号时返回错误
 */
func (s *RoundRobinSelector) Pick(model string, accounts []*Account) (*Account, error) {
	available := s.getOrRefreshCache(accounts)
	if len(available) == 0 {
		return nil, fmt.Errorf("没有可用的 Codex 账号")
	}

	/* 原子递增游标，无锁选择 */
	idx := s.cursor.Add(1) - 1
	selected := available[idx%uint64(len(available))]

	return selected, nil
}

/**
 * getOrRefreshCache 获取可用账号缓存，过期则刷新
 * 使用原子指针读取（零开销）+ CAS 防止惊群刷新
 * @param accounts - 全部账号列表
 * @returns []*Account - 已排序的可用账号列表
 */
func (s *RoundRobinSelector) getOrRefreshCache(accounts []*Account) []*Account {
	nowMs := time.Now().UnixMilli()

	/* 原子指针读取缓存，零开销 */
	c := s.cachePtr.Load()
	if c != nil && (nowMs-c.updatedAt) < availableCacheTTLMs {
		return c.accounts
	}

	/* CAS 抢占刷新权，防止多 goroutine 同时刷新 */
	if !s.refreshing.CompareAndSwap(0, 1) {
		/* 其他 goroutine 正在刷新，使用旧缓存或直接过滤 */
		if c != nil {
			return c.accounts
		}
		return filterAvailable(accounts)
	}
	defer s.refreshing.Store(0)

	/* 重新构建缓存 */
	available := filterAvailable(accounts)
	if len(available) > 1 {
		sortByUsedPercent(available)
	}

	/* 原子替换缓存指针 */
	s.cachePtr.Store(&cachedAvailable{
		accounts:  available,
		updatedAt: nowMs,
	})

	return available
}

/**
 * QuotaFirstSelector 额度优先选择器
 * 每次请求选择剩余额度最高（used_percent 最低）的可用账号
 * 适合希望尽量打满单账号额度的场景
 */
type QuotaFirstSelector struct{}

/**
 * NewQuotaFirstSelector 创建新的额度优先选择器
 */
func NewQuotaFirstSelector() *QuotaFirstSelector {
	return &QuotaFirstSelector{}
}

/**
 * Pick 选择剩余额度最高的可用账号
 */
func (s *QuotaFirstSelector) Pick(model string, accounts []*Account) (*Account, error) {
	available := filterAvailable(accounts)
	if len(available) == 0 {
		return nil, fmt.Errorf("没有可用的 Codex 账号")
	}
	if len(available) > 1 {
		sortByUsedPercent(available)
	}
	return available[0], nil
}

/**
 * FillFirstSelector 填充优先选择器
 * 始终优先使用第一个可用账号，直到该账号进入冷却后再切换
 * 适合需要消耗单个账号配额上限的场景
 */
type FillFirstSelector struct{}

/**
 * NewFillFirstSelector 创建新的填充优先选择器
 * @returns *FillFirstSelector - 填充优先选择器实例
 */
func NewFillFirstSelector() *FillFirstSelector {
	return &FillFirstSelector{}
}

/**
 * Pick 使用填充优先策略选择账号
 * @param model - 请求的模型名称
 * @param accounts - 全部账号列表
 * @returns *Account - 选中的账号
 * @returns error - 没有可用账号时返回错误
 */
func (s *FillFirstSelector) Pick(model string, accounts []*Account) (*Account, error) {
	available := filterAvailable(accounts)
	if len(available) == 0 {
		return nil, fmt.Errorf("没有可用的 Codex 账号")
	}

	/* 按文件路径排序后返回第一个 */
	sort.Slice(available, func(i, j int) bool {
		return available[i].FilePath < available[j].FilePath
	})

	return available[0], nil
}

/**
 * sortByUsedPercent 按额度使用率升序排序（剩余额度最多的优先）
 * used_percent: 0=最空闲, 100=已满, -1=未知（排最后）
 * 同使用率时按文件路径保持稳定顺序
 * @param accounts - 待排序的账号列表（原地排序）
 */
func sortByUsedPercent(accounts []*Account) {
	sort.Slice(accounts, func(i, j int) bool {
		pi := accounts[i].GetUsedPercent()
		pj := accounts[j].GetUsedPercent()
		if pi < 0 && pj >= 0 {
			return false
		}
		if pi >= 0 && pj < 0 {
			return true
		}
		if pi != pj {
			return pi < pj
		}
		return accounts[i].FilePath < accounts[j].FilePath
	})
}

/**
 * filterAvailable 过滤出当前可用的账号
 * 使用原子字段读取状态，完全无锁，适合 2w+ 账号高并发场景
 * @param accounts - 全部账号列表
 * @returns []*Account - 可用的账号列表
 */
func filterAvailable(accounts []*Account) []*Account {
	nowMs := time.Now().UnixMilli()
	available := make([]*Account, 0, len(accounts))

	for _, acc := range accounts {
		/* 原子读取状态，无锁 */
		status := AccountStatus(acc.atomicStatus.Load())
		switch status {
		case StatusDisabled:
			continue
		case StatusCooldown:
			if nowMs < acc.atomicCooldownMs.Load() {
				continue
			}
		}
		/* 即将过期的 access_token 不参与分配，避免周期性未刷新的号在调用时 401 */
		if expMs := acc.accessExpireUnixMs.Load(); expMs > 0 && nowMs >= expMs-pickAvoidTokenExpireMarginMs {
			continue
		}
		available = append(available, acc)
	}

	return available
}
