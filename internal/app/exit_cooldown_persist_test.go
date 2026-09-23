package app

// 出口冷却表落盘(2026-09-24): 重启不丢"已知耗尽"的出口。
//
// 此前冷却表只在内存里, 重启即清空 —— 那个已耗尽的出口会被重新选中一次、再吃一个
// 429、再重新学一遍。落盘后重启即可跳过它(实测额度冷却 8h 级别, 丢了必然白打一次)。

import (
	"encoding/json"
	"os"
	"sync"
	"testing"
	"time"
)

// withTestExitCooldowns 把冷却表落盘路径指向临时文件并清空内存表/重置加载闸。
func withTestExitCooldowns(t *testing.T) string {
	t.Helper()
	path := t.TempDir() + "/exit-cooldowns.json"
	prevPath := exitCooldownFileOverride
	exitCooldownFileOverride = path
	exitCooldownLoadOnce = sync.Once{} // 让每个用例都能重新走一次加载

	zenProxyCooldownsMu.Lock()
	prevC, prevS := zenProxyCooldowns, zenProxyQuotaStrikes
	zenProxyCooldowns = map[string]time.Time{}
	zenProxyQuotaStrikes = map[string]int{}
	zenProxyCooldownsMu.Unlock()

	t.Cleanup(func() {
		exitCooldownFileOverride = prevPath
		zenProxyCooldownsMu.Lock()
		zenProxyCooldowns, zenProxyQuotaStrikes = prevC, prevS
		zenProxyCooldownsMu.Unlock()
	})
	return path
}

func writeExitCooldownFile(t *testing.T, path string, snap exitCooldownCache) {
	t.Helper()
	b, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// 额度冷却落盘 → 模拟重启 → 该出口必须仍然不可用, 且 429 计数一并恢复。
func TestExitCooldownPersistRoundTrip(t *testing.T) {
	withTestExitCooldowns(t)
	const key = "sbox://node-a"

	if d := cooldownZenProxyQuota(key, 8*time.Hour); d == 0 {
		t.Fatal("应产生冷却")
	}
	if _, err := os.Stat(exitCooldownPath()); err != nil {
		t.Fatalf("额度冷却应已落盘: %v", err)
	}

	// 模拟重启: 清空内存表 + 重置加载闸。
	zenProxyCooldownsMu.Lock()
	zenProxyCooldowns = map[string]time.Time{}
	zenProxyQuotaStrikes = map[string]int{}
	zenProxyCooldownsMu.Unlock()
	exitCooldownLoadOnce = sync.Once{}

	// 走选路读取点, 顺带验证 zenProxyAvailable 会触发惰性加载。
	if zenProxyAvailable(key) {
		t.Fatal("重启恢复后该出口应仍处于冷却中(不可用)")
	}
	zenProxyCooldownsMu.Lock()
	n := zenProxyQuotaStrikes[key]
	zenProxyCooldownsMu.Unlock()
	if n != 1 {
		t.Fatalf("429 计数应一并恢复, got %d", n)
	}
}

// 已过期的冷却不该恢复: 存的是绝对时刻, 时间本身就是判据。
func TestExitCooldownCacheDropsExpired(t *testing.T) {
	path := withTestExitCooldowns(t)
	writeExitCooldownFile(t, path, exitCooldownCache{
		Cooldowns: map[string]time.Time{
			"sbox://gone":  time.Now().Add(-time.Minute),
			"sbox://alive": time.Now().Add(time.Hour),
		},
	})

	loadExitCooldownCache()

	if !zenProxyAvailable("sbox://gone") {
		t.Fatal("过期冷却应视为可用")
	}
	if zenProxyAvailable("sbox://alive") {
		t.Fatal("未过期冷却应仍不可用")
	}
}

// 429 计数只恢复"仍在冷却中"的出口: 冷却已过期的出口视为拿到新机会,
// 否则几周前的陈旧计数会让冷却虚高。
func TestExitCooldownCacheStrikeNeedsLiveCooldown(t *testing.T) {
	path := withTestExitCooldowns(t)
	writeExitCooldownFile(t, path, exitCooldownCache{
		Cooldowns: map[string]time.Time{"sbox://live": time.Now().Add(time.Hour)},
		Strikes:   map[string]int{"sbox://live": 3, "sbox://no-cooldown": 5},
	})

	loadExitCooldownCache()

	zenProxyCooldownsMu.Lock()
	live, orphan := zenProxyQuotaStrikes["sbox://live"], zenProxyQuotaStrikes["sbox://no-cooldown"]
	zenProxyCooldownsMu.Unlock()
	if live != 3 {
		t.Fatalf("仍冷却中的出口应恢复计数, got %d", live)
	}
	if orphan != 0 {
		t.Fatalf("无冷却的陈旧计数不应恢复, got %d", orphan)
	}
}

// 加载发生在写入之后时, 不能用文件里的短冷却覆盖本进程刚写的新值(只补空缺语义)。
func TestExitCooldownCacheNeverShortensFreshValue(t *testing.T) {
	path := withTestExitCooldowns(t)
	writeExitCooldownFile(t, path, exitCooldownCache{
		Cooldowns: map[string]time.Time{"sbox://x": time.Now().Add(time.Hour)},
	})
	zenProxyCooldownsMu.Lock()
	zenProxyCooldowns["sbox://x"] = time.Now().Add(8 * time.Hour)
	zenProxyCooldownsMu.Unlock()

	loadExitCooldownCache()

	zenProxyCooldownsMu.Lock()
	left := time.Until(zenProxyCooldowns["sbox://x"])
	zenProxyCooldownsMu.Unlock()
	if left < 7*time.Hour {
		t.Fatalf("本进程的新值被文件里的短冷却覆盖了, 剩余 %v", left)
	}
}

// ★ 顺序陷阱的回归: 通用短冷却(网络错误, 2 分钟)在**加载之前**写入时, 不能把
// 文件里那条 8h 额度冷却覆盖掉。修法是在 cooldownZenProxy 里先 ensure 加载 ——
// 少了那一步, 加载会走"只补空缺"语义跳过该键, 8h 就变成 2 分钟。
func TestGenericCooldownDoesNotShortenPersistedQuotaCooldown(t *testing.T) {
	path := withTestExitCooldowns(t)
	writeExitCooldownFile(t, path, exitCooldownCache{
		Cooldowns: map[string]time.Time{"sbox://x": time.Now().Add(8 * time.Hour)},
	})

	// 故意不先加载, 直接走网络错误路径的短冷却。
	cooldownZenProxy("sbox://x", 2*time.Minute)

	zenProxyCooldownsMu.Lock()
	left := time.Until(zenProxyCooldowns["sbox://x"])
	zenProxyCooldownsMu.Unlock()
	if left < 7*time.Hour {
		t.Fatalf("写前未加载, 落盘的 8h 额度冷却被 2 分钟通用冷却覆盖了: 剩余 %v", left)
	}
}
