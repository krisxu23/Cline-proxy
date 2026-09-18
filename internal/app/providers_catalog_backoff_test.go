package app

import (
	"strconv"
	"testing"
	"time"
)

// 目录刷新的**失败退避**与**控制面不污染数据面出口池**——
// 2026-09-17 线上实证: 目录刷新失败时每次轮换都会冷却出口, 9 个 provider × 16 次
// = 144 次冷却/分钟, 而健康出口只有 25~95 个 → 池子被抽干 → 所有请求退化成直连
// 并失败(日志满屏"节点池无可用出口")。以下两条测试各自锁住半个修复。

// catalogIntervalFor: 首次失败仍维持基准(单次抖动要能快速恢复), 从第 2 次连续
// 失败起按 2 的幂放大, 封顶 6h。
func TestCatalogIntervalForBackoff(t *testing.T) {
	cases := []struct {
		streak int
		want   time.Duration
	}{
		{0, providerCatalogRefresh}, // 15min — 未失败, 基准
		{1, providerCatalogRefresh}, // 15min — 首次失败仍按基准(暂态)
		{2, 30 * time.Minute},       // ×2
		{3, 60 * time.Minute},       // ×4
		{4, 120 * time.Minute},      // ×8
		{5, 240 * time.Minute},      // ×16
		{6, catalogFailBackoffMax},  // ×32 → 钳到 6h
		{50, catalogFailBackoffMax}, // 深度失败仍 6h
	}
	for _, c := range cases {
		if got := catalogIntervalFor(c.streak); got != c.want {
			t.Errorf("catalogIntervalFor(%d) = %v, want %v", c.streak, got, c.want)
		}
	}
	// 单调不减 + 不超上限
	prev := time.Duration(0)
	for s := 0; s < 40; s++ {
		iv := catalogIntervalFor(s)
		if iv < prev {
			t.Fatalf("间隔必须单调不减: streak=%d 得 %v < %v", s, iv, prev)
		}
		if iv > catalogFailBackoffMax {
			t.Fatalf("间隔不得超上限: streak=%d 得 %v", s, iv)
		}
		prev = iv
	}
}

// catalogRequestGateMs: 暂态失败(< catalogFastRetryStreak)保持 1 分钟快速重试,
// 之后切到退避间隔。这条分级同时满足两个约束 —— 抖动快恢复 + 持续失败不空转。
func TestCatalogRequestGateMs(t *testing.T) {
	for s := 0; s < catalogFastRetryStreak; s++ {
		if got := catalogRequestGateMs(s); got != providerCatalogRetryMs {
			t.Errorf("streak=%d 应为 1 分钟短门限, got %d ms", s, got)
		}
	}
	// 达到阈值那一次就切到退避间隔(catalogIntervalFor(阈值))
	wantSwitch := int64(catalogIntervalFor(catalogFastRetryStreak) / time.Millisecond)
	if got := catalogRequestGateMs(catalogFastRetryStreak); got != wantSwitch {
		t.Errorf("streak=%d 应切到退避间隔 %d ms, got %d ms", catalogFastRetryStreak, wantSwitch, got)
	}
	if catalogRequestGateMs(catalogFastRetryStreak-1) == wantSwitch {
		t.Fatal("退避必须在阈值前后发生切换, 否则分级没有意义")
	}
	deep := catalogRequestGateMs(30)
	if deep != int64(catalogFailBackoffMax/time.Millisecond) {
		t.Errorf("深度失败门限应为 6h, got %d ms", deep)
	}
	if deep <= providerCatalogRetryMs {
		t.Fatal("退避门限必须显著大于短门限, 否则退避无效")
	}
}

// noteCatalogSuccess / noteCatalogFailure 维护 streak。
func TestCatalogFailStreakLifecycle(t *testing.T) {
	p := &modelProvider{name: "bai"}
	if p.catalogRefreshInterval() != providerCatalogRefresh {
		t.Fatal("初始应为基准间隔")
	}
	p.noteCatalogFailure()
	p.noteCatalogFailure()
	if got := p.catalogRefreshInterval(); got != 30*time.Minute {
		t.Fatalf("连续 2 次失败应为 30min, got %v", got)
	}
	p.noteCatalogFailure()
	if got := p.catalogRefreshInterval(); got != 60*time.Minute {
		t.Fatalf("连续 3 次失败应为 1h, got %v", got)
	}
	p.noteCatalogSuccess()
	if got := p.catalogRefreshInterval(); got != providerCatalogRefresh {
		t.Fatalf("成功后应清零回基准, got %v", got)
	}
	// 带锁读 streak 不得死锁(由 -race + 超时兜底): 方法内部自己取锁。
	p.noteCatalogFailure()
	_ = p.catalogRefreshInterval()
}

// ★ 核心回归: 目录轮换**不得**冷却数据面出口池。
//
// 旧实现调 rotateProviderExit → cooldownZenProxyByIndex, 这是把控制面(拉 /models)
// 的失败记到数据面(对话)的出口上, 且聚合量无上界。这里断言轮换到底也不产生任何
// 出口冷却项。
func TestCatalogRotationDoesNotCoolExitPool(t *testing.T) {
	proxies := []string{"http://127.0.0.1:19101", "http://127.0.0.1:19102", "http://127.0.0.1:19103"}
	withTestConfig(t, &zenConfigData{ExitMode: exitModeProxy, Proxies: proxies})

	// 清空冷却表, 确保断言的是本次调用产生的效果
	zenProxyCooldownsMu.Lock()
	zenProxyCooldowns = map[string]time.Time{}
	zenProxyCooldownsMu.Unlock()
	// 预置轮询位置: 旧实现经 rotateProviderExit → lastZenProxyIdx() 取下标,
	// 计数为 0 时它返回 -1 会跳过冷却, 那样回归就抓不到。这里先把计数推上去,
	// 让"若退回旧实现则必然冷却"成立。
	prevCount := zenProxyCount.Load()
	zenProxyCount.Store(1)
	t.Cleanup(func() {
		zenProxyCount.Store(prevCount)
		zenProxyCooldownsMu.Lock()
		zenProxyCooldowns = map[string]time.Time{}
		zenProxyCooldownsMu.Unlock()
	})

	p := &modelProvider{name: "gemini"}
	// 反复触发连接层失败轮换(最典型的"池子整体连不上"场景)
	for i := 0; i < 64; i++ {
		p.retryCatalogOnNextExit(&catalogHTTPError{status: 0, body: ""})
	}

	zenProxyCooldownsMu.Lock()
	n := len(zenProxyCooldowns)
	zenProxyCooldownsMu.Unlock()
	if n != 0 {
		t.Fatalf("目录轮换不得冷却数据面出口, 却有 %d 个出口被冷却", n)
	}
}

// 轮换预算被压到 catalogExitMaxRotations: 池子再大也不会一次刷新烧掉几十个出口。
func TestCatalogRotationBudgetBounded(t *testing.T) {
	var proxies []string
	for i := 0; i < 200; i++ {
		proxies = append(proxies, "http://127.0.0.1:"+strconv.Itoa(19100+i))
	}
	withTestConfig(t, &zenConfigData{ExitMode: exitModeProxy, Proxies: proxies})
	if got := catalogExitBudget(); got != catalogExitMaxRotations {
		t.Fatalf("预算应被封顶为 %d, got %d", catalogExitMaxRotations, got)
	}
	if catalogExitMaxRotations > 4 {
		t.Fatalf("预算上限 %d 过大: 9 个 provider 同时轮换会抽干只有几十个健康出口的池子",
			catalogExitMaxRotations)
	}
}
