package app

// 稳定端口 / 每节点流量 / 构建去重 的测试。

import (
	"fmt"
	"net"
	"testing"
)

func TestCountingConnCountsUpDown(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	tc := trafficCounterFor("count-test")
	defer nodeTrafficReset()
	wrapped := &countingConn{Conn: client, up: &tc.up, down: &tc.down, last: &tc.last}

	// 对端 goroutine: 写 10 字节(触发下行计数), 再把上行写来的 5 字节读走
	// (net.Pipe 是同步管道, 对端不读会导致 Write 永久阻塞)
	go func() {
		server.Write([]byte("0123456789"))
		buf := make([]byte, 16)
		server.Read(buf)
	}()

	buf := make([]byte, 32)
	n, err := wrapped.Read(buf)
	if err != nil || n != 10 {
		t.Fatalf("应读到 10 字节, got %d err=%v", n, err)
	}
	if tc.down.Load() != 10 {
		t.Fatalf("下行计数应为 10, got %d", tc.down.Load())
	}
	if _, err := wrapped.Write([]byte("abcde")); err != nil {
		t.Fatal(err)
	}
	if tc.up.Load() != 5 {
		t.Fatalf("上行计数应为 5, got %d", tc.up.Load())
	}
	if tc.last.Load() == 0 {
		t.Fatal("最近活跃时间应被记录")
	}
}

func TestBuildNodePartsDedupsSameKey(t *testing.T) {
	// 同一节点链接出现两次(不同 # 名称) + 同 tag 的订阅出站出现两次:
	// 稳定端口下必须只产生一个 inbound, 否则同端口两个监听 → 实例必败。
	link := "ss://YWVzLTI1Ni1nY206cHdk@1.2.3.4:443#nodeA"
	entries := []any{
		link,
		"ss://YWVzLTI1Ni1nY206cHdk@1.2.3.4:443#nodeA-copy", // 同 key 不同名
		map[string]any{"tag": "sub-1-x", "type": "socks", "server": "1.2.3.4", "server_port": 1080},
		map[string]any{"tag": "sub-1-x", "type": "socks", "server": "1.2.3.4", "server_port": 1080},
	}
	ports, inbounds, _, _, _ := buildNodeParts(entries)
	if len(ports) != 2 {
		t.Fatalf("去重后应只有 2 个唯一节点, got %d: %v", len(ports), ports)
	}
	seen := map[int]bool{}
	for _, ib := range inbounds {
		p := ib["listen_port"].(int)
		if seen[p] {
			t.Fatalf("存在重复监听端口 %d — 实例会 Start 失败", p)
		}
		seen[p] = true
	}
}

func TestPurgeStablePortsRemovesFailedRecords(t *testing.T) {
	k1, k2 := "socks5://127.0.0.1:9501#p1", "socks5://127.0.0.1:9502#p2"
	nodeStableMu.Lock()
	nodeStablePorts = map[string]int{k1: 17001, k2: 17002}
	nodeStableMu.Unlock()

	// 模拟 k1 所在实例 Start 失败: 清除该批端口记录
	purgeStablePorts(map[string]int{k1: 17001})

	nodeStableMu.Lock()
	_, k1gone := nodeStablePorts[k1]
	_, k2kept := nodeStablePorts[k2]
	nodeStableMu.Unlock()
	if k1gone {
		t.Fatal("失败批次的端口记录应被清除")
	}
	if !k2kept {
		t.Fatal("未受影响的记录应保留")
	}
	nodeStableMu.Lock()
	delete(nodeStablePorts, k2)
	nodeStableMu.Unlock()
}

// Start 失败的端口清除必须是**最小范围**: bind 错误点名哪个端口清哪个,
// 清整批会让几百个健康节点的端口无谓漂移(2026-09-22 审查 P2)。
// 错误串解析不出端口时才退回整批清除(保留原自愈语义)。
func TestPurgeStablePortsFromError(t *testing.T) {
	k1, k2, k3 := "socks5://127.0.0.1:9601#e1", "socks5://127.0.0.1:9602#e2", "socks5://127.0.0.1:9603#e3"
	all := map[string]int{k1: 17101, k2: 17102, k3: 17103}
	reset := func() {
		nodeStableMu.Lock()
		// loaded 必须置真: purgeStablePorts 内部的惰性加载一旦触发, 会用磁盘
		// 文件覆盖这里注入的 map, 断言就会看到空表(实际踩过)。
		nodeStablePorts = map[string]int{k1: 17101, k2: 17102, k3: 17103}
		nodeStableLoaded = true
		nodeStableMu.Unlock()
	}
	reset()
	t.Cleanup(func() {
		nodeStableMu.Lock()
		nodeStablePorts = map[string]int{}
		nodeStableLoaded = false
		nodeStableMu.Unlock()
	})

	// 1) 错误点名 17102: 只清 k2, k1/k3 保留
	n := purgeStablePortsFromError(
		fmt.Errorf("listen tcp 127.0.0.1:17102: bind: Only one usage of each socket address"), all)
	if n != 1 {
		t.Fatalf("应只清 1 个, got %d", n)
	}
	nodeStableMu.Lock()
	_, k1in := nodeStablePorts[k1]
	_, k2in := nodeStablePorts[k2]
	_, k3in := nodeStablePorts[k3]
	nodeStableMu.Unlock()
	if !k1in || k2in || !k3in {
		t.Fatalf("应只清 k2(k1=%v k2=%v k3=%v): %v", k1in, k2in, k3in, nodeStablePorts)
	}

	// 2) 错误串无监听地址: 退回整批清除
	reset()
	if n := purgeStablePortsFromError(fmt.Errorf("some non-bind failure"), all); n != 3 {
		t.Fatalf("解析不出端口应整批清除(3), got %d", n)
	}
	nodeStableMu.Lock()
	remaining := len(nodeStablePorts)
	nodeStableMu.Unlock()
	if remaining != 0 {
		t.Fatalf("整批清除后应无记录, got %d", remaining)
	}
}
