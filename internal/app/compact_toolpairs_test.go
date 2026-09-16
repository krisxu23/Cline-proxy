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
	// assistant 有正文 + 未回复的 tool_calls 且**不是最后一条**: 修剪调用但保留消息(正文有价值)。
	//
	// ★ 必须放一条尾随消息, 否则它自己就是"最后一条" —— 按 OmniRoute
	//   contextManager.ts:735-737 的 `!isLastMessage(idx)` 例外, 最后一条
	//   assistant 的 tool_calls **永不修剪**(那正是 agent 刚发起调用的瞬时状态)。
	//   早先本用例只放单条消息, 断言"应被移除"其实与参考实现相反。
	in := []any{
		map[string]any{"role": "assistant", "content": "let me check", "tool_calls": []any{
			map[string]any{"id": "tx", "type": "function", "function": map[string]any{"name": "f", "arguments": "{}"}},
		}},
		map[string]any{"role": "user", "content": "next"},
	}
	out := fixToolPairs(in)
	if len(out) != 2 {
		t.Fatalf("有正文的 assistant 不应整条丢弃, got %d", len(out))
	}
	m := mustJSON(t, out[0])
	// :746 原版是 `newMsg.tool_calls = filteredToolCalls` —— 置成**空数组**,
	// 不是 `delete`。键仍然存在。
	tcs, has := m["tool_calls"]
	if !has {
		t.Fatal("tool_calls 键应保留(置空数组), 而非删除")
	}
	if arr, ok := tcs.([]any); !ok || len(arr) != 0 {
		t.Fatalf("未回复的 tool_calls 应被置为空数组, got %v", tcs)
	}
	if m["content"] != "let me check" {
		t.Fatalf("正文应保留, got %v", m["content"])
	}
}

func TestFixToolPairs_最后一条assistant的调用永不修剪(t *testing.T) {
	// 照抄 contextManager.ts:735-737 的 `!isLastMessage(idx)` 例外。
	//
	// 这是用户报「任务无缘无故中断、没有错误也没有提示」的机制级防护:
	// agent 循环里"刚发起调用、结果还没回来"是**正常瞬时状态**,
	// 若把这条 assistant 的 tool_calls 删掉, 客户端就再也看不到那次调用。
	in := []any{
		map[string]any{"role": "user", "content": "hi"},
		map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{
			map[string]any{"id": "c1", "type": "function", "function": map[string]any{"name": "Read", "arguments": "{}"}},
		}},
	}
	out := fixToolPairs(in)
	if len(out) != 2 {
		t.Fatalf("最后一条 assistant 不应被丢弃, got %d 条", len(out))
	}
	m := mustJSON(t, out[1])
	tcs, ok := m["tool_calls"].([]any)
	if !ok || len(tcs) != 1 {
		t.Fatalf("最后一条 assistant 的 tool_calls 必须原样保留, got %v", m["tool_calls"])
	}
	if mustJSON(t, tcs[0])["id"] != "c1" {
		t.Fatalf("调用 id 应保留, got %v", tcs[0])
	}
}

func TestFixToolPairs_Anthropic形态tool_result被识别(t *testing.T) {
	// 照抄 contextManager.ts:721-730 —— Pass 1 必须同时收集 Anthropic 形态
	// `user.content[].tool_result.tool_use_id`, 否则:
	//   a) 配对完好的 tool_result 会被当成孤儿(删掉);
	//   b) 孤儿的 tool_result 反而会被原样发给上游 -> 400。
	//
	// 子例 1: 配对完好 -> 两者都保留
	paired := []any{
		map[string]any{"role": "user", "content": "go"},
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "tool_use", "id": "t1", "name": "Read", "input": map[string]any{}},
		}},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": "ok"},
		}},
		map[string]any{"role": "assistant", "content": "done"},
	}
	out := fixToolPairs(paired)
	if len(out) != 4 {
		t.Fatalf("配对完好的 Anthropic 对话应原样保留 4 条, got %d", len(out))
	}
	tail := mustJSON(t, out[3])
	if tail["content"] != "done" {
		t.Fatalf("末条 assistant 应保留, got %v", tail["content"])
	}

	// 子例 2: 孤儿 tool_result -> 整条 user 消息被丢弃(:799)
	orphan := []any{
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "ghost", "content": "x"},
		}},
		map[string]any{"role": "assistant", "content": "after"},
	}
	out = fixToolPairs(orphan)
	if len(out) != 1 {
		t.Fatalf("只剩孤儿 tool_result 的 user 消息应被丢弃, got %d 条: %v", len(out), out)
	}
	if mustJSON(t, out[0])["role"] != "assistant" {
		t.Fatalf("应保留 assistant, got %v", out[0])
	}
}

func TestFixToolPairs_无id的tool_call保留(t *testing.T) {
	// 照抄 :743 `!tc.id || toolResultIds.has(tc.id)` —— 没有 id 的 tool_call 一律保留。
	// 按空串查表会把它误删, 而上游对无 id 调用是容忍的。
	in := []any{
		map[string]any{"role": "user", "content": "go"},
		map[string]any{"role": "assistant", "tool_calls": []any{
			map[string]any{"function": map[string]any{"name": "X"}},
		}},
		map[string]any{"role": "assistant", "content": "tail"},
	}
	out := fixToolPairs(in)
	tcs, ok := mustJSON(t, out[1])["tool_calls"].([]any)
	if !ok || len(tcs) != 1 {
		t.Fatalf("无 id 的 tool_call 应保留, got %v", mustJSON(t, out[1])["tool_calls"])
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
