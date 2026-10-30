package panel

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/pool"
	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
)

// missingCodeFake 假上游：任务列表只下发 codes 里的 code，其余请求一律 200 空信封。
// 记录所有请求的 path+body，供断言「没有为不存在的任务打上游」。
type missingCodeFake struct {
	codes []string
	mu    sync.Mutex
	seen  []string
	mpErr bool // true = mp 口径任务列表回 500（列表拿不到 → 不筛选）
}

func (f *missingCodeFake) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var buf strings.Builder
		buf.WriteString(r.URL.Path)
		if r.Header.Get("X-Client-Platform") != "" {
			buf.WriteString(" mp")
		}
		if b, _ := readAllBody(r); len(b) > 0 {
			buf.WriteString(" ")
			buf.Write(b)
		}
		f.mu.Lock()
		f.seen = append(f.seen, buf.String())
		mp, mpErr := r.Header.Get("X-Client-Platform") != "", f.mpErr
		f.mu.Unlock()

		if strings.Contains(r.URL.Path, "/growth/tasks") {
			if mp && mpErr {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			tasks := make([]any, 0, len(f.codes))
			for _, c := range f.codes {
				tasks = append(tasks, map[string]any{"task_code": c, "current": 0, "target": 1})
			}
			body, _ := json.Marshal(map[string]any{"code": 0, "msg": "OK", "data": map[string]any{"tasks": tasks}})
			_, _ = w.Write(body)
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{}}`))
	}
}

func readAllBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	defer r.Body.Close()
	return io.ReadAll(r.Body)
}

func missingCodePanel(t *testing.T, f *missingCodeFake) (*Panel, *auth.Auth) {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL}
	a := &auth.Auth{UID: "cn-1", Nickname: "cn", AccessToken: "at"}
	pl := pool.New("")
	pl.Add(a)
	return New(Config{Pool: pl, Upstream: up}), a
}

// TestRunAutoAllSkipsCodesAccountDoesNotHave 账号任务列表里没有的 code 不再打上游
// （线上 26 个 CN 号每轮为 4 个已下线的任务白打 104 次调用），但结果行照旧是 skipped。
func TestRunAutoAllSkipsCodesAccountDoesNotHave(t *testing.T) {
	old := reportGap
	reportGap = 0
	t.Cleanup(func() { reportGap = old })

	f := &missingCodeFake{codes: []string{"chat_5"}}
	p, a := missingCodePanel(t, f)
	res := p.runAutoAllObserved(nil, a, nil)

	var sawSkipped bool
	for _, r := range res {
		if r["task_code"] == "template_5" {
			sawSkipped = r["status"] == "skipped" && r["message"] == "该账号没有此任务"
		}
	}
	if !sawSkipped {
		t.Errorf("template_5 应输出 skipped 行，实际结果 %+v", res)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, req := range f.seen {
		if strings.Contains(req, "template_5") {
			t.Fatalf("不该为账号没有的任务打上游: %s", req)
		}
	}
}

// TestRunAutoAllNoFilterWhenListFails 列表读失败时不能筛选：一次读失败不该把所有任务跳过。
func TestRunAutoAllNoFilterWhenListFails(t *testing.T) {
	old := reportGap
	reportGap = 0
	t.Cleanup(func() { reportGap = old })

	f := &missingCodeFake{codes: []string{"chat_5"}, mpErr: true}
	p, a := missingCodePanel(t, f)
	res := p.runAutoAllObserved(nil, a, nil)
	if len(res) == 0 {
		t.Fatal("列表失败时仍应逐项执行并输出结果")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var sawNonList bool
	for _, req := range f.seen {
		if !strings.Contains(req, "/growth/tasks") {
			sawNonList = true
		}
	}
	if !sawNonList {
		t.Error("列表失败时应照旧执行动作，不能只顾跳过")
	}
}
