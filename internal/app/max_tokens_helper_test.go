package app

import (
	"math"
	"testing"
)

// 本文件是 max_tokens_helper.go 的权威行为规格。
// 所有期望值均由 Node 实跑 OmniRoute 原实现确认（probe 输出见交付说明），
// 逐条对应 maxTokensHelper.ts:8-21 + constants.ts:151/:154。

func assertMax(t *testing.T, body map[string]any, want float64) {
	t.Helper()
	got := adjustMaxTokens(body)
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("adjustMaxTokens(%v) = %v, 期望 %v", body, got, want)
	}
}

func TestAdjustMax_无任何字段用默认(t *testing.T) {
	// probe 1: {} → 64000
	assertMax(t, map[string]any{}, 64000)
}

func TestAdjustMax_无工具时不提升(t *testing.T) {
	// probe 2: {max_tokens:1000} → 1000
	assertMax(t, map[string]any{"max_tokens": float64(1000)}, 1000)
}

func TestAdjustMax_有工具且低于下限时提升(t *testing.T) {
	// probe 3: {max_tokens:1000, tools:[...]} → 32000
	// 这正是"防止工具调用参数被截断"的核心行为。
	assertMax(t, map[string]any{
		"max_tokens": float64(1000),
		"tools":      []any{map[string]any{"type": "function"}},
	}, 32000)
}

func TestAdjustMax_零值走默认而非下限(t *testing.T) {
	// probe 4 / 5: max_tokens=0 → 64000（有工具无工具都一样）
	//
	// ★ 反直觉且最易写错: `requestedMaxTokens || DEFAULT_MAX_TOKENS` 是 **`||`**
	// 真值判定, 0 被判假 → 回落到 64000。**在提升判断之前**就已经变成 64000,
	// 而 64000 >= 32000 所以不再提升。
	// 若误写成 `??` 语义(0 保留), 结果会是 0→提升到 32000, 与参考实现不符。
	assertMax(t, map[string]any{"max_tokens": float64(0), "tools": []any{map[string]any{}}}, 64000)
	assertMax(t, map[string]any{"max_tokens": float64(0)}, 64000)
}

func TestAdjustMax_只用max_completion_tokens(t *testing.T) {
	// probe 6: {max_completion_tokens:500} → 500
	assertMax(t, map[string]any{"max_completion_tokens": float64(500)}, 500)
}

func TestAdjustMax_max_tokens为null时回落到max_completion_tokens(t *testing.T) {
	// probe 7: {max_tokens:null, max_completion_tokens:800} → 800
	//
	// ★ 这是 `??` 那一层: null 才回落。与 0 的行为不同(0 不回落, 但会在
	// 下一层 `||` 被判假)。
	assertMax(t, map[string]any{
		"max_tokens":            nil,
		"max_completion_tokens": float64(800),
	}, 800)
}

func TestAdjustMax_max_tokens为null且无备选(t *testing.T) {
	// probe 8: {max_tokens:null} → 64000
	assertMax(t, map[string]any{"max_tokens": nil}, 64000)
}

func TestAdjustMax_空工具数组不提升(t *testing.T) {
	// probe 9: tools:[] → 不触发（`body.tools.length > 0` 为假）
	assertMax(t, map[string]any{"max_tokens": float64(1000), "tools": []any{}}, 1000)
}

func TestAdjustMax_工具非数组不提升(t *testing.T) {
	// probe 10 / 11: tools 为字符串或 null → 不触发
	assertMax(t, map[string]any{"max_tokens": float64(1000), "tools": "x"}, 1000)
	assertMax(t, map[string]any{"max_tokens": float64(1000), "tools": nil}, 1000)
}

func TestAdjustMax_高于下限时不动(t *testing.T) {
	// probe 12: 100000 → 100000（只抬不压）
	assertMax(t, map[string]any{"max_tokens": float64(100000), "tools": []any{map[string]any{}}}, 100000)
}

func TestAdjustMax_负值兜底为1(t *testing.T) {
	// probe 13: -5 → 1（`Math.max(1, maxTokens)`）
	assertMax(t, map[string]any{"max_tokens": float64(-5)}, 1)
}

func TestAdjustMax_恰好等于下限不提升(t *testing.T) {
	// probe 14: 32000 → 32000（`maxTokens < DEFAULT_MIN_TOKENS` 才提升）
	assertMax(t, map[string]any{"max_tokens": float64(32000), "tools": []any{map[string]any{}}}, 32000)
}

func TestAdjustMax_差一即提升(t *testing.T) {
	// probe 15: 31999 → 32000（边界另一侧）
	assertMax(t, map[string]any{"max_tokens": float64(31999), "tools": []any{map[string]any{}}}, 32000)
}

func TestAdjustMax_纯数字字符串按数值处理(t *testing.T) {
	// probe 16: {max_tokens:"1000"} → 1000
	//
	// ★ 这是 JS 隐式转换的真实可达语义（IDE/SDK 会发字符串数字）,
	// 已实测确认, 故 Go 侧必须复刻而不是保守回落。
	assertMax(t, map[string]any{"max_tokens": "1000"}, 1000)
}

func TestAdjustMax_非数字字符串保守回落(t *testing.T) {
	// JS 侧 `Math.max(1, "abc")` 会得 NaN; 该畸形输入不生产可用请求,
	// 我方保守归为"非数值"→ 走默认值。属已声明差异。
	assertMax(t, map[string]any{"max_tokens": "abc"}, 64000)
	assertMax(t, map[string]any{"max_tokens": ""}, 64000)
}

func TestAdjustMax_不修改入参(t *testing.T) {
	// 参考实现只 return 数值, 不写回 body。调用方负责赋值。
	body := map[string]any{"max_tokens": float64(1000), "tools": []any{map[string]any{}}}
	_ = adjustMaxTokens(body)
	if body["max_tokens"] != float64(1000) {
		t.Fatalf("不应写回 body, 实际 max_tokens = %v", body["max_tokens"])
	}
}
