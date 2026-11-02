package server

import (
	"encoding/json"
	"strings"
	"testing"
)

// archiveContext 是「请求事件日志」展开上下文的数据源。四条钉：只取上下文字段、
// 超限丢最旧而不是截成半截 JSON、单条超大压成截断串、脏 body 不产出。
func TestArchiveContextExtractsOnlyConversation(t *testing.T) {
	body := []byte(`{"model":"deepseek-v4.1-flash","temperature":0.7,
		"tools":[{"type":"function","function":{"name":"x","parameters":{"a":1}}}],
		"system":"你是助手","messages":[{"role":"user","content":"你好"}]}`)
	raw := archiveContext(body)
	if raw == nil {
		t.Fatal("合法请求体应产出上下文")
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("输出必须是合法 JSON: %v (%s)", err, raw)
	}
	if got["system"] != "你是助手" {
		t.Errorf("system 丢失: %v", got["system"])
	}
	if msgs, _ := got["messages"].([]any); len(msgs) != 1 {
		t.Errorf("messages 应保留 1 条，得到 %v", got["messages"])
	}
	// tools 与采样参数不属于上下文：带上它们只会挤掉真正要看的内容。
	if _, ok := got["tools"]; ok {
		t.Error("不该带 tools（体积大且与上下文无关）")
	}
	if _, ok := got["temperature"]; ok {
		t.Error("不该带采样参数")
	}
}

// 超预算时丢最旧的轮次，最近的对话必须留着，且输出仍是合法 JSON。
func TestArchiveContextDropsOldestWhenTooLarge(t *testing.T) {
	big := strings.Repeat("x", 4096)
	var msgs []string
	for i := 0; i < 20; i++ {
		msgs = append(msgs, `{"role":"user","content":"`+big+`"}`)
	}
	body := []byte(`{"messages":[` + strings.Join(msgs, ",") + `]}`)
	raw := archiveContext(body)
	if len(raw) > contextMaxBytes {
		t.Fatalf("超出上限 %d: %d", contextMaxBytes, len(raw))
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("收敛后仍须是合法 JSON: %v", err)
	}
	if n := len(got["messages"].([]any)); n == 0 || n >= 20 {
		t.Fatalf("应保留最近若干条（1..19），得到 %d", n)
	}
}

// 单条就超预算（贴了一大段代码）：content 压成截断串并标出来，不能返回半截 JSON。
func TestArchiveContextTruncatesSingleHugeMessage(t *testing.T) {
	huge := strings.Repeat("y", contextMaxBytes*2)
	raw := archiveContext([]byte(`{"messages":[{"role":"user","content":"` + huge + `"}]}`))
	var got struct {
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("截断后必须是合法 JSON: %v", err)
	}
	if len(got.Messages) != 1 || !strings.HasSuffix(got.Messages[0].Content, "（超长已截断）") {
		t.Fatalf("应保留一条并标记截断，得到 %+v", got.Messages)
	}
}

// 出图/embedding 的 prompt 也属于上下文；脏 body 一律不产出（宁缺勿烂）。
func TestArchiveContextPromptAndJunk(t *testing.T) {
	var got map[string]any
	if err := json.Unmarshal(archiveContext([]byte(`{"prompt":"一只猫"}`)), &got); err != nil {
		t.Fatalf("prompt 形态应可解析: %v", err)
	}
	if got["prompt"] != "一只猫" {
		t.Errorf("prompt 丢失: %v", got)
	}
	// 只有超长 system、没有 messages：同样要收到预算内（否则单事件能撑到几 MB）。
	if raw := archiveContext([]byte(`{"system":"` + strings.Repeat("z", contextMaxBytes*2) + `"}`)); len(raw) > contextMaxBytes {
		t.Errorf("超长 system 未收敛: %d 字节", len(raw))
	}
	for _, junk := range []string{"", "not json", "{}"} {
		if raw := archiveContext([]byte(junk)); raw != nil {
			t.Errorf("%q 不该产出上下文，得到 %s", junk, raw)
		}
	}
}
