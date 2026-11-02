package pool

import (
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
)

// 窗口用量调度（issue #112）：只按 credits 加权时，「烧掉百万 token」与「余额掉几分」
// 在权重上几乎无差别，热点号会一路烧到 6004 限额而同期其他号闲置。这里钉住三件事：
// 记账口径、窗口过期归零、选号确实被降权。
func TestBalanceWindowAccumulatesAndExpires(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "a1"})
	p.SetBalanceWindow(time.Hour)
	p.NoteModelCost("a1", "m", 0, 1000)
	p.NoteModelCost("a1", "m", 0, 500)

	p.mu.Lock()
	got := p.byUID["a1"].windowTokens("m", p.balanceWindow, time.Now())
	if got != 1500 {
		t.Fatalf("窗口内用量应为 1500，得到 %d", got)
	}
	// 另一个模型独立成账（调度是 per (账号, 模型) 的）。
	if other := p.byUID["a1"].windowTokens("other", p.balanceWindow, time.Now()); other != 0 {
		t.Fatalf("别的模型不该共享窗口用量，得到 %d", other)
	}
	// 窗口过期 → 归零（不是逐次衰减，是整体翻篇）。
	if expired := p.byUID["a1"].windowTokens("m", p.balanceWindow, time.Now().Add(2*time.Hour)); expired != 0 {
		t.Fatalf("窗口过期后应归零，得到 %d", expired)
	}
	p.mu.Unlock()

	// 窗口过期后再记一笔：从零起算，不叠加旧窗口的量。
	p.NoteModelCost("a1", "m", 0, 200)
	p.mu.Lock()
	if got := p.byUID["a1"].windowTokens("m", p.balanceWindow, time.Now()); got > 1500+200 {
		t.Fatalf("新窗口应重新起算，得到 %d（旧窗口未被丢弃）", got)
	}
	p.mu.Unlock()
}

// 关停（0）时完全不记账：不花钱维护没人用的运行态。
func TestBalanceWindowOffRecordsNothing(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "a1"})
	p.SetBalanceWindow(0)
	p.NoteModelCost("a1", "m", 0, 1000)
	p.mu.Lock()
	defer p.mu.Unlock()
	if got := p.byUID["a1"].windowTokens("m", time.Hour, time.Now()); got != 0 {
		t.Fatalf("关停后不该记账，得到 %d", got)
	}
}

// 选号层：两号余额/闲置完全相同（权重并列，靠 uid 兜底排序），把 a1 记为热点后，
// 同一个 randInt64N=0 的确定性抽签应改选 a2。这条同时验证「降权进了实际抽签」，
// 而不只是进了 top5 排序——只进排序时抽签仍按原权重，本用例会红。
func TestBalanceWindowLeansOffHotAccount(t *testing.T) {
	withNoPickGap(t)
	now := time.Now()
	newPool := func() *Pool {
		p := New("")
		p.SetBalanceWindow(time.Hour)
		p.SetCostExploreInterval(0) // 关掉探索改道：本用例只测用量降权，不让 tier 探索掩盖它
		for _, uid := range []string{"a1", "a2"} {
			p.Add(&auth.Auth{UID: uid})
			p.SetCreditsDetailed(uid, 100, 100, 0, time.Time{}, 0)
			// 两号都留一笔免费观测：否则「无观测」方会被打进 tier 1，选号结果与用量无关。
			p.NoteModelCost(uid, "m", 0, 1)
			// 闲置权重钉成同一值，避免两次 pick 之间的时钟漂移影响排序。
			p.mu.Lock()
			p.byUID[uid].lastUsed = now.Add(-2 * time.Hour)
			p.mu.Unlock()
		}
		p.randInt64N = func(int64) int64 { return 0 } // 恒取第一个候选 = 权重最高者
		return p
	}

	// 基线：零用量时权重并列，uid 字典序兜底 → a1。
	if got := newPool().Pick(); got == nil || got.UID != "a1" {
		t.Fatalf("零用量基线应选 a1（uid 兜底），得到 %v", got)
	}

	// a1 烧掉 9000 token，a2 零用量 → 均值 4500，a1 除数 3、a2 除数 1 → a2 权重更高。
	p := newPool()
	for i := 0; i < 9; i++ {
		p.NoteModelCost("a1", "m", 0, 1000)
	}
	if got := p.PickExcludingForModel(nil, "m"); got == nil || got.UID != "a2" {
		t.Fatalf("热点号应被降权到次席，得到 %v", got)
	}

	// 两个号都烧同样多 → 用量不再有区分度，回到 uid 兜底 a1（相对量纲，不是绝对阈值）。
	p2 := newPool()
	for i := 0; i < 9; i++ {
		p2.NoteModelCost("a1", "m", 0, 1000)
		p2.NoteModelCost("a2", "m", 0, 1000)
	}
	if got := p2.PickExcludingForModel(nil, "m"); got == nil || got.UID != "a1" {
		t.Fatalf("等量用量时应回到 uid 兜底 a1，得到 %v", got)
	}
}
