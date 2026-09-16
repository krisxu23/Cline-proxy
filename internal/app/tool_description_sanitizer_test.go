package app

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// 本文件锁定 sanitizeToolDescription / sanitizeToolDescriptions 的行为。
//
// 全部期望值来自**实跑** OmniRoute 参考实现
// (探针 .negbak/probe_tooldesc.mjs, 逐字搬运 schemaCoercion.ts:77-81, 226-254, 444-447)。

func dj(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal 失败: %v", err)
	}
	return string(b)
}

// TestSanitizeToolDescription_OpenAI形态 对应探针 D1/D2/D4。
func TestSanitizeToolDescription_OpenAI形态(t *testing.T) {
	t.Run("null转空串", func(t *testing.T) {
		out := sanitizeToolDescription(map[string]any{
			"type":     "function",
			"function": map[string]any{"name": "t", "description": nil, "parameters": map[string]any{}},
		})
		// 权威值 (D1): {"function":{"description":"","name":"t","parameters":{}},"type":"function"}
		fn := out.(map[string]any)["function"].(map[string]any)
		if got := fn["description"]; got != "" {
			t.Fatalf("description = %#v, 期望空串", got)
		}
		// ★ 必须是**空串**, 不是"删掉键"
		if _, has := fn["description"]; !has {
			t.Fatal("description 键不应被删除(参考实现是归一成空串)")
		}
	})

	t.Run("数字转字符串", func(t *testing.T) {
		out := sanitizeToolDescription(map[string]any{
			"type":     "function",
			"function": map[string]any{"name": "t", "description": float64(42), "parameters": map[string]any{}},
		})
		// 权威值 (D2): "...description":"42"...
		fn := out.(map[string]any)["function"].(map[string]any)
		if got := fn["description"]; got != "42" {
			t.Fatalf("description = %#v, 期望 \"42\"", got)
		}
	})

	t.Run("合法字符串不变", func(t *testing.T) {
		out := sanitizeToolDescription(map[string]any{
			"type":     "function",
			"function": map[string]any{"name": "t", "description": "A useful tool", "parameters": map[string]any{}},
		})
		// 权威值 (D4)
		fn := out.(map[string]any)["function"].(map[string]any)
		if got := fn["description"]; got != "A useful tool" {
			t.Fatalf("description = %#v, 期望原值", got)
		}
	})
}

// TestSanitizeToolDescription_Anthropic形态 对应探针 D3。
func TestSanitizeToolDescription_Anthropic形态(t *testing.T) {
	out := sanitizeToolDescription(map[string]any{
		"name": "t", "description": nil, "input_schema": map[string]any{},
	})
	// 权威值 (D3): {"description":"","input_schema":{},"name":"t"}
	if got := out.(map[string]any)["description"]; got != "" {
		t.Fatalf("description = %#v, 期望空串", got)
	}
}

// TestSanitizeToolDescription_键不存在时不动 对应探针 D6。
func TestSanitizeToolDescription_键不存在时不动(t *testing.T) {
	out := sanitizeToolDescription(map[string]any{
		"name": "t", "input_schema": map[string]any{},
	})
	// 权威值 (D6): {"input_schema":{},"name":"t"}, has description = false
	if _, has := out.(map[string]any)["description"]; has {
		t.Fatal("键不存在时不应新增 description")
	}
	if got := dj(t, out); got != `{"input_schema":{},"name":"t"}` {
		t.Fatalf("输出 = %s", got)
	}
}

// TestSanitizeToolDescription_顶层与function独立判定 对应探针 D7。
//
// ★ 反直觉点: `{function:{...description:1}, description:null}` 的顶层
// `description: null` **保持原样**。因为第二块的守卫 `!isPlainObject(result.function)`
// 为 false。照抄成 else-if 或"顶层总是处理"都会偏离。
func TestSanitizeToolDescription_顶层与function独立判定(t *testing.T) {
	in := map[string]any{
		"function":    map[string]any{"name": "t", "description": float64(1)},
		"description": nil,
	}
	out := sanitizeToolDescription(in).(map[string]any)
	// 权威值 (D7): {"function":{"description":"1","name":"t"},"description":null}
	fn := out["function"].(map[string]any)
	if got := fn["description"]; got != "1" {
		t.Fatalf("function.description = %#v, 期望 \"1\"", got)
	}
	if got := out["description"]; got != nil {
		t.Fatalf("顶层 description = %#v, 期望保持 null(不被处理)", got)
	}
}

// TestSanitizeToolDescription_functionDeclarations 对应探针 D8。
func TestSanitizeToolDescription_functionDeclarations(t *testing.T) {
	out := sanitizeToolDescription(map[string]any{
		"functionDeclarations": []any{
			map[string]any{"name": "a", "description": nil},
			map[string]any{"name": "b", "description": float64(7)},
			map[string]any{"name": "c"},
			nil,
			"str",
		},
	}).(map[string]any)

	// 权威值 (D8):
	// [{"name":"a","description":""},{"name":"b","description":"7"},{"name":"c"},null,"str"]
	decls := out["functionDeclarations"].([]any)
	if len(decls) != 5 {
		t.Fatalf("declarations 数量 = %d, 期望 5", len(decls))
	}
	if got := decls[0].(map[string]any)["description"]; got != "" {
		t.Fatalf("a.description = %#v, 期望空串", got)
	}
	if got := decls[1].(map[string]any)["description"]; got != "7" {
		t.Fatalf("b.description = %#v, 期望 \"7\"", got)
	}
	if _, has := decls[2].(map[string]any)["description"]; has {
		t.Fatal("c 无 description 键, 不应新增")
	}
	if decls[3] != nil {
		t.Fatalf("null 条目应原样保留, got %#v", decls[3])
	}
	if decls[4] != "str" {
		t.Fatalf("字符串条目应原样保留, got %#v", decls[4])
	}
}

// TestSanitizeToolDescriptions_数组与非数组 对应探针 D5/D9/D13/D14。
func TestSanitizeToolDescriptions_数组与非数组(t *testing.T) {
	t.Run("批量处理", func(t *testing.T) {
		out := sanitizeToolDescriptions([]any{
			map[string]any{"name": "t1", "description": nil, "input_schema": map[string]any{}},
			map[string]any{"type": "function",
				"function": map[string]any{"name": "t2", "description": float64(42), "parameters": map[string]any{}}},
		}).([]any)
		// 权威值 (D5)
		if got := out[0].(map[string]any)["description"]; got != "" {
			t.Fatalf("t1.description = %#v", got)
		}
		if got := out[1].(map[string]any)["function"].(map[string]any)["description"]; got != "42" {
			t.Fatalf("t2.description = %#v", got)
		}
	})

	t.Run("非数组原样返回", func(t *testing.T) {
		// 权威值 (D9): null -> null, "x" -> "x"
		if got := sanitizeToolDescriptions(nil); got != nil {
			t.Fatalf("nil 应原样返回, got %#v", got)
		}
		if got := sanitizeToolDescriptions("x"); got != "x" {
			t.Fatalf("字符串应原样返回, got %#v", got)
		}
	})

	t.Run("空数组返回空数组", func(t *testing.T) {
		// 权威值 (D13): []
		out := sanitizeToolDescriptions([]any{})
		if out == nil {
			t.Fatal("应返回空切片而非 nil")
		}
		if len(out.([]any)) != 0 {
			t.Fatalf("应返回空数组, got %#v", out)
		}
	})

	t.Run("含畸形条目", func(t *testing.T) {
		// 权威值 (D14): [null,5,"x",{"description":"","name":"ok"}]
		out := sanitizeToolDescriptions([]any{
			nil, float64(5), "x",
			map[string]any{"name": "ok", "description": nil},
		}).([]any)
		if len(out) != 4 {
			t.Fatalf("数量 = %d, 期望 4", len(out))
		}
		if out[0] != nil || out[1] != float64(5) || out[2] != "x" {
			t.Fatalf("畸形条目应原样透传, got %s", dj(t, out))
		}
		if got := out[3].(map[string]any)["description"]; got != "" {
			t.Fatalf("ok.description = %#v, 期望空串", got)
		}
	})
}

// TestSanitizeToolDescription_非对象原样返回 对应探针 D10。
func TestSanitizeToolDescription_非对象原样返回(t *testing.T) {
	// 权威值 (D10): null -> null, 5 -> 5, [1,2] -> [1,2]
	if got := sanitizeToolDescription(nil); got != nil {
		t.Fatalf("nil -> %#v", got)
	}
	if got := sanitizeToolDescription(float64(5)); got != float64(5) {
		t.Fatalf("5 -> %#v", got)
	}
	arr := []any{float64(1), float64(2)}
	got := sanitizeToolDescription(arr)
	if dj(t, got) != "[1,2]" {
		t.Fatalf("数组 -> %s", dj(t, got))
	}
}

// TestSanitizeDescriptionValue_JS字符串转换 对应探针 D11。
func TestSanitizeDescriptionValue_JS字符串转换(t *testing.T) {
	cases := []struct {
		in   any
		want string
		ok   bool
	}{
		// 权威值 (D11)
		{true, "true", true},
		{false, "false", true},
		{float64(0), "0", true},
		{float64(0.5), "0.5", true},
		{[]any{float64(1), float64(2)}, "1,2", true},
		{map[string]any{"a": float64(1)}, "[object Object]", true},
		{"", "", true},
		{nil, "", false}, // JS: value === undefined -> undefined
	}
	for _, c := range cases {
		got, ok := sanitizeDescriptionValue(c.in)
		if ok != c.ok {
			t.Fatalf("sanitizeDescriptionValue(%#v) ok = %v, 期望 %v", c.in, ok, c.ok)
		}
		if got != c.want {
			t.Fatalf("sanitizeDescriptionValue(%#v) = %q, 期望 %q", c.in, got, c.want)
		}
	}
}

// TestSanitizeToolDescription_不就地改写入参 对应探针 D12。
func TestSanitizeToolDescription_不就地改写入参(t *testing.T) {
	fn := map[string]any{"name": "t", "description": nil, "parameters": map[string]any{}}
	tool := map[string]any{"type": "function", "function": fn}

	sanitizeToolDescription(tool)

	// 权威值 (D12): 入参 function.description 仍为 null
	if fn["description"] != nil {
		t.Fatalf("入参被就地改写: %#v", fn["description"])
	}
	if tool["function"].(map[string]any)["description"] != nil {
		t.Fatal("入参 tool.function 被就地改写")
	}
	if _, has := tool["description"]; has {
		t.Fatal("入参被新增了 description 键")
	}
}

// TestSanitizeDescriptionValue_null与缺键的区别 —— 锁定 nil 的判定。
func TestSanitizeDescriptionValue_null与缺键的区别(t *testing.T) {
	// 显式 null -> 空串 (参考实现 :79)
	got, ok := sanitizeDescriptionValueJS(nil)
	if !ok || got != "" {
		t.Fatalf("显式 null 应得 (\"\", true), got (%q, %v)", got, ok)
	}
	// 缺键由调用方用 `_, has := m[k]` 提前排除 —— 见
	// TestSanitizeToolDescription_键不存在时不动。
}

// TestSanitizeToolDescriptions接线_源码层锁死 用源码文本断言防止接线脱节。
//
// 参考实现的调用点是 translator/index.ts:606-607 与 :628-629, 两处都在
// `result.tools` 的守卫之下、**没有** targetFormat / provider 前置条件 ——
// 故我方必须无条件跑, 不能塞进 `if cfg.APIType == "anthropic"` 里。
func TestSanitizeToolDescriptions接线_源码层锁死(t *testing.T) {
	src, err := os.ReadFile("providers_chat.go")
	if err != nil {
		t.Fatalf("读 providers_chat.go 失败: %v", err)
	}
	s := string(src)

	if !strings.Contains(s, "params[\"tools\"] = sanitizeToolDescriptions(tools)") {
		t.Fatal("接线缺失: sanitizeToolDescriptions 未接入 providers_chat.go")
	}

	// 必须**在** anthropic 分支之外 —— 参考实现不带格式限定。
	idxCall := strings.Index(s, "params[\"tools\"] = sanitizeToolDescriptions(tools)")
	idxAnthropic := strings.Index(s, `if cfg.APIType == "anthropic" {`)
	if idxAnthropic >= 0 && idxCall > idxAnthropic {
		t.Fatal("接线位置错误: sanitizeToolDescriptions 被放进了 anthropic 分支内, " +
			"参考实现在所有格式上无条件调用(translator/index.ts:606/:628)")
	}
}
