package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// zen 目录缓存: 同步过的模型在重启后必须仍然可用 —— 否则第一次同步失败
// 就会把用户正在用的模型从目录里挤掉, 误报 "paid zen model"。
func TestZenModelsCacheRoundTrip(t *testing.T) {
	withZenModelsReset(t)

	// 模拟一次同步写缓存
	zenModelsMu.Lock()
	zenModels["muse-spark-1.3-contributor-free"] = &ZenModel{
		ID: "muse-spark-1.3-contributor-free", Context: 200000, Output: 32768, Source: "synced",
	}
	saveZenModelsCacheLocked()
	zenModelsMu.Unlock()

	// 重置内存目录(模拟重启), init 应从缓存恢复
	zenModelsMu.Lock()
	zenModels = map[string]*ZenModel{}
	zenModelsMu.Unlock()
	initZenModels()

	zenModelsMu.RLock()
	_, ok := zenModels["muse-spark-1.3-contributor-free"]
	seedN := 0
	for _, m := range zenModels {
		if m.Source == "seed" {
			seedN++
		}
	}
	zenModelsMu.RUnlock()
	if !ok {
		t.Fatal("重启后同步模型必须从缓存恢复")
	}
	if seedN != len(zenSeedModels) {
		t.Fatalf("种子模型必须完整保留, 得到 %d/%d", seedN, len(zenSeedModels))
	}

	// 与免费模型别名冲突的 ID 不得通过缓存混入(保护别名解析)
	zenModelsMu.Lock()
	delete(zenModels, "muse-spark-1.3-contributor-free")
	zenModels["deepseek-v4-flash"] = &ZenModel{ID: "deepseek-v4-flash", Source: "synced"} // 冲突别名
	saveZenModelsCacheLocked()
	zenModels = map[string]*ZenModel{}
	zenModelsMu.Unlock()
	initZenModels()
	zenModelsMu.RLock()
	synced, hasSynced := zenModels["deepseek-v4-flash"]
	zenModelsMu.RUnlock()
	m, ok := resolveZenModel("deepseek-v4-flash")
	if hasSynced {
		t.Fatalf("别名冲突的缓存条目不得进入目录: %+v", synced)
	}
	if !ok || m.Source != "seed" {
		t.Fatalf("别名解析必须仍指向种子模型: %+v (ok=%v)", m, ok)
	}
}

// 过期缓存(>7 天)不再恢复, 避免上游已下架的模型永久驻留。
func TestZenModelsCacheExpires(t *testing.T) {
	withZenModelsReset(t)

	stale := zenModelsCacheFile{
		SyncedAt: time.Now().Add(-8 * 24 * time.Hour).Unix(),
		Models:   []ZenModel{{ID: "muse-spark-1.3-contributor-free", Context: 200000, Output: 32768, Source: "synced"}},
	}
	raw, _ := json.Marshal(stale)
	path := zenModelsCachePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	zenModelsMu.Lock()
	zenModels = map[string]*ZenModel{}
	zenModelsMu.Unlock()
	initZenModels()

	zenModelsMu.RLock()
	_, ok := zenModels["muse-spark-1.3-contributor-free"]
	zenModelsMu.RUnlock()
	if ok {
		t.Fatal("过期缓存不得恢复")
	}
}

// withZenModelsReset 保存并恢复 zenModels/zenAliases 与缓存文件, 测试互不污染。
func withZenModelsReset(t *testing.T) {
	t.Helper()
	zenModelsMu.Lock()
	prevModels, prevAliases := zenModels, zenAliases
	zenModels = map[string]*ZenModel{}
	zenAliases = map[string]*ZenModel{}
	zenModelsMu.Unlock()
	t.Cleanup(func() {
		zenModelsMu.Lock()
		zenModels, zenAliases = prevModels, prevAliases
		zenModelsMu.Unlock()
		os.Remove(zenModelsCachePath())
	})
}

// 拒绝文案必须如实说明原因, 不再把"目录里暂时没有"说成"付费模型"。
func TestZenRejectMessageExplainsCatalog(t *testing.T) {
	msg := zenRejectMessage("zen/muse-spark-1.3-contributor-free")
	if !strings.Contains(msg, "not in the current zen catalog") {
		t.Fatalf("文案必须说明是目录里没有: %s", msg)
	}
	if strings.Contains(msg, "is a paid zen model") {
		t.Fatalf("不得再误报付费模型: %s", msg)
	}
}
