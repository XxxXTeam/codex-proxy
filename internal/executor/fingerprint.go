package executor

import (
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"codex-proxy/internal/auth"

	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// CodexFingerprintMode 指纹收敛模式，与 sub2api 的 codex_fingerprint_mode 对齐：
//   - off：不收敛（透传客户端原样标识）
//   - device：收敛 installation_id（账号级指纹种子派生）
//   - session：device + 全局收敛的 session_id/thread_id（所有 API 路径共享）
//   - full：保留的兼容模式，与 session 使用同一全局 session/thread 收敛范围
//
// 默认 off：仅当 Config 显式配置了 codex-fingerprint-mode 与 codex-fingerprint-seed 时启用。
type CodexFingerprintMode string

const (
	// CodexFingerprintOff 关闭指纹收敛（默认）
	CodexFingerprintOff CodexFingerprintMode = "off"
	// CodexFingerprintDevice 仅收敛 installation_id
	CodexFingerprintDevice CodexFingerprintMode = "device"
	// CodexFingerprintSession 收敛 installation_id + session_id
	CodexFingerprintSession CodexFingerprintMode = "session"
	// CodexFingerprintFull 收敛 installation_id + session_id + thread_id
	CodexFingerprintFull CodexFingerprintMode = "full"
)

// CodexFingerprintIDs 单次出站请求的确定性收敛 ID 集合（由 resolveCodexFingerprintIDs 解析）。
type CodexFingerprintIDs struct {
	// Mode 收敛模式
	Mode CodexFingerprintMode
	// InstallationID 收敛后的 installation_id（device/session/full 模式派生）
	InstallationID string
	// SessionID 收敛后的会话标识（session/full 模式派生；否则回退 installationID）
	SessionID string
	// ThreadID 收敛后的线程标识（full 模式派生；否则回退 sessionID）
	ThreadID string
	// WindowID 每请求窗口标识：ThreadID + ":0"
	WindowID string
	// TurnID 每请求随机 turn 标识（UUIDv7）
	TurnID string
	// TurnStartedAtUnixMs turn 开始时间戳（毫秒）
	TurnStartedAtUnixMs int64
	// OriginalBodySessionID 原始请求体 client_metadata.session_id（未收敛值，用于 prompt_cache_key 判定）
	OriginalBodySessionID string
	// OriginalBodySessionIDCaptured 是否已捕获原始 body session_id（只捕获一次）
	OriginalBodySessionIDCaptured bool
}

// resolveCodexFingerprintIDs 按收敛模式解析出本次出站请求使用的 ID 集合。
// seed 为全局确定性种子（UUIDv4，来自配置 codex-fingerprint-seed）。
// session/full 模式的 session_id 与 thread_id 只由 seed 派生，因此所有 API 路径共享。
func resolveCodexFingerprintIDs(mode CodexFingerprintMode, seed string, _ string) *CodexFingerprintIDs {
	ids := &CodexFingerprintIDs{Mode: mode, TurnStartedAtUnixMs: timeNowUnixMillis()}
	ids.InstallationID = deriveStableUUIDv4("sub2api:codex-install-id:v2:" + seed)
	switch mode {
	case CodexFingerprintDevice:
		// 与 sub2api resolveCodexFingerprintIDs 的 device 分支一致：仅收敛 installation_id
		return ids
	case CodexFingerprintSession, CodexFingerprintFull:
		/* 所有 API、客户端与账号复用同一配置种子派生的会话标识，保持缓存前缀与上游身份上下文一致。 */
		ids.SessionID = deriveStableUUIDv4("sub2api:codex-session-id:v2:" + seed)
		ids.ThreadID = deriveStableUUIDv4("sub2api:codex-thread-id:v2:" + seed)
		ids.TurnID = newUUIDV7String()
		ids.WindowID = ids.ThreadID + ":0"
		return ids
	}
	return nil
}

// deriveStableUUIDv4 由字符串种子确定性派生 UUIDv4：
// sha256(seed) → 取前 16 字节 → 设置 version=4 / variant 位 → 输出标准 UUIDv4 字符串。
func deriveStableUUIDv4(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	b := append([]byte(nil), sum[:16]...)
	b[6] = (b[6] & 0x0f) | 0x40 // 设置 version=4（UUIDv4）
	b[8] = (b[8] & 0x3f) | 0x80 // 设置 variant=10（RFC 4122）
	return uuid.UUID(b).String()
}

// applyCodexFingerprintHeaders 按预计算的收敛 ID 改写出站 HTTP 头中的设备指纹。
// 与 sub2api applyCodexFingerprintHeaders 语义一致：所有非 off 模式收敛 installation_id；
// device 仅改写 x-codex-installation-id 与 x-codex-turn-metadata 内嵌 installation_id；
// session/full 额外改写 window/client-request/session（连字符+下划线）/thread 头。
func applyCodexFingerprintHeaders(h http.Header, ids *CodexFingerprintIDs) {
	if h == nil || ids == nil {
		return
	}
	h.Set("x-codex-installation-id", ids.InstallationID)
	if ids.Mode == CodexFingerprintDevice {
		rewriteCodexTurnMetadataFields(h, map[string]any{
			"installation_id": ids.InstallationID,
		})
		return
	}
	h.Set("x-codex-window-id", ids.WindowID)
	h.Set("x-client-request-id", ids.ThreadID)
	h.Set("session-id", ids.SessionID)
	h.Set("session_id", ids.SessionID)
	h.Set("thread-id", ids.ThreadID)
	rewriteCodexTurnMetadataFields(h, map[string]any{
		"installation_id":         ids.InstallationID,
		"session_id":              ids.SessionID,
		"thread_id":               ids.ThreadID,
		"turn_id":                 ids.TurnID,
		"window_id":               ids.WindowID,
		"turn_started_at_unix_ms": ids.TurnStartedAtUnixMs,
	})
}

// rewriteCodexTurnMetadataFields 解析 x-codex-turn-metadata 头中的 JSON，
// 替换指定字段后回写。合法对象保留未指定字段（如 sandbox、thread_source）；
// 非法/非对象值重建为最小合法 metadata，避免 flat 与 embedded identity 分裂。
func rewriteCodexTurnMetadataFields(h http.Header, fields map[string]any) {
	raw := strings.TrimSpace(h.Get("x-codex-turn-metadata"))
	if raw == "" {
		return
	}
	var metadata map[string]any
	if err := json.Unmarshal([]byte(raw), &metadata); err != nil || metadata == nil {
		metadata = make(map[string]any, len(fields))
	}
	for k, v := range fields {
		metadata[k] = v
	}
	rebuilt, err := json.Marshal(metadata)
	if err != nil {
		return
	}
	h.Set("x-codex-turn-metadata", string(rebuilt))
}

// applyCodexFingerprintClientMetadataRaw 在原始 JSON 字节上改写 client_metadata 与
// root prompt_cache_key（仅当它等于原始 body session 默认值）。语义与
// sub2api applyCodexFingerprintClientMetadataRaw 逐点一致（含「非对象值整体保留」行为）。
// 返回改写后的字节与是否发生修改；错误时原样返回 body。
func applyCodexFingerprintClientMetadataRaw(body []byte, ids *CodexFingerprintIDs) ([]byte, bool) {
	if len(body) == 0 || ids == nil {
		return body, false
	}
	root := gjson.ParseBytes(body)
	if !root.IsObject() {
		captureCodexFingerprintOriginalBodySessionIDRaw(ids, gjson.Result{})
		return body, false
	}
	existing := map[string]any{}
	if cm := gjson.GetBytes(body, "client_metadata"); cm.IsObject() {
		captureCodexFingerprintOriginalBodySessionIDRaw(ids, gjson.GetBytes(body, "client_metadata.session_id"))
		if err := json.Unmarshal([]byte(cm.Raw), &existing); err != nil {
			// 无法解析 client_metadata：视为无改写语义
			captureCodexFingerprintOriginalBodySessionIDRaw(ids, gjson.Result{})
			return body, false
		}
	} else {
		captureCodexFingerprintOriginalBodySessionIDRaw(ids, gjson.Result{})
	}

	next := body
	modified := false
	if applyCodexFingerprintToClientMetadataMap(existing, ids) {
		raw, err := json.Marshal(existing)
		if err != nil {
			return body, false
		}
		setBody, setErr := sjson.SetRawBytes(next, "client_metadata", raw)
		if setErr != nil {
			return body, false
		}
		next = setBody
		modified = true
	}
	promptCacheKey := gjson.GetBytes(next, "prompt_cache_key")
	if promptCacheKey.Exists() && promptCacheKey.Type == gjson.String &&
		strings.TrimSpace(promptCacheKey.String()) != "" && shouldRewriteCodexFingerprintPromptCacheKey(ids, promptCacheKey.String()) {
		rewritten, err := sjson.SetBytes(next, "prompt_cache_key", ids.SessionID)
		if err == nil {
			next = rewritten
			modified = true
		}
	}
	return next, modified
}

// applyCodexFingerprintToClientMetadataMap 是 client_metadata 改写的共享核心（与 sub2api 一致）。
func applyCodexFingerprintToClientMetadataMap(existing map[string]any, ids *CodexFingerprintIDs) bool {
	if existing == nil || ids == nil {
		return false
	}
	modified := false
	if ids.InstallationID != "" {
		existing["x-codex-installation-id"] = ids.InstallationID
		modified = true
	}
	if ids.Mode == CodexFingerprintDevice {
		rewriteClientMetadataEmbeddedTurnMetadata(existing, map[string]any{
			"installation_id": ids.InstallationID,
		})
		return modified
	}
	existing["session_id"] = ids.SessionID
	existing["thread_id"] = ids.ThreadID
	existing["turn_id"] = ids.TurnID
	existing["x-codex-window-id"] = ids.WindowID
	rewriteClientMetadataEmbeddedTurnMetadata(existing, map[string]any{
		"installation_id":         ids.InstallationID,
		"session_id":              ids.SessionID,
		"thread_id":               ids.ThreadID,
		"turn_id":                 ids.TurnID,
		"window_id":               ids.WindowID,
		"turn_started_at_unix_ms": ids.TurnStartedAtUnixMs,
	})
	return true
}

// rewriteClientMetadataEmbeddedTurnMetadata 改写 client_metadata 中内嵌的
// x-codex-turn-metadata JSON 字符串里的指定字段。非法/非对象值会重建。
func rewriteClientMetadataEmbeddedTurnMetadata(clientMetadata map[string]any, fields map[string]any) {
	raw, ok := clientMetadata["x-codex-turn-metadata"].(string)
	if !ok || raw == "" {
		return
	}
	var metadata map[string]any
	if err := json.Unmarshal([]byte(raw), &metadata); err != nil || metadata == nil {
		metadata = make(map[string]any, len(fields))
	}
	for k, v := range fields {
		metadata[k] = v
	}
	if rebuilt, err := json.Marshal(metadata); err == nil {
		clientMetadata["x-codex-turn-metadata"] = string(rebuilt)
	}
}

// captureCodexFingerprintOriginalBodySessionIDRaw 捕获原始 body client_metadata.session_id（只一次）。
func captureCodexFingerprintOriginalBodySessionIDRaw(ids *CodexFingerprintIDs, value gjson.Result) {
	if ids == nil || ids.OriginalBodySessionIDCaptured {
		return
	}
	ids.OriginalBodySessionIDCaptured = true
	if value.Exists() && value.Type == gjson.String {
		ids.OriginalBodySessionID = strings.TrimSpace(value.String())
	}
}

// shouldRewriteCodexFingerprintPromptCacheKey 判定 root prompt_cache_key 是否改写：
// 仅当它等于原始 body session 默认值（session/full 模式）时改写为收敛 sessionID。
func shouldRewriteCodexFingerprintPromptCacheKey(ids *CodexFingerprintIDs, promptCacheKey string) bool {
	if ids == nil || !ids.OriginalBodySessionIDCaptured || ids.OriginalBodySessionID == "" || ids.SessionID == "" {
		return false
	}
	if ids.Mode != CodexFingerprintSession && ids.Mode != CodexFingerprintFull {
		return false
	}
	return promptCacheKey == ids.OriginalBodySessionID
}

// newUUIDV7String 生成一次性的 UUIDv7（每请求 turn 标识），失败时回退随机 UUIDv4。
func newUUIDV7String() string {
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.NewString()
	}
	return id.String()
}

// timeNowUnixMillis 当前 Unix 毫秒时间戳。
func timeNowUnixMillis() int64 {
	return time.Now().UnixMilli()
}

// fpsIDs 取出指纹 ID 集合（nil 表示 off）。
func fpsIDs(fps *RequestsFingerprint) *CodexFingerprintIDs {
	if fps == nil {
		return nil
	}
	return fps.ids
}

// fpsGuardEcho 取出出站 turn-state 守卫闭包（fps.turnStateSeed 非空且与当前账号 origin 不一致时剥离回带）。
func fpsGuardEcho(fps *RequestsFingerprint, account *auth.Account) func(h http.Header) {
	if fps == nil || fps.turnStateSeed == "" {
		return nil
	}
	return guardEchoFn(fps.turnStateSeed, account)
}

// StickyTable 返回指纹状态携带的会话粘性表（Phase C：按会话绑定账号优先选号）。未携带返回 nil。
func (r *RequestsFingerprint) StickyTable() *SessionStickyTable {
	if r == nil {
		return nil
	}
	return r.sticky
}

// RebindSticky 在出站选号成功后记录会话→账号绑定（仅当表启用且账号键非空）。
// 返回值供日志/统计；表未启用或绑定键缺失时返回空串。
func (r *RequestsFingerprint) RebindSticky(body []byte, acc *auth.Account) string {
	if acc == nil || r == nil || r.sticky == nil {
		return ""
	}
	return r.sticky.Register(body, acc)
}
