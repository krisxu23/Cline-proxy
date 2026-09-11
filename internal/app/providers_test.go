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
