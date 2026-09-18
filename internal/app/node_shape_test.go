package app

import "testing"

// 出站"值方言"净化 —— 2026-09-18 实测: 订阅源普遍用 v2ray/Xray 写法, sing-box
// 不认就整条剔除。实测被剔除 72 条, 前四类语义都能表达, 不该丢节点。
//
// ★ 一条重要的实测发现: `validateOutboundEntry`(走 box.New)**并不校验**
// uTLS 指纹、vless flow、以及 transport=tcp —— 它只抓 unknown outbound type 与
// unknown transport type(xhttp)。所以用"校验通过"来断言这几类会得到**假阳性**
// (把净化关掉测试照样绿)。本文件因此直接断言**净化后的出站 map** —— 那才是真正
// 送进 sing-box 实例的东西。

func TestNormalizeUTLSFingerprint(t *testing.T) {
	valid := map[string]string{
		"chrome": "chrome", "Chrome": "chrome", "CHROME": "chrome",
		"firefox": "firefox", "safari": "safari", "ios": "ios",
		"android": "android", "edge": "edge", "360": "360", "qq": "qq",
		"random": "random", "randomized": "randomized",
		"  chrome  ": "chrome",
	}
	for in, want := range valid {
		if got := normalizeUTLSFingerprint(in); got != want {
			t.Errorf("normalizeUTLSFingerprint(%q) = %q, want %q", in, got, want)
		}
	}
	// 不认识的取值一律归空(调用方据此省略 utls 块)。
	// "unsafe" 是 v2ray 的"不校验指纹", sing-box 无对应值 —— 实测出现 22 次。
	invalid := []string{"unsafe", "UNSAFE", "golang", "zzz", "", "  ", "go"}
	for _, in := range invalid {
		if got := normalizeUTLSFingerprint(in); got != "" {
			t.Errorf("normalizeUTLSFingerprint(%q) = %q, want 空(省略)", in, got)
		}
	}
}

func TestNormalizeVLESSFlow(t *testing.T) {
	cases := map[string]string{
		"":                        "", // 未设 = none
		"none":                    "", // 显式 none 等价于不设(实测 12 次)
		"xtls-rprx-vision":        "xtls-rprx-vision",
		"xtls-rprx-vision-udp443": "xtls-rprx-vision", // 旧名(实测 2 次)
		"XTLS-RPRX-VISION":        "xtls-rprx-vision",
		"xtls-rprx-direct":        "", // 更老的写法, sing-box 不支持
		"xtls-rprx-origin":        "",
		"some-unknown-flow":       "",
	}
	for in, want := range cases {
		if got := normalizeVLESSFlow(in); got != want {
			t.Errorf("normalizeVLESSFlow(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsNoTransportMarker(t *testing.T) {
	// tcp/raw/空 = v2ray 对"裸 TCP"的表达, 省略 transport 字段即等价语义。
	for _, s := range []string{"", " ", "tcp", "TCP", "raw", " raw "} {
		if !isNoTransportMarker(s) {
			t.Errorf("%q 应被判为「无传输层」标记", s)
		}
	}
	// 真传输与未知传输都不是标记 —— 未知的留给 sing-box 剔除, 不静默降级。
	for _, s := range []string{"ws", "grpc", "http", "quic", "httpupgrade", "xhttp", "kcp"} {
		if isNoTransportMarker(s) {
			t.Errorf("%q 不应被判为「无传输层」标记", s)
		}
	}
}

func TestIsSingboxTransportType(t *testing.T) {
	yes := []string{"http", "ws", "quic", "grpc", "httpupgrade", "WS", "Grpc"}
	for _, s := range yes {
		if !isSingboxTransportType(s) {
			t.Errorf("%q 应被 sing-box 支持", s)
		}
	}
	no := []string{"tcp", "raw", "xhttp", "", " ", "kcp", "unknown"}
	for _, s := range no {
		if isSingboxTransportType(s) {
			t.Errorf("%q 不应被当作 sing-box 传输类型", s)
		}
	}
}

// sanitizeOutboundShape 的字段级行为。
func TestSanitizeOutboundShapeFields(t *testing.T) {
	t.Run("unsafe 指纹省略 utls 块但保留 tls", func(t *testing.T) {
		ob := map[string]any{
			"type": "vless",
			"tls": map[string]any{
				"enabled": true, "server_name": "a.com",
				"utls": map[string]any{"enabled": true, "fingerprint": "unsafe"},
			},
		}
		sanitizeOutboundShape(ob)
		tls := ob["tls"].(map[string]any)
		if _, ok := tls["utls"]; ok {
			t.Fatal("unsafe 指纹应整块省略 utls")
		}
		if tls["enabled"] != true {
			t.Fatal("tls 本身必须保留")
		}
	})

	t.Run("合法指纹保留并归一大小写", func(t *testing.T) {
		ob := map[string]any{
			"tls": map[string]any{"utls": map[string]any{"enabled": true, "fingerprint": "Chrome"}},
		}
		sanitizeOutboundShape(ob)
		u := ob["tls"].(map[string]any)["utls"].(map[string]any)
		if u["fingerprint"] != "chrome" {
			t.Fatalf("指纹应归一为 chrome, got %v", u["fingerprint"])
		}
	})

	t.Run("flow none 删字段 / vision 保留 / 旧名归一", func(t *testing.T) {
		ob := map[string]any{"flow": "none"}
		sanitizeOutboundShape(ob)
		if _, ok := ob["flow"]; ok {
			t.Fatal("flow=none 应删掉字段")
		}
		ob = map[string]any{"flow": "xtls-rprx-vision-udp443"}
		sanitizeOutboundShape(ob)
		if ob["flow"] != "xtls-rprx-vision" {
			t.Fatalf("旧名应归一, got %v", ob["flow"])
		}
		ob = map[string]any{"flow": "xtls-rprx-vision"}
		sanitizeOutboundShape(ob)
		if ob["flow"] != "xtls-rprx-vision" {
			t.Fatal("合法 flow 必须保留")
		}
	})

	t.Run("transport tcp 删字段 / ws 保留 / xhttp 原样留着", func(t *testing.T) {
		ob := map[string]any{"transport": map[string]any{"type": "tcp"}}
		sanitizeOutboundShape(ob)
		if _, ok := ob["transport"]; ok {
			t.Fatal("transport type=tcp 表示裸 TCP, 应删掉整个 transport")
		}
		ob = map[string]any{"transport": map[string]any{"type": "ws", "path": "/x"}}
		sanitizeOutboundShape(ob)
		tr, ok := ob["transport"].(map[string]any)
		if !ok || tr["type"] != "ws" {
			t.Fatal("合法的 ws transport 必须保留")
		}
		ob = map[string]any{"transport": map[string]any{"type": "xhttp"}}
		sanitizeOutboundShape(ob)
		if _, ok := ob["transport"]; !ok {
			t.Fatal("xhttp 不是「无传输层」标记, 不应被剥掉(留给 sing-box 剔除)")
		}
	})

	t.Run("幂等: 重复净化结果不变", func(t *testing.T) {
		ob := map[string]any{
			"flow":      "none",
			"transport": map[string]any{"type": "raw"},
			"tls":       map[string]any{"utls": map[string]any{"fingerprint": "unsafe"}},
		}
		sanitizeOutboundShape(ob)
		first := len(ob)
		sanitizeOutboundShape(ob)
		if len(ob) != first {
			t.Fatalf("重复净化应为幂等: %d -> %d", first, len(ob))
		}
	})

	t.Run("xtls 旧块被移除", func(t *testing.T) {
		ob := map[string]any{"xtls": map[string]any{"enabled": true}}
		sanitizeOutboundShape(ob)
		if _, ok := ob["xtls"]; ok {
			t.Fatal("xtls 是 v2ray 旧字段, sing-box 无此块, 应移除")
		}
	})
}

// ★ 核心回归: 净化后的出站里**不再残留 sing-box 不认的取值**。
// 四个用例各对应实测日志里的一类剔除原因。
func TestShapeSanitizeRescuesRejectedNodes(t *testing.T) {
	cases := []struct {
		name  string
		build func() map[string]any
		check func(t *testing.T, ob map[string]any)
	}{
		{
			name: "fp=unsafe 不再残留", // 实测 22 次: unknown uTLS fingerprint: unsafe
			build: func() map[string]any {
				return map[string]any{
					"type": "vless", "server": "1.2.3.4", "server_port": 443, "uuid": "u-1",
					"tls": map[string]any{
						"enabled": true, "server_name": "a.com",
						"utls": map[string]any{"enabled": true, "fingerprint": "unsafe"},
					},
				}
			},
			check: func(t *testing.T, ob map[string]any) {
				tls, ok := ob["tls"].(map[string]any)
				if !ok {
					t.Fatal("tls 块必须保留(只是不做指纹伪装)")
				}
				if _, ok := tls["utls"]; ok {
					t.Fatal("unsafe 指纹的 utls 块必须被省略, 否则 sing-box 整条剔除")
				}
			},
		},
		{
			name: "flow=none 不再残留", // 实测 12 次: unsupported flow: none
			build: func() map[string]any {
				return map[string]any{"type": "vless", "server": "1.2.3.4", "server_port": 443, "uuid": "u-2", "flow": "none"}
			},
			check: func(t *testing.T, ob map[string]any) {
				if v, ok := ob["flow"]; ok {
					t.Fatalf("flow=none 应被删掉, 仍有 %v", v)
				}
			},
		},
		{
			name: "flow 旧名归一到现值", // 实测 2 次: unsupported flow: xtls-rprx-vision-udp443
			build: func() map[string]any {
				return map[string]any{"type": "vless", "server": "1.2.3.4", "server_port": 443, "uuid": "u-3", "flow": "xtls-rprx-vision-udp443"}
			},
			check: func(t *testing.T, ob map[string]any) {
				if ob["flow"] != "xtls-rprx-vision" {
					t.Fatalf("旧名应归一为 xtls-rprx-vision, got %v", ob["flow"])
				}
			},
		},
		{
			name: "transport=tcp 不再残留", // 实测 9 次: unknown transport type: tcp / raw
			build: func() map[string]any {
				return map[string]any{
					"type": "vless", "server": "1.2.3.4", "server_port": 443, "uuid": "u-4",
					"transport": map[string]any{"type": "tcp"},
				}
			},
			check: func(t *testing.T, ob map[string]any) {
				if v, ok := ob["transport"]; ok {
					t.Fatalf("transport=tcp 意为裸 TCP, 应省略该字段, 仍有 %v", v)
				}
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ob := c.build()
			sanitizeOutboundShape(ob)
			c.check(t, ob)
			// 净化后仍必须是结构合法的出站(不能把节点改坏)
			if err := validateOutboundEntry(ob); err != nil {
				t.Fatalf("净化后出站结构必须仍合法: %v", err)
			}
		})
	}
}

// 反向保护: 真正无解的写法仍必须被 sing-box 剔除, 不能因为净化而放进来。
func TestShapeSanitizeKeepsGenuinelyInvalidRejected(t *testing.T) {
	withTestConfig(t, &zenConfigData{})
	// xhttp 是 Xray 传输, sing-box 没有对应实现, 且它**不是**「无传输层」标记。
	// 净化刻意不剥它 —— 剥掉会退化成裸 TCP, 节点"看起来合法却连不上", 比明确剔除更难排查。
	ob := map[string]any{
		"type": "vless", "server": "1.2.3.4", "server_port": 443,
		"uuid":      "11111111-1111-1111-1111-111111111111",
		"transport": map[string]any{"type": "xhttp"},
	}
	sanitizeOutboundShape(ob)
	if _, ok := ob["transport"]; !ok {
		t.Fatal("xhttp 不应被净化剥掉(它语义上不是「无传输层」标记)")
	}
	if err := validateOutboundEntry(ob); err == nil {
		t.Fatal("xhttp 无 sing-box 对应实现, 必须被继续剔除")
	}
}
