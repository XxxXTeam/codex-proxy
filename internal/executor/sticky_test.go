package executor

import (
	"testing"
	"time"
)

/* testStickyBody 构造带粘性信号的请求体（previous_response_id 优先） */
func testStickyBody(prev string) []byte {
	return []byte(`{"previous_response_id":"` + prev + `","model":"gpt-5.2-codex"}`)
}

/* TestStickySeedForPrevious 会话延续键：previous_response_id 前缀 */
func TestStickySeedForPrevious(t *testing.T) {
	key := seedFor(testStickyBody("resp_a1"))
	if key != "prev:resp_a1" {
		t.Fatalf("seek 期望 prev:resp_a1 got=%s", key)
	}
}

/* TestStickySeedForSession 显式会话信号键（无 previous_response_id 时 candidate 顺序优先） */
func TestStickySeedForSession(t *testing.T) {
	key := seedFor([]byte(`{"client_metadata":{"session_id":"cs-1"}}`))
	if key != "session:cs-1" {
		t.Fatalf("session 键不符 got=%s", key)
	}
	key = seedFor([]byte(`{"session_id":"s-x"}`))
	if key != "session:s-x" {
		t.Fatalf("显式 session_id 键不符 got=%s", key)
	}
	if key := seedFor([]byte(`{"model":"m"}`)); key != "" {
		t.Fatalf("无粘性信号应返回空 got=%s", key)
	}
}

/* TestStickyTableRegisterAccount if 启用后注册返回绑定键且按会话可读回账号 */
func TestStickyTableRegisterAccount(t *testing.T) {
	s := NewSessionStickyTable()
	if s.Enabled() {
		t.Fatal("默认未启用")
	}
	s.SetEnabled(true)
	if !s.Enabled() {
		t.Fatal("启用后应返回 true")
	}
	acc := &stubFPAccount{accountID: "acc-1"}
	body := testStickyBody("resp_x")
	if key := s.Register(body, acc); key != "prev:resp_x" {
		t.Fatalf("注册键不符 got=%s", key)
	}
	if got := s.AccountID(body); got != "acc-1" {
		t.Fatalf("AccountID 不符 got=%s", got)
	}
	if got := s.FilePath(body); got != "" {
		t.Fatalf("stub 账号无 FilePath 应为空 got=%s", got)
	}
}

/* TestStickyTableDisabledNoRecord 未启用时注册与读取均不生效（幂等关闭） */
func TestStickyTableDisabledNoRecord(t *testing.T) {
	s := NewSessionStickyTable() /* enabled=false 默认 */
	body := testStickyBody("resp_x")
	if key := s.Register(body, &stubFPAccount{accountID: "acc-1"}); key != "" {
		t.Fatalf("未启用注册不应返回键 got=%s", key)
	}
	if got := s.AccountID(body); got != "" {
		t.Fatalf("未启用读取不应有记录 got=%s", got)
	}
}

/* TestStickyTableOverride 同键再次注册应覆盖为新账号（最近一次成功绑定为准） */
func TestStickyTableOverride(t *testing.T) {
	s := NewSessionStickyTable()
	s.SetEnabled(true)
	body := testStickyBody("resp_y")
	s.Register(body, &stubFPAccount{accountID: "acc-old"})
	s.Register(body, &stubFPAccount{accountID: "acc-new"})
	if got := s.AccountID(body); got != "acc-new" {
		t.Fatalf("覆盖后应返回新账号 got=%s", got)
	}
}

/* TestStickyTableTTLExpiry 过期记录应返回空且被清理（负 TTL 模拟） */
func TestStickyTableTTLExpiry(t *testing.T) {
	s := NewSessionStickyTable()
	s.SetEnabled(true)
	body := testStickyBody("resp_z")
	s.Register(body, &stubFPAccount{accountID: "acc-1"})
	s.mu.Lock()
	s.ttl = -1
	s.mu.Unlock()
	if got := s.AccountID(body); got != "" {
		t.Fatalf("过期记录应视为不可用 got=%s", got)
	}
	if got := s.FilePath(body); got != "" {
		t.Fatalf("过期记录 FilePath 应为空 got=%s", got)
	}
}

/* TestStickyTableReportAndReset 关闭迁移路径：输出快照并清空（诊断用不丢绑定） */
func TestStickyTableReportAndReset(t *testing.T) {
	s := NewSessionStickyTable()
	s.SetEnabled(true)
	body := testStickyBody("resp_r")
	s.Register(body, &stubFPAccount{accountID: "acc-1"})
	out := s.ReportAndReset()
	if len(out) != 1 {
		t.Fatalf("快照应含 1 条 got=%d", len(out))
	}
	if out[0][0] != "prev:resp_r" || out[0][1] != "acc-1" {
		t.Fatalf("快照内容不符 %v", out[0])
	}
	if len(s.records) != 0 {
		t.Fatal("重置后表应为空")
	}
}

/* TestStickyHashDeterministic 摘要确定性且短长度 */
func TestStickyHashDeterministic(t *testing.T) {
	a := stickyHash("prev:r1", "acc")
	b := stickyHash("prev:r1", "acc")
	if a != b || len(a) != 12 {
		t.Fatalf("摘要应确定性且 12 字符 got=%s", a)
	}
	if stickyHash("x") == stickyHash("y") {
		t.Fatal("不同输入不应碰撞")
	}
}

/* TestStickyLastSeenUpdate 成功注册后最近成功时间应刷新（非零且单调） */
func TestStickyLastSeenUpdate(t *testing.T) {
	s := NewSessionStickyTable()
	s.SetEnabled(true)
	body := testStickyBody("resp_t")
	s.Register(body, &stubFPAccount{accountID: "acc-1"})
	first := s.LastSeen(body)
	if first.IsZero() {
		t.Fatal("注册后 lastSeen 不应为零值")
	}
	time.Sleep(5 * time.Millisecond)
	s.Register(body, &stubFPAccount{accountID: "acc-1"})
	second := s.LastSeen(body)
	if !second.After(first) {
		t.Fatal("再次注册应刷新 lastSeen")
	}
}

/* TestStickyTableNilSafe nil 表各项方法安全调用 */
func TestStickyTableNilSafe(t *testing.T) {
	var s *SessionStickyTable
	if s.Enabled() {
		t.Fatal("nil 表不应启用")
	}
	s.SetEnabled(true)
	if key := s.Register(testStickyBody("resp_n"), &stubFPAccount{accountID: "a"}); key != "" {
		t.Fatalf("nil 表注册应返回空 got=%s", key)
	}
	if got := s.AccountID(testStickyBody("resp_n")); got != "" {
		t.Fatalf("nil 表读取应为空 got=%s", got)
	}
	s.Sweep()
	if out := s.ReportAndReset(); out != nil {
		t.Fatal("nil 表快照应为 nil")
	}
}

/* TestStickyFileName 粘性键前缀仅用于内部记录；请求体不包含前缀时读取应匹配 */
func TestStickyFileName(t *testing.T) {
	s := NewSessionStickyTable()
	s.SetEnabled(true)
	body := testStickyBody("resp_f1")
	s.Register(body, &stubFPAccount{accountID: "acc-fp"})
	if got := s.AccountID(testStickyBody("resp_f1")); got != "acc-fp" {
		t.Fatalf("同键读取应一致 got=%s", got)
	}
	if got := s.AccountID(testStickyBody("resp_f2")); got != "" {
		t.Fatalf("异键读取应为空 got=%s", got)
	}
}