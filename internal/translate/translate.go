// Package translate 提供网关内部统一格式(OpenAI chat)与上游原生协议之间的
// 双向翻译, 方案与结构参照 OmniRoute 的 open-sse/translator(MIT, 版权声明见
// 仓库根 NOTICE)。调用方直接调用各转换函数(无注册表)。
//
// **接线状态(按生产调用点实测)**:
//   - claude 请求方向已接线: OpenAIChatToClaudeRequest
//     (providers_api_format.go 的 Anthropic Messages 出站、
//     zen_messages_convert.go 的 zen /messages 请求);
//   - claude 响应方向已接线: ClaudeResponseToOpenAIChat /
//     ClaudeSSEToOpenAISSE(zen_messages_convert.go 的非流式与流式回转);
//   - gemini 双向目前仅被测试引用: 网关的 Google 上游一律走 OpenAI 兼容
//     方言(/v1beta/openai), 不经本包的 Gemini 原生转换 —— 待真正接线后再
//     扩展, 勿按"已接线"引用 gemini 方向。
//
// 勿与 `internal/app/translate_registry` 混淆: 那个是"转换注册表 + 出站体
// 形态不变量校验"(防重复转换事故, 已在生产使用), 本包是"协议格式互转实现"。
// (两包 2026-09-16 之前同名 `translate`, 已按职责改名去歧义。)
//
// 与上游参照实现的**有意差异**(均为 OmniRoute 特有的供应商分支, 不适用于
// 本网关, 已在移植时略去): kimi-coding 思考注入、Copilot summarized
// thinking、Claude OAuth 的模型强制思考识别、adaptive 降级防御等。
package translate

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// newToolCallID 生成一个 tool_call id。
//
// 上游(尤其免费模型)常只回 function.name 不回 id, 而客户端对完整性有硬校验:
// 缺 id 会整体报 "tool_calls without a complete id and function name"。
// 形态与 app 包的 genToolCallID 一致(call_<12 hex>)—— 两个包各持一份, 因为
// translate 不能反向依赖 app(会形成导入环)。修复 2026-09-24 审查: 此前多处
// "缺 id 即空 id 直透"。
func newToolCallID() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		// 熵源不可用是极端情况: 退化成时间戳, 唯一性量级仍然够用。
		return fmt.Sprintf("call_%d", time.Now().UnixNano())
	}
	return "call_" + hex.EncodeToString(b)
}

// 格式标识(与 OmniRoute formats.ts 对齐, 只保留本网关需要的)。
const (
	FormatOpenAI = "openai"
	FormatClaude = "claude"
	FormatGemini = "gemini"
)

// 哨兵错误: 空请求体 / 无可转换消息。
var (
	errNilBody     = errors.New("translate: nil request body")
	errNilMessages = errors.New("translate: request has no convertible messages")
)

// OpenAIChatToClaudeRequest 把 OpenAI chat 请求体翻译为 Anthropic Messages
// 请求体(核心协议映射, 与上游参照实现的差异见包注释)。要点:
//   - Anthropic 必填 max_tokens: 缺省时按 8192 兜底;
//   - OpenAI 的 system 角色消息 → Anthropic 顶层 system 块(保留 cache_control);
//   - content 支持文本串与多段数组(文本/图片 image_url → source 块);
//   - assistant 的 tool_calls → tool_use 块; role=tool → tool_result 块;
//   - tools 的 function.parameters → input_schema; tool_choice 映射;
//   - stop → stop_sequences; reasoning_effort → thinking(enabled + 预算)。
func OpenAIChatToClaudeRequest(model string, body map[string]any, stream bool) (map[string]any, error) {
	if body == nil {
		return nil, errNilBody
	}
	out := map[string]any{
		"model":      model,
		"max_tokens": numOr(body["max_tokens"], 8192),
		"stream":     stream,
	}
	// 采样参数: temperature 与 top_p 互斥时 Anthropic 侧两者都接受, 保持原样。
	if v, ok := body["temperature"]; ok {
		out["temperature"] = v
	}
	if v, ok := body["top_p"]; ok {
		out["top_p"] = v
	}
	if v, ok := body["stop"]; ok {
		switch t := v.(type) {
		case string:
			out["stop_sequences"] = []any{t}
		case []any:
			out["stop_sequences"] = t
		}
	}
	// thinking: OpenAI reasoning_effort → Claude thinking(enabled + budget)
	if effort, ok := body["reasoning_effort"].(string); ok && effort != "" {
		budget := map[string]int{"low": 1024, "medium": 10240, "high": 131072, "max": 131072}[strings.ToLower(effort)]
		if budget > 0 {
			out["thinking"] = map[string]any{"type": "enabled", "budget_tokens": budget}
			// Claude 在 thinking 激活时拒绝 temperature
			delete(out, "temperature")
		}
	}

	var system []any
	var messages []any
	tools, _ := body["tools"].([]any)

	msgs, _ := body["messages"].([]any)
	// P3-25: 连续的 tool 消息合并进同一个 user 轮次。逐条各成一轮会产生连续
	// 多条 user 消息, Anthropic Messages API 对同 role 连续轮次有严格校验, 可能 400。
	// lastToolTurn 记录"由 tool 消息生成"的最后一个 user 轮在 messages 中的下标,
	// 仅当它仍是最后一条时才并入; 非 tool 消息的既有行为不变。
	lastToolTurn := -1
	for _, raw := range msgs {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role, _ := m["role"].(string)
		switch role {
		case "system", "developer":
			system = append(system, systemBlocks(m)...)
		case "user", "assistant":
			messages = append(messages, userAssistantBlock(role, m, tools))
			lastToolTurn = -1
		case "tool":
			// OpenAI 的工具结果消息 → user 轮次里的 tool_result 块
			content, _ := m["content"].(string)
			block := map[string]any{
				"type": "tool_result", "tool_use_id": m["tool_call_id"], "content": content,
			}
			if lastToolTurn >= 0 && lastToolTurn == len(messages)-1 {
				if prev, ok := messages[lastToolTurn].(map[string]any); ok {
					if blocks, ok := prev["content"].([]any); ok {
						prev["content"] = append(blocks, block)
						continue
					}
				}
			}
			messages = append(messages, map[string]any{"role": "user", "content": []any{block}})
			lastToolTurn = len(messages) - 1
		}
	}

	if len(system) > 0 {
		out["system"] = system
	}
	if len(messages) == 0 {
		return nil, errNilMessages
	}
	out["messages"] = messages

	if len(tools) > 0 {
		var claudeTools []any
		for _, t := range tools {
			tm, ok := t.(map[string]any)
			if !ok {
				continue
			}
			fn, _ := tm["function"].(map[string]any)
			if fn == nil {
				continue
			}
			name, _ := fn["name"].(string)
			desc, _ := fn["description"].(string)
			schema, _ := fn["parameters"].(map[string]any)
			if schema == nil {
				schema = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			claudeTools = append(claudeTools, map[string]any{
				"name":         name,
				"description":  desc,
				"input_schema": schema,
			})
		}
		if len(claudeTools) > 0 {
			out["tools"] = claudeTools
		}
	}
	if tc, ok := body["tool_choice"]; ok {
		out["tool_choice"] = convertToolChoice(tc)
	}
	return out, nil
}

// systemBlocks 把 system/developer 消息转为 Anthropic system 块
// (文本段透传, 保留 anthropic 专有的 cache_control 字段)。
func systemBlocks(m map[string]any) []any {
	var out []any
	switch c := m["content"].(type) {
	case string:
		if c != "" {
			out = append(out, map[string]any{"type": "text", "text": c})
		}
	case []any:
		for _, seg := range c {
			sm, ok := seg.(map[string]any)
			if !ok {
				continue
			}
			if txt, ok := sm["text"].(string); ok {
				blk := map[string]any{"type": "text", "text": txt}
				if cc, ok := sm["cache_control"]; ok {
					blk["cache_control"] = cc
				}
				out = append(out, blk)
			}
		}
	}
	return out
}

// userAssistantBlock 把 user/assistant 消息翻译为 Anthropic 消息
// (含图片与工具块的双向映射)。
func userAssistantBlock(role string, m map[string]any, tools []any) map[string]any {
	var blocks []any
	switch c := m["content"].(type) {
	case string:
		if c != "" {
			blocks = append(blocks, map[string]any{"type": "text", "text": c})
		}
	case []any:
		for _, seg := range c {
			sm, ok := seg.(map[string]any)
			if !ok {
				continue
			}
			st, _ := sm["type"].(string)
			switch st {
			case "text":
				if txt, ok := sm["text"].(string); ok && txt != "" {
					blocks = append(blocks, map[string]any{"type": "text", "text": txt})
				}
			case "image_url":
				if iu, ok := sm["image_url"].(map[string]any); ok {
					if u, ok := iu["url"].(string); ok && strings.HasPrefix(u, "data:") {
						// data URL: data:<mime>;base64,<data>
						rest := strings.TrimPrefix(u, "data:")
						mediaType, data, _ := strings.Cut(rest, ";base64,")
						blocks = append(blocks, map[string]any{
							"type": "image",
							"source": map[string]any{
								"type": "base64", "media_type": mediaType, "data": data,
							},
						})
					} else if u, _ := iu["url"].(string); u != "" {
						blocks = append(blocks, map[string]any{
							"type": "image", "source": map[string]any{"type": "url", "url": u},
						})
					}
				}
			}
		}
	}
	// assistant 的 tool_calls → tool_use 块
	if role == "assistant" {
		if tcs, ok := m["tool_calls"].([]any); ok {
			for _, tc := range tcs {
				tcm, ok := tc.(map[string]any)
				if !ok {
					continue
				}
				fn, _ := tcm["function"].(map[string]any)
				if fn == nil {
					continue
				}
				name, _ := fn["name"].(string)
				args, _ := fn["arguments"].(string)
				var parsed any
				if args != "" && json.Valid([]byte(args)) {
					_ = json.Unmarshal([]byte(args), &parsed)
				} else {
					parsed = map[string]any{}
				}
				id, _ := tcm["id"].(string)
				blocks = append(blocks, map[string]any{
					"type": "tool_use", "id": id, "name": name, "input": parsed,
				})
			}
		}
	}
	if len(blocks) == 0 {
		blocks = append(blocks, map[string]any{"type": "text", "text": ""})
	}
	return map[string]any{"role": role, "content": blocks}
}

// convertToolChoice OpenAI tool_choice → Anthropic tool_choice。
func convertToolChoice(tc any) any {
	switch v := tc.(type) {
	case string:
		switch v {
		case "auto":
			return map[string]any{"type": "auto"}
		case "none":
			return map[string]any{"type": "none"} // Anthropic 无 none, 靠不传 tools 实现; 保守映射
		case "required":
			return map[string]any{"type": "any"}
		}
		return map[string]any{"type": "auto"}
	case map[string]any:
		if fn, ok := v["function"].(map[string]any); ok {
			if name, ok := fn["name"].(string); ok && name != "" {
				return map[string]any{"type": "tool", "name": name}
			}
		}
	}
	return map[string]any{"type": "auto"}
}

// numOr 取数值, 缺省用 def。
func numOr(v any, def int) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case int:
		return t
	case json.Number:
		if n, err := t.Int64(); err == nil {
			return int(n)
		}
	}
	return def
}
