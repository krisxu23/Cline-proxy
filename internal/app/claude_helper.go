package app

import "strings"

// 逐字照抄 OmniRoute open-sse/translator/helpers/claudeHelper.ts:82-156。
//
// 参考实现原注释:
//
//	// Check if message has valid non-empty content
//	export function hasValidContent(msg: ClaudeMessage): boolean { ... }
//
//	// Move tool_result blocks out of assistant messages into the preceding user
//	// turn. Anthropic 400s on tool_result inside assistant. Drop tool_results
//	// whose tool_use_id has not been emitted by an earlier assistant turn —
//	// keeping them just shifts the 400 to "unexpected tool_use_id". See #2815.
//	export function splitMisplacedToolResults(messages: ClaudeMessage[]): ClaudeMessage[] { ... }
//
// # 为什么这两条对本网关重要
//
// 客户端的 agent 历史里，`tool_result` 块**本该**只出现在 user 回合。但 Codex /
// OpenCode / Claude Code 一类客户端在续接、压缩、重放历史时会把它写进 assistant
// 回合 —— Anthropic 对此直接 400。而 400 在 agent 客户端里常常只表现为
// **任务无声中断**（与用户报的现象一致）。
//
// `splitMisplacedToolResults` 把这类块搬回前一条 user；搬不进去（前一条不是
// user，或 content 不是数组）就新建一条 user。若某个 tool_result 的
// tool_use_id 从未被更早的 assistant 回合发出过，则**丢弃它** —— 留着只会把
// 400 从 "tool_result in assistant" 换成 "unexpected tool_use_id"（#2815）。
//
// # Go 侧语义对齐要点
//
//   - **in-place 语义**: 参考实现建 `out` 新数组但**复用消息对象引用**；
//     仅在需要合并/裁剪时才 `{...msg}` 浅拷贝。Go 侧同样: 未改动的消息原样
//     放进新切片（浅拷贝语义等价——切片元素是 map 引用）。
//   - **`out[out.length - 1]` 就地替换**: 合并时替换掉 out 的最后一条,
//     而不是修改原对象。这保证调用方持有的原消息对象不被就地改写。
//   - **id 累积时机**: 只在「assistant 且 content 是数组」的路径上记录
//     tool_use id —— 包括「无 tool_result」分支与「有 tool_result」后剩余的
//     分支。user 消息与 content 非数组的 assistant 都**不**记录。
//   - **`remaining.length > 0` 才推回**: 只带 tool_result 的 assistant 整条丢弃。
//   - hasValidContent 的 `#7777` 分支: 纯媒体( image / document )的 user 回合
//     是**真实内容**，不得因"没有文本"而被判空删掉 —— 那等于静默丢弃视觉输入。
//     已逐条用探针锁定 17 个判定的权威值。

// hasValidContent 照抄 claudeHelper.ts:82-101。
//
// 参考实现原注释: `// Check if message has valid non-empty content`
//
// 判定顺序与短路语义逐条对位:
//
//	字符串 → trim 后非空
//	数组   → 任一块满足:
//	           text             且 text.trim() 非空
//	           thinking         且 thinking.trim() 非空
//	           redacted_thinking 且 data 是非空字符串且 trim 非空
//	           tool_use / tool_result           → 恒真
//	           image / document  (#7777)        → 恒真
//	其余(数字 / null / 非上述类型) → false
func hasValidContent(msg map[string]any) bool {
	if msg == nil {
		return false
	}
	// :83 `if (typeof msg.content === "string" && msg.content.trim()) return true;`
	if s, ok := msg["content"].(string); ok {
		if strings.TrimSpace(s) != "" {
			return true
		}
	}
	// :84 `if (Array.isArray(msg.content)) {`
	if blocks, ok := msg["content"].([]any); ok {
		// :85 `return msg.content.some((block) => ...);`
		for _, blockRaw := range blocks {
			block, ok := blockRaw.(map[string]any)
			if !ok {
				continue
			}
			switch blockType(block) {
			case "text":
				// `(block.type === "text" && block.text?.trim())`
				if s, ok := block["text"].(string); ok && strings.TrimSpace(s) != "" {
					return true
				}
			case "thinking":
				// `(block.type === "thinking" && block.thinking?.trim())`
				if s, ok := block["thinking"].(string); ok && strings.TrimSpace(s) != "" {
					return true
				}
			case "redacted_thinking":
				// `(block.type === "redacted_thinking" && typeof block.data === "string"
				//  && block.data.trim())`
				if s, ok := block["data"].(string); ok && strings.TrimSpace(s) != "" {
					return true
				}
			case "tool_use", "tool_result":
				// `block.type === "tool_use" || block.type === "tool_result"`
				return true
			case "image", "document":
				// :94-96 `// #7777: media-only user turns are real content — dropping them
				//          silently deletes vision input on the CC bridge / Claude paths.`
				return true
			}
		}
	}
	// :100 `return false;`
	return false
}

// blockType 取块的 `type` 字段 (TS 里的 `block.type`)。
// 非字符串 / 缺失 → 空串（不匹配任何 case，等价于落到 default）。
func blockType(block map[string]any) string {
	s, _ := block["type"].(string)
	return s
}

// splitMisplacedToolResults 照抄 claudeHelper.ts:103-156。
//
// 参考实现原注释 (逐字):
//
//	// Move tool_result blocks out of assistant messages into the preceding user
//	// turn. Anthropic 400s on tool_result inside assistant. Drop tool_results
//	// whose tool_use_id has not been emitted by an earlier assistant turn —
//	// keeping them just shifts the 400 to "unexpected tool_use_id". See #2815.
//
// 返回的切片与入参共享消息对象（未改动的那部分），改动过的消息是浅拷贝副本。
func splitMisplacedToolResults(messages []any) []any {
	// :108 `if (!Array.isArray(messages) || messages.length === 0) return messages;`
	//
	// 注意 JS 的 `messages.length === 0` 对空数组成立, 返回的是**空数组**
	// (不是 null)。Go 侧 nil 切片序列化成 `null`, 与参考实现返回 `[]` 不等价,
	// 故这里归一成非 nil 空切片。
	if len(messages) == 0 {
		return []any{}
	}

	// :110 `const out: ClaudeMessage[] = [];`
	out := make([]any, 0, len(messages))
	// :111 `const seenToolUseIds = new Set<string>();`
	seenToolUseIds := make(map[string]bool)

	// :113-119 `const recordToolUseIds = (blocks: ClaudeContentBlock[]) => {
	//            for (const b of blocks) {
	//              if (b?.type === "tool_use" && typeof b.id === "string") {
	//                seenToolUseIds.add(b.id); } } };`
	recordToolUseIds := func(blocks []any) {
		for _, bRaw := range blocks {
			b, ok := bRaw.(map[string]any)
			if !ok {
				continue
			}
			if blockType(b) != "tool_use" {
				continue
			}
			if id, ok := b["id"].(string); ok {
				seenToolUseIds[id] = true
			}
		}
	}

	// :121 `for (const msg of messages) {`
	for _, msg := range messages {
		msgMap, isMap := msg.(map[string]any)
		// :122 `if (msg.role !== "assistant" || !Array.isArray(msg.content)) {
		//         out.push(msg); continue; }`
		// 非 map / 非 assistant / content 非数组 → 原样推入, 不记录 id。
		if !isMap {
			out = append(out, msg)
			continue
		}
		if role, _ := msgMap["role"].(string); role != "assistant" {
			out = append(out, msg)
			continue
		}
		content, ok := msgMap["content"].([]any)
		if !ok {
			out = append(out, msg)
			continue
		}

		// :127 `const toolResults = msg.content.filter((b) => b?.type === "tool_result");`
		toolResults := make([]any, 0, len(content))
		for _, bRaw := range content {
			if b, ok := bRaw.(map[string]any); ok && blockType(b) == "tool_result" {
				toolResults = append(toolResults, bRaw)
			}
		}
		// :128 `if (toolResults.length === 0) { out.push(msg); recordToolUseIds(msg.content); continue; }`
		if len(toolResults) == 0 {
			out = append(out, msg)
			recordToolUseIds(content)
			continue
		}

		// :133-135 `const validToolResults = toolResults.filter((b) =>
		//             typeof b?.tool_use_id === "string" && seenToolUseIds.has(b.tool_use_id));`
		validToolResults := make([]any, 0, len(toolResults))
		for _, bRaw := range toolResults {
			b, _ := bRaw.(map[string]any)
			if b == nil {
				continue
			}
			id, ok := b["tool_use_id"].(string)
			if !ok {
				continue
			}
			if seenToolUseIds[id] {
				validToolResults = append(validToolResults, bRaw)
			}
		}
		// :136 `const remaining = msg.content.filter((b) => b?.type !== "tool_result");`
		remaining := make([]any, 0, len(content))
		for _, bRaw := range content {
			if b, ok := bRaw.(map[string]any); ok && blockType(b) == "tool_result" {
				continue
			}
			remaining = append(remaining, bRaw)
		}

		// :138 `if (validToolResults.length > 0) {`
		if len(validToolResults) > 0 {
			// :139 `const prev = out[out.length - 1];`
			var prev map[string]any
			if len(out) > 0 {
				prev, _ = out[len(out)-1].(map[string]any)
			}
			// :140 `if (prev && prev.role === "user" && Array.isArray(prev.content)) {`
			if prev != nil {
				if role, _ := prev["role"].(string); role == "user" {
					if prevContent, ok := prev["content"].([]any); ok {
						// :141 `out[out.length - 1] = { ...prev, content: [...prev.content, ...validToolResults] };`
						merged := make([]any, 0, len(prevContent)+len(validToolResults))
						merged = append(merged, prevContent...)
						merged = append(merged, validToolResults...)
						replacement := shallowCopyStringAny(prev)
						replacement["content"] = merged
						out[len(out)-1] = replacement
					} else {
						// :143 `} else { out.push({ role: "user", content: validToolResults }); }`
						out = append(out, map[string]any{"role": "user", "content": validToolResults})
					}
				} else {
					out = append(out, map[string]any{"role": "user", "content": validToolResults})
				}
			} else {
				out = append(out, map[string]any{"role": "user", "content": validToolResults})
			}
		}

		// :149 `// Drop the assistant message entirely if only tool_result blocks remained.`
		// :150 `if (remaining.length > 0) {`
		if len(remaining) > 0 {
			// :151 `out.push({ ...msg, content: remaining });`
			replacement := shallowCopyStringAny(msgMap)
			replacement["content"] = remaining
			out = append(out, replacement)
			// :152 `recordToolUseIds(remaining);`
			recordToolUseIds(remaining)
		}
	}

	// :155 `return out;`
	return out
}

// shallowCopyStringAny 复刻 JS 的 `{ ...obj }` 浅拷贝。
func shallowCopyStringAny(src map[string]any) map[string]any {
	dst := make(map[string]any, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}
