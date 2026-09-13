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
	if err := validateOutboundEntry(ob); err != nil {
		t.Fatalf("补块后仍被判无效: %v", err)
	}
}
