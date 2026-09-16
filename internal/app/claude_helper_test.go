package app

import (
	"encoding/json"
	"testing"
)

// 本文件锁定 claudeHelper.ts:82-156 的权威行为。
//
// ★ 期望值全部来自「逐字搬运参考实现源码后实跑」（探针 .negbak/probe_final3.mjs），
// 不是读代码推断出来的。case 编号与探针输出一一对应。

func jsonEqHelper(t *testing.T, name string, got any, wantJSON string) {
	t.Helper()
	var want any
	if err := json.Unmarshal([]byte(wantJSON), &want); err != nil {
		t.Fatalf("%s: 期望值不是合法 JSON: %v", name, err)
	}
	gb, _ := json.Marshal(got)
	wb, _ := json.Marshal(want)
	if string(gb) != string(wb) {
		t.Errorf("%s:\n  得   %s\n  期望 %s", name, gb, wb)
	}
}

// 测试用构造器
func hToolUse(id string) map[string]any {
	return map[string]any{"type": "tool_use", "id": id, "name": "f", "input": map[string]any{}}
}
func hToolResult(id, text string) map[string]any {
	return map[string]any{"type": "tool_result", "tool_use_id": id, "content": text}
}
func hText(t string) map[string]any { return map[string]any{"type": "text", "text": t} }

func msgsAny(list ...any) []any { return list }

// ---------------------------------------------------------------- splitMisplacedToolResults

// 探针 S1
func TestSplitMisplaced_空数组(t *testing.T) {
	got := splitMisplacedToolResults(nil)
	jsonEqHelper(t, "S1", got, `[]`)
}

// 探针 S2
func TestSplitMisplaced_单条user原样(t *testing.T) {
	in := msgsAny(map[string]any{"role": "user", "content": msgsAny(hText("hi"))})
	jsonEqHelper(t, "S2", splitMisplacedToolResults(in),
		`[{"role":"user","content":[{"type":"text","text":"hi"}]}]`)
}

// 探针 S3: id 已知 → 搬到前一条 user 的 content 末尾
func TestSplitMisplaced_搬到前一条user(t *testing.T) {
	in := msgsAny(
		map[string]any{"role": "user", "content": msgsAny(hText("go"))},
		map[string]any{"role": "assistant", "content": msgsAny(hToolUse("a"), hToolResult("a", "R"))},
	)
	jsonEqHelper(t, "S3", splitMisplacedToolResults(in),
		`[{"role":"user","content":[{"type":"text","text":"go"}]},
		  {"role":"assistant","content":[{"type":"tool_use","id":"a","name":"f","input":{}}]}]`)
}

// 探针 S4: assistant 只剩 tool_result → 整条丢弃；搬进更早的那条 user
func TestSplitMisplaced_只剩toolResult整条丢弃(t *testing.T) {
	in := msgsAny(
		map[string]any{"role": "assistant", "content": msgsAny(hToolUse("a"))},
		map[string]any{"role": "user", "content": msgsAny(hText("x"))},
		map[string]any{"role": "assistant", "content": msgsAny(hToolResult("a", "R"))},
	)
	jsonEqHelper(t, "S4", splitMisplacedToolResults(in),
		`[{"role":"assistant","content":[{"type":"tool_use","id":"a","name":"f","input":{}}]},
		  {"role":"user","content":[{"type":"text","text":"x"},{"type":"tool_result","tool_use_id":"a","content":"R"}]}]`)
}

// 探针 S5: id 未被更早的 assistant 发出过 → 丢弃（#2815）
func TestSplitMisplaced_未知id被丢弃(t *testing.T) {
	in := msgsAny(
		map[string]any{"role": "user", "content": msgsAny(hText("x"))},
		map[string]any{"role": "assistant", "content": msgsAny(hToolUse("a"), hToolResult("UNKNOWN", "R"))},
	)
	jsonEqHelper(t, "S5", splitMisplacedToolResults(in),
		`[{"role":"user","content":[{"type":"text","text":"x"}]},
		  {"role":"assistant","content":[{"type":"tool_use","id":"a","name":"f","input":{}}]}]`)
}

// 探针 S6: 前一条不是 user → 新建一条 user
func TestSplitMisplaced_前一条非user时新建(t *testing.T) {
	in := msgsAny(
		map[string]any{"role": "assistant", "content": msgsAny(hToolUse("a"))},
		map[string]any{"role": "assistant", "content": msgsAny(hToolResult("a", "R"))},
	)
	jsonEqHelper(t, "S6", splitMisplacedToolResults(in),
		`[{"role":"assistant","content":[{"type":"tool_use","id":"a","name":"f","input":{}}]},
		  {"role":"user","content":[{"type":"tool_result","tool_use_id":"a","content":"R"}]}]`)
}

// 探针 S7: 既有 tool_result 又有 text → text 保留为新 assistant 消息
//
// ★ 注意顺序: 参考实现先把 tool_result 搬进 out 最后一条 user，再推 remaining。
// 但 out 最后一条此刻是 assistant(tool_use)，不是 user → 新建 user，
// 然后 remaining 的 assistant(text) 推在其后。
func TestSplitMisplaced_保留剩余text(t *testing.T) {
	in := msgsAny(
		map[string]any{"role": "assistant", "content": msgsAny(hToolUse("a"))},
		map[string]any{"role": "assistant", "content": msgsAny(hText("left"), hToolResult("a", "R"))},
	)
	jsonEqHelper(t, "S7", splitMisplacedToolResults(in),
		`[{"role":"assistant","content":[{"type":"tool_use","id":"a","name":"f","input":{}}]},
		  {"role":"user","content":[{"type":"tool_result","tool_use_id":"a","content":"R"}]},
		  {"role":"assistant","content":[{"type":"text","text":"left"}]}]`)
}

// 探针 S8: user 消息里的 tool_result 原样（只处理 assistant）
func TestSplitMisplaced_user里的toolResult不动(t *testing.T) {
	in := msgsAny(map[string]any{"role": "user", "content": msgsAny(hToolResult("a", "R"))})
	jsonEqHelper(t, "S8", splitMisplacedToolResults(in),
		`[{"role":"user","content":[{"type":"tool_result","tool_use_id":"a","content":"R"}]}]`)
}

// 探针 S9: content 非数组的 assistant 原样
func TestSplitMisplaced_非数组content原样(t *testing.T) {
	in := msgsAny(map[string]any{"role": "assistant", "content": "plain"})
	jsonEqHelper(t, "S9", splitMisplacedToolResults(in),
		`[{"role":"assistant","content":"plain"}]`)
}

// 探针 S10: 多轮 id 累积 → 两个 tool_result 一起搬
func TestSplitMisplaced_多轮id累积(t *testing.T) {
	in := msgsAny(
		map[string]any{"role": "assistant", "content": msgsAny(hToolUse("a"), hToolUse("b"))},
		map[string]any{"role": "user", "content": msgsAny(hText("r"))},
		map[string]any{"role": "assistant", "content": msgsAny(hToolResult("a", "R"), hToolResult("b", "R"))},
	)
	jsonEqHelper(t, "S10", splitMisplacedToolResults(in),
		`[{"role":"assistant","content":[{"type":"tool_use","id":"a","name":"f","input":{}},{"type":"tool_use","id":"b","name":"f","input":{}}]},
		  {"role":"user","content":[{"type":"text","text":"r"},{"type":"tool_result","tool_use_id":"a","content":"R"},{"type":"tool_result","tool_use_id":"b","content":"R"}]}]`)
}

// 探针 S11: 前一条 user 的 content 不是数组 → 新建
func TestSplitMisplaced_前一条user非数组content(t *testing.T) {
	in := msgsAny(
		map[string]any{"role": "assistant", "content": msgsAny(hToolUse("a"))},
		map[string]any{"role": "user", "content": "plain user"},
		map[string]any{"role": "assistant", "content": msgsAny(hToolResult("a", "R"))},
	)
	jsonEqHelper(t, "S11", splitMisplacedToolResults(in),
		`[{"role":"assistant","content":[{"type":"tool_use","id":"a","name":"f","input":{}}]},
		  {"role":"user","content":"plain user"},
		  {"role":"user","content":[{"type":"tool_result","tool_use_id":"a","content":"R"}]}]`)
}

// 探针 S12: 混合 id —— 有效的搬走，无效的丢弃
func TestSplitMisplaced_混合id(t *testing.T) {
	in := msgsAny(
		map[string]any{"role": "assistant", "content": msgsAny(hToolUse("a"))},
		map[string]any{"role": "user", "content": msgsAny(hText("r"))},
		map[string]any{"role": "assistant", "content": msgsAny(hToolResult("a", "R"), hToolResult("NOPE", "R"))},
	)
	jsonEqHelper(t, "S12", splitMisplacedToolResults(in),
		`[{"role":"assistant","content":[{"type":"tool_use","id":"a","name":"f","input":{}}]},
		  {"role":"user","content":[{"type":"text","text":"r"},{"type":"tool_result","tool_use_id":"a","content":"R"}]}]`)
}

// ★ 补充: nil / 非 map 元素不得 panic（参考实现会抛 TypeError）
func TestSplitMisplaced_畸形元素不崩溃(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("不得 panic: %v", r)
		}
	}()
	in := msgsAny(nil, 42, map[string]any{"role": "assistant", "content": msgsAny(hToolUse("a"))})
	got := splitMisplacedToolResults(in)
	if len(got) != 3 {
		t.Fatalf("应保留 3 条, 得 %d: %#v", len(got), got)
	}
}

// ★ 幂等性: 已正确排布的形态再跑一次不得变动
func TestSplitMisplaced_幂等(t *testing.T) {
	in := msgsAny(
		map[string]any{"role": "assistant", "content": msgsAny(hToolUse("a"))},
		map[string]any{"role": "user", "content": msgsAny(hToolResult("a", "R"))},
	)
	first := splitMisplacedToolResults(in)
	b1, _ := json.Marshal(first)
	// 第二次跑: 此时 tool_result 已在 user 里, 不应再有改动
	second := splitMisplacedToolResults(first)
	b2, _ := json.Marshal(second)
	if string(b1) != string(b2) {
		t.Errorf("非幂等:\n  第一次 %s\n  第二次 %s", b1, b2)
	}
}

// ★ 不得就地改写入参消息对象（参考实现用 `{...msg}` 浅拷贝）
func TestSplitMisplaced_不就地改写原消息(t *testing.T) {
	orig := map[string]any{"role": "assistant", "content": msgsAny(hToolUse("a"), hToolResult("a", "R"))}
	in := msgsAny(
		map[string]any{"role": "user", "content": msgsAny(hText("go"))},
		orig,
	)
	_ = splitMisplacedToolResults(in)

	// 原 assistant 消息的 content 必须仍是 2 个块（未被就地裁剪）
	content, _ := orig["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("原消息被就地改写了: content 变为 %d 个块", len(content))
	}
}

// ---------------------------------------------------------------- hasValidContent

// 探针 HVC 全部 17 条
func TestHasValidContent(t *testing.T) {
	cases := []struct {
		name    string
		content any
		want    bool
	}{
		{"字符串非空", "hi", true},
		{"字符串空白", "   ", false},
		{"字符串空", "", false},
		{"数组含非空text", msgsAny(hText("x")), true},
		{"数组含空text", msgsAny(hText("")), false},
		{"数组含空白text", msgsAny(hText("  ")), false},
		{"数组含thinking", msgsAny(map[string]any{"type": "thinking", "thinking": "t"}), true},
		{"数组含空thinking", msgsAny(map[string]any{"type": "thinking", "thinking": ""}), false},
		{"数组含redacted_thinking带data", msgsAny(map[string]any{"type": "redacted_thinking", "data": "d"}), true},
		{"数组含空data redacted_thinking", msgsAny(map[string]any{"type": "redacted_thinking", "data": " "}), false},
		{"数组含tool_use", msgsAny(hToolUse("a")), true},
		{"数组含tool_result", msgsAny(hToolResult("a", "R")), true},
		{"数组含image_#7777", msgsAny(map[string]any{"type": "image"}), true},
		{"数组含document_#7777", msgsAny(map[string]any{"type": "document"}), true},
		{"数组只含未知类型", msgsAny(map[string]any{"type": "weird"}), false},
		{"content是数字", 42, false},
		{"content为null", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := hasValidContent(map[string]any{"content": c.content})
			if got != c.want {
				t.Errorf("得 %v, 期望 %v", got, c.want)
			}
		})
	}
}

// ★ #7777 的语义必须显式锁定: 纯媒体 user 回合是真实内容。
// 这条如果被改坏, 表现是"视觉输入被静默删除" —— 没有任何报错。
func TestHasValidContent_纯媒体回合是真内容(t *testing.T) {
	for _, blockType := range []string{"image", "document"} {
		t.Run(blockType, func(t *testing.T) {
			msg := map[string]any{"role": "user", "content": msgsAny(map[string]any{"type": blockType})}
			if !hasValidContent(msg) {
				t.Errorf("#7777 回归: 纯 %s 回合被判为空 —— 视觉输入会被静默丢弃", blockType)
			}
		})
	}
}

// ★ hasValidContent 对 nil map 不得 panic
func TestHasValidContent_nil不崩溃(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("不得 panic: %v", r)
		}
	}()
	if hasValidContent(nil) {
		t.Error("nil msg 应为 false")
	}
}
