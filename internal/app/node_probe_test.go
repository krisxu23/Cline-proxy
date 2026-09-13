package app

import (
	"encoding/base64"
	"testing"
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
	if err := validateOutboundEntry(ob); err == nil {
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
	if err := validateOutboundEntry(ob); err != nil {
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
