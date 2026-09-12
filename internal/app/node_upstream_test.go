package app

import (
	"testing"
)

// 模型标识 → 上游归属: 用来决定"按哪个上游的可达性过滤节点"。
func TestUpstreamOfModel(t *testing.T) {
	withTestConfig(t, &zenConfigData{
		Providers: map[string]providerConfig{
			"bai":    {BaseURL: "https://api.b.ai/v1", APIKey: "sk-1"},
			"gemini": {BaseURL: "https://generativelanguage.googleapis.com", APIKey: "AQ.x"},
		},
	})
	cases := map[string]string{
		"bai:glm-5.3-flash":       "bai",
		"gemini:gemini-3.8-flash": "gemini",
		"zen:mimo-v2.5-free":      upstreamZen,
		"cline:*":                 upstreamCline,
		"unknown-provider:m":      "", // 不是已配置的 Provider, 不按上游过滤
		"no-colon":                "",
		"":                        "",
	}
	for in, want := range cases {
		if got := upstreamOfModel(in); got != want {
			t.Errorf("upstreamOfModel(%q) = %q, want %q", in, got, want)
		}
	}
}

// 未探测过必须当作可用: 否则刚启动时会把所有节点都排除掉, 直接没网。
func TestNodeSupportsUpstreamDefaultsToTrue(t *testing.T) {
	resetUpstreamMatrixForTest(t)
	if !nodeSupportsUpstream("bai", "node-a") {
		t.Fatal("未探测的上游/节点组合必须按可用处理")
	}
	if !nodeSupportsUpstream("", "node-a") {
		t.Fatal("上游未知时不做过滤")
	}

	setUpstreamMatrixForTest(map[string]map[string]bool{
		"bai": {"node-a": false, "node-b": true},
	})
	if nodeSupportsUpstream("bai", "node-a") {
		t.Fatal("已探测为不可达的节点必须被排除")
	}
	if !nodeSupportsUpstream("bai", "node-b") {
		t.Fatal("已探测为可达的节点必须保留")
	}
	if !nodeSupportsUpstream("bai", "node-c") {
		t.Fatal("该上游没探到的节点按可用处理")
	}
}

// 出口选择必须跳过"到该上游不通"的节点。
func TestPickZenProxySkipsNodesFailingUpstream(t *testing.T) {
	withTestConfig(t, &zenConfigData{
		ExitMode: exitModeProxy,
		Proxies: []string{
			"http://127.0.0.1:19101",
			"http://127.0.0.1:19102",
		},
		Providers: map[string]providerConfig{"bai": {BaseURL: "https://api.b.ai/v1", APIKey: "sk-1"}},
	})
	// 两个出口都标为"到 bai 不通"
	setUpstreamMatrixForTest(map[string]map[string]bool{
		"bai": {"http://127.0.0.1:19101": false, "http://127.0.0.1:19102": false},
	})
	if p, _ := pickZenProxyForModel("bai:glm-5.3-flash"); p != "" {
		t.Fatalf("所有出口到该上游都不通时不应挑出一个节点, got %q", p)
	}
	// 放开一个
	setUpstreamMatrixForTest(map[string]map[string]bool{
		"bai": {"http://127.0.0.1:19101": true, "http://127.0.0.1:19102": false},
	})
	if p, _ := pickZenProxyForModel("bai:glm-5.3-flash"); p != "http://127.0.0.1:19101" {
		t.Fatalf("应挑中可达的那个出口, got %q", p)
	}
	resetUpstreamMatrixForTest(t)
}

// 探测目标要覆盖到每个已配置的 Provider(不只是 zen/cline)。
func TestUpstreamTargetsIncludesProviders(t *testing.T) {
	withTestConfig(t, &zenConfigData{
		Providers: map[string]providerConfig{
			"bai":   {BaseURL: "https://api.b.ai/v1", APIKey: "sk-1"},
			"nokey": {BaseURL: "https://nokey.example/v1"},
		},
	})
	targets := upstreamTargets()
	if h := targets["bai"]; h != "api.b.ai" {
		t.Fatalf("Provider host 未纳入探测: %v", targets)
	}
	if _, ok := targets["nokey"]; ok {
		t.Fatal("没有 API Key 的 Provider 不必探测")
	}
	if _, ok := targets[upstreamZen]; !ok {
		t.Fatal("zen 必须纳入探测")
	}
	if _, ok := targets[upstreamCline]; !ok {
		t.Fatal("cline 必须纳入探测")
	}
}

// 面板概览: 统计每个上游可用节点数。
func TestNodeUpstreamOverview(t *testing.T) {
	setUpstreamMatrixForTest(map[string]map[string]bool{
		"bai": {"a": true, "b": false, "c": true},
		"zen": {"a": true},
	})
	defer resetUpstreamMatrixForTest(t)
	ov := nodeUpstreamOverview()
	bai, _ := ov["bai"].(map[string]any)
	if bai["usable"] != 2 || bai["probed"] != 3 {
		t.Fatalf("overview wrong: %v", bai)
	}
}

// 单节点快照: 该节点支持哪些上游。
func TestNodeUpstreamSnapshot(t *testing.T) {
	setUpstreamMatrixForTest(map[string]map[string]bool{
		"bai": {"a": false},
		"zen": {"a": true},
	})
	defer resetUpstreamMatrixForTest(t)
	snap := nodeUpstreamSnapshot("a")
	if snap["bai"] || !snap["zen"] {
		t.Fatalf("snapshot wrong: %v", snap)
	}
	if len(nodeUpstreamSnapshot("ghost")) != 0 {
		t.Fatal("未探测的节点应返回空快照")
	}
}

func setUpstreamMatrixForTest(m map[string]map[string]bool) {
	nodeUpstreamMu.Lock()
	nodeUpstreamOK = m
	nodeUpstreamMu.Unlock()
}

func resetUpstreamMatrixForTest(t *testing.T) {
	t.Helper()
	nodeUpstreamMu.Lock()
	prev := nodeUpstreamOK
	prevAt := nodeUpstreamLastAt
	nodeUpstreamOK = map[string]map[string]bool{}
	nodeUpstreamLastAt = prevAt
	nodeUpstreamMu.Unlock()
	t.Cleanup(func() {
		nodeUpstreamMu.Lock()
		nodeUpstreamOK = prev
		nodeUpstreamLastAt = prevAt
		nodeUpstreamMu.Unlock()
	})
}
