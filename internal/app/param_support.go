// Package app — 上游不支持的请求参数剥离。
//
// 逐字照抄 OmniRoute open-sse/translator/paramSupport.ts (251 行) 的
// 「硬编码规则 + max output clamp」部分。
//
// 原文件头注释逐字:
//
//	Strip request params a given provider/model rejects upstream (e.g. HTTP 400).
//	Config-driven: add a rule here instead of scattering `delete body.x` across
//	executors. Port from 9router#7ae9fff6 (fixes upstream #1748).
//
//	Rule semantics:
//	  - `provider` (optional) limits the rule to a single provider id.
//	  - `match` is a RegExp tested against the model id OR a predicate.
//	  - `drop` is the list of param keys to remove when the rule fires.
//	  - `clampToModelMaxOutput` clamps max_tokens/max_completion_tokens/max_output_tokens
//	    down to the model's catalog `maxOutputTokens` ceiling.
//	  - `maxOutputCap` clamps the same keys down to a fixed endpoint-imposed ceiling.
//
//	A param is removed only when it is present (!== undefined). The helper never
//	introduces new keys and never throws on null/undefined bodies.
//
// ⚠ 与参考实现的已知差异 (取数通道适配, 非判定逻辑差异):
// 参考实现的 **Phase 2** 是 config-driven 规则, 从 DB 读 ProviderParamFilter
// (getParamFilterConfig, lib/db/paramFilters)。我方网关没有这张表, 因此
// Phase 2 不实现 —— 只实现 Phase 1(硬编码规则)与 max output clamp。
// 这是**功能子集**, 不是行为偏离: Phase 1 的判定与参考实现逐条一致。
package app

import (
	"regexp"
	"strings"
)

// maxOutputTokenKeys 照抄 paramSupport.ts:30:
//
//	const MAX_OUTPUT_TOKEN_KEYS = ["max_tokens", "max_completion_tokens", "max_output_tokens"];
var maxOutputTokenKeys = []string{"max_tokens", "max_completion_tokens", "max_output_tokens"}

// stripRule 照抄 paramSupport.ts:22-28 的 StripRule 类型。
//
//	match 在 TS 里是 RegExp | ((model: string) => boolean); Go 侧统一用
//	函数表达, 正则型规则在构造时包一层闭包 (见 stripRules)。
type stripRule struct {
	provider string // 空串表示"任意 provider" (对应 TS 的 provider?: 可选)
	match    func(model string) bool
	drop     []string
	// clampToModelMaxOutput 对应 TS 同名字段: 把 max output 家族压到模型的
	// catalog maxOutputTokens 上限。我方无 model catalog 表, 该字段恒为 false,
	// 保留字段是为了与参考实现的规则表逐条对齐、便于后续补 catalog 时启用。
	clampToModelMaxOutput bool
	// maxOutputCap 对应 TS 同名字段: 固定端点上限 (0 表示不设)
	maxOutputCap int
}

// re 是正则匹配辅助: 把 TS 的 `/xxx/i` 字面量转成 Go 的 model 判定函数。
// 照抄每条规则时保持正则原文不变, 只把定界符换成 Go 写法。
func re(pattern string) func(string) bool {
	compiled := regexp.MustCompile("(?i)" + pattern)
	return func(model string) bool { return compiled.MatchString(model) }
}

// stripRules 逐条照抄 paramSupport.ts:32-96 的 STRIP_RULES。
//
// 每条规则的 provider 作用域、模型匹配、drop 列表都与原文一一对应,
// 原文的注释也保留在条目旁, 便于日后核对。
var stripRules = []stripRule{
	// :34 claude-opus-4 series: temperature is deprecated (Anthropic returns 400). #1748
	{match: re(`claude-opus-4`), drop: []string{"temperature"}},

	// :36 GitHub Copilot gpt-5.4: temperature unsupported.
	{provider: "github", match: re(`gpt-5\.4`), drop: []string{"temperature"}},

	// :38-42 GitHub Copilot Claude (except opus/sonnet 4.6): thinking + reasoning_effort rejected. #713
	{
		provider: "github",
		match: func(m string) bool {
			hasClaude := regexp.MustCompile(`(?i)claude`).MatchString(m)
			is46 := regexp.MustCompile(`(?i)claude.*(opus|sonnet).*4\.6`).MatchString(m)
			return hasClaude && !is46
		},
		drop: []string{"thinking", "reasoning_effort"},
	},

	// :43-48 NVIDIA NIM z-ai/glm-5.2: OpenAI-compatible wrapper rejects BOTH the
	// `reasoning` body field (#6102) and the Claude-style `thinking` field.
	// 9router#2023.
	{provider: "nvidia", match: re(`z-ai/glm-5\.2\b`), drop: []string{"reasoning", "thinking"}},

	// :49-52 NVIDIA NIM minimaxai/minimax-m2.7: 400 "Unsupported parameter(s): thinking".
	// Upstream #2268.
	{provider: "nvidia", match: re(`minimax-m2\.7`), drop: []string{"thinking"}},

	// :53-57 NVIDIA NIM: 400s on `prompt_cache_key` (Codex CLI injects it).
	// provider-wide, not model-specific. #7617.
	{provider: "nvidia", match: func(string) bool { return true }, drop: []string{"prompt_cache_key"}},

	// :58-71 VolcEngine Ark caps the Kimi coding-plan endpoint at max_tokens <= 32768.
	// Scoped to the exact model id (not a broad /kimi/i regex).
	{
		provider:              "volcengine",
		match:                 re(`^kimi-k2-5-260127$`),
		maxOutputCap:          32768,
		clampToModelMaxOutput: true,
	},

	// :72-81 #7364: Z.AI's glm-4.6v vision endpoint enforces a 32768 max_tokens ceiling.
	{provider: "zai", match: re(`^glm-4\.6v$`), maxOutputCap: 32768},
	// :82 glm executor path: has catalog ceiling, so clampToModelMaxOutput suffices.
	{provider: "glm", match: re(`^glm-4\.6v$`), clampToModelMaxOutput: true},

	// :83-95 Azure gpt-4o-mini deployments cap completion tokens at 16384.
	{provider: "azure-openai", match: re(`^gpt-4o-mini`), maxOutputCap: 16384},
	{provider: "azure-ai", match: re(`^gpt-4o-mini`), maxOutputCap: 16384},
}

// stripUnsupportedParams 照抄 paramSupport.ts:141-166。
//
//	export function stripUnsupportedParams<T>(provider, model, body): T {
//	  if (!model || !body || typeof body !== "object") return body;
//	  const rec = body as Record<string, unknown>;
//	  const snapshot = { ...rec };
//	  // Phase 1: Hardcoded rules (unchanged)
//	  for (const rule of STRIP_RULES) {
//	    if (rule.provider && rule.provider !== provider) continue;
//	    if (!matches(rule, model)) continue;
//	    for (const key of rule.drop ?? []) {
//	      if (rec[key] !== undefined) delete rec[key];
//	    }
//	    applyMaxOutputClamp(rule, provider, model, rec);
//	  }
//	  // Phase 2: Config-driven rules from DB
//	  applyConfigFilters(provider, model, rec, snapshot);
//	  return body;
//	}
//
// 三条不可动摇的语义 (原注释 :15-17):
//   - 只在键**存在**时才删 (Go 侧即"键在 map 里", 天然满足)
//   - 绝不引入新键
//   - body 为 nil / 非对象时不 panic, 直接原样返回
//
// 就地修改 (in-place): 直接改传入的 map, 与参考实现一致。
func stripUnsupportedParams(provider, model string, body map[string]any) {
	if model == "" || body == nil {
		return
	}
	for _, rule := range stripRules {
		// :154 `if (rule.provider && rule.provider !== provider) continue;`
		if rule.provider != "" && rule.provider != provider {
			continue
		}
		// :155 `if (!matches(rule, model)) continue;`
		if !rule.match(model) {
			continue
		}
		// :156-158
		for _, key := range rule.drop {
			delete(body, key)
		}
		// :159
		applyMaxOutputClamp(rule, body)
	}
	// Phase 2 (config-driven / DB) 未实现 —— 见文件头注释的差异说明。
}

// applyMaxOutputClamp 照抄 paramSupport.ts:108-135。
//
//	function applyMaxOutputClamp(rule, provider, model, body) {
//	  if (!rule.clampToModelMaxOutput && !Number.isFinite(rule.maxOutputCap)) return;
//	  const candidates = [];
//	  if (rule.clampToModelMaxOutput) {   // 读模型 catalog 上限
//	    const modelCeiling = getProviderModel(provider, model)?.maxOutputTokens;
//	    if (Number.isFinite(modelCeiling) && modelCeiling > 0) candidates.push(modelCeiling);
//	  }
//	  if (Number.isFinite(rule.maxOutputCap) && rule.maxOutputCap > 0) {
//	    candidates.push(rule.maxOutputCap);
//	  }
//	  if (candidates.length === 0) return;
//	  const ceiling = Math.min(...candidates);
//	  for (const key of MAX_OUTPUT_TOKEN_KEYS) {
//	    const value = body[key];
//	    if (typeof value === "number" && Number.isFinite(value) && value > ceiling) {
//	      body[key] = ceiling;
//	    }
//	  }
//	}
//
// 要点:
//   - 只压**已存在且是数字**的键, 绝不新建键 (:117 原注释 "never introduces a new key")
//   - 只在**大于**上限时才改 (小值保持原样)
//   - 两个上限同时存在时取较小者
//
// 我方差异: `clampToModelMaxOutput` 需要 model catalog 表 (getProviderModel),
// 我方没有, 因此该分支贡献不了候选值。只看 maxOutputCap。已在文件头声明。
func applyMaxOutputClamp(rule stripRule, body map[string]any) {
	// :114 `if (!rule.clampToModelMaxOutput && !Number.isFinite(rule.maxOutputCap)) return;`
	if !rule.clampToModelMaxOutput && rule.maxOutputCap <= 0 {
		return
	}
	if rule.maxOutputCap <= 0 {
		// 只有 clampToModelMaxOutput 但没有 catalog → 无候选值, 直接返回
		return
	}
	ceiling := float64(rule.maxOutputCap)
	for _, key := range maxOutputTokenKeys {
		value, ok := body[key].(float64)
		if !ok {
			continue
		}
		if value > ceiling {
			body[key] = ceiling
		}
	}
}

// modelMatchesAnyStripRule 供上层判定"该 provider/model 组合是否有参数规则",
// 用于日志与可观测性。参考实现无此函数, 属我方新增的只读辅助, 不改判定逻辑。
func modelMatchesAnyStripRule(provider, model string) bool {
	for _, rule := range stripRules {
		if rule.provider != "" && rule.provider != provider {
			continue
		}
		if rule.match(model) {
			return true
		}
	}
	return false
}

// stripRuleProviderCount 返回规则表中涉及的 provider 数(去重), 仅用于自检。
func stripRuleProviderCount() int {
	seen := map[string]bool{}
	for _, r := range stripRules {
		if r.provider != "" {
			seen[r.provider] = true
		}
	}
	return len(seen)
}

// normalizeProviderID 把内部 provider 标识归一成参考实现使用的 id 形式
// (小写、去空白)。参考实现的 provider 是注册表 id, 我方是配置里的字符串,
// 大小写可能不一致, 归一后再比对可避免规则漏匹配。
func normalizeProviderID(p string) string {
	return strings.ToLower(strings.TrimSpace(p))
}
