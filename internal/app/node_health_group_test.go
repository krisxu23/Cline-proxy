package app

import (
	"sync"
	"testing"
)

// outboundHostPort 取上游 host:port —— 健康检测按服务器分组依赖它。
func TestOutboundHostPort(t *testing.T) {
	cases := []struct {
		ob   map[string]any
		want string
	}{
		{map[string]any{"server": "1.2.3.4", "server_port": 443}, "1.2.3.4:443"},
		// server_port 经 JSON 往返后是 float64(订阅下发的 sing-box JSON 走这条路)
		{map[string]any{"server": "1.2.3.4", "server_port": float64(8443)}, "1.2.3.4:8443"},
		{map[string]any{"server": "a.example.com", "server_port": "2053"}, "a.example.com:2053"},
		// 取不到端口时退化成只返回 host(仍可用于分组: 同一主机的变体仍能聚在一起)
		{map[string]any{"server": "1.2.3.4"}, "1.2.3.4"},
		{map[string]any{"server": "1.2.3.4", "server_port": 0}, "1.2.3.4"},
		{map[string]any{"server_port": 443}, ""},
		{map[string]any{}, ""},
	}
	for i, c := range cases {
		if got := outboundHostPort(c.ob); got != c.want {
			t.Errorf("case %d: outboundHostPort = %q, want %q", i, got, c.want)
		}
	}
}

// nodeRemoteEndpoints 是整批替换的快照语义: 新表必须完整覆盖旧表(清掉已下线的节点)。
func TestNodeRemoteEndpointsSnapshot(t *testing.T) {
	prev := nodeRemoteEndpointOf("k1") // 记录初始(通常为空)
	_ = prev
	t.Cleanup(func() { setNodeRemoteEndpoints(map[string]string{}) })

	setNodeRemoteEndpoints(map[string]string{"k1": "1.1.1.1:443", "k2": "2.2.2.2:443"})
	if got := nodeRemoteEndpointOf("k1"); got != "1.1.1.1:443" {
		t.Fatalf("k1 = %q", got)
	}
	// 整批替换: k2 消失, k1 更新
	setNodeRemoteEndpoints(map[string]string{"k1": "3.3.3.3:443"})
	if got := nodeRemoteEndpointOf("k2"); got != "" {
		t.Fatalf("整批替换后 k2 应不存在, got %q", got)
	}
	if got := nodeRemoteEndpointOf("k1"); got != "3.3.3.3:443" {
		t.Fatalf("k1 应更新为 3.3.3.3:443, got %q", got)
	}
}

// 出口池里已消失的节点必须从健康表清掉: 残留条目永不覆写, 其旧 ExitIP 还会
// 参与 exit-fold 折叠, 让存活的兄弟节点被误判重复而跳过选路(2026-09-22 审查 P3)。
func TestPruneStaleNodeHealth(t *testing.T) {
	nodeHealthMu.Lock()
	prev := nodeHealth
	nodeHealth = map[string]nodeHealthState{
		"gone":  {Ok: true, Result: nodeTestResult{Alive: true, ExitIP: "9.9.9.9"}},
		"alive": {Ok: true},
	}
	nodeHealthMu.Unlock()
	t.Cleanup(func() {
		nodeHealthMu.Lock()
		nodeHealth = prev
		nodeHealthMu.Unlock()
	})

	pruneStaleNodeHealth([]string{"alive"})

	nodeHealthMu.RLock()
	_, hasGone := nodeHealth["gone"]
	_, hasAlive := nodeHealth["alive"]
	nodeHealthMu.RUnlock()
	if hasGone {
		t.Fatal("已消失节点的健康条目应被清除")
	}
	if !hasAlive {
		t.Fatal("在池节点的健康条目必须保留")
	}
}

// ★ 核心回归: 同一台服务器的变体**不重复探测**。
//
// 实测背景: 订阅源为同一台服务器生成多个 SNI 变体(13,841 实例 / 3,722 个
// host:port, 平均 3.7 个; 最极端的单台 94 个)。服务器连不上时逐个探测纯属浪费。
// 本用例断言:
//  1. 代表变体 Alive=false 的组, 先抽 1 个变体复探确认(防瞬态失败误杀整组,
//     审查 P2), 确认仍死则其余变体**一次都不探** —— 死组总探测数 = 2;
//  2. 那些变体被**显式记为不可用**(不是留空 —— 留空会被 nodeUsable 当"未探测即可用");
//  3. 代表变体 Alive=true 的组, 其余变体**仍然逐个探**(SNI 变体可能确实不同)。
func TestCheckAllNodeHealthSkipsSiblingsOfDeadServer(t *testing.T) {
	// ---- 保存并重建全局状态 ----
	nodeMu.Lock()
	prevPorts := nodePorts
	nodePorts = map[string]int{"a1": 1, "a2": 2, "a3": 3, "b1": 4, "c1": 5, "c2": 6}
	nodeMu.Unlock()

	nodeHealthMu.Lock()
	prevHealth := nodeHealth
	nodeHealth = map[string]nodeHealthState{}
	nodeHealthMu.Unlock()

	nodeHealthRunMu.Lock()
	prevRunning, prevAgain := nodeHealthRunning, healthRunAgain
	nodeHealthRunning, healthRunAgain = false, false
	nodeHealthRunMu.Unlock()

	// a1/a2/a3 同一台(死); b1 独占一台; c1/c2 同一台(活)
	setNodeRemoteEndpoints(map[string]string{
		"a1": "10.0.0.1:443", "a2": "10.0.0.1:443", "a3": "10.0.0.1:443",
		"b1": "10.0.0.2:443",
		"c1": "10.0.0.3:443", "c2": "10.0.0.3:443",
	})

	// ---- 探测替身: 按分组返回生死, 并记录每个 key 被探了几次 ----
	prevFn := testNodeComprehensiveFn
	var mu sync.Mutex
	probed := map[string]int{}
	testNodeComprehensiveFn = func(key string) nodeTestResult {
		mu.Lock()
		probed[key]++
		mu.Unlock()
		// 10.0.0.1 这台连不上; 10.0.0.2 / 10.0.0.3 正常
		if v := nodeRemoteEndpointOf(key); v == "10.0.0.1:443" {
			return nodeTestResult{Alive: false}
		}
		return nodeTestResult{Alive: true}
	}
	t.Cleanup(func() {
		testNodeComprehensiveFn = prevFn
		nodeMu.Lock()
		nodePorts = prevPorts
		nodeMu.Unlock()
		nodeHealthMu.Lock()
		nodeHealth = prevHealth
		nodeHealthMu.Unlock()
		nodeHealthRunMu.Lock()
		nodeHealthRunning, healthRunAgain = prevRunning, prevAgain
		nodeHealthRunMu.Unlock()
		setNodeRemoteEndpoints(map[string]string{})
	})

	checkAllNodeHealth()

	mu.Lock()
	aTotal := probed["a1"] + probed["a2"] + probed["a3"]
	bTotal := probed["b1"]
	cTotal := probed["c1"] + probed["c2"]
	mu.Unlock()

	// 1) 死服务器: 代表 + 1 个复探确认共 2 次; 第三个变体(a3)一次都不探
	if aTotal != 2 {
		t.Fatalf("同一台死服务器应只探测 2 次(代表变体 + 1 个复探确认), 实际 %d 次: %v", aTotal, probed)
	}
	if probed["a3"] != 0 {
		t.Fatalf("复探确认后其余变体应一次都不探, a3 实际 %d 次: %v", probed["a3"], probed)
	}
	// 2) 独占一台的节点正常探
	if bTotal != 1 {
		t.Fatalf("独占服务器的节点应被探测 1 次, 实际 %d", bTotal)
	}
	// 3) 活服务器的两个变体都要探(不能因为同组就跳过)
	if cTotal != 2 {
		t.Fatalf("代表变体存活时, 同组其余变体仍须逐个探测(应 2 次), 实际 %d: %v", cTotal, probed)
	}

	nodeHealthMu.Lock()
	defer nodeHealthMu.Unlock()
	// 4) 被跳过的兄弟变体必须**显式记为不可用**, 不能留空
	skippedMarked := 0
	for _, k := range []string{"a1", "a2", "a3"} {
		st, ok := nodeHealth[k]
		if !ok {
			t.Fatalf("%s 没有被写入 nodeHealth —— 留空会被 nodeUsable 当\"未探测即可用\"而继续选路", k)
		}
		if st.Ok {
			t.Fatalf("%s 与死服务器同组, 必须记为不可用", k)
		}
		skippedMarked++
	}
	if skippedMarked != 3 {
		t.Fatalf("死组 3 个变体都应记为不可用, 实际 %d", skippedMarked)
	}
	// 5) 活组的变体记为可用
	for _, k := range []string{"c1", "c2"} {
		if st, ok := nodeHealth[k]; !ok || !st.Ok {
			t.Fatalf("%s 应记为可用: %+v ok=%v", k, st, ok)
		}
	}
}
