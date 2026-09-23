package app

// tool_use / tool_result 的 id **双侧对称净化**（照抄 OmniRoute
// open-sse/translator/helpers/claudeHelper.ts:432-450, Pass 1.4）。
//
// 参考实现原文（逐字）:
//
//	// Pass 1.4: Filter out tool_use blocks with empty names (causes Claude 400 error)
//	// Apply to ALL roles (assistant tool_use + any user messages that may carry tool_use)
//	// Also filter tool_result blocks with missing tool_use_id
//	for (const msg of filtered) {
//	  if (Array.isArray(msg.content)) {
//	    msg.content = msg.content.filter(
//	      (block) => block.type !== "tool_use" || (block.name && block.name?.trim())
//	    );
//	    msg.content = msg.content.filter(
//	      (block) => block.type !== "tool_result" || block.tool_use_id
//	    );
//	    // Anthropic-shape upstreams enforce `^[a-zA-Z0-9_-]+$` on tool ids. Client
//	    // histories can carry ids with `.`/`:`/`#` (e.g. replayed from another
//	    // provider), which 400s as TOOL_SCHEMA_INVALID. Rewrite both sides with the
//	    // same function so tool_use/tool_result pairing survives — the later
//	    // ordering passes match on these ids.
//	    for (const block of msg.content) {
//	      if (block.type === "tool_use" && typeof block.id === "string" && block.id) {
//	        block.id = sanitizeToolId(block.id);
//	      } else if (
//	        block.type === "tool_result" &&
//	        typeof block.tool_use_id === "string" &&
//	        block.tool_use_id
//	      ) {
//	        block.tool_use_id = sanitizeToolId(block.tool_use_id);
//	      }
//	    }
//	  }
//	}
//
// ★ 为什么这条对本网关重要
//
// 本网关的 Anthropic 形态**有两个来源**，两边都可能在历史里带上非法 id:
//
//  1. 客户端直接发 /v1/messages（Codex 经 cc-switch 路由是最常见的一条）——
//     它重放的会话历史可能来自别的 provider（Gemini / OpenAI 兼容层），
//     tool id 形如 `call_abc.123` / `toolu_xyz:1` / `a#b`。
//  2. 本网关**自己**在 openAIToAnthropic 里生成的 tool_use id ——
//     上游 OpenAI 兼容层返回的 id 同样是任意形态。
//
// 两条最终都会送往 Anthropic-shape 上游（`cfg.APIType == "anthropic"` 的
// generic provider，或 Anthropic 原生）。上游对 `^[a-zA-Z0-9_-]+$` 的校验
// 失败即 400 `TOOL_SCHEMA_INVALID`；而在 agent 客户端里 400 常常只表现为
// **任务无声中断**（与用户报的现象一致）。
//
// ★ 关键纪律: **两侧必须同函数改写**
//
// 只改 `tool_use.id` 不改 `tool_result.tool_use_id`（或反之）会让配对断裂，
// 400 从 "invalid id" 变成 "tool_use ids were found without tool_result
// blocks immediately after"。故本函数把两侧放在**同一趟遍历**里处理，
// 不可能只改一边。参考实现原注释亦如此强调（"Rewrite both sides with the
// same function so tool_use/tool_result pairing survives"）。
//
// ★ 作用域说明（照抄的一部分，不可放宽）
//
// 参考实现这条 Pass 在 `prepareClaudeRequest` 内，而 `prepareClaudeRequest`
// 只在 `targetFormat === FORMATS.CLAUDE` 时被调用
// （translator/index.ts:569-572）。即：**仅"出站目标是 Claude 形态"才净化**。
//
// 我方对位 `cfg.APIType == "anthropic"`（providers_chat.go:292 的同一判定）。
// 反方向（Claude → OpenAI）**不净化** —— 参考实现的
// `claude-to-openai.ts` 里 `sanitizeToolId` 出现次数为 0，`tool_call_id` 是
// 原样透传（`:478`）。多净化会让 OpenAI 侧看到被改写的 id，属越界施加。
//
// ★ 为什么同时做 Pass 1.4 的两条过滤
//
// 参考实现把「空名 tool_use」「缺 id 的 tool_result」过滤与 id 净化放在同一个
// 循环里，顺序是**先过滤、后净化**。二者互为前提:
//   - 空名 tool_use 必定 400（"tool_use.name is required"），必须先删；
//   - 缺 id 的 tool_result 若被净化会**新造一个随机 id**
//     （sanitizeToolId 对 falsy 输入返回 `tool_<uuid>`），凭空造出一个永远
//     配不上 tool_use 的结果块 —— 正是 `sanitizeToolResultId.ts` 原注释警告的
//     "defeat that guard and silently fabricate a tool_result"。
// 所以本函数必须先过滤再净化，且 tool_result 侧只在 id 非空时才进入净化。
//
// 返回 (净化后的 messages, 是否有改动)。

import (
	"strconv"
	"strings"
)

// sanitizeClaudeToolIDs 对 messages[] 里的 tool_use / tool_result 做双侧对称 id 净化，
// 并顺带剔除「空名 tool_use」「缺 id 的 tool_result」两类必然 400 的块。
//
// 语义与参考实现逐条对齐:
//   - 只处理 `content` **是数组**的消息（`:433 Array.isArray(msg.content)`）；
//   - 原地改写 `block.id` / `block.tool_use_id`（参考实现是就地赋值）——
//     但为避免改写入参影响调用方持有的对象，先对消息与块做浅拷贝；
//   - tool_use 侧要求 `typeof block.id === "string" && block.id`；
//   - tool_result 侧要求 `typeof block.tool_use_id === "string" && block.tool_use_id`；
//   - 其余块类型原样保留。
//   - 净化结果**同遍去重**(超出参考实现的补充): 净化可把两个不同原始 id
//     (如 `a#b` 与 `a_b`)压成同一值, 上游会回 400 duplicate tool_use id;
//     撞名时追加 `_2`/`_3` 后缀(与 cloak aliasFor 的撞名消解同风格)。
//     tool_use 与 tool_result 在同一遍查同一张表, 同一原始 id 两侧得同一终值。
func sanitizeClaudeToolIDs(messages []any) ([]any, bool) {
	if len(messages) == 0 {
		return messages, false
	}
	changed := false
	out := make([]any, 0, len(messages))
	// final: 原始 id → 最终 id (memo, 双侧配对共用同一张表);
	// taken: 已被占用的净化值 → 占用它的原始 id。
	final := map[string]string{}
	taken := map[string]string{}
	resolve := func(id string) string {
		if v, ok := final[id]; ok {
			return v
		}
		base := sanitizeToolID(id)
		cand := base
		for n := 2; ; n++ {
			if _, used := taken[cand]; !used {
				break
			}
			cand = base + "_" + strconv.Itoa(n)
		}
		taken[cand] = id
		final[id] = cand
		return cand
	}
	for _, raw := range messages {
		msg, ok := raw.(map[string]any)
		if !ok {
			// 非对象元素原样保留（参考实现会抛 TypeError，我方不照抄这个崩溃，
			// 与 claude_helper.go 的既定处理一致）。
			out = append(out, raw)
			continue
		}
		blocks, ok := msg["content"].([]any)
		if !ok {
			out = append(out, raw)
			continue
		}

		// 先过滤: 空名 tool_use / 缺 id 的 tool_result。
		//
		// msgChanged 是本条消息是否真的被改过。**不能用"引用比对"来判断** ——
		// Go 的 map 不可比较, `next[i] != blocks[i]` 会在运行期 panic
		// (comparing uncomparable type map[string]interface {})。改为在两趟循环
		// 里显式置位, 语义更直白。
		msgChanged := false
		kept := make([]any, 0, len(blocks))
		for _, bRaw := range blocks {
			b, ok := bRaw.(map[string]any)
			if !ok {
				kept = append(kept, bRaw)
				continue
			}
			switch blockType(b) {
			case "tool_use":
				// `block.type !== "tool_use" || (block.name && block.name?.trim())`
				name, _ := b["name"].(string)
				if strings.TrimSpace(name) == "" {
					msgChanged = true
					continue
				}
			case "tool_result":
				// `block.type !== "tool_result" || block.tool_use_id`
				id, _ := b["tool_use_id"].(string)
				if id == "" {
					msgChanged = true
					continue
				}
			}
			kept = append(kept, bRaw)
		}

		// 再净化: 两侧同一趟、同一个函数。
		next := make([]any, 0, len(kept))
		for _, bRaw := range kept {
			b, ok := bRaw.(map[string]any)
			if !ok {
				next = append(next, bRaw)
				continue
			}
			switch blockType(b) {
			case "tool_use":
				id, ok := b["id"].(string)
				if !ok || id == "" {
					next = append(next, bRaw)
					continue
				}
				sid := resolve(id)
				if sid != id {
					nb := shallowCopyStringAny(b)
					nb["id"] = sid
					next = append(next, nb)
					msgChanged = true
					continue
				}
			case "tool_result":
				id, ok := b["tool_use_id"].(string)
				if !ok || id == "" {
					next = append(next, bRaw)
					continue
				}
				sid := resolve(id)
				if sid != id {
					nb := shallowCopyStringAny(b)
					nb["tool_use_id"] = sid
					next = append(next, nb)
					msgChanged = true
					continue
				}
			}
			next = append(next, bRaw)
		}

		if !msgChanged {
			// 整份历史上全部 id 合法且无畸形块: 原样保留该消息引用,
			// 不重建切片 —— 保持"干净历史上零改动"这一性质。
			out = append(out, raw)
			continue
		}
		nm := shallowCopyStringAny(msg)
		nm["content"] = next
		out = append(out, nm)
		changed = true
	}
	if !changed {
		return messages, false
	}
	return out, true
}
