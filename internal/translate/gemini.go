package translate

// Gemini ↔ OpenAI 双向翻译 (参照 OmniRoute open-sse/translator 的
// request/openai-to-gemini.ts 与 response/gemini-to-openai.ts, MIT;
// Go 侧重写)。覆盖 Gemini generateContent 的核心协议:
//
//	请求: contents[{role: user|model, parts: [{text},{inlineData}]}],
//	      systemInstruction, generationConfig(temperature/maxOutputTokens/
//	      stopSequences), tools[{functionDeclarations}], toolConfig(mode)
//	响应: candidates[{content:{parts:[{text},{functionCall:{name,args}}]},
//	      finishReason}], usageMetadata{promptTokenCount,candidatesTokenCount,
//	      totalTokenCount}
//	流式: 每行一个 JSON 对象(可带 data: 前缀), 逐行翻译为 chat 分块。

import (
	"bufio"
	"encoding/json"
	"io"
	"strconv"
	"strings"
)

// ---------- 请求方向: openai → gemini ----------

// OpenAIChatToGeminiRequest 把 OpenAI chat 请求体翻译为 Gemini
// generateContent 请求体。要点:
//   - system/developer 消息 → systemInstruction;
//   - role=user 保持 user, role=assistant → model;
//   - image_url data URL → inlineData{mimeType,data};
//   - assistant.tool_calls → model 轮的 functionCall part(无 OpenAI 对应物,
//     Gemini 由模型自己发起 functionCall);
//   - role=tool → user 轮 functionResponse part;
//   - tools[].function.parameters → tools.functionDeclarations;
//   - tool_choice: auto→AUTO / required→ANY / 具名→ANY(+allowedFunctionNames);
//   - stop → stop_sequences; max_tokens → maxOutputTokens。
func OpenAIChatToGeminiRequest(model string, body map[string]any, stream bool) (map[string]any, error) {
	if body == nil {
		return nil, errNilBody
	}
	out := map[string]any{}
	var sysParts []any
	var contents []any

	msgs, _ := body["messages"].([]any)
	for _, raw := range msgs {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role, _ := m["role"].(string)
		if role == "system" || role == "developer" {
			if t, ok := m["content"].(string); ok && t != "" {
				sysParts = append(sysParts, map[string]any{"text": t})
			}
			continue
		}
		gRole := "user"
		if role == "assistant" {
			gRole = "model"
		}
		var parts []any
		switch c := m["content"].(type) {
		case string:
			if c != "" {
				parts = append(parts, map[string]any{"text": c})
			}
		case []any:
			for _, seg := range c {
				sm, ok := seg.(map[string]any)
				if !ok {
					continue
				}
				switch sm["type"] {
				case "text":
					if t, ok := sm["text"].(string); ok && t != "" {
						parts = append(parts, map[string]any{"text": t})
					}
				case "image_url":
					if iu, ok := sm["image_url"].(map[string]any); ok {
						if u, ok := iu["url"].(string); ok && strings.HasPrefix(u, "data:") {
							rest := strings.TrimPrefix(u, "data:")
							mime, data, _ := strings.Cut(rest, ";base64,")
							parts = append(parts, map[string]any{
								"inlineData": map[string]any{"mimeType": mime, "data": data},
							})
						}
					}
				}
			}
		}
		// role=tool → functionResponse; assistant.tool_calls → functionCall
		if role == "tool" {
			content, _ := m["content"].(string)
			parts = append(parts, map[string]any{"functionResponse": map[string]any{
				"name": m["tool_call_id"], "response": map[string]any{"result": content},
			}})
		}
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
					var args map[string]any
					if a, ok := fn["arguments"].(string); ok && a != "" {
						_ = json.Unmarshal([]byte(a), &args)
					}
					if args == nil {
						args = map[string]any{}
					}
					parts = append(parts, map[string]any{"functionCall": map[string]any{
						"name": name, "args": args,
					}})
				}
			}
		}
		if len(parts) == 0 {
			parts = append(parts, map[string]any{"text": ""})
		}
		contents = append(contents, map[string]any{"role": gRole, "parts": parts})
	}

	if len(sysParts) > 0 {
		out["systemInstruction"] = map[string]any{"parts": sysParts}
	}
	if len(contents) == 0 {
		return nil, errNilMessages
	}
	out["contents"] = contents

	gc := map[string]any{}
	if v, ok := body["temperature"]; ok {
		gc["temperature"] = v
	}
	if v, ok := body["top_p"]; ok {
		gc["topP"] = v
	}
	if mt := numOr(body["max_tokens"], 0); mt > 0 {
		gc["maxOutputTokens"] = mt
	}
	if v, ok := body["stop"]; ok {
		switch t := v.(type) {
		case string:
			gc["stopSequences"] = []any{t}
		case []any:
			gc["stopSequences"] = t
		}
	}
	if len(gc) > 0 {
		out["generationConfig"] = gc
	}

	if tools, ok := body["tools"].([]any); ok && len(tools) > 0 {
		var decls []any
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
			decls = append(decls, map[string]any{
				"name": name, "description": desc, "parameters": schema,
			})
		}
		if len(decls) > 0 {
			out["tools"] = []any{map[string]any{"functionDeclarations": decls}}
		}
	}
	if tc, ok := body["tool_choice"]; ok {
		mode := "AUTO"
		var allowed []string
		switch v := tc.(type) {
		case string:
			if v == "required" {
				mode = "ANY"
			} else if v == "none" {
				mode = "NONE"
			}
		case map[string]any:
			if fn, ok := v["function"].(map[string]any); ok {
				if name, ok := fn["name"].(string); ok && name != "" {
					mode = "ANY"
					allowed = []string{name}
				}
			}
		}
		out["toolConfig"] = map[string]any{"functionCallingConfig": func() map[string]any {
			if len(allowed) > 0 {
				return map[string]any{"mode": mode, "allowedFunctionNames": allowed}
			}
			return map[string]any{"mode": mode}
		}()}
	}
	return out, nil
}

// ---------- 响应方向: gemini → openai ----------

// geminiFinishToOpenAI Gemini finishReason → OpenAI finish_reason。
func geminiFinishToOpenAI(fr string) string {
	switch fr {
	case "STOP":
		return "stop"
	case "MAX_TOKENS":
		return "length"
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII":
		return "content_filter"
	default:
		if fr == "" {
			return ""
		}
		return "stop"
	}
}

// geminiCandidatesToOpenAI 从 candidates 提取 (message, finish_reason)。
func geminiCandidatesToOpenAI(cands []any) (map[string]any, string) {
	var text strings.Builder
	var toolCalls []any
	finish := ""
	if len(cands) > 0 {
		c, _ := cands[0].(map[string]any)
		if c != nil {
			if content, ok := c["content"].(map[string]any); ok {
				parts, _ := content["parts"].([]any)
				for _, p := range parts {
					pm, ok := p.(map[string]any)
					if !ok {
						continue
					}
					if t, ok := pm["text"].(string); ok {
						text.WriteString(t)
					}
					if fc, ok := pm["functionCall"].(map[string]any); ok {
						name, _ := fc["name"].(string)
						argsB, _ := json.Marshal(fc["args"])
						// Gemini 原生不带调用 id。此前用**名字**占位 —— 并行调用同一个
						// 工具时两条 tool_call 同 id, tool_result 配对错乱, compact 按
						// id 去重还会误删(2026-09-24 审查)。改为每次生成唯一 id。
						id := newToolCallID()
						toolCalls = append(toolCalls, map[string]any{
							"id": id, "type": "function",
							"function": map[string]any{"name": name, "arguments": string(argsB)},
						})
					}
				}
			}
			if fr, ok := c["finishReason"].(string); ok {
				finish = geminiFinishToOpenAI(fr)
			}
		}
	}
	message := map[string]any{"role": "assistant", "content": text.String()}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
		if finish == "stop" || finish == "" {
			finish = "tool_calls"
		}
	}
	return message, finish
}

// GeminiResponseToOpenAIChat 非流式: Gemini generateContent body → chat.completion。
func GeminiResponseToOpenAIChat(body []byte) (map[string]any, error) {
	var src map[string]any
	if err := json.Unmarshal(body, &src); err != nil {
		return nil, err
	}
	cands, _ := src["candidates"].([]any)
	message, finish := geminiCandidatesToOpenAI(cands)
	out := map[string]any{
		"id": "gemini-" + fmtInt(timeSeed()), "object": "chat.completion",
		"created": timeSeed(), "model": src["modelVersion"],
		"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}},
	}
	if u, ok := src["usageMetadata"].(map[string]any); ok {
		out["usage"] = map[string]any{
			"prompt_tokens":     u["promptTokenCount"],
			"completion_tokens": u["candidatesTokenCount"],
			"total_tokens":      u["totalTokenCount"],
		}
	}
	return out, nil
}

// GeminiSSEToOpenAISSE 流式: Gemini SSE(每帧一个候选 JSON) → chat SSE。
func GeminiSSEToOpenAISSE(src io.Reader, dst io.Writer, model string) error {
	reader := bufio.NewReaderSize(src, 64*1024)
	writer := bufio.NewWriter(dst)
	defer writer.Flush()
	chunkID := "chatcmpl-gem-" + fmtInt(timeSeed())
	roleSent := false

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

	for {
		line, err := reader.ReadString('\n')
		if len(line) == 0 && err != nil {
			break
		}
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimRight(line, "\r\n"), "data: "))
		if line == "" {
			if err != nil {
				break
			}
			continue
		}
		var frame map[string]any
		if json.Unmarshal([]byte(line), &frame) != nil {
			if err != nil {
				break
			}
			continue
		}
		if !roleSent {
			roleSent = true
			if err := writeChunk(map[string]any{"choices": []any{map[string]any{
				"index": 0, "delta": map[string]any{"role": "assistant", "content": ""},
			}}}); err != nil {
				return err
			}
		}
		message, finish := geminiCandidatesToOpenAI(frameCandidates(frame))
		delta, _ := message["content"].(string)
		// 安全断言: 缺 tool_calls 键时不得 panic(纯文本流式帧很常见)。
		tcs, hasTCs := message["tool_calls"].([]any)
		if delta != "" || len(tcs) > 0 {
			d := map[string]any{"content": delta}
			if hasTCs {
				// OpenAI 流式协议靠 tool_calls[].index 让客户端把分片拼到同一个调用上。
				// 缺 index 时客户端会把每次出现的 tool_call 当成**新的**, arguments
				// 直接拼成非法 JSON(2026-09-24 审查)。按出现顺序补 index。
				for i, tc := range tcs {
					if m, ok := tc.(map[string]any); ok {
						if _, has := m["index"]; !has {
							m["index"] = i
						}
					}
				}
				d["tool_calls"] = tcs
			}
			if err := writeChunk(map[string]any{"choices": []any{map[string]any{
				"index": 0, "delta": d,
			}}}); err != nil {
				return err
			}
		}
		if finish != "" {
			if err := writeChunk(map[string]any{"choices": []any{map[string]any{
				"index": 0, "delta": map[string]any{}, "finish_reason": finish,
			}}}); err != nil {
				return err
			}
		}
		if u, ok := frame["usageMetadata"].(map[string]any); ok {
			if err := writeChunk(map[string]any{"choices": []any{}, "usage": u}); err != nil {
				return err
			}
		}
		if err != nil {
			break
		}
	}
	_, err := writer.WriteString("data: [DONE]\n\n")
	return err
}

// frameCandidates 取流式帧里的 candidates 数组。
func frameCandidates(frame map[string]any) []any {
	c, _ := frame["candidates"].([]any)
	return c
}

// timeSeed 秒级时间戳(供合成 id)。
func timeSeed() int64 { return timeNowUnixNano() / 1e9 }

// fmtInt 整数转字符串(合成 id 用)。
func fmtInt(v int64) string { return strconv.FormatInt(v, 10) }
