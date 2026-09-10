package protocol

import (
	"encoding/json"
	"strings"
)

// ResponsesInput represents a value in a Responses API `input` field; it
// is either a string or an array of items. We decode lazily because
// Responses clients are free to mix shapes.
type ResponsesInput = any

// ResponsesToChat converts an OpenAI Responses API request body (already
// parsed as map[string]any) into a Chat Completions request body. The
// conversion follows the same logic as the existing internal/app/responses.go
// in the host project so behavior is identical for Cursor clients.
func ResponsesToChat(body map[string]any) map[string]any {
	out := map[string]any{}
	if m, ok := body["model"].(string); ok {
		out["model"] = m
	}
	if s, ok := body["stream"].(bool); ok {
		out["stream"] = s
	}
	if mt, ok := body["max_output_tokens"].(float64); ok {
		out["max_tokens"] = int(mt)
	}
	for _, k := range []string{"temperature", "top_p", "stop", "seed", "user", "metadata", "logit_bias"} {
		if v, ok := body[k]; ok {
			out[k] = v
		}
	}
	if instr, ok := body["instructions"].(string); ok && instr != "" {
		msgs := []any{map[string]any{"role": "system", "content": instr}}
		msgs = append(msgs, responsesInputToMessages(body["input"])...)
		out["messages"] = msgs
	} else {
		out["messages"] = responsesInputToMessages(body["input"])
	}
	if tools, ok := body["tools"].([]any); ok {
		out["tools"] = responsesToolsToChat(tools)
	}
	if tc, ok := body["tool_choice"]; ok {
		out["tool_choice"] = tc
	}
	return out
}

func responsesInputToMessages(input any) []any {
	var msgs []any
	switch v := input.(type) {
	case string:
		msgs = append(msgs, map[string]any{"role": "user", "content": v})
	case []any:
		for _, raw := range v {
			m, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			switch m["type"] {
			case "message":
				role, _ := m["role"].(string)
				if role == "" {
					role = "user"
				}
				msgs = append(msgs, map[string]any{"role": role, "content": stringifyResponsesContent(m["content"])})
			case "function_call":
				callID, _ := m["call_id"].(string)
				if callID == "" {
					callID, _ = m["id"].(string)
				}
				name, _ := m["name"].(string)
				args := ""
				switch a := m["arguments"].(type) {
				case string:
					args = a
				case map[string]any:
					if b, err := json.Marshal(a); err == nil {
						args = string(b)
					}
				}
				msgs = append(msgs, map[string]any{
					"role":       "assistant",
					"content":    "",
					"tool_calls": []any{map[string]any{"id": callID, "type": "function", "function": map[string]any{"name": name, "arguments": args}}},
				})
			case "function_call_output":
				callID, _ := m["call_id"].(string)
				if callID == "" {
					callID, _ = m["id"].(string)
				}
				output := ""
				switch o := m["output"].(type) {
				case string:
					output = o
				case map[string]any:
					if b, err := json.Marshal(o); err == nil {
						output = string(b)
					}
				}
				msgs = append(msgs, map[string]any{"role": "tool", "content": output, "tool_call_id": callID})
			case "reasoning":
				// Reasoning input items cannot be replayed as Chat messages.
			}
		}
	}
	return msgs
}

func stringifyResponsesContent(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		parts := []string{}
		for _, block := range v {
			if b, ok := block.(map[string]any); ok {
				if t, ok := b["text"].(string); ok {
					parts = append(parts, t)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func responsesToolsToChat(tools []any) []any {
	out := make([]any, 0, len(tools))
	for _, t := range tools {
		tm, ok := t.(map[string]any)
		if !ok {
			continue
		}
		if tm["type"] == "function" {
			fn := map[string]any{}
			if n, ok := tm["name"].(string); ok {
				fn["name"] = n
			}
			if d, ok := tm["description"].(string); ok {
				fn["description"] = d
			}
			if p, ok := tm["parameters"].(map[string]any); ok {
				fn["parameters"] = p
			}
			out = append(out, map[string]any{"type": "function", "function": fn})
		}
	}
	return out
}

// ChatToResponses converts a Chat Completions response (parsed map) to a
// Responses API response. This is the inverse of ResponsesToChat for the
// non-streaming path; streaming conversion lives in the SSE emitter.
func ChatToResponses(chat map[string]any) map[string]any {
	resp := map[string]any{
		"id":          "resp_" + toHex(NowMillis()),
		"object":      "response",
		"created_at":  NowSecs(),
		"status":      "completed",
		"model":       chat["model"],
		"output":      []any{},
		"output_text": "",
	}
	choices, _ := chat["choices"].([]any)
	outputs := []any{}
	var outputText strings.Builder
	if len(choices) > 0 {
		if ch, ok := choices[0].(map[string]any); ok {
			msg, _ := ch["message"].(map[string]any)
			if msg == nil {
				msg, _ = ch["delta"].(map[string]any)
			}
			content := []any{}
			if msg != nil {
				if c, ok := msg["content"].(string); ok && c != "" {
					outputText.WriteString(c)
					content = append(content, map[string]any{"type": "output_text", "text": c, "annotations": []any{}})
				}
			}
			msgOut := map[string]any{
				"type":        "message",
				"id":          "msg_" + toHex(NowMillis()),
				"status":      "completed",
				"role":        "assistant",
				"content":     content,
				"output_text": outputText.String(),
			}
			outputs = append(outputs, msgOut)
			if msg != nil {
				if tc, ok := msg["tool_calls"].([]any); ok {
					for _, c := range tc {
						if cm, ok := c.(map[string]any); ok {
							fn, _ := cm["function"].(map[string]any)
							callID, _ := cm["id"].(string)
							if callID == "" {
								callID = "fc_" + toHex(NowMillis())
							}
							name := ""
							args := ""
							if fn != nil {
								name, _ = fn["name"].(string)
								if a, ok := fn["arguments"].(string); ok {
									args = a
								}
							}
							outputs = append(outputs, map[string]any{
								"type":      "function_call",
								"id":        "fc_" + toHex(NowMillis()),
								"call_id":   callID,
								"name":      name,
								"arguments": args,
								"status":    "completed",
							})
						}
					}
				}
			}
		}
	}
	resp["output"] = outputs
	resp["output_text"] = outputText.String()
	if u, ok := chat["usage"].(map[string]any); ok {
		details := map[string]any{}
		if pd, ok := u["prompt_tokens_details"].(map[string]any); ok {
			details["cached_tokens"] = pd["cached_tokens"]
		}
		od := map[string]any{}
		if rd, ok := u["reasoning_tokens"]; ok {
			od["reasoning_tokens"] = rd
		}
		resp["usage"] = map[string]any{
			"input_tokens":          u["prompt_tokens"],
			"input_tokens_details":  details,
			"output_tokens":         u["completion_tokens"],
			"output_tokens_details": od,
			"total_tokens":          u["total_tokens"],
		}
	}
	return resp
}
