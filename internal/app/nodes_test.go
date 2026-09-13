package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"strconv"
	"testing"
	"time"

	box "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/include"
	"github.com/sagernet/sing-box/option"
	sjson "github.com/sagernet/sing/common/json"
)

func TestParseVmessWsTls(t *testing.T) {
	payload := base64.StdEncoding.EncodeToString([]byte(
		`{"v":"2","ps":"vm节点","add":"vm.example.com","port":"443","id":"b831381d-6324-4d53-ad4f-8cda48b30811","aid":"0","net":"ws","host":"cdn.example.com","path":"/ws","tls":"tls","sni":"sni.example.com"}`))
	ob, err := nodeOutbound("vmess://"+payload, "out-0")
	if err != nil {
		t.Fatal(err)
	}
	if ob["server"] != "vm.example.com" || ob["server_port"] != 443 {
		t.Fatalf("server/port: %v %v", ob["server"], ob["server_port"])
	}
	if ob["uuid"] != "b831381d-6324-4d53-ad4f-8cda48b30811" {
		t.Fatalf("uuid: %v", ob["uuid"])
	}
	tls, _ := ob["tls"].(map[string]any)
	if tls == nil || tls["server_name"] != "sni.example.com" {
		t.Fatalf("tls: %v", tls)
	}
	tr, _ := ob["transport"].(map[string]any)
	if tr == nil || tr["type"] != "ws" || tr["path"] != "/ws" {
		t.Fatalf("transport: %v", tr)
	}
}

func TestParseVlessRealityGrpc(t *testing.T) {
	link := "vless://b831381d-6324-4d53-ad4f-8cda48b30811@1.2.3.4:443?security=reality&type=grpc&flow=xtls-rprx-vision&sni=www.microsoft.com&pbk=SbVKOEMjK0sIlbwg4akyBg5mL5KZwwB-ed4eEE7YnRc&sid=6ba85179&fp=chrome&serviceName=grpc-svc#vless节点"
	ob, err := nodeOutbound(link, "out-1")
	if err != nil {
		t.Fatal(err)
	}
	if ob["flow"] != "xtls-rprx-vision" {
		t.Fatalf("flow: %v", ob["flow"])
	}
	tls, _ := ob["tls"].(map[string]any)
	if tls == nil {
		t.Fatal("missing tls")
	}
	reality, _ := tls["reality"].(map[string]any)
	if reality == nil || reality["public_key"] != "SbVKOEMjK0sIlbwg4akyBg5mL5KZwwB-ed4eEE7YnRc" {
		t.Fatalf("reality: %v", reality)
	}
	tr, _ := ob["transport"].(map[string]any)
	if tr == nil || tr["service_name"] != "grpc-svc" {
		t.Fatalf("transport: %v", tr)
	}
}

func TestParseHy2Obfs(t *testing.T) {
	link := "hy2://secretpass@5.6.7.8:8443?sni=hy.example.com&insecure=1&obfs=salamander&obfs-password=obfspass#hy2"
	ob, err := nodeOutbound(link, "out-2")
	if err != nil {
		t.Fatal(err)
	}
	if ob["type"] != "hysteria2" || ob["password"] != "secretpass" {
		t.Fatalf("ob: %v", ob)
	}
	if ob["tls"].(map[string]any)["insecure"] != true {
		t.Fatal("insecure not applied")
	}
	obfs, _ := ob["obfs"].(map[string]any)
	if obfs == nil || obfs["password"] != "obfspass" {
		t.Fatalf("obfs: %v", obfs)
	}
}

func TestParseTuicAlpn(t *testing.T) {
	link := "tuic://b831381d-6324-4d53-ad4f-8cda48b30811:tuicpass@9.9.9.9:443?congestion_control=bbr&alpn=h3&udp_relay_mode=native&sni=tu.example.com#tuic"
	ob, err := nodeOutbound(link, "out-3")
	if err != nil {
		t.Fatal(err)
	}
	if ob["congestion_control"] != "bbr" || ob["udp_relay_mode"] != "native" {
		t.Fatalf("ob: %v", ob)
	}
	if alpn, _ := ob["alpn"].([]string); len(alpn) != 1 || alpn[0] != "h3" {
		t.Fatalf("alpn: %v", ob["alpn"])
	}
}

func TestParseSSBothFormats(t *testing.T) {
	cred := base64.URLEncoding.EncodeToString([]byte("aes-128-gcm:sspassword"))
	link1 := "ss://" + cred + "@10.0.0.1:8388#ss1"
	ob, err := nodeOutbound(link1, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if ob["method"] != "aes-128-gcm" || ob["password"] != "sspassword" || ob["server_port"] != 8388 {
		t.Fatalf("userinfo format: %v", ob)
	}
	whole := base64.URLEncoding.EncodeToString([]byte("aes-128-gcm:sspassword@10.0.0.2:8388"))
	ob2, err := nodeOutbound("ss://"+whole+"#ss2", "t2")
	if err != nil {
		t.Fatal(err)
	}
	if ob2["server"] != "10.0.0.2" || ob2["password"] != "sspassword" {
		t.Fatalf("whole format: %v", ob2)
	}
}

func TestParseTrojanWs(t *testing.T) {
	link := "trojan://tjpass@3.3.3.3:443?sni=tj.example.com&type=ws&path=%2Ftj&host=tjcdn.example.com#trojan"
	ob, err := nodeOutbound(link, "out-4")
	if err != nil {
		t.Fatal(err)
	}
	if ob["type"] != "trojan" || ob["password"] != "tjpass" {
		t.Fatalf("ob: %v", ob)
	}
	tls, _ := ob["tls"].(map[string]any)
	if tls == nil || tls["enabled"] != true {
		t.Fatal("trojan must force tls")
	}
	tr, _ := ob["transport"].(map[string]any)
	if tr == nil || tr["type"] != "ws" {
		t.Fatalf("transport: %v", tr)
	}
}

func TestParseHysteriaV1(t *testing.T) {
	link := "hysteria://authstring@7.7.7.7:36712?peer=hy1.example.com&insecure=1&upmbps=80&downmbps=200&obfs=Xplus#hy1"
	ob, err := nodeOutbound(link, "out-5")
	if err != nil {
		t.Fatal(err)
	}
	if ob["auth_str"] != "authstring" || ob["up_mbps"] != 80 || ob["down_mbps"] != 200 {
		t.Fatalf("ob: %v", ob)
	}
	if ob["obfs"] != "Xplus" {
		t.Fatalf("obfs: %v", ob["obfs"])
	}
	if ob["tls"].(map[string]any)["server_name"] != "hy1.example.com" {
		t.Fatal("peer sni not applied")
	}
	// query 形式 auth
	ob2, err := nodeOutbound("hysteria://7.7.7.7:36712?auth=qauth#hy1", "out-5b")
	if err != nil || ob2["auth_str"] != "qauth" {
		t.Fatalf("query auth: %v %v", ob2, err)
	}
}

func TestParseAnytlsSSHShadowtlsSnell(t *testing.T) {
	ob, err := nodeOutbound("anytls://anypass@1.1.1.1:8443?sni=at.example.com#anytls", "t")
	if err != nil || ob["type"] != "anytls" || ob["password"] != "anypass" {
		t.Fatalf("anytls: %v %v", ob, err)
	}
	ob, err = nodeOutbound("ssh://root:sshpw@4.4.4.4:2222?host_key=sha256-AAA,BBB#ssh", "t")
	if err != nil || ob["user"] != "root" || ob["password"] != "sshpw" || ob["server_port"] != 2222 {
		t.Fatalf("ssh: %v %v", ob, err)
	}
	if hk, _ := ob["host_key"].([]string); len(hk) != 2 {
		t.Fatalf("ssh host_key: %v", ob["host_key"])
	}
	ob, err = nodeOutbound("shadowtls://stpass@5.5.5.5:443?version=2&sni=st.example.com#stls", "t")
	if err != nil || ob["type"] != "shadowtls" || ob["version"] != 2 || ob["password"] != "stpass" {
		t.Fatalf("shadowtls: %v %v", ob, err)
	}
	ob, err = nodeOutbound("snell://snpsk@6.6.6.6:6160?version=4&obfs=http#snell", "t")
	if err != nil || ob["type"] != "snell" || ob["psk"] != "snpsk" || ob["version"] != 4 {
		t.Fatalf("snell: %v %v", ob, err)
	}
}

func TestIsNodeLink(t *testing.T) {
	for _, s := range []string{"vmess://x", "vless://x", "ss://x", "hy2://x", "hysteria2://x", "tuic://x", "trojan://x", "hysteria://x", "anytls://x", "ssh://x", "shadowtls://x", "snell://x"} {
		if !isNodeLink(s) {
			t.Fatalf("%s should be node link", s)
		}
	}
	for _, s := range []string{"http://1.2.3.4:8080", "socks5://u:p@1.2.3.4:1080", "https://p:80"} {
		if isNodeLink(s) {
			t.Fatalf("%s should not be node link", s)
		}
	}
}

// TestNodeBridgeEndToEnd 全链路: 网关节点桥 → 本地 sing-box 假节点服务器 → 目标回声服务
func TestNodeBridgeEndToEnd(t *testing.T) {
	requireNodeBox(t)
	nodeMu.Lock()
	if nodeBox != nil {
		nodeBox.Close()
		nodeBox = nil
		nodePorts = nil
		nodePortsKeys = ""
	}
	nodeMu.Unlock()

	targetPort, _ := freeLocalPort()
	ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(targetPort))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go io.Copy(c, c)
		}
	}()

	serverPort, _ := freeLocalPort()
	serverCfg := map[string]any{
		"log": map[string]any{"disabled": true},
		"inbounds": []any{map[string]any{
			"type": "shadowsocks", "tag": "ss-in",
			"listen": "127.0.0.1", "listen_port": serverPort,
			"method": "aes-128-gcm", "password": "testpass",
		}},
		"outbounds": []any{map[string]any{"type": "direct", "tag": "direct"}},
	}
	serverBox := startTestBox(t, serverCfg)
	defer serverBox.Close()

	cred := base64.URLEncoding.EncodeToString([]byte("aes-128-gcm:testpass"))
	link := "ss://" + cred + "@127.0.0.1:" + strconv.Itoa(serverPort) + "#e2e-node"

	zenConfigMu.Lock()
	zenConfig.Proxies = []string{link}
	zenConfigMu.Unlock()
	syncNodeBox()
	local := nodeLocalAddr(link)
	if local == "" {
		t.Fatal("node bridge did not start")
	}
	conn, err := dialNodeProxy(context.Background(), link, "tcp", "127.0.0.1:"+strconv.Itoa(targetPort))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(15 * time.Second))
	if _, err := conn.Write([]byte("ping-e2e")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 8)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "ping-e2e" {
		t.Fatalf("echo mismatch: %q", string(buf))
	}

	zenConfigMu.Lock()
	zenConfig.Proxies = nil
	zenConfigMu.Unlock()
	syncNodeBox()
}

// TestAllOutboundTypesRegistered 验证带构建标签的产物中,
// 全部受支持节点类型都能被 sing-box 实例化(出站惰性拨号, 无需真实服务器)。
func TestAllOutboundTypesRegistered(t *testing.T) {
	requireNodeBox(t)
	cfg := map[string]any{
		"log": map[string]any{"disabled": true},
		"dns": map[string]any{"servers": []any{map[string]any{"type": "udp", "tag": "dns-direct", "server": "8.8.8.8"}}},
		"outbounds": []any{
			map[string]any{"type": "shadowsocks", "tag": "ss", "server": "1.2.3.4", "server_port": 8388, "method": "aes-128-gcm", "password": "p"},
			map[string]any{"type": "vmess", "tag": "vmess", "server": "1.2.3.4", "server_port": 443, "uuid": "b831381d-6324-4d53-ad4f-8cda48b30811", "security": "auto", "alter_id": 0},
			map[string]any{"type": "vless", "tag": "vless-grpc", "server": "1.2.3.4", "server_port": 443, "uuid": "b831381d-6324-4d53-ad4f-8cda48b30811",
				"tls":       map[string]any{"enabled": true, "server_name": "x.com", "utls": map[string]any{"enabled": true, "fingerprint": "chrome"}},
				"transport": map[string]any{"type": "grpc", "service_name": "svc"}},
			map[string]any{"type": "trojan", "tag": "trojan", "server": "1.2.3.4", "server_port": 443, "password": "p", "tls": map[string]any{"enabled": true, "server_name": "x.com"}},
			map[string]any{"type": "hysteria2", "tag": "hy2", "server": "1.2.3.4", "server_port": 443, "password": "p", "tls": map[string]any{"enabled": true, "server_name": "x.com"}},
			map[string]any{"type": "tuic", "tag": "tuic", "server": "1.2.3.4", "server_port": 443, "uuid": "b831381d-6324-4d53-ad4f-8cda48b30811", "password": "p", "tls": map[string]any{"enabled": true, "server_name": "x.com"}},
			map[string]any{"type": "hysteria", "tag": "hy1", "server": "1.2.3.4", "server_port": 443, "auth_str": "p", "up_mbps": 50, "down_mbps": 100, "tls": map[string]any{"enabled": true, "server_name": "x.com"}},
			map[string]any{"type": "anytls", "tag": "anytls", "server": "1.2.3.4", "server_port": 443, "password": "p", "tls": map[string]any{"enabled": true, "server_name": "x.com"}},
			map[string]any{"type": "ssh", "tag": "ssh", "server": "1.2.3.4", "server_port": 22, "user": "u", "password": "p"},
			map[string]any{"type": "shadowtls", "tag": "shadowtls", "server": "1.2.3.4", "server_port": 443, "version": 3, "password": "p", "tls": map[string]any{"enabled": true, "server_name": "x.com"}},
			map[string]any{"type": "snell", "tag": "snell", "server": "1.2.3.4", "server_port": 6160, "psk": "p", "version": 4},
			map[string]any{"type": "direct", "tag": "direct"},
		},
	}
	instance := startTestBox(t, cfg)
	instance.Close()
}

func startTestBox(t *testing.T, cfg map[string]any) *box.Box {
	t.Helper()
	ctx := include.Context(context.Background())
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	opts, err := sjson.UnmarshalExtendedContext[option.Options](ctx, data)
	if err != nil {
		t.Fatal(err)
	}
	instance, err := box.New(box.Options{Context: ctx, Options: opts})
	if err != nil {
		t.Fatal(err)
	}
	if err := instance.Start(); err != nil {
		instance.Close()
		t.Fatal(err)
	}
	return instance
}
