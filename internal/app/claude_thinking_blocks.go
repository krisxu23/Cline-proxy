package app

// 逐字照抄 OmniRoute open-sse/translator/helpers/claudeHelper.ts:546-719
// （prepareClaudeRequest 的 Pass 2 thinking-block 处理段）+ 其依赖常量:
//
//	open-sse/config/defaultThinkingSignature.ts:2-3  → defaultThinkingClaudeSignature
//	open-sse/utils/reasoningPlaceholder.ts:6         → nonAnthropicThinkingPlaceholder (已在 zen_reasoning.go)
//
// # 参考实现原注释（逐字，claudeHelper.ts:546-567）
//
//	// Handle thinking blocks for Anthropic-shape endpoints.
//	// prepareClaudeRequest is only invoked when targetFormat === claude
//	// (translator/index.ts:165-168), so any provider that lands here has
//	// a Claude-format upstream: claude native, anthropic-compatible-*,
//	// kimi-coding (api.kimi.com/coding/v1/messages), glmt, zai, etc.
//	// All of these enforce the same body-shape contract for thinking mode:
//	// when body.thinking.type === "enabled" and an assistant turn contains
//	// a tool_use, the same content[] must include a thinking (or
//	// redacted_thinking) block emitted before the tool_use. Without it,
//	// the upstream rejects with errors like:
//	//   "thinking is enabled but reasoning_content is missing in
//	//    assistant tool call message at index N"  (kimi-coding)
//	//   "Invalid signature in thinking block"     (claude native, on
//	//                                              cross-provider replay)
//	// Guard: never modify EXISTING thinking blocks in the latest assistant
//	// message when sending to an Anthropic-native upstream. Anthropic returns
//	// 400 "blocks in the latest assistant message cannot be modified" if any
//	// field changes. Injecting a NEW thinking block (when none exists) is fine.
//	// Older assistant messages can still be rewritten.
//	// For non-Anthropic providers: only the text replacement is skipped
//	// for the latest assistant (if it already has non-empty thinking text);
//	// field cleanup (signature strip, type normalization) still runs.
//
// # 为什么这条对本网关重要
//
// 本网关的 anthropic 出站分支正是"Claude 形态上游"：claude 原生、各类
// anthropic-compatible 中转。这些上游在 `thinking.type === "enabled"` 时执行**严格
// 形态契约**：assistant 回合只要含 tool_use，同一 content[] 里就必须有一个
// thinking / redacted_thinking 块排在 tool_use 之前。
//
// 客户端（Codex / Cline / Claude Code 经 cc-switch）重放历史时常常把 thinking 块
// 丢掉（或只带一个空 signature 的合成块）。上游随即 400：
//
//   - `"thinking is enabled but reasoning_content is missing in assistant tool
//      call message at index N"`（kimi-coding 一类）
//   - `"Invalid signature in thinking block"`（claude 原生，跨 provider 重放）
//
// 而 400 在 agent 客户端里常常只表现为**任务无声中断** —— 与用户报的现象一致。
//
// # 关键语义点
//
//   - **最新 assistant 回合的既有 thinking 块不得改写**（Anthropic 400
//     "blocks in the latest assistant message cannot be modified"）—— 但**仅当**
//     它带的是**真实签名**（redacted_thinking.data 非空 / thinking.signature 非空）。
//     #6953: 由非 Anthropic 腿合成的**空签名**块不是真签名，透传上去必 400，
//     必须走与旧回合相同的净化路径。
//   - **三种 provider 形态**决定块形状（:640/:647/:652）：
//     ① kimi-coding → 一律 `thinking { thinking }`，删 data/signature；
//     ② supportsRedactedThinking → 一律 `redacted_thinking { data: <默认签名> }`；
//     ③ 其余 → `thinking { thinking: <文本 or 占位符> }`。
//   - **前置 thinking 块**（:693-719）：thinkingEnabled && 无 thinking && 有 tool_use
//     时 `unshift` 一个新块，形状同样按上面三分支选。
//   - `thinkingBlockIdx` 只在**遍历到 thinking 块**时自增（:683），用于把
//     第 N 个 thinking 块与第 N 个 tool_use id 配对查 reasoningCache（:664）。

// defaultThinkingClaudeSignature 照抄 config/defaultThinkingSignature.ts:2-3。
//
// 参考实现原注释: `// Default signature for thinking mode when no signature from thinkingStore`
const defaultThinkingClaudeSignature = "EpwGCkYIChgCKkCzVUuRrg7CcglSUWEef4rH6o35g9UYS8ZPe0/VomQTBsFx6sttYNj5l8GqgW6ejuHyYqpFToxIbZl0bw17l5dJEgzCnqDO0Z8fRlMrNgsaDLS1cnCjC53KBqE0CCIwAADQdo1eO+7qPAmo8J4WR3JPmr92S97kmvr5K1iPMiOpkZNj8mEXW8uzBoOJs/9ZKoMFiqHJ3UObwaJDqFOW70E9oCwDoc6jesaWVAEdN5vWfKMpIkjFJjECdjIdkxyJNJ8Ib8yXVal3qwE7uThoPRqSZDdHB5mmwPEjWE/90cSYCbtX2YsJki1265CabBb8/QEkODXg4kgRrL+c8e8rRXz/dr1RswvaPuzEdGKHRNi9UooNUeOK4/ebx1KkP9YZttyohN9GWqlts36kOoW0Cfie/ABDgF9g534BPth/sstxDM6d79QlRmh6NxizyTF74DXJI34u0M4tTRchqE5pAq85SgdJaa+dix1yJPMji8m6nZkwJbscJb9rdc2MKyKWjz8QL2+rTSSuZ2F1k1qSsW0xNcI7qLcI12Vncfn/VqY6YOIZy/saZBR0ezXvN6g+UYbuIdyVg7AyIFZt3nbrO7/kmOEb2VKzygwklHGEIJHfFgMpH3JSrAzbZIowVHOF7VaJ+KXRFDCFin7hHTOiOsdg+1ij1mML9Z/x/9CP4b7OUcaQm1llDZPSHc6rZMNL3DdB+fW5YfmNgKU35S+7AMtA10nVILzDAk1UV4T2K9Do09JlI6rjOs9UuULlIN2Z0eE8YTlANR6uQcw7lMcdfqYE8tke4rDKc2dDiaS5vVe45VewICNpdXGN11yw8QqH7p27CR1HtN30e0tHXOR3bIwWk/Yb6O5fTaKG6Ri8e5ZCPvdD9HqepVi188nM0iTjJqL58F3ni04ECIhcbyaQWnuTes1Kw4CMwiZDLQkk8Hgz7HkUOf1btQTF/0nhD7ry0n0hAEg2PaDM3V6TjOjf4hEldRmeqERcQF1PfgKb6ZM12rlIIfUqKACczWJSzTV158+47HX36o0cgux6nFlv/DE+sEiRVxgB"

// isKimiCodingProvider 照抄 claudeHelper.ts:368
//
//	const isKimiCoding = provider === "kimi-coding" || provider === "kimi-coding-apikey";
func isKimiCodingProvider(provider string) bool {
	return provider == "kimi-coding" || provider == "kimi-coding-apikey"
}

// supportsPromptCachingForProvider 照抄 claudeHelper.ts:366-367
//
//	const supportsPromptCaching =
//	  provider === "claude" || provider?.startsWith?.("anthropic-compatible-");
func supportsPromptCachingForProvider(provider string) bool {
	return provider == "claude" || strings_HasPrefix(provider, "anthropic-compatible-")
}

//	isThinkingEnabledForLatestUser 照抄 claudeHelper.ts:494-497
//
//	const lastMessage = filtered[filtered.length - 1];
//	const lastMessageIsUser = lastMessage?.role === "user";
//	const thinkingEnabled = body.thinking?.type === "enabled" && lastMessageIsUser;
//
// ★ 两个条件都要：body 开了 thinking **且** 最后一条是 user 回合。
func isThinkingEnabledForLatestUser(body map[string]any, filtered []any) bool {
	if body == nil {
		return false
	}
	lastRole := ""
	if len(filtered) > 0 {
		if m, ok := filtered[len(filtered)-1].(map[string]any); ok {
			lastRole, _ = m["role"].(string)
		}
	}
	if lastRole != "user" {
		return false
	}
	// `body.thinking?.type === "enabled"` —— 可选链, 非对象时为 undefined。
	if th, ok := body["thinking"].(map[string]any); ok {
		if s, _ := th["type"].(string); s == "enabled" {
			return true
		}
	}
	return false
}

// latestAssistantIdx 照抄 claudeHelper.ts:521-527
//
//	let latestAssistantIndex = -1;
//	for (let k = filtered.length - 1; k >= 0; k--) {
//	  if (filtered[k]?.role === "assistant") { latestAssistantIndex = k; break; }
//	}
func latestAssistantIdx(filtered []any) int {
	for k := len(filtered) - 1; k >= 0; k-- {
		if m, ok := filtered[k].(map[string]any); ok {
			if r, _ := m["role"].(string); r == "assistant" {
				return k
			}
		}
	}
	return -1
}

// applyClaudeThinkingBlocks 照抄 claudeHelper.ts:546-719（Pass 2 的 thinking 段）。
//
// 入参:
//   - filtered: 已过 Pass 1 / 1.4 / 1.45 / 1.5 的 messages（**就地**改）
//   - body:     整个 params（读 thinking 开关）
//   - provider: 上游 provider id（已 normalize）
//   - modelTargetsClaude: 对位参考实现的 `getModelTargetFormat(provider, model) === "claude"`。
//     ★ 这一条**不能写死 true** —— 它决定 supportsRedactedThinking（:385），
//     进而决定块形状走 redacted_thinking 还是 plain thinking。
//     探针 T11–T13 实测: glmt + targetsClaude=false 时前置块是
//     `thinking { thinking: "(prior reasoning summary unavailable)" }`；
//     若写死 true 则会错误地产出 `redacted_thinking { data: <默认签名> }` ——
//     而 glmt 一类非 Anthropic 上游无法校验该签名 blob，正会 400
//     "Invalid signature in thinking block"。
//     调用方须按自己是否知道"该 provider/model 的上游是真 Anthropic Messages
//     端点"来传值；本网关的 anthropic 出站分支若无 per-model 目标格式表，
//     保守传 `supportsPromptCachingForProvider(provider)` 的结果（即只认
//     claude / anthropic-compatible-*）。
//   - lookupReasoning: reasoningCache 查询函数（参考实现的 lookupReasoning 对位）。
//     传 nil 表示本网关没有该缓存 —— 此时只用占位符兜底（与参考实现 cache miss 等价）。
//   - recordReplay: 命中缓存时调一次的计数器（参考实现的 recordReplay 对位）。可传 nil。
//
// 返回是否发生过改动（供调用方决定是否 log）。
func applyClaudeThinkingBlocks(
	filtered []any,
	body map[string]any,
	provider string,
	modelTargetsClaude bool,
	lookupReasoning func(string) (string, bool),
	recordReplay func(),
) bool {
	// :366-367 / :368 / :383-385
	supportsPromptCaching := supportsPromptCachingForProvider(provider)
	isKimiCoding := isKimiCodingProvider(provider)
	supportsRedactedThinking := !isKimiCoding && (supportsPromptCaching || modelTargetsClaude)

	thinkingEnabled := isThinkingEnabledForLatestUser(body, filtered)
	latestAssistantIndex := latestAssistantIdx(filtered)
	lastAssistantProcessed := false

	changed := false

	// :530 `for (let i = filtered.length - 1; i >= 0; i--) {`
	for i := len(filtered) - 1; i >= 0; i-- {
		msg, ok := filtered[i].(map[string]any)
		if !ok {
			continue
		}
		// :532 `const content = ensureMessageContentArray(msg);`
		//
		// 注意：ensureMessageContentArray 对"非空字符串"是**就地改写**成数组。
		// 本函数的调用点已在 reanchorClaudePromptCache 里做过同一件事的
		// 姊妹操作；此处按参考实现语义再调一次是幂等的。
		content := ensureMessageContentArray(msg)

		// :534 `if (msg.role === "assistant" && content.length > 0) {`
		if role, _ := msg["role"].(string); role != "assistant" || len(content) == 0 {
			continue
		}

		// :537-544 `if (!preserveCacheControl && supportsPromptCaching &&
		//             !lastAssistantProcessed && markMessageCacheControl(msg)) {
		//             lastAssistantProcessed = true; }`
		//
		// cache_control 的标记已在 reanchorClaudePromptCache 里完成；
		// 此处仅同步 lastAssistantProcessed 的语义（避免重复标记）。
		_ = lastAssistantProcessed

		// :568-574
		isLatestAssistant := i == latestAssistantIndex
		var latestThinkingBlocks []map[string]any
		if isLatestAssistant {
			for _, bRaw := range content {
				b, ok := bRaw.(map[string]any)
				if !ok {
					continue
				}
				bt := blockType(b)
				if bt == "thinking" || bt == "redacted_thinking" {
					latestThinkingBlocks = append(latestThinkingBlocks, b)
				}
			}
		}
		latestHasExistingThinking := len(latestThinkingBlocks) > 0

		// :583-588 `const latestHasGenuineThinkingSignature = latestThinkingBlocks.every(
		//             (b) => b.type === "redacted_thinking"
		//               ? typeof b.data === "string" && b.data.length > 0
		//               : typeof b.signature === "string" && b.signature.length > 0);`
		//
		// ★ JS 的 `every` 对**空数组返回 true** —— 空数组时该值为 true，
		//   但下面的 if 还要求 latestHasExistingThinking，故不会误触发。
		latestHasGenuineThinkingSignature := true
		for _, b := range latestThinkingBlocks {
			if blockType(b) == "redacted_thinking" {
				if s, ok := b["data"].(string); !ok || len(s) == 0 {
					latestHasGenuineThinkingSignature = false
				}
			} else {
				if s, ok := b["signature"].(string); !ok || len(s) == 0 {
					latestHasGenuineThinkingSignature = false
				}
			}
		}

		// :589-597 原样透传：最新 assistant 回合 + 有既有 thinking + 真签名 + 是
		// Anthropic 原生上游 → 整段 thinking 重写全部跳过。
		if latestHasExistingThinking && supportsRedactedThinking && latestHasGenuineThinkingSignature {
			continue
		}

		// :599-600
		hasToolUse := false
		hasThinking := false

		// :607-614 `const toolUseIds: string[] = [];
		//             if (!supportsRedactedThinking) {
		//               for (const block of content) {
		//                 if (block.type === "tool_use" && typeof block.id === "string")
		//                   toolUseIds.push(block.id); } };`
		var toolUseIds []string
		if !supportsRedactedThinking {
			for _, bRaw := range content {
				b, ok := bRaw.(map[string]any)
				if !ok {
					continue
				}
				if blockType(b) == "tool_use" {
					if id, ok := b["id"].(string); ok {
						toolUseIds = append(toolUseIds, id)
					}
				}
			}
		}

		// :637 `let thinkingBlockIdx = 0;`
		thinkingBlockIdx := 0

		// :638 `for (const block of content) {`
		for _, bRaw := range content {
			block, ok := bRaw.(map[string]any)
			if !ok {
				continue
			}
			bt := blockType(block)
			if bt == "thinking" || bt == "redacted_thinking" {
				changed = true
				if isKimiCoding {
					// :640-646
					if bt == "redacted_thinking" {
						block["type"] = "thinking"
						if s, ok := block["thinking"].(string); ok {
							block["thinking"] = s
						} else {
							block["thinking"] = ""
						}
					}
					delete(block, "data")
					delete(block, "signature")
				} else if supportsRedactedThinking {
					// :647-651
					block["type"] = "redacted_thinking"
					block["data"] = defaultThinkingClaudeSignature
					delete(block, "thinking")
					delete(block, "signature")
				} else {
					// :652-681
					existing := ""
					if s, ok := block["thinking"].(string); ok && len(s) > 0 {
						existing = s
					}
					text := existing
					// :662 `if (!text || !latestHasExistingThinking) {`
					if text == "" || !latestHasExistingThinking {
						if text == "" {
							// :664-671
							if thinkingBlockIdx < len(toolUseIds) {
								pairedToolUseId := toolUseIds[thinkingBlockIdx]
								if pairedToolUseId != "" && lookupReasoning != nil {
									if cached, ok := lookupReasoning(pairedToolUseId); ok && cached != "" {
										text = cached
										if recordReplay != nil {
											recordReplay()
										}
									}
								}
							}
						}
						block["type"] = "thinking"
						if text == "" {
							block["thinking"] = nonAnthropicThinkingPlaceholder
						} else {
							block["thinking"] = text
						}
					} else {
						// :675-678 `// latestHasExistingThinking + non-empty text:
						//             // preserve text, still clean up fields
						//             block.type = "thinking";`
						block["type"] = "thinking"
					}
					delete(block, "data")
					delete(block, "signature")
				}
				hasThinking = true
				thinkingBlockIdx++
			}
			if blockType(block) == "tool_use" {
				hasToolUse = true
			}
		}

		// :693-719 加前置 thinking 块。
		if thinkingEnabled && !hasThinking && hasToolUse {
			changed = true
			if supportsRedactedThinking {
				// :694-698
				block := map[string]any{
					"type": "redacted_thinking",
					"data": defaultThinkingClaudeSignature,
				}
				content = append([]any{block}, content...)
			} else if isKimiCoding {
				// :699-703
				block := map[string]any{
					"type":     "thinking",
					"thinking": "",
				}
				content = append([]any{block}, content...)
			} else {
				// :704-718
				text := ""
				if len(toolUseIds) > 0 {
					firstToolUseId := toolUseIds[0]
					if firstToolUseId != "" && lookupReasoning != nil {
						if cached, ok := lookupReasoning(firstToolUseId); ok && cached != "" {
							text = cached
							if recordReplay != nil {
								recordReplay()
							}
						}
					}
				}
				if text == "" {
					text = nonAnthropicThinkingPlaceholder
				}
				block := map[string]any{
					"type":     "thinking",
					"thinking": text,
				}
				content = append([]any{block}, content...)
			}
			msg["content"] = content
		}
	}

	return changed
}

// strings_HasPrefix 是 strings.HasPrefix 的本地转写，避免本文件额外 import
// （本仓库其它照抄文件同样避免为一个函数引入 import 块）。
func strings_HasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}
