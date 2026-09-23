//go:build with_utls

package app

import "testing"

// anytls 补块的结果必须能通过 sing-box 的合法性校验 —— 补了块但校验过不了,
// 节点照样被剔除, 等于没补。
//
// 这条必须走 sing-box, 所以只能带 -tags with_utls 跑: anytls 依赖 uTLS,
// 裸 go test 下 sing-box 会在构造出站时直接报
// "uTLS is not included in this build, rebuild with -tags with_utls"。
//
// 拆分原因: 这份用例原先和纯 sanitize 断言写在同一个文件里, 只要忘带 tag
// 就是 CI 红灯 —— 而红灯掩盖了"TLS sanitize 的产物本身是对的"这个真实结论。
// 现在纯断言留在 nodes_tls_sanitize_test.go(裸 go test 可跑), 这条单独门禁。
func TestAnytlsMissingTLSPassesSingBoxValidation(t *testing.T) {
	ob := map[string]any{"type": "anytls", "server": "a.com", "server_port": 443, "password": "x"}
	sanitizeOutboundTLS(ob)
	dnsCfg, resolverTag := buildNodeDNS(getZenConfig())
	if err := validateOutboundEntry(ob, dnsCfg, resolverTag); err != nil {
		t.Fatalf("补块后仍被判无效: %v", err)
	}
}

// reality 缺 utls 的出站经净化补 chrome 后必须通过 sing-box 校验 ——
// "补了但校验过不了"等于没补, 节点照样进不了检测池。
// 走 box.New 的配置检查(reality_client.go), 依赖 with_utls 构建标签。
func TestRealitySanitizedPassesSingBoxValidation(t *testing.T) {
	for name, utlsBlock := range map[string]any{
		"缺 utls":    nil,
		"unsafe 指纹": map[string]any{"enabled": true, "fingerprint": "unsafe"},
	} {
		ob := map[string]any{
			"type": "vless", "server": "1.2.3.4", "server_port": 443,
			"uuid": "b831381d-6324-4d53-ad4f-8cda48b30811",
			"tls": map[string]any{
				"enabled": true, "server_name": "x.com",
				// 32 字节 X25519 公钥的 base64url: 随便写的短串会被 sing-box 以
				// "invalid public_key" 拒掉, 那样测不到 utls 指纹分支。
				"reality": map[string]any{"enabled": true, "public_key": "SbVKOEMjK0sIlbwg4akyBg5mL5KZwwB-ed4eEE7YnRc", "short_id": "0123456789ab"},
			},
		}
		if utlsBlock != nil {
			ob["tls"].(map[string]any)["utls"] = utlsBlock
		}
		sanitizeOutboundTLS(ob)
		sanitizeOutboundShape(ob)
		dnsCfg, resolverTag := buildNodeDNS(getZenConfig())
		if err := validateOutboundEntry(ob, dnsCfg, resolverTag); err != nil {
			t.Fatalf("%s: 净化后仍被 sing-box 拒绝: %v", name, err)
		}
		if u := ob["tls"].(map[string]any)["utls"].(map[string]any); u["fingerprint"] != "chrome" {
			t.Fatalf("%s: 应补 chrome 指纹, got %v", name, u)
		}
	}
}
