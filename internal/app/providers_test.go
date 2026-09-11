package app

import "testing"

// setTestProvider 在测试内挂一个 provider 配置, 结束时清理配置与运行时状态。
func setTestProvider(t *testing.T, name string, pc providerConfig) {
	t.Helper()
	zenConfigMu.Lock()
	if zenConfig == nil {
		zenConfig = &zenConfigData{}
	}
	if zenConfig.Providers == nil {
		zenConfig.Providers = map[string]providerConfig{}
	}
	zenConfig.Providers[name] = pc
	zenConfigMu.Unlock()
	t.Cleanup(func() {
		zenConfigMu.Lock()
		if zenConfig != nil {
			delete(zenConfig.Providers, name)
		}
		zenConfigMu.Unlock()
		providerRTMu.Lock()
		delete(providerRT, name)
		providerRTMu.Unlock()
	})
}

func TestProviderConfigDefaults(t *testing.T) {
	pc := providerConfig{BaseURL: "https://example.com/v1"}
	if pc.chatPath() != "/chat/completions" {
		t.Fatalf("chatPath default: %q", pc.chatPath())
	}
	if pc.modelsPath() != "/models" {
		t.Fatalf("modelsPath default: %q", pc.modelsPath())
	}
	custom := providerConfig{ChatPath: "/v1/chat", ModelsPath: "/v1/models"}
	if custom.chatPath() != "/v1/chat" || custom.modelsPath() != "/v1/models" {
		t.Fatal("explicit paths must win")
	}
}

func TestProviderFreeSet(t *testing.T) {
	pc := providerConfig{FreeModels: []string{" a ", "", "b"}}
	s := pc.freeSet()
	if len(s) != 2 || !s["a"] || !s["b"] {
		t.Fatalf("freeSet: %+v", s)
	}
}

func TestValidateProviderConfig(t *testing.T) {
	if err := validateProviderConfig("openrouter", providerConfig{BaseURL: "https://x"}); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	if err := validateProviderConfig("Bad-Name", providerConfig{BaseURL: "https://x"}); err == nil {
		t.Fatal("uppercase id must be rejected")
	}
	if err := validateProviderConfig("ok", providerConfig{}); err == nil {
		t.Fatal("missing baseUrl must be rejected")
	}
}

func TestProviderRegistryLookup(t *testing.T) {
	setTestProvider(t, "openrouter", providerConfig{BaseURL: "https://x", APIKey: "k"})
	if providerByName("openrouter") == nil {
		t.Fatal("configured provider must resolve")
	}
	if providerByName("unknown") != nil {
		t.Fatal("unknown provider must be nil")
	}
	found := false
	for _, n := range providerNames() {
		if n == "openrouter" {
			found = true
		}
	}
	if !found {
		t.Fatal("providerNames must include configured provider")
	}
}

func TestResolveHeaderOriginExpansion(t *testing.T) {
	pc := providerConfig{}
	got := pc.resolveHeader(providerHeaderSpec{Default: "${origin}"}, "http://127.0.0.1:3457")
	if got != "http://127.0.0.1:3457" {
		t.Fatalf("origin expansion: %q", got)
	}
	got = pc.resolveHeader(providerHeaderSpec{Default: "Free Router"}, "http://x")
	if got != "Free Router" {
		t.Fatalf("static default: %q", got)
	}
}

func boolPtr(v bool) *bool { return &v }

func TestNormalizeModelSlug(t *testing.T) {
	cases := map[string]string{
		"Gemini-3.8-Flash":          "gemini-3.8-flash",
		"z-ai/glm-5.3:free":         "glm-5.3",
		" models/gemini-3.7-flash ": "gemini-3.7-flash",
		"":                          "",
	}
	for in, want := range cases {
		if got := normalizeModelSlug(in); got != want {
			t.Errorf("normalizeModelSlug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsZeroCost(t *testing.T) {
	zero := &catalogModel{PricesKnown: true, PromptPrice: 0, CompletionPrice: 0}
	if !isZeroCost(zero) {
		t.Fatal("zero prices must count as free")
	}
	paid := &catalogModel{PricesKnown: true, PromptPrice: 0.001, CompletionPrice: 0.002}
	if isZeroCost(paid) {
		t.Fatal("paid model must not be free")
	}
	unknown := &catalogModel{PricesKnown: false}
	if isZeroCost(unknown) {
		t.Fatal("unknown pricing must not count as free")
	}
	if isZeroCost(nil) {
		t.Fatal("nil must not be free")
	}
}

func TestIsChatModel(t *testing.T) {
	if !isChatModel(&catalogModel{ChatCapable: boolPtr(true)}) {
		t.Fatal("explicit chatCapable=true must pass")
	}
	if isChatModel(&catalogModel{ChatCapable: boolPtr(false)}) {
		t.Fatal("explicit chatCapable=false must fail")
	}
	// 明确 chatCapable 优先于 tokenizer 启发式
	if !isChatModel(&catalogModel{ChatCapable: boolPtr(true), Tokenizer: "Router"}) {
		t.Fatal("explicit chatCapable must win over tokenizer heuristic")
	}
	if isChatModel(&catalogModel{Tokenizer: "Router"}) {
		t.Fatal("Router tokenizer must be excluded")
	}
	if isChatModel(&catalogModel{OutputModalities: []string{"image"}}) {
		t.Fatal("non-text output must be excluded")
	}
	if !isChatModel(&catalogModel{OutputModalities: []string{"text"}}) {
		t.Fatal("text output must pass")
	}
	if isChatModel(&catalogModel{ID: "openai/gpt-content-safety"}) {
		t.Fatal("moderation model must be excluded")
	}
}

func TestEvalProviderFree(t *testing.T) {
	cfg := providerConfig{APIKey: "k", Catalog: true, Pricing: true}
	cat := map[string]*catalogModel{
		"z-ai/glm-5.3:free": {ID: "z-ai/glm-5.3:free", PricesKnown: true, PromptPrice: 0, CompletionPrice: 0},
		"paid":              {ID: "paid", PricesKnown: true, PromptPrice: 1, CompletionPrice: 1},
	}
	if !evalProviderFree(cfg, cat, nil, nil, "z-ai/glm-5.3:free") {
		t.Fatal("zero-cost catalog model must be free")
	}
	if evalProviderFree(cfg, cat, nil, nil, "paid") {
		t.Fatal("paid catalog model must not be free")
	}
	if evalProviderFree(cfg, cat, nil, nil, "missing") {
		t.Fatal("model absent from catalog must not be free")
	}
	if evalProviderFree(providerConfig{APIKey: ""}, nil, nil, nil, "x") {
		t.Fatal("provider without key must not be free")
	}
	// 白名单模式
	wl := providerConfig{APIKey: "k", FreeModels: []string{"mimo-v2.5"}}
	if !evalProviderFree(wl, nil, nil, nil, "mimo-v2.5") {
		t.Fatal("whitelisted model must be free")
	}
	if evalProviderFree(wl, nil, nil, nil, "other") {
		t.Fatal("non-whitelisted model must not be free")
	}
	// 白名单 + 目录校验: 已下架模型不算免费
	wlc := providerConfig{APIKey: "k", Catalog: true, FreeModels: []string{"gone", "here"}}
	cat2 := map[string]*catalogModel{"here": {ID: "here"}}
	if evalProviderFree(wlc, cat2, nil, nil, "gone") {
		t.Fatal("whitelisted but withdrawn model must not be free")
	}
	if !evalProviderFree(wlc, cat2, nil, nil, "here") {
		t.Fatal("whitelisted and present model must be free")
	}
	// 永久拒绝
	if evalProviderFree(cfg, cat, nil, map[string]string{"z-ai/glm-5.3:free": "withdrawn"}, "z-ai/glm-5.3:free") {
		t.Fatal("permanently rejected model must not be free")
	}
}
