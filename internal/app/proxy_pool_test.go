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

	cooldownZenProxy(0, time.Minute)

	if p, idx := pickZenProxy(); p != "" || idx != -1 {
		t.Fatalf("唯一出口处于冷却时必须返回直连决策, 得到 %q %d", p, idx)
	}
}
