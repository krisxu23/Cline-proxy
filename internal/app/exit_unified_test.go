package app

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// resetNodeBoxForTest 清掉节点实例, 避免用例之间互相影响。
func resetNodeBoxForTest(t *testing.T) {
	t.Helper()
	nodeMu.Lock()
	if nodeBox != nil {
		nodeBox.Close()
	}
	nodeBox = nil
	nodePorts = nil
	nodePortsKeys = ""
	catchAllPort = 0
	nodeMu.Unlock()
	t.Cleanup(func() {
		nodeMu.Lock()
		if nodeBox != nil {
			nodeBox.Close()
		}
		nodeBox = nil
		nodePorts = nil
		nodePortsKeys = ""
		catchAllPort = 0
		nodeMu.Unlock()
	})
}

// 零节点也要建立实例: 否则"直连模式也经 sing-box"这条路径整个落空。
func TestZeroNodesStillBuildsCatchAllInbound(t *testing.T) {
	withTestConfig(t, &zenConfigData{ExitMode: exitModeDirect})
	resetNodeBoxForTest(t)
	syncNodeBox()

	if catchAllLocalAddr() == "" {
		t.Fatal("catch-all inbound must exist even with zero nodes")
	}
	nodeMu.Lock()
	boxReady := nodeBox != nil
	nodeMu.Unlock()
	if !boxReady {
		t.Fatal("sing-box instance must be built with zero nodes")
	}
}

// 直连模式下, 出站请求必须经 catch-all 入站(即经过 sing-box)而不是 Go 原生拨号。
func TestDirectModeDialsThroughCatchAll(t *testing.T) {
	withTestConfig(t, &zenConfigData{ExitMode: exitModeDirect})
	resetNodeBoxForTest(t)
	syncNodeBox()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("via-box"))
	}))
	defer srv.Close()
	target := strings.TrimPrefix(srv.URL, "http://")

	// 直接用拨号器验证: 拨通后按 HTTP 发一次请求
	conn, err := zenDialContext(context.Background(), "tcp", target)
	if err != nil {
		t.Fatalf("direct mode must still connect through sing-box: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("GET / HTTP/1.1\r\nHost: " + target + "\r\nConnection: close\r\n\r\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	body, _ := io.ReadAll(conn)
	if !strings.Contains(string(body), "via-box") {
		t.Fatalf("unexpected response through catch-all: %q", truncateForTest(string(body), 120))
	}
}

// 代理模式 + 无可用节点 + 兜底开启: 仍然经 catch-all 出网(由 sing-box 的 direct 出站), 不应报错。
func TestProxyModeWithNoNodeRescuesThroughCatchAll(t *testing.T) {
	rescue := true
	withTestConfig(t, &zenConfigData{ExitMode: exitModeProxy, RescueDirect: &rescue})
	resetNodeBoxForTest(t)
	syncNodeBox()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("rescued"))
	}))
	defer srv.Close()
	target := strings.TrimPrefix(srv.URL, "http://")

	conn, err := zenDialContext(context.Background(), "tcp", target)
	if err != nil {
		t.Fatalf("rescue path must connect: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("GET / HTTP/1.1\r\nHost: " + target + "\r\nConnection: close\r\n\r\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	body, _ := io.ReadAll(conn)
	if !strings.Contains(string(body), "rescued") {
		t.Fatalf("unexpected response: %q", truncateForTest(string(body), 120))
	}
}

// 代理模式 + 无可用节点 + 兜底关闭: 必须明确失败, 不能偷偷直连。
func TestProxyModeNoNodeWithoutRescueFails(t *testing.T) {
	rescue := false
	withTestConfig(t, &zenConfigData{ExitMode: exitModeProxy, RescueDirect: &rescue})
	resetNodeBoxForTest(t)
	syncNodeBox()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	target := strings.TrimPrefix(srv.URL, "http://")

	if _, err := zenDialContext(context.Background(), "tcp", target); err == nil {
		t.Fatal("with rescue disabled and no node, dialing must fail explicitly")
	}
}

func truncateForTest(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
