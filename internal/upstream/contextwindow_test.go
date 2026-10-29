// contextwindow_test.go 5.9.3 新增的 contextWindow 段解析（上下文档位可选项）。
//
// 上游形状（2026-10-29 真机 /v3/config 实测）：
//
//	"contextWindow": {"defaultLength": 400000, "supportedLengths": [400000, 1000000]}
//
// CN 17 个模型 / global 3 个模型带该段。它与 maxInputTokens 不是同一口径：
// maxInputTokens 是当前生效上限，supportedLengths 是客户端**可切换**的档位。
package upstream

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
)

// TestContextWindowParsedFromConfig contextWindow 段必须落进 ModelInfo 的两个新字段。
func TestContextWindowParsedFromConfig(t *testing.T) {
	prev := auth.GlobalEnabled()
	auth.SetGlobalEnabled(true)
	defer auth.SetGlobalEnabled(prev)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/v3/config") {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{
			"models": []map[string]any{
				{
					"id": "gpt-6-astra", "maxInputTokens": 1000000, "maxOutputTokens": 128000,
					"contextWindow": map[string]any{
						"defaultLength":    400000,
						"supportedLengths": []int{400000, 1000000},
					},
				},
				// 没有 contextWindow 段：两个新字段必须留零值，不得编造档位。
				{"id": "hy3", "maxInputTokens": 192000, "maxOutputTokens": 64000},
			},
		}})
	}))
	defer srv.Close()

	c := New()
	c.HTTP = srv.Client()
	c.ChatBaseGlobal = srv.URL
	c.GlobalEnabled = true

	infos := c.FetchGlobalModelInfos(&auth.Auth{UID: "g1", AccessToken: "at", Domain: "www.workbuddy.ai"})
	byID := map[string]ModelInfo{}
	for _, mi := range infos {
		byID[mi.ID] = mi
	}
	astra, ok := byID["gpt-6-astra"]
	if !ok {
		t.Fatalf("目录里没有 gpt-6-astra（拿到 %v）", byID)
	}
	if astra.ContextWindowDefault != 400000 {
		t.Errorf("ContextWindowDefault=%d want 400000", astra.ContextWindowDefault)
	}
	if len(astra.ContextWindowSupported) != 2 ||
		astra.ContextWindowSupported[0] != 400000 || astra.ContextWindowSupported[1] != 1000000 {
		t.Errorf("ContextWindowSupported=%v want [400000 1000000]", astra.ContextWindowSupported)
	}
	hy3 := byID["hy3"]
	if hy3.ContextWindowDefault != 0 || len(hy3.ContextWindowSupported) != 0 {
		t.Errorf("无 contextWindow 段时不得编造档位: default=%d supported=%v",
			hy3.ContextWindowDefault, hy3.ContextWindowSupported)
	}
}

// TestContextWindowIgnoresZeroLengths 全 0 / 空数组等于「上游没声明档位」：
// 必须当作没有，而不是输出 {"default":0,"supported":[]}（下游会读成"只有 0 token"）。
func TestContextWindowIgnoresZeroLengths(t *testing.T) {
	e := dynModelEntry{ID: "m"}
	e.ContextWindow = &struct {
		DefaultLength    int64   `json:"defaultLength"`
		SupportedLengths []int64 `json:"supportedLengths"`
	}{DefaultLength: 0, SupportedLengths: []int64{0, 0}}
	mi := e.modelInfo()
	if mi.ContextWindowDefault != 0 || len(mi.ContextWindowSupported) != 0 {
		t.Fatalf("零值档位不得透出: %+v", mi)
	}
}

// TestGlobalEffortFallbackMatches59Catalog 国际版静态兜底表必须与 5.9.3 实测目录一致：
// 新上架模型有档位可报，已下架模型不再占位。
//
// 静态表只在「远端 effort 桶取不到」时生效；错了的代价是按不存在的档位降级
// （把请求打到上游 400 非法参数）。
func TestGlobalEffortFallbackMatches59Catalog(t *testing.T) {
	want := map[string][]string{
		"fast-model":             {"medium"},
		"balanced-model":         {"medium"},
		"primary-model":          {"high"},
		"glm-5.3-flash":          {"low", "high", "max"},
		"kimi-k3.1-pro":          {"low", "high", "max"},
		"kimi-k3.1-pro-highspeed": {"low", "high", "max"},
		"minimax-m3.1-flash":     {"low", "medium", "high", "xhigh", "max"},
		"gpt-6-sol":              {"low", "medium", "high", "xhigh"},
		"gpt-6.1-sol":            {"low", "medium", "high", "xhigh"},
		"gemini-3.8-flash":       {"low", "medium", "high"},
		"grok-4.7":               {"low", "medium", "high", "xhigh"},
		"hy4-preview":            {"high"},
		"deepseek-v4.1-flash-sg": {"low", "high", "max"},
	}
	for id, efforts := range want {
		cap := staticEffortCap("global", id)
		if strings.Join(cap.efforts, ",") != strings.Join(efforts, ",") {
			t.Errorf("%s efforts=%v want %v", id, cap.efforts, efforts)
		}
	}
	// 2026-10-29 目录里已经没有这些 id：留着静态档位等于给不存在的模型发档位。
	for _, gone := range []string{"gpt-5.5", "gpt-5.4", "gpt-5.3-codex", "gemini-3.5-flash", "kimi-k2.6", "glm-5.2", "hy4-preview-f"} {
		if cap := staticEffortCap("global", gone); len(cap.efforts) != 0 {
			t.Errorf("%s 已下架，静态兜底表不该再给它档位 %v", gone, cap.efforts)
		}
	}
}

// TestGlobalModelNamesMatches59Catalog 历史名单必须与实际目录同代，
// 否则拿它做差集参照会得出一半假差异。
func TestGlobalModelNamesMatches59Catalog(t *testing.T) {
	want := map[string]bool{
		"default-model": true, "fast-model": true, "balanced-model": true, "primary-model": true,
		"deepseek-v4.1-flash": true, "deepseek-v4.1-flash-sg": true,
		"glm-5.3": true, "glm-5.3-flash": true, "hy4-preview": true, "hy3": true,
		"kimi-k3": true, "kimi-k3.1-pro": true, "kimi-k3.1-pro-highspeed": true,
		"minimax-m3.1-flash": true,
		"gpt-5.6-sol": true, "gpt-5.6-terra": true, "gpt-5.6-luna": true,
		"gpt-6-astra": true, "gpt-6-sol": true, "gpt-6-luna": true, "gpt-6.1-sol": true,
		"gemini-3.8-flash": true, "grok-4.7": true,
		"hy-image-3.5": true, "kling-video-v3": true,
	}
	if len(GlobalModelNames) != len(want) {
		t.Fatalf("GlobalModelNames 有 %d 条 want %d（5.9.3 实测）", len(GlobalModelNames), len(want))
	}
	for _, id := range GlobalModelNames {
		if !want[id] {
			t.Errorf("GlobalModelNames 含 5.9.3 目录里没有的 %q", id)
		}
	}
}
