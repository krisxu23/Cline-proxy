package app

import "strings"

// ─────────────────────────────────────────────────────────────────────────────
// 「上游有没有交付用户可见内容」的**唯一判定来源**。
//
// 背景(2026-09-17 审查 P0-1): 此前全仓把**帧级转发过滤**的判据当成了
// **流级产出判据** —— role 骨架帧、finish_reason 帧、只有输入 token 的 usage
// 都被算作"已交付", 于是"上游 200 但零产出"的静默空回合能穿过守卫, 客户端
// 拿到一个"成功但空"的回合(正是要根治的症状)。
//
// 两个问题必须分开, 判据不可互换:
//
//	帧级 —— 这一帧要不要转发给客户端?
//	  参考实现: OmniRoute utils/streamHelpers.ts:379 hasValuableContent,
//	  用于 stream.ts:1803 `if (!hasValuableContent(...)) continue;`。
//	  对这个问题, role / finish_reason **算有价值**(客户端靠它们收尾),
//	  所以那个函数用于帧级是对的。
//
//	流级 —— 整条流有没有向用户交付可见产出?   ← 本文件回答的问题
//	  role / finish_reason 是**脚手架**, 不是产出, 因此**不算**。
//
// 本文件只提供流级判据。帧级判据目前**没有生产调用点**(帧一律照常透传),
// 因此不再保留同名的帧级函数 —— 留着只会诱导下一次误用。
// ─────────────────────────────────────────────────────────────────────────────

// chunkDeliversUserContent 一个 OpenAI 形态的 chunk 是否携带用户可见产出。
//
// 只认三类真实产出:
//   - 非空正文(content 字符串, 或 content 数组里任一块带非空 text)
//   - 非空推理内容(reasoning 各别名, **trim 后**非空 —— 纯空白不算产出)
//   - 结构化工具调用(tool_calls 非空)
//
// 显式**不认**: role 骨架帧、finish_reason、usage。
// "本就不该有正文"的合法终止态由 legitEmptyTerminalReason 单独表达, 不走这里。
//
// 与参考实现的三处**有意**差异(都是"帧级 → 流级"换用所必需的):
//
//  1. role 骨架帧 / finish_reason **不算产出**。参考实现把它们算作有价值,
//     那是为帧级转发服务的(客户端需要这两个帧收尾); 但一条只发了
//     `{"delta":{"role":"assistant"}}` 或 `{"delta":{},"finish_reason":"stop"}`
//     就结束的流, 对用户等于零产出 —— 必须判空。实测: 这两种形态能骗过
//     此前全部守卫(2026-09-17 审查, 用临时用例验证)。
//  2. 遍历**全部** choice, 不只 choices[0]。流级问题问的是"有没有产出",
//     任一 choice 有产出即算交付。
//  3. 也接受**非流式的 message 形态**。完整回包合成路径用的是 message,
//     只认 delta 会让有内容的回包被判空。
//
// ★ trim 的取舍与 LiteLLM 相反, 且是有意的:
//
//	LiteLLM model_response_utils.py 的 _has_meaningful_content 明确不 trim
//	(注释原文 "Don't strip whitespace ... Even pure whitespace characters
//	like '\n' or ' ' are meaningful content"), 因为**流式 delta 的首尾空格
//	是有意义的**("Hello, " + "world." 拼接时不能吃掉空格)。
//	那个口径服务于**帧级**; 本函数回答的是**流级**"用户能不能看到东西",
//	一个只发了 " " 就结束的流对用户等于零产出, 必须算空。
//	正文(content)不做 trim —— 拼接语义同样适用, 且空串判断已足够。
func chunkDeliversUserContent(obj map[string]any) bool {
	if obj == nil {
		return false
	}
	// Cline 部分回包会包一层 {data:{...}}, 先解包裹
	body := obj
	if d, ok := obj["data"].(map[string]any); ok {
		body = d
	}
	if choices, ok := body["choices"].([]any); ok {
		for _, raw := range choices {
			choice, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			// delta 形态(流式) / message 形态(非流式)
			target, ok := choice["delta"].(map[string]any)
			if !ok {
				target, ok = choice["message"].(map[string]any)
			}
			if ok && messageDeliversUserContent(target) {
				return true
			}
		}
	}
	// Claude 形态: 顶层 delta / message
	if target, ok := body["delta"].(map[string]any); ok && messageDeliversUserContent(target) {
		return true
	}
	if target, ok := body["message"].(map[string]any); ok && messageDeliversUserContent(target) {
		return true
	}
	return false
}

// messageDeliversUserContent 判定一个 message / delta 对象是否携带可见产出。
func messageDeliversUserContent(m map[string]any) bool {
	if m == nil {
		return false
	}
	if c, ok := m["content"].(string); ok && c != "" {
		return true
	}
	if blocks, ok := m["content"].([]any); ok && contentBlocksHaveText(blocks) {
		return true
	}
	if hasNonBlankReasoning(m) {
		return true
	}
	if tc, ok := m["tool_calls"].([]any); ok && len(tc) > 0 {
		return true
	}
	return false
}

// contentBlocksHaveText content 数组形态里是否任一块带非空文本。
// OpenAI content-parts / Anthropic blocks 经翻译后都是这个形状。
func contentBlocksHaveText(blocks []any) bool {
	for _, raw := range blocks {
		block, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if t, ok := block["text"].(string); ok && t != "" {
			return true
		}
		if t, ok := block["thinking"].(string); ok && strings.TrimSpace(t) != "" {
			return true
		}
	}
	return false
}

// hasNonBlankReasoning 推理内容(各协议别名)trim 后是否非空。
//
// 覆盖: OpenAI 的 reasoning_content / reasoning、非标准的 reasoning_text /
// thinking / thought、以及 reasoning_details 数组里的 text / content。
// 全部要求 trim 后非空 —— 见 chunkDeliversUserContent 的 trim 取舍说明。
func hasNonBlankReasoning(m map[string]any) bool {
	if m == nil {
		return false
	}
	for _, field := range []string{"reasoning_content", "reasoning", "reasoning_text", "thinking", "thought"} {
		if s, ok := m[field].(string); ok && strings.TrimSpace(s) != "" {
			return true
		}
	}
	if details, ok := m["reasoning_details"].([]any); ok {
		for _, raw := range details {
			d, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			for _, field := range []string{"text", "content"} {
				if s, ok := d[field].(string); ok && strings.TrimSpace(s) != "" {
					return true
				}
			}
		}
	}
	return false
}

// hasOutputUsageTokens usage 里是否报告了**输出侧** token。
//
// 只有输出侧才算"上游确实生成了东西":
//
//	completion_tokens(OpenAI) / output_tokens(Claude) / candidatesTokenCount(Gemini)
//
// **输入侧不算**: prompt_tokens / input_tokens / promptTokenCount 有值只说明
// 上游收到了我们的 prompt, 完全不能说明它产出了任何东西。
// {prompt_tokens:1500, completion_tokens:0} 正是最典型的真·空回包 ——
// 此前的判定(任一 token 字段 > 0)把它当成"已交付", 于是空流守卫被绕过
// (2026-09-17 审查 P0-1)。
func hasOutputUsageTokens(usage map[string]any) bool {
	if usage == nil {
		return false
	}
	for _, field := range []string{
		"completion_tokens",    // OpenAI
		"output_tokens",        // Claude
		"candidatesTokenCount", // Gemini
	} {
		if v, ok := usage[field].(float64); ok && v > 0 {
			return true
		}
	}
	return false
}

// streamDeliveryEmpty 空流守卫的**唯一判定入口**。
//
// 返回 true 表示"整条流没有交付任何用户可见产出, 应判为静默空回合"。
//
// 三个放行条件(任一成立即不算空):
//   - delivered:   累积到过 content-bearing 内容(见 chunkDeliversUserContent)
//   - outputUsage: 上游报告了**输出侧** token
//   - legitEmpty:  终止原因是"本就不该有正文"的白名单值
//     (length / tool_calls / content_filter / max_tokens / tool_use)
//
// ★ finish_reason 只通过 legitEmpty 这一个**白名单**入口影响判定:
// finish_reason="stop" 而零产出 = 静默空回合, 必须拒绝。
// 此前的实现让任意 finish_reason(含 "stop")都算"有交付价值", 守卫因此失效。
func streamDeliveryEmpty(delivered, outputUsage, legitEmpty bool) bool {
	return !delivered && !outputUsage && !legitEmpty
}

// readinessScaffoldingOnly 判断一个已解析的上游帧是否**只是脚手架**(零产出)。
//
// 用途: 收紧**提交前探测**(见 stream_readiness.go 的 processStreamReadinessEvent)。
// 这类帧不算"流已就绪", 探测应继续读, 直到读到真正的内容帧 —— 因为**提交前是
// 唯一能"换站重试"的时机**(提交后响应头已发出, 只能报错)。
//
// ★ 为什么必须收紧: 原判据 hasNonPingStructuredPayload 只要"非空对象、非 error-only"
//
//	就算就绪, 于是 `{"choices":[{"delta":{"role":"assistant"}}]}` 与
//	`{"choices":[{"delta":{},"finish_reason":"stop"}]}` 都能骗过它 → 提交 →
//	整条流零产出 → 客户端拿到"成功但空"的回合。这是 P0-1 的主修复点。
//
// ★ 判据刻意保守: **只处理 OpenAI 形态**(带非空 choices 的帧), 其余形态一律返回
//
//	false 表示"不介入", 沿用原判据。原因: 探测是协议无关的, 会看到 Claude /
//	Gemini / Responses 各种形状, 而本函数不解析那些形状 —— 贸然收紧它们风险大于收益。
//
// 返回 true 的条件(全部满足):
//   - 是 OpenAI 形态(有非空 choices)
//   - 所有 choice 都没有正文、没有 tool_calls、没有任何 reasoning 键
//   - 终止原因不属于"合法空终止态"白名单(finish_reason=length 等是合法成功)
func readinessScaffoldingOnly(payload any) bool {
	m, ok := payload.(map[string]any)
	if !ok {
		return false // 非对象(数组 / 字符串 / 数字) → 不介入
	}
	choices, ok := m["choices"].([]any)
	if !ok || len(choices) == 0 {
		return false // 不是 OpenAI 形态 → 不介入
	}
	for _, raw := range choices {
		choice, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		target, ok := choice["delta"].(map[string]any)
		if !ok {
			target, ok = choice["message"].(map[string]any)
		}
		if !ok {
			continue
		}
		if s, ok := target["content"].(string); ok && s != "" {
			return false
		}
		if arr, ok := target["content"].([]any); ok && len(arr) > 0 {
			return false
		}
		if tc, ok := target["tool_calls"].([]any); ok && len(tc) > 0 {
			return false
		}
		// 有**任何** reasoning 键就不算脚手架 —— 这里**不判空**, 与
		// chunkDeliversUserContent 的 trim 口径有意不同: 探测阶段保守,
		// 宁可多读一帧, 也不要在"上游正在吐 reasoning"时误判成脚手架。
		for _, k := range []string{
			"reasoning_content", "reasoning", "reasoning_text",
			"thinking", "thought", "reasoning_details",
		} {
			if _, has := target[k]; has {
				return false
			}
		}
	}
	// 全部 choice 都无产出 → 看终止原因是否属于合法空白名单
	if legitEmptyTerminalReason(m) {
		return false
	}
	return true
}
