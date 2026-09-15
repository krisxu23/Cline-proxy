package app

// 出口选路增强 (P2) 的测试: latency 策略 / 手动拉黑 / 粘性会话。

import (
	"testing"
	"time"
)

func seedLatency(t *testing.T, key string, lat int64) {
	t.Helper()
	nodeHealthMu.Lock()
	nodeHealth[key] = nodeHealthState{Ok: true, At: time.Now(), Result: nodeTestResult{LatencyMs: lat, Alive: true}}
	nodeHealthMu.Unlock()
}

func TestLatencyPickPrefersLowestLatency(t *testing.T) {
	latencyPickMu.Lock()
	latencyPickLast = ""
	latencyPickMu.Unlock()
	a, b := "socks5://127.0.0.1:9001#lat-a", "socks5://127.0.0.1:9002#lat-b"
	seedLatency(t, nodeLocalKey(a), 300)
	seedLatency(t, nodeLocalKey(b), 80)
	avail := []int{0, 1}
	got := latencyPick([]string{a, b}, avail)
	if got != 1 {
		t.Fatalf("应选延迟最低的节点(下标 1), got %d", got)
	}
}

func TestLatencyPickTolerancePreventsFlapping(t *testing.T) {
	a, b := "socks5://127.0.0.1:9101#tol-a", "socks5://127.0.0.1:9102#tol-b"
	// a=100ms b=110ms: 差距在容差内, 且上次选中的是 a → 保持 a 不切换
	seedLatency(t, nodeLocalKey(a), 100)
	seedLatency(t, nodeLocalKey(b), 110)
	latencyPickMu.Lock()
	latencyPickLast = nodeLocalKey(a)
	latencyPickMu.Unlock()
	if got := latencyPick([]string{a, b}, []int{0, 1}); got != 0 {
		t.Fatalf("容差内应保持上次选中节点, got %d", got)
	}
	// 差距超过容差(b 明显更快) → 切换到 b
	seedLatency(t, nodeLocalKey(b), 50)
	if got := latencyPick([]string{a, b}, []int{0, 1}); got != 1 {
		t.Fatalf("差距超容差应切换, got %d", got)
	}
	latencyPickMu.Lock()
	latencyPickLast = ""
	latencyPickMu.Unlock()
}

func TestManualBlacklistLifecycle(t *testing.T) {
	key := "socks5://127.0.0.1:9201#bl"
	blacklistNode(key, 0) // 长期
	if !nodeManuallyBlacklisted(key) {
		t.Fatal("拉黑后应处于黑名单")
	}
	// 带到期时间的拉黑: 未到期有效
	blacklistNode(key, time.Hour)
	if !nodeManuallyBlacklisted(key) {
		t.Fatal("时限内应处于黑名单")
	}
	clearBlacklistNode(key)
	if nodeManuallyBlacklisted(key) {
		t.Fatal("解除后不应在黑名单")
	}
	// 到期自动解除
	blacklistNode(key, -time.Minute)
	if nodeManuallyBlacklisted(key) {
		t.Fatal("过期后应自动解除")
	}
}

func TestStickySessionPinAndExpiry(t *testing.T) {
	const ip = "203.0.113.7"
	stickyPinSave(ip, "socks5://127.0.0.1:9301#s", "key-s")
	pin, ok := stickyPinFor(ip)
	if !ok || pin.Proxy != "socks5://127.0.0.1:9301#s" {
		t.Fatalf("应取回钉住的出口, got %+v", pin)
	}
	// 过期: 手动把 Until 拨到过去
	stickyMu.Lock()
	e := stickyPinMap[ip]
	e.Until = time.Now().Add(-time.Second)
	stickyPinMap[ip] = e
	stickyMu.Unlock()
	if _, ok := stickyPinFor(ip); ok {
		t.Fatal("过期后应失效")
	}
	if _, ok := stickyPinFor(""); ok {
		t.Fatal("空 IP 不应有粘性")
	}
}
