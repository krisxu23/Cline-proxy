package app

import (
	"testing"
)

// 回归测试: 出站里"tls 存在但 enabled 不是 true"会让 sing-box 的 vless/trojan
// 出站构造出 config==nil 的 TLS dialer, 第一条连接就空指针 panic 打死整个进程。
// 这类配置来自订阅方生成的 sing-box JSON(常省略 enabled), 必须在入口处修正。
func TestSanitizeOutboundTLS(t *testing.T) {
	cases := []struct {
		name    string
		in      map[string]any
		wantTLS bool // 处理后 tls 键是否还存在
		wantEna any  // 若存在, enabled 的期望值
	}{
		{
			name:    "没有 tls 块: 不动",
			in:      map[string]any{"type": "vless"},
			wantTLS: false,
		},
		{
			name:    "tls 为 nil: 不动且不 panic",
			in:      map[string]any{"type": "vless", "tls": nil},
			wantTLS: false,
		},
		{
			name:    "显式 enabled=true: 保留",
			in:      map[string]any{"type": "vless", "tls": map[string]any{"enabled": true, "server_name": "a.com"}},
			wantTLS: true, wantEna: true,
		},
		{
			// 真实故障形态: 订阅只给了 server_name + utls, 没有 enabled
			name: "缺 enabled 但带 TLS 特征: 补 true",
			in: map[string]any{"type": "vless", "tls": map[string]any{
				"server_name": "cf-uh-sg01-2.23523463.xyz",
				"utls":        map[string]any{"enabled": true, "fingerprint": "chrome"},
			}},
			wantTLS: true, wantEna: true,
		},
		{
			name:    "缺 enabled 且没有任何字段: 仍补 true(不能留 nil dialer)",
			in:      map[string]any{"type": "vless", "tls": map[string]any{}},
			wantTLS: true, wantEna: true,
		},
		{
			name:    "显式 enabled=false: 整块删除(等价语义)",
			in:      map[string]any{"type": "vless", "tls": map[string]any{"enabled": false, "server_name": "a.com"}},
			wantTLS: false,
		},
		{
			name:    "tls 不是对象: 删除",
			in:      map[string]any{"type": "vless", "tls": "true"},
			wantTLS: false,
		},
		{
			name:    "enabled 类型不对(字符串): 按缺省处理, 补 true",
			in:      map[string]any{"type": "vless", "tls": map[string]any{"enabled": "true"}},
			wantTLS: true, wantEna: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sanitizeOutboundTLS(c.in)
			raw, exists := c.in["tls"]
			if exists != c.wantTLS {
				t.Fatalf("tls 存在性 = %v, 期望 %v (值 %#v)", exists, c.wantTLS, raw)
			}
			if !c.wantTLS {
				return
			}
			block, ok := raw.(map[string]any)
			if !ok {
				t.Fatalf("tls 不是对象: %#v", raw)
			}
			if block["enabled"] != c.wantEna {
				t.Fatalf("enabled = %#v, 期望 %#v", block["enabled"], c.wantEna)
			}
		})
	}
}

// 端到端: 订阅透传的坏出站经 buildNodeParts 后必须已经被修正,
// 否则 sing-box 会在第一条连接上崩溃。
func TestBuildNodePartsSanitizesPassedThroughOutbound(t *testing.T) {
	entry := map[string]any{
		"type": "vless", "tag": "sub-x",
		"server": "1.2.3.4", "server_port": 443, "uuid": "00000000-0000-0000-0000-000000000000",
		// 故意只给 tls 特征字段, 不给 enabled —— 就是打死进程的那种写法
		"tls": map[string]any{
			"server_name": "sni.example.com",
			"utls":        map[string]any{"enabled": true, "fingerprint": "chrome"},
		},
	}
	_, _, outbounds, _, _ := buildNodeParts([]any{entry})
	var found bool
	for _, ob := range outbounds {
		if ob["type"] != "vless" {
			continue
		}
		found = true
		blk, ok := ob["tls"].(map[string]any)
		if !ok {
			t.Fatal("vless 出站的 tls 块丢失")
		}
		if blk["enabled"] != true {
			t.Fatalf("tls.enabled = %#v, 期望 true —— 否则 sing-box 空指针崩溃", blk["enabled"])
		}
	}
	if !found {
		t.Fatal("没有生成 vless 出站, 测试无效")
	}
}

// 目录轮换预算必须始终可终止: 上限存在、下界为 1。
func TestCatalogExitBudgetBounded(t *testing.T) {
	got := catalogExitBudget()
	if got < 1 {
		t.Fatalf("预算必须 >= 1, 得到 %d", got)
	}
	if got > catalogExitMaxRotations {
		t.Fatalf("预算 %d 超过硬上限 %d", got, catalogExitMaxRotations)
	}
}
