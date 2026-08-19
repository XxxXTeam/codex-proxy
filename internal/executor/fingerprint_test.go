package executor

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

/* 测试用指纹种子（UUIDv4 种子；非真实账号凭据） */
const testFPSeed = "11111111-1111-4111-8111-111111111111"

/* 客户端会话标识（测试夹具） */
const testClientSession = "cs-test-session-001"

func TestResolveCodexFingerprintIDsOff(t *testing.T) {
	if ids := resolveCodexFingerprintIDs(CodexFingerprintOff, testFPSeed, testClientSession); ids != nil {
		t.Error("off 模式应返回 nil")
	}
}

func TestResolveCodexFingerprintIDsDevice(t *testing.T) {
	ids := resolveCodexFingerprintIDs(CodexFingerprintDevice, testFPSeed, testClientSession)
	if ids == nil {
		t.Fatal("device 模式应返回 ID 集合")
	}
	if ids.SessionID != "" || ids.ThreadID != "" {
		t.Error("device 模式不应派生 session/thread")
	}
	/* 同一 seed 输出确定性与格式 */
	want := deriveStableUUIDv4("sub2api:codex-install-id:v2:" + testFPSeed)
	if ids.InstallationID != want {
		t.Errorf("installation_id 不一致: got=%s want=%s", ids.InstallationID, want)
	}
	again := resolveCodexFingerprintIDs(CodexFingerprintDevice, testFPSeed, testClientSession)
	if again.InstallationID != ids.InstallationID {
		t.Error("同一 seed 的 installation_id 应稳定")
	}
}

func TestDeriveStableUUIDv4Format(t *testing.T) {
	id := deriveStableUUIDv4("sub2api:codex-install-id:v2:" + testFPSeed)
	if len(id) != 36 {
		t.Fatalf("长度异常: %s", id)
	}
	if id[14] != '4' {
		t.Errorf("version 位应为 4: %s", id)
	}
	// variant 位：第 17 个字符应为 8/9/a/b
	switch id[19] {
	case '8', '9', 'a', 'b':
	default:
		t.Errorf("variant 位异常: %s", id)
	}
}

func TestApplyCodexFingerprintHeadersSession(t *testing.T) {
	ids := resolveCodexFingerprintIDs(CodexFingerprintSession, testFPSeed, testClientSession)
	h := make(http.Header)
	h.Set("session-id", "client-original-session")
	applyCodexFingerprintHeaders(h, ids)

	if v := h.Get("x-codex-installation-id"); v != ids.InstallationID {
		t.Errorf("x-codex-installation-id 不一致: %s", v)
	}
	if v := h.Get("session_id"); v != ids.SessionID {
		t.Errorf("session_id 不一致: %s", v)
	}
	if v := h.Get("thread-id"); v != ids.ThreadID {
		t.Errorf("thread-id 不一致: %s", v)
	}
	if v := h.Get("x-codex-window-id"); v != ids.WindowID {
		t.Errorf("x-codex-window-id 不一致: %s", v)
	}
}

func TestApplyCodexFingerprintClientMetadataSession(t *testing.T) {
	ids := resolveCodexFingerprintIDs(CodexFingerprintSession, testFPSeed, testClientSession)
	body := []byte(`{"client_metadata":{"session_id":"cs-original","x-codex-turn-metadata":"{\"session_id\":\"x\"}"},"prompt_cache_key":"cs-original"}`)
	next, modified := applyCodexFingerprintClientMetadataRaw(body, ids)
	if !modified {
		t.Fatal("应发生改写")
	}
	var out struct {
		ClientMetadata   map[string]any `json:"client_metadata"`
		PromptCacheKey   string         `json:"prompt_cache_key"`
	}
	if err := json.Unmarshal(next, &out); err != nil {
		t.Fatalf("改写后 JSON 解析失败: %v", err)
	}
	if out.PromptCacheKey != ids.SessionID {
		t.Errorf("prompt_cache_key 应改写为收敛 sessionID: got=%s", out.PromptCacheKey)
	}
	if v := out.ClientMetadata["session_id"]; v != ids.SessionID {
		t.Errorf("client_metadata.session_id 应改写: %v", v)
	}
}

func TestRewriteTurnStateForClient(t *testing.T) {
	// 初始 provenance 与传入 seed/account 不一致：应改写
	raw := base64.RawURLEncoding.EncodeToString([]byte(`{"relationship_seed":"old-seed","account_id":"acc-old"}`))
	seed := "api-key-1\x00cs-1"
	turned := rewriteTurnStateForClient(raw, seed, "acc-1", "cs-1")
	if turned == raw {
		t.Fatal("seed/account 不一致时应改写 turn state")
	}
	decoded, err := base64Decode(turned)
	if err != nil {
		t.Fatalf("改写后解码失败: %v", err)
	}
	var prov turnStateProvenance
	if err := json.Unmarshal(decoded, &prov); err != nil {
		t.Fatalf("改写后 JSON 解析失败: %v", err)
	}
	if prov.RelationshipSeed != seed || prov.AccountID != "acc-1" || prov.ClientSession != "cs-1" {
		t.Errorf("溯源字段不符: %+v", prov)
	}
	if prov.IssuedAt == 0 {
		t.Error("issued_at 应为非零时间戳")
	}

	// 与当前溯源一致：应原样保留，不刷新 issued_at
	unchanged := rewriteTurnStateForClient(turned, seed, "acc-1", "cs-1")
	if unchanged != turned {
		t.Error("一致时应原样保留 turn state")
	}
}

func TestParseTurnStateProvenanceInvalid(t *testing.T) {
	if _, ok := parseTurnStateProvenance(base64.RawURLEncoding.EncodeToString([]byte(`{"x":1}`))); ok {
		t.Error("缺 relationship_seed 不应解析成功")
	}
	if _, ok := parseTurnStateProvenance(base64.RawURLEncoding.EncodeToString([]byte(`{"relationship_seed":"\xzz"}`))); ok {
		t.Error("非法 JSON 转义不应解析成功")
	}
	if _, ok := parseTurnStateProvenance("###not-base64###"); ok {
		t.Error("非 base64 不应解析成功")
	}
	if _, ok := parseTurnStateProvenance(""); ok {
		t.Error("空串不应解析成功")
	}
}

func TestGuardEchoFnStripsMismatch(t *testing.T) {
	seed := "api-key-1\x00cs-1"
	acc := &stubFPAccount{accountID: "acc-1"}
	h := make(http.Header)
	other := `{"relationship_seed":"other-seed","account_id":"acc-2"}`
	h.Set(CodexTurnStateHeader, base64.RawURLEncoding.EncodeToString([]byte(other)))
	fn := guardEchoFn(seed, acc)
	fn(h)
	if h.Get(CodexTurnStateHeader) != "" {
		t.Error("seed/账号不匹配时应剥离 turn-state")
	}
}

/* jsonSeedFor 生成可嵌入 JSON 字符串字面量的 seed 转义（Go 源码字面量含裸 NUL 不合法，故用 helper） */
func jsonSeedFor(seed string) string {
	// relationship_seed 内含 0x00 分隔符；json.Marshal 会将其转为转义序列
	b, _ := json.Marshal(seed)
	s := string(b)
	return s[1 : len(s)-1]
}

func TestGuardEchoFnKeepsMatch(t *testing.T) {
	seed := "api-key-1\x00cs-1"
	raw := base64.RawURLEncoding.EncodeToString([]byte(`{"relationship_seed":"` + jsonSeedFor(seed) + `","account_id":"acc-1"}`))
	h := make(http.Header)
	h.Set(CodexTurnStateHeader, raw)
	fn := guardEchoFn(seed, &stubFPAccount{accountID: "acc-1"})
	fn(h)
	if h.Get(CodexTurnStateHeader) != raw {
		t.Error("seed 与账号匹配时应保留 turn-state")
	}
}

func TestTurnStateStoreRoundTrip(t *testing.T) {
	s := NewTurnStateStore(0)
	seed := "api-key-1\x00cs-1"
	s.Store(seed, "acc-1", "cs-1")
	if got := s.OriginAccountID(seed); got != "acc-1" {
		t.Errorf("OriginAccountID 不符: %s", got)
	}
	if got := s.OriginAccountID("no-such-seed"); got != "" {
		t.Errorf("未知 seed 应返回空串: %s", got)
	}
}

func TestTurnStateStoreTTLExpiry(t *testing.T) {
	s := NewTurnStateStore(0)
	seed := "api-key-1\x00cs-1"
	s.Store(seed, "acc-1", "cs-1")
	// 将 ttl 缩短并回拨 createdAt 模拟过期
	s.mu.Lock()
	s.ttl = -1
	s.records[seed].createdAt = s.records[seed].createdAt.Add(-time.Hour)
	s.mu.Unlock()
	if got := s.OriginAccountID(seed); got != "" {
		t.Errorf("过期记录应返回空串: %s", got)
	}
}

/* stubFPAccount 测试用账号存根（实现 authAccountLike） */
type stubFPAccount struct {
	accountID string
}

func (a *stubFPAccount) GetAccountID() string {
	if a == nil {
		return ""
	}
	return a.accountID
}