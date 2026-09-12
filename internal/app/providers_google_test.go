package app

import (
	"net/http"
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

// Google 的目录名前缀剥离与双鉴权头是纯函数: 直接断言, 不再经测试服务走完整刷新。
func TestGoogleCatalogHeadersAndPrefixStripping(t *testing.T) {
	cfg := providerConfig{BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai", APIKey: "AK"}
	if got := cfg.stripProviderModelPrefix("models/gemini-3.8-flash"); got != "gemini-3.8-flash" {
		t.Fatalf("models/ prefix must be stripped: %q", got)
	}
	if got := cfg.stripProviderModelPrefix("gemini-3.8-flash"); got != "gemini-3.8-flash" {
		t.Fatalf("bare id must pass through: %q", got)
	}
	other := providerConfig{BaseURL: "https://x.example/v1"}
	if got := other.stripProviderModelPrefix("models/m"); got != "models/m" {
		t.Fatalf("non-google prefix must be kept: %q", got)
	}
	// 兼容端点同时发送两个头
	open := catalogHeaders(cfg, cfg.chatEndpoint())
	if open["Authorization"] != "Bearer AK" || open[googleKeyHeader] != "AK" {
		t.Fatalf("openai-compat path must send both auth headers: %+v", open)
	}
	// 原生目录只认 x-goog-api-key: 多发 Bearer 会被回 401 并盖住真实错误
	native := catalogHeaders(cfg, googleNativeCatalogURL(cfg))
	if native["Authorization"] != "" {
		t.Fatalf("native catalog must not send Bearer: %+v", native)
	}
	if native[googleKeyHeader] != "AK" {
		t.Fatalf("native catalog must send %s: %+v", googleKeyHeader, native)
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
