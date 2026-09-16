package app

import (
	"bufio"
	"bytes"
	"cline-go-proxy/internal/kit"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// chatBodyToResponsesBody 把 OpenAI chat completions 请求体翻译成 Responses 请求体。
func chatBodyToResponsesBody(chat map[string]any) map[string]any {
	out := map[string]any{}
	if m, ok := chat["model"].(string); ok && m != "" {
		out["model"] = m
	}
	if s, ok := chat["stream"].(bool); ok && s {
		out["stream"] = true
	}
	for _, key := range []string{"temperature", "top_p"} {
		if v, ok := chat[key]; ok {
			out[key] = v
		}
	}
	// max_tokens / max_completion_tokens -> max_output_tokens
	//
	// 两道闸门(参照 OmniRoute 的 MUSE_SPARK_MIN_OUTPUT_TOKENS):
	//   1) 上游硬下限 16: 低于 16 直接 400 invalid_request_error;
	//   2) muse-spark 家族抬高到 512: 它的隐藏推理会先吃掉预算, 预算太小会让
	//      正文恒为空(实测 300/800 都出不了正文), 客户端会以为模型坏了。
	//      只在调用方给了预算且小于 512 时抬高; 没给预算就不合成(与上游默认一致)。
	for _, key := range []string{"max_tokens", "max_completion_tokens"} {
		if v, ok := chat[key]; ok {
			if n := anyToInt(v); n > 0 {
				if n < 16 {
					n = 16
				}
				if n < museSparkMinOutputTokens {
					if mid, _ := chat["model"].(string); isMuseSparkModel(mid) {
						n = museSparkMinOutputTokens
					}
				}
				out["max_output_tokens"] = n
			}
			break
		}
	}

	msgs, _ := chat["messages"].([]any)
	input := make([]any, 0, len(msgs))
	instructions := ""
	for _, mi := range msgs {
		m, ok := mi.(map[string]any)
		if !ok {
			continue
		}
		role, _ := m["role"].(string)
		text := contentText(m["content"])
		switch role {
		case "system", "developer":
			if instructions != "" {
				instructions += "\n\n"
			}
			instructions += text
		case "user":
			input = append(input, map[string]any{"role": "user", "content": text})
		case "assistant":
			if text != "" {
				input = append(input, map[string]any{"role": "assistant", "content": text})
			}
			if tcs, ok := m["tool_calls"].([]any); ok {
				for _, ti := range tcs {
					tc, ok := ti.(map[string]any)
					if !ok {
						continue
					}
					fn, _ := tc["function"].(map[string]any)
					args, _ := fn["arguments"].(string)
					input = append(input, map[string]any{
						"type":      "function_call",
						"call_id":   tc["id"],
						"name":      fn["name"],
						"arguments": args,
					})
				}
			}
		case "tool":
			input = append(input, map[string]any{
				"type":    "function_call_output",
				"call_id": m["tool_call_id"],
				"output":  text,
			})
		}
	}
	out["input"] = input
	if instructions != "" {
		out["instructions"] = instructions
	}

	// tools: chat 的 {type:function, function:{name,description,parameters}}
	// -> Responses 的扁平 {type:function, name, description, parameters}
	if tools, ok := chat["tools"].([]any); ok {
		conv := make([]any, 0, len(tools))
		for _, ti := range tools {
			t, ok := ti.(map[string]any)
			if !ok {
				continue
			}
			fn, _ := t["function"].(map[string]any)
			if fn == nil {
				conv = append(conv, t) // 已是扁平形态的原样保留
				continue
			}
			conv = append(conv, map[string]any{
				"type":        "function",
				"name":        fn["name"],
				"description": fn["description"],
				"parameters":  fn["parameters"],
			})
		}
		out["tools"] = conv
	}
	// tool_choice: 字符串原样; {type:function,function:{name}} -> {type:function,name}
	if tc, ok := chat["tool_choice"].(map[string]any); ok {
		fn, _ := tc["function"].(map[string]any)
		if fn != nil {
			out["tool_choice"] = map[string]any{"type": "function", "name": fn["name"]}
		}
	} else if tc, ok := chat["tool_choice"].(string); ok {
		out["tool_choice"] = tc
	}
	return out
}

// ============ 响应转换: Responses -> OpenAI chat ============

// usageFieldInt 从响应体的 usage 对象里取整数字段(兼容 float64 / int 两种解码形态)。
func usageFieldInt(r map[string]any, keys ...string) (int, bool) {
	u, ok := r["usage"].(map[string]any)
	if !ok {
		return 0, false
	}
	for _, k := range keys {
		if v, exists := u[k]; exists {
			switch n := v.(type) {
			case float64:
				return int(n), true
			case int:
				return n, true
			}
		}
	}
	return 0, false
}

// modelIDOfResponsesBody 取 Responses 响应体里的模型名。
func modelIDOfResponsesBody(r map[string]any) string {
	s, _ := r["model"].(string)
	return s
}

// responsesToChatBody 把 Responses 响应体翻译成 chat completions 响应体。
// 可选参数 requestedBudget 是客户端请求的 max_tokens, 用于判断 finish_reason
// 是否属于"推理吃掉预算导致的假截断"(见 normalizeMuseSparkFinish)。
func responsesToChatBody(r map[string]any, requestedBudget ...int) map[string]any {
	model, _ := r["model"].(string)
	var text strings.Builder
	var toolCalls []any
	if output, ok := r["output"].([]any); ok {
		for _, oi := range output {
			item, ok := oi.(map[string]any)
			if !ok {
				continue
			}
			switch item["type"] {
			case "message":
				if parts, ok := item["content"].([]any); ok {
					for _, pi := range parts {
						pm, ok := pi.(map[string]any)
						if !ok {
							continue
						}
						if pm["type"] == "output_text" {
							if s, ok := pm["text"].(string); ok {
								text.WriteString(s)
							}
						}
					}
				}
			case "function_call":
				name, _ := item["name"].(string)
				args, _ := item["arguments"].(string)
				callID, _ := item["call_id"].(string)
				if callID == "" {
					callID, _ = item["id"].(string)
				}
				toolCalls = append(toolCalls, map[string]any{
					"id":   callID,
					"type": "function",
					"function": map[string]any{
						"name":      name,
						"arguments": args,
					},
				})
			}
		}
	}

	finish := "stop"
	if inc, ok := r["incomplete_details"].(map[string]any); ok {
		if inc["reason"] == "max_output_tokens" {
			finish = "length"
		}
	}
	// muse-spark 的假截断修正(需要完成量, 所以放在 usage 解析之后)。
	completion, _ := usageFieldInt(r, "output_tokens", "completion_tokens")
	budget := 0
	if len(requestedBudget) > 0 {
		budget = requestedBudget[0]
	}
	finish = normalizeMuseSparkFinish(finish, modelIDOfResponsesBody(r), completion, budget)
	// tool_calls 优先于 length: 截断前已经产生了完整的函数调用项,
	// 客户端需要执行它, 报 length 会让客户端丢弃本该执行的工具调用。
	if len(toolCalls) > 0 {
		finish = "tool_calls"
	}

	message := map[string]any{"role": "assistant"}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	} else {
		message["content"] = text.String()
	}

	choice := map[string]any{"index": 0, "message": message, "finish_reason": finish}
	chat := map[string]any{
		"id":      r["id"],
		"object":  "chat.completion",
		"created": r["created_at"],
		"model":   model,
		"choices": []any{choice},
	}
	if u, ok := r["usage"].(map[string]any); ok {
		chat["usage"] = map[string]any{
			"prompt_tokens":     u["input_tokens"],
			"completion_tokens": u["output_tokens"],
			"total_tokens":      u["total_tokens"],
		}
	}
	return chat
}

// ============ 流式翻译: Responses SSE -> chat SSE ============

// translateResponsesStreamToChat 把 /responses 的 SSE 事件流实时翻译成
// chat completions 的 SSE 分块。事件覆盖: 文本增量、函数调用(声明+参数增量)、
// 完成(usage/finish_reason)、失败。
// translateResponsesStreamToChat 把 Responses 的 SSE 流逐事件翻译成 chat SSE 流。
// requestedBudget 用于 muse-spark 的 finish_reason 假截断修正(见 normalizeMuseSparkFinish)。
func translateResponsesStreamToChat(src io.Reader, dst io.Writer, model string, requestedBudget ...int) error {
	reader := bufio.NewReaderSize(src, 64*1024)
	writer := bufio.NewWriter(dst)

	chunkID := "chatcmpl-resp-" + fmt.Sprint(time.Now().UnixNano())
	toolIdx := map[string]int{} // item_id -> chat tool_calls 下标
	nextToolIdx := 0
	finishReason := ""

	writeChunk := func(obj map[string]any) error {
		obj["id"] = chunkID
		obj["object"] = "chat.completion.chunk"
		obj["model"] = model
		line, err := json.Marshal(obj)
		if err != nil {
			return err
		}
		if _, err := writer.WriteString("data: " + string(line) + "\n\n"); err != nil {
			return err
		}
		return nil
	}

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
		if payload == "" || payload == "[DONE]" {
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
			continue
		}
		evType, _ := ev["type"].(string)

		switch evType {
		case "response.output_text.delta":
			delta, _ := ev["delta"].(string)
			if delta != "" {
				if err := writeChunk(map[string]any{
					"choices": []any{map[string]any{
						"index": 0, "delta": map[string]any{"content": delta},
					}},
				}); err != nil {
					return err
				}
			}
		case "response.output_item.added":
			item, _ := ev["item"].(map[string]any)
			if item != nil && item["type"] == "function_call" {
				itemID, _ := item["id"].(string)
				callID, _ := item["call_id"].(string)
				if callID == "" {
					callID = itemID
				}
				name, _ := item["name"].(string)
				idx := nextToolIdx
				nextToolIdx++
				toolIdx[itemID] = idx
				if err := writeChunk(map[string]any{
					"choices": []any{map[string]any{
						"index": 0,
						"delta": map[string]any{"tool_calls": []any{map[string]any{
							"index": idx, "id": callID, "type": "function",
							"function": map[string]any{"name": name, "arguments": ""},
						}}},
					}},
				}); err != nil {
					return err
				}
			}
		case "response.function_call_arguments.delta":
			itemID, _ := ev["item_id"].(string)
			idx := toolIdx[itemID]
			delta, _ := ev["delta"].(string)
			if delta != "" {
				if err := writeChunk(map[string]any{
					"choices": []any{map[string]any{
						"index": 0,
						"delta": map[string]any{"tool_calls": []any{map[string]any{
							"index": idx, "function": map[string]any{"arguments": delta},
						}}},
					}},
				}); err != nil {
					return err
				}
			}
		case "response.completed", "response.incomplete":
			finishReason = "stop"
			if evType == "response.incomplete" {
				finishReason = "length"
			}
			usageOut := any(nil)
			completion := 0
			if resp, ok := ev["response"].(map[string]any); ok {
				if u, ok := resp["usage"].(map[string]any); ok {
					usageOut = map[string]any{
						"prompt_tokens":     u["input_tokens"],
						"completion_tokens": u["output_tokens"],
						"total_tokens":      u["total_tokens"],
					}
					if n, isNum := u["output_tokens"].(float64); isNum {
						completion = int(n)
					}
				}
			}
			if evType == "response.incomplete" {
				budget := 0
				if len(requestedBudget) > 0 {
					budget = requestedBudget[0]
				}
				finishReason = normalizeMuseSparkFinish(finishReason, model, completion, budget)
			}
			final := map[string]any{
				"choices": []any{map[string]any{
					"index": 0, "delta": map[string]any{}, "finish_reason": finishReason,
				}},
			}
			if usageOut != nil {
				final["usage"] = usageOut
			}
			if err := writeChunk(final); err != nil {
				return err
			}
			_, err := writer.WriteString("data: [DONE]\n\n")
			if err != nil {
				return err
			}
			_ = writer.Flush()
			return nil
		case "response.failed", "error":
			// 上游在流中途失败: 补一个 finish 让客户端正常收尾, 不再续传。
			if err := writeChunk(map[string]any{
				"choices": []any{map[string]any{
					"index": 0, "delta": map[string]any{}, "finish_reason": "stop",
				}},
			}); err != nil {
				return err
			}
			_, werr := writer.WriteString("data: [DONE]\n\n")
			if werr != nil {
				return werr
			}
			_ = writer.Flush()
			return nil
		}

		if err != nil {
			break
		}
	}
	_ = writer.Flush()
	return nil
}

// ============ 合成 chat 形态的 http.Response ============

func synthesizeChatJSONResponse(body []byte) *http.Response {
	hdr := http.Header{}
	hdr.Set("Content-Type", "application/json")
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Proto:      "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header: hdr,
		Body:   io.NopCloser(bytes.NewReader(body)),
	}
}

func synthesizeChatSSEResponse(pipeReader io.Reader) *http.Response {
	hdr := http.Header{}
	hdr.Set("Content-Type", "text/event-stream")
	hdr.Set("Cache-Control", "no-cache")
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Proto:      "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header: hdr,
		Body:   io.NopCloser(pipeReader),
	}
}

// convertResponsesResponseToChat 非流式: 读 Responses 响应体, 翻译成 chat JSON 响应。
// convertResponsesResponseToChat 非流式: 拉全响应体, 翻译回 chat completions 形状。
// requestedBudget 透传给 finish_reason 归一(见 normalizeMuseSparkFinish)。
func convertResponsesResponseToChat(resp *http.Response, fallbackModel string, requestedBudget ...int) (*http.Response, error) {
	raw := kit.ReadBody(resp)
	resp.Body.Close()
	var parsed map[string]any
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return nil, fmt.Errorf("decode zen responses body: %w", err)
	}
	budget := 0
	if len(requestedBudget) > 0 {
		budget = requestedBudget[0]
	}
	out, err := json.Marshal(responsesToChatBody(parsed, budget))
	if err != nil {
		return nil, fmt.Errorf("marshal converted chat body: %w", err)
	}
	return synthesizeChatJSONResponse(out), nil
}

// wrapResponsesStreamToChat 流式: 把 Responses SSE 体翻译成 chat SSE 体。
// wrapResponsesStreamToChat 流式: 把 Responses SSE 体翻译成 chat SSE 体。
// requestedBudget 透传给 finish_reason 归一。
func wrapResponsesStreamToChat(resp *http.Response, model string, requestedBudget ...int) *http.Response {
	pr, pw := io.Pipe()
	go func() {
		err := translateResponsesStreamToChat(resp.Body, pw, model, requestedBudget...)
		resp.Body.Close()
		pw.CloseWithError(err)
	}()
	return synthesizeChatSSEResponse(pr)
}
