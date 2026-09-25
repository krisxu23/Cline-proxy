package app

// Task 4 (4.1-4.4) + Task 9 pool part (9.1-9.3) + Task 1 pool part 回归锁.
// TDD 红测: 先断言目标行为, 再最小修. 全部用例名命中验收 -run 正则.

import (
	"context"
	"testing"
	"time"
)

func poolFixCleanSub(t *testing.T) {
	t.Helper()
	subMu.Lock()
	prevNodes := subNodes
	prevKeys := subNodeKeys
	subNodes = nil
	subNodeKeys = nil
	subMu.Unlock()
	t.Cleanup(func() {
		subMu.Lock()
		subNodes = prevNodes
		subNodeKeys = prevKeys
		subMu.Unlock()
	})
	invalidateExitListCache()
	t.Cleanup(invalidateExitListCache)
	clearZenProxyCooldowns()
	t.Cleanup(clearZenProxyCooldowns)
}

// 4.1: effectiveProxyList 一次性应用 NodeExcludeKeywords + filterByExitRegion;
// 空地区过滤只告警一次且回退完整列表(绝不黑名单清零).
func TestPoolExcludeKeywordsOnceAndRegionFallback(t *testing.T) {
	poolFixCleanSub(t)
	setNodeHealthForTest(t, "vmess://us-node", "US", true)
	setNodeHealthForTest(t, "vmess://jp-node", "JP", true)

	proxies := []string{
		"http://127.0.0.1:11080#官网节点",
		"vmess://us-node",
		"vmess://jp-node",
	}
	withTestConfig(t, &zenConfigData{
		Enabled: true, ExitMode: exitModeProxy,
		Proxies: proxies, NodeExcludeKeywords: []string{"官网"},
	})
	invalidateExitListCache()
	got := effectiveProxyList()
	if len(got) != 2 || got[0] != "vmess://us-node" || got[1] != "vmess://jp-node" {
		t.Fatalf("排除关键词应一次性生效且保留其余出口, got %v", got)
	}

	// 地区过滤接线: 只勾日本 -> 只剩日本节点.
	withTestConfig(t, &zenConfigData{
		Enabled: true, ExitMode: exitModeProxy,
		Proxies: proxies, NodeExcludeKeywords: []string{"官网"},
		EnabledRegions: []string{"jp"},
	})
	invalidateExitListCache()
	if got := effectiveProxyList(); len(got) != 1 || got[0] != "vmess://jp-node" {
		t.Fatalf("勾选日本后出口池应只剩日本节点, got %v", got)
	}

	// 空过滤: 勾美国但池里只有日本 -> 回退完整列表(不黑名单清零) + 只告警一次.
	regionFilterWarnMu.Lock()
	regionFilterWarnedFor = ""
	regionFilterWarnMu.Unlock()
	withTestConfig(t, &zenConfigData{
		Enabled: true, ExitMode: exitModeProxy,
		Proxies:        []string{"vmess://jp-a", "vmess://jp-b"},
		EnabledRegions: []string{"us"},
	})
	setNodeHealthForTest(t, "vmess://jp-a", "JP", true)
	setNodeHealthForTest(t, "vmess://jp-b", "JP", true)
	invalidateExitListCache()
	first := filterByExitRegion([]string{"vmess://jp-a", "vmess://jp-b"})
	invalidateExitListCache()
	second := filterByExitRegion([]string{"vmess://jp-a", "vmess://jp-b"})
	if len(first) != 2 || len(second) != 2 {
		t.Fatalf("空地区过滤必须回退完整列表(不黑名单), got %v / %v", first, second)
	}
	regionFilterWarnMu.Lock()
	warned := regionFilterWarnedFor
	regionFilterWarnMu.Unlock()
	if warned != "us" {
		t.Fatalf("空过滤应告警一次(regKey=us), got %q", warned)
	}
}

// 4.2: nodeUsable=false 当且仅当 healthOf==fail 或折叠副本;
// 未知/未探测可用, 普通代理恒可用.
func TestPoolNodeUsableFailOrFoldedOnly(t *testing.T) {
	poolFixCleanSub(t)
	if !nodeUsable("http://127.0.0.1:11080") {
		t.Fatal("普通代理应恒可用")
	}
	unknownLink := "vless://11111111-1111-1111-1111-111111111111@unknown.example.com:443#u"
	nodeHealthMu.Lock()
	delete(nodeHealth, nodeLocalKey(unknownLink))
	nodeHealthMu.Unlock()
	exitFoldMu.Lock()
	delete(exitFoldDupOf, nodeLocalKey(unknownLink))
	exitFoldMu.Unlock()
	if !nodeUsable(unknownLink) {
		t.Fatal("未知/未探测节点应可用")
	}

	failLink := "vless://22222222-2222-2222-2222-222222222222@fail.example.com:443#fail"
	setNodeHealthForTest(t, nodeLocalKey(failLink), "US", false)
	if nodeUsable(failLink) {
		t.Fatal("healthOf==fail 的节点必须不可用")
	}

	dupLink := "vless://33333333-3333-3333-3333-333333333333@dup.example.com:443#dup"
	setNodeHealthForTest(t, nodeLocalKey(dupLink), "US", true)
	exitFoldMu.Lock()
	prev, hadPrev := exitFoldDupOf[nodeLocalKey(dupLink)]
	exitFoldDupOf[nodeLocalKey(dupLink)] = "some-best"
	exitFoldMu.Unlock()
	t.Cleanup(func() {
		exitFoldMu.Lock()
		if hadPrev {
			exitFoldDupOf[nodeLocalKey(dupLink)] = prev
		} else {
			delete(exitFoldDupOf, nodeLocalKey(dupLink))
		}
		exitFoldMu.Unlock()
	})
	if nodeUsable(dupLink) {
		t.Fatal("折叠副本必须不可用")
	}
}

// 4.3: lastZenProxyIdx 必须来自拨号层 setLastZenExit 原子回写,
// 而不是轮询计数反推; 直连兜底/已移除 key 返回 -1.
func TestPickLastZenIdxFromDialLayerAtomic(t *testing.T) {
	poolFixCleanSub(t)
	a, b := "http://127.0.0.1:19311", "http://127.0.0.1:19312"
	withTestConfig(t, &zenConfigData{Enabled: true, ExitMode: exitModeProxy, Proxies: []string{a, b}})
	invalidateExitListCache()
	t.Cleanup(func() { setLastZenExit("") })

	setLastZenExit(b)
	if idx := lastZenProxyIdx(); idx != 1 {
		t.Fatalf("拨号层回写 %s 后下标应为 1, got %d", b, idx)
	}
	setLastZenExit(a)
	if idx := lastZenProxyIdx(); idx != 0 {
		t.Fatalf("拨号层回写 %s 后下标应为 0, got %d", a, idx)
	}
	setLastZenExit("")
	if idx := lastZenProxyIdx(); idx != -1 {
		t.Fatalf("直连兜底(空key)应返回 -1, got %d", idx)
	}
	setLastZenExit("http://127.0.0.1:19999-gone")
	if idx := lastZenProxyIdx(); idx != -1 {
		t.Fatalf("已移除的key应返回 -1, got %d", idx)
	}
}

// 4.4: cooldown 只标记真实尝试的出口; 健康的未触碰出口绝不继承冷却.
func TestCooldownMarksOnlyRealExit(t *testing.T) {
	poolFixCleanSub(t)
	a, b := "http://127.0.0.1:19411", "http://127.0.0.1:19412"
	withTestConfig(t, &zenConfigData{Enabled: true, ExitMode: exitModeProxy, Proxies: []string{a, b}})

	ctx := context.WithValue(context.Background(), ctxKeyReqExit, &reqExit{})
	setReqExit(ctx, a)
	cooldownActualExit(ctx, time.Minute)
	if zenProxyAvailable(a) {
		t.Fatal("真实尝试的出口必须被冷却")
	}
	if !zenProxyAvailable(b) {
		t.Fatal("未触碰的健康出口绝不能继承冷却")
	}

	// 无 reqExit 的 ctx: 空操作, 不 panic 也不污染池子.
	clearZenProxyCooldowns()
	cooldownActualExit(context.Background(), time.Minute)
	if !zenProxyAvailable(a) || !zenProxyAvailable(b) {
		t.Fatal("直连/无出口 ctx 不应冷却任何出口")
	}
}

// 9.1: 地区受限模型排除 unknown 档(有已知可用时绝不轮到未知);
// 非受限模型保留 unknown; UI 的 filterByExitRegion 语义不变.
func TestRegionRestrictedExcludesUnknown(t *testing.T) {
	poolFixCleanSub(t)
	a, b, c := "http://127.0.0.1:19511", "http://127.0.0.1:19512", "http://127.0.0.1:19513"
	withTestConfig(t, &zenConfigData{Enabled: true, ExitMode: exitModeProxy, Proxies: []string{a, b, c}})
	const model = "pool-91-spark"
	markRegionModelForTest(t, model)
	setRegionNodeOK(model, nodeLocalKey(c), true) // 仅 c 已知可用, a/b 保持未知

	for i := 0; i < 8; i++ {
		p, _ := pickZenProxyForModel(model)
		if p != c {
			t.Fatalf("已知可用存在时受限模型不得选中未知出口: got %s want %s", p, c)
		}
	}

	// 非受限模型: 未知节点照常参与(轮询能转到 a/b).
	seen := map[string]bool{}
	for i := 0; i < 6; i++ {
		p, _ := pickZenProxyForModel("pool-91-mimo")
		seen[p] = true
	}
	if !seen[a] || !seen[b] {
		t.Fatalf("非受限模型必须保留未知出口, picks=%v", seen)
	}

	// UI 过滤语义不变: 勾选 us 时未知节点仍被排除(归 other).
	setNodeHealthForTest(t, "vmess://us-91", "US", true)
	withTestConfig(t, &zenConfigData{Enabled: true, ExitMode: exitModeProxy, EnabledRegions: []string{"us"}})
	if exitRegionAllowed("vmess://never-probed-91") {
		t.Fatal("UI 地区过滤下未知节点仍应被排除")
	}
	if !exitRegionAllowed("vmess://us-91") {
		t.Fatal("UI 地区过滤下美国节点应可用")
	}
}

// Task 1 pool part + 9.2: 空池时受限模型回退轮询、绝不直连(("",-1)).
func TestPoolRestrictedEmptyPoolNeverDirect(t *testing.T) {
	poolFixCleanSub(t)
	withTestConfig(t, &zenConfigData{Enabled: true, ExitMode: exitModeProxy})
	const model = "pool-empty-spark"
	markRegionModelForTest(t, model)
	p, idx := pickZenProxyForModel(model)
	if p != "" || idx != -1 {
		t.Fatalf("空池时受限模型必须无候选(绝不伪造直连), got %q %d", p, idx)
	}
}
