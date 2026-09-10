package protocol

// NormalizeOpenAIChunk cleans an upstream OpenAI-format chunk or response
// so downstream clients see a consistent shape:
//   - drops provider/proxy metadata keys at the top level, in each choice,
//     in message and in delta objects;
//   - ensures delta/content is a string (never nil) when tool_calls present.
//
// Moved from internal/app (normalizeOpenAIResponse) so protocol consumers
// (providers, SSE relays) can share it. Pure function: input is unchanged.
func NormalizeOpenAIChunk(obj map[string]any) map[string]any {
	out := make(map[string]any, len(obj))
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
				nc := make(map[string]any, len(c))
				for k, v := range c {
					if k == "provider_metadata" || k == "proxy_metadata" {
						continue
					}
					nc[k] = v
				}
				if msg, ok := nc["message"].(map[string]any); ok {
					nc["message"] = normalizeMsg(msg)
				}
				if delta, ok := nc["delta"].(map[string]any); ok {
					nd := make(map[string]any, len(delta))
					for k, v := range delta {
						if k == "provider_metadata" || k == "proxy_metadata" {
							continue
						}
						nd[k] = v
					}
					if tc, ok := nd["tool_calls"].([]any); ok && len(tc) > 0 {
						if nd["content"] == nil {
							nd["content"] = ""
						}
					}
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

func normalizeMsg(msg map[string]any) map[string]any {
	out := make(map[string]any, len(msg))
	for k, v := range msg {
		if k == "provider_metadata" || k == "proxy_metadata" {
			continue
		}
		out[k] = v
	}
	if tc, ok := out["tool_calls"].([]any); ok && len(tc) > 0 {
		if out["content"] == nil {
			out["content"] = ""
		}
	}
	if c, ok := out["content"].(string); ok {
		out["content"] = SanitizeContent(c)
	}
	return out
}

// HasChoices reports whether a parsed SSE payload carries a non-empty
// choices array. Used to decide whether an empty fallback chunk is needed.
func HasChoices(obj map[string]any) bool {
	choices, ok := obj["choices"].([]any)
	return ok && len(choices) > 0
}

// HasFinishReason reports whether any choice in a parsed OpenAI chunk
// carries a non-null finish_reason. Used to detect streams that end
// without a terminal chunk so a synthetic stop chunk can be appended.
func HasFinishReason(obj map[string]any) bool {
	choices, ok := obj["choices"].([]any)
	if !ok {
		return false
	}
	for _, ch := range choices {
		if c, ok := ch.(map[string]any); ok {
			if fr, present := c["finish_reason"]; present && fr != nil {
				return true
			}
		}
	}
	return false
}
