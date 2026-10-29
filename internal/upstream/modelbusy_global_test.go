package upstream

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
)

// globalBusyStub 模拟两域繁忙度端点：host 匹配 www.workbuddy.ai 时一律回 404
// {"error_msg":"404 Route Not Found"}（2026-10-29 真机形态），其余 host 正常回一条 normal。
// hits 记录被打过的总次数，globalHits 记录其中打到国际域的次数。
type globalBusyStub struct {
	hits       int
	globalHits int
}

func (g *globalBusyStub) client() *Client {
	return &Client{
		HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			g.hits++
			if r.URL.Host == "www.workbuddy.ai" {
				g.globalHits++
				return &http.Response{StatusCode: http.StatusNotFound,
					Header: http.Header{"Content-Type": []string{"application/json"}},
					Body: io.NopCloser(strings.NewReader(
						`{"error_msg":"404 Route Not Found"}`))}, nil
			}
			return &http.Response{StatusCode: http.StatusOK,
				Header: http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(strings.NewReader(
					`{"code":0,"msg":"OK","data":[{"model":"hy3","busy_level":"idle"}]}`))}, nil
		})},
		ChatBaseCN:     "https://cn.example",
		BillingBaseCN:  "https://cn.example",
		ChatBaseGlobal: "https://www.workbuddy.ai",
		GlobalEnabled:  true,
	}
}

// busyGlobalAuth 判为 global realm 的账号（Domain 后缀决定，见 auth.Realm）。
func busyGlobalAuth() *auth.Auth {
	return &auth.Auth{UID: "g1", AccessToken: "at", Domain: "www.workbuddy.ai"}
}

// busyCNAuth 判为 cn realm 的账号。
func busyCNAuth() *auth.Auth {
	return &auth.Auth{UID: "u1", AccessToken: "at"}
}

// TestModelBusyGlobal404IsNotAnError 国际版无 /v2/chat/busy（实测 404）：
// 必须当成「该域没有这个观测能力」返回空表 + nil error，而不是把 404 抛给上层。
//
// 反例代价（本测试存在的原因）：面板把 busy 失败并进目录错误列表，于是只要池里有
// global 账号，`len(fetchErrs)==0` 的写缓存条件永远不成立，模型目录缓存整份失效。
func TestModelBusyGlobal404IsNotAnError(t *testing.T) {
	g := &globalBusyStub{}
	got, err := g.client().ModelBusy(busyGlobalAuth(), []string{"fast-model", "gpt-6-astra"})
	if err != nil {
		t.Fatalf("global 404 必须吞掉（返回 nil error），实际 err=%v", err)
	}
	if len(got) != 0 {
		t.Fatalf("404 时不得编造繁忙度，实际 %v", got)
	}
	if g.globalHits != 1 {
		t.Fatalf("首次调用应真打国际域 1 次，实际 %d", g.globalHits)
	}
}

// TestModelBusyGlobal404Memoized 探到 404 后本进程不再重试：繁忙度是纯观测数据，
// 每轮刷新重打一次不存在的端点只刷日志、白等 RTT。
func TestModelBusyGlobal404Memoized(t *testing.T) {
	g := &globalBusyStub{}
	c := g.client()
	for i := 0; i < 3; i++ {
		if _, err := c.ModelBusy(busyGlobalAuth(), []string{"fast-model"}); err != nil {
			t.Fatalf("第 %d 次 err=%v", i+1, err)
		}
	}
	if g.globalHits != 1 {
		t.Fatalf("404 记住后应只打过国际域 1 次，实际 %d", g.globalHits)
	}
}

// TestModelBusyGlobal404NotAppliedToCN 记忆只作用于 global：CN 端点实测 200，
// 不能因为国际域探到 404 就把国内版的繁忙度也一并关掉。
func TestModelBusyGlobal404NotAppliedToCN(t *testing.T) {
	g := &globalBusyStub{}
	c := g.client()
	if _, err := c.ModelBusy(busyGlobalAuth(), []string{"fast-model"}); err != nil {
		t.Fatal(err)
	}
	got, err := c.ModelBusy(busyCNAuth(), []string{"hy3"})
	if err != nil {
		t.Fatal(err)
	}
	if got["hy3"] != "idle" {
		t.Fatalf("CN 繁忙度必须照常，实际 %v", got)
	}
}

// TestModelBusyGlobalNon404StillErrors 非 404（如 401/500）不属于「端点不存在」，
// 必须原样返回错误——否则运维看不到凭证失效这类真故障。
func TestModelBusyGlobalNon404StillErrors(t *testing.T) {
	c := &Client{
		HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusUnauthorized,
				Header: http.Header{"Content-Type": []string{"application/json"}},
				Body:   io.NopCloser(strings.NewReader(`{"code":12153,"msg":"invalid token"}`))}, nil
		})},
		ChatBaseGlobal: "https://www.workbuddy.ai",
		GlobalEnabled:  true,
	}
	if _, err := c.ModelBusy(busyGlobalAuth(), []string{"fast-model"}); err == nil {
		t.Fatal("401 必须报错")
	}
	if c.modelBusyUnsupported.Load() {
		t.Fatal("非 404 不得置位「端点不存在」记忆")
	}
}
