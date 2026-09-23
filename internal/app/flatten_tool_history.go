// Package app — 工具历史扁平化 (flatten tool history)。
//
// 逐字照抄 OmniRoute open-sse/utils/flattenToolHistory.ts (117 行)。
// 调用方: open-sse/handlers/chatCore/unsupportedParamsStrip.ts:45。
//
// 解决的问题 (原文件头注释逐字):
//
//	Why: when a combo leg (or any prose-only fan-out) strips the tools
//	definitions but the prior history still carries structured tool turns,
//	agentic models keep emitting tool_calls — returning empty prose and
//	triggering an upstream 503. Flattening keeps the context but removes
//	the tool-loop trigger.
//
// 真实事故 (unsupportedParamsStrip.ts:6-14 原注释):
//
//	Live incident: AI Horde (registry unsupportedParams: ["tools", ...]) 500'd
//	on real combo traffic even after `tools`/`tool_choice` were correctly
//	stripped from the live request, because the conversation HISTORY still
//	carried a prior turn's role:"assistant" tool_calls and role:"tool" result
//	messages (left over from before the combo failed over from a tool-capable
//	model). AI Horde's raw completion backend doesn't understand those message
//	shapes at all, regardless of whether live `tools` is present.
//
// 即: 剔除实时 tools 还不够 —— 历史里残留的 tool_calls / role:"tool" 消息
// 照样会让不支持工具的上游 500。必须把历史也扁平化成普通散文。
package app

import (
	"encoding/json"
	"strings"
)

// 照抄 flattenToolHistory.ts:18-19 的两个前缀常量。
//
// ⚠ 注意冒号位置: 是 "[Called tools: " 与 "[Tool result: ",
// **不是** "[Tool result]: "。我方 compact.go:115 用的是后者, 但那是
// 官方 Cline 摘要格式(自研, 无 OmniRoute 对应), 两者用途不同, 不可混用。
// 这里必须用参考实现的原始字面量, 否则与上游模型的既有预期不一致。
const (
	// flattenToolCallPrefix 照抄 :18 `export const TOOL_CALL_PREFIX = "[Called tools: ";`
	flattenToolCallPrefix = "[Called tools: "
	// flattenToolResultPrefix 照抄 :19 `export const TOOL_RESULT_PREFIX = "[Tool result: ";`
	flattenToolResultPrefix = "[Tool result: "
)

// flattenExtractTextContent 照抄 geminiHelper.ts:284-294 extractTextContent。
//
//	export function extractTextContent(content: unknown): string {
//	  if (typeof content === "string") return content;
//	  if (Array.isArray(content)) {
//	    return content
//	      .map((item) => toRecord(item))
//	      .filter((c) => c.type === "text")
//	      .map((c) => (typeof c.text === "string" ? c.text : ""))
//	      .join("");                    // ← 空串连接, 不是 "\n"
//	  }
//	  return "";
//	}
//
// ⚠ 与包内既有的 msgText (compact.go:81) **不同**: msgText 用 "\n" 连接,
// 这里是 ""。照抄必须保留 "" —— 该结果会拼进 "[Called tools: ...]" 里,
// 换行会破坏单行语义。因此独立实现, 不复用 msgText。
//
// 另一个差异: msgText 不过滤 block 的 type; 这里只收 type=="text" 的块,
// tool_use / tool_result 等块不参与文本提取。
func flattenExtractTextContent(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []any:
		var sb strings.Builder
		for _, item := range c {
			block, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if block["type"] != "text" {
				continue
			}
			if t, ok := block["text"].(string); ok {
				sb.WriteString(t)
			}
		}
		return sb.String()
	default:
		return ""
	}
}

// flattenToolHistory 照抄 flattenToolHistory.ts:46-116。
//
// 纯函数, 不改动输入 (原注释 :12 "Pure function. Does not mutate input.") ——
// Go 侧同样通过**新建 map** 保证, 绝不就地改写入参。
//
// 四条分支逐条对应:
//
//  1. :55-63  role=tool / function → 转成 assistant 散文
//     content = "[Tool result: " + text + "]"
//  2. :66-79  role=assistant 且带 tool_calls → 摘掉 tool_calls, 把工具名写进正文
//     content = base + ("\n" if base) + "[Called tools: " + names + "]"
//  3. :82-111 content 是数组且含 tool_use / tool_result 块 → 摊平为文本
//     text 块用 "\n" 连接; 工具名与工具结果各自追加一行
//  4. :113    其余消息原样保留 (同一个 map 引用)
func flattenToolHistory(messages []any) []any {
	out := make([]any, 0, len(messages))
	for _, raw := range messages {
		msg, ok := raw.(map[string]any)
		if !ok {
			// :51 `if (!isMessage(raw)) continue;` —— nil / 非对象一律跳过
			continue
		}

		role, _ := msg["role"].(string)

		// 分支 1 (:55-63): OpenAI tool / function role -> assistant prose
		if role == "tool" || role == "function" {
			text := flattenExtractTextContent(msg["content"])
			if text == "" {
				// :57 `extractTextContent(msg.content) || String(msg.content ?? "")`
				text = stringifyContentFallback(msg["content"])
			}
			out = append(out, map[string]any{
				"role":    "assistant",
				"content": flattenToolResultPrefix + text + "]",
			})
			continue
		}

		// 分支 2 (:66-79): assistant 带结构化 tool_calls -> 摊平成散文
		if role == "assistant" {
			if toolCalls, ok := msg["tool_calls"].([]any); ok {
				names := make([]string, 0, len(toolCalls))
				for _, c := range toolCalls {
					names = append(names, toolCallName(c))
				}
				// :67 `const { tool_calls, ...rest } = msg;` —— 去掉 tool_calls 字段
				rest := shallowCopyRecord(msg)
				delete(rest, "tool_calls")
				base := flattenExtractTextContent(rest["content"])
				if base == "" {
					// :72-73 `|| (typeof rest.content === "string" ? rest.content : "")`
					if s, ok := rest["content"].(string); ok {
						base = s
					}
				}
				// :76 `${base}${base ? "\n" : ""}${TOOL_CALL_PREFIX}${names}]`
				content := base
				if base != "" {
					content += "\n"
				}
				content += flattenToolCallPrefix + strings.Join(names, ", ") + "]"
				rest["content"] = content
				out = append(out, rest)
				continue
			}
		}

		// 分支 3 (:82-111): Anthropic 风格的 content 数组含 tool_use / tool_result
		if blocks, ok := msg["content"].([]any); ok {
			hasToolUse := false
			hasToolResult := false
			for _, b := range blocks {
				bm, ok := b.(map[string]any)
				if !ok {
					continue
				}
				switch bm["type"] {
				case "tool_use":
					hasToolUse = true
				case "tool_result":
					hasToolResult = true
				}
			}
			if hasToolUse || hasToolResult {
				textParts := []string{}
				toolNames := []string{}
				toolResults := []string{}
				for _, b := range blocks {
					bm, ok := b.(map[string]any)
					if !ok {
						continue
					}
					switch bm["type"] {
					case "text":
						if t, ok := bm["text"].(string); ok {
							textParts = append(textParts, t)
						}
					case "tool_use":
						// :94 `toolNames.push(block.name || "tool");`
						name, _ := bm["name"].(string)
						if name == "" {
							name = "tool"
						}
						toolNames = append(toolNames, name)
					case "tool_result":
						// :96-98
						tr := flattenExtractTextContent(bm["content"])
						if tr == "" {
							tr = stringifyContentFallback(bm["content"])
						}
						toolResults = append(toolResults, tr)
					}
				}
				// :101-107
				newContent := strings.Join(textParts, "\n")
				if len(toolNames) > 0 {
					if newContent != "" {
						newContent += "\n"
					}
					newContent += flattenToolCallPrefix + strings.Join(toolNames, ", ") + "]"
				}
				if len(toolResults) > 0 {
					if newContent != "" {
						newContent += "\n"
					}
					newContent += flattenToolResultPrefix + strings.Join(toolResults, "\n") + "]"
				}
				replaced := shallowCopyRecord(msg)
				replaced["content"] = newContent
				out = append(out, replaced)
				continue
			}
		}

		// 分支 4 (:113): 原样保留
		out = append(out, msg)
	}
	return out
}

// toolCallName 对应 `c?.function?.name || c?.name || "tool"` (:69)。
func toolCallName(c any) string {
	cm, ok := c.(map[string]any)
	if !ok {
		return "tool"
	}
	if fn, ok := cm["function"].(map[string]any); ok {
		if name, ok := fn["name"].(string); ok && name != "" {
			return name
		}
	}
	if name, ok := cm["name"].(string); ok && name != "" {
		return name
	}
	return "tool"
}

// stringifyContentFallback 对应 JS 的 `String(msg.content ?? "")`。
//
// JS 的 String() 语义: null/undefined → ""(经 ?? 兜底), 标量 → 字面量,
// 对象 → "[object Object]"。
//
// 实际数据里 content 到这一步只剩 nil / 标量(字符串与数组已被
// flattenExtractTextContent 处理干净)。标量按 JS 语义输出; 若真出现
// 对象/数组, JS 会退化成 "[object Object]" 把信息全丢掉, 这里改用
// JSON 序列化 —— 这是**有意的、已标注的偏离**, 理由是丢信息会让模型
// 看到无意义的占位符, 反而破坏上下文。
func stringifyContentFallback(content any) string {
	switch c := content.(type) {
	case nil:
		return ""
	case string:
		return c
	case bool:
		if c {
			return "true"
		}
		return "false"
	}
	// 数字 / 其他标量: JSON 编码结果与 JS String() 一致
	if b, err := json.Marshal(content); err == nil {
		s := string(b)
		// JSON 会给字符串加引号, 但字符串已在上面的 case 处理, 到这里不可能是 string
		return s
	}
	return ""
}
