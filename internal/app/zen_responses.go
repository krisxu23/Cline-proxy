package app

// zen 上游的 Responses 专用模型适配。
//
// 背景: zen 上游的部分模型(如 muse-spark-*-contributor-free 家族)只在
// /v1/responses 端点提供服务, 走 /v1/chat/completions 会得到上游后端的
// 500 "Internal server error"(上游没有把"端点不支持"翻译成 4xx, 而是直接
// 崩成 500)。opencode 官方客户端对这类模型使用 @ai-sdk/openai(即 Responses
// API), 所以官方能用而网关原来的 chat/completions 通道全出口 502。
//
// 另外免费档要求会话身份头(x-opencode-session / x-opencode-request),
// 缺失时 /responses 返回 400 MissingSessionID —— callZenAPI 本来就带这些头,
// 这里只需沿用。
//
// 策略: 自适应学习。某模型 chat/completions 吃 500 时, 用同一出口/同一会话
// 身份向 /responses 发一次等价请求(请求体做 chat→Responses 转换); 成功则把
// 该模型记入"Responses 专用"名单并持久化, 之后直接走 /responses。负向结论
// (/responses 也失败)只在进程内记住, 不落盘 —— 上游随时可能修复, 不能写死。

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"cline-go-proxy/internal/kit"
)

// anyToInt 把 JSON 数值(int/float64/json.Number/字符串)安全转 int, 失败返回 0。
func anyToInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	case string:
		var i int
		fmt.Sscanf(n, "%d", &i)
		return i
	default:
		return 0
	}
}

// ============ 模型 API 形态登记 ============

var (
	zenRespOnlyMu     sync.Mutex
	zenRespOnlyLoaded bool
	zenRespOnly       = map[string]bool{} // 已确认 Responses 专用(持久化)
	zenChatOnlyMemo   = map[string]bool{} // 已确认 chat 可用/Responses 也失败(仅进程内)
)

func zenResponsesOnlyFile() string {
	if zenResponsesOnlyFileOverride != "" {
		return zenResponsesOnlyFileOverride
	}
	return kit.ResolveDataPath("zen-responses-only.json")
}

// 测试注入点: 把登记文件重定向到临时路径。
var zenResponsesOnlyFileOverride string

func setZenResponsesOnlyFileForTest(path string) {
	zenResponsesOnlyFileOverride = path
}

func loadZenResponsesOnlyLocked() {
	if zenRespOnlyLoaded {
		return
	}
	zenRespOnlyLoaded = true
	raw, err := os.ReadFile(zenResponsesOnlyFile())
	if err != nil || len(raw) == 0 {
		return
	}
	var doc struct {
		Models []string `json:"models"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return
	}
	for _, m := range doc.Models {
		zenRespOnly[m] = true
	}
}

func persistZenResponsesOnlyLocked() {
	models := make([]string, 0, len(zenRespOnly))
	for m := range zenRespOnly {
		models = append(models, m)
	}
	doc := map[string]any{"syncedAt": time.Now().Unix(), "models": models}
	if raw, err := json.Marshal(doc); err == nil {
		_ = kit.WriteFileAtomicDefault(zenResponsesOnlyFile(), raw)
	}
}

// zenStaticResponsesOnly 静态规则: muse 免费档家族(muse-* 且 -free 结尾,
// 如 muse-spark-1.2/1.3-contributor-free)已实测只在上游 /responses 端点
// 提供, chat/completions 一律 500。固定直走, 无需探测。
// 注意范围必须窄: 其他 -free 模型(如 mimo-v2.5-free)在 chat/completions
// 上工作正常, 不能按后缀一刀切。
func zenStaticResponsesOnly(zenModelID string) bool {
	return strings.HasPrefix(zenModelID, "muse-") && strings.HasSuffix(zenModelID, "-free")
}

// zenUseResponsesAPI 该模型是否已知必须走 /responses 端点。
func zenUseResponsesAPI(zenModelID string) bool {
	if zenModelID == "" {
		return false
	}
	if zenStaticResponsesOnly(zenModelID) {
		return true
	}
	zenRespOnlyMu.Lock()
	defer zenRespOnlyMu.Unlock()
	loadZenResponsesOnlyLocked()
	return zenRespOnly[zenModelID]
}

// zenLearnResponsesOnly 成功经 /responses 调通的模型, 持久化登记。
func zenLearnResponsesOnly(zenModelID string) {
	if zenModelID == "" {
		return
	}
	zenRespOnlyMu.Lock()
	defer zenRespOnlyMu.Unlock()
	zenRespOnlyLoaded = true
	if !zenRespOnly[zenModelID] {
		zenRespOnly[zenModelID] = true
		persistZenResponsesOnlyLocked()
	}
	delete(zenChatOnlyMemo, zenModelID)
}

// zenMemoChatOnly /responses 也调不通(或已确认 chat 正常), 进程内记住,
// 本轮进程不再对它做 Responses 回退。
func zenMemoChatOnly(zenModelID string) {
	zenRespOnlyMu.Lock()
	defer zenRespOnlyMu.Unlock()
	zenChatOnlyMemo[zenModelID] = true
}

func zenChatOnlyKnown(zenModelID string) bool {
	zenRespOnlyMu.Lock()
	defer zenRespOnlyMu.Unlock()
	return zenChatOnlyMemo[zenModelID]
}

// ============ 请求转换: OpenAI chat -> Responses ============

// contentText 把消息 content 归一成纯文本。字符串原样; 分块数组拼接全部
// text/input_text/output_text 块(图片等非文本块忽略 —— zen 免费档 MVP 不做视觉)。
func contentText(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		var b strings.Builder
		for _, part := range v {
			pm, ok := part.(map[string]any)
			if !ok {
				continue
			}
			switch pm["type"] {
			case "text", "input_text", "output_text":
				if s, ok := pm["text"].(string); ok {
					b.WriteString(s)
				}
			}
		}
		return b.String()
	default:
		return ""
	}
}

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
	// Responses 端点要求 max_output_tokens >= 16: 推理模型低于 16 的预算
	// 没有意义, 上游直接 400 invalid_request_error, 这里钳到下限。
	for _, key := range []string{"max_tokens", "max_completion_tokens"} {
		if v, ok := chat[key]; ok {
			if n := anyToInt(v); n > 0 {
				if n < 16 {
					n = 16
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

// responsesToChatBody 把非流式 Responses 响应体翻译成 chat completions 响应体。
func responsesToChatBody(r map[string]any) map[string]any {
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
func translateResponsesStreamToChat(src io.Reader, dst io.Writer, model string) error {
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
			if resp, ok := ev["response"].(map[string]any); ok {
				if u, ok := resp["usage"].(map[string]any); ok {
					usageOut = map[string]any{
						"prompt_tokens":     u["input_tokens"],
						"completion_tokens": u["output_tokens"],
						"total_tokens":      u["total_tokens"],
					}
				}
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
func convertResponsesResponseToChat(resp *http.Response, fallbackModel string) (*http.Response, error) {
	raw := kit.ReadBody(resp)
	resp.Body.Close()
	var parsed map[string]any
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return nil, fmt.Errorf("decode zen responses body: %w", err)
	}
	out, err := json.Marshal(responsesToChatBody(parsed))
	if err != nil {
		return nil, fmt.Errorf("marshal converted chat body: %w", err)
	}
	return synthesizeChatJSONResponse(out), nil
}

// wrapResponsesStreamToChat 流式: 把 Responses SSE 体翻译成 chat SSE 体。
func wrapResponsesStreamToChat(resp *http.Response, model string) *http.Response {
	pr, pw := io.Pipe()
	go func() {
		err := translateResponsesStreamToChat(resp.Body, pw, model)
		resp.Body.Close()
		pw.CloseWithError(err)
	}()
	return synthesizeChatSSEResponse(pr)
}

// tryZenResponsesFallback chat/completions 吃 500 时的自适应回退: 用同一出口、
// 同一会话身份向 /responses 发一次等价请求。成功 -> 登记该模型为 Responses
// 专用(持久化)并返回合成好的 chat 形态响应; 失败 -> 返回 nil, 调用方继续
// 原有的换出口重试流程。
func tryZenResponsesFallback(ctx context.Context, base string, chatBody map[string]any, stream bool, client *http.Client) *http.Response {
	modelID, _ := chatBody["model"].(string)
	if modelID == "" || zenChatOnlyKnown(modelID) {
		return nil
	}
	respBody := chatBodyToResponsesBody(chatBody)
	raw, err := json.Marshal(respBody)
	if err != nil {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, "POST", base+"/responses", bytes.NewReader(raw))
	if err != nil {
		return nil
	}
	sess, user, ua := kit.FreshZenIdentity()
	req.Header.Set("Authorization", "Bearer "+getZenConfig().Key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", ua)
	req.Header.Set("x-opencode-session", sess)
	req.Header.Set("x-opencode-request", user)
	req.Header.Set("x-opencode-client", "cli")
	req.Header.Set("x-opencode-model", modelID)

	resp, err := client.Do(req)
	if err != nil {
		return nil // 网络问题不记负向结论, 换出口后 chat 重试照旧
	}
	if resp.StatusCode != http.StatusOK {
		raw := kit.ReadBody(resp)
		resp.Body.Close()
		// 4xx 说明模型/端点层面明确不买账(而非出口线路问题), 进程内记住
		// 不再对它回退; 5xx 可能只是这条线路坏, 不记。
		if resp.StatusCode < 500 {
			zenMemoChatOnly(modelID)
		}
		log.Printf("  zen: model %s 的 /responses 回退未命中(%d): %s", modelID, resp.StatusCode, kit.Truncate(raw, 200))
		return nil
	}
	log.Printf("  zen: model %s 只在 /responses 端点提供服务, 已自动切换并登记", modelID)
	zenLearnResponsesOnly(modelID)
	if stream {
		return wrapResponsesStreamToChat(resp, modelID)
	}
	conv, err := convertResponsesResponseToChat(resp, modelID)
	if err != nil {
		log.Printf("  zen: model %s /responses 响应转换失败: %v", modelID, err)
		return nil
	}
	return conv
}
