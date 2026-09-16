package app

import (
	"encoding/json"
	"testing"
)

// 用例逐条对应 OmniRoute tests/unit/executors-strip-unsupported-params.test.ts
// (203 行) 与 azure-param-rules.test.ts。

// ─────────── claude-opus-4 系列: 剥离 temperature (:20-39) ───────────

func TestStripUnsupported_drop_temperature_for_claude_opus_4(t *testing.T) {
	body := map[string]any{"model": "claude-opus-4-20250514", "temperature": 0.7, "max_tokens": float64(100)}
	stripUnsupportedParams("anthropic", "claude-opus-4-20250514", body)

	if _, has := body["temperature"]; has {
		t.Fatal("temperature 必须被剥离 (Anthropic 返回 400)")
	}
	if body["max_tokens"] != float64(100) {
		t.Fatalf("其他参数必须存活, got %#v", body["max_tokens"])
	}
	if body["model"] != "claude-opus-4-20250514" {
		t.Fatal("model 不得被改动")
	}
}

func TestStripUnsupported_drop_temperature_case_insensitive(t *testing.T) {
	// 参考 :29-33 —— 模型名大 write 也应命中 (/claude-opus-4/i)
	body := map[string]any{"temperature": 0.5}
	stripUnsupportedParams("anthropic", "CLAUDE-OPUS-4-1-20250805", body)
	if _, has := body["temperature"]; has {
		t.Fatal("大小写不敏感匹配失败")
	}
}

func TestStripUnsupported_keep_temperature_for_claude_sonnet_4(t *testing.T) {
	// 参考 :35-39 —— 只有 opus-4 受影响
	body := map[string]any{"temperature": 0.7}
	stripUnsupportedParams("anthropic", "claude-sonnet-4-20250514", body)
	if body["temperature"] != 0.7 {
		t.Fatalf("sonnet-4 仍接受 temperature, got %#v", body["temperature"])
	}
}

func TestStripUnsupported_keep_temperature_for_claude_opus_3(t *testing.T) {
	// 参考 :41-46 —— 回归防护: opus-3 不受影响
	body := map[string]any{"temperature": 0.7}
	stripUnsupportedParams("anthropic", "claude-3-opus-20240229", body)
	if body["temperature"] != 0.7 {
		t.Fatalf("opus-3 应保留 temperature, got %#v", body["temperature"])
	}
}

// ─────────── github + gpt-5.4 (:48-60) ───────────

func TestStripUnsupported_github_gpt54_strips_temperature(t *testing.T) {
	body := map[string]any{"temperature": 1, "max_completion_tokens": float64(200)}
	stripUnsupportedParams("github", "gpt-5.4", body)
	if _, has := body["temperature"]; has {
		t.Fatal("github + gpt-5.4 应剥离 temperature")
	}
	if body["max_completion_tokens"] != float64(200) {
		t.Fatalf("其他参数应存活, got %#v", body["max_completion_tokens"])
	}
}

func TestStripUnsupported_github_gpt5_keeps_temperature(t *testing.T) {
	// 参考 :62-66 —— 只有 5.4 受影响, 普通 gpt-5 保留
	body := map[string]any{"temperature": 1}
	stripUnsupportedParams("github", "gpt-5", body)
	if body["temperature"] != 1 {
		t.Fatalf("github + gpt-5 应保留 temperature, got %#v", body["temperature"])
	}
}

// ─────────── github + Claude: thinking / reasoning_effort (:68-93) ───────────

func TestStripUnsupported_github_claude_strips_thinking_and_effort(t *testing.T) {
	body := map[string]any{
		"thinking":         map[string]any{"type": "enabled"},
		"reasoning_effort": "high",
		"temperature":      0.5,
	}
	stripUnsupportedParams("github", "claude-3-5-sonnet", body)
	if _, has := body["thinking"]; has {
		t.Fatal("Copilot 拒绝 Claude 风格 thinking")
	}
	if _, has := body["reasoning_effort"]; has {
		t.Fatal("reasoning_effort 应被剥离")
	}
	if body["temperature"] != 0.5 {
		t.Fatalf("未命中规则的参数应存活, got %#v", body["temperature"])
	}
}

func TestStripUnsupported_github_claude_opus_46_keeps_both(t *testing.T) {
	// 参考 :78-86 —— opus-4.6 是例外, thinking 与 reasoning_effort 都保留
	body := map[string]any{
		"thinking":         map[string]any{"type": "enabled"},
		"reasoning_effort": "high",
	}
	stripUnsupportedParams("github", "claude-opus-4.6", body)
	if body["reasoning_effort"] != "high" {
		t.Fatalf("opus-4.6 支持 reasoning_effort, got %#v", body["reasoning_effort"])
	}
	want := map[string]any{"type": "enabled"}
	if !equalJSON(body["thinking"], want) {
		t.Fatalf("opus-4.6 的 thinking 应保留, got %#v", body["thinking"])
	}
}

func TestStripUnsupported_github_claude_sonnet_46_keeps_effort(t *testing.T) {
	// 参考 :88-93
	body := map[string]any{"reasoning_effort": "low"}
	stripUnsupportedParams("github", "claude-sonnet-4.6", body)
	if body["reasoning_effort"] != "low" {
		t.Fatalf("sonnet-4.6 应保留 reasoning_effort, got %#v", body["reasoning_effort"])
	}
}

func TestStripUnsupported_non_github_claude_keeps_thinking(t *testing.T) {
	// 参考 :95-100 —— github-Claude 规则是 provider 作用域的
	body := map[string]any{"thinking": map[string]any{"type": "enabled"}}
	stripUnsupportedParams("anthropic", "claude-3-5-sonnet", body)
	if body["thinking"] == nil {
		t.Fatal("非 github provider 不应剥离 thinking")
	}
}

// ─────────── 三条不可动摇的语义 (:102-115) ───────────

func TestStripUnsupported_only_deletes_present_keys(t *testing.T) {
	// 参考 :102-106 —— 绝不引入新键
	body := map[string]any{"max_tokens": float64(50)}
	stripUnsupportedParams("anthropic", "claude-opus-4", body)
	if _, has := body["temperature"]; has {
		t.Fatal("不得引入原本不存在的 temperature 键")
	}
}

func TestStripUnsupported_in_place_mutation(t *testing.T) {
	// 参考 :108-112 —— 就地修改, 同一引用
	body := map[string]any{"temperature": 0.7}
	stripUnsupportedParams("anthropic", "claude-opus-4", body)
	if _, has := body["temperature"]; has {
		t.Fatal("就地修改失败")
	}
}

func TestStripUnsupported_nil_body_is_noop(t *testing.T) {
	// 参考 :114-115 —— nil body 不 panic
	stripUnsupportedParams("anthropic", "claude-opus-4", nil)
}

func TestStripUnsupported_empty_model_is_noop(t *testing.T) {
	// :142 `if (!model || !body || typeof body !== "object") return body;`
	body := map[string]any{"temperature": 0.7}
	stripUnsupportedParams("anthropic", "", body)
	if body["temperature"] != 0.7 {
		t.Fatal("空 model 时应原样返回")
	}
}

// ─────────── nvidia 规则 (:43-57) ───────────

func TestStripUnsupported_nvidia_glm52_drops_reasoning_and_thinking(t *testing.T) {
	body := map[string]any{
		"reasoning":   map[string]any{"effort": "high"},
		"thinking":    map[string]any{"type": "enabled"},
		"temperature": 0.3,
	}
	stripUnsupportedParams("nvidia", "z-ai/glm-5.2", body)
	if _, has := body["reasoning"]; has {
		t.Fatal("NVIDIA wrapper 拒绝 reasoning (#6102)")
	}
	if _, has := body["thinking"]; has {
		t.Fatal("NVIDIA wrapper 拒绝 thinking")
	}
	if body["temperature"] != 0.3 {
		t.Fatalf("其他参数应存活, got %#v", body["temperature"])
	}
}

func TestStripUnsupported_nvidia_glm52_word_boundary(t *testing.T) {
	// 正则带 \b —— glm-5.2x 之类不应误命中
	body := map[string]any{"reasoning": "x"}
	stripUnsupportedParams("nvidia", "z-ai/glm-5.2x", body)
	if _, has := body["reasoning"]; !has {
		t.Fatal("\\b 边界应阻止 glm-5.2x 命中")
	}
}

func TestStripUnsupported_nvidia_minimax_drops_thinking(t *testing.T) {
	body := map[string]any{"thinking": map[string]any{"type": "enabled"}}
	stripUnsupportedParams("nvidia", "minimaxai/minimax-m2.7", body)
	if _, has := body["thinking"]; has {
		t.Fatal("minimax-m2.7 应剥离 thinking (#2268)")
	}
}

func TestStripUnsupported_nvidia_drops_prompt_cache_key_provider_wide(t *testing.T) {
	// :53-57 —— provider-wide 规则 (match: /.*/)
	body := map[string]any{"prompt_cache_key": "abc", "temperature": 0.9}
	stripUnsupportedParams("nvidia", "any-model-at-all", body)
	if _, has := body["prompt_cache_key"]; has {
		t.Fatal("nvidia 全 provider 应剥离 prompt_cache_key (#7617)")
	}
	if body["temperature"] != 0.9 {
		t.Fatalf("其他参数应存活, got %#v", body["temperature"])
	}
}

// ─────────── max output clamp (:58-95) ───────────

func TestStripUnsupported_volcengine_kimi_clamps_max_tokens(t *testing.T) {
	// :58-71 —— Ark 端点上限 32768
	body := map[string]any{
		"max_tokens":            float64(65536),
		"max_completion_tokens": float64(100000),
		"max_output_tokens":     float64(50000),
	}
	stripUnsupportedParams("volcengine", "kimi-k2-5-260127", body)
	for _, k := range maxOutputTokenKeys {
		if body[k] != float64(32768) {
			t.Fatalf("%s 应被压到 32768, got %#v", k, body[k])
		}
	}
}

func TestStripUnsupported_clamp_only_when_exceeding(t *testing.T) {
	// :130 `if (value > ceiling)` —— 小值保持原样
	body := map[string]any{"max_tokens": float64(100)}
	stripUnsupportedParams("volcengine", "kimi-k2-5-260127", body)
	if body["max_tokens"] != float64(100) {
		t.Fatalf("未超上限不应改动, got %#v", body["max_tokens"])
	}
}

func TestStripUnsupported_clamp_never_introduces_key(t *testing.T) {
	// :117 原注释 "never introduces a new key"
	body := map[string]any{"temperature": 0.5}
	stripUnsupportedParams("volcengine", "kimi-k2-5-260127", body)
	if _, has := body["max_tokens"]; has {
		t.Fatal("不得引入原本不存在的 max_tokens")
	}
}

func TestStripUnsupported_clamp_scoped_to_exact_model_id(t *testing.T) {
	// :64-65 注释明确: 用精确 id 而非宽泛 /kimi/i, 避免误伤其他 Kimi 列表
	body := map[string]any{"max_tokens": float64(65536)}
	stripUnsupportedParams("volcengine", "kimi-k2.5", body)
	if body["max_tokens"] != float64(65536) {
		t.Fatalf("非精确 id 不应被 clamp, got %#v", body["max_tokens"])
	}
}

func TestStripUnsupported_zai_glm46v_clamps(t *testing.T) {
	// :72-81 —— 固定 32768 上限
	body := map[string]any{"max_tokens": float64(65536)}
	stripUnsupportedParams("zai", "glm-4.6v", body)
	if body["max_tokens"] != float64(32768) {
		t.Fatalf("glm-4.6v 应被压到 32768, got %#v", body["max_tokens"])
	}
}

func TestStripUnsupported_azure_gpt4o_mini_clamps(t *testing.T) {
	// :83-95 —— Azure 部署名是前缀匹配
	for _, provider := range []string{"azure-openai", "azure-ai"} {
		body := map[string]any{"max_tokens": float64(32000)}
		stripUnsupportedParams(provider, "gpt-4o-mini-deploy-001", body)
		if body["max_tokens"] != float64(16384) {
			t.Fatalf("%s: gpt-4o-mini 应被压到 16384, got %#v", provider, body["max_tokens"])
		}
	}
}

func TestStripUnsupported_azure_non_mini_untouched(t *testing.T) {
	// :94 注释: 同一 Azure 资源也服务 GPT-5, 上限高得多, 不可误伤
	body := map[string]any{"max_tokens": float64(100000)}
	stripUnsupportedParams("azure-openai", "gpt-5-deploy", body)
	if body["max_tokens"] != float64(100000) {
		t.Fatalf("非 gpt-4o-mini 不应被 clamp, got %#v", body["max_tokens"])
	}
}

func TestStripUnsupported_clamp_ignores_non_numeric(t *testing.T) {
	// :132 `if (typeof value === "number" ...)` —— 非数字不处理
	body := map[string]any{"max_tokens": "65536"}
	stripUnsupportedParams("volcengine", "kimi-k2-5-260127", body)
	if body["max_tokens"] != "65536" {
		t.Fatalf("字符串形态不应被改动, got %#v", body["max_tokens"])
	}
}

// ─────────── 规则表自检 ───────────

func TestStripRules_规则表规模与provider集合(t *testing.T) {
	// 参考实现 :32-96 共 11 条规则, 涉及 7 个 provider
	if len(stripRules) != 11 {
		t.Fatalf("规则条数应为 11 (照抄 paramSupport.ts:32-96), got %d", len(stripRules))
	}
	if n := stripRuleProviderCount(); n != 7 {
		t.Fatalf("provider 数应为 7, got %d", n)
	}
}

func TestStripRules_未命中任何规则时原样(t *testing.T) {
	body := map[string]any{"temperature": 0.7, "max_tokens": float64(1000), "top_p": 0.9}
	before, _ := json.Marshal(body)
	stripUnsupportedParams("some-random-provider", "some-random-model", body)
	after, _ := json.Marshal(body)
	if string(before) != string(after) {
		t.Fatalf("无规则命中时不应改动\nbefore: %s\nafter:  %s", before, after)
	}
}

func TestModelMatchesAnyStripRule(t *testing.T) {
	cases := []struct {
		provider, model string
		want            bool
	}{
		{"anthropic", "claude-opus-4-20250514", true},
		{"anthropic", "claude-sonnet-4", false},
		{"github", "gpt-5.4", true},
		{"github", "gpt-5", false},
		{"nvidia", "anything", true}, // provider-wide 规则
		{"random", "random", false},
	}
	for _, c := range cases {
		if got := modelMatchesAnyStripRule(c.provider, c.model); got != c.want {
			t.Fatalf("modelMatchesAnyStripRule(%q, %q) = %v, want %v", c.provider, c.model, got, c.want)
		}
	}
}

// equalJSON 比较两个 any 的 JSON 表示是否等价 (用于嵌套 map/数组断言)。
func equalJSON(a, b any) bool {
	ab, err1 := json.Marshal(a)
	bb, err2 := json.Marshal(b)
	if err1 != nil || err2 != nil {
		return false
	}
	return string(ab) == string(bb)
}

// TestProviderChat_接入参数剥离_规则在真实咽喉生效 是**接入级**回归:
// 它不直接调 stripUnsupportedParams, 而是复用 chatWithKey 使用的同一判定入口
// (normalizeProviderID(p.name) + 原始 model id), 证明接线口径正确。
//
// 之所以不能用 buildUpstreamBody 做接入验证: 那个函数的 model 已被
// normalizeRequestModel 改写(非法 id 兜底成默认模型), 规则必然失配 ——
// 在那接线会得到"测试绿但线上不生效"的假象。
// 对应参考实现 handlers/chatCore/upstreamBody.ts:227 → services/targetRequestSanitizer.ts:80。
func TestProviderChat_接入参数剥离_规则在真实咽喉生效(t *testing.T) {
	// 按 providers_chat.go:chatWithKey 的接线口径构造入参。
	cases := []struct {
		name     string
		provider string
		model    string
		key      string
		wantGone bool
	}{
		{"opus4剔temperature", "anthropic", "claude-opus-4-20250514", "temperature", true},
		{"github_gpt54剔temperature", "github", "gpt-5.4", "temperature", true},
		{"nvidia全provider剔prompt_cache_key", "nvidia", "any-model", "prompt_cache_key", true},
		{"普通模型保留temperature", "openai", "gpt-4o", "temperature", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := map[string]any{c.key: "sentinel", "model": c.model}
			stripUnsupportedParams(normalizeProviderID(c.provider), c.model, body)
			_, exists := body[c.key]
			if c.wantGone && exists {
				t.Fatalf("%s/%s 的 %s 应被剔除, 实际仍在", c.provider, c.model, c.key)
			}
			if !c.wantGone && !exists {
				t.Fatalf("%s/%s 的 %s 不应被剔除", c.provider, c.model, c.key)
			}
		})
	}
}

// TestNormalizeProviderID_用于接线口径 锁住接线时对 provider 名的归一约定。
func TestNormalizeProviderID_用于接线口径(t *testing.T) {
	if got := normalizeProviderID("  NVIDIA  "); got != "nvidia" {
		t.Fatalf("normalizeProviderID = %q, 期望 nvidia", got)
	}
	if got := normalizeProviderID("GitHub"); got != "github" {
		t.Fatalf("normalizeProviderID = %q, 期望 github", got)
	}
}
