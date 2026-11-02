package upstream

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/proxypool"
)

// Prevent new account endpoints from silently bypassing account-aware routing.
func TestAccountHTTPExitsUseProxyRouting(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") || name == "proxy.go" {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Do" {
				return true
			}
			if base, ok := sel.X.(*ast.SelectorExpr); ok {
				if root, ok := base.X.(*ast.Ident); ok && root.Name == "c" {
					t.Errorf("%s bypasses account routing via c.%s.Do", name, base.Sel.Name)
				}
			}
			if base, ok := sel.X.(*ast.CallExpr); ok {
				if method, ok := base.Fun.(*ast.SelectorExpr); ok {
					if root, ok := method.X.(*ast.Ident); ok && root.Name == "c" {
						t.Errorf("%s bypasses account routing via c.%s().Do", name, method.Sel.Name)
					}
				}
			}
			return true
		})
	}
}

func TestAccountRequestsAndLongStreamUseBoundProxy(t *testing.T) {
	var mu sync.Mutex
	paths := map[string]int{}
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths[r.URL.Path]++
		mu.Unlock()
		switch r.URL.Path {
		case "/v2/chat/completions":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n")
			w.(http.Flusher).Flush()
			time.Sleep(100 * time.Millisecond)
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		case "/v2/plugin/auth/token/refresh":
			_, _ = io.WriteString(w, `{"code":0,"data":{"accessToken":"new","refreshToken":"new-rt","expiresIn":3600}}`)
		case "/v2/embeddings":
			_, _ = io.WriteString(w, `{"code":0,"data":[{"embedding":[1,2]}]}`)
		default:
			_, _ = io.WriteString(w, `{"code":0,"data":{}}`)
		}
	}))
	defer proxy.Close()
	c := New()
	manager, err := proxypool.New("", c.HTTP.Transport.(*http.Transport), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	s := proxypool.DefaultSettings()
	s.URL = "http://health.invalid/"
	if err := manager.Configure(s); err != nil {
		t.Fatal(err)
	}
	results, err := manager.Import(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	id := results[0].ID
	if err := manager.Probe(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if err := manager.Bind("u1", proxypool.Binding{Mode: "fixed", ProxyID: id}); err != nil {
		t.Fatal(err)
	}
	c.ProxyPool = manager
	c.ChatBaseCN, c.BillingBaseCN, c.WebBaseCN = "http://upstream.invalid", "http://upstream.invalid", "http://upstream.invalid"
	a := &auth.Auth{UID: "u1", AccessToken: "token", RefreshToken: "refresh"}
	if err := c.RefreshToken(a); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.UserResource(a); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Embeddings(context.Background(), a, "bge-m3", []string{"hi"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ImageGenerations(a, map[string]any{"model": "image", "prompt": "hi"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Search(context.Background(), a, "hi", 1); err != nil {
		t.Fatal(err)
	}
	if err := c.ReportChatActivity(a, "conversation", "request"); err != nil {
		t.Fatal(err)
	}
	// The short RPC timeout must not become the chat stream's total timeout.
	c.HTTP.Timeout = 20 * time.Millisecond
	rc, status, _, err := c.ChatStreamContext(context.Background(), a, []byte(`{"model":"test","messages":[{"role":"user","content":"hi"}]}`), "", ChatMeta{}, nil)
	if err != nil || status != 200 {
		t.Fatalf("chat status=%d err=%v", status, err)
	}
	body, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil || !strings.Contains(string(body), "[DONE]") {
		t.Fatalf("stream=%q err=%v", body, err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, path := range []string{"/v2/plugin/auth/token/refresh", "/v2/embeddings", "/v2/chat/completions", "/v2/images/generations", "/agenttool/v1/search", "/v2/report"} {
		if paths[path] == 0 {
			t.Errorf("proxy did not receive %s", path)
		}
	}
}
