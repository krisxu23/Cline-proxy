package app

import (
	"encoding/json"
	"strconv"
)

// 逐字照抄 OmniRoute open-sse/translator/helpers/maxTokensHelper.ts (21 行)
// + open-sse/config/constants.ts:151 / :154 的两个常量。
//
// 参考实现原文:
//
//	export const DEFAULT_MAX_TOKENS = 64000;
//	export const DEFAULT_MIN_TOKENS = 32000;
//
//	/**
//	 * Adjust max_tokens based on request context
//	 * @param {object} body - Request body
//	 * @returns {number} Adjusted max_tokens
//	 */
//	export function adjustMaxTokens(body) {
//	  const requestedMaxTokens = body.max_tokens ?? body.max_completion_tokens;
//	  let maxTokens = requestedMaxTokens || DEFAULT_MAX_TOKENS;
//
//	  // Auto-increase for tool calling to prevent truncated arguments
//	  // Tool calls with large content (like writing files) need more tokens
//	  if (body.tools && Array.isArray(body.tools) && body.tools.length > 0) {
//	    if (maxTokens < DEFAULT_MIN_TOKENS) {
//	      maxTokens = DEFAULT_MIN_TOKENS;
//	    }
//	  }
//
//	  return Math.max(1, maxTokens);
//	}
//
// # 与用户场景的关系
//
// 原注释点名用途: **防止工具调用的参数被截断**。带大量内容的工具调用（比如写文件、
// 长 diff）如果在 max_tokens 很小的情况下发出, 模型输出会在参数中途被截停,
// 表现为"工具调用参数不完整 / agent 卡住"。这是 agent 任务无声中断的一个真实成因。
//
// # Go 侧语义对齐要点（JS 真值/空值语义是易错点）
//
//  1. `body.max_tokens ?? body.max_completion_tokens`
//     —— `??` 只在 **null/undefined** 时回落。0 不算空, 会取 0。
//  2. `requestedMaxTokens || DEFAULT_MAX_TOKENS`
//     —— `||` 是**真值**判定: 0 / NaN / "" 都算假 → 回落 64000。
//     所以 `max_tokens: 0` 走完 ①得到 0, 走 ②被判假 → 最终 64000。
//  3. `Math.max(1, maxTokens)` —— 兜底下界 1（负值/0 时）。
//  4. tools 判定: `body.tools && Array.isArray(body.tools) && body.tools.length > 0`
//     —— 非数组 / 空数组 / falsy 都不触发提升。
//  5. **返回值是纯函数结果, 不写回 body** —— 参考实现只 `return` 数值,
//     由调用方决定赋给哪个键（四个调用方各赋给 `max_tokens`）。

// 照抄 constants.ts:151 / :154。
const (
	// defaultMaxTokensOmniRoute defaultMaxTokens 已在本仓库另有定义(见 config 常量),
	// 此处用独立名字避免与既有值冲突; 数值逐字照抄 constants.ts:151。
	omniRouteDefaultMaxTokens = 64000
	// omniRouteDefaultMinTokens 照抄 constants.ts:154。
	omniRouteDefaultMinTokens = 32000
)

// adjustMaxTokens 照抄 maxTokensHelper.ts:8-21。
//
// 注意: 返回 float64 以匹配 JSON 解码后的数值类型; 调用方赋给 max_tokens 时
// 无需再转换。
func adjustMaxTokens(body map[string]any) float64 {
	// :9 `const requestedMaxTokens = body.max_tokens ?? body.max_completion_tokens;`
	var requestedMaxTokens any
	if v, ok := body["max_tokens"]; ok && v != nil {
		requestedMaxTokens = v
	} else if v, ok := body["max_completion_tokens"]; ok && v != nil {
		requestedMaxTokens = v
	}

	// :10 `let maxTokens = requestedMaxTokens || DEFAULT_MAX_TOKENS;`
	//
	// ★ 关键: 这是 **`||` 真值**判定而不是 `??`。所以数值 0 会被判为假而回落
	// 到 64000 —— 与第 9 行的 `??` 语义**不同层**。两行必须分别照抄, 不可合并。
	requested, requestedIsNum := toFloat64(requestedMaxTokens)
	var maxTokens float64
	if requestedIsNum && requested != 0 {
		maxTokens = requested
	} else {
		maxTokens = omniRouteDefaultMaxTokens
	}

	// :14 `if (body.tools && Array.isArray(body.tools) && body.tools.length > 0) {`
	if toolsRaw, ok := body["tools"]; ok && toolsRaw != nil {
		if toolsArr, isArr := toolsRaw.([]any); isArr && len(toolsArr) > 0 {
			// :15-17 `if (maxTokens < DEFAULT_MIN_TOKENS) { maxTokens = DEFAULT_MIN_TOKENS; }`
			if maxTokens < omniRouteDefaultMinTokens {
				maxTokens = omniRouteDefaultMinTokens
			}
		}
	}

	// :20 `return Math.max(1, maxTokens);`
	if maxTokens < 1 {
		return 1
	}
	return maxTokens
}

// toFloat64 把 JSON 数值转成 float64, 并报告是否为数值。
//
// ★ 为什么必须处理字符串: Node 实测 `{max_tokens:"1000"}` 返回 **1000** ——
// 第 10 行的 `requestedMaxTokens || DEFAULT_MAX_TOKENS` 对非空字符串 "1000"
// 判真(保留该值), 随后 `Math.max(1, "1000")` 靠 JS 隐式转换得到 1000。
// 这不是"未被覆盖的副作用", 而是真实可达的语义(IDE/SDK 会发字符串数字),
// 故 Go 侧必须复刻: 纯数字字符串按数值处理。
//
// 复刻规则(与 JS 的 Number() 对齐到本模块会遇到的输入):
//   - 数值类型 → 直接
//   - 纯数字字符串(允许正负号/小数) → 解析成数值
//   - 其余(含 "abc" / "" / nil) → 非数值
//
// 注: JS 对 `Math.max(1, "abc")` 返回 NaN, 最终调用方拿到 NaN;
// 但 `||` 那一层已把 **空串** 判假回落, 故只有非空非数字串能到达 NaN 分支。
// 该畸形输入不生产可用请求, 我方保守归为"非数值"→ 走默认值分支(不发 NaN)。
func toFloat64(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		// 加固(2026-09-24 审查): 若上游/未来某处改用 json.Decoder.UseNumber(),
		// 数值会以 json.Number 出现。此前它落到 default 分支被判"非数值" ——
		// 调用方 applyMaxOutputClamp 对非数值直接 continue, 于是**上限校验静默失效**
		// (客户端可以要一个超过模型上限的 max_tokens)。当前全仓没有 UseNumber,
		// 所以这条不可达, 但代价是一行, 收益是断掉一个"将来静默失效"的坑。
		if f, err := n.Float64(); err == nil {
			return f, true
		}
		return 0, false
	case string:
		// 空串在 `||` 层被判假, 这里只需处理非空。
		if n == "" {
			return 0, false
		}
		f, err := strconv.ParseFloat(n, 64)
		if err != nil {
			return 0, false
		}
		return f, true
	default:
		return 0, false
	}
}
