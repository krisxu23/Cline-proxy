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
	if got := pc.chatEndpoint(); got != "https://example.com/v1/chat/completions" {
		t.Fatalf("chatEndpoint default: %q", got)
	}
	if got := catalogURL(pc); got != "https://example.com/v1/models" {
		t.Fatalf("catalogURL default: %q", got)
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

// isFree 只认显式开关: 未迁移一律不可用, 缺 key 与永久拒绝直接否决。
func TestIsFreeExplicit(t *testing.T) {
	setTestProvider(t, "ex2", providerConfig{BaseURL: "https://x", APIKey: "k",
		Models: []providerModelEntry{{ID: "a", Enabled: true}, {ID: "b", Enabled: false}}})
	p := providerByName("ex2")
	if !p.isFree("a") {
		t.Fatal("explicit enabled must be free")
	}
	if p.isFree("b") {
		t.Fatal("explicit disabled must not be free")
	}
	if p.isFree("missing") {
		t.Fatal("unlisted model must not be free")
	}
	p.mu.Lock()
	p.rejected = map[string]string{"a": "withdrawn"}
	p.mu.Unlock()
	if p.isFree("a") {
		t.Fatal("permanently rejected model must not be free")
	}
	setTestProvider(t, "unmig", providerConfig{BaseURL: "https://x", APIKey: "k"})
	if providerByName("unmig").isFree("a") {
		t.Fatal("unmigrated provider must publish nothing")
	}
	setTestProvider(t, "nokey2", providerConfig{BaseURL: "https://x",
		Models: []providerModelEntry{{ID: "a", Enabled: true}}})
	if providerByName("nokey2").isFree("a") {
		t.Fatal("provider without key must not be free")
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

func TestRefreshCatalogBackfillsExplicitModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("catalog path: %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer k" {
			t.Errorf("catalog auth: %q", got)
		}
		json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{
			{"id": "free-a"},
			{"id": "free-b"},
		}})
	}))
	defer srv.Close()

	setTestProvider(t, "t", providerConfig{BaseURL: srv.URL, APIKey: "k", Catalog: true})
	p := providerByName("t")
	if err := p.refreshCatalog(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if p.isFree("free-a") || p.isFree("free-b") {
		t.Fatal("catalog models must be backfilled as disabled opt-in")
	}
	ids := p.freeModelIDs()
	if len(ids) != 0 {
		t.Fatalf("freeModelIDs: %+v", ids)
	}
	cfg, _ := providerConfigFor("t")
	if !cfg.Migrated {
		t.Fatal("refresh must mark the provider migrated")
	}
	byID := map[string]bool{}
	for _, e := range cfg.Models {
		byID[e.ID] = e.Enabled
	}
	if en, ok := byID["free-a"]; !ok || en {
		t.Fatalf("free-a must be disabled: %+v", cfg.Models)
	}
	if en, ok := byID["free-b"]; !ok || en {
		t.Fatalf("free-b must be disabled: %+v", cfg.Models)
	}
}

func TestRefreshCatalogErrorRecorded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"bad key"}`))
	}))
	defer srv.Close()
	setTestProvider(t, "bad", providerConfig{BaseURL: srv.URL, APIKey: "k", Catalog: true})
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

	setTestProvider(t, "tr", providerConfig{BaseURL: srv.URL, APIKey: "k", Catalog: true,
		Models: []providerModelEntry{{ID: "free-a", Enabled: true}}})
	p := providerByName("tr")
	if err := p.refreshCatalog(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if !p.isFree("free-a") {
		t.Fatal("explicitly enabled model must be free before rejection")
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
	setTestProvider(t, "bai", providerConfig{BaseURL: "https://x", APIKey: "k",
		Models: []providerModelEntry{{ID: "glm-5.3-flash", Enabled: true}, {ID: "mimo-v2.5", Enabled: true}}})
	setTestProvider(t, "nokey", providerConfig{BaseURL: "https://y",
		Models: []providerModelEntry{{ID: "whatever", Enabled: true}}})
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

// 显式开关等价旧勾选语义: 启用的可用, 勾掉的不可用。
func TestExplicitModelsExclude(t *testing.T) {
	setTestProvider(t, "sel", providerConfig{
		BaseURL: "https://x", APIKey: "k",
		Models: []providerModelEntry{{ID: "keep-me", Enabled: true}, {ID: "drop-me", Enabled: false}},
	})
	p := providerByName("sel")
	if !p.isFree("keep-me") {
		t.Fatal("启用的模型必须可用")
	}
	if p.isFree("drop-me") {
		t.Fatal("勾掉的模型必须不可用")
	}
	ids := p.freeModelIDs()
	if len(ids) != 1 || ids[0].ID != "keep-me" {
		t.Fatalf("freeModelIDs: %+v", ids)
	}
}

// 回填取并集: 白名单固化为显式启用, 目录聊天模型固化为显式禁用(opt-in);
// 价格不再参与判定(定价模式随 legacy 判定一并删除)。
func TestRefreshCatalogBackfillIsUnion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{
			{"id": "glm-5.3-flash"},
			{"id": "paid-pro"},
		}})
	}))
	defer srv.Close()

	setTestProvider(t, "priceless", providerConfig{
		BaseURL: srv.URL, APIKey: "k", Catalog: true,
		FreeModels: []string{"glm-5.3-flash"},
	})
	p := providerByName("priceless")
	if err := p.refreshCatalog(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if !p.isFree("glm-5.3-flash") {
		t.Fatal("whitelisted model must stay available")
	}
	if p.isFree("paid-pro") {
		t.Fatal("catalog-only model must be backfilled as disabled opt-in")
	}
	ids := p.freeModelIDs()
	if len(ids) != 1 || ids[0].ID != "glm-5.3-flash" {
		t.Fatalf("freeModelIDs: %+v", ids)
	}
}

func TestExplicitModelsPrecedence(t *testing.T) {
	setTestProvider(t, "ex", providerConfig{BaseURL: "https://x", APIKey: "k",
		Models: []providerModelEntry{{ID: "a", Enabled: true}, {ID: "b", Enabled: false}}})
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
	byID := map[string]bool{}
	for _, e := range cfg.Models {
		byID[e.ID] = e.Enabled
	}
	if !cfg.Migrated || !byID["m1"] || byID["m2"] || len(cfg.Models) != 2 {
		t.Fatalf("must backfill whitelist as enabled + catalog-only as disabled: %+v", cfg.Models)
	}
}

// 回填必须把 DisabledModels 折叠为显式禁用条目, 而不是直接丢掉。
func TestMigrationBackfillFoldsDisabledModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{
			{"id": "m1"}, {"id": "m2"},
		}})
	}))
	defer srv.Close()
	setTestProvider(t, "migdis", providerConfig{BaseURL: srv.URL, APIKey: "k", Catalog: true,
		FreeModels: []string{"m1", "m2"}, DisabledModels: []string{"m2"}})
	p := providerByName("migdis")
	if err := p.refreshCatalog(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	cfg, _ := providerConfigFor("migdis")
	if !cfg.Migrated {
		t.Fatal("must be marked migrated")
	}
	byID := map[string]bool{}
	for _, e := range cfg.Models {
		byID[e.ID] = e.Enabled
	}
	if en, ok := byID["m1"]; !ok || !en {
		t.Fatalf("m1 must stay enabled: %+v", cfg.Models)
	}
	if en, ok := byID["m2"]; !ok || en {
		t.Fatalf("m2 must be backfilled as explicitly disabled: %+v", cfg.Models)
	}
	if p.isFree("m2") {
		t.Fatal("m2 must not be free after migration")
	}
}
