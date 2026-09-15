package app

// 稳定端口 / 每节点流量 / 构建去重 的测试。

import (
	"net"
	"testing"
)

func TestAssignStablePortReusesAcrossCalls(t *testing.T) {
	key := "socks5://127.0.0.1:9401#stable"
	p1, reused1, err := assignStablePort(key)
	if err != nil {
		t.Fatal(err)
	}
	_ = reused1
	p2, reused2, err := assignStablePort(key)
	if err != nil {
		t.Fatal(err)
	}
	if p1 != p2 {
		t.Fatalf("同 key 两次分配应得到同一端口: %d vs %d", p1, p2)
	}
	if !reused2 {
		t.Fatalf("第二次分配应报告复用, got reused=%v", reused2)
	}
	nodeStableMu.Lock()
	delete(nodeStablePorts, key)
	nodeStableMu.Unlock()
}

func TestAssignStablePortReallocatesWhenOccupied(t *testing.T) {
	key := "socks5://127.0.0.1:9402#occupied"
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	occupied := l.Addr().(*net.TCPAddr).Port
	nodeStableMu.Lock()
	nodeStablePorts[key] = occupied
	nodeStableMu.Unlock()

	p, reused, err := assignStablePort(key)
	if err != nil {
		t.Fatal(err)
	}
	if reused {
		t.Fatal("被占端口不应报告复用")
	}
	if p == occupied {
		t.Fatal("新端口不应与被占端口相同")
	}
	nodeStableMu.Lock()
	delete(nodeStablePorts, key)
	nodeStableMu.Unlock()
}

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
