package app

import (
	"strings"
	"testing"
)

// withTestConfig 在测试期间替换全局配置, 结束后还原。
func withTestConfig(t *testing.T, cfg *zenConfigData) {
	t.Helper()
	prev := getZenConfig()
	setConfigForTest(cfg)
	t.Cleanup(func() { setConfigForTest(prev) })
}

// 别名条目按书写顺序展开, 无法解析的条目丢弃而不是让整条链失败。
func TestExpandRouteListOrderAndParsing(t *testing.T) {
	got := expandRouteList("r", []string{
		"gemini:gemini-3.8-flash",
		"  zen:mimo-v2.5-free  ",
		"cline:*",
		"",
		":missing-upstream",
		"missing-model:",
	})
	want := []routeCandidate{
		{Upstream: "gemini", Model: "gemini-3.8-flash"},
		{Upstream: upstreamZen, Model: "mimo-v2.5-free"},
		{Upstream: upstreamCline, Model: clinePoolPlaceholder},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d candidates, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("hop %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// 普通模型名不是候选链, 必须回退到原有路由(行为不变)。
func TestResolveRouteChainLeavesPlainModelsAlone(t *testing.T) {
	withTestConfig(t, &zenConfigData{})
	for _, m := range []string{"gpt-5", "some-random-model", "opencode/x"} {
		if _, matched, _ := resolveRouteChain(m); matched {
			t.Errorf("%q must not be treated as a route chain", m)
		}
	}
}

// 配置里显式声明的别名按配置顺序展开。
func TestResolveRouteChainConfiguredAlias(t *testing.T) {
	withTestConfig(t, &zenConfigData{
		Routes: map[string][]string{
			"my-chain": {"gemini:m1", "cline:*"},
		},
	})
	cands, matched, errMsg := resolveRouteChain("my-chain")
	if !matched || errMsg != "" {
		t.Fatalf("matched=%v errMsg=%q", matched, errMsg)
	}
	if len(cands) != 2 || cands[0] != (routeCandidate{"gemini", "m1"}) || cands[1].Upstream != upstreamCline {
		t.Fatalf("unexpected chain: %+v", cands)
	}
}

// 一个 provider 都没配时, 自动路由别名必须给出明确错误而不是静默回退。
func TestResolveRouteChainAutoRouterWithoutProviders(t *testing.T) {
	withTestConfig(t, &zenConfigData{})
	cands, matched, errMsg := resolveRouteChain(defaultAutoRouterAlias)
	if !matched {
		t.Fatal("the auto-router alias must be recognised as a route")
	}
	if len(cands) != 0 {
		t.Fatalf("expected no candidates, got %+v", cands)
	}
	if !strings.Contains(errMsg, "no provider") {
		t.Fatalf("error must explain why: %q", errMsg)
	}
}

// 未显式勾选模型时, 默认链按 provider + 模型名排序展开, 顺序必须可复现。
func TestResolveRouteChainAutoRouterFromWhitelist(t *testing.T) {
	withTestConfig(t, &zenConfigData{
		Providers: map[string]providerConfig{
			"bai": {BaseURL: "https://api.b.ai/v1", APIKey: "sk-x",
				FreeModels: []string{"glm-5.3-flash", "deepseek-v4-flash"}},
			// 缺 key 的 provider 不参与候选
			"nogkey": {BaseURL: "https://x.example/v1",
				FreeModels: []string{"should-not-appear"}},
		},
	})
	cands, matched, errMsg := resolveRouteChain(defaultAutoRouterAlias)
	if !matched || errMsg != "" {
		t.Fatalf("matched=%v errMsg=%q", matched, errMsg)
	}
	if len(cands) != 2 {
		t.Fatalf("expected 2 candidates, got %+v", cands)
	}
	if cands[0] != (routeCandidate{"bai", "deepseek-v4-flash"}) ||
		cands[1] != (routeCandidate{"bai", "glm-5.3-flash"}) {
		t.Fatalf("chain must be deterministic and sorted: %+v", cands)
	}
}

// 跳过判定综合了候选层冷却与"上游是否可用"。
func TestCandidateSkip(t *testing.T) {
	withTestConfig(t, &zenConfigData{
		Providers: map[string]providerConfig{
			"bai": {BaseURL: "https://api.b.ai/v1", APIKey: "sk-x", FreeModels: []string{"glm-5.3-flash"}},
		},
	})
	resetCandidateState()
	defer resetCandidateState()

	ok := routeCandidate{Upstream: "bai", Model: "glm-5.3-flash"}
	if why := candidateSkip(ok); why != "" {
		t.Fatalf("configured candidate must be usable, got %q", why)
	}
	// 未配置的 provider
	if why := candidateSkip(routeCandidate{Upstream: "ghost", Model: "m"}); why == "" {
		t.Fatal("unknown provider must be skipped")
	}
	// 冷却中的候选
	markCandidateCooldown(ok.Upstream, ok.Model, classRateLimit, "429")
	if why := candidateSkip(ok); !strings.Contains(why, classRateLimit) {
		t.Fatalf("cooled candidate must be skipped with its class, got %q", why)
	}
	// 永久剔除
	resetCandidateState()
	markCandidatePermanent(ok.Upstream, ok.Model, "无免费层")
	if why := candidateSkip(ok); !strings.Contains(why, "永久剔除") {
		t.Fatalf("permanently rejected candidate must be skipped, got %q", why)
	}
}

// 面板展示: 每站带出可用性说明, 便于看出为什么没被选中。
func TestDescribeRouteChain(t *testing.T) {
	withTestConfig(t, &zenConfigData{
		Routes: map[string][]string{"auto-router": {"bai:m1", "ghost:m2"}},
		Providers: map[string]providerConfig{
			"bai": {BaseURL: "https://api.b.ai/v1", APIKey: "sk-x"},
		},
	})
	resetCandidateState()
	defer resetCandidateState()

	d := describeRouteChain(defaultAutoRouterAlias)
	if d["error"] != "" {
		t.Fatalf("unexpected error: %v", d["error"])
	}
	hops, _ := d["hops"].([]map[string]any)
	if len(hops) != 2 {
		t.Fatalf("expected 2 hops, got %d", len(hops))
	}
	if hops[0]["skip"] != "" {
		t.Errorf("configured hop must be usable, got %v", hops[0]["skip"])
	}
	if hops[1]["skip"] == "" {
		t.Error("unknown provider hop must be marked as skipped")
	}
}
