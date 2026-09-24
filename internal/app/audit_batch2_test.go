package app

// 2026-09-24 审查: OpenAI → Anthropic 响应的 usage 映射此前整块丢掉缓存 token。

import "testing"

func TestOpenAIToAnthropicKeepsCacheTokens(t *testing.T) {
	out := openAIToAnthropicWithMap(map[string]any{
		"choices": []any{map[string]any{
			"message":       map[string]any{"role": "assistant", "content": "hi"},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{
			"prompt_tokens":               100,
			"completion_tokens":           20,
			"total_tokens":                120,
			"prompt_tokens_details":       map[string]any{"cached_tokens": 64},
			"cache_creation_input_tokens": 8,
		},
	}, nil, nil)

	u, ok := out["usage"].(map[string]any)
	if !ok {
		t.Fatalf("usage 缺失: %+v", out["usage"])
	}
	if u["input_tokens"] != 100 || u["output_tokens"] != 20 {
		t.Fatalf("基础字段映射错: %+v", u)
	}
	if u["cache_read_input_tokens"] != 64 {
		t.Fatalf("缓存命中量应映射到 cache_read_input_tokens(客户端靠它算命中率): %+v", u)
	}
	if u["cache_creation_input_tokens"] != 8 {
		t.Fatalf("缓存写入量应透传: %+v", u)
	}
	// Anthropic 的 usage 里**没有** total_tokens —— 映射过去反而是非法字段。
	if _, has := u["total_tokens"]; has {
		t.Fatalf("Anthropic 无 total_tokens 字段, 不该映射: %+v", u)
	}
}
