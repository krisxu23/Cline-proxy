package app

import (
	"cline-go-proxy/internal/kit"
	"encoding/json"
	"strings"
)

func normalizeOpenAIResponse(obj map[string]any) map[string]any {
	out := make(map[string]any)
	for k, v := range obj {
		if k == "provider_metadata" || k == "proxy_metadata" {
			continue
		}
		out[k] = v
	}

	if choices, ok := out["choices"].([]any); ok {
		normalized := make([]any, 0, len(choices))
		for _, ch := range choices {
			if c, ok := ch.(map[string]any); ok {
				nc := make(map[string]any)
				for k, v := range c {
					if k == "provider_metadata" || k == "proxy_metadata" {
						continue
					}
					nc[k] = v
				}
				if msg, ok := nc["message"].(map[string]any); ok {
					nm := normalizeMessage(msg)
					nc["message"] = nm
				}
				if delta, ok := nc["delta"].(map[string]any); ok {
					nd := make(map[string]any)
					for k, v := range delta {
						if k == "provider_metadata" || k == "proxy_metadata" {
							continue
						}
						nd[k] = v
					}
					if tc, ok := nd["tool_calls"].([]any); ok && len(tc) > 0 {
						repairToolCallDeltas(tc)
						if nd["content"] == nil {
							nd["content"] = ""
						}
					}
					// 对齐 OmniRoute 流式 delta 处理: 与 normalizeMessage 同一份
					// 归一逻辑, 保证流式/非流式两条路径不漂移。
					copyOpenAICompatibleReasoningFields(delta, nd)
					nc["delta"] = nd
				}
				// T18 归一(照抄 OmniRoute open-sse/utils/stream.ts:1953):
				//
				//	// T18: Normalize finish_reason to 'tool_calls' if tool calls were used
				//	if (isFinishChunk && passthroughHasToolCalls &&
				//	    parsed.choices[0].finish_reason !== "tool_calls") {
				//	  parsed.choices[0].finish_reason = "tool_calls";
				//
				// 非流式形态的等价实现见:
				//   open-sse/handlers/responseSanitizer.ts:302
				//   open-sse/handlers/chatCore/passthroughToolNames.ts:93
				//     normalizeOpenAIToolFinishReasons()
				// 两者都做同一件事: `message.tool_calls?.length > 0 &&
				// choice.finish_reason !== "tool_calls"` → 改成 "tool_calls"。
				//
				// 上游(尤其 OpenAI 兼容网关与部分 free 模型)在本回合用了工具调用时,
				// 终止帧的 finish_reason 仍会送 "stop"。agent 工具(Codex / Cline 类)
				// 依据 finish_reason 决定"这一回合是结束还是要去执行工具", 看到 "stop"
				// 就当成回合正常结束 —— 于是工具调用被静默丢弃, 任务无故中断且无提示。
				// 必须在本回合确实出现了 tool_calls 时把它归一为 "tool_calls"。
				//
				// 注意判定依据是本 choice **自身**是否带 tool_calls(非流式的
				// message.tool_calls 与流式的 delta.tool_calls 两种形态),
				// 而非整个响应跨 choice 累计的标志 —— 与 OmniRoute 逐 choice 的
				// 写法对齐: n>1 时 choice 0 的工具调用不得把 choice 1 的
				// finish_reason="stop" 误改成 "tool_calls"。
				if fr, ok := nc["finish_reason"].(string); ok && fr != "" && fr != "tool_calls" {
					choiceHasToolCalls := false
					if msg, ok := nc["message"].(map[string]any); ok {
						if tc, ok := msg["tool_calls"].([]any); ok && len(tc) > 0 {
							choiceHasToolCalls = true
						}
					}
					if !choiceHasToolCalls {
						if d, ok := nc["delta"].(map[string]any); ok {
							if tc, ok := d["tool_calls"].([]any); ok && len(tc) > 0 {
								choiceHasToolCalls = true
							}
						}
					}
					if choiceHasToolCalls {
						nc["finish_reason"] = "tool_calls"
					}
				}
				normalized = append(normalized, nc)
			} else {
				normalized = append(normalized, ch)
			}
		}
		out["choices"] = normalized
	}

	return out
}

func sanitizeContent(s string) string {
	return s
}

func normalizeMessage(msg map[string]any) map[string]any {
	out := make(map[string]any)
	for k, v := range msg {
		if k == "provider_metadata" || k == "proxy_metadata" {
			continue
		}
		out[k] = v
	}
	if tc, ok := out["tool_calls"].([]any); ok && len(tc) > 0 {
		// 部分开源/免费模型返回的 tool_calls 缺 id, 客户端会整体报错
		// "tool_calls without a complete id and function name", 在出口处修好
		repaired, _ := repairToolCalls(tc)
		out["tool_calls"] = repaired
		if out["content"] == nil {
			out["content"] = ""
		}
	}
	if c, ok := out["content"].(string); ok {
		out["content"] = sanitizeContent(c)
	}
	// 对齐 OmniRoute sanitizeMessage: 推理字段归一 + 内部占位符净化。
	// 必须在 content 处理之后、tool_calls 修补之后的出口侧执行, 保证转发给
	// 客户端的内容里不含请求脚手架。
	copyOpenAICompatibleReasoningFields(msg, out)
	return out
}

// appendToolCallArgumentDelta 照抄 OmniRoute open-sse/utils/toolCallArguments.ts:41。
//
// 上游有两种 tool-call arguments 分片形态, 必须区别对待:
//   - **增量分片**: 每帧只带**新**片段。必须**逐字拼接** —— 即便某片段的开头
//     字节与已有内容的结尾重复(例如 `ls -ll` 里那个重复的 `l`)。
//   - **完整快照**: 每帧重发**截至此刻的全部** arguments。拼接会复制 payload
//     (OmniRoute issue #3701)。
//
// 只在**无歧义**时才按快照处理(取后者): 完全相同的重复, 或以已有内容为前缀的
// 增长。其余一律按增量分片原样追加。
//
// 原注释明确警告(**这是最容易写错的地方**):
//
//	A fuzzy suffix/prefix-overlap heuristic must NOT be used here: it silently
//	drops bytes from legitimate incremental deltas (turning `ll` into `l`,
//	`xx` into `x`), which trades a visible duplication bug for a silent
//	truncation bug.
//
// 即: **绝不能**用"后缀/前缀模糊重叠"去重 —— 那会把 `ll` 悄悄吞成 `l`。
// 把可见的重复 bug 换成静默的截断 bug, 后者更危险(参数被截断 = 工具调用出错)。
//
// 第三种非规范形态 OmniRoute #6459 也处理了: 某些上游把**已解析的 JSON 对象/数组**
// 当作 arguments 直接发来(违反 OpenAI 流式契约, 但 Anthropic 形态透传的后端会这么干)。
// 若按"非字符串"静默丢弃, 上游的 tool_use.input 会变空; 若用普通字符串强转,
// 客户端会看到字面量 `[object Object]`。正确做法是 JSON 序列化成合法分片。
func appendToolCallArgumentDelta(current, incoming any) string {
	existing, _ := current.(string)
	next := normalizeIncomingFragment(incoming)

	if existing == "" {
		return next
	}
	if next == "" {
		return existing
	}
	// 无歧义的"快照重复/增长" → 替换而非拼接
	if next == existing {
		return existing
	}
	if strings.HasPrefix(next, existing) {
		return next
	}
	// 增量分片 → 逐字追加(保留重复字符)
	return existing + next
}

// normalizeIncomingFragment 对应同文件 normalizeIncomingFragment:
// 字符串直接用; nil → 空; 对象/数组 → JSON 序列化(照抄 #6459 的修法,
// 避免丢参数或渲染出字面量 "[object Object]")。
func normalizeIncomingFragment(incoming any) string {
	switch v := incoming.(type) {
	case nil:
		return ""
	case string:
		return v
	case map[string]any, []any:
		b, err := json.Marshal(v)
		if err != nil {
			return ""
		}
		return string(b)
	default:
		return ""
	}
}

// genToolCallID 生成 OpenAI 风格的 tool_call id。
func genToolCallID() string { return "call_" + kit.RandHex(12) }

// repairToolCalls 修补非流式响应里的 tool_calls, 返回修补后的条目与剔除数。
//
// 客户端(Cline 类 agent 工具)对完整性有硬校验: id 与 function.name 缺一即
// 整体报 "Model provider returned tool_calls without a complete id and
// function name"。实测部分 free 模型只回 function.name 不回 id —— 补一个
// 随机 id 就是完全合法的调用; 连 name 都没有的条目客户端无法执行, 只能剔除。
func repairToolCalls(tcs []any) ([]any, int) {
	out := make([]any, 0, len(tcs))
	dropped := 0
	for _, raw := range tcs {
		tc, ok := raw.(map[string]any)
		if !ok {
			dropped++
			continue
		}
		fn, _ := tc["function"].(map[string]any)
		name := ""
		if fn != nil {
			name, _ = fn["name"].(string)
		}
		if strings.TrimSpace(name) == "" {
			dropped++ // 没有 name 的调用无法执行
			continue
		}
		if id, _ := tc["id"].(string); strings.TrimSpace(id) == "" {
			tc["id"] = genToolCallID()
		}
		if t, _ := tc["type"].(string); t == "" {
			tc["type"] = "function"
		}
		if fn["arguments"] == nil || fn["arguments"] == "" {
			fn["arguments"] = "{}"
		}
		out = append(out, tc)
	}
	return out, dropped
}

// repairToolCallDeltas 流式 delta 的保守修补: 起始块(function.name 非空)缺 id
// 时补一个并确保 type; 续流块(name 为空、只带 arguments 分片)保持原样 ——
// 客户端按 index 累积分片, 乱动续流块会把流弄坏。
func repairToolCallDeltas(tcs []any) {
	for _, raw := range tcs {
		tc, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		fn, _ := tc["function"].(map[string]any)
		if fn == nil {
			continue
		}
		name, _ := fn["name"].(string)
		if strings.TrimSpace(name) == "" {
			continue
		}
		if id, _ := tc["id"].(string); strings.TrimSpace(id) == "" {
			tc["id"] = genToolCallID()
		}
		if t, _ := tc["type"].(string); t == "" {
			tc["type"] = "function"
		}
	}
}
