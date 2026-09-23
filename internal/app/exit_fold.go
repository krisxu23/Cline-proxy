package app

// 出口级去重 (P2, 对齐 freesub 的 "同出口IP+端口 只留最快" 语义):
//
// 多个不同节点(不同入口/协议/订阅源)可能落在**同一个出口 IP**上 —— 同一
// 落地机器换马甲。它们对"出口选择"完全等价, 全部参与轮询只会:
//   - 浪费探测与选择机会在同一条落地上;
//   - 命中同一落地的风控时集体失败。
//
// 处理: 每轮增强检测完成后, 按 ExitIP 折叠 —— 同出口只保留实测延迟最快的
// 一个做主力, 其余标记为折叠态, 选路层(nodeUsable)跳过。主力仍在探测,
// 若它劣化/死亡, 下一轮折叠会重新选举(其它兄弟自动转正)。
// 只按 ExitIP 折叠而不管节点自身端口: 同一出口 IP 本来就是同一台落地机,
// 端口差异只是同一台机器上的不同服务, 对出口选择无意义。

import (
	"sync"
)

var (
	exitFoldMu    sync.RWMutex
	exitFoldDupOf = map[string]string{} // 重复节点 key -> 它所在出口组的主力 key
)

// recomputeExitFold 按最新一轮检测结果重算出口折叠。
// 约束: 只折叠"有实测出口 IP 且健康"的节点; 同组内 latency 最小者为主力。
func recomputeExitFold() {
	// 折叠依赖 Result.ExitIP, 而它随健康表一起落盘 —— 不先恢复的话重启后折叠表
	// 是空的, 同出口 IP 的副本会全部参与选路(见 ensureNodeHealthLoaded)。
	ensureNodeHealthLoaded()
	type group struct {
		bestKey string
		bestLat int64
	}
	groups := map[string]*group{}

	nodeHealthMu.RLock()
	for key, st := range nodeHealth {
		ip := st.Result.ExitIP
		if ip == "" || !st.Ok {
			continue // 无出口 IP(未测/测挂)或节点不健康, 不参与折叠
		}
		lat := st.Result.LatencyMs
		if lat <= 0 {
			lat = int64(1 << 30) // 无延迟数据排后
		}
		g := groups[ip]
		if g == nil {
			groups[ip] = &group{bestKey: key, bestLat: lat}
			continue
		}
		if lat < g.bestLat {
			groups[ip].bestKey = key
			groups[ip].bestLat = lat
		}
	}
	nodeHealthMu.RUnlock()

	dupOf := make(map[string]string, len(groups))
	// 第二遍: 标记同组的非主力成员。
	nodeHealthMu.RLock()
	for key, st := range nodeHealth {
		ip := st.Result.ExitIP
		if ip == "" || !st.Ok {
			continue
		}
		if g := groups[ip]; g != nil && g.bestKey != key {
			dupOf[key] = g.bestKey
		}
	}
	nodeHealthMu.RUnlock()

	exitFoldMu.Lock()
	exitFoldDupOf = dupOf
	exitFoldMu.Unlock()
}

// nodeFoldedDuplicate 节点是否为同出口的折叠副本(选路应跳过)。
func nodeFoldedDuplicate(key string) bool {
	exitFoldMu.RLock()
	defer exitFoldMu.RUnlock()
	_, ok := exitFoldDupOf[key]
	return ok
}

// exitFoldDupTarget 折叠表的线程安全读取入口: 返回该 key 所在出口组的主力 key。
// recomputeExitFold 会在 exitFoldMu 下**整体替换** exitFoldDupOf, 任何读方都
// 必须经本函数(或自己套 RLock) —— 直接裸读 map 与替换并发即数据竞争
// (2026-09-15 R2 审计 F1: node_view.go 面板路径曾裸读)。
func exitFoldDupTarget(key string) (string, bool) {
	exitFoldMu.RLock()
	defer exitFoldMu.RUnlock()
	best, ok := exitFoldDupOf[key]
	return best, ok
}
