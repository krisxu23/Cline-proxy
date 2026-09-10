package app

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestParseSubPlainLines(t *testing.T) {
	body := strings.Join([]string{
		"trojan://tpass@1.2.3.4:443?security=tls&sni=t.example.com&type=ws&path=%2Ftr#node1",
		"socks5://u:p@5.6.7.8:1080/#node2",
		"https://not-a-node.example/ignored",
		"",
	}, "\n")
	nodes, err := parseSubContent(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 {
		t.Fatalf("http 行不应作为节点, 得到 %d 个", len(nodes))
	}
	link, _ := nodes[0].(string)
	ob, err := nodeOutbound(link, "t")
	if err != nil || ob["password"] != "tpass" {
		t.Fatalf("trojan 节点: %v %v", ob, err)
	}
}

func TestParseSubBase64(t *testing.T) {
	list := strings.Join([]string{
		"vless://b831381d-6324-4d53-ad4f-8cda48b30811@1.1.1.1:443?security=tls&type=ws&allowInsecure=1&alpn=h2,http%2F1.1&sni=s.example.com&fp=chrome#节点A",
		"vmess://" + base64.StdEncoding.EncodeToString([]byte(`{"v":"2","add":"2.2.2.2","port":"9527","id":"ae862e1d-3b8c-443a-027b-377c6c69ef4c","aid":"0","net":"tcp","scy":"chacha20-poly1305","ps":"tw"}`)),
	}, "\n")
	body := base64.StdEncoding.EncodeToString([]byte(list))
	nodes, err := parseSubContent(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 2 {
		t.Fatalf("base64 订阅应有 2 个节点, 得到 %d", len(nodes))
	}
	ob, err := nodeOutbound(nodes[0].(string), "t")
	if err != nil {
		t.Fatal(err)
	}
	tls, _ := ob["tls"].(map[string]any)
	if tls == nil || tls["insecure"] != true {
		t.Fatalf("allowInsecure 未生效: %v", tls)
	}
	if alpn, _ := tls["alpn"].([]string); len(alpn) != 2 || alpn[0] != "h2" {
		t.Fatalf("alpn 未生效: %v", tls["alpn"])
	}
	vm := nodes[1].(string)
	ob2, err := nodeOutbound(vm, "t")
	if err != nil || ob2["security"] != "chacha20-poly1305" {
		t.Fatalf("vmess scy 未生效: %v %v", ob2, err)
	}
}

func TestParseSubSingBoxJSON(t *testing.T) {
	body := `{"outbounds":[
		{"type":"shadowsocks","tag":"机场-SS","server":"1.2.3.4","server_port":8388,"method":"aes-128-gcm","password":"p"},
		{"type":"direct","tag":"direct"},
		{"type":"selector","tag":"自动选择","outbounds":["机场-SS"]}
	]}`
	nodes, err := parseSubContent(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 {
		t.Fatalf("应只保留 1 个可用出站, 得到 %d", len(nodes))
	}
	ob, _ := nodes[0].(map[string]any)
	if ob["tag"] != "sub-机场-SS" {
		t.Fatalf("tag 重命名: %v", ob["tag"])
	}
}

func TestParseSubClashYAML(t *testing.T) {
	body := `proxies:
  - {name: '🇸🇬 新加坡', type: ss, server: 1.2.3.4, port: 8388, cipher: aes-128-gcm, password: sspass}
  - {name: '🇺🇸 美国', type: vmess, server: cf.example.com, port: 443, uuid: b831381d-6324-4d53-ad4f-8cda48b30811, alterId: 0, cipher: auto, tls: true, servername: us.example.com, network: ws, ws-opts: {path: /vws, headers: {Host: cdn.example.com}}}
  - {name: '🇯🇵 日本', type: vless, server: 5.6.7.8, port: 443, uuid: b831381d-6324-4d53-ad4f-8cda48b30811, tls: true, servername: jp.example.com, client-fingerprint: chrome, reality-opts: {public-key: JWjdtg9aV2c1XBSjOwoP7qRQYimlqKkMH4zx9dJn-xg, short-id: af317506}, flow: xtls-rprx-vision}
  - {name: '🇫🇷 法国', type: trojan, server: 9.9.9.9, port: 443, password: tjp, sni: fr.example.com, skip-cert-verify: true, network: ws, ws-opts: {path: /tr}}
  - {name: '🇰🇷 韩国', type: hysteria2, server: 7.7.7.7, port: 8443, password: hy2p, sni: kr.example.com, skip-cert-verify: true, obfs: salamander, obfs-password: op}
  - {name: 'socks节点', type: socks5, server: 8.8.4.4, port: 1080, username: su, password: sp}
`
	nodes, err := parseSubContent(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 6 {
		t.Fatalf("Clash 订阅应有 6 个节点, 得到 %d", len(nodes))
	}
	ob0 := nodes[0].(map[string]any)
	if ob0["type"] != "shadowsocks" || ob0["method"] != "aes-128-gcm" || ob0["server_port"] != 8388 {
		t.Fatalf("ss: %v", ob0)
	}
	ob1 := nodes[1].(map[string]any)
	if ob1["type"] != "vmess" || ob1["uuid"] != "b831381d-6324-4d53-ad4f-8cda48b30811" {
		t.Fatalf("vmess: %v", ob1)
	}
	tls1, _ := ob1["tls"].(map[string]any)
	if tls1 == nil || tls1["server_name"] != "us.example.com" {
		t.Fatalf("vmess tls: %v", tls1)
	}
	tr1, _ := ob1["transport"].(map[string]any)
	if tr1 == nil || tr1["path"] != "/vws" {
		t.Fatalf("vmess transport: %v", tr1)
	}
	ob2 := nodes[2].(map[string]any)
	tls2, _ := ob2["tls"].(map[string]any)
	reality2, _ := tls2["reality"].(map[string]any)
	if reality2 == nil || reality2["public_key"] != "JWjdtg9aV2c1XBSjOwoP7qRQYimlqKkMH4zx9dJn-xg" {
		t.Fatalf("vless reality: %v", tls2)
	}
	ob3 := nodes[3].(map[string]any)
	tls3, _ := ob3["tls"].(map[string]any)
	if tls3 == nil || tls3["insecure"] != true {
		t.Fatalf("trojan skip-cert-verify: %v", tls3)
	}
	ob4 := nodes[4].(map[string]any)
	if ob4["type"] != "hysteria2" || ob4["obfs"].(map[string]any)["password"] != "op" {
		t.Fatalf("hysteria2: %v", ob4)
	}
	ob5 := nodes[5].(map[string]any)
	if ob5["type"] != "socks" || ob5["username"] != "su" {
		t.Fatalf("socks5: %v", ob5)
	}
}

func TestParseSSWithV2RayPlugin(t *testing.T) {
	link := "ss://bm9uZTpkMWNmNGI5Yy0zZTU3LTA4NWQtYjM0YS03OTdmY2Y2MDEzODE@staticdelivery.nexusmods.com:443?plugin=v2ray-plugin%3Bmode%3Dwebsocket%3Bhost%3Dvercel0731.5566248.cc.cd%3Bpath%3D%2Fd1cf4b9c%3Btls%3Bsni%3Dvercel0731.5566248.cc.cd%3Bskip-cert-verify%3Dtrue%3Bmux%3D0#ss-plugin"
	ob, err := nodeOutbound(link, "t")
	if err != nil {
		t.Fatal(err)
	}
	if ob["method"] != "none" || ob["password"] != "d1cf4b9c-3e57-085d-b34a-797fcf601381" {
		t.Fatalf("ss base64: %v", ob)
	}
	if ob["plugin"] != "v2ray-plugin" {
		t.Fatalf("plugin: %v", ob["plugin"])
	}
	opts, _ := ob["plugin_opts"].(string)
	if !strings.Contains(opts, "mode=websocket") || !strings.Contains(opts, "host=vercel0731.5566248.cc.cd") || !strings.Contains(opts, "tls") {
		t.Fatalf("plugin_opts: %q", opts)
	}
}
