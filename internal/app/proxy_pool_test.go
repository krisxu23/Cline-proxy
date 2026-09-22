package app

import (
	"testing"
	"time"
)

// 整池都处于冷却时, 必须返回"直连决策", 而不是把冷却中的出口照旧选中。
//
// 回归自 pickZenProxyWhere 的一个缺陷: 线性探测跑满 n 次后会落回起始下标,
// 之后只检查一次 nodeDialable 就把它返回。而普通代理 URL(http/socks5)的
// nodeDialable 与 nodeUsable 恒为 true, 于是"冷却"这一个条件被整个绕过 ——
// 冷却形同虚设, 撞了限流的出口会被立刻反复重用。
//
// 同一段代码里 extra(上游可达性)那条分支本来就有正确写法(返回 "", -1),
// 说明冷却与健康度这两条是漏了。
func TestPickZenProxyRefusesCooledDownExit(t *testing.T) {
	withTestConfig(t, &zenConfigData{
		ExitMode:      exitModeProxy,
		ProxyStrategy: "fill", // 固定从下标 0 开始, 结果可预期
		Proxies:       []string{"socks5://127.0.0.1:1080"},
	})
	// 订阅解析出的节点是进程级状态, 清掉才能让"池里只有这一个出口"成立。
	subMu.Lock()
	prevSub := subNodes
	subNodes = nil
	subMu.Unlock()
	t.Cleanup(func() {
		subMu.Lock()
		subNodes = prevSub
		subMu.Unlock()
	})
	clearZenProxyCooldowns()
	t.Cleanup(clearZenProxyCooldowns)

	p, idx := pickZenProxy()
	if p != "socks5://127.0.0.1:1080" || idx != 0 {
		t.Fatalf("冷却前应选中池内唯一出口, 得到 %q %d", p, idx)
	}

	cooldownZenProxy("socks5://127.0.0.1:1080", time.Minute)

	if p, idx := pickZenProxy(); p != "" || idx != -1 {
		t.Fatalf("唯一出口处于冷却时必须返回直连决策, 得到 %q %d", p, idx)
	}
}

// 慢节点降权(2026-09-22 审查): 实测吞吐 < nodeSpeedSlowBPS 的节点不判死,
// 但有正常节点时绝不选它; 正常节点全不可用时才降级兜底。
// 配置顺序故意把慢节点放前面 —— 不做降权时 fill 策略会取到它, 用例即红。
func TestPickZenProxyDeprioritizesSlowNodes(t *testing.T) {
	slowLink := "vless://11111111-1111-1111-1111-111111111111@slow.example.com:443?security=tls#slow"
	fastLink := "vless://22222222-2222-2222-2222-222222222222@fast.example.com:443?security=tls#fast"
	slowKey, fastKey := nodeLocalKey(slowLink), nodeLocalKey(fastLink)

	withTestConfig(t, &zenConfigData{
		ExitMode:      exitModeProxy,
		ProxyStrategy: "fill",
		Proxies:       []string{slowLink, fastLink}, // 慢在前: 降权失效时会被选中
	})
	subMu.Lock()
	prevSub := subNodes
	subNodes = nil
	subMu.Unlock()
	t.Cleanup(func() {
		subMu.Lock()
		subNodes = prevSub
		subMu.Unlock()
	})

	nodeMu.Lock()
	prevPorts := nodePorts
	nodePorts = map[string]int{slowKey: 17201, fastKey: 17202}
	nodeMu.Unlock()
	t.Cleanup(func() {
		nodeMu.Lock()
		nodePorts = prevPorts
		nodeMu.Unlock()
	})

	nodeHealthMu.Lock()
	prevHealth := nodeHealth
	nodeHealth = map[string]nodeHealthState{
		slowKey: {Ok: true, Result: nodeTestResult{Alive: true, SpeedBPS: 10000}}, // < 50KB/s
		fastKey: {Ok: true, Result: nodeTestResult{Alive: true, SpeedBPS: 200000}},
	}
	nodeHealthMu.Unlock()
	t.Cleanup(func() {
		nodeHealthMu.Lock()
		nodeHealth = prevHealth
		nodeHealthMu.Unlock()
	})
	clearZenProxyCooldowns()
	t.Cleanup(clearZenProxyCooldowns)

	if p, _ := pickZenProxy(); p != fastLink {
		t.Fatalf("有快节点时必须跳过慢节点, got %q", p)
	}

	// 快节点判死 → 慢节点降级兜底, 而不是无出口(降权 ≠ 判死)
	nodeHealthMu.Lock()
	nodeHealth[fastKey] = nodeHealthState{Ok: false, Result: nodeTestResult{}}
	nodeHealthMu.Unlock()
	if p, _ := pickZenProxy(); p != slowLink {
		t.Fatalf("快节点不可用时慢节点应兜底, got %q", p)
	}

	// 测速失败(speedBPS==0)不算慢: 与真慢节点同池时仍可正常参与
	nodeHealthMu.Lock()
	nodeHealth[fastKey] = nodeHealthState{Ok: true, Result: nodeTestResult{Alive: true, SpeedTestFailed: true}}
	nodeHealthMu.Unlock()
	if p, _ := pickZenProxy(); p != fastLink {
		t.Fatalf("测速失败的节点不应被降权, got %q", p)
	}
}
