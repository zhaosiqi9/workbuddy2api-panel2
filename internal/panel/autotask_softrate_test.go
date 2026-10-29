package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/pool"
	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
)

// softRateFake 假上游：前 fail 次任务列表回 429（soft_rate），之后正常下发。
type softRateFake struct {
	mu    sync.Mutex
	fail  int
	calls int
}

func (f *softRateFake) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.calls++
		shouldFail := f.fail > 0
		if shouldFail {
			f.fail--
		}
		f.mu.Unlock()
		if shouldFail {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"code":6004,"msg":"rate limit exceeded, please retry later"}`))
			return
		}
		body, _ := json.Marshal(map[string]any{"code": 0, "msg": "OK", "data": map[string]any{
			"tasks": []any{map[string]any{"task_code": "chat_5", "current": 1, "target": 5}},
		}})
		_, _ = w.Write(body)
	}
}

func softRatePanel(t *testing.T, f *softRateFake) (*Panel, *auth.Auth) {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL}
	a := &auth.Auth{UID: "cn-1", Nickname: "cn", AccessToken: "at"}
	pl := pool.New("")
	pl.Add(a)
	return New(Config{Pool: pl, Upstream: up}), a
}

// 撞两次 429 后第三次成功：任务读得到，不再直接把「查询任务失败」写进结果。
func TestTaskByCodeRetriesOnSoftRate(t *testing.T) {
	oldGap, oldRetries := softRateGap, softRateRetries
	softRateGap, softRateRetries = 0, 2
	t.Cleanup(func() { softRateGap, softRateRetries = oldGap, oldRetries })

	f := &softRateFake{fail: 2}
	p, a := softRatePanel(t, f)
	task, err := p.taskByCode(a, "chat_5")
	if err != nil {
		t.Fatalf("软限流应等待重试后成功，实际 err=%v", err)
	}
	if task == nil || task.TaskCode != "chat_5" {
		t.Fatalf("task=%+v want chat_5", task)
	}
	if f.calls != 3 {
		t.Errorf("上游调用=%d want 3（首次 + 2 次重试）", f.calls)
	}
}

// 重试用尽后记冷却：同一轮后续动作快失败，不再打上游也不再睡窗口——
// 否则一轮几十个动作各睡一遍，后台作业会卡几十分钟还占着账号锁。
func TestTaskByCodeSoftRateExhaustedFailsFast(t *testing.T) {
	oldGap, oldRetries := softRateGap, softRateRetries
	softRateGap, softRateRetries = 0, 2
	t.Cleanup(func() { softRateGap, softRateRetries = oldGap, oldRetries })

	f := &softRateFake{fail: 999}
	p, a := softRatePanel(t, f)
	if _, err := p.taskByCode(a, "chat_5"); err == nil {
		t.Fatal("重试用尽应返回错误")
	} else if !isSoftRateErr(err) {
		t.Errorf("err=%v 应仍可识别为软限流（调用方据此提示稍后重跑）", err)
	}
	first := f.calls
	if first != 1+softRateRetries {
		t.Errorf("上游调用=%d want %d", first, 1+softRateRetries)
	}
	if d := p.softRateGate("cn-1"); d <= 0 {
		t.Errorf("重试用尽应记冷却窗口，实际剩余=%v", d)
	}
	if _, err := p.taskByCode(a, "chat_5"); err == nil {
		t.Error("冷却窗口内应快失败")
	}
	if f.calls != first {
		t.Errorf("冷却窗口内不应再打上游，calls %d -> %d", first, f.calls)
	}
}

// 等待时长：上游 Retry-After 优先，缺省按次数递增，两者都封顶 softRateGapMax。
func TestSoftRateWaitPrefersRetryAfterAndCaps(t *testing.T) {
	oldGap := softRateGap
	softRateGap = 10 * time.Second
	t.Cleanup(func() { softRateGap = oldGap })

	if got := softRateWait(&upstream.Error{Kind: upstream.ErrSoftRate, RetryAfter: 40 * time.Second}, 1); got != 40*time.Second {
		t.Errorf("Retry-After 优先：got=%v want 40s", got)
	}
	if got := softRateWait(&upstream.Error{Kind: upstream.ErrSoftRate}, 2); got != 20*time.Second {
		t.Errorf("无 Retry-After 按次数递增：got=%v want 20s", got)
	}
	if got := softRateWait(&upstream.Error{Kind: upstream.ErrSoftRate, RetryAfter: time.Hour}, 1); got != softRateGapMax {
		t.Errorf("封顶：got=%v want %v", got, softRateGapMax)
	}
}
