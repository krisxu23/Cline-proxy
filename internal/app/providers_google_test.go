package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Google 的端点归一化: 用户只需要填 https://generativelanguage.googleapis.com,
// 后面缺什么补什么; 已经带 /openai 的原样保留。
func TestGoogleOpenAIBaseNormalisation(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://generativelanguage.googleapis.com", "https://generativelanguage.googleapis.com/v1beta/openai"},
		{"https://generativelanguage.googleapis.com/", "https://generativelanguage.googleapis.com/v1beta/openai"},
		{"https://generativelanguage.googleapis.com/v1beta", "https://generativelanguage.googleapis.com/v1beta/openai"},
		{"https://generativelanguage.googleapis.com/v1beta/openai", "https://generativelanguage.googleapis.com/v1beta/openai"},
		{"https://generativelanguage.googleapis.com/v1beta/openai/", "https://generativelanguage.googleapis.com/v1beta/openai"},
		// 旧配置里出现过漏掉 s 的目录地址, 归一化后不再带着它跑
		{"https://generativelanguage.googleapis.com/v1beta/model", "https://generativelanguage.googleapis.com/v1beta/openai"},
	}
	for _, c := range cases {
		if got := googleOpenAIBase(c.in); got != c.want {
			t.Errorf("googleOpenAIBase(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestGoogleChatEndpointAndAuth(t *testing.T) {
	cfg := providerConfig{BaseURL: "https://generativelanguage.googleapis.com", APIKey: "AQ.test"}
	if !isGoogleProvider(cfg) {
		t.Fatal("generativelanguage host must be detected")
	}
	want := "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions"
	if got := cfg.chatEndpoint(); got != want {
		t.Fatalf("chatEndpoint = %q, want %q", got, want)
	}
	if got := catalogURL(cfg); got != "https://generativelanguage.googleapis.com/v1beta/openai/models" {
		t.Fatalf("catalogURL = %q", got)
	}
	// OpenAI 兼容路径认 Bearer, 原生路径只认 x-goog-api-key: 按路径择一发送。
	openai := http.Header{}
	cfg.applyAuth(cfg.chatEndpoint(), openai.Set)
	if openai.Get("Authorization") != "Bearer AQ.test" {
		t.Fatalf("openai-compat path must send Bearer: %q", openai.Get("Authorization"))
	}
	if openai.Get(googleKeyHeader) != "AQ.test" {
		t.Fatalf("google must send %s: %q", googleKeyHeader, openai.Get(googleKeyHeader))
	}
	// 原生路径上多发一个 Bearer 会被 Google 判成 OAuth2 请求并回 401,
	// 把真实的地区/配额错误盖掉, 因此这里只能发 x-goog-api-key。
	native := http.Header{}
	cfg.applyAuth(googleNativeCatalogURL(cfg), native.Set)
	if native.Get("Authorization") != "" {
		t.Fatalf("native google path must not send Bearer: %q", native.Get("Authorization"))
	}
	if native.Get(googleKeyHeader) != "AQ.test" {
		t.Fatalf("native google path must send %s", googleKeyHeader)
	}
	// 非 Google 上游不应被塞进 Google 专属头, 且照旧走 Bearer
	other := providerConfig{BaseURL: "https://openrouter.ai/api/v1", APIKey: "sk-1"}
	h2 := http.Header{}
	other.applyAuth("https://openrouter.ai/api/v1/chat/completions", h2.Set)
	if h2.Get(googleKeyHeader) != "" {
		t.Fatal("non-google provider must not send the google key header")
	}
	if h2.Get("Authorization") != "Bearer sk-1" {
		t.Fatalf("non-google provider must send Bearer: %q", h2.Get("Authorization"))
	}
}

// Google 的目录形状写死在代码里: id 带 models/ 前缀时必须剥掉,
// 否则面板和请求里会出现 provider:models/gemini-... 这种名字;
// 同时 Bearer 与 x-goog-api-key 两个鉴权头都要发出。
func TestGoogleCatalogStripsModelsPrefixAndSendsBothHeaders(t *testing.T) {
	var sawBearer, sawKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawBearer = r.Header.Get("Authorization")
		sawKey = r.Header.Get(googleKeyHeader)
		json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []map[string]any{
			{"id": "models/gemini-3.8-flash", "object": "model"},
			{"id": "models/text-embedding-004", "object": "model"},
		}})
	}))
	defer srv.Close()

	// Base URL 用真实 Google 域名(判定走 Google 分支), 目录地址指向本地测试服务。
	setTestProvider(t, "g", providerConfig{
		BaseURL:   "https://generativelanguage.googleapis.com/v1beta/openai",
		APIKey:    "AK",
		Catalog:   true,
		AllModels: true,
		ModelsURL: srv.URL + "/v1beta/openai/models",
	})
	p := providerByName("g")
	if err := p.refreshCatalog(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if p.catalogEntry("gemini-3.8-flash") == nil {
		t.Fatalf("models/ prefix must be stripped: keys=%v", catalogKeys(p))
	}
	for k := range p.catalog {
		if len(k) > 7 && k[:7] == "models/" {
			t.Fatalf("prefixed id must not be kept: %q", k)
		}
	}
	if sawBearer == "" || sawKey == "" {
		t.Fatalf("both auth headers expected, got bearer=%q key=%q", sawBearer, sawKey)
	}
}

func catalogKeys(p *modelProvider) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, 0, len(p.catalog))
	for k := range p.catalog {
		out = append(out, k)
	}
	return out
}

func TestGoogleCustomModelsURLIgnoredWhenPathIsWrong(t *testing.T) {
	// 用户在面板上填错的 /v1beta/model 不应被当成目录地址
	cfg := providerConfig{
		BaseURL:   "https://generativelanguage.googleapis.com/v1beta/openai",
		ModelsURL: "https://generativelanguage.googleapis.com/v1beta/model",
	}
	if cfg.customModelsURL() != "" {
		t.Fatal("bogus google models url must be ignored")
	}
	if got := catalogURL(cfg); got != "https://generativelanguage.googleapis.com/v1beta/openai/models" {
		t.Fatalf("catalogURL = %q", got)
	}
	// 非 Google 上游的自定义目录地址照旧生效
	other := providerConfig{BaseURL: "https://x.example/v1", ModelsURL: "https://x.example/whatever"}
	if other.customModelsURL() == "" {
		t.Fatal("explicit models url must be honoured for non-google providers")
	}
}

// AllModels = 通用 Provider 的默认形态: 目录里的聊天模型全部可用,
// 目录还没拉到(或上游不提供 /models)时退回白名单。
func TestCatalogAllModelsMode(t *testing.T) {
	yes, no := true, false
	cfg := providerConfig{APIKey: "k", Catalog: true, AllModels: true, FreeModels: []string{"manual-only"}}
	slugs := map[string]*catalogModel{}
	cat := map[string]*catalogModel{
		"glm-5.3-flash": {ID: "glm-5.3-flash", ChatCapable: &yes},
		"whisper-1":     {ID: "whisper-1", ChatCapable: &no},
	}
	for k, v := range cat {
		slugs[k] = v
	}
	if !evalProviderFree(cfg, cat, slugs, nil, "glm-5.3-flash") {
		t.Fatal("catalog chat model must be usable in allModels mode")
	}
	if evalProviderFree(cfg, cat, slugs, nil, "whisper-1") {
		t.Fatal("non-chat catalog model must stay unusable even in allModels mode")
	}
	if evalProviderFree(cfg, cat, slugs, nil, "not-in-catalog") {
		t.Fatal("model absent from the loaded catalog must not be published")
	}
	// 空目录: 退回白名单
	if !evalProviderFree(cfg, nil, nil, nil, "manual-only") {
		t.Fatal("empty catalog must fall back to the whitelist")
	}
	if evalProviderFree(cfg, nil, nil, nil, "something-else") {
		t.Fatal("empty catalog must not open up everything")
	}
	// 永久拒绝依然优先
	if evalProviderFree(cfg, cat, slugs, map[string]string{"glm-5.3-flash": "gone"}, "glm-5.3-flash") {
		t.Fatal("rejected models must stay rejected")
	}
	// 价格模式不受影响: 只有 0 价模型可用
	priced := providerConfig{APIKey: "k", Catalog: true, Pricing: true}
	paid := map[string]*catalogModel{
		"paid":     {ID: "paid", ChatCapable: &yes, PricesKnown: true, PromptPrice: 0.1, CompletionPrice: 0.2},
		"free-one": {ID: "free-one", ChatCapable: &yes, PricesKnown: true},
	}
	slugs2 := map[string]*catalogModel{}
	for k, v := range paid {
		slugs2[k] = v
	}
	if evalProviderFree(priced, paid, slugs2, nil, "paid") {
		t.Fatal("priced mode must reject paid models")
	}
	if !evalProviderFree(priced, paid, slugs2, nil, "free-one") {
		t.Fatal("priced mode must accept zero-cost models")
	}
}

// 直连模式: 全网关统一直连, 即使池里有可用节点也不再走节点出口。
func TestExitModeDirectBypassesProxyPool(t *testing.T) {
	prev := getZenConfig()
	defer setConfigForTest(prev)

	direct := *prev
	direct.Proxies = []string{"socks5://127.0.0.1:1080"}
	direct.ProxyStrategy = "fill" // 固定取池内第 0 个, 结果可预期
	direct.ExitMode = exitModeDirect
	setConfigForTest(&direct)

	if !exitModeDirectNow() {
		t.Fatal("exit mode must read back as direct")
	}
	if p, idx := pickZenProxy(); p != "" || idx != -1 {
		t.Fatalf("direct mode must not pick a proxy: %q %d", p, idx)
	}
	if p, idx := pickZenProxyForModel("gemini-3.8-flash"); p != "" || idx != -1 {
		t.Fatalf("direct mode must not pick a proxy for restricted models: %q %d", p, idx)
	}

	proxied := direct
	proxied.ExitMode = exitModeProxy
	setConfigForTest(&proxied)
	if exitModeDirectNow() {
		t.Fatal("exit mode must read back as proxy")
	}
	p, idx := pickZenProxy()
	if p != "socks5://127.0.0.1:1080" || idx != 0 {
		t.Fatalf("proxy mode must pick from the pool: %q %d", p, idx)
	}
}

// setConfigForTest 测试内换配置: 不落盘、不重建 sing-box 节点,
// 避免把测试配置写进用户的 data/.zen-config.json。
func setConfigForTest(c *zenConfigData) {
	zenConfigMu.Lock()
	zenConfig = c
	zenConfigMu.Unlock()
}

// 原生目录路径只发 x-goog-api-key。
// 实测在 /v1beta/models 上带 Bearer 会被 Google 回 401
// "API keys are not supported by this API", 从而盖住真实的地区拒绝。
func TestGoogleNativeCatalogSendsOnlyKeyHeader(t *testing.T) {
	var bearer, key string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bearer = r.Header.Get("Authorization")
		key = r.Header.Get(googleKeyHeader)
		json.NewEncoder(w).Encode(map[string]any{"models": []map[string]any{
			{
				"name":                       "models/gemini-3.8-flash",
				"displayName":                "Gemini 3.8 Flash",
				"supportedGenerationMethods": []string{"generateContent"},
			},
		}})
	}))
	defer srv.Close()

	setTestProvider(t, "gn", providerConfig{
		BaseURL:   "https://generativelanguage.googleapis.com",
		APIKey:    "AQ.test",
		Catalog:   true,
		AllModels: true,
		ModelsURL: srv.URL + "/v1beta/models",
	})
	if err := providerByName("gn").refreshCatalog(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if bearer != "" {
		t.Fatalf("native catalog must not send Bearer, got %q", bearer)
	}
	if key != "AQ.test" {
		t.Fatalf("native catalog must send %s, got %q", googleKeyHeader, key)
	}
}

// 出口地区被上游拒绝时与网络错误同源: 必须触发换出口重试,
// 而不是把 400 原样透传给调用方。
func TestExitRegionRejectionDetection(t *testing.T) {
	google := []byte(`{"error":{"code":400,"message":"User location is not supported for the API use.","status":"FAILED_PRECONDITION"}}`)
	if !isExitRegionRejected(400, google) {
		t.Fatal("google location rejection must be treated as a bad exit")
	}
	if !isExitRegionRejected(403, google) {
		t.Fatal("403 region rejection must rotate exits too")
	}
	if isExitRegionRejected(400, []byte(`{"error":{"message":"invalid request"}}`)) {
		t.Fatal("an ordinary 400 must not rotate exits")
	}
	if isExitRegionRejected(200, google) {
		t.Fatal("only 4xx rejections rotate exits")
	}
}
