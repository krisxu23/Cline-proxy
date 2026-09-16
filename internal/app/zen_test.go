package app

import (
	"encoding/json"
	"os"
	"testing"

	"cline-go-proxy/internal/kit"
)

// TestSaveZenConfigAtomic 验证 saveZenConfig 走原子写, 落盘后是完整可解析的 JSON。
// saveZenConfig 自身在持有 zenConfigMu 的情况下被调用, 这里从外部直接调用它,
// 它内部会再次加 zenConfigMu —— Mutex 不可重入, 因此必须确认调用链没有重复加锁。
// 实际调用链: saveZenConfig() 自己 Lock/defer Unlock, 不调用其它会加 zenConfigMu 的函数,
// 故从外部调用不会自锁。
func TestSaveZenConfigAtomic(t *testing.T) {
	orig := zenConfig
	defer func() { zenConfig = orig }()
	zenConfig = &zenConfigData{
		Key:             "public",
		BaseURLs:        []string{"https://example.com/zen/v1"},
		SubsRefreshMins: 30,
	}

	saveZenConfig()

	path := kit.ResolveDataPath(".zen-config.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("zen config not written: %v", err)
	}
	var got zenConfigData
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("zen config not valid JSON: %v\nraw=%s", err, data)
	}
	if got.SubsRefreshMins != 30 || got.Key != "public" {
		t.Fatalf("zen config content mismatch: %+v", got)
	}
}

// TestIsZenFreeModelManualEnable 手动启用列表让"没标 -free 的测试模型"通过
// 免费判定 —— 这是 opencode 不定期放进免费测试模型(如 union-alpha)的放行通道。
func TestIsZenFreeModelManualEnable(t *testing.T) {
	// 备份并清空启用集合
	zenEnabledMu.Lock()
	origSet := zenEnabledModelSet
	zenEnabledModelSet = map[string]bool{}
	zenEnabledMu.Unlock()
	defer func() {
		zenEnabledMu.Lock()
		zenEnabledModelSet = origSet
		zenEnabledMu.Unlock()
	}()

	// 未标注的 synced 模型: 默认不放行
	untagged := &ZenModel{ID: "union-alpha", Source: "synced"}
	if isZenFreeModel(untagged) {
		t.Fatal("未标注 -free 的 synced 模型不应自动放行")
	}

	// 放进启用集合后放行
	zenEnabledMu.Lock()
	zenEnabledModelSet["union-alpha"] = true
	zenEnabledMu.Unlock()
	if !isZenFreeModel(untagged) {
		t.Fatal("手动启用后, isZenFreeModel 应放行")
	}

	// -free 后缀照旧放行(不受启用集合影响)
	if !isZenFreeModel(&ZenModel{ID: "x-free", Source: "synced"}) {
		t.Fatal("-free 后缀应自动放行")
	}
	// seed 白名单照旧放行
	if !isZenFreeModel(&ZenModel{ID: "big-pickle", Source: "seed"}) {
		t.Fatal("seed 模型应放行")
	}
	// 空值保护
	if isZenFreeModel(nil) {
		t.Fatal("nil 模型不应放行")
	}
}

// TestRefreshZenEnabledModels 配置里的 EnabledModels 应被正确重建为集合,
// 且 trim/去空生效。
func TestRefreshZenEnabledModels(t *testing.T) {
	orig := zenConfig
	defer func() { zenConfig = orig }()
	zenConfig = &zenConfigData{
		EnabledModels: []string{"  union-alpha  ", "", "muse-spark-1.3", " "},
	}
	refreshZenEnabledModels()
	if !zenModelEnabled("union-alpha") {
		t.Fatal("trim 后的 ID 应在集合中")
	}
	if !zenModelEnabled("muse-spark-1.3") {
		t.Fatal("muse-spark-1.3 应在集合中")
	}
	if zenModelEnabled("not-enabled") {
		t.Fatal("未配置的 ID 不应在集合中")
	}
	if zenModelEnabled("") {
		t.Fatal("空 ID 不应匹配")
	}
	// 清空配置后集合应清空
	zenConfig = &zenConfigData{}
	refreshZenEnabledModels()
	if zenModelEnabled("union-alpha") {
		t.Fatal("清空配置后集合应清空")
	}
}

// TestZenAllCatalogContainsUntagged zenAllCatalog 必须带出未标注的 synced 模型,
// 否则面板上根本看不到它, 用户无从启用。
func TestZenAllCatalogContainsUntagged(t *testing.T) {
	prevModels, prevAliases := zenModels, zenAliases
	defer func() { zenModels, zenAliases = prevModels, prevAliases }()
	zenModels = map[string]*ZenModel{
		"deepseek-v4-flash-free": {ID: "deepseek-v4-flash-free", Source: "seed"},
		"union-alpha":            {ID: "union-alpha", Source: "synced"},
	}
	zenAliases = map[string]*ZenModel{}

	all := zenAllCatalog()
	seen := map[string]bool{}
	for _, m := range all {
		seen[m.ID] = true
	}
	if !seen["union-alpha"] {
		t.Fatal("zenAllCatalog 必须包含未标注的 union-alpha")
	}
	if !seen["deepseek-v4-flash-free"] {
		t.Fatal("zenAllCatalog 必须包含 seed 模型")
	}
	// zenFreeCatalog 只应有免费的那个
	free := zenFreeCatalog()
	freeSeen := map[string]bool{}
	for _, m := range free {
		freeSeen[m.ID] = true
	}
	if freeSeen["union-alpha"] {
		t.Fatal("未启用时 zenFreeCatalog 不应包含 union-alpha")
	}
	if !freeSeen["deepseek-v4-flash-free"] {
		t.Fatal("zenFreeCatalog 应包含 seed 模型")
	}
}
