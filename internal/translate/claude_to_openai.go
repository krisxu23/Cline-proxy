package translate

// Anthropic(Claude Messages) → OpenAI chat 的响应方向翻译
// (参照 OmniRoute open-sse/translator/response/claude-to-openai.ts, MIT;
// Go 侧重写)。覆盖: 非流式 body 与 SSE 事件流两条路径。

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// claudeStopReasonToOpenAI Anthropic stop_reason → OpenAI finish_reason。
func claudeStopReasonToOpenAI(sr string) string {
	switch sr {
	case "end_turn", "stop_sequence":
		return "stop"
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	default:
		if sr == "" {
			return ""
		}
		return "stop"
	}
}

// ClaudeResponseToOpenAIChat 非流式: Anthropic Messages body → chat.completion。
func ClaudeResponseToOpenAIChat(body []byte) (map[string]any, error) {
	var src map[string]any
	if err := json.Unmarshal(body, &src); err != nil {
		return nil, err
	}
	id, _ := src["id"].(string)
	model, _ := src["model"].(string)
	content, _ := src["content"].([]any)

	var text strings.Builder
	var toolCalls []any
	for _, blk := range content {
		bm, ok := blk.(map[string]any)
		if !ok {
			continue
		}
		switch bm["type"] {
		case "text":
			if t, ok := bm["text"].(string); ok {
				text.WriteString(t)
			}
		case "tool_use":
			input := bm["input"]
			args, _ := json.Marshal(input)
			idv, _ := bm["id"].(string)
			name, _ := bm["name"].(string)
			// 缺 id 必须补一个, 不能空 id 直透: 客户端对 tool_calls 完整性有硬
			// 校验, 缺 id 会整体报错(2026-09-24 审查)。app 侧 repairToolCalls
			// 兜了一道, 但源头补上更稳(流式路径未必经过它)。
			if strings.TrimSpace(idv) == "" {
				idv = newToolCallID()
			}
			toolCalls = append(toolCalls, map[string]any{
				"id": idv, "type": "function",
				"function": map[string]any{"name": name, "arguments": string(args)},
			})
		}
	}
	message := map[string]any{"role": "assistant", "content": text.String()}
	finish := ""
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
		finish = "tool_calls"
	}
	sr, _ := src["stop_reason"].(string)
	if f := claudeStopReasonToOpenAI(sr); f != "" {
		finish = f
	}
	out := map[string]any{
		"id": id, "object": "chat.completion", "model": model,
		"choices": []any{map[string]any{
			"index": 0, "message": message, "finish_reason": finish,
		}},
	}
	if u, ok := src["usage"].(map[string]any); ok {
		out["usage"] = map[string]any{
			"prompt_tokens":     u["input_tokens"],
			"completion_tokens": u["output_tokens"],
			"total_tokens":      addNums(u["input_tokens"], u["output_tokens"]),
		}
	}
	return out, nil
}

// addNums 两数相加(JSON 数值兼容)。
func addNums(a, b any) int {
	sum := 0
	for _, v := range []any{a, b} {
		switch t := v.(type) {
		case float64:
			sum += int(t)
		case int:
			sum += t
		}
	}
	return sum
}

// claudeUsageToOpenAI Anthropic usage(input_tokens/output_tokens) → OpenAI
// usage(prompt_tokens/completion_tokens/total_tokens)。Anthropic 的键名原样透传
// 会让 OpenAI 客户端读不到任何 token 数, 必须映射后再投递。
func claudeUsageToOpenAI(u map[string]any) map[string]any {
	return map[string]any{
		"prompt_tokens":     u["input_tokens"],
		"completion_tokens": u["output_tokens"],
		"total_tokens":      addNums(u["input_tokens"], u["output_tokens"]),
	}
}

// ClaudeSSEToOpenAISSE 流式: Anthropic Messages SSE → chat completions SSE。
// 事件覆盖: message_start, content_block_start(tool_use 声明),
// content_block_delta(text_delta/input_json_delta/thinking_delta),
// message_delta(stop_reason+usage), message_stop。结尾补 [DONE]。
func ClaudeSSEToOpenAISSE(src io.Reader, dst io.Writer, model string) error {
	reader := bufio.NewReaderSize(src, 64*1024)
	writer := bufio.NewWriter(dst)
	defer writer.Flush()

	chunkID := "chatcmpl-claude-" + fmt.Sprintf("%d", timeNowUnixNano())
	writeChunk := func(obj map[string]any) error {
		obj["id"] = chunkID
		obj["object"] = "chat.completion.chunk"
		obj["model"] = model
		b, err := json.Marshal(obj)
		if err != nil {
			return err
		}
		_, err = writer.WriteString("data: " + string(b) + "\n\n")
		return err
	}

	// tool_use 块: index → 调用 id/name(来自 content_block_start)
	toolMeta := map[int]map[string]any{} // index -> {"id","name"}
	roleSent := false
	sawStop := false
	// message_start 帧的 input_tokens: message_delta 的 usage 只带 output_tokens,
	// 末帧合并后再映射输出, 否则最后一帧(OpenAI 客户端取帧)会变成
	// prompt_tokens:null、total=completion, 把 message_start 的正确值覆盖掉。
	var startInputTokens any

	for {
		line, err := reader.ReadString('\n')
		if len(line) == 0 && err != nil {
			break
		}
		line = strings.TrimRight(line, "\r\n")
		if !strings.HasPrefix(line, "data: ") {
			if err != nil {
				break
			}
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data: "))
		if payload == "" {
			if err != nil {
				break
			}
			continue
		}
		var ev map[string]any
		if json.Unmarshal([]byte(payload), &ev) != nil {
			if err != nil {
				break
			}
			continue // 坏行丢弃(与网关中继同一纪律)
		}
		evType, _ := ev["type"].(string)
		idx := 0
		if v, ok := ev["index"].(float64); ok {
			idx = int(v)
		}

		switch evType {
		case "message_start":
			if !roleSent {
				roleSent = true
				if err := writeChunk(map[string]any{"choices": []any{map[string]any{
					"index": 0, "delta": map[string]any{"role": "assistant", "content": ""},
				}}}); err != nil {
					return err
				}
			}
			// message_start.usage → 输入侧 token: 必须真正投递一帧 usage
			// (此前只拷进局部 ev、从不写出, 输入 token 永不到端), 键名同步映射
			// 为 OpenAI 形态。
			if u, ok := ev["message"].(map[string]any); ok {
				if uu, ok := u["usage"].(map[string]any); ok {
					if it, ok := uu["input_tokens"]; ok {
						startInputTokens = it
					}
					if err := writeChunk(map[string]any{
						"choices": []any{},
						"usage":   claudeUsageToOpenAI(uu),
					}); err != nil {
						return err
					}
				}
			}
		case "content_block_start":
			bm, _ := ev["content_block"].(map[string]any)
			bt, _ := bm["type"].(string)
			if bt == "tool_use" {
				idv, _ := bm["id"].(string)
				name, _ := bm["name"].(string)
				// 同非流式: 缺 id 补一个, 空 id 直透会让客户端整体报错。
				if strings.TrimSpace(idv) == "" {
					idv = newToolCallID()
				}
				toolMeta[idx] = map[string]any{"id": idv, "name": name}
				if err := writeChunk(map[string]any{"choices": []any{map[string]any{
					"index": 0, "delta": map[string]any{"tool_calls": []any{map[string]any{
						"index": idx, "id": idv, "type": "function", "function": map[string]any{"name": name, "arguments": ""},
					}}},
				}}}); err != nil {
					return err
				}
			}
		case "content_block_delta":
			dm, _ := ev["delta"].(map[string]any)
			dt, _ := dm["type"].(string)
			delta := map[string]any{}
			switch dt {
			case "text_delta":
				if t, ok := dm["text"].(string); ok && t != "" {
					delta["content"] = t
				}
			case "thinking_delta":
				if t, ok := dm["thinking"].(string); ok && t != "" {
					delta["reasoning_content"] = t
				}
			case "input_json_delta":
				if pj, ok := dm["partial_json"].(string); ok && pj != "" {
					delta["tool_calls"] = []any{map[string]any{
						"index": idx, "function": map[string]any{"arguments": pj},
					}}
				}
			}
			if len(delta) > 0 {
				if err := writeChunk(map[string]any{"choices": []any{map[string]any{
					"index": 0, "delta": delta,
				}}}); err != nil {
					return err
				}
			}
		case "message_delta":
			dm, _ := ev["delta"].(map[string]any)
			sr, _ := dm["stop_reason"].(string)
			finish := claudeStopReasonToOpenAI(sr)
			if len(toolMeta) > 0 && finish == "stop" {
				finish = "tool_calls"
			}
			sawStop = true
			// usage 键名必须映射为 OpenAI 形态, 原样透传 Anthropic 键名
			// (output_tokens)会让客户端读不到任何 token 数。
			var usageOut any
			if u, ok := ev["usage"].(map[string]any); ok {
				// 末帧合并: message_delta 通常只带 output_tokens, 补上
				// message_start 记住的 input_tokens → prompt+completion+total 齐全。
				merged := u
				if _, has := u["input_tokens"]; !has && startInputTokens != nil {
					merged = make(map[string]any, len(u)+1)
					for k, v := range u {
						merged[k] = v
					}
					merged["input_tokens"] = startInputTokens
				}
				usageOut = claudeUsageToOpenAI(merged)
			}
			if err := writeChunk(map[string]any{
				"choices": []any{map[string]any{
					"index": 0, "delta": map[string]any{}, "finish_reason": finish,
				}},
				"usage": usageOut,
			}); err != nil {
				return err
			}
		}
		if err != nil {
			break
		}
	}
	if !sawStop {
		// 上游未给终止事件: 合成 finish + [DONE](对齐网关中继同一纪律)
		if err := writeChunk(map[string]any{"choices": []any{map[string]any{
			"index": 0, "delta": map[string]any{}, "finish_reason": "stop",
		}}}); err != nil {
			return err
		}
	}
	_, err := writer.WriteString("data: [DONE]\n\n")
	return err
}

// timeNowUnixNano 供 chunkID。
func timeNowUnixNano() int64 { return time.Now().UnixNano() }
