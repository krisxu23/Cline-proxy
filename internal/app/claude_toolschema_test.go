package app

import (
	"reflect"
	"testing"
)

// 权威值来自 .negbak/probe_toolschema.mjs 逐字搬运参考实现
// (translator/request/claude-to-openai.ts) 后 Node 探针跑出的输出，
// 见 memory/2026-09-17.md 与 docs/omniroute-mapping.md 第 32 项。
//
// 反直觉项已逐个加注释锁定，禁止"顺手修正"：
//   - P1g: 空对象 {} 原样返回（不是 fallback）。
//   - P1j: type=="object" 且 properties==null → 补成 {}。
//   - P1n: type=="" → 原样返回，**不**补 properties（type 不是 "object"）。
//   - P2h: "  "（纯空格）lower 后仍是 "  "，非空 → 原样返回。
//   - P3p: output_config.effort=="" → 归 undefined → fall through 到 thinking 分支。

func TestNormalizeToolSchema(t *testing.T) {
	fallback := map[string]any{"type": "object", "properties": map[string]any{}}

	cases := []struct {
		name string
		in   any
		want any
	}{
		// P1a: undefined / JSON null → fallback
		//
		// JSON 的 null 经 json.Unmarshal 成 any 是 **untyped nil interface**，
		// jsTruthy 命中 case nil → false → fallback。
		{"P1a nil", nil, fallback},
		// P1c: 数组 → fallback（JS 的 Array.isArray 分支）
		{"P1c array", []any{"a", "b"}, fallback},
		// P1d: 空数组 → fallback
		{"P1d empty array", []any{}, fallback},
		// P1e: 字符串 → fallback（typeof !== "object"）
		{"P1e string", "hello", fallback},
		// P1f: 数字 → fallback
		{"P1f number", float64(42), fallback},
		// P1g: 空对象 {} → 原样返回（type 缺失，非 "object"）
		{"P1g empty object", map[string]any{}, map[string]any{}},
		// P1h: type=="object" 无 properties → 补成 {}
		{"P1h object no properties", map[string]any{"type": "object"}, fallback},
		// P1i: type=="object" 有 properties → 原样
		{"P1i object with properties", map[string]any{"type": "object", "properties": map[string]any{"a": float64(1)}}, map[string]any{"type": "object", "properties": map[string]any{"a": float64(1)}}},
		// P1j: properties==null → 视为 falsy → 补成 {}
		{"P1j properties null", map[string]any{"type": "object", "properties": nil}, fallback},
		// P1k: type=="string" → 原样
		{"P1k string type", map[string]any{"type": "string"}, map[string]any{"type": "string"}},
		// P1l: type=="array" 无 items → 原样（只 type=="object" 才补）
		{"P1l array type", map[string]any{"type": "array"}, map[string]any{"type": "array"}},
		// P1m: 带额外键 → 保留 extra 键 + 补 properties
		{"P1m extra keys", map[string]any{"type": "object", "additionalProperties": false}, map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{}}},
		// P1n: type=="" → 原样返回（非 "object"，不补 properties）
		// ★ 反直觉：properties 是 nil 也不会被补，因为判的是 type=="object"。
		{"P1n empty type", map[string]any{"type": "", "properties": nil}, map[string]any{"type": "", "properties": nil}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeToolSchema(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("normalizeToolSchema(%#v) = %#v, want %#v", tc.in, got, tc.want)
			}
		})
	}
}

// 浅拷贝纪律：P1h/P1j/P1m 的输出必须是**新 map**，不是入参本身。
// 否则客户端复用同一份请求体做重试时，我们改出的 properties 键会残留。
//
// Go map 不可比较，用"改一个看另一个是否跟着变"的行为断言判是否同一底层对象。
func TestNormalizeToolSchema_ShallowCopy(t *testing.T) {
	in := map[string]any{"type": "object"}
	outAny := normalizeToolSchema(in)
	out, ok := outAny.(map[string]any)
	if !ok {
		t.Fatalf("期望 map，得到 %#v", outAny)
	}
	if _, exists := in["properties"]; exists {
		t.Fatal("normalizeToolSchema 原地改写了入参，违反浅拷贝纪律")
	}
	if p, ok := out["properties"].(map[string]any); !ok || len(p) != 0 {
		t.Fatalf("补出的 properties 应为空 map，得到 %#v", out["properties"])
	}
	// 行为断言：改 out 的 type 键，in 不受影响 → 证明是浅拷贝的新 map。
	out["type"] = "MUTATED"
	if in["type"] != "object" {
		t.Fatal("改输出 map 影响了入参，说明不是浅拷贝")
	}

	// P1i：不补的路径返回入参本身（参考实现也是 return s）。
	// 用相同的行为断言反过来验证：改返回值会影响入参。
	same := map[string]any{"type": "object", "properties": map[string]any{"a": float64(1)}}
	gotAny := normalizeToolSchema(same)
	gotMap, ok := gotAny.(map[string]any)
	if !ok {
		t.Fatalf("期望 map，得到 %#v", gotAny)
	}
	gotMap["type"] = "MUTATED"
	if same["type"] != "MUTATED" {
		t.Fatal("不需要补 properties 时应该原样返回入参（改返回值应影响入参）")
	}
}

func TestNormalizeOpenAIReasoningEffort(t *testing.T) {
	// 参考实现返回 undefined，Go 侧用 "" 表达。
	cases := []struct {
		name string
		in   any
		want string
	}{
		{"P2a nil", nil, ""},
		{"P2b explicit nil map", map[string]any(nil), ""},
		{"P2c number", float64(3), ""},
		{"P2d bool", true, ""},
		{"P2e upper", "HIGH", "high"},
		{"P2f mixed", "XHigh", "xhigh"},
		{"P2g empty", "", ""},
		// ★ 反直觉：纯空格 lower 后仍是 "  "，非空 → 原样返回。
		{"P2h spaces", "  ", "  "},
		{"P2i xhigh", "xhigh", "xhigh"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeOpenAIReasoningEffort(tc.in)
			if got != tc.want {
				t.Fatalf("normalizeOpenAIReasoningEffort(%#v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestOpenAIReasoningEffort(t *testing.T) {
	mkBody := func(thinking, outputConfig map[string]any) map[string]any {
		b := map[string]any{}
		if thinking != nil {
			b["thinking"] = thinking
		}
		if outputConfig != nil {
			b["output_config"] = outputConfig
		}
		return b
	}

	cases := []struct {
		name string
		body map[string]any
		want string
	}{
		// P3a: output_config.effort 严格优先于 thinking.budget_tokens
		{"P3a output_config priority", mkBody(
			map[string]any{"type": "enabled", "budget_tokens": float64(2048)},
			map[string]any{"effort": "high"},
		), "high"},

		// P3b/P3c: budget <= 0 → 不设
		{"P3b budget 0", mkBody(map[string]any{"type": "enabled", "budget_tokens": float64(0)}, nil), ""},
		{"P3c budget negative", mkBody(map[string]any{"type": "enabled", "budget_tokens": float64(-5)}, nil), ""},

		// P3d: budget == 1024 → low（含边界）
		{"P3d budget 1024 → low", mkBody(map[string]any{"type": "enabled", "budget_tokens": float64(1024)}, nil), "low"},
		// P3e/P3f: 1025..10240 → medium
		{"P3e budget 1025 → medium", mkBody(map[string]any{"type": "enabled", "budget_tokens": float64(1025)}, nil), "medium"},
		{"P3f budget 10240 → medium", mkBody(map[string]any{"type": "enabled", "budget_tokens": float64(10240)}, nil), "medium"},
		// P3g/P3h: 10241..131071 → high（< 131072）
		{"P3g budget 10241 → high", mkBody(map[string]any{"type": "enabled", "budget_tokens": float64(10241)}, nil), "high"},
		{"P3h budget 131071 → high", mkBody(map[string]any{"type": "enabled", "budget_tokens": float64(131071)}, nil), "high"},
		// P3i/P3j: >= 131072 → xhigh
		{"P3i budget 131072 → xhigh", mkBody(map[string]any{"type": "enabled", "budget_tokens": float64(131072)}, nil), "xhigh"},
		{"P3j budget 500000 → xhigh", mkBody(map[string]any{"type": "enabled", "budget_tokens": float64(500000)}, nil), "xhigh"},

		// P3k: thinking.type != "enabled" → 不设
		{"P3k type disabled", mkBody(map[string]any{"type": "disabled", "budget_tokens": float64(100000)}, nil), ""},

		// P3l: enabled 但无 budget_tokens → 不设
		{"P3l enabled no budget", mkBody(map[string]any{"type": "enabled"}, nil), ""},

		// P3m: budget_tokens 是字符串（非 number）→ 不设
		{"P3m budget string", mkBody(map[string]any{"type": "enabled", "budget_tokens": "100000"}, nil), ""},

		// P3n: 无 thinking 字段 → 不设
		{"P3n no thinking", map[string]any{}, ""},

		// P3o: output_config 存在但无 effort → 不设（且 thinking 也没有）
		{"P3o output_config no effort", mkBody(nil, map[string]any{"foo": "bar"}), ""},

		// ★ P3p 反直觉：output_config.effort == "" → 归 undefined → fall through
		// 到 thinking 分支，budget 2048 → medium。
		// 不是"output_config 存在就屏蔽 thinking"，而是"有**有效** effort 才屏蔽"。
		{"P3p output_config empty effort fallthrough", mkBody(
			map[string]any{"type": "enabled", "budget_tokens": float64(2048)},
			map[string]any{"effort": ""},
		), "medium"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := openAIReasoningEffort(tc.body)
			if got != tc.want {
				t.Fatalf("openAIReasoningEffort(%#v) = %q, want %q", tc.body, got, tc.want)
			}
		})
	}
}
