package pool

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
)

func TestModelRateCooldownsIndependentAndPersisted(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.SetSoftRateMax(2 * time.Minute)
	p.CooldownSoftForModel("u1", time.Minute, time.Time{}, "kimi", "429 model rate limit")
	first, _ := p.Status("u1")
	p.CooldownSoftForModel("u1", time.Hour, time.Time{}, "kimi", "429 model rate limit")
	again, _ := p.Status("u1")
	if !first.RateLimitedModels[0].Until.Equal(again.RateLimitedModels[0].Until) {
		t.Fatal("repeat failure extended active cooldown")
	}
	p.CooldownSoftForModel("u1", time.Hour, time.Time{}, "deepseek", "429 model rate limit")
	p.NoteSuccess("u1")
	p.Flush()
	q := New(fp)
	q.Add(&auth.Auth{UID: "u1"})
	s, _ := q.Status("u1")
	if s.Cooling || len(s.RateLimitedModels) != 2 {
		t.Fatalf("reload status=%+v", s)
	}
	for _, m := range []string{"kimi", "deepseek"} {
		if q.PickExcludingForModel(nil, m) != nil || q.PickByUIDForModel("u1", m) != nil {
			t.Fatalf("%s cooldown bypassed", m)
		}
	}
	if q.PickExcludingForModel(nil, "glm") == nil {
		t.Fatal("unrelated model blocked")
	}
	q.CooldownUntilTomorrow4AM("u1", "余额不足")
	if q.PickExcludingForModel(nil, "glm") != nil {
		t.Fatal("account credit exhaustion must still block every model")
	}
}

func TestFallbackCannotBypassModelRateCooldown(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.CooldownSoftForModel("u1", time.Minute, time.Time{}, "kimi", "429 model rate limit")
	// 账号另有降权时会进入全冷却兜底，仍必须遵守模型冷却。
	p.mu.Lock()
	p.byUID["u1"].degradeUntil = time.Now().Add(10 * time.Minute)
	p.mu.Unlock()
	if p.PickExcludingForModel(nil, "kimi") != nil {
		t.Fatal("fallback bypassed the active model cooldown")
	}
	if p.PickExcludingForModel(nil, "deepseek") == nil {
		t.Fatal("fallback should remain available for an unrelated model")
	}
}
