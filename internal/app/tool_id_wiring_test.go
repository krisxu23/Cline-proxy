package app

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

// jstr 把任意值序列化成 JSON 文本, 用于"整份结果是否一致"的比较。
func jstr(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal 失败: %v", err)
	}
	return string(b)
}

// 本文件锁定 sanitizeClaudeToolIDs 的行为。
//
// 全部期望值来自**实跑** OmniRoute 参考实现得到的输出
// (探针 .negbak/probe_toolid_wiring.mjs, 逐字搬运 schemaCoercion.ts:449-453
// 的 sanitizeToolId 与 claudeHelper.ts:432-450 的 Pass 1.4)。禁止凭读代码推断。

// toolIDAllowedRe 是上游 Anthropic 对 tool id 强制的字符集约束。
var toolIDAllowedRe = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// walkToolIDs 收集一份 messages 里所有 tool_use.id 与 tool_result.tool_use_id。
func walkToolIDs(t *testing.T, messages []any) (uses []string, results []string, blockCount int) {
	t.Helper()
	for _, raw := range messages {
		msg, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("消息不是对象: %T", raw)
		}
		blocks, ok := msg["content"].([]any)
		if !ok {
			continue
		}
		for _, bRaw := range blocks {
			b, ok := bRaw.(map[string]any)
			if !ok {
				t.Fatal("块不是对象")
			}
			blockCount++
			switch blockType(b) {
			case "tool_use":
				id, _ := b["id"].(string)
				uses = append(uses, id)
			case "tool_result":
				id, _ := b["tool_use_id"].(string)
				results = append(results, id)
			}
		}
	}
	return
}

// TestSanitizeClaudeToolIDs_配对双侧同改 对应探针 T1。
//
// ★ 这是本模块存在的意义: 只改一侧会让配对断裂, 400 从 "invalid id" 变成
// "tool_use ids were found without tool_result blocks"。
func TestSanitizeClaudeToolIDs_配对双侧同改(t *testing.T) {
	in := []any{
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "tool_use", "id": "call.abc:123", "name": "Read", "input": map[string]any{}},
		}},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "call.abc:123", "content": "ok"},
		}},
	}
	out, changed := sanitizeClaudeToolIDs(in)
	if !changed {
		t.Fatal("应报告已改动")
	}
	uses, results, _ := walkToolIDs(t, out)
	// 权威值 (探针 T1): "call.abc:123" -> "call_abc_123"
	if len(uses) != 1 || uses[0] != "call_abc_123" {
		t.Fatalf("tool_use.id = %q, 期望 %q", uses, "call_abc_123")
	}
	if len(results) != 1 || results[0] != "call_abc_123" {
		t.Fatalf("tool_result.tool_use_id = %q, 期望 %q", results, "call_abc_123")
	}
	// ★ 配对必须存活
	if uses[0] != results[0] {
		t.Fatalf("配对断裂: tool_use=%q tool_result=%q", uses[0], results[0])
	}
}

// TestSanitizeClaudeToolIDs_空名tool_use被过滤 对应探针 T2。
func TestSanitizeClaudeToolIDs_空名tool_use被过滤(t *testing.T) {
	in := []any{
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "tool_use", "id": "x1", "name": "", "input": map[string]any{}},
			map[string]any{"type": "tool_use", "id": "x2", "name": "  ", "input": map[string]any{}},
			map[string]any{"type": "tool_use", "id": "x3", "name": "Read", "input": map[string]any{}},
		}},
	}
	out, changed := sanitizeClaudeToolIDs(in)
	if !changed {
		t.Fatal("应报告已改动")
	}
	msgs := out[0].(map[string]any)
	blocks := msgs["content"].([]any)
	// 权威值 (探针 T2): 剩余 1 块, id = ["x3"]
	if len(blocks) != 1 {
		t.Fatalf("应剩 1 块, got %d", len(blocks))
	}
	if id := blocks[0].(map[string]any)["id"]; id != "x3" {
		t.Fatalf("剩余 id = %v, 期望 x3", id)
	}
}

// TestSanitizeClaudeToolIDs_缺id的tool_result被过滤 对应探针 T3。
//
// ★ 反直觉点: 缺 id 的 tool_result **必须被删掉, 而不是补一个新 id**。
// 参考实现刻意用 sanitizeToolResultId 之外的方式处理 —— sanitizeToolId 对
// falsy 输入会新造 `tool_<uuid>`, 那会凭空造出一个永远配不上 tool_use 的结果块
// (sanitizeToolResultId.ts 原注释: "defeat that guard and silently fabricate
// a tool_result that can never match a tool_use")。
func TestSanitizeClaudeToolIDs_缺id的tool_result被过滤(t *testing.T) {
	in := []any{
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "", "content": "a"},
			map[string]any{"type": "tool_result", "content": "b"},
			map[string]any{"type": "tool_result", "tool_use_id": "keep.me", "content": "c"},
		}},
	}
	out, changed := sanitizeClaudeToolIDs(in)
	if !changed {
		t.Fatal("应报告已改动")
	}
	blocks := out[0].(map[string]any)["content"].([]any)
	// 权威值 (探针 T3): 剩余 1 块, tool_use_id = "keep_me"
	if len(blocks) != 1 {
		t.Fatalf("应剩 1 块(缺 id 的两块被删), got %d", len(blocks))
	}
	got := blocks[0].(map[string]any)["tool_use_id"]
	if got != "keep_me" {
		t.Fatalf("tool_use_id = %v, 期望 keep_me", got)
	}
}

// TestSanitizeClaudeToolIDs_多字符替换 对应探针 T4。
func TestSanitizeClaudeToolIDs_多字符替换(t *testing.T) {
	in := []any{
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "tool_use", "id": "a#b", "name": "T1", "input": map[string]any{}},
			map[string]any{"type": "tool_use", "id": "with space", "name": "T2", "input": map[string]any{}},
			map[string]any{"type": "tool_use", "id": "已经合法-ok_1", "name": "T3", "input": map[string]any{}},
		}},
	}
	out, _ := sanitizeClaudeToolIDs(in)
	uses, _, _ := walkToolIDs(t, out)
	// 权威值 (探针 T4): ["a_b","with_space","____-ok_1"]
	want := []string{"a_b", "with_space", "____-ok_1"}
	if len(uses) != len(want) {
		t.Fatalf("块数 %d, 期望 %d", len(uses), len(want))
	}
	for i := range want {
		if uses[i] != want[i] {
			t.Fatalf("ids[%d] = %q, 期望 %q (全部: %v)", i, uses[i], want[i], uses)
		}
	}
}

// TestSanitizeClaudeToolIDs_合法id不变 对应探针 T5 —— no-op 幂等。
func TestSanitizeClaudeToolIDs_合法id不变(t *testing.T) {
	in := []any{
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "tool_use", "id": "toolu_01ABC-def_2", "name": "Read", "input": map[string]any{}},
		}},
	}
	out, changed := sanitizeClaudeToolIDs(in)
	if changed {
		t.Fatalf("合法 id 不应报告改动, got %s", jstr(t, out))
	}
	uses, _, _ := walkToolIDs(t, out)
	if len(uses) != 1 || uses[0] != "toolu_01ABC-def_2" {
		t.Fatalf("合法 id 被改写了: %v", uses)
	}
}

// TestSanitizeClaudeToolIDs_内容非数组不动 对应探针 T6。
func TestSanitizeClaudeToolIDs_内容非数组不动(t *testing.T) {
	in := []any{
		map[string]any{"role": "assistant", "content": "plain text"},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "content": "orphan"},
		}},
	}
	out, changed := sanitizeClaudeToolIDs(in)
	if got := out[0].(map[string]any)["content"]; got != "plain text" {
		t.Fatalf("非数组 content 被改: %v", got)
	}
	// 权威值 (探针 T6): msgs[1] blocks = 0 (缺 id 的 tool_result 被删)
	if blocks := out[1].(map[string]any)["content"].([]any); len(blocks) != 0 {
		t.Fatalf("缺 id 的 tool_result 应被删, 剩余 %d", len(blocks))
	}
	if !changed {
		t.Fatal("应报告已改动(第二块被删)")
	}
}

// TestSanitizeClaudeToolIDs_user回合的tool_use也净化 对应探针 T7。
//
// 参考实现注释: "Apply to ALL roles (assistant tool_use + any user messages
// that may carry tool_use)"。
func TestSanitizeClaudeToolIDs_user回合的tool_use也净化(t *testing.T) {
	in := []any{
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_use", "id": "u.1:2", "name": "Read", "input": map[string]any{}},
		}},
	}
	out, _ := sanitizeClaudeToolIDs(in)
	uses, _, _ := walkToolIDs(t, out)
	// 权威值 (探针 T7): "u.1:2" -> "u_1_2"
	if len(uses) != 1 || uses[0] != "u_1_2" {
		t.Fatalf("user 回合的 tool_use.id = %v, 期望 u_1_2", uses)
	}
}

// TestSanitizeClaudeToolIDs_净化后恒合法 覆盖探针 T9 的字符集结论。
func TestSanitizeClaudeToolIDs_净化后恒合法(t *testing.T) {
	dirty := []string{
		".", ":", "#", " ", "!!!", "call.abc:123",
		"toolu:xyz", "a#b", "with space", "已经合法-ok_1",
		"a/b", "a\\b", "a@b", "a+b", "a=b", "a?b", "a&b", "a%b", "a$b", "a!b",
		"a,b", "a;b", "a(b)", "a[b]", "a{b}", "a<b>", "a'b", `a"b`, "a|b", "a~b",
	}
	for _, d := range dirty {
		got := sanitizeToolID(d)
		if !toolIDAllowedRe.MatchString(got) {
			t.Fatalf("净化后仍非法: %q -> %q", d, got)
		}
	}
}

// TestSanitizeClaudeToolIDs_净化不兜底为随机 对应探针 T9。
//
// ★ 反直觉点: `"."` 净化成 `"_"` —— 非空, 因此**不**进入随机兜底分支。
// "看着像该兜底" 就加一条 `if got == "" || got == "_"` 会偏离参考实现。
func TestSanitizeClaudeToolIDs_净化不兜底为随机(t *testing.T) {
	cases := map[string]string{
		".":   "_",
		":":   "_",
		"#":   "_",
		" ":   "_",
		"!!!": "___",
	}
	for in, want := range cases {
		got := sanitizeToolID(in)
		if got != want {
			t.Fatalf("sanitizeToolID(%q) = %q, 期望 %q", in, got, want)
		}
		if strings.HasPrefix(got, "tool_") {
			t.Fatalf("sanitizeToolID(%q) 不应兜底为随机 id, got %q", in, got)
		}
	}
}

// TestSanitizeClaudeToolIDs_非字符串id不动 对应探针 T10。
//
// 参考实现 `typeof block.id === "string"` 守卫让数字 id 原样透传。
func TestSanitizeClaudeToolIDs_非字符串id不动(t *testing.T) {
	in := []any{
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "tool_use", "id": float64(12345), "name": "Read", "input": map[string]any{}},
		}},
	}
	out, changed := sanitizeClaudeToolIDs(in)
	if changed {
		t.Fatal("非字符串 id 不应触发改动")
	}
	blocks := out[0].(map[string]any)["content"].([]any)
	if got := blocks[0].(map[string]any)["id"]; got != float64(12345) {
		t.Fatalf("数字 id 被改写: %v", got)
	}
}

// TestSanitizeClaudeToolIDs_幂等 —— 跑两遍结果一致。
func TestSanitizeClaudeToolIDs_幂等(t *testing.T) {
	in := []any{
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "tool_use", "id": "a.b:c", "name": "Read", "input": map[string]any{}},
		}},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "a.b:c", "content": "r"},
		}},
	}
	once, _ := sanitizeClaudeToolIDs(in)
	twice, _ := sanitizeClaudeToolIDs(once)
	if jstr(t, once) != jstr(t, twice) {
		t.Fatalf("非幂等:\n once=%s\ntwice=%s", jstr(t, once), jstr(t, twice))
	}
}

// TestSanitizeClaudeToolIDs_不就地改写入参 —— 调用方持有的对象不得被污染。
func TestSanitizeClaudeToolIDs_不就地改写入参(t *testing.T) {
	orig := map[string]any{"type": "tool_use", "id": "a.b", "name": "Read", "input": map[string]any{}}
	msg := map[string]any{"role": "assistant", "content": []any{orig}}
	in := []any{msg}

	_, changed := sanitizeClaudeToolIDs(in)
	if !changed {
		t.Fatal("应报告已改动")
	}
	if orig["id"] != "a.b" {
		t.Fatalf("入参块被就地改写: id = %v, 期望 a.b", orig["id"])
	}
	if msg["content"].([]any)[0].(map[string]any)["id"] != "a.b" {
		t.Fatal("入参消息被就地改写")
	}
}

// TestSanitizeClaudeToolIDs_畸形元素不崩溃 —— 我方刻意不照抄参考实现对 null
// 抛 TypeError 的行为(与 claude_helper.go 的既定处理一致)。
func TestSanitizeClaudeToolIDs_畸形元素不崩溃(t *testing.T) {
	in := []any{
		nil,
		"not-a-message",
		map[string]any{"role": "user", "content": "str"},
		map[string]any{"role": "user", "content": []any{nil, "str", float64(1)}},
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "tool_use", "id": "ok.id", "name": "Read"},
		}},
	}
	out, _ := sanitizeClaudeToolIDs(in)
	if len(out) != len(in) {
		t.Fatalf("消息条数不应变化: %d -> %d", len(in), len(out))
	}
}

// TestSanitizeClaudeToolIDs_空输入 —— 空切片返回空切片(不返回 nil)。
func TestSanitizeClaudeToolIDs_空输入(t *testing.T) {
	out, changed := sanitizeClaudeToolIDs([]any{})
	if changed {
		t.Fatal("空输入不应报告改动")
	}
	if out == nil {
		t.Fatal("应返回空切片而非 nil")
	}
	if len(out) != 0 {
		t.Fatalf("空输入应返回空, got %d", len(out))
	}
}

// TestSanitizeClaudeToolIDs_混合形态端到端 —— 一份真实的"另一个 provider 重放"
// 历史, 一次性覆盖过滤 + 双侧净化 + 配对存活。
func TestSanitizeClaudeToolIDs_混合形态端到端(t *testing.T) {
	in := []any{
		map[string]any{"role": "user", "content": "start"},
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "text", "text": "calling"},
			map[string]any{"type": "tool_use", "id": "call_1.x:y", "name": "Read", "input": map[string]any{}},
			map[string]any{"type": "tool_use", "id": "call_2", "name": "  ", "input": map[string]any{}},
		}},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "call_1.x:y", "content": "file body"},
			map[string]any{"type": "tool_result", "content": "orphan no id"},
		}},
	}
	out, changed := sanitizeClaudeToolIDs(in)
	if !changed {
		t.Fatal("应报告已改动")
	}
	uses, results, _ := walkToolIDs(t, out)
	// 空名 tool_use 被删 -> 只剩 1 个 tool_use
	if len(uses) != 1 || uses[0] != "call_1_x_y" {
		t.Fatalf("tool_use = %v, 期望 [call_1_x_y]", uses)
	}
	// 缺 id 的 tool_result 被删 -> 只剩 1 个
	if len(results) != 1 || results[0] != "call_1_x_y" {
		t.Fatalf("tool_result = %v, 期望 [call_1_x_y]", results)
	}
	if uses[0] != results[0] {
		t.Fatalf("配对断裂: %v vs %v", uses, results)
	}
}
