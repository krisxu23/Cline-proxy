package app

// 2026-09-24 计划 Task 8: 新节点国家快车道(host:port 复用)。
//
// 订阅 churn 后新出现的键, 若其远端 host:port 与某个已知键相同(同一台服务器换了
// 节点名/协议), 直接继承国家 —— 免掉一次探测请求。国家探测本身在 Task 6 之后
// 已是"单源首个成功即返回"。

import "testing"

func TestSeedNewNodeCountriesByHostPort(t *testing.T) {
	const (
		known  = "sbox://known-node"
		fresh  = "sbox://fresh-node"
		lonely = "sbox://lonely-node"
		hp     = "1.2.3.4:443"
	)

	nodeCountryMu.Lock()
	prevLoaded, prevCountries, prevDirty := nodeCountryLoaded, nodeCountryMap, nodeCountryDirty
	nodeCountryLoaded = true // 阻止 loadNodeCountriesLocked 从磁盘覆盖注入值
	nodeCountryMap = map[string]string{known: "US"}
	nodeCountryDirty = false
	nodeCountryMu.Unlock()

	nodeRemoteEndpointsMu.Lock()
	prevEndpoints := nodeRemoteEndpoints
	nodeRemoteEndpoints = map[string]string{known: hp, fresh: hp, lonely: "9.9.9.9:443"}
	nodeRemoteEndpointsMu.Unlock()

	nodeHealthMu.Lock()
	prevHealth := nodeHealth
	nodeHealth = map[string]nodeHealthState{}
	nodeHealthMu.Unlock()

	t.Cleanup(func() {
		nodeCountryMu.Lock()
		nodeCountryLoaded, nodeCountryMap, nodeCountryDirty = prevLoaded, prevCountries, prevDirty
		nodeCountryMu.Unlock()
		nodeRemoteEndpointsMu.Lock()
		nodeRemoteEndpoints = prevEndpoints
		nodeRemoteEndpointsMu.Unlock()
		nodeHealthMu.Lock()
		nodeHealth = prevHealth
		nodeHealthMu.Unlock()
	})

	// 同 host:port → 继承国家, 且**不写健康结论**(绝不因"没探到"就判死)。
	if n := seedNewNodeCountries([]string{fresh}); n != 1 {
		t.Fatalf("同 host:port 应继承 1 条, got %d", n)
	}
	if got := rememberedNodeCountry(fresh); got != "US" {
		t.Fatalf("新键应继承 US, got %q", got)
	}
	if h, ok := healthResultOf(fresh); ok {
		t.Fatalf("播种国家不得写健康结论: %+v", h)
	}

	// 没有同 host:port 的已知键 → 保持未知, 不播种。
	if n := seedNewNodeCountries([]string{lonely}); n != 0 {
		t.Fatalf("无同 host:port 时不应播种, got %d", n)
	}
	if got := rememberedNodeCountry(lonely); got != "" {
		t.Fatalf("无同 host:port 应保持未知, got %q", got)
	}

	// 已有健康结论的键不算"新节点" → 不播种(避免覆盖实测值)。
	nodeHealthMu.Lock()
	nodeHealth[fresh] = nodeHealthState{Ok: true}
	nodeHealthMu.Unlock()
	nodeCountryMu.Lock()
	delete(nodeCountryMap, fresh)
	nodeCountryMu.Unlock()
	if n := seedNewNodeCountries([]string{fresh}); n != 0 {
		t.Fatalf("已有健康结论的键不应被播种, got %d", n)
	}
}
