package app

import (
	"cline-go-proxy/internal/kit"
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
					nc["message"] = normalizeMessage(msg)
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
