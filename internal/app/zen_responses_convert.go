package app

import (
	"bufio"
	"bytes"
	"cline-go-proxy/internal/kit"
	"encoding/json"
	"fmt"
	"io"
	"log"
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
		// muse-spark 家族: 调用方**没给**预算时也必须补一个兜底 —— 否则上游默认预算
		// 会被隐藏推理吃光, 收到"完成"但可见正文为空, 客户端视为静默中断
		// (见 museSparkDefaultOutputTokens 的实测说明)。
		if mid, _ := chat["model"].(string); mid != "" {
			applyMuseSparkBudget(out, mid)
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
	delivered := 0      // 已交付的可见内容量(正文字符 + 工具调用), 用于识别空回合
	reasoningChars := 0 // 只推理未出正文时的诊断线索
	sawCompletion := false

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

	// writeTerminalError 向客户端交付"可见的失败": 同一个帧里既带 error(客户端
	// 能弹出原因), 又带 finish_reason(不依赖 error 的客户端也能正常收尾),
	// 再补 [DONE]。这样中继的收尾合成不会再追加第二份终止帧。
	//
	// 为什么不直接伪装成功(旧行为): 上游失败/空回合时写 finish_reason=stop,
	// 客户端会当成"模型这一轮没话说了", agent 于是静默结束任务 —— 用户看到的是
	// "无缘无故中断, 没有任何提示"(2026-09-16 定位)。
	writeTerminalError := func(kind, msg string) error {
		frame := map[string]any{
			"choices": []any{map[string]any{
				"index": 0, "delta": map[string]any{}, "finish_reason": "stop",
			}},
			"error": map[string]any{"type": kind, "message": msg},
		}
		if err := writeChunk(frame); err != nil {
			return err
		}
		if _, err := writer.WriteString("data: [DONE]\n\n"); err != nil {
			return err
		}
		return writer.Flush()
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
				delivered += len(delta)
				if err := writeChunk(map[string]any{
					"choices": []any{map[string]any{
						"index": 0, "delta": map[string]any{"content": delta},
					}},
				}); err != nil {
					return err
				}
			}
		case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
			// 服务端推理/推理摘要: 不算"交付内容"(客户端要的是正文或工具调用),
			// 但记一笔 —— 出现"只有推理、没有正文"的空回合时这是关键诊断线索。
			if d, _ := ev["delta"].(string); d != "" {
				reasoningChars += len(d)
			}
		case "response.output_item.added":
			item, _ := ev["item"].(map[string]any)
			if item != nil && item["type"] == "function_call" {
				delivered++ // 工具调用本身就是一次有效交付(agent 靠它继续干活)
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
			sawCompletion = true
			// 空回合必须是"可见的失败", 不能伪装成正常收尾 —— 否则 agent 拿到
			// 一个没有正文、没有工具调用的回合就直接结束任务, 用户侧表现为
			// "无缘无故中断, 没有任何提示"(2026-09-16 实证: 当天 394 次成功里 12 次)。
			if delivered == 0 {
				log.Printf("  zen: /responses 空回合(model=%s completion_tokens=%d reasoning_chars=%d 可见输出 0) — 已向客户端报错而非静默结束",
					model, completion, reasoningChars)
				return writeTerminalError("upstream_empty_response",
					fmt.Sprintf("上游模型 %s 本轮没有产出任何可见内容(completion_tokens=%d, 推理字符=%d)。"+
						"该家族会把预算烧在隐藏推理上, 通常是输出预算不足或上游配额/地区问题; 请重试, 若持续出现请检查 zen 配额与出口地区。",
						model, completion, reasoningChars))
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
			// 上游在流中途失败: 不再伪装成"正常收尾"(旧实现写 finish_reason=stop,
			// 客户端以为模型答完了 → 任务静默中断)。改为交付一个带原因的错误帧,
			// 已经流出去的正文保持不变, 客户端能弹出真实原因。
			msg := "上游流中途失败"
			if e, ok := ev["error"].(map[string]any); ok {
				if m, _ := e["message"].(string); m != "" {
					msg = m
				}
			} else if r, ok := ev["response"].(map[string]any); ok {
				if m, _ := r["error"].(map[string]any); ok {
					if s, _ := m["message"].(string); s != "" {
						msg = s
					}
				}
			}
			log.Printf("  zen: /responses 流中途失败(model=%s, 已交付=%d): %s", model, delivered, kit.Truncate(msg, 200))
			return writeTerminalError("upstream_stream_failed",
				fmt.Sprintf("上游流在交付过程中失败(model=%s): %s", model, msg))
		}

		if err != nil {
			break
		}
	}
	// 循环正常结束但从未收到 response.completed/incomplete: 上游把连接掐了。
	// 同样不能静默收尾 —— 已交付内容保留, 补一个可见的错误帧。
	if !sawCompletion {
		log.Printf("  zen: /responses 流被上游截断(model=%s, 已交付=%d, 推理字符=%d)", model, delivered, reasoningChars)
		return writeTerminalError("upstream_stream_truncated",
			fmt.Sprintf("上游流被意外截断(model=%s, 已交付=%d 字节内容); 通常是出口节点或上游 worker 掉线, 请重试。", model, delivered))
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

// synthesizeChatSSEResponse 合成 SSE 形态的 http.Response。
//
// Body 必须直接用 *io.PipeReader(它自带有效 Close): 此前包了一层
// io.NopCloser, Close() 是空操作, 调用方的 defer resp.Body.Close() 全部失效 ——
// 客户端断流后写侧 io.Pipe 同步写永久阻塞, 转换 goroutine 与上游连接每次断流
// 各泄漏一条, 上游 resp.Body 永远不关(P1-2)。消费方 Close 后写侧收到
// ErrClosedPipe, goroutine 关掉上游 body 即退出。
func synthesizeChatSSEResponse(pipeReader *io.PipeReader) *http.Response {
	hdr := http.Header{}
	hdr.Set("Content-Type", "text/event-stream")
	hdr.Set("Cache-Control", "no-cache")
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Proto:      "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header: hdr,
		Body:   pipeReader,
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
	chat := responsesToChatBody(parsed, budget)
	// 非流式同样要拦住"空回合": 转出来既没有正文也没有工具调用时, 直接报错让
	// 调用方换出口/换候选, 而不是把一个空消息交付给客户端(那会让 agent 静默结束)。
	if responsesChatEmpty(chat) {
		return nil, fmt.Errorf("上游 %s 返回空回合(无正文且无工具调用): 通常是输出预算不足或上游配额/地区问题", fallbackModel)
	}
	out, err := json.Marshal(chat)
	if err != nil {
		return nil, fmt.Errorf("marshal converted chat body: %w", err)
	}
	return synthesizeChatJSONResponse(out), nil
}

// responsesChatEmpty 转换后的 chat 响应是否"什么都没产出"。
func responsesChatEmpty(chat map[string]any) bool {
	choices, _ := chat["choices"].([]any)
	if len(choices) == 0 {
		return true
	}
	c0, _ := choices[0].(map[string]any)
	if c0 == nil {
		return true
	}
	msg, _ := c0["message"].(map[string]any)
	if msg == nil {
		return true
	}
	if s, _ := msg["content"].(string); strings.TrimSpace(s) != "" {
		return false
	}
	if tcs, _ := msg["tool_calls"].([]any); len(tcs) > 0 {
		return false
	}
	if fr, _ := msg["function_call"].(map[string]any); fr != nil {
		return false
	}
	return true
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
