package app

import "strings"

// 逐字照抄 OmniRoute open-sse/translator/helpers/claudeHelper.ts 的 prompt-cache
// 三件套 + 重锚逻辑。
//
// 参考实现出处逐条对位:
//
//	ensureMessageContentArray  claudeHelper.ts:286-293
//	markMessageCacheControl    claudeHelper.ts:295-302
//	bodyHasAnyCacheControl     claudeHelper.ts:307-328
//	system 重锚                claudeHelper.ts:387-396
//	messages 内 cc 清除        claudeHelper.ts:408-413
//	第二后一条 user 标记       claudeHelper.ts:503-513
//	assistant 标记             claudeHelper.ts:530-544
//	tools 重锚                 claudeHelper.ts:727-740
//
// # 为什么这条对本网关重要
//
// Anthropic 的 prompt cache 是**显式断点**制: 只有被 `cache_control` 标记的前缀
// 才会被缓存。客户端(Codex / Cline / Claude Code)常常一个 marker 都不带,
// 于是**每一轮都把整个前缀按未缓存计费** —— 长会话下成本可达数倍。
//
// 参考实现的策略是"重锚": 清掉客户端散落的 marker, 再按 Claude Code 的既有
// 约定补三个断点:
//
//  1. system 的**最后一块** -> `{type:"ephemeral", ttl:"1h"}`;
//  2. **倒数第二条 user** 的最后一个内容块(供下一轮复用);
//  3. 从后往前第一条**非 defer_loading** 的 tool -> `{type:"ephemeral", ttl:"1h"}`;
//     外加**最后一条有内容的 assistant** 的最后一个内容块。
//
// # 关键语义点(探针实测, 逐条锁定)
//
//   - `ensureMessageContentArray` 对**非空字符串**会**就地改写** `msg.content`
//     成 `[{type:"text",text:...}]`; 空白串/空串/null/数字一律返回**空切片**
//     (且不改写)。探针 E1。
//   - `markMessageCacheControl` 返回 bool, `content.length === 0` 时 false。
//     它**覆盖**已有的 cache_control(探针 E2 "已有 cc 被覆盖")。
//   - `bodyHasAnyCacheControl` 用 **JS 真值**判定 (探针 E3):
//     `cache_control: null` 为**假**、`{}` 为**真**。且 `system` 是字符串时
//     `Array.isArray` 为假 -> 直接跳过。
//   - tools 重锚**跳过 `defer_loading` 为真值**的工具; 若全部都是 defer_loading
//     则**一个都不标记**(探针 E5 "全 defer_loading")。
//   - 第二后一条 user: `userMessageIndexes.length >= 2` 才标记, 否则不标记
//     (探针 E6 三栏)。

// ensureMessageContentArray 照抄 claudeHelper.ts:286-293。
//
//	如果 msg.content 是数组 -> 原样返回该数组(同一引用)。
//	否则若是**非空**字符串 -> **就地改写** msg.content 为 [{type:"text",text:...}]
//	                          并返回新数组。
//	其余情况(空串/空白串/null/数字/缺失) -> 返回**空切片**。
//
// ★ 就地改写是照抄的: 参考实现写 `msg.content = [...]`。Go 侧 map 是引用类型,
// 同样写回 msg["content"], 调用方持有的对象随之更新。
func ensureMessageContentArray(msg map[string]any) []any {
	// :287 `if (Array.isArray(msg?.content)) return msg.content;`
	//
	// ★ `msg?.` 可选链: msg 为 null/undefined 时整体返回 undefined, 而
	// `Array.isArray(undefined)` 为假 -> 继续往下走。而下一行的 `msg?.content`
	// 同样为 undefined -> `typeof undefined === "string"` 为假 -> 返回 []。
	// 所以 null msg 的结果是**空切片**。
	if msg != nil {
		if arr, ok := msg["content"].([]any); ok {
			return arr
		}
		// :288-291 `if (typeof msg?.content === "string" && msg.content.trim()) {
		//             msg.content = [{ type: "text", text: msg.content }];
		//             return msg.content; }`
		if s, ok := msg["content"].(string); ok && strings.TrimSpace(s) != "" {
			blocks := []any{map[string]any{"type": "text", "text": s}}
			msg["content"] = blocks
			return blocks
		}
	}
	// :292 `return [];`
	return []any{}
}

// markMessageCacheControl 照抄 claudeHelper.ts:295-302。
//
//	content 为空 -> false(不改任何东西)。
//	否则给**最后一个块**挂 cache_control:
//	  ttl 给定 -> {type:"ephemeral", ttl:<ttl>}
//	  ttl 缺失 -> {type:"ephemeral"}
//	返回 true。
//
// ★ 覆盖语义: 已有 cache_control 会被**整体替换**(探针 E2)。
// ★ 块不是对象时参考实现抛 TypeError(探针 E2 "[null] 块")。本网关是长驻服务,
// panic 会打掉整个进程 —— 此处**跳过非对象块**并返回 false, 登记为有意偏离。
func markMessageCacheControl(msg map[string]any, ttl string, hasTTL bool) bool {
	content := ensureMessageContentArray(msg)
	// :297 `if (content.length === 0) return false;`
	if len(content) == 0 {
		return false
	}
	// :298-300 `content[lastIndex].cache_control = ttl !== undefined
	//             ? { type: "ephemeral", ttl } : { type: "ephemeral" };`
	last := content[len(content)-1]
	bm, ok := last.(map[string]any)
	if !ok {
		// ★ 有意偏离参考实现: 参考实现在此处对 null 块抛
		// `TypeError: Cannot set properties of null`。长驻服务不照抄崩溃路径。
		return false
	}
	var marker map[string]any
	if hasTTL {
		marker = map[string]any{"type": "ephemeral", "ttl": ttl}
	} else {
		marker = map[string]any{"type": "ephemeral"}
	}
	bm["cache_control"] = marker
	return true
}

// bodyHasAnyCacheControl 照抄 claudeHelper.ts:307-328。
//
// 依次检查 body.system / body.messages[].content[] / body.tools[],
// 任一元素带有**真值** cache_control 即返回 true。
//
// ★ 用 jsTruthy 而非 `!= nil`: 参考实现写的是 `block.cache_control` 直接当条件,
// 故 `null` 为假、`{}` 为真(探针 E3)。
// ★ `system` 是字符串时 `Array.isArray` 为假 -> 整段跳过(探针 E3)。
func bodyHasAnyCacheControl(body map[string]any) bool {
	// :308-312
	if system, ok := body["system"].([]any); ok {
		for _, raw := range system {
			if block, ok := raw.(map[string]any); ok && jsTruthy(block["cache_control"]) {
				return true
			}
		}
	}
	// :313-320
	if messages, ok := body["messages"].([]any); ok {
		for _, mRaw := range messages {
			msg, ok := mRaw.(map[string]any)
			if !ok {
				continue
			}
			content, ok := msg["content"].([]any)
			if !ok {
				continue
			}
			for _, bRaw := range content {
				if block, ok := bRaw.(map[string]any); ok && jsTruthy(block["cache_control"]) {
					return true
				}
			}
		}
	}
	// :321-326
	if tools, ok := body["tools"].([]any); ok {
		for _, tRaw := range tools {
			if tool, ok := tRaw.(map[string]any); ok && jsTruthy(tool["cache_control"]) {
				return true
			}
		}
	}
	return false
}

// reanchorSystemCacheControl 照抄 claudeHelper.ts:387-396。
//
// 清掉 system 每块的 cache_control; 若 supportsPromptCaching 为真, 只给
// **最后一块**挂 `{type:"ephemeral", ttl:"1h"}`。
//
// ★ 必须返回**新切片**(参考实现是 `systemBlocks.map(...)`) —— 不能就地改。
func reanchorSystemCacheControl(system []any, supportsPromptCaching bool) []any {
	out := make([]any, 0, len(system))
	for i, raw := range system {
		block, ok := raw.(map[string]any)
		if !ok {
			out = append(out, raw)
			continue
		}
		// `const { cache_control, ...rest } = block;` —— 剔除 cache_control。
		rest := make(map[string]any, len(block))
		for k, v := range block {
			if k == "cache_control" {
				continue
			}
			rest[k] = v
		}
		// `if (i === systemBlocks.length - 1 && supportsPromptCaching) { ... }`
		if i == len(system)-1 && supportsPromptCaching {
			rest["cache_control"] = map[string]any{"type": "ephemeral", "ttl": "1h"}
		}
		out = append(out, rest)
	}
	return out
}

// stripMessageCacheControl 照抄 claudeHelper.ts:408-413 —— 逐块删除
// cache_control(就地改块; 参考实现是 `delete block.cache_control`)。
//
// ★ 只在非 passthrough 模式下调用(参考实现由 `!preserveCacheControl` 守卫)。
func stripMessageCacheControl(msg map[string]any) {
	content, ok := msg["content"].([]any)
	if !ok {
		return
	}
	for _, bRaw := range content {
		if block, ok := bRaw.(map[string]any); ok {
			delete(block, "cache_control")
		}
	}
}

// markSecondToLastUserCacheControl 照抄 claudeHelper.ts:503-513。
//
// 找出所有 user 回合的下标; 只有当**至少两条**时才给**倒数第二条**的最后一个
// 内容块挂 `{type:"ephemeral"}`(无 ttl)。返回是否真的标记了。
//
// 参考实现原注释:
//
//	// Claude Code-style prompt caching:
//	// - cache the second-to-last user turn for conversation reuse
//	// Skip in passthrough mode to preserve client's cache_control markers
func markSecondToLastUserCacheControl(messages []any) bool {
	userIdx := make([]int, 0, len(messages))
	for i, raw := range messages {
		if msg, ok := raw.(map[string]any); ok {
			if role, _ := msg["role"].(string); role == "user" {
				userIdx = append(userIdx, i)
			}
		}
	}
	// `const secondToLastUserIndex = userMessageIndexes.length >= 2
	//    ? userMessageIndexes[userMessageIndexes.length - 2] : -1;`
	if len(userIdx) < 2 {
		return false
	}
	target := messages[userIdx[len(userIdx)-2]]
	msg, ok := target.(map[string]any)
	if !ok {
		return false
	}
	// `markMessageCacheControl(filtered[secondToLastUserIndex])` —— 无 ttl。
	return markMessageCacheControl(msg, "", false)
}

// markLastAssistantCacheControl 照抄 claudeHelper.ts:530-544 的 assistant 分支:
// 从**后往前**扫描, 遇到第一条 `role=="assistant"` 且 content 非空的回合,
// 就给它挂 `{type:"ephemeral"}`(无 ttl), 然后停止。返回是否标记了。
//
// 参考实现原注释:
//
//	// Add cache_control to last block of first (from end) assistant with content
//	// Skip in passthrough mode to preserve client's cache_control markers
func markLastAssistantCacheControl(messages []any) bool {
	for i := len(messages) - 1; i >= 0; i-- {
		msg, ok := messages[i].(map[string]any)
		if !ok {
			continue
		}
		if role, _ := msg["role"].(string); role != "assistant" {
			continue
		}
		// `const content = ensureMessageContentArray(msg);` +
		// `if (msg.role === "assistant" && content.length > 0) {`
		//
		// ★ 参考实现在这个循环里对**每条**消息都先调 ensureMessageContentArray,
		//   因此字符串 content 的 assistant 会被就地改写成块数组。
		content := ensureMessageContentArray(msg)
		if len(content) == 0 {
			continue
		}
		if markMessageCacheControl(msg, "", false) {
			return true
		}
	}
	return false
}

// reanchorToolsCacheControl 照抄 claudeHelper.ts:727-740。
//
// 清掉每个 tool 的 cache_control; 若 supportsPromptCaching 为真, 从**后往前**
// 找第一条 `defer_loading` 为**假值**的 tool, 给它挂
// `{type:"ephemeral", ttl:"1h"}` 后停止。全部都是 defer_loading 时一个都不标记。
//
// 参考实现原注释:
//
//	// 3. Tools: remove all cache_control, add only to last non-deferred tool with ttl 1h
//	// Tools with defer_loading=true cannot have cache_control (API rejects it)
//
// ★ 用 jsTruthy 判定 defer_loading: 参考实现写的是 `!body.tools[i].defer_loading`,
// 故 `false` / `0` / `""` / 缺失都算"非 deferred"(探针 E5 "defer_loading=false")。
func reanchorToolsCacheControl(tools []any, supportsPromptCaching bool) []any {
	out := make([]any, 0, len(tools))
	for _, raw := range tools {
		tool, ok := raw.(map[string]any)
		if !ok {
			out = append(out, raw)
			continue
		}
		rest := make(map[string]any, len(tool))
		for k, v := range tool {
			if k == "cache_control" {
				continue
			}
			rest[k] = v
		}
		out = append(out, rest)
	}
	if !supportsPromptCaching {
		return out
	}
	for i := len(out) - 1; i >= 0; i-- {
		tool, ok := out[i].(map[string]any)
		if !ok {
			continue
		}
		if jsTruthy(tool["defer_loading"]) {
			continue
		}
		tool["cache_control"] = map[string]any{"type": "ephemeral", "ttl": "1h"}
		break
	}
	return out
}

// supportsPromptCachingFor 照抄 claudeHelper.ts:366-367:
//
//	// 1. System: remove all cache_control, add only to last block with ttl 1h
//	const supportsPromptCaching =
//	  provider === "claude" || provider?.startsWith?.("anthropic-compatible-");
//
// ★ `provider?.startsWith?.(...)` 双重可选链: provider 为 null/undefined 时
// 整体为 undefined -> 假。Go 侧 provider 为空串时同理为假。
func supportsPromptCachingFor(provider string) bool {
	return provider == "claude" || strings.HasPrefix(provider, "anthropic-compatible-")
}

// claudeFormatProvidersWithoutOutputConfig 照抄 claudeHelper.ts:16:
//
//	const CLAUDE_FORMAT_PROVIDERS_WITHOUT_OUTPUT_CONFIG =
//	  new Set<string>(["minimax", "minimax-cn"]);
//
// 这些 provider 的 Claude 兼容端点会拒收 `output_config`, 必须在任何下游处理
// **之前**剥掉(claudeHelper.ts:342-347)。
var claudeFormatProvidersWithoutOutputConfig = map[string]bool{
	"minimax":    true,
	"minimax-cn": true,
}

// reanchorClaudePromptCache 是 prompt-cache 重锚的**接入级包装**, 逐字对位
// prepareClaudeRequest 里各段的**组合顺序**:
//
//  0. output_config 剥离      claudeHelper.ts:342-347 (仅 minimax 系)
//  1. preserve 模式降级判定    claudeHelper.ts:349-362
//  2. system 重锚            claudeHelper.ts:387-396
//  3. messages 内 cc 清除      claudeHelper.ts:408-413
//  4. tools 重锚             claudeHelper.ts:727-740
//  5. 第二后一条 user 标记     claudeHelper.ts:503-513
//  6. 最后一条 assistant 标记   claudeHelper.ts:530-544
//
// 顺序是照抄的, 每一步都有理由:
//
//   - 0 必须在最前: 参考实现原注释写 "Must run before any downstream processing
//     so the field never reaches translateRequest/the executor"。
//   - 1 必须在 2/3/6 之前: 它决定后面三个"要不要补 marker"。
//   - 2 在 3 之前不是硬要求(两者作用于不同位置), 但保持参考实现顺序。
//   - 5 在 6 之前: 参考实现先标 user 再标 assistant(claudeHelper.ts:503 → :530)。
//     若反过来, "最后一条 assistant" 的扫描会先把标记占掉, 语义就变了。
//     注意两者标记的**不是同一个对象**(user vs assistant), 所以顺序本身不改变
//     结果 —— 但照抄纪律要求顺序一致, 便于机械核对。
//
// ★ 与参考实现的**差异**(必须显式说明): 参考实现每次调用只跑一次
//
//	prepareClaudeRequest, 而它同时承担"过滤空消息"(Pass 1) + "净化 tool id"
//	(Pass 1.4) + "搬块"(Pass 1.45) + "排序"(Pass 1.5) —— 那四步在本网关由
//	chatWithKey 的既有链路承担(见 providers_chat.go 的 anthropic 分支)。
//	本包装**只**负责 cache_control 与 output_config 这几段, 避免重复施加。
func reanchorClaudePromptCache(params map[string]any, provider string) {
	// 0. output_config 剥离(minimax 系)
	if provider != "" && claudeFormatProvidersWithoutOutputConfig[provider] {
		delete(params, "output_config")
	}

	// 1. preserve 模式降级: 客户端一个 marker 都没带时, "preserve" 无从 preserve ——
	//    照常补断点(参考实现 claudeHelper.ts:356-362, 语义同
	//    opts.fallbackToHeuristicWhenNoMarkers === true)。
	//
	// `if (preserveCacheControl && fallbackToHeuristicWhenNoMarkers && !bodyHasAnyCacheControl)`
	// 的等价形式: 有 marker 就 preserve, 没有就补。
	//
	// ★ 本网关不做真正的 passthrough 分支(没有 relay 调用方), 因此这里的
	//   preserveCacheControl 含义是"客户端自带 marker -> 一个都不动"。
	//   这是对参考实现 `preserveCacheControl=true` 那一路的直接照抄。
	preserveCacheControl := bodyHasAnyCacheControl(params)

	supportsPromptCaching := supportsPromptCachingFor(provider)

	if preserveCacheControl {
		// passthrough: 清掉 output_config 之外**什么都不动**, 保留客户端 marker。
		return
	}

	// copy-on-write 深拷贝(根治跨候选 cache_control 泄漏): params 在候选链里被
	// 多个候选共享, 下面 2/3/5/6 步会**就地**改写 msg map(stripMessageCacheControl /
	// markMessageCacheControl / ensureMessageContentArray)并把新 marker 写进共享
	// 内容块 —— 不拷贝的话, 后续候选会把上一候选补的断点原样带出去。
	// 拷贝逐 map 进行(msg map / content 块 map / tool map / system 块 map, 值本身
	// 从不被就地改写, 不递归进 input_schema); preserve 早退在前, 这笔拷贝只在真正
	// 要改写的非 preserve 路径、且只在 anthropic 候选(reanchor 的唯一调用作用域)
	// 付出。tools/system 的重锚函数本身就是先建新切片新 map 再写(自带 COW),
	// 这里一并入口拷贝是为防御未来就地改写, 代价仅一层浅 map。
	deepCopyClaudeReanchorTargets(params)

	// 2. system 重锚
	if system, ok := params["system"].([]any); ok {
		params["system"] = reanchorSystemCacheControl(system, supportsPromptCaching)
	}

	// 3. messages 内 cc 清除
	messages, hasMessages := params["messages"].([]any)
	if hasMessages {
		for _, raw := range messages {
			if msg, ok := raw.(map[string]any); ok {
				stripMessageCacheControl(msg)
			}
		}
	}

	// 4. tools 重锚
	if tools, ok := params["tools"].([]any); ok {
		params["tools"] = reanchorToolsCacheControl(tools, supportsPromptCaching)
	}

	// 5. 第二后一条 user 标记 + 6. 最后一条 assistant 标记
	//
	// ★ 两者只对**支持 prompt caching** 的 provider 生效(参考实现由
	//   `if (!preserveCacheControl && supportsPromptCaching)` 守卫)。
	if hasMessages && supportsPromptCaching {
		markSecondToLastUserCacheControl(messages)
		markLastAssistantCacheControl(messages)
	}
}

// deepCopyClaudeReanchorTargets 把重锚将要**就地改写**的三层 map 逐个拷一份,
// 写回 params —— copy-on-write 的入口半边(写回的是拷贝, 原对象不再被碰)。
//
// 拷贝深度止于"这一层的 map":候选链的泄漏只经过 msg map(strip 删 cc)、
// content 块 map(mark 写 cc)、tool/system 块 map(reanchor 写 cc)这三类;
// map 的值(字符串/嵌套 schema)从不被就地改写, 递归拷它们只烧 CPU。
// 非 map 元素原样搬运(块数组里的字符串等)。
func deepCopyClaudeReanchorTargets(params map[string]any) {
	copyMapSlice := func(raw any) (any, bool) {
		arr, ok := raw.([]any)
		if !ok {
			return nil, false
		}
		out := make([]any, len(arr))
		for i, el := range arr {
			if m, ok := el.(map[string]any); ok {
				cp := make(map[string]any, len(m))
				for k, v := range m {
					cp[k] = v
				}
				out[i] = cp
			} else {
				out[i] = el
			}
		}
		return out, true
	}
	// messages: msg map 拷一层; content 是块数组时, 块 map 再拷一层
	// (stripMessageCacheControl / markMessageCacheControl 正是改到这一层)。
	if arr, ok := params["messages"].([]any); ok {
		out := make([]any, len(arr))
		for i, raw := range arr {
			msg, isMap := raw.(map[string]any)
			if !isMap {
				out[i] = raw
				continue
			}
			cp := make(map[string]any, len(msg))
			for k, v := range msg {
				cp[k] = v
			}
			if blocks, ok := msg["content"].([]any); ok {
				bc := make([]any, len(blocks))
				for j, braw := range blocks {
					if b, isMap := braw.(map[string]any); isMap {
						bcp := make(map[string]any, len(b))
						for k, v := range b {
							bcp[k] = v
						}
						bc[j] = bcp
					} else {
						bc[j] = braw
					}
				}
				cp["content"] = bc
			}
			out[i] = cp
		}
		params["messages"] = out
	}
	if v, ok := copyMapSlice(params["tools"]); ok {
		params["tools"] = v
	}
	if v, ok := copyMapSlice(params["system"]); ok {
		params["system"] = v
	}
}
