package app

// 出口选路增强 (P2 批次, 参照 easy_proxies / mihomo url-test / Resin 的机制设计,
// 代码为原创实现 —— easy_proxies 仓库无 LICENSE, 不可复制其代码):
//
//   1. latency 选路策略: 实测延迟最低优先, 带 1.2 倍容差防抖(mihomo url-test
//      的 tolerance 思路) —— 两个速度接近的节点之间不反复横跳;
//   2. 手动拉黑/恢复: 探测自动冷却之外, 允许人工把某个节点拉黑(带到期时间),
//      选路层无条件跳过;
//   3. 粘性会话(Sticky Session, Resin 的核心卖点): 按客户端来源 IP 把会话
//      钉在固定出口上(带 TTL), 服务于"同 IP 连续请求"的场景(如 CF 风控类上游)。

import (
	"sort"
	"sync"
	"time"
)

// ============ 1) latency 选路策略 ============

const (
	latencyTolerance = 1.2 // 容差: 上次选中节点延迟 <= 最优×1.2 时保持不动
	latencyUnknown   = int64(99999)
)

var latencyPickMu sync.Mutex
var latencyPickLast string // 上次选中的 nodeLocalKey(容差防抖)

// latencyPick 从可用候选里选实测延迟最低的节点。
// list 是完整出口列表, avail 是其中通过全部过滤条件的下标。
func latencyPick(list []string, avail []int) int {
	type cand struct {
		idx int
		lat int64
	}
	cands := make([]cand, 0, len(avail))
	for _, j := range avail {
		lat := latencyUnknown
		if r, ok := healthResultOf(nodeLocalKey(list[j])); ok && r.LatencyMs > 0 {
			lat = r.LatencyMs
		}
		cands = append(cands, cand{idx: j, lat: lat})
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].lat < cands[j].lat })
	if len(cands) == 0 {
		return 0
	}
	best := cands[0]

	latencyPickMu.Lock()
	last := latencyPickLast
	latencyPickMu.Unlock()
	if last != "" {
		for _, c := range cands {
			if nodeLocalKey(list[c.idx]) == last {
				// 容差防抖(mihomo url-test 的 tolerance 思路):
				// 上次选中节点的延迟不劣于最优的 1.2 倍就保持不动
				if float64(c.lat) <= float64(best.lat)*latencyTolerance {
					return c.idx
				}
				break
			}
		}
	}
	latencyPickMu.Lock()
	latencyPickLast = nodeLocalKey(list[best.idx])
	latencyPickMu.Unlock()
	return best.idx
}

// ============ 2) 手动拉黑 ============

var (
	manualBlackMu sync.Mutex
	manualBlack   = map[string]time.Time{} // nodeLocalKey -> 到期时间(零值 = 长期)
)

// nodeManuallyBlacklisted 节点是否处于人工拉黑期(选路层无条件跳过)。
func nodeManuallyBlacklisted(key string) bool {
	manualBlackMu.Lock()
	defer manualBlackMu.Unlock()
	until, ok := manualBlack[key]
	if !ok {
		return false
	}
	if !until.IsZero() && time.Now().After(until) {
		delete(manualBlack, key) // 到期自动解除
		return false
	}
	return true
}

// blacklistNode 人工拉黑节点。d==0 表示长期(手动解除前一直生效);
// d<0 表示已过期(立即解除, 供测试与清理使用)。
func blacklistNode(key string, d time.Duration) {
	manualBlackMu.Lock()
	defer manualBlackMu.Unlock()
	if d == 0 {
		manualBlack[key] = time.Time{}
		return
	}
	manualBlack[key] = time.Now().Add(d)
}

// clearBlacklistNode 解除人工拉黑。
func clearBlacklistNode(key string) {
	manualBlackMu.Lock()
	defer manualBlackMu.Unlock()
	delete(manualBlack, key)
}

// blacklistList 当前拉黑清单(面板展示)。
func blacklistList() []map[string]any {
	manualBlackMu.Lock()
	defer manualBlackMu.Unlock()
	out := make([]map[string]any, 0, len(manualBlack))
	for k, until := range manualBlack {
		e := map[string]any{"key": k, "permanent": until.IsZero()}
		if !until.IsZero() {
			e["until"] = until.Format(time.RFC3339)
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["key"].(string) < out[j]["key"].(string) })
	return out
}

// ============ 3) 粘性会话 ============

const stickySessionTTL = 30 * time.Minute

var (
	stickyMu     sync.Mutex
	stickyPinMap = map[string]stickyPin{} // 客户端 IP -> 钉住的出口
)

type stickyPin struct {
	Proxy string
	Key   string // nodeLocalKey(健康复核用)
	Until time.Time
}

// stickyPinFor 取某客户端当前钉住的出口; 过期/失效返回 false。
// stillHealthy 由调用方注入复核函数(选路上下文才知道节点是否可用)。
func stickyPinFor(clientIP string) (stickyPin, bool) {
	if clientIP == "" {
		return stickyPin{}, false
	}
	stickyMu.Lock()
	defer stickyMu.Unlock()
	pin, ok := stickyPinMap[clientIP]
	if !ok {
		return stickyPin{}, false
	}
	if time.Now().After(pin.Until) {
		delete(stickyPinMap, clientIP)
		return stickyPin{}, false
	}
	return pin, true
}

// stickyPinFor 钉住/续期某客户端的出口。
func stickyPinSave(clientIP, proxy, key string) {
	if clientIP == "" || proxy == "" {
		return
	}
	stickyMu.Lock()
	defer stickyMu.Unlock()
	stickyPinMap[clientIP] = stickyPin{Proxy: proxy, Key: key, Until: time.Now().Add(stickySessionTTL)}
	// 顺带清理过期项, 避免 map 随客户端数无限增长
	if len(stickyPinMap) > 512 {
		now := time.Now()
		for k, v := range stickyPinMap {
			if now.After(v.Until) {
				delete(stickyPinMap, k)
			}
		}
	}
}

// stickySessionEnabled 粘性会话是否开启(面板/配置控制, 缺省关闭)。
func stickySessionEnabled() bool {
	cfg := getZenConfig()
	return cfg != nil && cfg.StickySessions
}
