package executor

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"
)

// errTurnStateDecode base64 解码失败标记。
var errTurnStateDecode = errors.New("turn-state base64 解码失败")

var (
	b64std    = base64.StdEncoding
	b64url    = base64.RawURLEncoding
	b64stdRaw = base64.RawStdEncoding
	b64urlRaw = base64.RawURLEncoding
)

// CodexTurnStateHeader 上游/客户端间回传的 turn 状态头。
const CodexTurnStateHeader = "x-codex-turn-state"

// turnStateRecord 一条回传 turn 状态的溯源记录。
type turnStateRecord struct {
	// relationshipSeed 溯源键：apiKeyID + "\x00" + clientSessionID
	relationshipSeed string
	// accountID 归因账号（首次收到该 turn state 的上游账号）
	accountID string
	// clientSessionID 客户端会话标识（参与溯源键）
	clientSessionID string
	// createdAt 记录创建时间（用于 TTL 清理）
	createdAt time.Time
}

// TurnStateStore 进程内回传 turn 状态溯源表（并发安全，由 handler 持有）。
type TurnStateStore struct {
	mu      sync.Mutex
	records map[string]*turnStateRecord
	// ttl 记录 TTL
	ttl time.Duration
	// writes 自上次 sweep 以来的写入计数（每 256 次写入执行一次机会清理）
	writes uint32
	// lastSweep 上次机会清理时间
	lastSweep time.Time
	// sweepEvery 机会清理间隔
	sweepEvery time.Duration
}

// NewTurnStateStore 构造回传 turn 状态溯源表。ttl<=0 时默认 1 小时。
func NewTurnStateStore(ttl time.Duration) *TurnStateStore {
	if ttl <= 0 {
		ttl = time.Hour
	}
	return &TurnStateStore{
		records:    make(map[string]*turnStateRecord),
		ttl:        ttl,
		lastSweep:  time.Now(),
		sweepEvery: time.Minute,
	}
}

// Store 记录一个 turn state 的溯源关系（来自上游响应头回传）。
// relationshipSeed 为溯源键；accountID 为响应来源账号。
func (s *TurnStateStore) Store(relationshipSeed, accountID, clientSessionID string) {
	if s == nil || relationshipSeed == "" || accountID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records[relationshipSeed] = &turnStateRecord{
		relationshipSeed: relationshipSeed,
		accountID:        accountID,
		clientSessionID:  clientSessionID,
		createdAt:        time.Now(),
	}
	s.writes++
	if s.writes%256 == 0 {
		s.sweepLocked(time.Now())
	}
}

// OriginAccountID 返回溯源键对应的归属账号 ID（未记录返回空串）。
func (s *TurnStateStore) OriginAccountID(relationshipSeed string) string {
	if s == nil || relationshipSeed == "" {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.records[relationshipSeed]
	if !ok {
		return ""
	}
	if s.ttl > 0 && time.Since(rec.createdAt) > s.ttl {
		delete(s.records, relationshipSeed)
		return ""
	}
	if s.ttl < 0 {
		// 测试等场景显式负 TTL：视为已过期
		delete(s.records, relationshipSeed)
		return ""
	}
	return rec.accountID
}

// OpportunisticSweep 供出站路径调用：距离上次清理超过 sweepEvery 时执行机会清理。
func (s *TurnStateStore) OpportunisticSweep() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if time.Since(s.lastSweep) >= s.sweepEvery {
		s.sweepLocked(time.Now())
	}
}

// sweepLocked 清理过期记录（调用方需持锁）。
func (s *TurnStateStore) sweepLocked(now time.Time) {
	if s.ttl > 0 {
		for seed, rec := range s.records {
			if now.Sub(rec.createdAt) > s.ttl {
				delete(s.records, seed)
			}
		}
	}
	s.lastSweep = now
}

// provenanceMatches 判断 x-codex-turn-state 中的溯源关系是否与当前账号一致：
// blob 为 base64 存储的 JSON（内含 relationship_seed / account_id / client_session / issued_at）。
// seed 为本次请求的溯源键；account 为当前出站账号。不匹配时出站守卫剥离该头。
// 当 AccountID 为空（客户端直传未标注归属的 turn state）时视为一致并保留，交由 store 记录归属。
func provenanceMatches(raw, seed string, account authAccountLike) bool {
	if raw == "" || account == nil {
		return false
	}
	rec, ok := parseTurnStateProvenance(raw)
	if !ok {
		// 无法解析：视为无溯源信息，交由守卫直接剥离
		return false
	}
	if rec.RelationshipSeed != seed {
		return false
	}
	if accountIDOf(account) == "" {
		// 当前账号无 accountID：无法比对归属，仍允许（防止误剥合法上下文）
		return true
	}
	return rec.AccountID == "" || rec.AccountID == accountIDOf(account)
}

// authAccountLike 描述取 accountID 的最小接口（避免指纹模块与 auth 包强耦合）。
type authAccountLike interface {
	GetAccountID() string
}

// accountIDOf 取账号 ID（接口断言失败返回空串）。
func accountIDOf(a authAccountLike) string {
	if a == nil {
		return ""
	}
	return a.GetAccountID()
}

// turnStateProvenance 从响应头取出的 turn state 解析结构（SSE 头内嵌 base64 JSON）。
type turnStateProvenance struct {
	RelationshipSeed string `json:"relationship_seed,omitempty"`
	AccountID        string `json:"account_id,omitempty"`
	ClientSession    string `json:"client_session,omitempty"`
	IssuedAt         int64  `json:"issued_at,omitempty"`
}

// parseTurnStateProvenance 解析 x-codex-turn-state 头，提取溯源字段。
// Codex 原始 turn-state 为不透明 JSON；sub2api 在其上叠加 relationship_seed/account_id 派生字段，
// 以便代理换号时判断是否需要剥离。解析失败返回 (nil,false)。
func parseTurnStateProvenance(raw string) (*turnStateProvenance, bool) {
	if raw == "" {
		return nil, false
	}
	// 解 base64：Codex 客户端与上游间的 turn-state 为 base64 JSON（URL-safe padding 两种都兼容）
	decoded, err := base64Decode(raw)
	if err != nil {
		return nil, false
	}
	var out turnStateProvenance
	if err := json.Unmarshal(decoded, &out); err != nil {
		return nil, false
	}
	if out.RelationshipSeed == "" {
		return nil, false
	}
	return &out, true
}

// rewriteTurnStateForClient 在转发给客户端前改写 turn-state 的溯源字段（记录被客户端回传的源头）。
// 若 header 为空或不含可解析溯源字段，返回原值（客户端看到原始 turn-state，回传时由守卫处理剥离）。
// NOTE：仅当传入 seed/account 与原有溯源不一致时才改写；一致时原样返回，保护既有上下文标识不被刷新。
func rewriteTurnStateForClient(raw, relationshipSeed, accountID, clientSessionID string) string {
	if raw == "" || relationshipSeed == "" || accountID == "" {
		return raw
	}
	provenance, ok := parseTurnStateProvenance(raw)
	if ok &&
		provenance.RelationshipSeed == relationshipSeed &&
		provenance.AccountID == accountID &&
		provenance.ClientSession == clientSessionID {
		return raw
	}
	if ok {
		provenance.RelationshipSeed = relationshipSeed
		provenance.AccountID = accountID
		provenance.ClientSession = clientSessionID
		provenance.IssuedAt = time.Now().UnixMilli()
		if b, err := json.Marshal(provenance); err == nil {
			return base64Encode(b)
		}
	}
	// 原值非我们认识的格式：保持透传
	return raw
}

// guardEchoFn 构造 turn-state 出站守卫：
// 当 x-codex-turn-state 的溯源 seed 与当前账号不匹配（failover 换号）时剥离该回带头，避免跨账号串用 turn 上下文。
func guardEchoFn(seed string, account authAccountLike) func(h http.Header) {
	return func(h http.Header) {
		raw := h.Get(CodexTurnStateHeader)
		if raw == "" {
			return
		}
		if provenanceMatches(raw, seed, account) {
			return
		}
		h.Del(CodexTurnStateHeader)
	}
}

// turnStateRelationshipSeed 构造溯源键：apiKeyID + "\x00" + clientSessionID。
func turnStateRelationshipSeed(apiKeyID, clientSessionID string) string {
	return apiKeyID + "\x00" + clientSessionID
}

// RelationshipSeed 供 handler 层构造 turn-state 溯源键（apiKeyID + "\x00" + clientSessionID）。
func RelationshipSeed(apiKeyID, clientSessionID string) string {
	return turnStateRelationshipSeed(apiKeyID, clientSessionID)
}

// RewriteTurnStateForClient 供 handler 层在转发给客户端前改写 turn-state 溯源字段。
func RewriteTurnStateForClient(raw, relationshipSeed, accountID, clientSessionID string) string {
	return rewriteTurnStateForClient(raw, relationshipSeed, accountID, clientSessionID)
}

// turnStateTTL 默认 turn-state 溯源记录 TTL。
const turnStateTTL = time.Hour

// getCodexTurnStateFromHeaders 从上游响应头提取 turn-state。
func getCodexTurnStateFromHeaders(h http.Header) string {
	if h == nil {
		return ""
	}
	return h.Get(CodexTurnStateHeader)
}

// base64Decode 解 base64（兼容标准与 URL-safe）。
func base64Decode(s string) ([]byte, error) {
	// 使用标准库 base64；URL-safe 形式去补齐后同样可用 Raw 变体兜底
	s = strings.TrimSpace(s)
	if d, err := b64std.DecodeString(s); err == nil {
		return d, nil
	}
	if d, err := b64url.DecodeString(s); err == nil {
		return d, nil
	}
	// 去掉 padding 再试 Raw 变体
	if d, err := b64stdRaw.DecodeString(s); err == nil {
		return d, nil
	}
	if d, err := b64urlRaw.DecodeString(s); err == nil {
		return d, nil
	}
	return nil, errTurnStateDecode
}

// base64Encode 使用 URL-safe（无 padding）编码，与 Codex/OpenAI 通用格式一致。
func base64Encode(b []byte) string {
	return b64url.EncodeToString(b)
}