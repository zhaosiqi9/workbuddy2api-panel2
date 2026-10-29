package panel

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/pool"
	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
)

// TestPanelModelsBusyFailureDoesNotBlockCache 国际版池（global realm）下，
// /v2/chat/busy 恒 404（2026-10-29 真机实测）。
//
// 回归目标：繁忙度是纯观测数据，它的失败**不得**阻断模型目录缓存。此前 busy 失败
// 与目录失败共用同一份错误切片，于是「只要池里有 global 账号」→ 错误切片恒非空
// → 缓存永远写不进去 → 面板每次刷新都实打上游（1h TTL 形同虚设）。
//
// 断言：首轮 200 且给出 busy_errs（失败可见，不静默）；第二轮命中缓存（cached=true，
// 上游 v3/config 不再被请求）。
func TestPanelModelsBusyFailureDoesNotBlockCache(t *testing.T) {
	prev := auth.GlobalEnabled()
	auth.SetGlobalEnabled(true)
	defer auth.SetGlobalEnabled(prev)

	var cfgHits, busyHits int
	up := &upstream.Client{
		HTTP: &http.Client{Transport: fakeRoundTripper(func(r *http.Request) (*http.Response, error) {
			status := 200
			var body string
			switch {
			case strings.HasSuffix(r.URL.Path, "/v2/chat/busy"):
				busyHits++
				status = http.StatusNotFound
				body = `{"error_msg":"404 Route Not Found"}`
			case strings.HasSuffix(r.URL.Path, "/v3/config"):
				cfgHits++
				body = `{"code":0,"data":{"models":[` +
					`{"id":"fast-model","name":"Fast","maxInputTokens":200000,"maxOutputTokens":32000},` +
					`{"id":"gpt-6-astra","name":"GPT-6-Astra","maxInputTokens":1000000,"maxOutputTokens":128000,` +
					`"contextWindow":{"defaultLength":400000,"supportedLengths":[400000,1000000]}}]}}`
			default:
				// 企业端点家族：本用例不关心，失败可容忍（v3 成功即够）。
				status = http.StatusInternalServerError
			}
			return &http.Response{StatusCode: status,
				Header: http.Header{"Content-Type": []string{"application/json"}},
				Body:   io.NopCloser(strings.NewReader(body))}, nil
		})},
		ChatBaseGlobal:    "https://www.workbuddy.ai",
		BillingBaseGlobal: "https://www.workbuddy.ai",
		GlobalEnabled:     true,
	}
	p := pool.New("")
	p.Add(&auth.Auth{UID: "g1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.workbuddy.ai"})
	p.SetCredits("g1", 1000, 0)

	pn := New(Config{Version: "test", APIKey: "k", Pool: p, Upstream: up})

	call := func() (int, map[string]any) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/panel/api/models", nil)
		req.Header.Set("Authorization", "Bearer k")
		pn.ServeHTTP(rec, req)
		var resp map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)
		return rec.Code, resp
	}

	code, first := call()
	if code != 200 {
		t.Fatalf("首轮 code=%d（繁忙度失败不该让目录视图失败）", code)
	}
	models, _ := first["models"].([]any)
	if len(models) == 0 {
		t.Fatalf("首轮没有模型: %v", first)
	}
	if _, ok := first["busy_errs"]; !ok {
		t.Errorf("繁忙度失败应在 busy_errs 里可见（不静默）: %v", first)
	}
	if cfgHits == 0 {
		t.Fatal("首轮应真探过 /v3/config")
	}
	cfgAfterFirst := cfgHits

	// 第二轮：目录缓存必须已写入（此前的实现因为 busy 404 恒非空而永不写缓存）。
	code, second := call()
	if code != 200 {
		t.Fatalf("第二轮 code=%d", code)
	}
	if cached, _ := second["cached"].(bool); !cached {
		t.Fatalf("繁忙度失败后目录仍应进缓存，实际 cached=%v（cfgHits=%d busyHits=%d）",
			second["cached"], cfgHits, busyHits)
	}
	if cfgHits != cfgAfterFirst {
		t.Fatalf("命中缓存后不应再探 /v3/config，首轮 %d → 现在 %d", cfgAfterFirst, cfgHits)
	}
}
