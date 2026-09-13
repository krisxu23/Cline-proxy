package app

import (
	"testing"
	"time"
)

// 模型列表页的数据源契约: cline 行必须是官方推荐清单全量(带状态/费用/同步时间),
// 不能再退回单个 "*" 占位 —— 否则面板把 cline 画成一行, 用户看不到任何模型。
func TestBuiltinEntriesClineFullModelList(t *testing.T) {
	initModelsCache()
	const seeded = "test-deleted-model"
	modelsMu.Lock()
	modelsCache[seeded] = &ModelInfo{
		ID: seeded, Source: "free", Provider: "test", Cost: "free", Status: ModelRemoved,
	}
	modelsMu.Unlock()
	t.Cleanup(func() {
		modelsMu.Lock()
		delete(modelsCache, seeded)
		modelsMu.Unlock()
	})

	e := builtinProviderEntries()
	cline, ok := e["cline"]
	if !ok {
		t.Fatal("cline entry missing")
	}
	models, ok := cline["models"].([]map[string]any)
	if !ok || len(models) < 4 {
		t.Fatalf("cline must carry the whole recommended list, got %d entries", len(models))
	}
	byID := map[string]map[string]any{}
	for _, m := range models {
		id, _ := m["model"].(string)
		if id == "" {
			t.Fatalf("model id empty: %+v", m)
		}
		if m["id"] != "cline:"+id {
			t.Fatalf("full id must be cline/<model>, got %+v", m)
		}
		for _, k := range []string{"status", "cost"} {
			if _, ok := m[k]; !ok {
				t.Fatalf("%s missing %q: %+v", id, k, m)
			}
		}
		byID[id] = m
	}
	if got, _ := byID[seeded]["status"].(string); got != string(ModelRemoved) {
		t.Fatalf("removed status must survive into the entry: %+v", byID[seeded])
	}

	cat, _ := cline["catalogModels"].([]map[string]any)
	disabled := map[string]bool{}
	for _, m := range cat {
		id, _ := m["id"].(string)
		d, _ := m["disabled"].(bool)
		disabled[id] = d
	}
	if len(cat) < 4 {
		t.Fatalf("cline catalog must list every recommended model, got %d", len(cat))
	}
	if !disabled[seeded] {
		t.Fatal("removed model must be disabled in the cline catalog")
	}
	if _, ok := disabled["deepseek/deepseek-v4-flash"]; !ok {
		t.Fatal("seed model must appear in the cline catalog")
	}

	ls, _ := cline["lastSync"].(string)
	if ls != "" {
		if _, err := time.Parse(time.RFC3339, ls); err != nil {
			t.Fatalf("lastSync must be RFC3339: %q (%v)", ls, err)
		}
	}
	if !isBuiltinProvider("cline") {
		t.Fatal("cline must stay pinned as builtin (no edit/delete)")
	}
}

// modelsSyncStamp: 空值为 "", 非零为 UTC RFC3339。
// 写入方(syncRecommendedModels)持 modelsMu 写 modelsLastSync, 这里同样持锁读,
// 消除 /admin/api/models 与同步协程之间的数据竞争。
func TestModelsSyncStamp(t *testing.T) {
	modelsMu.Lock()
	old := modelsLastSync
	modelsLastSync = time.Time{}
	modelsMu.Unlock()
	t.Cleanup(func() {
		modelsMu.Lock()
		modelsLastSync = old
		modelsMu.Unlock()
	})

	if got := modelsSyncStamp(); got != "" {
		t.Fatalf("zero sync time must render empty, got %q", got)
	}
	modelsMu.Lock()
	modelsLastSync = time.Date(2026, 9, 13, 8, 5, 9, 0, time.UTC)
	modelsMu.Unlock()
	want := "2026-09-13T08:05:09Z"
	if got := modelsSyncStamp(); got != want {
		t.Fatalf("stamp: got %q want %q", got, want)
	}
}

// 空态: 没有任何供应商时 GET 仍要返回两条内置行, 面板不至于画成空页。
func TestBuiltinEntriesPresentWithoutAnyProvider(t *testing.T) {
	e := builtinProviderEntries()
	if len(e) != 2 {
		t.Fatalf("exactly two builtin entries, got %d", len(e))
	}
	for _, name := range []string{"cline", "opencode"} {
		p, ok := e[name]
		if !ok {
			t.Fatalf("%s entry missing", name)
		}
		if p["builtin"] != true {
			t.Fatalf("%s must carry builtin:true", name)
		}
		if _, ok := p["display"].(string); !ok {
			t.Fatalf("%s missing display name", name)
		}
	}
}
