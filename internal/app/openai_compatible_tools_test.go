package app

import (
	"encoding/json"
	"reflect"
	"testing"
)

// 本文件是 openai_compatible_tools.go 的权威行为规格。
// 所有期望值均由 Node 实跑 OmniRoute 原实现确认 (见交付说明的 probe 输出),
// 不靠读 TS 推测。凡"看着像 bug"的期望都在注释里标明出处。

func normJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// --- normalizeOpenAICompatibleTools ---

func TestNormTools_扁平形态无type时原样透传(t *testing.T) {
	// Node: 1 flat: {"tools":[{"name":"get_weather",...}],"dropped":0}
	//
	// ★ 反直觉: 该条目**没有** type, 按 `if (!tool.type ...) return tool;` 直接
	// 原样返回, 并**不会**被包成 {type:"function",function:{...}}。
	// 参考实现的归一**只处理"有 type 但非 function"的条目**,
	// 无 type 的走"已是目标形态"分支。
	in := []any{map[string]any{
		"name":         "get_weather",
		"description":  "d",
		"input_schema": map[string]any{"type": "object"},
	}}
	got := normalizeOpenAICompatibleTools(in, "openai")
	if got.dropped != 0 {
		t.Fatalf("dropped = %d, 期望 0", got.dropped)
	}
	want := `[{"description":"d","input_schema":{"type":"object"},"name":"get_weather"}]`
	if s := normJSON(t, got.tools); s != want {
		t.Fatalf("tools = %s, 期望 %s", s, want)
	}
}

func TestNormTools_无type无function无name被丢弃(t *testing.T) {
	// Node: 2 junk-drop: {"tools":[],"dropped":1}
	in := []any{map[string]any{"type": "web_search_20250305"}}
	got := normalizeOpenAICompatibleTools(in, "openai")
	if got.dropped != 1 || len(got.tools) != 0 {
		t.Fatalf("dropped = %d len = %d, 期望 1 / 0", got.dropped, len(got.tools))
	}
}

func TestNormTools_name为空串时被保留(t *testing.T) {
	// Node: 3 empty-name: {"tools":[{"name":""}],"dropped":0}
	//
	// ★ 反直觉: `!!tool.name` 对空串为 false, 但 `!tool.type` 对该条目为 **true**
	// (它没有 type), filter 的四个条件是 **||** 并联 —— 只要有一个真就保留。
	// 所以空串 name 条目能活下来。
	in := []any{map[string]any{"name": ""}}
	got := normalizeOpenAICompatibleTools(in, "openai")
	if got.dropped != 0 || len(got.tools) != 1 {
		t.Fatalf("dropped = %d len = %d, 期望 0 / 1", got.dropped, len(got.tools))
	}
}

func TestNormTools_parameters为null时回落到input_schema(t *testing.T) {
	// Node: 4 null-params: {"function":{"name":"x","parameters":{"a":1}}}
	in := []any{map[string]any{
		"type": "custom_t", "name": "x",
		"parameters": nil, "input_schema": map[string]any{"a": float64(1)},
	}}
	got := normalizeOpenAICompatibleTools(in, "openai")
	want := `[{"function":{"name":"x","parameters":{"a":1}},"type":"function"}]`
	if s := normJSON(t, got.tools); s != want {
		t.Fatalf("tools = %s, 期望 %s", s, want)
	}
}

func TestNormTools_两个参数键都不存在时parameters键不出现(t *testing.T) {
	// Node: 5 no-params: {"function":{"name":"x"}} —— 注意没有 parameters 键
	in := []any{map[string]any{"type": "custom_t", "name": "x"}}
	got := normalizeOpenAICompatibleTools(in, "openai")
	want := `[{"function":{"name":"x"},"type":"function"}]`
	if s := normJSON(t, got.tools); s != want {
		t.Fatalf("tools = %s, 期望 %s", s, want)
	}
}

func TestNormTools_strict为null时键仍出现(t *testing.T) {
	// Node: 6 null-strict: {"function":{"name":"x","strict":null}}
	//
	// ★ 反直觉最重要的两条之一: `strict === undefined` 判定**只排 undefined**,
	// null 不算 undefined → 键出现且值为 null。
	// 这与 parameters 的处理**不对称**: 后者用的是 `!== undefined` 的**或**条件
	// 配 `??` 回落, 而 strict 是"只要不是 undefined 就带上"。
	in := []any{map[string]any{"type": "custom_t", "name": "x", "strict": nil}}
	got := normalizeOpenAICompatibleTools(in, "openai")
	want := `[{"function":{"name":"x","strict":null},"type":"function"}]`
	if s := normJSON(t, got.tools); s != want {
		t.Fatalf("tools = %s, 期望 %s", s, want)
	}
}

func TestNormTools_description为null时键仍出现(t *testing.T) {
	// Node: 7 null-desc: {"function":{"name":"x","description":null}}
	// 与 strict 同理: `description === undefined` 只排 undefined, null 保留。
	in := []any{map[string]any{"type": "custom_t", "name": "x", "description": nil}}
	got := normalizeOpenAICompatibleTools(in, "openai")
	want := `[{"function":{"description":null,"name":"x"},"type":"function"}]`
	if s := normJSON(t, got.tools); s != want {
		t.Fatalf("tools = %s, 期望 %s", s, want)
	}
}

func TestNormTools_responses源格式原样返回(t *testing.T) {
	// Node: 8 responses: {"tools":[{"type":"web_search"}],"dropped":0}
	in := []any{map[string]any{"type": "web_search"}}
	got := normalizeOpenAICompatibleTools(in, "openai-responses")
	if got.dropped != 0 || len(got.tools) != 1 {
		t.Fatalf("dropped = %d len = %d, 期望 0 / 1", got.dropped, len(got.tools))
	}
	if got.tools[0].(map[string]any)["type"] != "web_search" {
		t.Fatal("responses 源格式的工具定义不应被归一")
	}
}

func TestNormTools_type为function时原样透传(t *testing.T) {
	in := []any{map[string]any{
		"type":     "function",
		"function": map[string]any{"name": "f"},
	}}
	got := normalizeOpenAICompatibleTools(in, "openai")
	want := `[{"function":{"name":"f"},"type":"function"}]`
	if s := normJSON(t, got.tools); s != want {
		t.Fatalf("tools = %s, 期望 %s", s, want)
	}
}

func TestNormTools_非对象条目原样透传(t *testing.T) {
	// Go 侧对非对象条目的处理是"原样透传"。
	// 参考实现在此路径会因属性访问 null 而抛错(调用方保证为对象),
	// 我方选择不 panic 的等价安全形态 —— 已在实现注释声明。
	in := []any{"str", float64(42)}
	got := normalizeOpenAICompatibleTools(in, "openai")
	if len(got.tools) != 2 || got.dropped != 0 {
		t.Fatalf("len = %d dropped = %d, 期望 2 / 0", len(got.tools), got.dropped)
	}
}

func TestNormTools_空输入(t *testing.T) {
	got := normalizeOpenAICompatibleTools(nil, "openai")
	if len(got.tools) != 0 || got.dropped != 0 {
		t.Fatalf("len = %d dropped = %d, 期望 0 / 0", len(got.tools), got.dropped)
	}
}

// --- defaultClaudeToolType ---

func TestClaudeToolType_缺type补custom(t *testing.T) {
	// Node: C1 no-type: [{"type":"custom","name":"x"}]
	// 注意键序: JS `{type:"custom", ...tool}` → type 在前。Go map 无序, 故只比内容。
	got := defaultClaudeToolType([]any{map[string]any{"name": "x"}})
	arr, ok := got.([]any)
	if !ok || len(arr) != 1 {
		t.Fatalf("结果类型/长度异常: %#v", got)
	}
	rec := arr[0].(map[string]any)
	if rec["type"] != "custom" || rec["name"] != "x" {
		t.Fatalf("= %#v, 期望 type=custom name=x", rec)
	}
}

func TestClaudeToolType_已有type不动(t *testing.T) {
	// Node: C2 has-type: [{"type":"bash_20241022","name":"x"}]
	in := map[string]any{"type": "bash_20241022", "name": "x"}
	got := defaultClaudeToolType([]any{in})
	rec := got.([]any)[0].(map[string]any)
	if rec["type"] != "bash_20241022" {
		t.Fatalf("type = %v, 期望 bash_20241022", rec["type"])
	}
}

func TestClaudeToolType_空串type不补(t *testing.T) {
	// Node: C3 empty-type: [{"type":"","name":"x"}]
	//
	// ★ 反直觉: JS 的 `tool.type ?` 是真值判定 —— 空串为 **falsy**,
	// 所以空串 type **会被当成"缺 type"补成 "custom"**? 不对 ——
	// Node 实测输出是 {"type":"","name":"x"}, 即**保留了空串**。
	// 原因: `tool.type ? tool : {type:"custom",...tool}` 的展开是
	// `{type:"custom", ...tool}` —— tool 自带的 `type:""` **覆盖**了前面的 "custom"!
	// 所以终态是 type:"", 与"没补"看起来一样, 但机制是"补了又被覆盖"。
	in := map[string]any{"type": "", "name": "x"}
	got := defaultClaudeToolType([]any{in})
	rec := got.([]any)[0].(map[string]any)
	if rec["type"] != "" {
		t.Fatalf("type = %q, 期望空串", rec["type"])
	}
}

func TestClaudeToolType_null条目原样透传(t *testing.T) {
	// Node: C4 null-entry: [null]
	got := defaultClaudeToolType([]any{nil})
	arr := got.([]any)
	if len(arr) != 1 || arr[0] != nil {
		t.Fatalf("= %#v, 期望 [nil]", got)
	}
}

func TestClaudeToolType_基本类型原样透传(t *testing.T) {
	// Node: C5 prim: ["str",42]
	in := []any{"str", float64(42)}
	got := defaultClaudeToolType(in)
	if !reflect.DeepEqual(got, in) {
		t.Fatalf("= %#v, 期望与输入一致", got)
	}
}

func TestClaudeToolType_非数组原样返回(t *testing.T) {
	// Node: C6 notarray: "nope"
	if got := defaultClaudeToolType("nope"); got != "nope" {
		t.Fatalf("= %#v, 期望原样", got)
	}
	if got := defaultClaudeToolType(nil); got != nil {
		t.Fatalf("nil 输入 = %#v, 期望 nil", got)
	}
}

func TestClaudeToolType_不修改入参(t *testing.T) {
	// 参考实现用新对象, 调用方原始 tool 对象不被 mutate。
	orig := map[string]any{"name": "x"}
	_ = defaultClaudeToolType([]any{orig})
	if _, exists := orig["type"]; exists {
		t.Fatal("原始 tool 对象不应被注入 type 键")
	}
}
