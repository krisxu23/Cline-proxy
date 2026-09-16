package app

import (
	"encoding/json"
	"testing"
)

// 本文件锁定 fixToolUseOrdering。
//
// 全部期望值来自**实跑** OmniRoute 参考实现
// (探针 .negbak/probe_fixtooluseordering.mjs, 逐字搬运
// translator/helpers/claudeHelper.ts:160-284)。

func toj(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal 失败: %v", err)
	}
	return string(b)
}

// ftu 造一个 tool_use 块。
func ftu(id string) map[string]any {
	return map[string]any{"type": "tool_use", "id": id, "name": "Read", "input": map[string]any{}}
}

// ftr 造一个 tool_result 块。
func ftr(id string, content any) map[string]any {
	return map[string]any{"type": "tool_result", "tool_use_id": id, "content": content}
}

// ftxt 造一个 text 块。
func ftxt(s string) map[string]any {
	return map[string]any{"type": "text", "text": s}
}

// TestFixToolUseOrdering_空与单条 对应探针 F1/F2/F3。
func TestFixToolUseOrdering_空与单条(t *testing.T) {
	// F1: 空数组 -> 原引用
	empty := []any{}
	if got := fixToolUseOrderingClaudeShape(empty); len(got) != 0 {
		t.Fatalf("空数组应返回空, got %s", toj(t, got))
	}

	// F2: 单条不含 tool_result -> 原引用。
	//
	// ★ 判"同一底层数组"用 `&x[0] == &y[0]` —— 比较的是元素地址(指针),
	//   不是 map 值本身, 故不会触发 Go 的不可比较 panic。
	one := []any{map[string]any{"role": "user", "content": []any{ftxt("hi")}}}
	if got := fixToolUseOrderingClaudeShape(one); &got[0] != &one[0] {
		t.Fatal("单条不含 tool_result 时应原样返回")
	}

	// F3: 单条**含** tool_result -> 继续处理, 因上一条不是 assistant, 全降级
	got := fixToolUseOrderingClaudeShape([]any{
		map[string]any{"role": "user", "content": []any{ftr("t1", "out")}},
	})
	want := `[{"content":[{"text":"[Unpaired tool result t1]\nout","type":"text"}],"role":"user"}]`
	if toj(t, got) != want {
		t.Fatalf("F3 不符:\n got=%s\nwant=%s", toj(t, got), want)
	}
}

// TestFixToolUseOrdering_Pass1删tool_use之后的text 对应探针 F4/F5。
//
// ★ 这是 Claude 协议的位置约束: assistant 回合里 tool_use 之后不能跟 text。
func TestFixToolUseOrdering_Pass1删tool_use之后的text(t *testing.T) {
	// F4: before 保留, after/trailing 删
	got := fixToolUseOrderingClaudeShape([]any{
		map[string]any{"role": "user", "content": []any{ftxt("q")}},
		map[string]any{"role": "assistant", "content": []any{
			ftxt("before"), ftu("a1"), ftxt("after"), ftu("a2"), ftxt("trailing"),
		}},
	})
	want := `[{"content":[{"text":"q","type":"text"}],"role":"user"},` +
		`{"content":[{"text":"before","type":"text"},` +
		`{"id":"a1","input":{},"name":"Read","type":"tool_use"},` +
		`{"id":"a2","input":{},"name":"Read","type":"tool_use"}],"role":"assistant"}]`
	if toj(t, got) != want {
		t.Fatalf("F4 不符:\n got=%s\nwant=%s", toj(t, got), want)
	}

	// F5: thinking 与 tool_use 之前的 text 都保留
	got5 := fixToolUseOrderingClaudeShape([]any{
		map[string]any{"role": "user", "content": []any{ftxt("q")}},
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "thinking", "thinking": "th"}, ftxt("pre"), ftu("a1"), ftxt("post"),
		}},
	})
	want5 := `[{"content":[{"text":"q","type":"text"}],"role":"user"},` +
		`{"content":[{"thinking":"th","type":"thinking"},{"text":"pre","type":"text"},` +
		`{"id":"a1","input":{},"name":"Read","type":"tool_use"}],"role":"assistant"}]`
	if toj(t, got5) != want5 {
		t.Fatalf("F5 不符:\n got=%s\nwant=%s", toj(t, got5), want5)
	}
}

// TestFixToolUseOrdering_Pass2合并同role 对应探针 F6/F7/F18。
//
// ★ 合并时 tool_result **全部提前**, 其余内容保持相对顺序。
func TestFixToolUseOrdering_Pass2合并同role(t *testing.T) {
	// F6: 三条 user 合成一条, tool_result 在前
	got := fixToolUseOrderingClaudeShape([]any{
		map[string]any{"role": "assistant", "content": []any{ftu("a1")}},
		map[string]any{"role": "user", "content": []any{ftxt("u1")}},
		map[string]any{"role": "user", "content": []any{ftr("a1", "out1"), ftxt("u2")}},
		map[string]any{"role": "user", "content": []any{ftxt("u3")}},
	})
	want := `[{"content":[{"id":"a1","input":{},"name":"Read","type":"tool_use"}],"role":"assistant"},` +
		`{"content":[{"content":"out1","tool_use_id":"a1","type":"tool_result"},` +
		`{"text":"u1","type":"text"},{"text":"u2","type":"text"},{"text":"u3","type":"text"}],"role":"user"}]`
	if toj(t, got) != want {
		t.Fatalf("F6 不符:\n got=%s\nwant=%s", toj(t, got), want)
	}

	// F7: 字符串 content 被包成 text 块
	got7 := fixToolUseOrderingClaudeShape([]any{
		map[string]any{"role": "user", "content": "hi"},
		map[string]any{"role": "user", "content": []any{ftxt("there")}},
	})
	want7 := `[{"content":[{"text":"hi","type":"text"},{"text":"there","type":"text"}],"role":"user"}]`
	if toj(t, got7) != want7 {
		t.Fatalf("F7 不符:\n got=%s\nwant=%s", toj(t, got7), want7)
	}

	// F18: 长度 2 且同 role -> 输出长度 1
	if n := len(got7); n != 1 {
		t.Fatalf("F18: 合并后长度应为 1, got %d", n)
	}
}

// TestFixToolUseOrdering_Pass3失去配对降级 对应探针 F8/F10/F14。
//
// ★ 参考实现原注释: Claude 只接受"紧邻上一条 assistant 的 tool_use"的
//
//	tool_result; 被压缩掉的跨模型历史可能留下"有输出无调用", 此时把它保留成
//	**user 文本** —— 既不发无效结构化引用, 也不丢弃有用上下文。
func TestFixToolUseOrdering_Pass3失去配对降级(t *testing.T) {
	// F8: ghost 降级, a1 保留
	got := fixToolUseOrderingClaudeShape([]any{
		map[string]any{"role": "assistant", "content": []any{ftu("a1")}},
		map[string]any{"role": "user", "content": []any{ftr("ghost", "lost-output"), ftr("a1", "good")}},
	})
	want := `[{"content":[{"id":"a1","input":{},"name":"Read","type":"tool_use"}],"role":"assistant"},` +
		`{"content":[{"content":"good","tool_use_id":"a1","type":"tool_result"},` +
		`{"text":"[Unpaired tool result ghost]\nlost-output","type":"text"}],"role":"user"}]`
	if toj(t, got) != want {
		t.Fatalf("F8 不符:\n got=%s\nwant=%s", toj(t, got), want)
	}

	// F10: 重复 tool_result 也降级
	got10 := fixToolUseOrderingClaudeShape([]any{
		map[string]any{"role": "assistant", "content": []any{ftu("a1")}},
		map[string]any{"role": "user", "content": []any{ftr("a1", "first"), ftr("a1", "dup")}},
	})
	want10 := `[{"content":[{"id":"a1","input":{},"name":"Read","type":"tool_use"}],"role":"assistant"},` +
		`{"content":[{"content":"first","tool_use_id":"a1","type":"tool_result"},` +
		`{"text":"[Unpaired tool result a1]\ndup","type":"text"}],"role":"user"}]`
	if toj(t, got10) != want10 {
		t.Fatalf("F10 不符:\n got=%s\nwant=%s", toj(t, got10), want10)
	}

	// F14: 上一条不是 assistant -> validIds 空 -> 两条 user 合并后全降级
	got14 := fixToolUseOrderingClaudeShape([]any{
		map[string]any{"role": "user", "content": []any{ftr("a1", "out")}},
		map[string]any{"role": "user", "content": []any{ftxt("more")}},
	})
	want14 := `[{"content":[{"text":"[Unpaired tool result a1]\nout","type":"text"},` +
		`{"text":"more","type":"text"}],"role":"user"}]`
	if toj(t, got14) != want14 {
		t.Fatalf("F14 不符:\n got=%s\nwant=%s", toj(t, got14), want14)
	}
}

// TestFixToolUseOrdering_Pass3补空占位 对应探针 F9。
//
// ★ 反直觉点: 某个 tool_use 若**没有**对应 tool_result, 参考实现会**补一个
//
//	content 为空的占位**, 而不是省略它。且输出顺序按 validIds 顺序。
func TestFixToolUseOrdering_Pass3补空占位(t *testing.T) {
	got := fixToolUseOrderingClaudeShape([]any{
		map[string]any{"role": "assistant", "content": []any{ftu("a1"), ftu("a2")}},
		map[string]any{"role": "user", "content": []any{ftr("a2", "only-a2")}},
	})
	want := `[{"content":[{"id":"a1","input":{},"name":"Read","type":"tool_use"},` +
		`{"id":"a2","input":{},"name":"Read","type":"tool_use"}],"role":"assistant"},` +
		`{"content":[{"content":"","tool_use_id":"a1","type":"tool_result"},` +
		`{"content":"only-a2","tool_use_id":"a2","type":"tool_result"}],"role":"user"}]`
	if toj(t, got) != want {
		t.Fatalf("F9 不符:\n got=%s\nwant=%s", toj(t, got), want)
	}
}

// TestFixToolUseOrdering_Pass3降级序列化 对应探针 F11/F12/F13/F19/F20/F21。
//
// ★ 这是本项最容易抄错的地方 —— 降级文本里的 serialized 部分:
//   - 字符串 content 原样;
//   - 非字符串走 `JSON.stringify(content ?? "")`: null/undefined 先变空串,
//     再 stringify 得 **`""`(两个引号字符)**;
//   - HTML 字符 `<> &` **不转义**（Go 的 json.Marshal 默认会转义, 必须还原）。
func TestFixToolUseOrdering_Pass3降级序列化(t *testing.T) {
	cases := []struct {
		name    string
		block   map[string]any
		wantTxt string
	}{
		{
			// ★ 注意 JSON 键序: Go 的 encoding/json 按字典序输出
			// (`text` 在 `type` 前), 而 Node 的 JSON.stringify 保持插入顺序。
			// 这里按 **Go 的输出** 写期望值, 语义与参考实现一致。
			"F11 嵌套数组",
			map[string]any{"type": "tool_result", "tool_use_id": "ghost",
				"content": []any{ftxt("nested")}},
			"[Unpaired tool result ghost]\n[{\"text\":\"nested\",\"type\":\"text\"}]",
		},
		{
			"F12 content=null",
			map[string]any{"type": "tool_result", "tool_use_id": "ghost", "content": nil},
			"[Unpaired tool result ghost]\n\"\"",
		},
		{
			"F13 无 tool_use_id",
			map[string]any{"type": "tool_result", "content": "x"},
			"[Unpaired tool result unknown]\nx",
		},
		{
			"F19 HTML 不转义",
			map[string]any{"type": "tool_result", "tool_use_id": "ghost", "content": "<a&b>"},
			"[Unpaired tool result ghost]\n<a&b>",
		},
		{
			"F20 数字",
			map[string]any{"type": "tool_result", "tool_use_id": "g1", "content": float64(42)},
			"[Unpaired tool result g1]\n42",
		},
		{
			"F20 布尔",
			map[string]any{"type": "tool_result", "tool_use_id": "g2", "content": false},
			"[Unpaired tool result g2]\nfalse",
		},
		{
			"F21 键不存在(undefined)",
			map[string]any{"type": "tool_result", "tool_use_id": "g1"},
			"[Unpaired tool result g1]\n\"\"",
		},
	}
	for _, c := range cases {
		got := fixToolUseOrderingClaudeShape([]any{
			map[string]any{"role": "assistant", "content": []any{ftu("a1")}},
			map[string]any{"role": "user", "content": []any{c.block}},
		})
		// 输出末尾必是一个 text 块, 取它的 text。
		msgs := got
		userMsg := msgs[len(msgs)-1].(map[string]any)
		blocks := userMsg["content"].([]any)
		last := blocks[len(blocks)-1].(map[string]any)
		if last["type"] != "text" {
			t.Errorf("%s: 末块应为 text, got %v", c.name, last["type"])
			continue
		}
		if last["text"] != c.wantTxt {
			t.Errorf("%s: 降级文本不符\n got=%q\nwant=%q", c.name, last["text"], c.wantTxt)
		}
	}
}

// TestFixToolUseOrdering_HTML转义还原 —— 单点锁死 jsJSONStringify 的还原逻辑。
//
// Go 的 encoding/json 默认 escapeHTML=true, 会把 < > & 写成 \u003c 等;
// 参考实现的 JSON.stringify 不转义。若有人去掉还原步骤, 本测试立刻变红。
func TestFixToolUseOrdering_HTML转义还原(t *testing.T) {
	if got := jsJSONStringify("<a&b>"); got != `"<a&b>"` {
		t.Fatalf("HTML 字符不应被转义, got %q", got)
	}
	if got := jsJSONStringify(nil); got != `""` {
		t.Fatalf("nil 应先变空串再 stringify, got %q", got)
	}
	if got := jsJSONStringify(float64(42)); got != "42" {
		t.Fatalf("数字应输出 42, got %q", got)
	}
	if got := jsJSONStringify("plain"); got != `"plain"` {
		t.Fatalf("字符串应加引号, got %q", got)
	}
}

// TestFixToolUseOrdering_混合场景 对应探针 F15。
func TestFixToolUseOrdering_混合场景(t *testing.T) {
	got := fixToolUseOrderingClaudeShape([]any{
		map[string]any{"role": "user", "content": []any{ftxt("do it")}},
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "thinking", "thinking": "let me"}, ftu("c1"), ftxt("done"),
		}},
		map[string]any{"role": "user", "content": []any{ftr("c1", "file-content")}},
		map[string]any{"role": "assistant", "content": []any{ftu("c2")}},
		map[string]any{"role": "user", "content": []any{ftr("c2", "r2")}},
	})
	want := `[{"content":[{"text":"do it","type":"text"}],"role":"user"},` +
		`{"content":[{"thinking":"let me","type":"thinking"},` +
		`{"id":"c1","input":{},"name":"Read","type":"tool_use"}],"role":"assistant"},` +
		`{"content":[{"content":"file-content","tool_use_id":"c1","type":"tool_result"}],"role":"user"},` +
		`{"content":[{"id":"c2","input":{},"name":"Read","type":"tool_use"}],"role":"assistant"},` +
		`{"content":[{"content":"r2","tool_use_id":"c2","type":"tool_result"}],"role":"user"}]`
	if toj(t, got) != want {
		t.Fatalf("F15 不符:\n got=%s\nwant=%s", toj(t, got), want)
	}
}

// TestFixToolUseOrdering_首条同role是浅拷贝 对应探针 F16/F16b。
//
// ★ 参考实现 `merged.push({ role: msg.role, content: [...content] })` 会**新建**
//
//	对象与新的 content 数组, 因此后续合并不会污染调用方传入的对象。
//	实测 F16b: `out[0] === origA` 为 false, `out[0].content === origA.content` 也为 false。
//
// ★ 实现注意: Go 的 map **不可比较**（`got[0] == any(origA)` 会在运行期
//
//	panic: comparing uncomparable type map[string]interface {}）。故改用
//	"合并是否改写了入参对象的 content" 来判定 —— 这是浅拷贝语义的可观察后果。
func TestFixToolUseOrdering_首条同role是浅拷贝(t *testing.T) {
	origA := map[string]any{"role": "user", "content": []any{ftxt("a")}}
	origB := map[string]any{"role": "user", "content": []any{ftxt("b")}}
	inContent := origA["content"].([]any)
	got := fixToolUseOrderingClaudeShape([]any{origA, origB})

	// 入参对象未被就地改写
	if n := len(origA["content"].([]any)); n != 1 {
		t.Fatalf("F16: 入参 origA.content 长度应仍为 1, got %d", n)
	}
	if n := len(origB["content"].([]any)); n != 1 {
		t.Fatalf("F16: 入参 origB.content 长度应仍为 1, got %d", n)
	}
	if n := len(inContent); n != 1 {
		t.Fatalf("F16: 之前取到的入参切片也应仍为 1, got %d", n)
	}
	// 输出应把两条合成一条, 且首条的 content 有 2 个块(入参只有 1 个)。
	if len(got) != 1 {
		t.Fatalf("F16b: 合并后长度应为 1, got %d", len(got))
	}
	out0 := got[0].(map[string]any)
	outContent := out0["content"].([]any)
	if len(outContent) != 2 {
		t.Fatalf("F16b: 合并后首条应有 2 个块, got %d", len(outContent))
	}
}

// TestFixToolUseOrdering_非对象容忍 —— ★ 有意偏离参考实现。
//
// 探针 F17/F22 实测: 参考实现对 `null` 消息 / `null` 块会抛
// `TypeError: Cannot read properties of null`。本网关是长驻服务, 一个 panic 会
// 打掉整个进程(比一次 400 严重得多), 故 Go 侧**容忍**并跳过。
//
// 这是照抄纪律里唯一允许的偏离类型: 参考实现在该输入上的行为是**崩溃**,
// 不构成可照抄的语义。此处显式登记, 防止后人误以为抄漏了。
func TestFixToolUseOrdering_非对象容忍(t *testing.T) {
	// null 消息: 不 panic, 且后续消息仍被处理
	got := fixToolUseOrderingClaudeShape([]any{
		nil,
		map[string]any{"role": "user", "content": []any{ftxt("x")}},
	})
	if len(got) != 1 {
		t.Fatalf("null 消息应被跳过, got %d 条", len(got))
	}
	if m, ok := got[0].(map[string]any); !ok || m["role"] != "user" {
		t.Fatalf("应只保留 user 消息, got %s", toj(t, got))
	}

	// null 块: 不 panic
	got2 := fixToolUseOrderingClaudeShape([]any{
		map[string]any{"role": "assistant", "content": []any{ftu("a1")}},
		map[string]any{"role": "user", "content": []any{ftxt("keep"), nil, ftr("a1", "ok")}},
	})
	if len(got2) != 2 {
		t.Fatalf("null 块不应影响结构, got %d 条", len(got2))
	}
}

// TestFixToolUseOrdering_幂等 —— 干净历史上跑两遍结果一致。
func TestFixToolUseOrdering_幂等(t *testing.T) {
	in := []any{
		map[string]any{"role": "user", "content": []any{ftxt("do it")}},
		map[string]any{"role": "assistant", "content": []any{ftu("c1"), ftxt("tail")}},
		map[string]any{"role": "user", "content": []any{ftr("c1", "out")}},
	}
	once := fixToolUseOrderingClaudeShape(in)
	twice := fixToolUseOrderingClaudeShape(once)
	if toj(t, once) != toj(t, twice) {
		t.Fatalf("非幂等:\n once=%s\ntwice=%s", toj(t, once), toj(t, twice))
	}
}

// TestFixToolUseOrdering_形态守卫 —— ★ 锁死"非 Claude 形态不跑纯变换"这条前置条件。
//
// 背景: 参考实现里本函数只在 `targetFormat === CLAUDE` 且翻译已完成后执行
// (translator/index.ts:567 是最后一步), 因此它**必然**见到 Claude 形态的消息
// (tool 调用在 content[] 的 tool_use 块里, 没有 tool_calls 字段 ——
// openaiToClaudeRequest 已转换, 见 openai-to-claude.ts:660-675)。
//
// 本网关的出站咽喉拿的是客户端**原始 body**, 可能仍是 OpenAI 形态。此时跑纯变换
// 会把 `content:null` 包成 `[{type:"text",text:null}]` 并**丢掉 tool_calls 信息**,
// 还会让下游 stripTrailingAssistantOrphanToolUse 漏判(它依赖 tool_calls 字段)。
// 实测: 不加守卫时该缺口直接让两条既有接入测试变红。
func TestFixToolUseOrdering_形态守卫(t *testing.T) {
	// OpenAI 形态(带 tool_calls): 必须**原样返回同一引用**, 不得改写。
	openAI := []any{
		map[string]any{"role": "user", "content": "read the file"},
		map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{
			map[string]any{"id": "c1", "type": "function",
				"function": map[string]any{"name": "Read", "arguments": `{"p":"a"}`}},
		}},
	}
	got := fixToolUseOrdering(openAI)
	if len(got) != 2 {
		t.Fatalf("OpenAI 形态应原样保留 2 条, got %d: %s", len(got), toj(t, got))
	}
	a := got[1].(map[string]any)
	if _, has := a["tool_calls"]; !has {
		t.Fatal("OpenAI 形态必须保留 tool_calls 字段")
	}
	if a["content"] != nil {
		t.Fatalf("OpenAI 形态的 content 不应被改写, got %v", a["content"])
	}

	// 混合形态(部分带 tool_calls): 同样跳过。
	mixed := []any{
		map[string]any{"role": "assistant", "content": []any{ftu("a1")}},
		map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{
			map[string]any{"id": "c9"},
		}},
	}
	if gotM := fixToolUseOrdering(mixed); len(gotM) != 2 {
		t.Fatalf("混合形态应原样保留 2 条, got %d", len(gotM))
	}

	// 纯 Claude 形态: 守卫放行, 纯变换生效(F15 场景)。
	claudeShape := []any{
		map[string]any{"role": "assistant", "content": []any{ftu("c1"), ftxt("tail")}},
		map[string]any{"role": "user", "content": []any{ftr("c1", "out")}},
	}
	gotC := fixToolUseOrdering(claudeShape)
	ac := gotC[0].(map[string]any)["content"].([]any)
	if len(ac) != 1 {
		t.Fatalf("Claude 形态应删掉 tool_use 之后的 text, got %d 块", len(ac))
	}
}

// TestFixToolUseOrdering_工具调用不被抹掉 —— 回归: 守卫存在时 tool_calls 恒定保留。
//
// 这是**接入级**缺口的最小复现: 若有人去掉形态守卫, 本测试变红。
func TestFixToolUseOrdering_工具调用不被抹掉(t *testing.T) {
	in := []any{
		map[string]any{"role": "user", "content": "go"},
		map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{
			map[string]any{"id": "c1", "function": map[string]any{"name": "R"}},
		}},
		map[string]any{"role": "tool", "tool_call_id": "c1", "content": "res"},
	}
	got := fixToolUseOrdering(in)
	// 逐条核对: 结构与内容都不变(包括 tool_calls 与 role:"tool" 条目)。
	if len(got) != 3 {
		t.Fatalf("长度应为 3, got %d: %s", len(got), toj(t, got))
	}
	for i, raw := range got {
		m := raw.(map[string]any)
		orig := in[i].(map[string]any)
		if m["role"] != orig["role"] {
			t.Errorf("第 %d 条 role 变了: %v -> %v", i, orig["role"], m["role"])
		}
		if _, has := orig["tool_calls"]; has {
			if _, still := m["tool_calls"]; !still {
				t.Errorf("第 %d 条的 tool_calls 被抹掉了", i)
			}
		}
	}
}
