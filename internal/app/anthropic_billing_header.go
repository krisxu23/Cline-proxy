package app

import "regexp"

// 逐字照抄 OmniRoute open-sse/translator/request/claude-to-openai.ts:16-19。
//
// 参考实现原文 + 原注释:
//
//	/**
//	 * Port of decolua/9router commit 0aaa5ab3 (closes prompt-cache instability):
//	 * Anthropic injects a dynamic `x-anthropic-billing-header: <value>` line at
//	 * the top of some system prompts. When we translate Claude → OpenAI and
//	 * forward to a non-Anthropic upstream, that line both leaks into the
//	 * assistant prompt and rotates per request, destroying prompt-cache hits.
//	 * Strip it from each system entry before assembling the OpenAI request.
//	 */
//	function stripAnthropicBillingHeader(text: unknown): string {
//	  if (typeof text !== "string") return "";
//	  return text.replace(/^x-anthropic-billing-header:[^\n]*(?:\r?\n)?/i, "");
//	}
//
// # 为什么这条对本网关重要
//
// 本网关的入站 Claude 路径（`handleAnthropicMessages` → `anthropicToOpenAI`）正是
// "translate Claude → OpenAI 再转发给非 Anthropic 上游"这一场景。Anthropic 会在
// **部分** system prompt 顶部注入一行动态的
// `x-anthropic-billing-header: <每请求都变的值>`。
//
// 不剥掉有两个后果，且**两个都不报错**：
//
//  1. 那一行会漏进上游的 system prompt（污染模型上下文）；
//  2. 该值**每请求轮换** —— 而它在 prompt 的**最开头**，于是整个前缀的
//     prompt-cache 永远不命中，长会话下每一轮都按未缓存计费。
//
// 这是"无声的"缺陷：请求成功、回答正常，只有账单和延迟变了。
//
// # Go 侧语义对齐要点（探针 B1–B12 逐条锁定）
//
//   - `^` 是**字符串开头**（JS 无 `m` flag）→ 只有**第一行**会被删。中间行
//     （如 `"Hello\nx-anthropic-billing-header: abc\nWorld"`）**原样保留**（B5）。
//   - **只替换一次**（无 `g` flag）→ 连续两行头部只删第一行（B6）。
//   - `/i` 大小写不敏感（B4）。
//   - `[^\n]*` 吃到行尾（不含 `\n`）；`(?:\r?\n)?` 再顺手吃掉一个 `\n` 或 `\r\n`。
//     头部位于**字符串末尾**时（无换行）整段删掉后得空串（B2/B7）。
//   - 冒号后**无空格**也算匹配（B8: `"x-anthropic-billing-header:\nHello"` → `"Hello"`）；
//     冒号后多个空格同样被 `[^\n]*` 吃掉。
//   - 前后有其它字符时不匹配（B9 的 `" x-anthropic..."` 与 `"prefix x-anthropic..."`）。
//   - `\r` **单独**出现（后面没有 `\n`）时被 `[^\n]*` 当作行内容吃掉（B12）。
//   - **非字符串一律返回空串**（B10）—— 连 `null`/数字/对象都变 `""`，
//     不是"原样返回"。这一点在 Go 侧最容易抄错（写成 `return text` 或返回 nil）。
//
// ★ Go 的 `regexp` 与 JS 的差异: Go 的 `(?i)` 前缀表达 `/i`；Go 没有 `^` 的
//
//	"仅字符串开头"歧义（默认即如此，除非加 `(?m)`）。此处**不能**加 `(?m)`。
var anthropicBillingHeaderRe = regexp.MustCompile(`(?i)^x-anthropic-billing-header:[^\n]*(?:\r?\n)?`)

// stripAnthropicBillingHeader 照抄 claude-to-openai.ts:16-19。
//
// 输入非字符串时返回**空串**（不是入参本身，也不是 nil）—— 见探针 B10。
func stripAnthropicBillingHeader(text any) string {
	s, ok := text.(string)
	if !ok {
		// `if (typeof text !== "string") return "";`
		return ""
	}
	// `text.replace(/^x-anthropic-billing-header:[^\n]*(?:\r?\n)?/i, "")`
	//
	// ★ Go 的 ReplaceAllString 默认就是"替换所有非重叠匹配"，但本正则被 `^`
	//   锚定在字符串开头，故最多只有一处匹配 —— 与 JS 的"只替换一次"等价。
	return anthropicBillingHeaderRe.ReplaceAllString(s, "")
}
