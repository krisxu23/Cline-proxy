package app

import "regexp"

// 过期 / 失效 thinking 的**被动修复重放**(每请求最多一次)。
//
// 两个目标错误都出自 Anthropic 形态上游的 "thinking.type === enabled" 契约
// (字符串与成因见 claude_thinking_blocks.go 顶部的参考实现注释):
//
//   - "Invalid signature in thinking block"
//     (claude 原生, 跨 provider 重放历史带外来/合成签名。主动防御对
//     "最新 assistant 的真签名块" 按协议不敢改写 —— 改了会撞
//     "blocks in the latest assistant message cannot be modified", 所以
//     签名真伪只能由上游判, 判不过就是这个 400)
//   - "thinking is enabled but reasoning_content is missing in assistant
//     tool call message at index N"
//     (kimi-coding 一类: thinking 开着而 assistant tool_use 回合缺 thinking 块。
//     主动防御的前置占位块在进环前已尽力补, 但上游比占位规则更严时仍然 400)
//
// 两个错误都是同一条契约的两面(签名无效 ≈ 块缺失 —— 校验不过的块等同没有)。
// 因此兜底策略是**关掉契约本身**: 剥掉 params 里的 thinking / reasoning_effort
// 配置(后者会让 convertOutboundRequest 在每个 attempt 重新生成 thinking),
// 并从 claude 形态 content[] 里剥掉全部 thinking / redacted_thinking 块,
// 另删 assistant 消息的 reasoning_content 字段(kimi-coding 一类字段型推理:
// 转换层会用它在下个 attempt 重建 thinking, 不删则重放与原请求相同、再次 400),
// 然后重放一次。契约不再启用, 两类 400 结构性消失。
//
// 作用域严格限 Anthropic 形态(APIType == "anthropic" 或 APIFormat == messages):
// OpenAI 形态上游的 reasoning 契约是**反向的**(缺了要补空串, 见
// reasoning_replay.go), 在那条路上剥 reasoning 字段反而会打出
// "The reasoning_content in the thinking mode must be passed back" —— 绝不能碰。
//
// 与 Gemini thought_signature 重放(providers_chat.go 的
// isMissingThoughtSignatureError 分支)同一纪律: 只重放一次, 仍失败就原样交还。

// staleThinkingErrorRe 只认两条实证错误串(大小写不敏感、允许后缀细节),
// 不做模糊匹配 —— 宽匹配会把无关 400 拉进"剥字段重放", 掩盖真实错误。
var staleThinkingErrorRe = regexp.MustCompile(
	`(?i)invalid signature in thinking block|thinking is enabled but reasoning_content is missing`)

// isStaleThinkingError 400 且命中已知的过期/缺失 thinking 契约错误。
func isStaleThinkingError(status int, message string) bool {
	return status == 400 && staleThinkingErrorRe.MatchString(message)
}

// stripStaleThinking 剥掉请求里的 thinking 配置、thinking 块与 assistant
// 字段型推理。返回是否有改动 —— 无改动时调用方不重放(重放一个一模一样的
// 请求毫无意义)。
//
// 剥离项:
//  1. params["thinking"]        —— claude 形态的 thinking 开关;
//  2. params["reasoning_effort"] —— 会让 OpenAIChatToClaudeRequest 在下个
//     attempt 重新生成 thinking 配置(OpenAI 形态出站里它本身无害, 但既然
//     契约已判定失效, 一并去掉保证两个形态的重放体都干净);
//  3. assistant content[] 里的 thinking / redacted_thinking 块 —— 签名无效的
//     正是它们。text / tool_use / tool_result 等其余块原样保留;
//  4. assistant 消息的 reasoning_content 字段 —— kimi-coding 一类把推理放
//     字段(content 是普通文本): 转换层会在下个 attempt 用它原样重建
//     thinking, 不删则重放与原请求相同、再次 400, 修复预算白烧。只删
//     assistant;本函数仅在 Anthropic 形态作用域被调用(见文件头), OpenAI
//     反向契约那条路进不来。
//
// 若某个 assistant 消息的块被剥光(只剩过期 thinking 的病态历史)且**不带
// tool_calls**, 整条消息删除 —— 空 content 数组会被上游按 "content must be
// non-empty" 再打回一次, 等于这次修复白做; 纯 thinking 回合本身也不携带
// 任何信息。带 tool_calls 的必须留下: 整条删除会让后续 tool_result 变孤儿
// (剥离重放已不再过 fixToolPairs), 空 content[] 由转换层从 tool_calls
// 重建 tool_use 兜底, 不会触发上面的非空拒收。
func stripStaleThinking(params map[string]any) bool {
	if params == nil {
		return false
	}
	changed := false
	for _, k := range []string{"thinking", "reasoning_effort"} {
		if _, ok := params[k]; ok {
			delete(params, k)
			changed = true
		}
	}
	msgs, ok := params["messages"].([]any)
	if !ok {
		return changed
	}
	keptMsgs := make([]any, 0, len(msgs))
	for _, raw := range msgs {
		m, ok := raw.(map[string]any)
		if !ok {
			keptMsgs = append(keptMsgs, raw)
			continue
		}
		// 字段型推理见函数头剥离项 4;content 是字符串也照删(那正是
		// kimi-coding 的形态), 放在 content 形态分支之前。
		if jsIsAssistant(m) {
			if _, has := m["reasoning_content"]; has {
				delete(m, "reasoning_content")
				changed = true
			}
		}
		blocks, ok := m["content"].([]any)
		if !ok {
			// chat 形态(content 字符串)或非数组: 无块可剥, 原样。
			keptMsgs = append(keptMsgs, m)
			continue
		}
		kept := make([]any, 0, len(blocks))
		removed := 0
		for _, b := range blocks {
			if bm, isObj := b.(map[string]any); isObj {
				if t, _ := bm["type"].(string); t == "thinking" || t == "redacted_thinking" {
					removed++
					changed = true
					continue
				}
			}
			kept = append(kept, b)
		}
		if removed == 0 {
			keptMsgs = append(keptMsgs, m)
			continue
		}
		if len(kept) == 0 {
			// 块被剥光: 带非空 tool_calls 的消息不能整条删(孤儿
			// tool_result), 留下并置空 content, 见函数头注释;
			// 纯 thinking 回合整条删除。
			if tc, hasTC := m["tool_calls"].([]any); !hasTC || len(tc) == 0 {
				continue
			}
		}
		m["content"] = kept
		keptMsgs = append(keptMsgs, m)
	}
	if changed {
		params["messages"] = keptMsgs
	}
	return changed
}
