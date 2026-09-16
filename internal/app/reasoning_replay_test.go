package app

import (
	"encoding/json"
	"testing"
)

// 本文件锁定 requiresReasoningReplay 与 injectEmptyReasoningContentForToolCalls。
//
// 全部期望值来自**实跑** OmniRoute 参考实现
// (探针 .negbak/probe_reasoning_replay.mjs, 逐字搬运 reasoningCache.ts:26-121
// 与 schemaCoercion.ts:455-483)。

func rj(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal 失败: %v", err)
	}
	return string(b)
}

// rr 是 requiresReasoningReplay 的调用简写(默认 thinkingEnabled=true, 走 fallback)。
func rr(provider, model string) bool {
	return requiresReasoningReplay(provider, model, "", true, true)
}

// TestRequiresReasoningReplay_显式信号优先 对应探针 RR1。
//
// ★ 反直觉点: `interleavedField = "reasoning_details"` 会**否定** provider 白名单
// 的命中 —— 显式信号在一切判定之前, 且两个方向的信号都是"短路返回"。
func TestRequiresReasoningReplay_显式信号优先(t *testing.T) {
	// 权威值 (RR1): reasoning_content -> true
	if !requiresReasoningReplay("anything", "whatever", "reasoning_content", true, true) {
		t.Fatal("interleavedField=reasoning_content 应恒为 true")
	}
	// 权威值 (RR1): reasoning_details -> false, 即便 deepseek 在白名单里
	if requiresReasoningReplay("deepseek", "deepseek-v4-flash", "reasoning_details", true, true) {
		t.Fatal("interleavedField=reasoning_details 应恒为 false(覆盖 provider 命中)")
	}
}

// TestRequiresReasoningReplay_K3正则 对应探针 RR2。
func TestRequiresReasoningReplay_K3正则(t *testing.T) {
	cases := map[string]bool{
		// 权威值 (RR2)
		"k3":        true,
		"kimi-k3":   true,
		"foo/k3":    true,
		"k3-mini":   true,
		"kimi-k3.1": true,
		"k30":       false, // `k3(?:$|-)` 要求 k3 后是结尾或连字符
		"k3x":       false,
	}
	for m, want := range cases {
		if got := rr("x", m); got != want {
			t.Fatalf("requiresReasoningReplay(x, %q) = %v, 期望 %v", m, got, want)
		}
	}
}

// TestRequiresReasoningReplay_原生K27 对应探针 RR3。
func TestRequiresReasoningReplay_原生K27(t *testing.T) {
	// 权威值 (RR3)
	if !rr("moonshot", "kimi-k2.7-code") {
		t.Fatal("moonshot + kimi-k2.7-code 应为 true")
	}
	if !rr("kimi", "kimi-k2.7-code") {
		t.Fatal("kimi + kimi-k2.7-code 应为 true")
	}
	// ★ 反直觉点: provider 不匹配时**仍为 true**, 但走的是 fallback 正则
	//   `kimi[-/]k\d`(命中 "kimi-k2"), 不是 NATIVE_K27 分支。
	if !rr("openai", "kimi-k2.7-code") {
		t.Fatal("openai + kimi-k2.7-code 经 fallback 正则应为 true")
	}
}

// TestRequiresReasoningReplay_DeepSeek反向契约 对应探针 RR4/RR5。
//
// ★ 反直觉点: `deepseek-reasoner` / `deepseek-r1` 是**反向**契约 —— 明确 false,
// 即便 provider 是 deepseek(在白名单里)。这条短路发生在 fallback 之前。
func TestRequiresReasoningReplay_DeepSeek反向契约(t *testing.T) {
	// 权威值 (RR4)
	for _, m := range []string{"deepseek-reasoner", "deepseek-r1", "deepseek-r1-0528"} {
		if rr("deepseek", m) {
			t.Fatalf("deepseek + %q 应为 false(旧 reasoner 反向契约)", m)
		}
	}
	// deepseek-chat / deepseek-v4-flash 走 fallback 正则 -> true
	for _, m := range []string{"deepseek-chat", "deepseek-v4-flash"} {
		if !rr("deepseek", m) {
			t.Fatalf("deepseek + %q 应为 true", m)
		}
	}
}

// TestRequiresReasoningReplay_DeepSeekV4需thinking 对应探针 RR5。
//
// ★ 关键细节: 单看 `isDeepSeekReasoningModel` 在 thinkingEnabled=false 时返回
// false, 但**整体函数仍返回 true** —— 因为 fallback 的模型正则
// `deepseek[-/]v4[-.](flash|pro)` 同样命中。两条路径都要照抄, 不能只留一条。
func TestRequiresReasoningReplay_DeepSeekV4需thinking(t *testing.T) {
	// 权威值 (RR5)
	if !requiresReasoningReplay("openai", "deepseek-v4-flash", "", true, true) {
		t.Fatal("thinkingEnabled=true 应为 true")
	}
	if !requiresReasoningReplay("openai", "deepseek-v4-flash", "", false, true) {
		t.Fatal("thinkingEnabled=false 仍应为 true(fallback 正则命中)")
	}
	if !requiresReasoningReplay("openai", "deepseek-v4-flash", "", true, false) {
		t.Fatal("allowLegacyFallback=false 时 isDeepSeekReasoningModel 分支应为 true")
	}

	// 单测 isDeepSeekReasoningModel 自身的 thinking 门槛
	if isDeepSeekReasoningModel("deepseek-v4-flash", false) {
		t.Fatal("isDeepSeekReasoningModel 在 thinkingEnabled=false 时应为 false")
	}
	if !isDeepSeekReasoningModel("deepseek-v4-flash", true) {
		t.Fatal("isDeepSeekReasoningModel 在 thinkingEnabled=true 时应为 true")
	}
	// 注意 deepseek-v4-flash-free 也命中(模式里 `(-free)?` 可选)
	if !isDeepSeekReasoningModel("deepseek-v4-flash-free", true) {
		t.Fatal("deepseek-v4-flash-free 应命中")
	}
}

// TestRequiresReasoningReplay_provider白名单 对应探针 RR6。
func TestRequiresReasoningReplay_provider白名单(t *testing.T) {
	// 权威值 (RR6)
	for _, p := range []string{"deepseek", "opencode-go", "siliconflow", "xiaomi-mimo"} {
		if !rr(p, "some-random-model") {
			t.Fatalf("provider %q 在白名单里, 应为 true", p)
		}
	}
	for _, p := range []string{"openai", "anthropic"} {
		if rr(p, "some-random-model") {
			t.Fatalf("provider %q 不在白名单, 应为 false", p)
		}
	}
}

// TestRequiresReasoningReplay_模型正则白名单 对应探针 RR7。
func TestRequiresReasoningReplay_模型正则白名单(t *testing.T) {
	cases := map[string]bool{
		// 权威值 (RR7)
		"qwq-32b":     true,
		"qwen3-think": true,
		"glm-4-think": true,
		"mimo-v2":     true,
		"mimo.v1":     true,
		"kimi-k2":     true,
		"kimi/k3":     true,
		"kimi-latest": false, // 参考实现注释显式排除的泛化别名
	}
	for m, want := range cases {
		if got := rr("openai", m); got != want {
			t.Fatalf("requiresReasoningReplay(openai, %q) = %v, 期望 %v", m, got, want)
		}
	}
}

// TestRequiresReasoningReplay_关掉fallback 对应探针 RR8。
func TestRequiresReasoningReplay_关掉fallback(t *testing.T) {
	// 权威值 (RR8): K3 仍 true(在 fallback 之前), 白名单 provider 变 false
	if !requiresReasoningReplay("x", "k3", "", true, false) {
		t.Fatal("allowLegacyFallback=false 时 K3 仍应为 true")
	}
	if requiresReasoningReplay("deepseek", "random", "", true, false) {
		t.Fatal("allowLegacyFallback=false 时 provider 白名单应被跳过")
	}
}

// TestInjectEmptyReasoning_不需要回放时同引用 对应探针 IR1。
func TestInjectEmptyReasoning_不需要回放时同引用(t *testing.T) {
	msgs := []any{map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "c1"}}}}
	out, changed := injectEmptyReasoningContentForToolCalls(msgs, "openai", "gpt-4")
	// 权威值 (IR1): 同一引用 = true
	if changed {
		t.Fatal("不需要回放时不应报告改动")
	}
	if rj(t, out) != rj(t, msgs) {
		t.Fatalf("应原样返回, got %s", rj(t, out))
	}
}

// TestInjectEmptyReasoning_非数组原样返回 对应探针 IR2。
func TestInjectEmptyReasoning_非数组原样返回(t *testing.T) {
	// 权威值 (IR2): null -> null, "x" -> "x"
	if out, _ := injectEmptyReasoningContentForToolCalls(nil, "deepseek", "deepseek-v4-flash"); out != nil {
		t.Fatalf("nil -> %#v", out)
	}
	if out, _ := injectEmptyReasoningContentForToolCalls("x", "deepseek", "deepseek-v4-flash"); out != "x" {
		t.Fatalf(`"x" -> %#v`, out)
	}
}

// TestInjectEmptyReasoning_补空串 对应探针 IR3 —— 本模块的核心行为。
func TestInjectEmptyReasoning_补空串(t *testing.T) {
	msgs := []any{
		map[string]any{"role": "user", "content": "hi"},
		map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "c1"}}},
		map[string]any{"role": "tool", "content": "r"},
	}
	out, changed := injectEmptyReasoningContentForToolCalls(msgs, "deepseek", "deepseek-v4-flash")
	if !changed {
		t.Fatal("应报告已改动")
	}
	arr := out.([]any)
	if len(arr) != 3 {
		t.Fatalf("条数 = %d, 期望 3", len(arr))
	}
	// 权威值 (IR3): 第二条补 reasoning_content:""
	second := arr[1].(map[string]any)
	v, has := second["reasoning_content"]
	if !has {
		t.Fatal("第二条应新增 reasoning_content 键")
	}
	// ★ 必须是**空串**
	if v != "" {
		t.Fatalf("reasoning_content = %#v, 期望空串", v)
	}
	// 其它两条不得被加上该键
	for _, i := range []int{0, 2} {
		if _, has := arr[i].(map[string]any)["reasoning_content"]; has {
			t.Fatalf("第 %d 条不应被加 reasoning_content", i)
		}
	}
}

// TestInjectEmptyReasoning_已有字段不动 对应探针 IR4。
//
// ★ 反直觉点: 判定是 `message.reasoning_content !== undefined` ——
//
//	**空串、纯空白、显式 null 都算"已存在"**, 一律原样保留(连引用都不换)。
func TestInjectEmptyReasoning_已有字段不动(t *testing.T) {
	for _, v := range []any{"", "   ", "real reasoning", nil} {
		m := map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "c1"}}, "reasoning_content": v}
		msgs := []any{m}
		out, changed := injectEmptyReasoningContentForToolCalls(msgs, "deepseek", "deepseek-v4-flash")
		if changed {
			t.Fatalf("已有 reasoning_content=%#v 时不应报告改动", v)
		}
		got := out.([]any)[0].(map[string]any)["reasoning_content"]
		// 权威值 (IR4): 原值原样保留
		if rj(t, got) != rj(t, v) {
			t.Fatalf("reasoning_content 被改写: %#v -> %#v", v, got)
		}
	}
}

// TestInjectEmptyReasoning_不该补的情形 对应探针 IR5。
func TestInjectEmptyReasoning_不该补的情形(t *testing.T) {
	msgs := []any{
		map[string]any{"role": "assistant", "content": "plain"},
		map[string]any{"role": "assistant", "tool_calls": []any{}},
		map[string]any{"role": "user", "tool_calls": []any{map[string]any{"id": "c1"}}},
		map[string]any{"role": "assistant", "tool_calls": "not-an-array"},
	}
	out, changed := injectEmptyReasoningContentForToolCalls(msgs, "deepseek", "deepseek-v4-flash")
	if changed {
		t.Fatal("四种情形都不应改动")
	}
	// 权威值 (IR5): 全部无 reasoning_content 键
	for i, raw := range out.([]any) {
		if _, has := raw.(map[string]any)["reasoning_content"]; has {
			t.Fatalf("第 %d 条不应被加 reasoning_content", i)
		}
	}
}

// TestInjectEmptyReasoning_非对象条目透传 对应探针 IR6。
func TestInjectEmptyReasoning_非对象条目透传(t *testing.T) {
	msgs := []any{
		nil, float64(5), "x",
		map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "c1"}}},
	}
	out, changed := injectEmptyReasoningContentForToolCalls(msgs, "deepseek", "deepseek-v4-flash")
	if !changed {
		t.Fatal("最后一条应被补, 故应报告改动")
	}
	arr := out.([]any)
	if len(arr) != 4 {
		t.Fatalf("条数 = %d, 期望 4", len(arr))
	}
	// 权威值 (IR6): [null,5,"x",{...,"reasoning_content":""}]
	if arr[0] != nil || arr[1] != float64(5) || arr[2] != "x" {
		t.Fatalf("非对象条目应原样透传, got %s", rj(t, arr))
	}
	if got := arr[3].(map[string]any)["reasoning_content"]; got != "" {
		t.Fatalf("第 4 条 reasoning_content = %#v, 期望空串", got)
	}
}

// TestInjectEmptyReasoning_只判tool_calls非空 对应探针 IR7。
//
// ★ 反直觉点: 只校验 `tool_calls` 是**非空数组**, 不校验元素内容 ——
//
//	`[null, 1]` 也会触发补空串。
func TestInjectEmptyReasoning_只判tool_calls非空(t *testing.T) {
	msgs := []any{map[string]any{"role": "assistant", "tool_calls": []any{nil, float64(1)}}}
	out, changed := injectEmptyReasoningContentForToolCalls(msgs, "deepseek", "deepseek-v4-flash")
	if !changed {
		t.Fatal("[null,1] 是非空数组, 应触发补空串")
	}
	// 权威值 (IR7): [{"role":"assistant","tool_calls":[null,1],"reasoning_content":""}]
	if got := out.([]any)[0].(map[string]any)["reasoning_content"]; got != "" {
		t.Fatalf("reasoning_content = %#v, 期望空串", got)
	}
}

// TestInjectEmptyReasoning_不就地改写入参 对应探针 IR8。
func TestInjectEmptyReasoning_不就地改写入参(t *testing.T) {
	m := map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "c1"}}}
	msgs := []any{m}
	if _, changed := injectEmptyReasoningContentForToolCalls(msgs, "deepseek", "deepseek-v4-flash"); !changed {
		t.Fatal("应报告改动")
	}
	// 权威值 (IR8): 入参被污染 = false
	if _, has := m["reasoning_content"]; has {
		t.Fatal("入参消息被就地改写")
	}
}

// TestInjectEmptyReasoning_null提供者 对应探针 IR9。
func TestInjectEmptyReasoning_null提供者(t *testing.T) {
	// 权威值 (IR9): String(null ?? "") = "" -> 不匹配任何白名单 -> false
	msgs := []any{map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "c1"}}}}
	out, changed := injectEmptyReasoningContentForToolCalls(msgs, nil, nil)
	if changed {
		t.Fatal("provider/model 为 nil 时不应改动")
	}
	if rj(t, out) != rj(t, msgs) {
		t.Fatalf("应原样返回, got %s", rj(t, out))
	}
}

// TestInjectEmptyReasoning_幂等 —— 跑两遍结果一致。
func TestInjectEmptyReasoning_幂等(t *testing.T) {
	msgs := []any{
		map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "c1"}}},
	}
	once, _ := injectEmptyReasoningContentForToolCalls(msgs, "deepseek", "deepseek-v4-flash")
	twice, changed := injectEmptyReasoningContentForToolCalls(once, "deepseek", "deepseek-v4-flash")
	if changed {
		t.Fatal("第二遍不应再改动")
	}
	if rj(t, once) != rj(t, twice) {
		t.Fatalf("非幂等: once=%s twice=%s", rj(t, once), rj(t, twice))
	}
}

// ──────────────── 接线级: hasThinkingConfig / applyEmptyReasoningReplay ────────────────
//
// 权威值来自探针 .negbak/probe_replay_apply.mjs (G1–G9), 该探针逐字搬运
// services/provider.ts:471-473 + reasoningCache.ts:83-121 +
// schemaCoercion.ts:455-487 + translator/index.ts:610-619 的 gate。

// TestHasThinkingConfig_JS真值语义 对应探针 G6。
//
// ★ 反直觉点（三条，都是 JS 语义）:
//  1. `!!body.reasoning_effort` 是 **JS truthy**, 不是"键存在"。空串 / 0 /
//     false / null 全部为假 —— 客户端显式传 `reasoning_effort: ""` 不算开了 thinking。
//  2. `body.thinking?.type === "enabled"` 走**可选链**: thinking 是字符串
//     `"enabled"` 或 null 时都是 undefined, 判定为假; 必须**恰好**是对象且
//     type 严格等于小写 `"enabled"`。`"ENABLED"` 为假（大小写敏感）。
func TestHasThinkingConfig_JS真值语义(t *testing.T) {
	cases := []struct {
		name  string
		body  map[string]any
		truth bool
	}{
		{"空 body", map[string]any{}, false},
		{"effort=high", map[string]any{"reasoning_effort": "high"}, true},
		{"effort=low", map[string]any{"reasoning_effort": "low"}, true},
		{"effort=1", map[string]any{"reasoning_effort": float64(1)}, true},
		{"effort=空串", map[string]any{"reasoning_effort": ""}, false},
		{"effort=0", map[string]any{"reasoning_effort": float64(0)}, false},
		{"effort=false", map[string]any{"reasoning_effort": false}, false},
		{"effort=null", map[string]any{"reasoning_effort": nil}, false},
		{"thinking.type=enabled", map[string]any{"thinking": map[string]any{"type": "enabled"}}, true},
		{"thinking.type=ENABLED", map[string]any{"thinking": map[string]any{"type": "ENABLED"}}, false},
		{"thinking.type=disabled", map[string]any{"thinking": map[string]any{"type": "disabled"}}, false},
		{"thinking=null", map[string]any{"thinking": nil}, false},
		{"thinking=字符串enabled", map[string]any{"thinking": "enabled"}, false},
	}
	for _, c := range cases {
		if got := hasThinkingConfig(c.body); got != c.truth {
			t.Errorf("%s: hasThinkingConfig=%v, 期望 %v", c.name, got, c.truth)
		}
	}
}

// rrExplicit 是接线级 gate 的调用简写(allowLegacyFallback=false)。
func rrExplicit(provider, model string, thinking bool) bool {
	return requiresReasoningReplay(provider, model, "", thinking, false)
}

// TestApplyEmptyReasoningReplay_无thinking时注入 对应探针 G1。
//
// ★ 这是**用户实际报障的场景**: Codex 经 cc-switch 路由到 deepseek-v4-flash,
// 客户端不带 thinking 配置 -> 参考实现会补 reasoning_content 空串。
func TestApplyEmptyReasoningReplay_无thinking时注入(t *testing.T) {
	cases := []struct {
		provider, model string
		runs            bool
	}{
		// 白名单 provider: fallback 命中, 但 explicit 不命中 -> 注入
		{"deepseek", "deepseek-v4-flash", true},
		{"tokenrouter", "deepseek-v4-flash", true},
		{"xiaomi-mimo", "mimo-v2", true},
		{"mimo", "mimo-v2", true},
		{"siliconflow", "qwen3-think", true},
		// 都不命中 -> 不注入
		{"tokenrouter", "some-random-model", false},
		{"openrouter", "glm-5.3-flash", false},
		// 反向契约: deepseek 旧 reasoner 家族不回放
		{"deepseek", "deepseek-reasoner", false},
	}
	for _, c := range cases {
		msgs := replaySampleMsgs()
		got, changed := applyEmptyReasoningReplay(msgs, c.provider, c.model, map[string]any{})
		if changed != c.runs {
			t.Errorf("%s/%s: changed=%v, 期望 %v", c.provider, c.model, changed, c.runs)
			continue
		}
		if !c.runs {
			if rj(t, got) != rj(t, msgs) {
				t.Errorf("%s/%s: 不改动时应原样返回", c.provider, c.model)
			}
			continue
		}
		blocks := got.([]any)
		first := blocks[0].(map[string]any)
		if first["reasoning_content"] != "" {
			t.Errorf("%s/%s: 应补空串, got %v", c.provider, c.model, first["reasoning_content"])
		}
	}
}

// TestApplyEmptyReasoningReplay_显式thinking关掉占位通道 对应探针 G2/G3/G4。
//
// ★ 最核心的照抄点（也是本项最容易抄错的地方）:
//   - `deepseek/deepseek-v4-flash` 一旦客户端带上 reasoning_effort 或
//     thinking.type=enabled, `requiresExplicitReasoningReplay` 变 true,
//     占位通道**整体让位**给真实回放通道 -> 不再补空串。
//   - `xiaomi-mimo` 不在 explicit 集合里(白名单只在 fallback 分支生效),
//     所以即使带了 thinking 也**仍然**补空串。
//   - thinking.type=disabled 不等于"开了 thinking" -> 仍走占位通道。
func TestApplyEmptyReasoningReplay_显式thinking关掉占位通道(t *testing.T) {
	effort := map[string]any{"reasoning_effort": "high"}
	thinkOn := map[string]any{"thinking": map[string]any{"type": "enabled"}}
	thinkOff := map[string]any{"thinking": map[string]any{"type": "disabled"}}

	cases := []struct {
		name            string
		provider, model string
		body            map[string]any
		runs            bool
	}{
		{"deepseek+v4+effort", "deepseek", "deepseek-v4-flash", effort, false},
		{"tokenrouter+v4+effort", "tokenrouter", "deepseek-v4-flash", effort, false},
		{"deepseek+v4+thinking", "deepseek", "deepseek-v4-flash", thinkOn, false},
		{"tokenrouter+v4+thinking", "tokenrouter", "deepseek-v4-flash", thinkOn, false},
		{"xiaomi+effort 仍注入", "xiaomi-mimo", "mimo-v2", effort, true},
		{"xiaomi+thinking 仍注入", "xiaomi-mimo", "mimo-v2", thinkOn, true},
		{"siliconflow+effort 仍注入", "siliconflow", "qwen3-think", effort, true},
		{"deepseek+v4+disabled 仍注入", "deepseek", "deepseek-v4-flash", thinkOff, true},
	}
	for _, c := range cases {
		msgs := replaySampleMsgs()
		_, changed := applyEmptyReasoningReplay(msgs, c.provider, c.model, c.body)
		if changed != c.runs {
			t.Errorf("%s: changed=%v, 期望 %v", c.name, changed, c.runs)
		}
	}
}

// TestApplyEmptyReasoningReplay_非数组原样 对应探针 G7。
func TestApplyEmptyReasoningReplay_非数组原样(t *testing.T) {
	for _, v := range []any{"str", nil, float64(7), map[string]any{}} {
		got, changed := applyEmptyReasoningReplay(v, "deepseek", "deepseek-v4-flash", map[string]any{})
		if changed {
			t.Errorf("%T: 不应改动", v)
		}
		if rj(t, got) != rj(t, v) {
			t.Errorf("%T: 应原样返回", v)
		}
	}
}

// TestApplyEmptyReasoningReplay_锚定探针G8G9 比对整条输出的**语义**。
//
// 注意: 不能拿 JSON 文本做逐字节比对 —— Go 的 `encoding/json` 会把 map 的键
// 按字典序输出(`content`,`reasoning_content`,`role`,`tool_calls`), 而 Node 的
// `JSON.stringify` 保持插入顺序(`role`,`content`,`tool_calls`,`reasoning_content`)。
// 两者语义完全一致, 差异纯属序列化实现。故这里逐字段断言。
func TestApplyEmptyReasoningReplay_锚定探针G8G9(t *testing.T) {
	// G8: 无 thinking -> 注入, assistant 回合多出 reasoning_content: ""
	got, changed := applyEmptyReasoningReplay(replaySampleMsgs(), "deepseek", "deepseek-v4-flash", map[string]any{})
	if !changed {
		t.Fatal("G8: 应发生改动")
	}
	blocks := got.([]any)
	if len(blocks) != 2 {
		t.Fatalf("G8: 长度应为 2, got %d", len(blocks))
	}
	a := blocks[0].(map[string]any)
	if a["role"] != "assistant" || a["content"] != "" {
		t.Errorf("G8: 首条应仍是 assistant/空 content, got %v", a)
	}
	if v, has := a["reasoning_content"]; !has || v != "" {
		t.Errorf("G8: 应补 reasoning_content:\"\", got has=%v v=%v", has, v)
	}
	if n := len(a["tool_calls"].([]any)); n != 1 {
		t.Errorf("G8: tool_calls 应保持 1 项, got %d", n)
	}
	u := blocks[1].(map[string]any)
	if u["role"] != "user" || u["content"] != "go" {
		t.Errorf("G8: 次条应原样为 user/go, got %v", u)
	}
	if _, has := u["reasoning_content"]; has {
		t.Error("G8: user 回合不应被补字段")
	}

	// G9: 带 effort -> 不注入, assistant 回合**没有** reasoning_content 键
	got2, changed2 := applyEmptyReasoningReplay(replaySampleMsgs(), "deepseek", "deepseek-v4-flash",
		map[string]any{"reasoning_effort": "high"})
	if changed2 {
		t.Fatal("G9: 不应发生改动")
	}
	a2 := got2.([]any)[0].(map[string]any)
	if _, has := a2["reasoning_content"]; has {
		t.Error("G9: explicit 为真时不得出现 reasoning_content 键")
	}
}

// TestApplyEmptyReasoningReplay_gate不对称性 —— ★ 把"两个 gate 故意不同"这件事钉死。
//
// 断言: 存在输入使 inject 内部判定为真、而接线 gate(explicit)也为真 —— 正是
// 这种"内外不一致"让 deepseek+v4+thinking 场景必须**跳过**注入。
// 若有人把接线 gate 改成 allowLegacyFallback=true(即内外同参), 本测试立刻变红。
func TestApplyEmptyReasoningReplay_gate不对称性(t *testing.T) {
	const p, m = "deepseek", "deepseek-v4-flash"
	inner := requiresReasoningReplay(p, m, "", true, true) // inject 内部: fallback 开
	explicit := rrExplicit(p, m, false)                    // 接线 gate: fallback 关
	if !inner {
		t.Fatal("inject 内部判定应为 true")
	}
	if explicit {
		t.Fatal("explicit 判定应为 false (白名单只在 fallback 分支生效)")
	}
	// 而一旦开了 thinking, explicit 变 true, 注入必须停。
	if !rrExplicit(p, m, true) {
		t.Fatal("thinking 开启后 explicit 应为 true")
	}
	if _, changed := applyEmptyReasoningReplay(replaySampleMsgs(), p, m,
		map[string]any{"reasoning_effort": "high"}); changed {
		t.Fatal("explicit 为真时不得注入")
	}
}

// replaySampleMsgs 是探针里那份固定样例(assistant 带一个 tool_call + 一条 user)。
func replaySampleMsgs() []any {
	return []any{
		map[string]any{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{"id": "t1"}}},
		map[string]any{"role": "user", "content": "go"},
	}
}
