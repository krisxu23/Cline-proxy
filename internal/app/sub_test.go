package app

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// TestSaveSubCacheAtomic 验证订阅节点缓存走原子写, 落盘后是完整可解析的 JSON。
func TestSaveSubCacheAtomic(t *testing.T) {
	seed := []any{"vmess://n1", "trojan://n2", "ss://n3"}
	subMu.Lock()
	subNodes = seed
	rebuildSubKeysLocked()
	subMu.Unlock()

	saveSubCacheLocked()

	data, err := os.ReadFile(subCacheFile())
	if err != nil {
		t.Fatalf("subs cache not written: %v", err)
	}
	var got struct {
		Nodes []any `json:"nodes"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("subs cache not valid JSON: %v\nraw=%s", err, data)
	}
	if len(got.Nodes) != len(seed) {
		t.Fatalf("subs cache content mismatch: %d nodes, want %d", len(got.Nodes), len(seed))
	}
}

// TestSaveSubCacheMarshalErrorNoWrite 验证 marshal 失败时绝不落盘,
// 不会把已有缓存覆盖成一个空文件(静默清空订阅节点)。
func TestSaveSubCacheMarshalErrorNoWrite(t *testing.T) {
	path := subCacheFile()
	// 预置一个合法缓存文件
	if err := os.WriteFile(path, []byte(`{"nodes":["vmess://seed"]}`), 0o600); err != nil {
		t.Fatalf("prewrite failed: %v", err)
	}

	// subNodes 含不可 marshal 的值(chan/func), 触发 json.Marshal 失败
	subMu.Lock()
	subNodes = []any{func() {}}
	subMu.Unlock()

	saveSubCacheLocked()

	// marshal 失败时缓存文件必须保持原样
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("cache file missing after marshal error: %v", err)
	}
	if !strings.Contains(string(b), "vmess://seed") {
		t.Fatalf("marshal error path overwrote cache file: %s", b)
	}

	subMu.Lock()
	subNodes = nil
	subMu.Unlock()
}

// TestSubsRefreshIntervalClamp 验证越界值被夹回允许区间(下限 subRefreshMinMins, 上限 subRefreshMaxMins),
// 与注释、与写入侧(越界直接 400)行为一致。
func TestSubsRefreshIntervalClamp(t *testing.T) {
	orig := zenConfig
	defer func() { zenConfig = orig }()

	cases := []struct {
		in   int
		want time.Duration
	}{
		{0, time.Duration(subRefreshMinMins) * time.Minute},
		{-10, time.Duration(subRefreshMinMins) * time.Minute},
		{subRefreshMinMins, time.Duration(subRefreshMinMins) * time.Minute},
		{30, 30 * time.Minute},
		{subRefreshMaxMins, time.Duration(subRefreshMaxMins) * time.Minute},
		{subRefreshMaxMins + 1000, time.Duration(subRefreshMaxMins) * time.Minute},
	}
	for _, c := range cases {
		zenConfig = &zenConfigData{SubsRefreshMins: c.in}
		got := subsRefreshInterval()
		if got != c.want {
			t.Fatalf("subsRefreshInterval(%d) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestLoadSubCacheLenInsideLock 验证 loadSubCache 在锁内读取节点数量(修复并发读 subNodes 的 data race),
// 且能从缓存文件正确恢复节点。
func TestLoadSubCacheLenInsideLock(t *testing.T) {
	orig := zenConfig
	defer func() { zenConfig = orig }()
	zenConfig = defaultZenConfig()

	path := subCacheFile()
	seed := []any{"vmess://n1", "vmess://n2", "trojan://n3"}
	b, _ := json.Marshal(map[string]any{"nodes": seed})
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatalf("write cache: %v", err)
	}
	subMu.Lock()
	subNodes = nil
	subNodeKeys = nil
	subMu.Unlock()

	loadSubCache()

	got := subNodeKeysSnapshot()
	if len(got) != len(seed) {
		t.Fatalf("loadSubCache restored %d nodes, want %d (keys=%v)", len(got), len(seed), got)
	}
}
