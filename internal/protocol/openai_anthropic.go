package protocol

import (
	"encoding/json"
	"strings"
)

// AnthropicMessage is a single element of the Anthropic Messages API
// `messages` array. Content is decoded lazily because Anthropic clients
// send either a string or an array of content blocks.
type AnthropicMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

// AnthropicRequest is the wire shape of POST /v1/messages.
type AnthropicRequest struct {
	Model       string           `json:"model"`
	MaxTokens   int              `json:"max_tokens"`
	Messages    []AnthropicMessage `json:"messages"`
	System      json.RawMessage  `json:"system,omitempty"`
	Stream      bool             `json:"stream,omitempty"`
	Temperature float64          `json:"temperature,omitempty"`
	TopP        float64          `json:"top_p,omitempty"`
	TopK        int              `json:"top_k,omitempty"`
	Stop        json.RawMessage  `json:"stop_sequences,omitempty"`
	Tools       json.RawMessage  `json:"tools,omitempty"`
	ToolChoice  json.RawMessage  `json:"tool_choice,omitempty"`
	Metadata    json.RawMessage  `json:"metadata,omitempty"`
}

// AnthropicToOpenAIRequest converts an Anthropic Messages request to a
// Chat Completions request that the upstream (Cline or zen) understands.
//
// Borrowed from defyma/cline-proxy: their anthropicToOpenAiRequest drops
// image blocks silently and flattens tool_use to OpenAI's function-call
// shape. We reproduce the same shape here because the existing Cline-proxy
// already validated this translation against Cline's actual upstream.
func AnthropicToOpenAIRequest(req AnthropicRequest) map[string]any {
	out := map[string]any{
		"model":      req.Model,
		"max_tokens": req.MaxTokens,
		"stream":     req.Stream,
		"messages":   []any{},
	}
	if req.Temperature != 0 {
		out["temperature"] = req.Temperature
	}
	if req.TopP != 0 {
		out["top_p"] = req.TopP
	}
	if req.TopK != 0 {
		out["top_k"] = req.TopK
	}

	if len(req.Tools) > 0 {
		var toolsArr []any
		if err := json.Unmarshal(req.Tools, &toolsArr); err == nil {
			out["tools"] = AnthropicToolsToOpenAI(toolsArr)
		}
	}
	if len(req.ToolChoice) > 0 {
		var tc any
		if err := json.Unmarshal(req.ToolChoice, &tc); err == nil {
			out["tool_choice"] = AnthropicToolChoiceToOpenAI(tc)
		}
	}

	msgs := []any{}
	if sys := anthropicContentToText(req.System); sys != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": sys})
	}
	for _, m := range req.Messages {
		msgs = append(msgs, anthropicMessageToOpenAIMessages(m)...)
	}
	out["messages"] = msgs
	return out
}

func AnthropicToolsToOpenAI(tools []any) []any {
	out := make([]any, 0, len(tools))
	for _, t := range tools {
		tm, ok := t.(map[string]any)
		if !ok {
			continue
		}
		if tm["type"] == "function" {
			out = append(out, t)
			continue
		}
		out = append(out, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        tm["name"],
				"description": tm["description"],
				"parameters":  tm["input_schema"],
			},
		})
	}
	return out
}

func AnthropicToolChoiceToOpenAI(tc any) any {
	if s, ok := tc.(string); ok {
		switch s {
		case "any":
			return "required"
		case "auto", "none":
			return s
		}
		return s
	}
	m, ok := tc.(map[string]any)
	if !ok {
		return tc
	}
	switch m["type"] {
	case "auto", "none":
		return m["type"]
	case "any":
		return "required"
	case "tool":
		if name, _ := m["name"].(string); name != "" {
			return map[string]any{"type": "function", "function": map[string]any{"name": name}}
		}
	}
	return "auto"
}

func anthropicMessageToOpenAIMessages(m AnthropicMessage) []any {
	if m.Role == "assistant" {
		return []any{anthropicAssistantToOpenAI(m)}
	}
	return anthropicUserToOpenAI(m)
}

func anthropicAssistantToOpenAI(m AnthropicMessage) map[string]any {
	blocks, ok := m.Content.([]any)
	if !ok {
		return map[string]any{"role": "assistant", "content": anthropicContentToText(m.Content)}
	}
	var textParts []string
	var toolCalls []any
	for _, b := range blocks {
		bm, ok := b.(map[string]any)
		if !ok {
			continue
		}
		switch bm["type"] {
		case "text":
			if s, ok := bm["text"].(string); ok {
				textParts = append(textParts, s)
			}
		case "tool_use":
			args := "{}"
			switch v := bm["input"].(type) {
			case string:
				args = v
			case map[string]any, []any:
				if bts, err := json.Marshal(v); err == nil {
					args = string(bts)
				}
			}
			toolCalls = append(toolCalls, map[string]any{
				"id":   bm["id"],
				"type": "function",
				"function": map[string]any{
					"name":      bm["name"],
					"arguments": args,
				},
			})
		}
	}
	out := map[string]any{"role": "assistant", "content": strings.Join(textParts, "\n")}
	if len(toolCalls) > 0 {
		out["tool_calls"] = toolCalls
	}
	return out
}

func anthropicUserToOpenAI(m AnthropicMessage) []any {
	blocks, ok := m.Content.([]any)
	if !ok {
		return []any{map[string]any{"role": m.Role, "content": anthropicContentToText(m.Content)}}
	}
	var textParts []string
	var toolResults []map[string]any
	for _, b := range blocks {
		bm, ok := b.(map[string]any)
		if !ok {
			continue
		}
		switch bm["type"] {
		case "text":
			if s, ok := bm["text"].(string); ok {
				textParts = append(textParts, s)
			}
		case "image":
			// Cline upstream ignores images; skip to avoid validation errors.
		case "tool_result":
			id, _ := bm["tool_use_id"].(string)
			if id == "" {
				continue
			}
			toolResults = append(toolResults, map[string]any{
				"role":         "tool",
				"content":      anthropicContentToText(bm["content"]),
				"tool_call_id": id,
			})
		}
	}
	out := []any{}
	if m.Role == "user" && len(toolResults) > 0 {
		for _, tr := range toolResults {
			out = append(out, tr)
		}
		if len(textParts) > 0 {
			out = append(out, map[string]any{"role": "user", "content": strings.Join(textParts, "\n")})
		}
	} else {
		out = append(out, map[string]any{"role": m.Role, "content": strings.Join(textParts, "\n")})
	}
	return out
}

func anthropicContentToText(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	// Handle json.RawMessage from unmarshal
	if rm, ok := v.(json.RawMessage); ok {
		// Try to unmarshal as string first
		var s string
		if err := json.Unmarshal(rm, &s); err == nil {
			return s
		}
		// Fallback to array of blocks
		var blocks []map[string]any
		if err := json.Unmarshal(rm, &blocks); err == nil {
			return blocksToText(blocks)
		}
		return string(rm)
	}
	arr, ok := v.([]any)
	if !ok {
		return ""
	}
	blocks := make([]map[string]any, 0, len(arr))
	for _, e := range arr {
		if m, ok := e.(map[string]any); ok {
			blocks = append(blocks, m)
		}
	}
	return blocksToText(blocks)
}

func blocksToText(blocks []map[string]any) string {
	parts := []string{}
	for _, b := range blocks {
		if b == nil {
			continue
		}
		if t, ok := b["text"].(string); ok {
			parts = append(parts, t)
		}
		if b["type"] == "tool_result" {
			if inner := anthropicContentToText(b["content"]); inner != "" {
				parts = append(parts, inner)
			}
		}
	}
	return strings.Join(parts, "\n")
}

// OpenAIToAnthropicResponse converts a Chat Completions response (parsed
// map) into an Anthropic Messages response shape. The conversion handles
// tool_calls -> tool_use blocks, content strings, and the finish_reason
// mapping borrowed from defyma/cline-proxy.
func OpenAIToAnthropicResponse(openAI map[string]any, fallbackModel string) map[string]any {
	out := map[string]any{
		"id":    "msg_" + toHex(NowMillis()),
		"type":  "message",
		"role":  "assistant",
		"model": fallbackModel,
	}

	choice0, _ := getNested(openAI, "choices", 0).(map[string]any)
	if choice0 == nil {
		out["content"] = []any{map[string]any{"type": "text", "text": ""}}
		out["stop_reason"] = "end_turn"
		out["usage"] = map[string]any{"input_tokens": 0, "output_tokens": 0}
		return out
	}
	msg, _ := choice0["message"].(map[string]any)
	if msg == nil {
		msg, _ = choice0["delta"].(map[string]any)
	}

	text := ""
	var contentBlocks []any
	if msg != nil {
		if s, ok := msg["content"].(string); ok {
			text = s
		}
	}
	if text != "" {
		contentBlocks = append(contentBlocks, map[string]any{"type": "text", "text": SanitizeContent(text)})
	}
	if msg != nil {
		if tc, ok := msg["tool_calls"].([]any); ok && len(tc) > 0 {
			for _, item := range tc {
				tcm, ok := item.(map[string]any)
				if !ok {
					continue
				}
				fn, _ := tcm["function"].(map[string]any)
				if fn == nil {
					continue
				}
				name, _ := fn["name"].(string)
				if name == "" {
					continue
				}
				id, _ := tcm["id"].(string)
				if id == "" {
					id = "toolu_" + toHex(NowMillis())
				}
				input := fn["arguments"]
				if argsStr, ok := input.(string); ok {
					var parsed any
					if err := json.Unmarshal([]byte(argsStr), &parsed); err == nil {
						input = parsed
					}
				}
				if input == nil {
					input = map[string]any{}
				}
				contentBlocks = append(contentBlocks, map[string]any{
					"type":  "tool_use",
					"id":    id,
					"name":  name,
					"input": input,
				})
			}
		}
	}
	if len(contentBlocks) == 0 {
		contentBlocks = []any{map[string]any{"type": "text", "text": ""}}
	}
	out["content"] = contentBlocks
	out["stop_reason"] = ReasonToAnthropicStop(stringOf(getNested(choice0, "finish_reason")))

	usage := map[string]any{}
	if u, ok := getNested(openAI, "usage").(map[string]any); ok {
		usage["input_tokens"] = u["prompt_tokens"]
		usage["output_tokens"] = u["completion_tokens"]
	} else {
		usage["input_tokens"] = 0
		usage["output_tokens"] = 0
	}
	out["usage"] = usage
	return out
}
