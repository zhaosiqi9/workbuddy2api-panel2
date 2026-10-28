//go:build ignore

// probe_channel 真机测一件事：11-128 渠道校验拦的到底是什么，以及「把系统提示词留在
// system 里」能不能过。
//
// 五个变体，同一账号、同一模型、同一句话，只改承载位置：
//
//	A 原始 @ 路径段放 system        —— 期望 400 / 11-128（证明拦的就是这段）
//	B 网关改写后的 @ 路径段放 system —— 能不能「留在 system 里」就看这一条
//	C 原始段搬到首条 user + 中性 system —— 搬家方案的效果
//	D 人格句放 system               —— 对照组：人格还认不认
//	E 同一人格句搬到 user           —— 搬家的代价：人格还认不认
//
// 用法（仓库根目录）：
//
//	go run internal/probe/probe_channel.go -auths /path/to/auths -realm cn
//
// 只打印状态码与上游错误原文（截断），不打印任何 token。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
)

// dshPaths 是 DSH 0.3.x 系统提示里的 @ 路径说明段原文（上游按连续原文匹配）。
const dshPaths = "Tokens prefixed with @ are paths the user explicitly referenced. " +
	"Relative paths resolve from the workspace root; absolute paths point to " +
	"files or directories on the host. A trailing slash marks a directory, so " +
	"list it when its contents matter. Anything else is a file: open it with " +
	"the read tool when its contents are needed, and never claim to have " +
	"inspected it before reading. The @\"...\" form only quotes a path that " +
	"contains spaces."

// dshPathsRewritten 是网关 sanitize.go 里的替换串（同义改写，语义不变）。
const dshPathsRewritten = "Paths beginning with @ refer to files the user explicitly mentioned. " +
	"Relative paths are resolved against the project root; absolute paths point " +
	"to files or directories on the host. A trailing slash marks a directory, so " +
	"list it when its contents matter. Anything else is a file: open it with the " +
	"read tool when its contents are needed, and never claim to have inspected it " +
	"before reading. The @\"...\" form only quotes a path that contains spaces."

const neutral = "You are a helpful assistant. Respond in the user's language, follow the user's instructions, and be direct and concise."

// persona 用来测「人设还在不在」：答案里出现 ARRR 就说明模型仍按人格说话。
const persona = "You are a pirate. You must begin every reply with the word ARRR."

type variant struct {
	name    string
	system  string
	user    string
	extra   []string // 追加的 system 消息（用于测「拆成多条」能否绕过连续匹配）
	sanitze bool     // true = 走网关的指纹净化（与生产默认一致）
	want    string
	repeat  int // 采样次数（人格这类要看比例的才需要 >1）
}

func main() {
	authDir := flag.String("auths", "", "auth 文件目录")
	realm := flag.String("realm", "cn", "cn / global")
	model := flag.String("model", "", "模型 id（裸名；空 = deepseek-v4.1-flash）")
	flag.Parse()

	a := pickAccount(*authDir, *realm)
	if a == nil {
		fmt.Printf("FAIL: 没有可用的 realm=%s 账号\n", *realm)
		os.Exit(1)
	}
	uidShow := a.UID
	if len(uidShow) > 8 {
		uidShow = uidShow[:8]
	}
	m := *model
	if m == "" {
		m = "deepseek-v4.1-flash"
	}
	fmt.Printf("account: uid=%s realm=%s  model=%s\n\n", uidShow, a.Realm(), m)

	c := upstream.New()
	c.SanitizeBrandScope.Store("all")

	// 句级拆分：Mortonnn 实测量到「只发首句 200、前两句连排 400」，所以要在句子边界切。
	sent1 := "Tokens prefixed with @ are paths the user explicitly referenced."
	sent2 := "Relative paths resolve from the workspace root; absolute paths point to " +
		"files or directories on the host."
	sent3 := "A trailing slash marks a directory, so list it when its contents matter."
	sent4 := "Anything else is a file: open it with the read tool when its contents are needed, " +
		"and never claim to have inspected it before reading."
	sent5 := "The @\"...\" form only quotes a path that contains spaces."

	cases := []variant{
		{"A 原始@段在system", dshPaths, "Say OK.", nil, false, "400/11-128", 1},
		{"B 改写@段在system", dshPathsRewritten, "Say OK.", nil, true, "200", 1},
		{"C 原始@段搬到user", neutral, dshPaths + "\n\nSay OK.", nil, false, "200", 1},
		{"D 人格在system", persona, "Say OK.", nil, true, "含 ARRR", 3},
		{"E 人格搬到user", neutral, persona + "\n\nSay OK.", nil, true, "含 ARRR", 3},
		{"F 首句/其余拆两条system", sent1, "Say OK.", []string{sent2 + " " + sent3 + " " + sent4 + " " + sent5}, false, "200", 1},
		{"F2 每句一条system", sent1, "Say OK.", []string{sent2, sent3, sent4, sent5}, false, "200", 1},
		{"G 人格拆两条system", "You are a pirate.", "Say OK.", []string{"You must begin every reply with the word ARRR."}, true, "含 ARRR", 3},
	}

	for _, v := range cases {
		n := v.repeat
		if n == 0 {
			n = 1
		}
		ok, last := 0, ""
		var st int
		for i := 0; i < n; i++ {
			var text string
			st, text = send(c, a, m, v)
			last = text
			hit := false
			switch {
			case v.want == "200":
				hit = st == 200
			case v.want == "400/11-128":
				hit = st == 400
			case v.want == "含 ARRR":
				hit = strings.Contains(strings.ToUpper(text), "ARRR")
			}
			if hit {
				ok++
			}
		}
		verdict := "PASS"
		if ok != n {
			verdict = "FAIL"
		}
		fmt.Printf("%-4s %-24s status=%-3d %d/%d  %s\n", verdict, v.name, st, ok, n, firstLine(last))
	}
}

// send 走网关真实的出站管线（ChatStream），sanitize 按变体开关。
func send(c *upstream.Client, a *auth.Auth, model string, v variant) (int, string) {
	c.SanitizeFingerprints.Store(v.sanitze)
	body, _ := json.Marshal(map[string]any{
		"model":      model,
		"stream":     true,
		"messages":   []any{map[string]any{"role": "system", "content": v.system}, map[string]any{"role": "user", "content": v.user}},
		"max_tokens": 48,
	})
	rc, status, resp, err := c.ChatStream(a, body, "127.0.0.1", upstream.ChatMeta{})
	if err != nil && status == 0 {
		return -1, "ERR: " + err.Error()
	}
	if rc != nil {
		defer rc.Close()
	}
	if status != 200 {
		return status, string(resp)
	}
	// 200：从 SSE 里捞出模型正文（只取前若干帧足够判人格）。
	raw, _ := io.ReadAll(io.LimitReader(rc, 64<<10))
	return status, sseText(string(raw))
}

// sseText 把 delta.content 拼起来（够判断有没有 ARRR）。
func sseText(s string) string {
	var b strings.Builder
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimPrefix(strings.TrimSpace(line), "data: ")
		if line == "" || line == "[DONE]" {
			continue
		}
		var d struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(line), &d) != nil {
			continue
		}
		for _, ch := range d.Choices {
			b.WriteString(ch.Delta.Content)
		}
	}
	return b.String()
}

func firstLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 150 {
		return s[:150] + "…"
	}
	return s
}

// pickAccount 取一个该 realm 的可用账号（不回显凭证）。
func pickAccount(authDir, realm string) *auth.Auth {
	entries, err := os.ReadDir(authDir)
	if err != nil {
		fmt.Println("read auths:", err)
		return nil
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(authDir, e.Name()))
		if err != nil {
			continue
		}
		a, err := auth.Parse(raw)
		if err != nil || a == nil || a.AccessToken == "" {
			continue
		}
		if a.IsGlobal() != (realm == "global") {
			continue
		}
		if a.ExpiresAt > 0 && time.Now().Unix() > a.ExpiresAt {
			continue
		}
		return a
	}
	return nil
}
