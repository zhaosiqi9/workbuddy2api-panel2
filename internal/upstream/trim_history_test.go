package upstream

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestTrimHistoryKeepsLastRounds 一轮 = 一条 user 消息及其后的 assistant/tool 回复；
// 更早的整轮丢掉，system 全留（issue #99）。
func TestTrimHistoryKeepsLastRounds(t *testing.T) {
	body := map[string]any{
		"messages": []any{
			msg("system", "你是助手"),
			msg("user", "第一轮"),
			msg("assistant", "回一"),
			msg("user", "第二轮"),
			msg("assistant", "回二"),
			msg("user", "第三轮"),
		},
	}
	trimHistory(body, 2)
	roles := rolesOf(body)
	want := "system,user,assistant,user"
	if roles != want {
		t.Fatalf("保留最近 2 轮应为 %s，实际 %s", want, roles)
	}
	// 首条保留的必须是第二轮的用户消息，不能是残留的旧 assistant。
	if m := firstContent(body); m != "第二轮" {
		t.Fatalf("保留段应从「第二轮」开始，实际 %q", m)
	}
}

// TestTrimHistoryKeepsSystemBeforeCut cut 之前出现的 system 必须提上来保留：
// 客户端把 system 插在中间时，裁掉它就是裁掉协议。
func TestTrimHistoryKeepsSystemBeforeCut(t *testing.T) {
	body := map[string]any{
		"messages": []any{
			msg("system", "旧人格"),
			msg("user", "第一轮"),
			msg("system", "补充约束"),
			msg("assistant", "回一"),
			msg("user", "第二轮"),
		},
	}
	trimHistory(body, 1)
	if roles := rolesOf(body); roles != "system,system,user" {
		t.Fatalf("system 必须全留，实际 %s", roles)
	}
}

// TestTrimHistoryNoOp 关闭（<=0）与「轮数不足」都必须原样不动：不打乱 tool 配对，
// 也不因为开关关着就改 body 的字段顺序。
func TestTrimHistoryNoOp(t *testing.T) {
	body := map[string]any{
		"messages": []any{msg("system", "s"), msg("user", "u1"), msg("assistant", "a1"), msg("user", "u2")},
	}
	before, _ := json.Marshal(body)
	trimHistory(body, 0)
	trimHistory(body, -3)
	trimHistory(body, 5)
	after, _ := json.Marshal(body)
	if string(before) != string(after) {
		t.Fatalf("不该改动 body:\n before=%s\n after=%s", before, after)
	}
}

// TestTrimHistoryKeepsToolPairing 保留段开头不能是孤立 tool 结果：cut 只落在 user
// 下标上，所以带 tool_calls 的一整轮被完整保留或完整丢弃。
func TestTrimHistoryKeepsToolPairing(t *testing.T) {
	body := map[string]any{
		"messages": []any{
			msg("system", "s"),
			msg("user", "旧轮"),
			toolCallMsg("call_old"),
			toolResult("call_old"),
			msg("assistant", "回旧"),
			msg("user", "新轮"),
			toolCallMsg("call_new"),
			toolResult("call_new"),
		},
	}
	trimHistory(body, 1)
	roles := rolesOf(body)
	if roles != "system,user,assistant,tool" {
		t.Fatalf("应保留 system + 完整一轮（含 tool 配对），实际 %s", roles)
	}
	// 旧的 tool 调用与结果必须一起消失，新的必须都在。
	raw, _ := json.Marshal(body["messages"])
	s := string(raw)
	if strings.Contains(s, "call_old") {
		t.Errorf("旧轮的 tool 调用/结果没清干净: %s", s)
	}
	if !strings.Contains(s, "call_new") {
		t.Errorf("新轮的 tool 调用/结果被误删: %s", s)
	}
}

func msg(role, content string) map[string]any {
	return map[string]any{"role": role, "content": content}
}

func toolCallMsg(id string) map[string]any {
	return map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{
		map[string]any{"id": id, "type": "function",
			"function": map[string]any{"name": "f", "arguments": "{}"}},
	}}
}

func toolResult(id string) map[string]any {
	return map[string]any{"role": "tool", "tool_call_id": id, "content": "ok"}
}

func rolesOf(body map[string]any) string {
	out := ""
	for _, m := range body["messages"].([]any) {
		if out != "" {
			out += ","
		}
		out += m.(map[string]any)["role"].(string)
	}
	return out
}

func firstContent(body map[string]any) string {
	for _, m := range body["messages"].([]any) {
		mm := m.(map[string]any)
		if mm["role"] == "user" {
			s, _ := mm["content"].(string)
			return s
		}
	}
	return ""
}
