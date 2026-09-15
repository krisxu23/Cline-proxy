package app

// 出口选路增强第二组的测试: 稳定端口 / 每节点流量计数。

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
	// 第二次分配必须复用同一端口(这是"订阅更新端口不漂移"的核心保证)
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
	if p1 <= 0 || p1 > 65535 {
		t.Fatalf("端口应在合法范围, got %d", p1)
	}
	// 清理: 从稳定表移除, 避免污染其它测试
	nodeStableMu.Lock()
	delete(nodeStablePorts, key)
	nodeStableMu.Unlock()
}

func TestAssignStablePortReallocatesWhenOccupied(t *testing.T) {
	key := "socks5://127.0.0.1:9402#occupied"
	// 先占住一个端口
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	occupied := l.Addr().(*net.TCPAddr).Port
	nodeStableMu.Lock()
	nodeStablePorts[key] = occupied
	nodeStableMu.Unlock()

	// 历史端口被占: 必须重新分配一个可用端口
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
	// 用一对 net.Pipe 模拟连接(注意: net.Pipe 是同步的, 对端必须消费数据,
	// 否则 Write 会永久阻塞 —— 本测试曾因此挂死)
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	tc := trafficCounterFor("count-test")
	defer nodeTrafficReset()
	wrapped := &countingConn{Conn: client, up: &tc.up, down: &tc.down, last: &tc.last}

	// 对端 goroutine: 写 10 字节(触发下行计数), 再把上行写来的 5 字节读走
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
