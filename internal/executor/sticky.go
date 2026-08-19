package executor

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"codex-proxy/internal/auth"

	"github.com/tidwall/gjson"
)

/* SessionStickyTable 进程内会话→账号绑定表：同一会话（session-id / previous_response_id 前缀）固定走同一上游账号，
 * 减少指纹上下文换号导致的 turn-state 剥离/额度分片。线程安全；由 handler 持有。
 */
type SessionStickyTable struct {
	mu      sync.Mutex
	records map[string]*stickyRecord
	ttl     time.Duration
	enabled atomic.Bool
	/* mode 粘性调度模式（当前固定 prefer：命中绑定号时优先选；未命中/不可用照常选号）；
	 * 保留为字段以支持后续配置化开关（strict 模式不回落） */
	mode stickyMode
}

/* stickyRecord 单条会话绑定记录 */
type stickyRecord struct {
	seed      string    /* 会话粘性键（prev:<id> 或 session:<value>） */
	accountID string    /* 绑定的账号 ID（GetAccountID） */
	filePath  string    /* 账号文件路径（选号排除与记录用） */
	lastSeen  time.Time /* 最近一次成功时间（用于过期） */
	refCount  int       /* 命中次数（仅统计/诊断，不参与并发控制） */
}

/* Enabled 是否已启用会话粘性选号（显式关闭时 report 当前计数值并停止） */
func (s *SessionStickyTable) Enabled() bool {
	if s == nil {
		return false
	}
	return s.enabled.Load()
}

/* SetEnabled 切换会话粘性选号开关（Register/AccountID 读取路径都检查） */
func (s *SessionStickyTable) SetEnabled(on bool) {
	if s == nil {
		return
	}
	s.enabled.Store(on)
}

/* ReportAndReset 输出诊断快照（seed:accountID:refCount 累计）并清空表。用于关闭时保留已产生的
 * 多账号分片统计（汇总后返回），避免表内粘性命中被丢。 */
func (s *SessionStickyTable) ReportAndReset() [][3]string {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([][3]string, 0, len(s.records))
	for k, r := range s.records {
		out = append(out, [3]string{k, r.accountID, strconv.Itoa(r.refCount)})
	}
	s.records = make(map[string]*stickyRecord)
	return out
}

/* stickyMode 会话粘性调度模式：关闭优先 / 粘性优先 / 仅粘性 */
type stickyMode int

const (
	/* stickyModeOff 关闭会话粘性选号（客户端显式会话以 sub2api「上一轮-轮询绑定」为唯一信号；默认） */
	stickyModeOff stickyMode = iota
	/* stickyModePrefer 命中绑定账号时优先选择命中号（非命中/命中号不可用时照常选号） */
	stickyModePrefer
	/* stickyModeStrict 命中绑定账号后仅选绑定号；不可用则直接报错（不回落） */
	stickyModeStrict
)

/* NewSessionStickyTable 构造会话粘性表（ttl<=0 默认 1 小时） */
func NewSessionStickyTable() *SessionStickyTable {
	return &SessionStickyTable{
		records: make(map[string]*stickyRecord),
		ttl:     time.Hour,
	}
}

/* seedFor 从请求体推导会话粘性键：优先后续回复（previous_response_id），其次显式会话信号 */
func seedFor(requestBody []byte) string {
	/* 会话延续请求：previous_response_id 前缀在统一响应 ID 生成规则下稳定（形如 resp_xxx） */
	if v := previousResponseIDFromBody(requestBody); v != "" {
		return "prev:" + v
	}
	for _, key := range sessionIDCandidates {
		if v := strings.TrimSpace(gjsonGetString(requestBody, key)); v != "" {
			return "session:" + v
		}
	}
	return ""
}

/* gjsonGetString 读取 JSON 路径字符串（避免在循环内重复 import 解析） */
func gjsonGetString(body []byte, path string) string {
	return strings.TrimSpace(gjson.GetBytes(body, path).String())
}

/* Register 记录一次会话成功绑定；返回绑定键（供日志/统计）。仅 enabled 时写入。 */
func (s *SessionStickyTable) Register(body []byte, acc authAccountLike) string {
	if s == nil || !s.enabled.Load() {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := seedFor(body)
	if key == "" || acc == nil {
		return ""
	}
	now := time.Now()
	r, ok := s.records[key]
	if !ok {
		r = &stickyRecord{seed: key, accountID: accountIDOf(acc), filePath: filePathOf(acc), lastSeen: now}
		s.records[key] = r
	} else {
		r.accountID = accountIDOf(acc)
		r.lastSeen = now
	}
	if r.refCount < (1 << 30) {
		r.refCount++
	}
	s.sweepLocked(now)
	return key
}

/* AccountID 返回会话粘性键绑定的账号 ID（未启用/无记录或已过期返回空串） */
func (s *SessionStickyTable) AccountID(body []byte) string {
	if s == nil || !s.enabled.Load() {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := seedFor(body)
	if key == "" {
		return ""
	}
	if r, ok := s.records[key]; ok && s.validLocked(r, time.Now()) {
		return r.accountID
	}
	return ""
}

/* FilePath 返回会话粘性键绑定的账号文件路径（供选号排除；未启用/无记录返回空串） */
func (s *SessionStickyTable) FilePath(body []byte) string {
	if s == nil || !s.enabled.Load() {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := seedFor(body)
	if key == "" {
		return ""
	}
	if r, ok := s.records[key]; ok && s.validLocked(r, time.Now()) {
		return r.filePath
	}
	return ""
}

/* validLocked 判断记录是否仍有效（TTL 内）；无效/键冲突记录直接删除。调用方持锁。 */
func (s *SessionStickyTable) validLocked(r *stickyRecord, now time.Time) bool {
	if r == nil {
		return false
	}
	k, ok := s.records[r.seed]
	if !ok || k != r {
		/* 记录已替换（后续注册覆盖了该键）：视为无效 */
		return false
	}
	if s.ttl < 0 {
		/* 测试等显式负 TTL：视为已过期 */
		delete(s.records, r.seed)
		return false
	}
	if s.ttl > 0 && now.Sub(r.lastSeen) > s.ttl {
		delete(s.records, r.seed)
		return false
	}
	return true
}

/* LastSeen 返回会话粘性键绑定的最近成功时间（未启用/无记录返回零值；诊断用） */
func (s *SessionStickyTable) LastSeen(body []byte) time.Time {
	if s == nil || !s.enabled.Load() {
		return time.Time{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := seedFor(body)
	if key == "" {
		return time.Time{}
	}
	if r, ok := s.records[key]; ok && s.validLocked(r, time.Now()) {
		return r.lastSeen
	}
	return time.Time{}
}

/* Sweep 定时清理过期记录（handler 无需显式调用，Register/读取路径都会机会性清理） */
func (s *SessionStickyTable) Sweep() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked(time.Now())
}

/* sweepLocked 清除过期记录并重置（调用方持锁） */
func (s *SessionStickyTable) sweepLocked(now time.Time) {
	if s.ttl > 0 {
		for k, r := range s.records {
			if now.Sub(r.lastSeen) > s.ttl {
				delete(s.records, k)
			}
		}
	}
}

/* stickyHash 汇总粘性键/账号，生成确定性短摘要（诊断日志用，不含敏感信息外泄） */
func stickyHash(parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(h[:6])
}

/* filePathOf 取账号文件路径：*auth.Account 类型断言（nil 返回空串） */
func filePathOf(a authAccountLike) string {
	if acc, ok := a.(*auth.Account); ok && acc != nil {
		return acc.FilePath
	}
	return ""
}