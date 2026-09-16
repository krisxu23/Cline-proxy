package app

import (
	"encoding/json"
	"reflect"
	"testing"
)

// 用例逐条对应 OmniRoute tests/unit/combo-flatten-anthropic-tool-messages.test.ts (161 行)。
// 该测试文件同时是 flattenToolHistory 的权威行为规格。

// ─────────── 用例 1 (:26-50) OpenAI tool role 与 assistant tool_calls ───────────

func TestFlatten_OpenAI_tool_role与tool_calls(t *testing.T) {
	msgs := []any{
		map[string]any{"role": "user", "content": "find files"},
		map[string]any{
			"role": "assistant", "content": "",
			"tool_calls": []any{
				map[string]any{"id": "c1", "type": "function", "function": map[string]any{"name": "find"}},
			},
		},
		map[string]any{"role": "tool", "tool_call_id": "c1", "content": "['a.js']"},
		map[string]any{"role": "user", "content": "describe it"},
	}
	out := flattenToolHistory(msgs)

	if len(out) != 4 {
		t.Fatalf("长度应为 4, got %d", len(out))
	}
	// out[0] 原样
	if out[0].(map[string]any)["role"] != "user" {
		t.Fatalf("out[0] 应保持 user")
	}
	// assistant: tool_calls 被摘掉, content 含工具名与前缀 (:34-38)
	a1 := out[1].(map[string]any)
	if _, has := a1["tool_calls"]; has {
		t.Fatal("out[1].tool_calls 应被摘除")
	}
	c1, isStr := a1["content"].(string)
	if !isStr {
		t.Fatalf("out[1].content 应为字符串, got %T", a1["content"])
	}
	if !containsAll(c1, "find", flattenToolCallPrefix) {
		t.Fatalf("out[1].content 应含工具名与前缀, got %q", c1)
	}
	// tool role -> assistant prose (:40-44)
	tc := out[2].(map[string]any)
	if tc["role"] != "assistant" {
		t.Fatalf("out[2].role 应为 assistant, got %#v", tc["role"])
	}
	c2 := tc["content"].(string)
	if !containsAll(c2, "['a.js']", flattenToolResultPrefix) {
		t.Fatalf("out[2].content 应含结果与前缀, got %q", c2)
	}
	// out[3] 深度相等
	want3 := map[string]any{"role": "user", "content": "describe it"}
	if !reflect.DeepEqual(out[3], want3) {
		t.Fatalf("out[3] 应为 %#v, got %#v", want3, out[3])
	}
}

// ─────────── 用例 2 (:52-75) Anthropic tool_use / tool_result ───────────

func TestFlatten_Anthropic工具块(t *testing.T) {
	msgs := []any{
		map[string]any{"role": "user", "content": "do it"},
		map[string]any{
			"role": "assistant",
			"content": []any{
				map[string]any{"type": "text", "text": "ok"},
				map[string]any{"type": "tool_use", "id": "t1", "name": "run"},
			},
		},
		map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": "done"},
			},
		},
	}
	out := flattenToolHistory(msgs)

	if len(out) != 3 {
		t.Fatalf("长度应为 3, got %d", len(out))
	}
	// :68 `assert.equal(out[1].content, \`ok\n${TOOL_CALL_PREFIX}run]\`);`
	want1 := "ok\n" + flattenToolCallPrefix + "run]"
	if got := out[1].(map[string]any)["content"]; got != want1 {
		t.Fatalf("out[1].content 错误\n got: %q\nwant: %q", got, want1)
	}
	// :70 tool_result 摊平后 role 保持 user
	want2 := flattenToolResultPrefix + "done]"
	if got := out[2].(map[string]any)["content"]; got != want2 {
		t.Fatalf("out[2].content 错误\n got: %q\nwant: %q", got, want2)
	}
}

// ─────────── 用例 3 (:77-84) 无工具轮次原样保留 ───────────

func TestFlatten_无工具轮次原样保留(t *testing.T) {
	msgs := []any{
		map[string]any{"role": "system", "content": "you are helpful"},
		map[string]any{"role": "user", "content": "hello"},
		map[string]any{"role": "assistant", "content": "hi"},
	}
	out := flattenToolHistory(msgs)
	if !reflect.DeepEqual(out, msgs) {
		t.Fatalf("应完全原样\n got: %#v\nwant: %#v", out, msgs)
	}
}

// ─────────── 用例 4 (:86-95) 过滤 null/undefined ───────────

func TestFlatten_过滤nil条目(t *testing.T) {
	msgs := []any{
		map[string]any{"role": "user", "content": "a"},
		nil,
		map[string]any{"role": "assistant", "content": "b"},
	}
	out := flattenToolHistory(msgs)
	// :94 `assert.equal(out.length, 2);`
	if len(out) != 2 {
		t.Fatalf("应过滤掉 nil, 长度 2, got %d", len(out))
	}
}

// ─────────── 用例 5 (:97-106) legacy function role ───────────

func TestFlatten_function_role(t *testing.T) {
	msgs := []any{
		map[string]any{"role": "user", "content": "q"},
		map[string]any{"role": "function", "name": "f", "content": "result"},
	}
	out := flattenToolHistory(msgs)
	if out[1].(map[string]any)["role"] != "assistant" {
		t.Fatalf("function role 应转为 assistant, got %#v", out[1].(map[string]any)["role"])
	}
	if !containsAll(out[1].(map[string]any)["content"].(string), "result") {
		t.Fatalf("应保留 result, got %#v", out[1].(map[string]any)["content"])
	}
}

// ─────────── 用例 6 (:108-119) 文本 + tool_calls 同时存在 ───────────

func TestFlatten_文本与tool_calls并存时保留文本(t *testing.T) {
	msgs := []any{
		map[string]any{
			"role": "assistant", "content": "thinking out loud",
			"tool_calls": []any{
				map[string]any{"function": map[string]any{"name": "search"}},
				map[string]any{"function": map[string]any{"name": "fetch"}},
			},
		},
	}
	out := flattenToolHistory(msgs)
	m := out[0].(map[string]any)
	if _, has := m["tool_calls"]; has {
		t.Fatal("tool_calls 应被摘除")
	}
	// :117 断言精确字符串
	want := "thinking out loud\n" + flattenToolCallPrefix + "search, fetch]"
	if m["content"] != want {
		t.Fatalf("content 错误\n got: %q\nwant: %q", m["content"], want)
	}
}

// ─────────── 用例 7 (:121-133) 纯 tool_use 无 text 块 ───────────

func TestFlatten_纯tool_use无文本块(t *testing.T) {
	msgs := []any{
		map[string]any{
			"role": "assistant",
			"content": []any{
				map[string]any{"type": "tool_use", "id": "t1", "name": "alpha"},
				map[string]any{"type": "tool_use", "id": "t2", "name": "beta"},
			},
		},
	}
	out := flattenToolHistory(msgs)
	want := flattenToolCallPrefix + "alpha, beta]"
	if got := out[0].(map[string]any)["content"]; got != want {
		t.Fatalf("content 错误\n got: %q\nwant: %q", got, want)
	}
}

// ─────────── 用例 8 (:135-149) tool_result 的 content 是文本块数组 ───────────

func TestFlatten_tool_result内部是文本块数组(t *testing.T) {
	msgs := []any{
		map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{
					"type": "tool_result", "tool_use_id": "t1",
					"content": []any{map[string]any{"type": "text", "text": "file: a.js"}},
				},
			},
		},
	}
	out := flattenToolHistory(msgs)
	want := flattenToolResultPrefix + "file: a.js]"
	if got := out[0].(map[string]any)["content"]; got != want {
		t.Fatalf("content 错误\n got: %q\nwant: %q", got, want)
	}
}

// ─────────── 用例 9 (:151-160) 纯函数: 不得改动输入 ───────────

func TestFlatten_纯函数不改动输入(t *testing.T) {
	msgs := []any{
		map[string]any{"role": "tool", "tool_call_id": "c1", "content": "x"},
		map[string]any{
			"role": "assistant", "content": "",
			"tool_calls": []any{map[string]any{"function": map[string]any{"name": "n"}}},
		},
	}
	snapshot, _ := json.Marshal(msgs)
	flattenToolHistory(msgs)
	after, _ := json.Marshal(msgs)
	if string(snapshot) != string(after) {
		t.Fatalf("输入被改动了\nbefore: %s\nafter:  %s", snapshot, after)
	}
}

// ─────────── 前缀常量字面量锁 ───────────

func TestFlatten_前缀常量字面量(t *testing.T) {
	// 照抄 flattenToolHistory.ts:18-19 的原始字面量。
	// 冒号位置易错: 是 "[Tool result: " 而非 "[Tool result]: "。
	if flattenToolCallPrefix != "[Called tools: " {
		t.Fatalf("TOOL_CALL_PREFIX 字面量错误: %q", flattenToolCallPrefix)
	}
	if flattenToolResultPrefix != "[Tool result: " {
		t.Fatalf("TOOL_RESULT_PREFIX 字面量错误: %q", flattenToolResultPrefix)
	}
}

// ─────────── extractTextContent 语义 (geminiHelper.ts:284-294) ───────────

func TestFlattenExtractText_数组用空串连接(t *testing.T) {
	// ⚠ 与 compact.go 的 msgText 不同: 那里用 "\n", 这里必须是 ""
	got := flattenExtractTextContent([]any{
		map[string]any{"type": "text", "text": "a"},
		map[string]any{"type": "text", "text": "b"},
	})
	if got != "ab" {
		t.Fatalf("数组应用空串连接, got %q", got)
	}
}

func TestFlattenExtractText_只收text类型块(t *testing.T) {
	got := flattenExtractTextContent([]any{
		map[string]any{"type": "text", "text": "keep"},
		map[string]any{"type": "tool_use", "text": "drop"},
		map[string]any{"type": "image", "text": "drop"},
	})
	if got != "keep" {
		t.Fatalf("非 text 块应被忽略, got %q", got)
	}
}

func TestFlattenExtractText_非字符串非数组返回空(t *testing.T) {
	for _, in := range []any{nil, 42, true, map[string]any{"a": 1}} {
		if got := flattenExtractTextContent(in); got != "" {
			t.Fatalf("%#v 应返回空串, got %q", in, got)
		}
	}
}

// ─────────── 工具名兜底 (:69) ───────────

func TestToolCallName_三级兜底(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{map[string]any{"function": map[string]any{"name": "fn"}}, "fn"},
		{map[string]any{"name": "direct"}, "direct"},
		{map[string]any{"function": map[string]any{"name": ""}, "name": "fallback"}, "fallback"},
		{map[string]any{}, "tool"},
		{nil, "tool"},
		{"not-a-map", "tool"},
	}
	for _, c := range cases {
		if got := toolCallName(c.in); got != c.want {
			t.Fatalf("toolCallName(%#v) = %q, want %q", c.in, got, c.want)
		}
	}
}

// ─────────── 边界: 空数组 / 非数组 ───────────

func TestFlatten_空数组(t *testing.T) {
	if out := flattenToolHistory(nil); len(out) != 0 {
		t.Fatalf("nil 应返回空, got %#v", out)
	}
	if out := flattenToolHistory([]any{}); len(out) != 0 {
		t.Fatalf("空数组应返回空, got %#v", out)
	}
}

func TestFlatten_被跳过的条目不进结果(t *testing.T) {
	// :51 `if (!isMessage(raw)) continue;` —— 非对象一律跳过
	msgs := []any{"a string", 42, true, nil, map[string]any{"role": "user", "content": "keep"}}
	out := flattenToolHistory(msgs)
	if len(out) != 1 {
		t.Fatalf("只应保留 1 条, got %d: %#v", len(out), out)
	}
	if out[0].(map[string]any)["content"] != "keep" {
		t.Fatalf("保留的应是 keep, got %#v", out[0])
	}
}

// ─────────── tool_calls 为空数组时不进入分支 2 ───────────

func TestFlatten_空tool_calls数组(t *testing.T) {
	// `Array.isArray(msg.tool_calls)` 对空数组为真 → 进入分支 2,
	// 于是 content 变成 "[Called tools: ]" (名字为空串连接)
	msgs := []any{
		map[string]any{"role": "assistant", "content": "hi", "tool_calls": []any{}},
	}
	out := flattenToolHistory(msgs)
	m := out[0].(map[string]any)
	want := "hi\n" + flattenToolCallPrefix + "]"
	if m["content"] != want {
		t.Fatalf("content 错误\n got: %q\nwant: %q", m["content"], want)
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !containsString(s, sub) {
			return false
		}
	}
	return true
}

func containsString(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
