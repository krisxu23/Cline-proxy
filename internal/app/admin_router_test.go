package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// routerGet 调一次 GET /admin/api/router 并取出 data。
func routerGet(t *testing.T) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	handleAdminRouter(rec, httptest.NewRequest("GET", "/admin/api/router", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /router status = %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode /router: %v", err)
	}
	return resp.Data
}

// routerPost 调一次 POST 接口。
func routerPost(t *testing.T, h http.HandlerFunc, path, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h(rec, req)
	var resp struct {
		Data map[string]any `json:"data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	return rec.Code, resp.Data
}

// 页面要探测出网关当前加入的全部供应商, 并各自列出可用模型。
func TestRouterSnapshotDetectsEveryProvider(t *testing.T) {
	withTestConfig(t, &zenConfigData{
		Providers: map[string]providerConfig{
			"bai": {BaseURL: "https://api.b.ai/v1", APIKey: "sk-1",
				FreeModels: []string{"glm-5.3-flash", "deepseek-v4-flash"}},
			"nokey": {BaseURL: "https://x.example/v1",
				FreeModels: []string{"m"}},
			"gemini": {BaseURL: "https://generativelanguage.googleapis.com", APIKey: "AQ.x",
				FreeModels: []string{"gemini-3.8-flash"}},
		},
	})
	d := routerGet(t)

	if d["alias"] != defaultAutoRouterAlias {
		t.Fatalf("default alias must be %s, got %v", defaultAutoRouterAlias, d["alias"])
	}
	provs, _ := d["providers"].([]any)
	if len(provs) != 3 {
		t.Fatalf("all three providers must be detected, got %d", len(provs))
	}

	byName := map[string]map[string]any{}
	for _, p := range provs {
		pm, _ := p.(map[string]any)
		byName[pm["name"].(string)] = pm
	}
	if byName["bai"] == nil || byName["nokey"] == nil || byName["gemini"] == nil {
		t.Fatalf("providers missing: %v", byName)
	}
	if models, _ := byName["bai"]["models"].([]any); len(models) != 2 {
		t.Fatalf("bai must list its 2 whitelist models, got %d", len(models))
	}
	if byName["gemini"]["google"] != true {
		t.Fatal("google provider must be flagged so the UI can label it")
	}
	if byName["nokey"]["configured"] != false {
		t.Fatal("a provider without an API key must be reported as unconfigured")
	}
	// 缺 key 的供应商要在 problems 里点名, 否则用户不知道它为什么没生效
	probs, _ := d["problems"].([]any)
	found := false
	for _, p := range probs {
		if s, _ := p.(string); strings.Contains(s, "nokey") {
			found = true
		}
	}
	if !found {
		t.Fatalf("unconfigured provider must be reported: %v", probs)
	}
}

// 保存: 别名可自定义, 勾选的模型落进 routes, 改名时旧键要被搬走。
func TestRouterSaveStoresAliasAndSelection(t *testing.T) {
	withTestConfig(t, &zenConfigData{
		Providers: map[string]providerConfig{
			"bai": {BaseURL: "https://api.b.ai/v1", APIKey: "sk-1",
				FreeModels: []string{"m1", "m2"}},
		},
		Routes: map[string][]string{defaultAutoRouterAlias: {"bai:m1"}},
	})

	code, data := routerPost(t, handleAdminRouterSave, "/admin/api/router/save",
		`{"alias":"my-router","providers":["bai"],"models":["bai:m1","bai:m2"]}`)
	if code != http.StatusOK {
		t.Fatalf("save status = %d (%v)", code, data)
	}
	if data["alias"] != "my-router" || data["models"] != float64(2) {
		t.Fatalf("save response = %v", data)
	}

	cfg := getZenConfig()
	if cfg.Router.Alias != "my-router" {
		t.Fatalf("alias not stored: %q", cfg.Router.Alias)
	}
	if got := cfg.Routes["my-router"]; len(got) != 2 || got[0] != "bai:m1" || got[1] != "bai:m2" {
		t.Fatalf("selection not stored in order: %v", got)
	}
	if _, stale := cfg.Routes[defaultAutoRouterAlias]; stale {
		t.Fatal("renaming must migrate the chain off the old alias, not leave a stale copy")
	}
	if len(cfg.Router.Providers) != 1 || cfg.Router.Providers[0] != "bai" {
		t.Fatalf("provider selection not stored: %v", cfg.Router.Providers)
	}

	// 改完名字后, 新的模型名必须能解析出候选链
	cands, matched, errMsg := resolveRouteChain("my-router")
	if !matched || errMsg != "" {
		t.Fatalf("new alias must resolve: matched=%v err=%q", matched, errMsg)
	}
	if len(cands) != 2 || cands[0].Upstream != "bai" {
		t.Fatalf("unexpected chain after rename: %+v", cands)
	}
	// 官方老名字仍然可用, 避免已有客户端配置失效
	if _, matched, _ := resolveRouteChain(legacyFreeBestAlias); !matched {
		t.Fatal("the legacy alias must keep working")
	}
}

// 一个模型都不勾 = 回落到默认链, 而不是写一条空链把别名弄失效。
func TestRouterSaveEmptySelectionFallsBackToDefaultChain(t *testing.T) {
	withTestConfig(t, &zenConfigData{
		Providers: map[string]providerConfig{
			"bai": {BaseURL: "https://api.b.ai/v1", APIKey: "sk-1",
				FreeModels: []string{"m1", "m2"}},
		},
		Routes: map[string][]string{defaultAutoRouterAlias: {"bai:m1"}},
	})

	if code, _ := routerPost(t, handleAdminRouterSave, "/admin/api/router/save",
		`{"alias":"auto-router","providers":[],"models":[]}`); code != http.StatusOK {
		t.Fatalf("save status = %d", code)
	}
	cfg := getZenConfig()
	if _, exists := cfg.Routes[defaultAutoRouterAlias]; exists {
		t.Fatal("an empty selection must not leave an empty chain behind")
	}
	cands, matched, errMsg := resolveRouteChain(defaultAutoRouterAlias)
	if !matched || errMsg != "" {
		t.Fatalf("alias must still resolve via the default chain: %v %q", matched, errMsg)
	}
	if len(cands) != 2 {
		t.Fatalf("default chain must contain both whitelist models: %+v", cands)
	}
}

// 别名校验: 空白与冒号必须拒绝 —— 冒号是 "provider:model" 的分隔符。
func TestNormalizeRouterAlias(t *testing.T) {
	for _, ok := range []string{"auto-router", "free.best", "a/b_c-d", "router1"} {
		if _, err := normalizeRouterAlias(ok); err != nil {
			t.Errorf("%q must be accepted: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "   ", "has space", "has:colon", "-leading", "$dollar"} {
		if _, err := normalizeRouterAlias(bad); err == nil {
			t.Errorf("%q must be rejected", bad)
		}
	}
	code, data := routerPost(t, handleAdminRouterSave, "/admin/api/router/save",
		`{"alias":"bad:name","providers":[],"models":[]}`)
	if code != http.StatusBadRequest {
		t.Fatalf("invalid alias must be rejected, got %d (%v)", code, data)
	}
}

// 校验勾选: 指向已消失的模型、未勾选供应商的模型、缺 key 的供应商都要报出来。
func TestRouterValidateReportsSelectionProblems(t *testing.T) {
	withTestConfig(t, &zenConfigData{
		Providers: map[string]providerConfig{
			"bai": {BaseURL: "https://api.b.ai/v1", APIKey: "sk-1",
				FreeModels: []string{"m1"}},
			"nokey": {BaseURL: "https://x.example/v1",
				FreeModels: []string{"m"}},
		},
	})

	// 干净的一份勾选不该报问题
	_, data := routerPost(t, handleAdminRouterValidate, "/admin/api/router/validate",
		`{"alias":"auto-router","providers":["bai"],"models":["bai:m1"]}`)
	if probs, _ := data["problems"].([]any); len(probs) != 0 {
		t.Fatalf("clean selection must validate: %v", probs)
	}

	_, data = routerPost(t, handleAdminRouterValidate, "/admin/api/router/validate",
		`{"alias":"auto-router","providers":["bai","nokey"],"models":["bai:m1","bai:ghost","nokey:m"]}`)
	probs, _ := data["problems"].([]any)
	joined := ""
	for _, p := range probs {
		s, _ := p.(string)
		joined += s + "\n"
	}
	if !strings.Contains(joined, "ghost") {
		t.Errorf("a model missing from the catalog must be reported:\n%s", joined)
	}
	if !strings.Contains(joined, "nokey") || !strings.Contains(joined, "API Key") {
		t.Errorf("a provider without an API key must be reported:\n%s", joined)
	}

	// 勾了未选供应商的模型
	_, data = routerPost(t, handleAdminRouterValidate, "/admin/api/router/validate",
		`{"alias":"auto-router","providers":["bai"],"models":["nokey:m"]}`)
	probs, _ = data["problems"].([]any)
	if len(probs) == 0 {
		t.Fatal("a model from an unchecked provider must be reported")
	}
}

// 快照里必须带上诊断数据, 页面一次请求就能渲染完整。
func TestRouterSnapshotCarriesDiagnostics(t *testing.T) {
	withTestConfig(t, &zenConfigData{
		Providers: map[string]providerConfig{
			"bai": {BaseURL: "https://api.b.ai/v1", APIKey: "sk-1", FreeModels: []string{"m1"}},
		},
	})
	resetCandidateState()
	resetUsageLedger()
	defer func() { resetCandidateState(); resetUsageLedger() }()

	markCandidateCooldown("bai", "m1", classRateLimit, "429")
	recordUsageForCandidate(routeCandidate{Upstream: "bai", Model: "m1"}, false)

	d := routerGet(t)
	if cool, _ := d["cooling"].([]any); len(cool) != 1 {
		t.Fatalf("cooling entries must be present: %v", d["cooling"])
	}
	usage, _ := d["usage"].(map[string]any)
	if rows, _ := usage["rows"].([]any); len(rows) != 1 {
		t.Fatalf("usage rows must be present: %v", usage)
	}
	if _, ok := d["discovery"].(map[string]any); !ok {
		t.Fatal("discovery status must be present")
	}
	if _, ok := d["cooldownMs"].(map[string]any); !ok {
		t.Fatal("cooldown table must be present")
	}
}
