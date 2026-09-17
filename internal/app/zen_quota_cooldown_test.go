package app

// 出口额度冷却(429 专用)的测试。
//
// 背景: opencode 免费额度按**出口 IP** 计, 用完即 429, 换 IP 可继续。
// 重置窗口未知(标 [未知], 见 docs/opencode-zen-facts.md 第 4 节), 所以冷却时长
// 用**指数升级**自适应, 而不是猜一个固定值。

import (
	"testing"
	"time"
)

// resetQuotaCooldownState 清空冷却表与计数表(测试隔离用)。
func resetQuotaCooldownState() {
	zenProxyCooldownsMu.Lock()
	zenProxyCooldowns = map[string]time.Time{}
	zenProxyQuotaStrikes = map[string]int{}
	zenProxyCooldownsMu.Unlock()
}

func TestCooldownZenProxyQuota_指数升级(t *testing.T) {
	resetQuotaCooldownState()
	defer resetQuotaCooldownState()

	const proxy = "socks5://127.0.0.1:1080"

	// 连续 429 → 时长逐次翻倍
	want := []time.Duration{
		10 * time.Minute,
		20 * time.Minute,
		40 * time.Minute,
		80 * time.Minute,
	}
	for i, w := range want {
		got := cooldownZenProxyQuota(proxy, 0)
		if got != w {
			t.Fatalf("第 %d 次 429 冷却 = %v, 期望 %v", i+1, got, w)
		}
	}
}

func TestCooldownZenProxyQuota_封顶(t *testing.T) {
	resetQuotaCooldownState()
	defer resetQuotaCooldownState()

	const proxy = "socks5://127.0.0.1:1080"
	// 连续打很多次, 时长应稳定在 6 小时上限, 不得无限增长
	for i := 0; i < 20; i++ {
		got := cooldownZenProxyQuota(proxy, 0)
		if got > zenQuotaCooldownCap {
			t.Fatalf("第 %d 次冷却 %v 超过上限 %v", i+1, got, zenQuotaCooldownCap)
		}
	}
	if got := cooldownZenProxyQuota(proxy, 0); got != zenQuotaCooldownCap {
		t.Fatalf("多次命中后应稳定在上限 %v, 实得 %v", zenQuotaCooldownCap, got)
	}
}

// 成功一次必须清零计数 —— 否则计数单调递增, 出口被限流一次后
// 再也回不到短冷却。
func TestClearZenProxyQuotaStrike_成功清零(t *testing.T) {
	resetQuotaCooldownState()
	defer resetQuotaCooldownState()

	const proxy = "socks5://127.0.0.1:1080"
	cooldownZenProxyQuota(proxy, 0) // 1st → 10min
	cooldownZenProxyQuota(proxy, 0) // 2nd → 20min
	if got := cooldownZenProxyQuota(proxy, 0); got != 40*time.Minute {
		t.Fatalf("升级后应为 40min, 实得 %v", got)
	}

	clearZenProxyQuotaStrike(proxy)

	if got := cooldownZenProxyQuota(proxy, 0); got != zenQuotaCooldownBase {
		t.Fatalf("清零后应回到基准 %v, 实得 %v", zenQuotaCooldownBase, got)
	}
}

// Retry-After 与升级时长取较大者: 上游说等更久就听它的, 但绝不因此缩短冷却。
func TestCooldownZenProxyQuota_RetryAfter取较大者(t *testing.T) {
	resetQuotaCooldownState()
	defer resetQuotaCooldownState()

	// 上游说等 2 小时 > 基准 10 分钟 → 用 2 小时
	if got := cooldownZenProxyQuota("socks5://a:1", 2*time.Hour); got != 2*time.Hour {
		t.Fatalf("Retry-After 更长时应采用它, 实得 %v", got)
	}
	// 上游说等 1 秒 < 基准 10 分钟 → 仍用 10 分钟(缩短只会换来又一次 429)
	if got := cooldownZenProxyQuota("socks5://b:2", time.Second); got != zenQuotaCooldownBase {
		t.Fatalf("Retry-After 更短时不得缩短冷却, 实得 %v", got)
	}
	// 离谱的 Retry-After 要封顶, 否则出口被锁死
	if got := cooldownZenProxyQuota("socks5://c:3", 72*time.Hour); got != zenQuotaCooldownCap {
		t.Fatalf("超长 Retry-After 应封顶到 %v, 实得 %v", zenQuotaCooldownCap, got)
	}
}

// 冷却必须真的写进冷却表, 且 zenProxyAvailable 能读到。
func TestCooldownZenProxyQuota_写入冷却表(t *testing.T) {
	resetQuotaCooldownState()
	defer resetQuotaCooldownState()

	const proxy = "socks5://127.0.0.1:1080"
	if !zenProxyAvailable(proxy) {
		t.Fatal("冷却前应可用")
	}
	cooldownZenProxyQuota(proxy, 0)
	if zenProxyAvailable(proxy) {
		t.Fatal("冷却后应不可用")
	}
}

// 空出口(直连)没有可冷却的对象, 返回 0 且不 panic。
func TestCooldownZenProxyQuota_空出口(t *testing.T) {
	resetQuotaCooldownState()
	defer resetQuotaCooldownState()

	if got := cooldownZenProxyQuota("", 0); got != 0 {
		t.Fatalf("空出口应返回 0, 实得 %v", got)
	}
}

// 不同出口的计数互不干扰 —— 冷却键必须是出口标识, 不是列表下标。
func TestCooldownZenProxyQuota_出口间独立(t *testing.T) {
	resetQuotaCooldownState()
	defer resetQuotaCooldownState()

	cooldownZenProxyQuota("socks5://a:1", 0)
	cooldownZenProxyQuota("socks5://a:1", 0)
	// b 是第一次, 不该受 a 的升级影响
	if got := cooldownZenProxyQuota("socks5://b:2", 0); got != zenQuotaCooldownBase {
		t.Fatalf("另一个出口应独立计数(基准 %v), 实得 %v", zenQuotaCooldownBase, got)
	}
}
