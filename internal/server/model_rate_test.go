package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/pool"
	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
)

func assertModelCooling(t *testing.T, p *pool.Pool, uid, model string, duration time.Duration) {
	t.Helper()
	s, _ := p.Status(uid)
	if s.Cooling || len(s.RateLimitedModels) != 1 || s.RateLimitedModels[0].Model != model {
		t.Fatalf("expected only %s cooling, status=%+v", model, s)
	}
	remaining := time.Until(s.RateLimitedModels[0].Until)
	if remaining <= duration-3*time.Second || remaining > duration+time.Second {
		t.Fatalf("model cooldown=%s want ~%s", remaining, duration)
	}
}

func TestSoftRatePolicyIsolatesEveryResponseForm(t *testing.T) {
	reset := time.Now().Add(20 * time.Minute).In(upstream.SoftRateResetLoc()).Format("2006-01-02 15:04:05")
	for _, tc := range []struct {
		name, body string
		retry      time.Duration
		want       time.Duration
	}{
		{"plain429", `{"msg":"rate limit"}`, 0, time.Minute},
		{"6004WithoutReset", `{"code":6004,"msg":"model usage limit exceeded"}`, 0, time.Minute},
		{"retryAfter", `{"msg":"rate limit"}`, 5 * time.Minute, 5 * time.Minute},
		{"non6004Reset", `{"code":11140,"msg":"将在 ` + reset + ` UTC+8 重置"}`, time.Minute, 20 * time.Minute},
		{"6004Reset", `{"code":6004,"msg":"将在 ` + reset + ` UTC+8 重置"}`, 0, 20 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := testPoolWith(&auth.Auth{UID: "u1"})
			h := NewHandler(Config{Pool: p, SoftCooldown: time.Minute})
			h.applyErrorPolicy("u1", upstream.ErrSoftRate, tc.body, "kimi-k3.1-pro", &upstream.Error{RetryAfter: tc.retry})
			assertModelCooling(t, p, "u1", "kimi-k3.1-pro", tc.want)
			if p.PickByUIDForModel("u1", "deepseek-v4.1-flash") == nil {
				t.Fatal("unrelated model must remain selectable through sticky path")
			}
			if p.PickExcludingForModel(nil, "kimi-k3.1-pro") != nil {
				t.Fatal("fallback must not bypass model cooldown")
			}
			// 冷却期间的同模型请求必须本地返回 429，不能误报不存在或访问上游。
			if !upstream.IsModelRateLimit(tc.body) {
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"kimi-k3.1-pro","messages":[]}`)))
				if rec.Code != http.StatusTooManyRequests {
					t.Fatalf("cooled model status=%d body=%s", rec.Code, rec.Body)
				}
			}
		})
	}
}

func TestChatKimiRateLimitStillAllowsDeepSeekSameAccount(t *testing.T) {
	calls := 0
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		calls++
		if calls == 1 {
			return 429, `{"msg":"rate limit"}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: time.Minute})
	chat := func(model string) int {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"`+model+`","messages":[]}`)))
		return rec.Code
	}
	if code := chat("cn:kimi-k3.1-pro"); code != 429 {
		t.Fatalf("first status=%d", code)
	}
	if code := chat("kimi-k3.1-pro"); code != 429 {
		t.Fatalf("cooled model status=%d", code)
	}
	if calls != 1 {
		t.Fatal("cooled model must not issue another upstream call")
	}
	if code := chat("deepseek-v4.1-flash"); code != 200 {
		t.Fatalf("other model status=%d", code)
	}
	if calls != 2 {
		t.Fatalf("upstream calls=%d", calls)
	}
	assertModelCooling(t, p, "u1", "kimi-k3.1-pro", time.Minute)
}
