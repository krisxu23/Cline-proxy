package app

// 面板的 opencode 模型列表只列免费(2026-09-24 用户要求)。
//
// 背景: 上游 zen /v1/models 现在返回**整个商业目录**(实测 84 条里只有 15 条免费),
// 而面板此前刻意"带出全部", 于是被 69 个付费模型(claude-opus-5 / gpt-6 / ...)淹没。
// 那个"带出全部"的原始理由是"opencode 会不定期放进免费但未标注 -free 的测试模型,
// 只列免费就看不到、无从启用" —— 该需求由 **seed 白名单**(zenSeedModels)承接,
// 所以现在可以安全地只列免费。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// withTestZenCatalog 注入一份混合目录与"手动启用"集合。
func withTestZenCatalog(t *testing.T) {
	t.Helper()
	zenModelsMu.Lock()
	prevModels := zenModels
	zenModels = map[string]*ZenModel{
		"mimo-v2.6-flash-free": {ID: "mimo-v2.6-flash-free", Source: "synced"}, // 免费: -free 后缀
		"big-pickle":           {ID: "big-pickle", Source: "seed"},             // 免费: seed 白名单(无 -free 后缀)
		"claude-opus-5":        {ID: "claude-opus-5", Source: "synced"},        // 纯付费
		"gpt-6":                {ID: "gpt-6", Source: "synced"},                // 纯付费
		"legacy-manual":        {ID: "legacy-manual", Source: "synced"},        // 付费, 但用户曾手动启用
	}
	zenModelsMu.Unlock()

	zenEnabledMu.Lock()
	prevSet := zenEnabledModelSet
	zenEnabledModelSet = map[string]bool{"legacy-manual": true}
	zenEnabledMu.Unlock()

	t.Cleanup(func() {
		zenModelsMu.Lock()
		zenModels = prevModels
		zenModelsMu.Unlock()
		zenEnabledMu.Lock()
		zenEnabledModelSet = prevSet
		zenEnabledMu.Unlock()
	})
}

func zenEntryModels(t *testing.T) map[string]map[string]any {
	t.Helper()
	e := builtinProviderEntries()
	p, ok := e["opencode"]
	if !ok {
		t.Fatal("opencode entry missing")
	}
	ms, _ := p["models"].([]map[string]any)
	got := map[string]map[string]any{}
	for _, m := range ms {
		id, _ := m["model"].(string)
		got[id] = m
	}
	return got
}

// 纯付费模型不得出现在面板; 免费模型必须保留; 手动启用过的付费模型必须保留。
func TestOpencodeEntryListsOnlyFreeModels(t *testing.T) {
	withTestZenCatalog(t)
	got := zenEntryModels(t)

	for _, id := range []string{"claude-opus-5", "gpt-6"} {
		if _, ok := got[id]; ok {
			t.Fatalf("纯付费模型不应出现在面板: %s", id)
		}
	}
	// 两种免费形态都要在: -free 后缀, 以及 seed 白名单里没有后缀的(big-pickle)。
	for _, id := range []string{"mimo-v2.6-flash-free", "big-pickle"} {
		m, ok := got[id]
		if !ok {
			t.Fatalf("免费模型必须保留: %s", id)
		}
		if m["free"] != true || m["on"] != true {
			t.Fatalf("%s 应为 free=true/on=true, got %+v", id, m)
		}
	}
	// 曾手动启用过的付费模型必须保留 —— 否则它会从面板消失, 用户再也无法取消它
	// (与 2026-09-17 审查 C1 同一类问题)。
	m, ok := got["legacy-manual"]
	if !ok {
		t.Fatal("手动启用过的模型必须保留, 否则用户无法取消勾选")
	}
	if m["free"] != false || m["manually"] != true || m["on"] != true {
		t.Fatalf("legacy-manual 口径应为 free=false/manually=true/on=true, got %+v", m)
	}
}

// catalogModels(自动路由的候选表)与 models 同口径, 不能偷偷把付费模型塞回去。
func TestOpencodeCatalogAlsoHidesPaid(t *testing.T) {
	withTestZenCatalog(t)
	e := builtinProviderEntries()
	cat, _ := e["opencode"]["catalogModels"].([]map[string]any)
	ids := map[string]bool{}
	for _, c := range cat {
		if s, ok := c["id"].(string); ok {
			ids[s] = true
		}
	}
	if ids["claude-opus-5"] || ids["gpt-6"] {
		t.Fatalf("catalog 也不应含纯付费模型, got %v", ids)
	}
	if !ids["mimo-v2.6-flash-free"] || !ids["big-pickle"] {
		t.Fatalf("catalog 应保留免费模型, got %v", ids)
	}
}

// /admin/api/zen/models 同口径, 并把被过滤的条数回报给 hiddenPaid 供自查 ——
// 否则面板上"怎么少了这么多"看起来像目录同步坏了。
func TestHandleZenModelsHidesPaidAndReportsCount(t *testing.T) {
	withTestZenCatalog(t)

	rec := httptest.NewRecorder()
	handleZenModels(rec, httptest.NewRequest(http.MethodGet, "/admin/api/zen/models", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var resp struct {
		Success bool `json:"success"`
		Data    struct {
			Models     []map[string]any `json:"models"`
			Count      int              `json:"count"`
			HiddenPaid int              `json:"hiddenPaid"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if !resp.Success {
		t.Fatal("success 应为 true")
	}
	if resp.Data.Count != 3 {
		t.Fatalf("应只返回 3 个(2 免费 + 1 手动启用), got %d", resp.Data.Count)
	}
	if resp.Data.HiddenPaid != 2 {
		t.Fatalf("应报告隐藏了 2 个纯付费模型, got %d", resp.Data.HiddenPaid)
	}
	seen := map[string]bool{}
	for _, m := range resp.Data.Models {
		if id, ok := m["id"].(string); ok {
			seen[id] = true
		}
	}
	if seen["claude-opus-5"] || seen["gpt-6"] {
		t.Fatalf("接口不应返回纯付费模型, got %v", seen)
	}
	if !seen["big-pickle"] {
		t.Fatalf("接口应返回 seed 白名单里的免费模型, got %v", seen)
	}
}
