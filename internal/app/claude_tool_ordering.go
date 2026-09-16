package app

import (
	"encoding/json"
	"strings"
)

// 逐字照抄 OmniRoute open-sse/translator/helpers/claudeHelper.ts:160-284 ——
// `fixToolUseOrdering`。
//
// 参考实现原注释（:160-162）:
//
//	// 2. Merge consecutive same-role messages
//	// 3. Reconcile tool_result blocks against the immediately previous tool_use message
//
// # 为什么这条对本网关重要
//
// Claude 协议对 tool_use / tool_result 有**位置**约束（不是语义约束）:
//
//  1. assistant 回合里 `tool_use` 之后**不能**再跟 text 块 —— Anthropic 只接受
//     "先 thinking/text，后 tool_use" 的形状。
//  2. `tool_result` 必须出现在**紧邻的下一条** user 回合里，且该回合里的
//     tool_result 必须**顺序对应**前一条 assistant 的 tool_use。
//  3. 客户端被压缩/重放过的历史里，tool_result 可能失去配对的 tool_use
//     （call 被丢了、output 还在），这时不能直接发结构化引用，否则 400。
//
// 这三条任一被违反，Anthropic 都直接 400 —— 而在 Codex / Cline / Claude Code
// 这类 agent 客户端里，400 常常只表现为**任务无声中断**（与用户报的现象一致）。
//
// # 与 fixToolAdjacency 的分工（两者都要跑，不重叠）
//
//   - `fixToolAdjacency`（tool_adjacency.go）照抄 services/contextManager.ts，
//     管的是"tool_result 是否**紧邻**其 tool_use"——偏**搬移/相邻**。
//   - `fixToolUseOrdering`（本文件）照抄 claudeHelper.ts，管的是
//     ① assistant 内 text 与 tool_use 的**顺序**（删 tool_use 之后的 text）、
//     ② **合并**相邻同 role 回合、③ 把失去配对的 tool_result **降级成文本**。
//
// fixToolUseOrdering 是 `fixToolUseOrderingClaudeShape` 的**接入级包装**，
// 负责补齐参考实现隐含依赖的前置条件：**messages 必须是 Claude 形态**。
//
// 判定"Claude 形态"的口径（取参考实现的必要条件，宁宽勿窄地**保守跳过**）：
//
//	没有任何一条消息带 OpenAI 形态的 `tool_calls` 字段。
//
// 依据: `openaiToClaudeRequest` 会把每个 `tool_calls[i]` 转成 `content[]` 里的
// `tool_use` 块（openai-to-claude.ts:660-675），转换后**不再保留** `tool_calls`
// 字段。所以"存在 tool_calls"== "尚未转成 Claude 形态"。此时本函数的 Pass 1/2/3
// 都建立在 Claude 块语义上，跑下去只会把 tool 调用信息抹掉（实测: 两条既有
// 接入测试变红 —— 见 tool_adjacency_test.go 的 trailing/adjacency 两例）。
//
// ★ 保守跳过而非"顺便转换"：转换是 `openaiToClaudeRequest` 的职责（含
//
//	`CLAUDE_OAUTH_TOOL_PREFIX` 前缀、`tryParseJSON` 参数解析等本函数不该复刻的
//	细节）。此处越界去猜只会引入第二套转换语义。
//
// 混合形态（部分消息是块、部分是 tool_calls）同样跳过 —— 那种历史本身就需要
// 上游兼容层处理，不是本函数该收拾的。
func fixToolUseOrdering(messages []any) []any {
	for _, raw := range messages {
		msg, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if _, has := msg["tool_calls"]; has {
			// OpenAI 形态 -> 不满足参考实现的前置条件，原样返回。
			return messages
		}
	}
	return fixToolUseOrderingClaudeShape(messages)
}

// fixToolUseOrderingClaudeShape 逐字照抄 claudeHelper.ts:160-284 的纯变换。
//
// # Go 侧语义对齐要点
//
//   - 参考实现会**就地改写** `msg.content`（`msg.content = newContent`）。Go 侧
//     同样就地改写 map（map 是引用类型），保持"调用方持有的对象被更新"这一
//     可观察行为一致。
//   - Pass 2 的 `merged.push({ role, content: [...content] })` 是**新建对象**——
//     首条同 role 消息会被换成浅拷贝，后续同 role 消息则**合并进**这条。
//     Go 侧照抄：`merged` 里存的是新建 map，合并时改它的 `content`。
//   - `merged` 恒为新切片（即使未改动），与参考实现一致。
//
// # ★ 前置条件（照抄的一部分，必须由调用方保证）
//
// 参考实现里本函数被 `prepareClaudeRequest` 调用，而后者只会在
// **`targetFormat === FORMATS.CLAUDE` 且翻译已全部完成之后**执行
// （translator/index.ts:567 是 translateRequest 的**最后**一步）。也就是说，
// 它见到的 messages **必然是 Claude 形态**：
//
//   - tool 调用在 `content[]` 的 `tool_use` 块里；
//   - **不存在** `tool_calls` 字段（`openaiToClaudeRequest` 已把它转成块，
//     见 translator/request/openai-to-claude.ts:660-675）。
//
// 本网关的出站咽喉拿到的是**客户端原始 body**，可能仍是 OpenAI 形态
// （assistant 带 `tool_calls`、content 为 null）。若不加区分地跑本函数：
//
//   - Pass 2 会把 `content: null` 包成 `[{type:"text",text:null}]`；
//   - 而本函数**不认识** `tool_calls`，于是那条 assistant 的 tool 调用信息
//     在块里凭空消失；
//   - 下游 `stripTrailingAssistantOrphanToolUse` 依赖 `tool_calls` 字段判定
//     "尾部孤儿调用"，此时它看不到该字段 -> 漏判。
//
// 因此对外入口是包装函数 `fixToolUseOrdering`（见下），由它把关
// "是否 Claude 形态"；本 `…ClaudeShape` 函数只做纯变换，与参考实现逐条对应。
func fixToolUseOrderingClaudeShape(messages []any) []any {
	if len(messages) == 0 {
		return messages
	}
	// :164-172 —— 单条消息且不含 tool_result 时直接返回（原引用）。
	if len(messages) == 1 {
		if m, ok := messages[0].(map[string]any); ok {
			if !hasBlockTypeInContent(m, "tool_result") {
				return messages
			}
		} else {
			return messages
		}
	}

	// ── Pass 1: assistant 里 tool_use 之后的 text 块删掉 ──
	//
	// :174-199。保留规则: 只留 thinking / redacted_thinking / tool_use,
	// 以及 **tool_use 之前**的 text。tool_use 之后的 text 一律丢弃。
	for _, raw := range messages {
		msg, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := msg["role"].(string); role != "assistant" {
			continue
		}
		content, ok := msg["content"].([]any)
		if !ok {
			continue
		}
		if !blocksContainType(content, "tool_use") {
			continue
		}
		newContent := make([]any, 0, len(content))
		foundToolUse := false
		for _, bRaw := range content {
			b, ok := bRaw.(map[string]any)
			if !ok {
				// 非对象块: 参考实现里 `block.type` 为 undefined, 三条分支都不成立,
				// 且 `!foundToolUse` 时会被 push。此处同样处理。
				if !foundToolUse {
					newContent = append(newContent, bRaw)
				}
				continue
			}
			t := blockType(b)
			switch {
			case t == "tool_use":
				foundToolUse = true
				newContent = append(newContent, bRaw)
			case t == "thinking" || t == "redacted_thinking":
				newContent = append(newContent, bRaw)
			case !foundToolUse:
				// tool_use 之前的 text 保留
				newContent = append(newContent, bRaw)
			}
			// tool_use 之后的 text: 丢弃
		}
		msg["content"] = newContent
	}

	// ── Pass 2: 合并相邻同 role 回合 ──
	//
	// :201-234。合并时把两边的 tool_result **全部提前**，其余内容保持相对顺序。
	//
	// ★ 首条同 role 消息用浅拷贝入 merged（`{ role, content: [...content] }`），
	//   而不是原对象 —— 这样后续合并不会污染调用方传入的对象。
	merged := make([]map[string]any, 0, len(messages))
	for _, raw := range messages {
		msg, ok := raw.(map[string]any)
		if !ok {
			// ★ 有意偏离参考实现（探针 F17 实测）。
			//
			// 参考实现在这里读 `msg.role`，对 `null` 条目会抛
			// `TypeError: Cannot read properties of null (reading 'role')` ——
			// 即参考实现在该输入上的行为是**崩溃**, 不构成可照抄的语义（属于
			// 参考实现的潜在缺陷, 其调用链上游已有 `Array.isArray(body.messages)`
			// 与逐条 `msg.content` 的隐式前提）。
			//
			// 本网关是长驻服务: 一个 panic 会打掉整个进程, 比一次 400 严重得多。
			// 故此处**跳过**非对象条目 —— 既保证不崩溃, 也不凭空造出一条
			// `role: null` 的假回合（那反而会招致新的 400）。
			continue
		}
		if len(merged) > 0 {
			last := merged[len(merged)-1]
			lastRole, _ := last["role"].(string)
			msgRole, _ := msg["role"].(string)
			if lastRole == msgRole {
				// 合并: tool_result 全部提前, 其余保持相对顺序。
				lastContent := asContentBlocks(last["content"])
				msgContent := asContentBlocks(msg["content"])
				toolResults := make([]any, 0)
				other := make([]any, 0)
				for _, bl := range append(append([]any{}, lastContent...), msgContent...) {
					if b, ok := bl.(map[string]any); ok && blockType(b) == "tool_result" {
						toolResults = append(toolResults, bl)
					} else {
						other = append(other, bl)
					}
				}
				last["content"] = append(toolResults, other...)
				continue
			}
		}
		// `merged.push({ role: msg.role, content: [...content] })`
		merged = append(merged, map[string]any{
			"role":    msg["role"],
			"content": append([]any{}, asContentBlocks(msg["content"])...),
		})
	}

	// ── Pass 3: 把失去配对的 tool_result 降级成文本 ──
	//
	// :236-281。参考实现原注释:
	//
	//	// Claude accepts tool_result only for a tool_use in the immediately previous
	//	// assistant message. Compacted cross-model history can retain an output after
	//	// dropping its call; keep that output as user text instead of sending an
	//	// invalid structured reference or discarding useful context.
	//
	// ★ 每条 user 回合只保留**一个**匹配的 tool_result（`!pairedById.has(toolUseId)`），
	//   重复的同样降级成文本。且输出顺序是「按 validIds 的顺序补齐 + 其余内容」——
	//   即使某个 tool_use 没有对应的 tool_result, 也会**补一个空 content 的占位**。
	for i := 0; i < len(merged); i++ {
		msg := merged[i]
		if role, _ := msg["role"].(string); role != "user" {
			continue
		}
		content, ok := msg["content"].([]any)
		if !ok {
			continue
		}
		// validIds 来自**紧邻上一条** assistant 的 tool_use id, 保持出现顺序。
		validIDs := make([]string, 0)
		validSet := make(map[string]bool)
		if i-1 >= 0 {
			prev := merged[i-1]
			if role, _ := prev["role"].(string); role == "assistant" {
				if pcontent, ok := prev["content"].([]any); ok {
					for _, bRaw := range pcontent {
						b, ok := bRaw.(map[string]any)
						if !ok {
							continue
						}
						if blockType(b) != "tool_use" {
							continue
						}
						if id, ok := b["id"].(string); ok && id != "" {
							if !validSet[id] {
								validSet[id] = true
								validIDs = append(validIDs, id)
							}
						}
					}
				}
			}
		}

		pairedByID := make(map[string]any)
		other := make([]any, 0, len(content))
		for _, bRaw := range content {
			b, ok := bRaw.(map[string]any)
			if !ok {
				other = append(other, bRaw)
				continue
			}
			if blockType(b) != "tool_result" {
				other = append(other, bRaw)
				continue
			}
			// `const toolUseId = typeof block.tool_use_id === "string" ? block.tool_use_id : ""`
			toolUseID, _ := b["tool_use_id"].(string)
			if validSet[toolUseID] && pairedByID[toolUseID] == nil {
				pairedByID[toolUseID] = bRaw
				continue
			}
			// 失去配对（或重复）-> 降级成文本
			//
			// `typeof block.content === "string" ? block.content : (JSON.stringify(block.content ?? "") ?? "")`
			var serialized string
			if s, ok := b["content"].(string); ok {
				serialized = s
			} else {
				serialized = jsJSONStringify(b["content"])
			}
			label := toolUseID
			if label == "" {
				label = "unknown"
			}
			other = append(other, map[string]any{
				"type": "text",
				"text": "[Unpaired tool result " + label + "]\n" + serialized,
			})
		}

		// `const pairedResults = [...validIds].map((id) => pairedById.get(id) ?? { type: "tool_result", tool_use_id: id, content: "" })`
		pairedResults := make([]any, 0, len(validIDs))
		for _, id := range validIDs {
			if b, ok := pairedByID[id]; ok {
				pairedResults = append(pairedResults, b)
				continue
			}
			pairedResults = append(pairedResults, map[string]any{
				"type":        "tool_result",
				"tool_use_id": id,
				"content":     "",
			})
		}
		msg["content"] = append(pairedResults, other...)
	}

	// merged 是 []map[string]any, 参考实现的返回类型是数组 —— 转回 []any。
	out := make([]any, len(merged))
	for i, m := range merged {
		out[i] = m
	}
	return out
}

// asContentBlocks 复刻 `Array.isArray(msg.content) ? msg.content : [{ type:"text", text: msg.content }]`。
//
// 非数组时**包成单元素数组**（哪怕 content 是 undefined —— 参考实现会造出
// `{type:"text", text: undefined}`，序列化后 text 被丢掉但块还在）。
func asContentBlocks(v any) []any {
	if arr, ok := v.([]any); ok {
		return arr
	}
	return []any{map[string]any{"type": "text", "text": v}}
}

// hasBlockTypeInContent 报告 msg.content 里是否存在指定 type 的块。
func hasBlockTypeInContent(msg map[string]any, want string) bool {
	content, ok := msg["content"].([]any)
	if !ok {
		return false
	}
	return blocksContainType(content, want)
}

// blocksContainType 报告块数组里是否存在指定 type 的块。
func blocksContainType(blocks []any, want string) bool {
	for _, b := range blocks {
		if m, ok := b.(map[string]any); ok && blockType(m) == want {
			return true
		}
	}
	return false
}

// jsJSONStringify 复刻 `JSON.stringify(block.content ?? "")`。
//
// 用途: 把失去配对的 tool_result 的 `content` 序列化进降级文本。
//
// ★ 反直觉点（三点, 都是探针实测出来的）:
//
//  1. 参考实现先做 `?? ""` —— 即 `null` / `undefined` 会先被换成**空串**,
//     再进 `JSON.stringify`。所以 `content: null` 的序列化结果是 **`""`**
//     （两个引号字符, 共 2 字符）, 不是空串。探针 F12 实测:
//     `[Unpaired tool result ghost]\n""`。
//     若直接用 Go 的 `json.Marshal(nil)` 得到 `"null"`, 更是错的。
//
//  2. 外层还有 `?? ""` —— 因为 `JSON.stringify(undefined)` 返回 **undefined**
//     （一个值, 不是字符串）。经过内层 `?? ""` 之后这条已不可能触发;
//     但 Go 侧仍保留"出错给空串"的兜底。
//
//  3. Go 的 `encoding/json` 默认把 `<` `>` `&` 转义成 `\u003c` `\u003e` `\u0026`,
//     而 JS 的 `JSON.stringify` **不转义**。必须反转义回字面量, 否则降级文本里的
//     工具输出会被改写成 `\u003c` 形式（探针实测 JS 输出 `<a&b>` 原样）。
func jsJSONStringify(v any) string {
	// `block.content ?? ""`
	if v == nil {
		v = ""
	}
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	s := string(b)
	// 还原 Go 的 HTML 转义, 对齐 JS `JSON.stringify` 的字面量输出。
	s = strings.ReplaceAll(s, `\u003c`, "<")
	s = strings.ReplaceAll(s, `\u003e`, ">")
	s = strings.ReplaceAll(s, `\u0026`, "&")
	return s
}
