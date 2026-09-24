package app

import (
	"encoding/base64"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

// 本文件锁定 2026-09-13 一次线上事故的三条根因(证据见 cline-proxy.log 15:22:04):
//
//	全量构建失败(initialize outbound[1142]: unknown method: chacha20-poly1305)
//	启动失败: initialize outbound[296]: unknown method: chacha20-poly1305
//
//  1. ss 的 chacha20-poly1305 是 v2ray 的写法, sing-box 只认 chacha20-ietf-poly1305,
//     不归一化时该节点一路进到 box.New, 把整个实例打死。
//  2. buildNodeParts 原来只校验 map 分支(订阅原始 JSON), 字符串链接解析出的出站
//     从不做 sing-box 校验, 于是单个坏链接能拖垮全部出口。
//  3. 后果: nodePorts 归零 → checkAllNodeHealth 整轮跳过 → 面板上所有订阅节点
//     永久停在"未检测"。
//
// 这些用例只做 box.New 构建校验, 不建连, 不依赖真实 sing-box 实例。

func ssLink(method, password string) string {
	cred := base64.StdEncoding.EncodeToString([]byte(method + ":" + password))
	return "ss://" + cred + "@1.2.3.4:8388#ss"
}

func TestNormalizeSSMethod(t *testing.T) {
	cases := map[string]string{
		"chacha20-poly1305":      "chacha20-ietf-poly1305",
		"chacha20poly1305":       "chacha20-ietf-poly1305",
		"chacha20_poly1305":      "chacha20-ietf-poly1305",
		"Chacha20-Poly1305":      "chacha20-ietf-poly1305",
		"CHACHA20-POLY1305":      "chacha20-ietf-poly1305",
		"chacha20-ietf-poly1305": "chacha20-ietf-poly1305",
		// 报告 §3 点名的大写写法: 订阅聚合源常见 AES-128-CFB, 不归一会原样透传给 sing-box
		"AES-128-CFB": "aes-128-cfb",
		"Aes-256-Gcm": "aes-256-gcm",
		"aes-192-ctr": "aes-192-ctr",
		// 订阅里方法字段常带前后空格
		" chacha20-poly1305 ": "chacha20-ietf-poly1305",
		// 表外一律原样: 不做猜测性映射, 交给逐节点校验决定剔除
		"aes-128-gcm": "aes-128-gcm",
		"rc4-md5":     "rc4-md5",
		"tabless":     "tabless",
		"AES-128-XXX": "AES-128-XXX", // 表外即使大写也不改写
		"":            "",
		"   ":         "   ", // 纯空格不改写(不改原串, 避免把空白当成有效方法)
	}
	for in, want := range cases {
		if got := normalizeSSMethod(in); got != want {
			t.Errorf("normalizeSSMethod(%q) = %q, 期望 %q", in, got, want)
		}
	}
}

// 复现线上失败: 未归一化的 chacha20-poly1305 会被 sing-box 直接拒绝。
// 这个用例反过来证明 normalizeSSMethod 是必需的, 不是多余的一层。
func TestSandboxRejectsRawChacha20Poly1305(t *testing.T) {
	ob := map[string]any{
		"type": "shadowsocks", "server": "1.2.3.4", "server_port": 8388,
		"method": "chacha20-poly1305", "password": "p0000",
	}
	dnsCfg, resolverTag := buildNodeDNS(getZenConfig())
	if err := validateOutboundEntry(ob, dnsCfg, resolverTag); err == nil {
		t.Fatal("chacha20-poly1305 未被 sing-box 拒绝 —— 与线上日志矛盾, 测试前提失效")
	}
}

func TestSSBadMethodNormalizedAndValid(t *testing.T) {
	ob, err := nodeOutbound(ssLink("chacha20-poly1305", "p0000"), "out-0")
	if err != nil {
		t.Fatalf("nodeOutbound: %v", err)
	}
	if got := ob["method"].(string); got != "chacha20-ietf-poly1305" {
		t.Fatalf("method = %q, 期望归一化为 chacha20-ietf-poly1305", got)
	}
	dnsCfg, resolverTag := buildNodeDNS(getZenConfig())
	if err := validateOutboundEntry(ob, dnsCfg, resolverTag); err != nil {
		t.Fatalf("归一化后仍被判无效: %v", err)
	}
}

// 好节点必须留下, 坏节点必须被剔除 —— 不允许"一个坏链接让整个出口池归零"。
// tabless 是 SS-Panel 常用但 sing-box 不支持的方法, 线上正是这类节点打死的实例。
func TestBuildNodePartsDropsBadStringLink(t *testing.T) {
	good := ssLink("chacha20-poly1305", "p0000") // 经归一化后有效
	bad := ssLink("tabless", "p0000")            // sing-box 拒绝

	ports, _, outbounds, _, _ := buildNodeParts([]any{good, bad})
	if len(ports) != 1 {
		t.Fatalf("ports = %v, 期望好节点留下、坏节点剔除(共 1 个)", ports)
	}
	if len(outbounds) != 1 {
		t.Fatalf("outbounds = %d, 期望 1", len(outbounds))
	}
	if got := outbounds[0]["method"].(string); got != "chacha20-ietf-poly1305" {
		t.Fatalf("留下的节点 method = %v, 期望 chacha20-ietf-poly1305", got)
	}
}

// --- 解析健壮性(2026-09-22 审查) ---

// 订阅里的无 @ 畸形行(vless://host:port?...)曾让 u.User.Username() nil panic
// 打挂整个进程 —— 订阅是不可信输入, 必须报错而不是崩溃。
func TestUserinfoNodeMissingUserNoPanic(t *testing.T) {
	for _, rest := range []string{
		"1.2.3.4:443?security=tls",
		"1.2.3.4:443?security=reality&pbk=pk",
		"1.2.3.4:443",
	} {
		for _, typ := range []string{"vless", "trojan"} {
			if _, err := parseUserinfoNode(rest, "out-0", typ); err == nil {
				t.Fatalf("%s:// 缺 @ 的畸形行必须报错: %q", typ, rest)
			}
		}
	}
	// 对照: 带 @ 的正常链接解析成功
	if _, err := parseUserinfoNode("b831381d-6324-4d53-ad4f-8cda48b30811@1.2.3.4:443?security=tls", "out-0", "vless"); err != nil {
		t.Fatalf("正常链接不应报错: %v", err)
	}
}

// http/h2 transport 的 host 缺失时不得注入 host:[""] —— 空串会被 sing-box
// 判为无效 HTTP transport 整条剔除, 而 type=http 不带 host 是常见写法。
func TestTransportHTTPHostOmittedWhenEmpty(t *testing.T) {
	tr, ok := transportBlock("http", url.Values{"path": {"/x"}})
	if !ok {
		t.Fatal("type=http 应产出 transport 块")
	}
	if _, has := tr["host"]; has {
		t.Fatalf("host 缺失不得注入, got %v", tr["host"])
	}
	tr2, _ := transportBlock("h2", url.Values{"host": {"h.example.com"}})
	got, _ := tr2["host"].([]string)
	if len(got) != 1 || got[0] != "h.example.com" {
		t.Fatalf("host 存在时应原样透传, got %v", tr2["host"])
	}
}

// --- 测速取消时机回归(2026-09-15 审计) ---
//
// 旧实现 probeNodeSpeed 在 client.Do 返回后立刻 cancel(), 然后才读响应体。
// Go 的语义: 请求 ctx 一旦取消, 后续 Body.Read 直接返回 context canceled。
// 于是下载永远 0 字节 → 两个端点都"失败" → 返回 (0, true) → 每个活节点
// 都被误判"断流"而记为不健康 —— 正是"网关跑起来了, 检测节点跑不起来/
// 全部失败"的直接根因。
//
// 本用例不依赖真实节点: 起一个本地流式 HTTP 服务, 用裸 client 直接调
// probeNodeSpeed。旧代码下必然断言失败(0 字节、stalled), 新代码下必须
// 测到真实吞吐。
func TestProbeNodeSpeedReadsBodyBeforeCancel(t *testing.T) {
	const total = 2 << 20 // 2 MiB, 足够大到 Do 返回时体肯定没读完
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		chunk := make([]byte, 64<<10)
		for sent := 0; sent < total; sent += len(chunk) {
			// sleep 放在**每块写入之前**(含首块): 若实现回归为"Do 返回即
			// cancel", 首块尚无任何字节进入客户端缓冲, cancel 后必测出 0
			// 吞吐, 用例立即变红。旧写法(sleep 在写后)首块可能已入缓冲,
			// 对旧 bug 复现偏弱(R2 审计 F7)。
			time.Sleep(5 * time.Millisecond) // 拉开传输时间, 保证 cancel 抢在体完成前
			if _, err := w.Write(chunk); err != nil {
				return
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}))
	defer srv.Close()

	prev := nodeSpeedTestURLs
	nodeSpeedTestURLs = []string{srv.URL + "/__down"}
	t.Cleanup(func() { nodeSpeedTestURLs = prev })

	client := &http.Client{Timeout: 30 * time.Second}
	speed, stalled, failed := probeNodeSpeed(client)
	if stalled {
		t.Fatalf("健康链路被判为断流(stalled): speed=%d —— 取消时机回归失败", speed)
	}
	if failed {
		t.Fatalf("有真实吞吐就不该标测速失败: speed=%d", speed)
	}
	if speed <= 0 {
		t.Fatalf("吞吐应为正数, got %d(stalled=%v)", speed, stalled)
	}
}

// 测速端点全挂(两个 URL 同为 speed.cloudflare.com, 同生同死)必须报
// "测速失败"而不是"断流" —— 2026-09-22 审查 P1: 旧实现 return (0, true)
// 无条件 IsStalled, 一次 CF 侧事故就把全部活节点踢出选路池。
func TestProbeNodeSpeedAllEndpointsFailNotStalled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	prev := nodeSpeedTestURLs
	nodeSpeedTestURLs = []string{srv.URL + "/down1", srv.URL + "/down2"}
	t.Cleanup(func() { nodeSpeedTestURLs = prev })

	speed, stalled, failed := probeNodeSpeed(&http.Client{Timeout: 10 * time.Second})
	if speed != 0 || stalled {
		t.Fatalf("端点全挂应得 (0,false,failed=true), 得到 speed=%d stalled=%v", speed, stalled)
	}
	if !failed {
		t.Fatal("端点全挂必须报测速失败, 否则 0 字节会被误读成断流")
	}
}

// --- 活性三源并发 + 地区多数投票(2026-09-16 用户需求) ---
//
// 活性: 三权威源并发, 任一成功即活。本用例起三个本地源 —— 快源立即 204,
// 慢源延迟后 200, 死源恒 500 —— 必须判活, 且延迟应接近快源而非等慢源。
func TestProbeNodeLivenessConcurrent(t *testing.T) {
	fast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer fast.Close()
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(800 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer slow.Close()
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer dead.Close()

	prev := nodeLivenessURLs
	nodeLivenessURLs = []string{dead.URL, slow.URL, fast.URL}
	t.Cleanup(func() { nodeLivenessURLs = prev })

	client := &http.Client{Timeout: 30 * time.Second}
	t0 := time.Now()
	lat, alive := probeNodeLiveness(client)
	if !alive {
		t.Fatal("两源可用却判死 —— 并发活性失败")
	}
	if elapsed := time.Since(t0); elapsed > 700*time.Millisecond {
		t.Fatalf("并发退化为串行等待: 耗时 %v, 快源本应立即成功", elapsed)
	}
	if lat < 0 {
		t.Fatalf("延迟为负: %d", lat)
	}
}

// 活性全死: 三源全 500 必须判死。用"立即拒连"的本地端口而非超时桩,
// 判死应毫秒级返回, 不必等满超时。
func TestProbeNodeLivenessAllDead(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	deadAddr := ln.Addr().String()
	ln.Close() // 关闭即拒连: dial 立刻 ECONNREFUSED

	prev := nodeLivenessURLs
	nodeLivenessURLs = []string{
		"http://" + deadAddr,
		"http://" + deadAddr,
		"http://" + deadAddr,
	}
	t.Cleanup(func() { nodeLivenessURLs = prev })

	t0 := time.Now()
	if _, alive := probeNodeLiveness(&http.Client{Timeout: 30 * time.Second}); alive {
		t.Fatal("三源全死却判活")
	}
	if elapsed := time.Since(t0); elapsed > 5*time.Second {
		t.Fatalf("全死源判死过慢: %v(应毫秒级拒连返回)", elapsed)
	}
}

// ★ 首个成功源即采用(2026-09-24 计划 Task 6, **取代**原"三源多数投票")。
//
// 原实现要等**全部**源返回(或各自超时)才能定国家, 慢源会把整轮探测拖到最慢者
// 的超时; 而国家现在只是"提示" —— 地区门控的权威判据是每模型实测的 regionNodeOK
// (见 model_region.go 的 regionExitTier)。
//
// 这里让 "US 快 / CA 慢 600ms", 断言采用先到的 US 且**不等**慢源。
// 旧实现会等满 600ms, 且两票 1:1 判平后把地区留空 —— 两种表现都能被本用例抓到。
func TestProbeNodeExitInfoFirstSuccessWins(t *testing.T) {
	fast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ip":"1.1.1.1","country_code":"US"}`))
	}))
	defer fast.Close()
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(600 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ip":"1.1.1.1","country_code":"CA"}`))
	}))
	defer slow.Close()

	prev := nodeIPEchoURLs
	nodeIPEchoURLs = []string{fast.URL, slow.URL}
	t.Cleanup(func() { nodeIPEchoURLs = prev })

	start := time.Now()
	var result nodeTestResult
	probeNodeExitInfo(&http.Client{Timeout: 30 * time.Second}, &result)
	elapsed := time.Since(start)

	if result.ExitCountry != "US" {
		t.Fatalf("应采用先返回的源(US), got %q", result.ExitCountry)
	}
	if result.ExitIP != "1.1.1.1" {
		t.Fatalf("出口 IP 应为 1.1.1.1, got %q", result.ExitIP)
	}
	if elapsed > 300*time.Millisecond {
		t.Fatalf("不应等慢源: 耗时 %v(应毫秒级返回)", elapsed)
	}
}

// 多个源都返回有效数据时, 必须采用其中一个, **不再**因为"票数各异"把地区留空。
//
// 旧契约(三源投票): US/JP/DE 各一票 → 判平 → 地区留空归 other。
// 新契约(Task 6): 首个成功源即采用 —— 留空会让地区过滤把该节点归 other,
// 反而放大 Task 9 要修的"未知国家被放行"问题。
func TestProbeNodeExitInfoNoVoteKeepsFirstCountry(t *testing.T) {
	mk := func(cc string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"ip":"9.9.9.9","country_code":"` + cc + `"}`))
		}))
	}
	s1, s2, s3 := mk("US"), mk("JP"), mk("DE")
	defer s1.Close()
	defer s2.Close()
	defer s3.Close()

	prev := nodeIPEchoURLs
	nodeIPEchoURLs = []string{s1.URL, s2.URL, s3.URL}
	t.Cleanup(func() { nodeIPEchoURLs = prev })

	var result nodeTestResult
	probeNodeExitInfo(&http.Client{Timeout: 30 * time.Second}, &result)
	switch result.ExitCountry {
	case "US", "JP", "DE":
		// 合法: 采用先返回的那个源
	default:
		t.Fatalf("应采用某个有效源的国家码, got %q", result.ExitCountry)
	}
	if result.ExitIP != "9.9.9.9" {
		t.Fatalf("出口 IP 应保留, got %q", result.ExitIP)
	}
}
