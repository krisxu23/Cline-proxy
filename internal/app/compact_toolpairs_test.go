package app

// fixToolPairs 的测试: 压缩/截断后断裂的 tool 配对必须被修复,
// 否则上游直接 400("tool message must follow tool_calls")。

import (
	"encoding/json"
	"testing"
)

func mustJSON(t *testing.T, v any) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("期望 map, got %T", v)
	}
	return m
}

func asst(id string, calls ...string) map[string]any {
	tcs := make([]any, 0, len(calls))
	for _, c := range calls {
		tcs = append(tcs, map[string]any{"id": c, "type": "function", "function": map[string]any{"name": "f", "arguments": "{}"}})
	}
	m := map[string]any{"role": "assistant", "content": ""}
	if len(tcs) > 0 {
		m["tool_calls"] = tcs
	}
	return m
}

func toolRes(id, body string) map[string]any {
	return map[string]any{"role": "tool", "tool_call_id": id, "content": body}
}

func TestFixToolPairsDropsOrphanToolResult(t *testing.T) {
	// assistant 被切掉了, 只剩 tool 结果 → 孤儿, 必须丢弃
	in := []any{toolRes("t1", `{"ok":true}`)}
	out := fixToolPairs(in)
	if len(out) != 0 {
		t.Fatalf("孤儿 tool 结果应被丢弃, got %v", out)
	}
}

func TestFixToolPairsKeepsValidPair(t *testing.T) {
	in := []any{
		asst("a", "t1"),
		toolRes("t1", "42"),
	}
	out := fixToolPairs(in)
	if len(out) != 2 {
		t.Fatalf("完整配对应原样保留, got %d 条", len(out))
	}
}

func TestFixToolPairsTrimsUnansweredCalls(t *testing.T) {
	// assistant 发起 t1/t2 两个调用, 但 t2 的回复被切掉 → 修剪 t2, 保留 t1
	in := []any{
		asst("a", "t1", "t2"),
		toolRes("t1", "42"),
	}
	out := fixToolPairs(in)
	if len(out) != 2 {
		t.Fatalf("应保留 assistant + tool 两条, got %d", len(out))
	}
	m := mustJSON(t, out[0])
	tcs, _ := m["tool_calls"].([]any)
	if len(tcs) != 1 {
		t.Fatalf("未回复的 tool_call 应被修剪, got %d 个", len(tcs))
	}
	if first := mustJSON(t, tcs[0])["id"]; first != "t1" {
		t.Fatalf("应保留已回复的 t1, got %v", first)
	}
}

func TestFixToolPairsDropsPureCallShell(t *testing.T) {
	// assistant 只有 tool_calls 没有正文, 调用又全部没回复 → 整条无意义, 丢弃
	in := []any{
		map[string]any{"role": "user", "content": "hi"},
		asst("a", "t9"),
		map[string]any{"role": "assistant", "content": "real answer"},
	}
	out := fixToolPairs(in)
	if len(out) != 2 {
		t.Fatalf("纯调用壳应被丢弃, got %d 条: %v", len(out), out)
	}
	if r := mustJSON(t, out[1])["role"]; r != "assistant" {
		t.Fatalf("应保留有正文的 assistant, got %v", r)
	}
}

func TestFixToolPairsKeepsAssistantWithContent(t *testing.T) {
	// assistant 有正文 + 未回复的 tool_calls: 修剪调用但保留消息(正文有价值)
	in := []any{
		map[string]any{"role": "assistant", "content": "let me check", "tool_calls": []any{
			map[string]any{"id": "tx", "type": "function", "function": map[string]any{"name": "f", "arguments": "{}"}},
		}},
	}
	out := fixToolPairs(in)
	if len(out) != 1 {
		t.Fatalf("有正文的 assistant 不应整条丢弃, got %d", len(out))
	}
	m := mustJSON(t, out[0])
	if _, has := m["tool_calls"]; has {
		t.Fatal("未回复的 tool_calls 应被移除")
	}
	if m["content"] != "let me check" {
		t.Fatalf("正文应保留, got %v", m["content"])
	}
}

func TestFixToolPairsEndToEndViaCompact(t *testing.T) {
	// 端到端: 一个"会砍断配对"的切片场景, 修复后不应残留孤儿
	raw := `[
		{"role":"user","content":"q"},
		{"role":"assistant","content":"","tool_calls":[{"id":"t1","type":"function","function":{"name":"f","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"t1","content":"r1"},
		{"role":"assistant","content":"mid","tool_calls":[{"id":"t2","type":"function","function":{"name":"g","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"t2","content":"r2"},
		{"role":"user","content":"next"}
	]`
	var msgs []any
	if err := json.Unmarshal([]byte(raw), &msgs); err != nil {
		t.Fatal(err)
	}
	// 模拟"从第 3 条开始保留"(t1 的结果被切掉)
	kept := msgs[2:]
	out := fixToolPairs(kept)
	for _, mi := range out {
		m := mustJSON(t, mi)
		if m["role"] == "tool" {
			id, _ := m["tool_call_id"].(string)
			if id == "t1" {
				t.Fatal("孤儿 t1 结果应被丢弃")
			}
		}
		if m["role"] == "assistant" {
			if tcs, ok := m["tool_calls"].([]any); ok {
				for _, tc := range tcs {
					if mustJSON(t, tc)["id"] == "t1" {
						t.Fatal("未回复的 t1 调用应被修剪")
					}
				}
			}
		}
	}
}
