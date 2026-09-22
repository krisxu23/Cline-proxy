package app

import (
	"testing"
	"time"
)

// 回归: probeUpstreamMatrixAsync 只能由工作协程写内层 map, 外层 map 必须在
// 并发开始前就建好。原实现是主协程边循环边 next[upstream] = ... 而工作协程
// 同时在读外层 map —— Go 对 map 的并发读写是 fatal error(不可 recover),
// 直接把整个网关打死。用 -race 跑这个用例即可抓住回归。
func TestProbeUpstreamMatrixNoConcurrentMapAccess(t *testing.T) {
	withTestConfig(t, &zenConfigData{
		ExitMode: exitModeProxy,
		Proxies: []string{
			"http://127.0.0.1:19101",
			"http://127.0.0.1:19102",
			"http://127.0.0.1:19103",
		},
	})
	// host 用 127.0.0.1, 出口是死端口: 探测会立刻失败返回, 不依赖外网
	setTestProvider(t, "p1", providerConfig{BaseURL: "http://127.0.0.1:19201/v1", APIKey: "k1"})
	setTestProvider(t, "p2", providerConfig{BaseURL: "http://127.0.0.1:19202/v1", APIKey: "k2"})
	setTestProvider(t, "p3", providerConfig{BaseURL: "http://127.0.0.1:19203/v1", APIKey: "k3"})

	nodeUpstreamMu.Lock()
	prevLast, prevProbing := nodeUpstreamLastAt, nodeUpstreamProbing
	nodeUpstreamLastAt = time.Time{}
	nodeUpstreamProbing = false
	nodeUpstreamMu.Unlock()
	t.Cleanup(func() {
		nodeUpstreamMu.Lock()
		nodeUpstreamLastAt = prevLast
		nodeUpstreamProbing = prevProbing
		nodeUpstreamMu.Unlock()
	})

	probeUpstreamMatrixAsync()

	exits := len(effectiveProxyList())
	if exits == 0 {
		t.Fatal("测试配置里应有出口")
	}
	// [W5-P2-2 适配] probeUpstreamMatrixAsync 已改为真异步(调用立即返回,
	// 探测体在独立协程里跑完再整批写 nodeUpstreamOK)。原断言在调用后同步读
	// 结果必然读到空 map, 改为轮询等待探测协程收尾(置 probing=false 时结果已落盘)。
	deadline := time.Now().Add(15 * time.Second)
	for {
		nodeUpstreamMu.RLock()
		done := !nodeUpstreamProbing && !nodeUpstreamLastAt.IsZero()
		nodeUpstreamMu.RUnlock()
		if done {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("等待上游可达性探测完成超时")
		}
		time.Sleep(20 * time.Millisecond)
	}
	nodeUpstreamMu.RLock()
	defer nodeUpstreamMu.RUnlock()
	if len(nodeUpstreamOK) == 0 {
		t.Fatal("应至少写入一个上游的可达性结果")
	}
	// 结果必须完整覆盖每个上游 × 每个出口: 并发写丢数据同样是缺陷
	for up, m := range nodeUpstreamOK {
		if len(m) != exits {
			t.Errorf("上游 %s 只有 %d 条结果, 期望 %d", up, len(m), exits)
		}
	}
}
