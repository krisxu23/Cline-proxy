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
