package app

import (
	"regexp"
	"strings"
)

// 逐字照抄 OmniRoute open-sse/services/reasoningCache.ts:83-121 与
// open-sse/translator/helpers/schemaCoercion.ts:455-483。
//
// 参考实现原文（reasoningCache.ts:80-121）:
//
//	/**
//	 * Check if a provider/model combination requires reasoning replay.
//	 * ...
//	 */
//	export function requiresReasoningReplay(params: {...}): boolean {
//	  const normalizedProvider = params.provider.trim().toLowerCase();
//	  const normalizedModel = params.model.trim();
//	  const normalizedInterleavedField = ...;
//
//	  // Explicit model signal from models.dev (preferred source of truth).
//	  if (normalizedInterleavedField === "reasoning_content") return true;
//	  if (normalizedInterleavedField === "reasoning_details") return false;
//
//	  if (K3_REASONING_REPLAY_MODEL_PATTERN.test(normalizedModel)) return true;
//	  if ((normalizedProvider === "moonshot" || normalizedProvider === "kimi") &&
//	      NATIVE_K27_REASONING_REPLAY_MODEL_PATTERN.test(normalizedModel)) {
//	    return true;
//	  }
//
//	  // DeepSeek legacy reasoner family has an inverse contract: do not replay.
//	  if (/deepseek-reasoner/i.test(normalizedModel) || /deepseek-r1/i.test(normalizedModel)) {
//	    return false;
//	  }
//
//	  // Explicit known contract: DeepSeek V4 thinking with tool calls requires replay.
//	  if (isDeepSeekReasoningModel(params)) return true;
//
//	  const useLegacyFallback = params.allowLegacyFallback !== false;
//	  if (!useLegacyFallback) return false;
//
//	  if (REASONING_REPLAY_PROVIDERS.has(normalizedProvider)) return true;
//	  return REASONING_REPLAY_MODEL_PATTERNS.some((p) => p.test(normalizedModel));
//	}
//
// 与 schemaCoercion.ts:455-483:
//
//	export function injectEmptyReasoningContentForToolCalls(
//	  messages: unknown, provider: unknown, model: unknown
//	): unknown {
//	  const normalizedProvider = String(provider ?? "");
//	  const normalizedModel = String(model ?? "");
//	  const needsReasoning = requiresReasoningReplay({
//	    provider: normalizedProvider, model: normalizedModel, thinkingEnabled: true,
//	  });
//	  if (!Array.isArray(messages) || !needsReasoning) return messages;
//	  return messages.map((message) => {
//	    if (!isPlainObject(message)) return message;
//	    if (message.role !== "assistant" ||
//	        !Array.isArray(message.tool_calls) ||
//	        message.tool_calls.length === 0 ||
//	        message.reasoning_content !== undefined) {
//	      return message;
//	    }
//	    return { ...message, reasoning_content: "" };
//	  });
//	}
//
// # 为什么这条对本网关重要
//
// DeepSeek V4 / Kimi thinking / Xiaomi MiMo 这类上游有**反向契约**: 多轮请求里
// assistant 回合若带 `tool_calls`, 就必须同时带上 `reasoning_content`(哪怕是空串)。
// 客户端(尤其 Codex 经 cc-switch 路由、OpenRouter 等中间层)会把该字段丢掉,
// 上游直接 400:
//
//	"Param Incorrect: The reasoning_content in the thinking mode must be
//	 passed back to the API."
//
// 在 agent 客户端里这条 400 常常只表现为**任务无声中断** —— 与用户报的现象一致。
//
// 参考实现的解法很小: 缺字段时补一个**空串**(不是占位符、不是省略), 让上游的
// "必须存在该字段"校验通过, 同时不污染上下文。
//
// ★ 反直觉点（禁止"顺手修正"）:
//   - 补的是**空串 `""`**, 不是任何占位符文案;
//   - 判定用的是 `message.reasoning_content !== undefined` —— **空串也算已存在**,
//     已存在的字段一律不动(不覆盖、不清空);
//   - 只在 `role === "assistant"` 且 `tool_calls` 是**非空数组**时才补;
//     纯文本 assistant 不补。
//
// ★ 接入作用域（照抄的一部分）
//
// 参考实现有两层限定 (translator/index.ts:610-619):
//
//	if (
//	  targetFormat === FORMATS.OPENAI &&
//	  !requiresExplicitReasoningReplay &&
//	  result.messages && Array.isArray(result.messages)
//	) {
//	  result.messages = injectEmptyReasoningContentForToolCalls(result.messages, provider, model);
//	}
//
// 即: **只在出站目标是 OpenAI 形态、且该 provider/model 不由"显式回放"通道处理**
// 时才跑。`REASONING_REPLAY_PROVIDERS` / 正则表本身则决定"哪些 provider/model
// 需要回放"。

// ──────────────── 判定常量表（照抄 reasoningCache.ts:30-68）────────────────

// reasoningReplayProviders 对应 REASONING_REPLAY_PROVIDERS。
var reasoningReplayProviders = map[string]bool{
	"deepseek":           true,
	"opencode-go":        true,
	"siliconflow":        true,
	"nebius":             true,
	"deepinfra":          true,
	"sambanova":          true,
	"fireworks":          true,
	"together":           true,
	"kimi-coding":        true,
	"kimi-coding-apikey": true,
	// Xiaomi MiMo 与 DeepSeek/Kimi-thinking 同契约: 后续回合必须回传
	// reasoning_content, 否则上游 400 "Param Incorrect: The reasoning_content
	// in the thinking mode must be passed back to the API."
	"xiaomi-mimo": true,
}

// reasoningReplayModelPatterns 对应 REASONING_REPLAY_MODEL_PATTERNS。
var reasoningReplayModelPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)deepseek-r1`),
	regexp.MustCompile(`(?i)deepseek-reasoner`),
	regexp.MustCompile(`(?i)deepseek-chat`),
	regexp.MustCompile(`(?i)deepseek[-/]v4[-.](flash|pro)(-free)?`),
	regexp.MustCompile(`(?i)zen/deepseek-v4`),
	// 匹配原生 kimi-kN 与带命名空间的 kimi/kN 家族, 但不把 kimi-latest 这类
	// 泛化别名当成严格 thinking 模型。
	regexp.MustCompile(`(?i)kimi[-/]k\d`),
	regexp.MustCompile(`(?i)qwq`),
	regexp.MustCompile(`(?i)qwen.*think`),
	regexp.MustCompile(`(?i)glm.*think`),
	// MiMo (小米) thinking 模型 —— 万一通配路由把非 xiaomi-mimo 的 provider id
	// 分给了 mimo-* 别名, 这条兜底。
	regexp.MustCompile(`(?i)^mimo[-.]?v\d`),
}

// deepseekV4ModelPattern 对应 DEEPSEEK_V4_MODEL_PATTERN。
var deepseekV4ModelPattern = regexp.MustCompile(`(?i)deepseek[-/]v4[-.](flash|pro)`)

// deepseekLegacyReasonerPatterns 对应参考实现 :108-111 的两条内联正则
// (/deepseek-reasoner/i、/deepseek-r1/i)。模式与 reasoningReplayModelPatterns
// 前两条一致, 但语义独立(反向契约分支), 单独声明为包级变量避免每次调用重编译。
var deepseekLegacyReasonerPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)deepseek-reasoner`),
	regexp.MustCompile(`(?i)deepseek-r1`),
}

// K3 / 原生 K2.7 判定复用 zen_reasoning.go 里已照抄的同名正则:
//
//	k3AuthenticReasoningPattern        ← K3_REASONING_REPLAY_MODEL_PATTERN
//	nativeK27AuthenticReasoningPattern ← NATIVE_K27_REASONING_REPLAY_MODEL_PATTERN
//
// 两者在参考实现里是**同一个正则字面量**分别用于两个常量, 故复用同一个
// Go 变量, 不重复定义。

// isDeepSeekReasoningModel 照抄 reasoningCache.ts:71-78。
//
//	export function isDeepSeekReasoningModel(params: {
//	  provider: string; model: string; thinkingEnabled?: boolean;
//	}): boolean {
//	  if (params.thinkingEnabled !== true) return false;
//	  return DEEPSEEK_V4_MODEL_PATTERN.test(params.model);
//	}
func isDeepSeekReasoningModel(model string, thinkingEnabled bool) bool {
	if !thinkingEnabled {
		return false
	}
	return deepseekV4ModelPattern.MatchString(model)
}

// requiresReasoningReplay 照抄 reasoningCache.ts:83-121。
//
// 参数对应参考实现的字段; `interleavedField` 传空串表示"无 models.dev 信号",
// `allowLegacyFallback` 传 true 表示走默认的 fallback 分支。
func requiresReasoningReplay(provider, model, interleavedField string, thinkingEnabled, allowLegacyFallback bool) bool {
	normalizedProvider := toLowerTrim(provider)
	normalizedModel := trimSpace(model)
	normalizedInterleavedField := toLowerTrim(interleavedField)

	// :96-98 显式模型信号优先(来自 models.dev, 是权威来源)。
	if normalizedInterleavedField == "reasoning_content" {
		return true
	}
	if normalizedInterleavedField == "reasoning_details" {
		return false
	}

	// :100
	if k3AuthenticReasoningPattern.MatchString(normalizedModel) {
		return true
	}
	// :101-106
	if (normalizedProvider == "moonshot" || normalizedProvider == "kimi") &&
		nativeK27AuthenticReasoningPattern.MatchString(normalizedModel) {
		return true
	}

	// :108-111 DeepSeek 旧 reasoner 家族是**反向**契约: 不回放。
	// (复用包级已编译正则, 不再在每请求路径上重新 MustCompile。)
	for _, re := range deepseekLegacyReasonerPatterns {
		if re.MatchString(normalizedModel) {
			return false
		}
	}

	// :113-114 显式已知契约: DeepSeek V4 thinking + 工具调用必须回放。
	if isDeepSeekReasoningModel(normalizedModel, thinkingEnabled) {
		return true
	}

	// :116-117
	if !allowLegacyFallback {
		return false
	}

	// :119-120
	if reasoningReplayProviders[normalizedProvider] {
		return true
	}
	for _, re := range reasoningReplayModelPatterns {
		if re.MatchString(normalizedModel) {
			return true
		}
	}
	return false
}

// injectEmptyReasoningContentForToolCalls 照抄 schemaCoercion.ts:455-483。
//
// 语义（逐条）:
//   - 非数组 messages → 原样返回;
//   - 该 provider/model 不需要回放 → 原样返回(**不重建切片**);
//   - 逐条消息: 非对象 / 非 assistant / 无 tool_calls / tool_calls 为空 /
//     已存在 reasoning_content 字段 → 该条原样;
//   - 否则浅拷贝并补 `reasoning_content: ""`。
//
// 返回 (messages, 是否有改动)。
func injectEmptyReasoningContentForToolCalls(messages any, provider any, model any) (any, bool) {
	providerStr := jsStringOrEmpty(provider)
	modelStr := jsStringOrEmpty(model)

	// :463-467
	needsReasoning := requiresReasoningReplay(providerStr, modelStr, "", true, true)
	// :469 `if (!Array.isArray(messages) || !needsReasoning) return messages;`
	arr, isArr := messages.([]any)
	if !isArr || !needsReasoning {
		return messages, false
	}

	// :471 `return messages.map((message) => {`
	changed := false
	out := make([]any, 0, len(arr))
	for _, mRaw := range arr {
		// :472 `if (!isPlainObject(message)) return message;`
		if !isPlainObject(mRaw) {
			out = append(out, mRaw)
			continue
		}
		m := mRaw.(map[string]any)
		// :473-478
		tcs, hasTCs := m["tool_calls"].([]any)
		if !jsIsAssistant(m) || !hasTCs || len(tcs) == 0 || jsHasKey(m, "reasoning_content") {
			out = append(out, mRaw)
			continue
		}
		// :480 `return { ...message, reasoning_content: "" };`
		nm := shallowCopyStringAny(m)
		nm["reasoning_content"] = ""
		out = append(out, nm)
		changed = true
	}
	if !changed {
		return messages, false
	}
	// 参考实现的 .map 恒返回新数组; 我方仅在真有改动时返回新切片,
	// 让"无需改动"的路径保持引用不变(下游可据此跳过后续处理)。
	return out, true
}

// hasThinkingConfig 照抄 services/provider.ts:471-473。
//
//	export function hasThinkingConfig(body) {
//	  return !!(body.reasoning_effort || body.thinking?.type === "enabled");
//	}
//
// 用途: 它是 `requiresExplicitReasoningReplay` 的 `thinkingEnabled` 实参
// (translator/index.ts:365)。注意与 injectEmptyReasoningContentForToolCalls
// 内部写死的 `thinkingEnabled: true` **不对称** —— 这不是笔误, 是参考实现故意的:
// 显式通道看客户端是否真的开了 thinking, 占位通道则一律按"需要"处理。
func hasThinkingConfig(params map[string]any) bool {
	if _, ok := params["reasoning_effort"]; ok {
		// `!!body.reasoning_effort`: 数字 0 / 空串 / false / null 在 JS 里都是 falsy。
		if jsTruthy(params["reasoning_effort"]) {
			return true
		}
	}
	// `body.thinking?.type === "enabled"` —— 可选链, 非对象时为 undefined。
	if th, ok := params["thinking"].(map[string]any); ok {
		if s, _ := th["type"].(string); s == "enabled" {
			return true
		}
	}
	return false
}

// jsTruthy 复刻 JS 的 `!!value` 真值判定。
//
// 对本表的输入集, 差异只在字符串与数字上: `""` / `0` / `NaN` 为假, 其余为真;
// null / undefined / false 为假; 空数组与空对象在 JS 里**为真**(这点常被记错)。
func jsTruthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != ""
	case float64:
		return t != 0
	case int:
		return t != 0
	case int64:
		return t != 0
	default:
		// 数组 / map / 其它对象: JS 里一律为真。
		return true
	}
}

// applyEmptyReasoningReplay 是 injectEmptyReasoningContentForToolCalls 的**接入级包装**,
// 逐字对位 translator/index.ts:610-619 的 gate:
//
//	if (
//	  targetFormat === FORMATS.OPENAI &&
//	  !requiresExplicitReasoningReplay &&
//	  result.messages && Array.isArray(result.messages)
//	) {
//	  result.messages = injectEmptyReasoningContentForToolCalls(result.messages, provider, model);
//	}
//
// ★ 三方限定必须都照抄, 缺一条就会越界施加:
//
//  1. `targetFormat === FORMATS.OPENAI` —— 只在出站是 **OpenAI 形态**时跑。
//     我方由调用方用 `cfg.APIType != "anthropic"` 表达(anthropic 形态走
//     messages/blocks 协议, 补 `reasoning_content` 字段毫无意义; 参考实现对
//     该分支走的是 content[] 里插 thinking block, 是另一套逻辑)。
//
//  2. `!requiresExplicitReasoningReplay` —— ★ **最容易被抄错的一条**。
//     `requiresExplicitReasoningReplay` 的实参是
//     `{...replayRequirements, allowLegacyFallback: false}`, 而 `replayRequirements`
//     里的 `thinkingEnabled` 是 `hasThinkingConfig(result)`。
//     实测(探针 probe_replay_gate_equiv.mjs): 它与 inject **内部**的
//     `needsReasoning`(= `allowLegacyFallback` 默认 true、`thinkingEnabled` 写死 true)
//     **不相等, 480 组输入里 256 组不同**。差异全集中在"provider 落在
//     REASONING_REPLAY_PROVIDERS 白名单里"的输入上 —— 白名单只在 fallback 分支生效,
//     所以 `allowLegacyFallback=false` 时它们全变 false。
//     若照抄时把这里错写成 `!requiresReasoningReplay(..., true)`(即 Gate B),
//     则 provider=deepseek/tokenrouter/siliconflow 等**所有**模型都会整体不跑 ——
//     恰恰是本网关最常见的场景。故必须保持 `allowLegacyFallback=false` + 真实
//     `thinkingEnabled`。
//
//  3. `result.messages && Array.isArray(result.messages)` —— 非数组直接跳过。
//     我方 `params["messages"].([]any)` 的类型断言同时表达了这两件事。
//
// 返回 (改写后的 messages, 是否改动)。
func applyEmptyReasoningReplay(messages any, provider, model string, params map[string]any) (any, bool) {
	arr, ok := messages.([]any)
	if !ok {
		// 对位 `result.messages && Array.isArray(result.messages)` 的失败分支。
		return messages, false
	}
	// 对位 index.ts:373-376: `requiresExplicitReasoningReplay = requiresReasoningReplay({
	// ...replayRequirements, allowLegacyFallback: false })`。
	explicit := requiresReasoningReplay(provider, model, "", hasThinkingConfig(params), false)
	if explicit {
		return messages, false
	}
	return injectEmptyReasoningContentForToolCalls(arr, provider, model)
}

// jsIsAssistant 复刻 `message.role !== "assistant"` 的判定。
func jsIsAssistant(m map[string]any) bool {
	role, _ := m["role"].(string)
	return role == "assistant"
}

// jsHasKey 复刻 `key in obj` / `obj[key] !== undefined`。
//
// JSON 反序列化到 Go 后, "值显式为 null" 与 "键存在" 都能被 `_, has` 观察到 ——
// 参考实现的 `message.reasoning_content !== undefined` 对显式 null **为真**
// (null !== undefined), 故此处同样按"键存在"判定, 与参考实现一致。
func jsHasKey(m map[string]any, key string) bool {
	_, has := m[key]
	return has
}

// jsStringOrEmpty 复刻 `String(value ?? "")`。
func jsStringOrEmpty(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return jsStringOf(v)
}

// toLowerTrim 对应参考实现的 `String(provider).trim().toLowerCase()`。
//
// trimSpace 已在 textual_tool_call_collect.go 定义(同为 JS `String.trim()` 语义);
// 小写折叠复用 strings.ToLower —— provider / model id 都是 ASCII, 与 JS 的
// toLowerCase() 在本表覆盖的输入上等价。
func toLowerTrim(s string) string {
	return strings.ToLower(trimSpace(s))
}
