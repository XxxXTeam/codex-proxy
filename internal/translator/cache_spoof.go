/**
 * 缓存用量伪造模块
 * 当上游返回了 cache_read 但未返回 cache_write 时，伪造缓存数据：
 * - 首次触发：将 input_tokens 全部改为 cache_write_tokens
 * - 后续请求：维持 cache_read + cache_write = input_tokens * 99%
 */
package translator

import "sync/atomic"

var (
	cacheSpoofEnabled      atomic.Bool
	cacheSpoofFirstWritten atomic.Bool
)

/**
 * SetCacheSpoofEnabled 设置缓存伪造开关
 * 关闭时会重置首次写入标记
 * @param enabled - 是否启用缓存伪造
 */
func SetCacheSpoofEnabled(enabled bool) {
	cacheSpoofEnabled.Store(enabled)
	if !enabled {
		cacheSpoofFirstWritten.Store(false)
	}
}

/**
 * IsCacheSpoofEnabled 返回当前缓存伪造开关状态
 * @returns bool - 是否启用
 */
func IsCacheSpoofEnabled() bool {
	return cacheSpoofEnabled.Load()
}

/**
 * ApplyCacheSpoof 对缓存用量数据进行伪造
 * 规则：
 *  1. 上游有 cache_read 但没有 cache_write → 触发伪造
 *  2. 首次触发：input_tokens 全部改为 cache_write
 *  3. 后续请求：cache_read + cache_write = input_tokens * 99%
 *
 * @param inputTokens - 输入 token 总数
 * @param cacheRead - 缓存读取 token 数（会被原地修改）
 * @param cacheWrite - 缓存写入 token 数（会被原地修改）
 */
func ApplyCacheSpoof(inputTokens int64, cacheRead, cacheWrite *int64) {
	if !cacheSpoofEnabled.Load() || inputTokens <= 0 {
		return
	}
	if cacheRead == nil || cacheWrite == nil {
		return
	}

	origRead := *cacheRead
	origWrite := *cacheWrite

	/*
	 * 触发条件：
	 *   1. 上游有 cache_read 但没有 cache_write
	 *   2. 上游完全没有返回缓存数据（cache_read 和 cache_write 均为 0）
	 */
	needSpoof := (origWrite == 0 && origRead > 0) || (origRead == 0 && origWrite == 0)

	if needSpoof {
		if cacheSpoofFirstWritten.CompareAndSwap(false, true) {
			/* 首次触发：全部改为 cache_write */
			*cacheWrite = inputTokens
			*cacheRead = 0
			return
		}
		/* 已标记首次写入，这是后续请求：维持 99% 缓存 */
		cacheTotal := inputTokens * 99 / 100
		if cacheTotal > inputTokens {
			cacheTotal = inputTokens
		}
		if cacheTotal < 1 {
			return
		}
		*cacheRead = cacheTotal * 40 / 100
		*cacheWrite = cacheTotal - *cacheRead
		return
	}

	/* 已有首次写入标记，确保 99% 缓存 */
	if cacheSpoofFirstWritten.Load() {
		cacheTotal := inputTokens * 99 / 100
		if cacheTotal > inputTokens {
			cacheTotal = inputTokens
		}
		if cacheTotal < 1 {
			return
		}
		current := *cacheRead + *cacheWrite
		if current < cacheTotal {
			deficit := cacheTotal - current
			*cacheRead += deficit
		}
	}
}

/**
 * ResetCacheSpoofState 重置首次写入标记（供测试使用）
 */
func ResetCacheSpoofState() {
	cacheSpoofFirstWritten.Store(false)
}