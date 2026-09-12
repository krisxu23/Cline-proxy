package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

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

func TestNormalizeCatalogPayloadOpenAI(t *testing.T) {
	payload := []byte(`{"data":[
		{"id":"m1","created":1,"context_length":8000,"pricing":{"prompt":"0","completion":"0"},"architecture":{"output_modalities":["text"]}},
		{"id":"m2","pricing":{"prompt":"0.000001","completion":"0.000002"}}
	]}`)
	page := normalizeCatalogPayload(payload)
	if page.shape != "openai" || len(page.models) != 2 {
		t.Fatalf("openai shape: %+v", page)
	}
	if !page.models[0].PricesKnown || page.models[0].PromptPrice != 0 || page.models[0].ContextLength != 8000 {
		t.Fatalf("openai model parse: %+v", page.models[0])
	}
	if page.models[1].PromptPrice != 0.000001 {
		t.Fatalf("price parse: %+v", page.models[1])
	}
}

func TestNormalizeCatalogPayloadGoogle(t *testing.T) {
	payload := []byte(`{"models":[{
		"name":"models/gemini-3.8-flash","displayName":"Gemini 3.8",
		"inputTokenLimit":1000,"outputTokenLimit":2000,
		"supportedGenerationMethods":["generateContent","embedContent"]
	}],"nextPageToken":"p2"}`)
	page := normalizeCatalogPayload(payload)
	if page.shape != "google" || len(page.models) != 1 || page.nextPageToken != "p2" {
		t.Fatalf("google shape: %+v", page)
	}
	m := page.models[0]
	if m.ID != "gemini-3.8-flash" || m.Name != "Gemini 3.8" || m.ContextLength != 1000 || m.MaxOutput != 2000 {
		t.Fatalf("google model parse: %+v", m)
	}
	if m.ChatCapable == nil || !*m.ChatCapable {
		t.Fatalf("generateContent must mark chat capability: %+v", m.ChatCapable)
	}
	// 仅 embedContent 的模型不可聊天
	payload2 := []byte(`{"models":[{"name":"models/embed","supportedGenerationMethods":["embedContent"]}]}`)
	p2 := normalizeCatalogPayload(payload2)
	if len(p2.models) != 1 || p2.models[0].ChatCapable == nil || *p2.models[0].ChatCapable {
		t.Fatalf("embedding model must not be chat capable: %+v", p2.models)
	}
}

func TestNormalizeCatalogPayloadUnknown(t *testing.T) {
	if got := normalizeCatalogPayload([]byte(`{"foo":1}`)); got.shape != "unknown" {
		t.Fatalf("unknown shape: %+v", got)
	}
	if got := normalizeCatalogPayload([]byte(`not json`)); got.shape != "unknown" {
		t.Fatalf("invalid json shape: %+v", got)
	}
}

func TestRefreshCatalogPricingMode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("catalog path: %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer k" {
			t.Errorf("catalog auth: %q", got)
		}
		json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{
			{"id": "free-a", "pricing": map[string]any{"prompt": "0", "completion": "0"}},
			{"id": "paid", "pricing": map[string]any{"prompt": "1", "completion": "1"}},
		}})
	}))
	defer srv.Close()

	setTestProvider(t, "t", providerConfig{BaseURL: srv.URL, APIKey: "k", Catalog: true, Pricing: true})
	p := providerByName("t")
	if err := p.refreshCatalog(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if !p.isFree("free-a") {
		t.Fatal("zero-cost catalog model must be free")
	}
	if p.isFree("paid") {
		t.Fatal("paid catalog model must not be free")
	}
	ids := p.freeModelIDs()
	if len(ids) != 1 || ids[0].ID != "free-a" {
		t.Fatalf("freeModelIDs: %+v", ids)
	}
}

func TestRefreshCatalogGoogleKeyHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("x-goog-api-key"); got != "gk" {
			t.Errorf("models key header: %q", got)
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("Bearer must not be used when modelsKeyHeader is set: %q", got)
		}
		json.NewEncoder(w).Encode(map[string]any{"models": []map[string]any{
			{"name": "models/g1", "supportedGenerationMethods": []string{"generateContent"}},
		}})
	}))
	defer srv.Close()

	setTestProvider(t, "g", providerConfig{
		BaseURL: srv.URL, APIKey: "gk", Catalog: true,
		ModelsURL: srv.URL + "/models", ModelsKeyHeader: "x-goog-api-key",
	})
	p := providerByName("g")
	if err := p.refreshCatalog(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if p.catalogEntry("g1") == nil {
		t.Fatalf("google catalog entry missing: %+v", p.catalog)
	}
}

func TestRefreshCatalogErrorRecorded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"bad key"}`))
	}))
	defer srv.Close()
	setTestProvider(t, "bad", providerConfig{BaseURL: srv.URL, APIKey: "k", Catalog: true, Pricing: true})
	p := providerByName("bad")
	if err := p.refreshCatalog(context.Background(), true); err == nil {
		t.Fatal("catalog failure must return an error")
	}
	st := p.catalogStatus()
	if st["error"] == "" {
		t.Fatalf("catalog error must be recorded: %+v", st)
	}
}

func TestFreeModelIDsMatchesIsFree(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{
			{"id": "free-a", "pricing": map[string]any{"prompt": "0", "completion": "0"}},
		}})
	}))
	defer srv.Close()

	setTestProvider(t, "tr", providerConfig{BaseURL: srv.URL, APIKey: "k", Catalog: true, Pricing: true})
	p := providerByName("tr")
	if err := p.refreshCatalog(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if !p.isFree("free-a") {
		t.Fatal("zero-cost catalog model must be free before rejection")
	}
	p.mu.Lock()
	p.rejected = map[string]string{"free-a": "withdrawn"}
	p.mu.Unlock()

	if p.isFree("free-a") {
		t.Fatal("permanently rejected model must not be free")
	}
	if ids := p.freeModelIDs(); len(ids) != 0 {
		t.Fatalf("freeModelIDs must not publish a permanently rejected model: %+v", ids)
	}
}

func TestProviderModelList(t *testing.T) {
	setTestProvider(t, "bai", providerConfig{BaseURL: "https://x", APIKey: "k", FreeModels: []string{"glm-5.3-flash", "mimo-v2.5"}})
	setTestProvider(t, "nokey", providerConfig{BaseURL: "https://y", FreeModels: []string{"whatever"}})
	list := providerModelList()
	seen := map[string]bool{}
	for _, m := range list {
		seen[m["id"].(string)] = true
	}
	if !seen["bai:glm-5.3-flash"] || !seen["bai:mimo-v2.5"] {
		t.Fatalf("provider models must be prefixed: %+v", seen)
	}
	if seen["nokey:whatever"] {
		t.Fatal("provider without key must be excluded")
	}
}

// ai-gateway 式显式开关: 白名单/目录模型可逐个勾选加入或剔除。
// DisabledModels 命中即不可用(优于一切免费判定), 空=全部启用, 兼容旧配置。
func TestDisabledModelsExclude(t *testing.T) {
	setTestProvider(t, "sel", providerConfig{
		BaseURL: "https://x", APIKey: "k",
		FreeModels:     []string{"keep-me", "drop-me"},
		DisabledModels: []string{"drop-me"},
	})
	p := providerByName("sel")
	if !p.isFree("keep-me") {
		t.Fatal("未勾掉的模型必须可用")
	}
	if p.isFree("drop-me") {
		t.Fatal("勾掉的模型必须不可用")
	}
	ids := p.freeModelIDs()
	if len(ids) != 1 || ids[0].ID != "keep-me" {
		t.Fatalf("freeModelIDs: %+v", ids)
	}
}

// 线上故障: 价格模式 + 无价格目录(B.AI 类上游目录不带价格) + 白名单,
// 白名单里的模型必须可用 —— 价格判据在无价格目录上恒为 false, 不能把白名单也埋了。
func TestRefreshCatalogPricingFallsBackToWhitelist(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{
			{"id": "glm-5.3-flash"},
			{"id": "paid-pro"},
		}})
	}))
	defer srv.Close()

	setTestProvider(t, "priceless", providerConfig{
		BaseURL: srv.URL, APIKey: "k", Catalog: true, Pricing: true,
		FreeModels: []string{"glm-5.3-flash"},
	})
	p := providerByName("priceless")
	if err := p.refreshCatalog(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if !p.isFree("glm-5.3-flash") {
		t.Fatal("无价格目录时白名单模型必须可用")
	}
	if p.isFree("paid-pro") {
		t.Fatal("不在白名单的模型不得可用")
	}
	ids := p.freeModelIDs()
	if len(ids) != 1 || ids[0].ID != "glm-5.3-flash" {
		t.Fatalf("freeModelIDs: %+v", ids)
	}
}

func TestExplicitModelsPrecedence(t *testing.T) {
	setTestProvider(t, "ex", providerConfig{BaseURL: "https://x", APIKey: "k",
		FreeModels: []string{"a", "b"},
		Models:     []providerModelEntry{{ID: "a", Enabled: true}, {ID: "b", Enabled: false}}})
	p := providerByName("ex")
	if !p.isFree("a") {
		t.Fatal("explicit enabled must be free")
	}
	if p.isFree("b") {
		t.Fatal("explicit disabled must not be free")
	}
}

func TestMigrationBackfillOnRefresh(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{
			{"id": "m1"}, {"id": "m2"},
		}})
	}))
	defer srv.Close()
	setTestProvider(t, "mig", providerConfig{BaseURL: srv.URL, APIKey: "k", Catalog: true, FreeModels: []string{"m1"}})
	p := providerByName("mig")
	if err := p.refreshCatalog(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	cfg, _ := providerConfigFor("mig")
	if !cfg.Migrated || len(cfg.Models) != 1 || cfg.Models[0].ID != "m1" {
		t.Fatalf("must backfill whitelist into explicit models: %+v", cfg.Models)
	}
}
