package server

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
)

func issue117Auths() (*auth.Auth, *auth.Auth) {
	return &auth.Auth{UID: "a1", AccessToken: "at1", Domain: "www.codebuddy.cn", ExpiresAt: 9999999999},
		&auth.Auth{UID: "a2", AccessToken: "at2", Domain: "www.codebuddy.cn", ExpiresAt: 9999999999}
}

func doChatReq(t *testing.T, h *Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestIssue117OneLocked issue #117 主现场：两个 CN 号，其中一个对该模型 6004 冷却。另一个应照常承接。
func TestIssue117OneLocked(t *testing.T) {
	a1, a2 := issue117Auths()
	p := testPoolWith(a1, a2)
	p.CooldownSoftForModel("a1", time.Minute, time.Now().Add(4*time.Hour), "deepseek-v4.1-flash",
		`{"code":6004,"msg":"您的使用量已超出频率限制，将在 2026-10-29 19:58:03 UTC+8 重置"}`)

	var hit []string
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		hit = append(hit, authz)
		return 200, sseOK, true
	})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := doChatReq(t, h, `{"model":"cn:deepseek-v4.1-flash","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	t.Logf("code=%d uids=%v", rec.Code, hit)
	if rec.Code != 200 {
		t.Errorf("期望 200（a2 健康），得到 %d：%s", rec.Code, rec.Body.String())
	}
	for _, r := range p.ModelLockView() {
		t.Logf("lock: model=%s realm=%s state=%s servable=%d total=%d locked=%d",
			r.Model, r.Realm, r.State, r.Servable, r.Total, r.Locked)
	}
}

// TestIssue117OtherBusyWaitsForSlot issue #117 真实现场：另一号在途占满 → 等一个名额（而不是立刻 503）。
func TestIssue117OtherBusyWaitsForSlot(t *testing.T) {
	oldCap := busyWaitCap
	busyWaitCap = 3 * time.Second
	t.Cleanup(func() { busyWaitCap = oldCap })

	a1, a2 := issue117Auths()
	p := testPoolWith(a1, a2)
	p.SetMaxInFlight(3)
	p.CooldownSoftForModel("a1", time.Minute, time.Now().Add(4*time.Hour), "deepseek-v4.1-flash", "6004 ...")
	for i := 0; i < 3; i++ {
		if !p.Acquire("a2") {
			t.Fatalf("acquire %d 失败", i)
		}
	}
	for _, r := range p.ModelLockView() {
		t.Logf("lock: state=%s servable=%d total=%d locked=%d", r.State, r.Servable, r.Total, r.Locked)
	}
	if !p.SlotWaitForRealm("deepseek-v4.1-flash", "cn") {
		t.Fatal("应判定为「都在途占满」")
	}
	// 200ms 后有一个在途请求收尾（释放名额）—— 请求应等到它并成功。
	go func() {
		time.Sleep(200 * time.Millisecond)
		p.Release("a2")
	}()
	up := newFakeUpstream(t, func(authz string) (int, string, bool) { return 200, sseOK, true })
	h := NewHandler(Config{Pool: p, Upstream: up})
	start := time.Now()
	rec := doChatReq(t, h, `{"model":"cn:deepseek-v4.1-flash","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	t.Logf("code=%d waited=%s body=%.100s", rec.Code, time.Since(start).Round(time.Millisecond), rec.Body.String())
	if rec.Code != 200 {
		t.Errorf("名额释放后应 200，得到 %d：%s", rec.Code, rec.Body.String())
	}
}

// 名额一直不释放时必须收敛回 503（等待有上限，不能把请求挂死）。
func TestIssue117SlotWaitIsBounded(t *testing.T) {
	oldCap := busyWaitCap
	busyWaitCap = 150 * time.Millisecond
	t.Cleanup(func() { busyWaitCap = oldCap })

	a1, a2 := issue117Auths()
	p := testPoolWith(a1, a2)
	p.SetMaxInFlight(3)
	p.CooldownSoftForModel("a1", time.Minute, time.Now().Add(4*time.Hour), "deepseek-v4.1-flash", "6004 ...")
	for i := 0; i < 3; i++ {
		p.Acquire("a2")
	}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) { return 200, sseOK, true })
	h := NewHandler(Config{Pool: p, Upstream: up})
	start := time.Now()
	rec := doChatReq(t, h, `{"model":"cn:deepseek-v4.1-flash","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("等待应被 busyWaitCap 收住，实际 %v", d)
	}
	if rec.Code != 503 || !strings.Contains(rec.Body.String(), "no_healthy_account") {
		t.Errorf("名额始终不释放应 503 no_healthy_account，得到 %d：%s", rec.Code, rec.Body.String())
	}
}
