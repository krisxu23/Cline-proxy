package app

import "testing"

func TestThoughtSignatureCacheLRU(t *testing.T) {
	cache := newThoughtSignatureCache(2)
	cache.remember("a", "1")
	cache.remember("b", "2")
	cache.remember("c", "3")
	if cache.lookup("a") != "" {
		t.Fatal("oldest entry must be evicted")
	}
	if cache.lookup("b") != "2" || cache.lookup("c") != "3" {
		t.Fatal("recent entries must survive")
	}
	// 空白值不入缓存
	cache.remember("d", "   ")
	if cache.lookup("d") != "" {
		t.Fatal("blank signature must not be stored")
	}
}

func TestThoughtSignatureReadWrite(t *testing.T) {
	call := map[string]any{"id": "c1"}
	writeThoughtSignature(call, "sig-1")
	if got := readThoughtSignature(call); got != "sig-1" {
		t.Fatalf("read after write: %q", got)
	}
	// 也识别扁平字段
	flat := map[string]any{"thought_signature": "sig-2"}
	if got := readThoughtSignature(flat); got != "sig-2" {
		t.Fatalf("flat field: %q", got)
	}
	alt := map[string]any{"extra_content": map[string]any{"google": map[string]any{"thoughtSignature": "sig-3"}}}
	if got := readThoughtSignature(alt); got != "sig-3" {
		t.Fatalf("camelCase field: %q", got)
	}
	if readThoughtSignature(nil) != "" {
		t.Fatal("nil call must read empty")
	}
}

func TestInjectThoughtSignatures(t *testing.T) {
	cache := newThoughtSignatureCache(10)
	cache.remember("known", "sig-known")
	body := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "assistant", "tool_calls": []any{
				map[string]any{"id": "known"},
				map[string]any{"id": "unknown"},
			}},
		},
	}
	injectThoughtSignatures(body, cache)
	msgs := body["messages"].([]any)
	calls := msgs[1].(map[string]any)["tool_calls"].([]any)
	if got := readThoughtSignature(calls[0].(map[string]any)); got != "sig-known" {
		t.Fatalf("cached signature: %q", got)
	}
	if got := readThoughtSignature(calls[1].(map[string]any)); got != skipThoughtSignature {
		t.Fatalf("sentinel for unknown: %q", got)
	}
	// 已有签名不被覆盖
	body2 := map[string]any{"messages": []any{
		map[string]any{"role": "assistant", "tool_calls": []any{
			map[string]any{"id": "x", "extra_content": map[string]any{"google": map[string]any{"thought_signature": "keep"}}},
		}},
	}}
	injectThoughtSignatures(body2, cache)
	calls2 := body2["messages"].([]any)[0].(map[string]any)["tool_calls"].([]any)
	if got := readThoughtSignature(calls2[0].(map[string]any)); got != "keep" {
		t.Fatalf("existing signature must be preserved: %q", got)
	}
}

func TestRememberSignaturesFromPayload(t *testing.T) {
	cache := newThoughtSignatureCache(10)
	payload := map[string]any{"choices": []any{
		map[string]any{"message": map[string]any{"tool_calls": []any{
			map[string]any{"id": "t1", "extra_content": map[string]any{"google": map[string]any{"thought_signature": "s1"}}},
		}}},
	}}
	rememberSignaturesFromPayload(payload, cache)
	if got := cache.lookup("t1"); got != "s1" {
		t.Fatalf("remembered: %q", got)
	}
}

func TestSignatureStreamExtractor(t *testing.T) {
	cache := newThoughtSignatureCache(10)
	ext := newSignatureStreamExtractor(cache)
	// id 与签名分两条到达(真实流式可能拆开)
	ext.push("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"st1\"}]}}]}\n")
	ext.push("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"extra_content\":{\"google\":{\"thought_signature\":\"sig-st\"}}}]}}]}\n")
	if got := cache.lookup("st1"); got != "sig-st" {
		t.Fatalf("stream signature: %q", got)
	}
	// 非 data: 行与 [DONE] 被忽略
	ext.push(": keepalive\ndata: [DONE]\n")
}

func TestIsMissingThoughtSignatureError(t *testing.T) {
	if !isMissingThoughtSignatureError(400, "missing thought_signature for tool call") {
		t.Fatal("must detect missing signature 400")
	}
	if isMissingThoughtSignatureError(500, "thought_signature") {
		t.Fatal("only 400 counts")
	}
	if isMissingThoughtSignatureError(400, "other problem") {
		t.Fatal("unrelated 400 must not match")
	}
}

func TestProviderNeedsThoughtSignatures(t *testing.T) {
	setTestProvider(t, "gemini", providerConfig{BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai", APIKey: "k"})
	if !providerNeedsThoughtSignatures(providerByName("gemini")) {
		t.Fatal("gemini provider must need signatures")
	}
	setTestProvider(t, "custom", providerConfig{BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai", APIKey: "k"})
	if !providerNeedsThoughtSignatures(providerByName("custom")) {
		t.Fatal("generativelanguage base url must need signatures")
	}
	setTestProvider(t, "openrouter", providerConfig{BaseURL: "https://openrouter.ai/api/v1", APIKey: "k"})
	if providerNeedsThoughtSignatures(providerByName("openrouter")) {
		t.Fatal("openrouter must not need signatures")
	}
	if providerNeedsThoughtSignatures(nil) {
		t.Fatal("nil provider must be false")
	}
}
