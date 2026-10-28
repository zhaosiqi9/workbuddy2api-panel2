// headers_parity_test.go 2026-10-28 后续修复的回归：六项与 5.9.3 真机的头级偏差。
//
// 前一批（cn_contract_test.go）钉的是「哪些头出现 / 关键值」；本文件钉的是**形态与
// 条件分支**——派生 ID 必须是 UUIDv7、身份头按 id_source 条件触发、根请求头独立于
// 轮级聚合键、X-Agent-Type 分场景、X-Private-Data 有 true 分支、baggage 带运行时键。
package upstream

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
)

// b64url JWT 段编码（无填充）。
func b64url(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

// decodeUserinfo 解 X-Userinfo 头为 map[string]string。
func decodeUserinfo(t *testing.T, req *http.Request) map[string]string {
	t.Helper()
	raw := req.Header.Get("X-Userinfo")
	if raw == "" {
		t.Fatal("X-Userinfo 未发送")
	}
	dec, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		t.Fatalf("X-Userinfo 不是标准 base64: %v", err)
	}
	var m map[string]string
	if err := json.Unmarshal(dec, &m); err != nil {
		t.Fatalf("X-Userinfo 不是 JSON 对象: %v (%s)", err, dec)
	}
	return m
}

// TestDerivedConversationIDsAreV7 派生分支（有会话源）的两个会话头必须是时间有序
// UUIDv7——真机恒 `01a1815d-dacf-75f1-…`，旧实现派生时摆 v4 位（第 13 位 4）。
func TestDerivedConversationIDsAreV7(t *testing.T) {
	c := &Client{}
	a := &auth.Auth{UID: "85329a86-a3e7-448a-9666-ec34e0bdfb67", AccessToken: "at"}
	meta := ChatMeta{CacheKeyID: "sess-v7"}

	first := httptest.NewRequest(http.MethodPost, "https://example.invalid/v2/chat/completions", nil)
	c.ChatHeaders(first, a, "", meta)
	second := httptest.NewRequest(http.MethodPost, "https://example.invalid/v2/chat/completions", nil)
	c.ChatHeaders(second, a, "", meta)

	for _, h := range []string{"X-Conversation-ID", "acp-connection-id"} {
		got := first.Header.Get(h)
		if !uuidV7Re.MatchString(got) {
			t.Errorf("%s=%q 不是 UUIDv7（派生分支也必须 48bit 时间位 + ver 7）", h, got)
		}
		if again := second.Header.Get(h); again != got {
			t.Errorf("%s 跨请求不稳定：%q vs %q（同账号同会话必须同值，否则缓存槽每轮换）", h, got, again)
		}
	}
	if first.Header.Get("X-Conversation-ID") == first.Header.Get("acp-connection-id") {
		t.Error("会话 ID 与 acp-connection-id 不能同值（官方是两个独立值）")
	}
}

// TestBuildTimeOrderedUUID 派生函数本身：v7 形态 / 稳定 / 跨会话跨 purpose 互不相同 /
// 时间位落在合理窗口（不是 1974 或 5000 年那种一眼假的值）。
func TestBuildTimeOrderedUUID(t *testing.T) {
	const uid = "u-1"
	got := buildTimeOrderedUUID(uid, "conv-1", "acp")
	if !uuidV7Re.MatchString(got) {
		t.Fatalf("形态: %q", got)
	}
	if again := buildTimeOrderedUUID(uid, "conv-1", "acp"); again != got {
		t.Fatalf("不稳定: %q vs %q", got, again)
	}
	if other := buildTimeOrderedUUID(uid, "conv-2", "acp"); other == got {
		t.Fatal("跨会话不该同值")
	}
	if other := buildTimeOrderedUUID(uid, "conv-1", ""); other == got {
		t.Fatal("不同 purpose 不该同值")
	}
	if other := buildTimeOrderedUUID("u-2", "conv-1", "acp"); other == got {
		t.Fatal("跨账号不该同值（隔离约束）")
	}
	// 时间位：48 bit = 前 8 hex + 第 10~13 位 4 hex，解出来必须落在锚点窗口内
	// （锚 2026-01-01 + 0..24 个月），否则就是越界到 1974 / 5000 年的假值。
	ms, err := strconv.ParseInt(got[0:8]+got[9:13], 16, 64)
	if err != nil {
		t.Fatalf("解时间位失败: %v (%q)", err, got)
	}
	const lo, hi = int64(1767225600000), int64(1767225600000) + int64(24*30*24*60*60*1000)
	if ms < lo || ms > hi {
		t.Fatalf("时间位 %d 越界（应在 %d..%d）", ms, lo, hi)
	}
}

// TestIdentityHeadersIdSource 带 id_source 声明的个人号：发 X-Id-Source，且 X-Userinfo
// 带 id_source 段（不带 owner_uin，因为个人号没有企业）。
func TestIdentityHeadersIdSource(t *testing.T) {
	payload := `{"id_source":"sso_login"}`
	a := &auth.Auth{UID: "u1", AccessToken: "h." + b64url(payload) + ".s"}
	req := httptest.NewRequest(http.MethodPost, "https://example.invalid/v2/chat/completions", nil)
	New().injectIdentityHeaders(req, a)

	if got := req.Header.Get("X-Id-Source"); got != "sso_login" {
		t.Fatalf("X-Id-Source=%q want sso_login", got)
	}
	m := decodeUserinfo(t, req)
	if m["id_source"] != "sso_login" {
		t.Errorf("X-Userinfo 缺 id_source 段: %v", m)
	}
	if m["uin"] != "u1" {
		t.Errorf("uin=%q want u1", m["uin"])
	}
	if _, ok := m["owner_uin"]; ok {
		t.Errorf("个人号不该写 owner_uin: %v", m)
	}
	if _, ok := m["token_source"]; ok {
		t.Errorf("个人号不该写 token_source: %v", m)
	}
}

// TestRootRequestIDSessionScoped X-Root-Request-ID 独立于轮级聚合键；缺省回落不丢头。
func TestRootRequestIDSessionScoped(t *testing.T) {
	c := &Client{}
	a := &auth.Auth{UID: "u1", AccessToken: "at"}

	req := httptest.NewRequest(http.MethodPost, "https://example.invalid/v2/chat/completions", nil)
	c.ChatHeaders(req, a, "", ChatMeta{
		ConversationID: "c1", ConversationRequestID: "turn-id", RootRequestID: "session-root",
	})
	if got := req.Header.Get("X-Root-Request-ID"); got != "session-root" {
		t.Errorf("X-Root-Request-ID=%q want session-root（会话根，不是轮级键）", got)
	}
	if req.Header.Get("X-Conversation-Request-ID") != "turn-id" {
		t.Error("轮级聚合键被改了")
	}

	// 无会话根 → 回落轮级值，头仍然在（真机 chat 7/7 都带）。
	req2 := httptest.NewRequest(http.MethodPost, "https://example.invalid/v2/chat/completions", nil)
	c.ChatHeaders(req2, a, "", ChatMeta{ConversationID: "c1", ConversationRequestID: "turn-id"})
	if got := req2.Header.Get("X-Root-Request-ID"); got != "turn-id" {
		t.Errorf("无会话根时应回落轮级值，got=%q", got)
	}
}

// TestAgentTypePerScene X-Agent-Type 分场景：缺省 main，辅助链 auxiliary，且 baggage
// 里的 x-agent-intent 与同名头同值。
func TestAgentTypePerScene(t *testing.T) {
	c := &Client{}
	a := &auth.Auth{UID: "u1", AccessToken: "at"}

	main := httptest.NewRequest(http.MethodPost, "https://example.invalid/v2/chat/completions", nil)
	c.ChatHeaders(main, a, "", ChatMeta{ConversationID: "c1"})
	if got := main.Header.Get("X-Agent-Type"); got != "main" {
		t.Errorf("缺省 X-Agent-Type=%q want main", got)
	}

	aux := httptest.NewRequest(http.MethodPost, "https://example.invalid/v2/chat/completions", nil)
	c.ChatHeaders(aux, a, "", ChatMeta{ConversationID: "c1", AgentType: AgentTypeAuxiliary})
	if got := aux.Header.Get("X-Agent-Type"); got != "auxiliary" {
		t.Errorf("辅助链 X-Agent-Type=%q want auxiliary", got)
	}
}

// TestPrivateDataTrueBranch 企业号发 X-Private-Data: true（企业版默认关模型优化）。
func TestPrivateDataTrueBranch(t *testing.T) {
	c := &Client{}
	ent := &auth.Auth{UID: "u1", EnterpriseID: "ent-1", AccessToken: "at"}
	req := httptest.NewRequest(http.MethodPost, "https://example.invalid/v2/chat/completions", nil)
	c.ChatHeaders(req, ent, "", ChatMeta{ConversationID: "c1"})
	if got := req.Header.Get("X-Private-Data"); got != "true" {
		t.Errorf("企业号 X-Private-Data=%q want true", got)
	}
	personal := httptest.NewRequest(http.MethodPost, "https://example.invalid/v2/chat/completions", nil)
	c.ChatHeaders(personal, &auth.Auth{UID: "u2", AccessToken: "at"}, "", ChatMeta{ConversationID: "c1"})
	if got := personal.Header.Get("X-Private-Data"); got != "false" {
		t.Errorf("个人号 X-Private-Data=%q want false", got)
	}
}

// TestBaggageCarriesRuntimeKeys baggage 必须承载 x-agent-runtime / x-agent-intent
// （客户端 resolveAgentRuntime / resolveRequestScene 从 baggage 读这两项）。
func TestBaggageCarriesRuntimeKeys(t *testing.T) {
	c := &Client{}
	req := httptest.NewRequest(http.MethodPost, "https://example.invalid/v2/chat/completions", nil)
	c.ChatHeaders(req, &auth.Auth{UID: "u1", AccessToken: "at"}, "", ChatMeta{ConversationID: "c1", ConversationRequestID: "r1"})
	bg := req.Header.Get("baggage")
	for _, want := range []string{
		"codebuddy.session_id=c1", "codebuddy.conversation_request_id=r1",
		"x-agent-runtime=" + req.Header.Get("X-Agent-Runtime"),
		"x-agent-intent=" + req.Header.Get("X-Agent-Intent"),
	} {
		if !strings.Contains(bg, want) {
			t.Errorf("baggage 缺 %q: %s", want, bg)
		}
	}
	// 与同名头同值（头体一致的硬要求）。
	if req.Header.Get("X-Agent-Runtime") == "" || req.Header.Get("X-Agent-Intent") == "" {
		t.Error("X-Agent-Runtime / X-Agent-Intent 头本身必须存在")
	}
}
