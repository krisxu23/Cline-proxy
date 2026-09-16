package app

// tool_adjacency.go —— 上游发送前的消息序列守卫。
//
// 照抄 OmniRoute services/contextManager.ts:829-1000（三个导出函数）。
//
// 为什么单独成文件而不并进 compact.go: 这三个函数的调用时机不同 ——
// `fixToolPairs` 是"修配对"(可以在任何阶段跑), 而这三个是**上游发送路径专用**
// 的守卫, 语义上要求"已经跑完 fixToolPairs", 且 `stripTrailingAssistant*` 只动
// 最后一条消息。混在一起容易让人误以为可以随便调用。

// fixToolAdjacency 照抄 contextManager.ts:829-896。
//
// 与 fixToolPairs 的区别（原注释 :821-828）:
//
//	fixToolPairs 只保证"每个 tool_use 在某处有对应 tool_result";
//	本函数进一步要求这个 tool_result 出现在 **紧接着的下一条消息**里。
//
// Claude 只认后者的严格形态, 否则报
// `messages.N: tool_use ids were found without tool_result blocks immediately after`。
// 注意判据只有"下一条消息", 不看更远处 —— 那是刻意的: 更远处的结果
// 对 Claude 而言等于不存在。
//
// 早退: `messages.length <= 1` 时无"下一条"可言, 原样返回(:830)。
func fixToolAdjacency(messages []any) []any {
	if len(messages) <= 1 {
		return messages
	}

	result := make([]any, 0, len(messages))
	for i := 0; i < len(messages); i++ {
		msg, ok := messages[i].(map[string]any)
		if !ok {
			result = append(result, messages[i])
			continue
		}
		// :838 `if (msg.role !== "assistant" || !nextMsg)`
		if strField(msg, "role") != "assistant" || i+1 >= len(messages) {
			result = append(result, messages[i])
			continue
		}
		nextMsg, _ := messages[i+1].(map[string]any)

		// :843-854 只从**下一条**消息收集 tool_result id
		nextToolResultIDs := map[string]bool{}
		if nextMsg != nil {
			if strField(nextMsg, "role") == "tool" {
				if id, _ := nextMsg["tool_call_id"].(string); id != "" {
					nextToolResultIDs[id] = true
				}
			}
			if strField(nextMsg, "role") == "user" {
				if blocks, ok := nextMsg["content"].([]any); ok {
					for _, bi := range blocks {
						b, ok := bi.(map[string]any)
						if !ok || strField(b, "type") != "tool_result" {
							continue
						}
						if id, _ := b["tool_use_id"].(string); id != "" {
							nextToolResultIDs[id] = true
						}
					}
				}
			}
		}

		// :857 浅拷贝, 仅在真有改动时替换(:881-892)
		modified := false
		newMsg := shallowCopyRecord(msg)

		// :860-868 Claude 形态: content 数组里的 tool_use 块
		if blocks, ok := newMsg["content"].([]any); ok {
			live := make([]any, 0, len(blocks))
			for _, bi := range blocks {
				b, _ := bi.(map[string]any)
				// :862 `block.type !== "tool_use" || !block.id || nextToolResultIds.has(block.id)`
				if b == nil || strField(b, "type") != "tool_use" {
					live = append(live, bi)
					continue
				}
				id, _ := b["id"].(string)
				if id == "" || nextToolResultIDs[id] {
					live = append(live, bi)
				}
			}
			if len(live) != len(blocks) {
				newMsg["content"] = live
				modified = true
			}
		}

		// :871-879 OpenAI 形态: tool_calls 数组（与 content 独立处理）
		if tcs, ok := newMsg["tool_calls"].([]any); ok {
			live := make([]any, 0, len(tcs))
			for _, tc := range tcs {
				tcm, _ := tc.(map[string]any)
				id, _ := tcm["id"].(string)
				if id == "" || nextToolResultIDs[id] {
					live = append(live, tc)
				}
			}
			if len(live) != len(tcs) {
				newMsg["tool_calls"] = live
				modified = true
			}
		}

		if !modified {
			result = append(result, messages[i])
			continue
		}
		// :882-888 修剪后既无内容也无调用 -> 整条丢弃
		if contentEmpty(newMsg) {
			hasToolCalls := false
			if tcs, ok := newMsg["tool_calls"].([]any); ok && len(tcs) > 0 {
				hasToolCalls = true
			}
			if blocks, ok := newMsg["content"].([]any); ok && len(blocks) > 0 {
				hasToolCalls = true
			}
			if !hasToolCalls {
				continue
			}
		}
		result = append(result, newMsg)
	}
	return result
}

// stripTrailingAssistantOrphanToolUse 照抄 contextManager.ts:919-964。
//
// 为什么必须单独有这一步（原注释 :898-917 逐字保留其逻辑）:
//
//	fixToolPairs **故意**保留最后一条 assistant 里未配对的 tool_use ——
//	因为上下文裁剪时客户端还在等对应的 tool_result, 在那里删会丢状态。
//	但到了"真正发给上游"这一刻, 请求体必须以 user 回合结尾; 结尾若是
//	`assistant(tool_use)`, Anthropic 会报
//	  `messages.N: tool_use ids were found without tool_result blocks immediately after: toolu_...`
//
// 即: 同一个"保留"动作, 在裁剪阶段是对的, 在发送阶段是错的 —— 两处的判据不同,
// 不能合并成一个函数。
//
// 幂等性（原注释 :916-917）: 干净的对话(以 user 结尾 / 以纯文本 assistant 结尾)
// 调用本函数不产生任何变化。
func stripTrailingAssistantOrphanToolUse(messages []any) []any {
	if len(messages) == 0 {
		return messages
	}
	lastIdx := len(messages) - 1
	last, ok := messages[lastIdx].(map[string]any)
	// :926 `if (!last || last.role !== "assistant") return messages;`
	if !ok || strField(last, "role") != "assistant" {
		return messages
	}

	modified := false
	newLast := shallowCopyRecord(last)

	// :931-939 结尾的 tool_calls 全删 —— 按定义它们不可能有配对
	if tcs, ok := newLast["tool_calls"].([]any); ok && len(tcs) > 0 {
		newLast["tool_calls"] = []any{}
		modified = true
	}

	// :941-949 结尾的 tool_use 块全删
	if blocks, ok := newLast["content"].([]any); ok {
		live := make([]any, 0, len(blocks))
		for _, bi := range blocks {
			b, _ := bi.(map[string]any)
			if b != nil && strField(b, "type") == "tool_use" {
				continue
			}
			live = append(live, bi)
		}
		if len(live) != len(blocks) {
			newLast["content"] = live
			modified = true
		}
	}

	// :951 `if (!modified) return messages;` —— 无改动时返回**原切片**(幂等)
	if !modified {
		return messages
	}

	// :953-962 删空则整条丢弃
	hasContent := !contentEmpty(newLast)
	hasToolCalls := false
	if tcs, ok := newLast["tool_calls"].([]any); ok && len(tcs) > 0 {
		hasToolCalls = true
	}
	if blocks, ok := newLast["content"].([]any); ok && len(blocks) > 0 {
		hasContent = true
	}

	result := append([]any{}, messages[:lastIdx]...)
	if hasContent || hasToolCalls {
		result = append(result, newLast)
	}
	return result
}

// providersRequiringUserLastMessage 照抄 contextManager.ts:972。
//
// 原注释: Mistral 拒绝以 assistant 结尾的纯文本回合, 报
// `Expected last role User or Tool … but got assistant`（#3396）。
// Anthropic/OpenAI 则允许(表示"从这里继续")。
var providersRequiringUserLastMessage = map[string]bool{"mistral": true}

// stripTrailingAssistantForProvider 照抄 contextManager.ts:981-1000。
//
// 调用顺序（原注释 :978-979）: 必须在 `stripTrailingAssistantOrphanToolUse`
// **之后** —— 先把 tool_use 孤儿清掉, 这一步才能正确判断"是否只剩纯文本"。
func stripTrailingAssistantForProvider(messages []any, provider string) []any {
	if !providersRequiringUserLastMessage[provider] {
		return messages
	}
	if len(messages) == 0 {
		return messages
	}
	last, ok := messages[len(messages)-1].(map[string]any)
	if !ok || strField(last, "role") != "assistant" {
		return messages
	}

	// :991-997 只处理"没有 tool_use/tool_calls"的纯文本尾巴
	if blocks, ok := last["content"].([]any); ok {
		for _, bi := range blocks {
			if b, ok := bi.(map[string]any); ok && strField(b, "type") == "tool_use" {
				return messages
			}
		}
	}
	if tcs, ok := last["tool_calls"].([]any); ok && len(tcs) > 0 {
		return messages
	}

	return append([]any{}, messages[:len(messages)-1]...)
}
