package main

// 开管理窗口前的就绪等待(2026-09-24)。
//
// 用户实证: 双击启动后弹出来的管理页是浏览器错误页, 关掉过一会儿重开才正常。
// 根因是等固定 1.2 秒就开窗, 而 StartProxy 在开始监听**之前**要同步构建 sing-box
// 节点池, 实测从进程启动到 3457 开始监听要 16 秒(06:34:08 → 06:34:24)。
// 改成轮询 /health 直到真正可用。

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// 服务稍后才就绪时必须等到那一刻, 而不是探一次就返回。
func TestWaitAdminReadyWaitsForHealth(t *testing.T) {
	ready := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-ready:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"version":"1.2.3"}`))
		default:
			http.Error(w, "not ready", http.StatusServiceUnavailable)
		}
	}))
	defer srv.Close()

	go func() {
		time.Sleep(150 * time.Millisecond)
		close(ready)
	}()

	start := time.Now()
	if !waitAdminReady(srv.URL, 5*time.Second) {
		t.Fatal("服务就绪后应返回 true")
	}
	// 回归性质: 若实现退化成"探一次就返回"(或等固定时长后不看结果), 这里会挂。
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
		t.Fatalf("应在服务真正就绪之后才返回, 实际只等了 %v", elapsed)
	}
}

// 服务始终起不来 → 超时返回 false(调用方据此落日志, 但仍会开窗)。
func TestWaitAdminReadyTimesOut(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	if waitAdminReady(srv.URL, 600*time.Millisecond) {
		t.Fatal("服务不可用时应超时返回 false")
	}
}

// 端口可连但 /health 不是预期形态(半启动实例) → 不算就绪。
// 只判"端口可连"会在这里得到假阳性, 开出来的窗口照样是错误页 ——
// 这正是 isServiceAlive 校验 version 字段的原因。
func TestWaitAdminReadyRejectsNonHealthResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok")) // 200, 但没有 version 字段
	}))
	defer srv.Close()

	if waitAdminReady(srv.URL, 600*time.Millisecond) {
		t.Fatal("响应缺 version 字段不应判为就绪")
	}
}
