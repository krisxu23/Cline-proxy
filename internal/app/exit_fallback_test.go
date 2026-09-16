package app

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// 2026-09-16 线上症状: 出口池近乎全挂(48/4482 可达)时
//   1) 面板"一直拉取不了上游免费模型" —— zen 目录同步经出口全失败, 没有真直连兜底;
//   2) muse-spark 从免费模型列表消失 —— 线路失败被记成"模型连续硬失败 30 分钟暂停"。
// 本组用例固化修复后的行为。

// ---- 直连客户端必须真的直连 ----

func TestDirectHTTPClientHasNoProxyAndIgnoresEnv(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte("direct-ok"))
	}))
	defer srv.Close()

	// 即使环境里挂了(死的)代理, 直连客户端也必须照常可达。
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")

	cl := directHTTPClient()
	if tr, ok := cl.Transport.(*http.Transport); ok {
		if tr.Proxy != nil {
			t.Fatal("直连客户端的 Transport 不得配置 Proxy —— 否则所谓'直连兜底'仍会走出口")
		}
	}
	resp, err := cl.Get(srv.URL)
	if err != nil {
		t.Fatalf("直连请求失败: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "direct-ok" {
		t.Fatalf("响应异常: %q", body)
	}
}

func TestCatalogFallbackClientFollowsRescueDirectSwitch(t *testing.T) {
	off, on := false, true

	withTestConfig(t, &zenConfigData{RescueDirect: &off})
	if cl := catalogFallbackClient(); cl != nil {
		t.Fatal("显式关闭直连兜底时, 目录兜底客户端必须为 nil(调用方跳过), 而不是拿出口客户端充数")
	}

	withTestConfig(t, &zenConfigData{RescueDirect: &on})
	if cl := catalogFallbackClient(); cl == nil {
		t.Fatal("开启直连兜底时应返回客户端")
	}
}

// ---- 拨号层: 候选节点全挂后必须继续换节点/兜底, 而不是直接判死 ----

func TestZenDialSkipsDeadExitAndFallsBackToDirect(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("起本地监听失败: %v", err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		_, _ = c.Write([]byte("ok"))
		_ = c.Close()
	}()

	on := true
	withTestConfig(t, &zenConfigData{
		ExitMode:      exitModeProxy, // 代理模式: 唯一出口是下面那个必然连不上的 socks5
		ProxyStrategy: "fill",
		Proxies:       []string{"socks5://127.0.0.1:1"},
		RescueDirect:  &on,
	})
	subMu.Lock()
	prevSub := subNodes
	subNodes = nil
	subMu.Unlock()
	t.Cleanup(func() {
		subMu.Lock()
		subNodes = prevSub
		subMu.Unlock()
	})
	clearZenProxyCooldowns()
	t.Cleanup(clearZenProxyCooldowns)

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	conn, err := zenDialContext(ctx, "tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("出口节点不可用时必须继续尝试并最终兜底直连, got %v", err)
	}
	defer conn.Close()

	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 2)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("兜底连接不可用: %v", err)
	}
	if string(buf) != "ok" {
		t.Fatalf("兜底连接数据异常: %q", buf)
	}
}

func TestZenDialRespectsDisabledRescueDirect(t *testing.T) {
	off := false
	withTestConfig(t, &zenConfigData{
		ExitMode:      exitModeProxy,
		ProxyStrategy: "fill",
		Proxies:       []string{"socks5://127.0.0.1:1"},
		RescueDirect:  &off, // 用户显式要求: 不许直连
	})
	subMu.Lock()
	prevSub := subNodes
	subNodes = nil
	subMu.Unlock()
	t.Cleanup(func() {
		subMu.Lock()
		subNodes = prevSub
		subMu.Unlock()
	})
	clearZenProxyCooldowns()
	t.Cleanup(clearZenProxyCooldowns)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := zenDialContext(ctx, "tcp", "127.0.0.1:9"); err == nil {
		t.Fatal("显式关闭直连兜底后, 出口全挂必须报错而不是偷偷直连")
	}
}

// ---- 模型健康门: 只有上游明确表达的问题才暂停模型 ----

func TestTimeoutDoesNotPauseModelButServerErrorDoes(t *testing.T) {
	initZenModels() // 模型目录是 resolveZenModel 的前置(生产路径由目录函数触发)
	resetZenModelHealth()
	t.Cleanup(resetZenModelHealth)
	t.Cleanup(resetCandidateState)

	const model = "mimo-v2.5-free"
	cand := routeCandidate{Upstream: "zen", Model: model}

	for i := 0; i < zenModelFailThreshold; i++ {
		applyCandidateFailure(cand, classTimeout, "line timeout", nil)
	}
	if zenModelUnavailable(model) {
		t.Fatal("超时属线路问题, 不得暂停模型 —— 否则出口池抖动会让健康模型从列表消失")
	}

	for i := 0; i < zenModelFailThreshold; i++ {
		applyCandidateFailure(cand, classServerError, "upstream 500", nil)
	}
	if !zenModelUnavailable(model) {
		t.Fatal("连续 5xx 说明上游模型本身有问题, 应暂停")
	}
}

func TestTimeoutFailureKeepsModelInFreeCatalog(t *testing.T) {
	initZenModels()
	resetZenModelHealth()
	t.Cleanup(resetZenModelHealth)
	t.Cleanup(resetCandidateState)

	const model = "mimo-v2.5-free"
	if !isZenFreeModel(mustZenModel(t, model)) {
		t.Skip("该模型不在当前 zen 免费目录, 跳过")
	}
	for i := 0; i < zenModelFailThreshold*2; i++ {
		applyCandidateFailure(routeCandidate{Upstream: "zen", Model: model}, classTimeout, "line timeout", nil)
	}
	found := false
	for _, m := range zenFreeCatalog() {
		if m.ID == model {
			found = true
		}
	}
	if !found {
		t.Fatal("只有线路失败时, 模型必须仍然出现在免费模型列表里")
	}
}

func mustZenModel(t *testing.T, id string) *ZenModel {
	t.Helper()
	m, ok := resolveZenModel(id)
	if !ok || m == nil {
		t.Skipf("模型 %s 不在目录中, 跳过", id)
	}
	return m
}
