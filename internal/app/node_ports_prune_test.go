package app

// 稳定端口表按当前快照裁剪(2026-09-23)。
//
// 这张表的唯一用途是"让仍在订阅里的节点的本地入站端口不漂移", 但它此前只增不减,
// 会随订阅 churn 无限增长(实测 27634 条 / 3.8MB, 每次重建整份重写), 而实际生效
// 的只有当前池里那 1000 多条。

import "testing"

// withTestStablePorts 注入一份稳定端口表并屏蔽磁盘读取。
func withTestStablePorts(t *testing.T, ports map[string]int) {
	t.Helper()
	nodeStableMu.Lock()
	prev := nodeStablePorts
	prevLoaded := nodeStableLoaded
	// loaded 置真: 否则 pruneStablePortsTo 内部的惰性加载会用磁盘文件覆盖这里
	// 注入的 map, 断言就会看到空表(现有测试踩过同一个坑)。
	nodeStablePorts = ports
	nodeStableLoaded = true
	nodeStableMu.Unlock()
	t.Cleanup(func() {
		nodeStableMu.Lock()
		nodeStablePorts = prev
		nodeStableLoaded = prevLoaded
		nodeStableMu.Unlock()
	})
}

func stablePortsSnapshot() map[string]int {
	nodeStableMu.Lock()
	defer nodeStableMu.Unlock()
	out := make(map[string]int, len(nodeStablePorts))
	for k, v := range nodeStablePorts {
		out[k] = v
	}
	return out
}

// 不在当前快照里的记录必须被裁掉, 在的必须原样保留(端口稳定性不能丢)。
func TestPruneStablePortsToKeepsCurrentOnly(t *testing.T) {
	withTestStablePorts(t, map[string]int{
		"node-a": 20001, // 仍在订阅里
		"node-b": 20002, // 已离开订阅
		"node-c": 20003, // 已离开订阅
		"node-d": 20004, // 仍在订阅里
	})

	// 池规模没变(上一轮 2 个、本轮还是 2 个) → 守卫放行, 裁掉已离开的 2 条。
	dropped := pruneStablePortsTo(map[string]int{"node-a": 20001, "node-d": 20004}, 2)

	if dropped != 2 {
		t.Fatalf("应裁掉 2 条, got %d", dropped)
	}
	got := stablePortsSnapshot()
	if len(got) != 2 || got["node-a"] != 20001 || got["node-d"] != 20004 {
		t.Fatalf("只应保留 node-a / node-d 及其端口, got %v", got)
	}
}

// 快照明显缩小 = 疑似订阅抓取部分失败: 本轮不裁剪, 否则会把仍在的节点误删、
// 端口漂移 —— 那正是这张表要解决的问题。
func TestPruneStablePortsSkipsShrunkSnapshot(t *testing.T) {
	withTestStablePorts(t, map[string]int{
		"node-a": 21001,
		"node-b": 21002,
		"node-c": 21003,
		"node-d": 21004,
	})

	// 上一轮 100 个节点, 本轮只有 1 个 → 判为中间态, 不裁剪。
	if dropped := pruneStablePortsTo(map[string]int{"node-a": 21001}, 100); dropped != 0 {
		t.Fatalf("疑似部分抓取时不应裁剪, got %d", dropped)
	}
	if got := stablePortsSnapshot(); len(got) != 4 {
		t.Fatalf("表应保持原样, got %v", got)
	}
}

// 空快照不裁剪: 已被 syncNodeBox 的空入口守卫拦住, 这里再兜一层 ——
// 真让它裁下去会把整张表清空。
func TestPruneStablePortsSkipsEmptySnapshot(t *testing.T) {
	withTestStablePorts(t, map[string]int{"node-a": 22001})

	if dropped := pruneStablePortsTo(map[string]int{}, 0); dropped != 0 {
		t.Fatalf("空快照不应裁剪, got %d", dropped)
	}
	if got := stablePortsSnapshot(); len(got) != 1 {
		t.Fatalf("表应保持原样, got %v", got)
	}
}
