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
	// 热路径快路径: SSE 每帧都过这里, 绝大多数帧既无 provider/proxy_metadata
	// 也无需补 content —— 先零分配探测, 无事可改就原样返回(调用方只读:
	// providers/clinepass 序列化即弃, app/clinepass 只读取字段)。
	if !chunkNeedsNormalize(obj) {
		return obj
	}
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

// hasStripKeys 该对象是否含需剥离的元数据键。
func hasStripKeys(m map[string]any) bool {
	_, p := m["provider_metadata"]
	_, q := m["proxy_metadata"]
	return p || q
}

// contentNeedsFix tool_calls 非空而 content 缺失(nil)时需补空串。
// SanitizeContent 目前是恒等变换, 探测无需覆盖它; 若未来实现真实清洗,
// 快路径必须同步接入, 否则无 strip 键的帧会被跳过清洗。
func contentNeedsFix(delta map[string]any) bool {
	if tc, ok := delta["tool_calls"].([]any); ok && len(tc) > 0 && delta["content"] == nil {
		return true
	}
	return false
}

// chunkNeedsNormalize 零分配探测: 归一化是否会产生任何可见改动。
// 与 NormalizeOpenAIChunk 的拷贝分支逐条件对应 —— 两处必须同步维护。
func chunkNeedsNormalize(obj map[string]any) bool {
	if hasStripKeys(obj) {
		return true
	}
	choices, ok := obj["choices"].([]any)
	if !ok {
		return false
	}
	for _, ch := range choices {
		c, ok := ch.(map[string]any)
		if !ok {
			continue
		}
		if hasStripKeys(c) {
			return true
		}
		if msg, ok := c["message"].(map[string]any); ok && (hasStripKeys(msg) || contentNeedsFix(msg)) {
			return true
		}
		if delta, ok := c["delta"].(map[string]any); ok && (hasStripKeys(delta) || contentNeedsFix(delta)) {
			return true
		}
	}
	return false
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

// HasStopSignal 跨协议终止判定(参照 OmniRoute 的 checkIfStopSignal, MIT;
// Go 侧实现)。中继用它决定"上游是不是已经结束了" —— 只认 OpenAI 的
// finish_reason 时, 混入其它协议终止形态的上游会让网关一直等到超时:
//
//	OpenAI    choices[].finish_reason
//	Gemini    candidates[].finishReason / finish_reason
//	Anthropic content_block_stop / message_stop / message_delta.stop_reason
//	Responses response.done / .completed / .cancelled / .failed
func HasStopSignal(obj map[string]any) bool {
	if HasFinishReason(obj) {
		return true
	}
	if cds, ok := obj["candidates"].([]any); ok {
		for _, cd := range cds {
			m, ok := cd.(map[string]any)
			if !ok {
				continue
			}
			for _, k := range []string{"finishReason", "finish_reason"} {
				if v, present := m[k]; present && v != nil && v != "" {
					return true
				}
			}
		}
	}
	if t, ok := obj["type"].(string); ok {
		switch t {
		case "content_block_stop", "message_stop",
			"response.done", "response.completed", "response.cancelled", "response.failed":
			return true
		case "message_delta":
			if d, ok := obj["delta"].(map[string]any); ok {
				if v, present := d["stop_reason"]; present && v != nil && v != "" {
					return true
				}
			}
		}
	}
	return false
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
