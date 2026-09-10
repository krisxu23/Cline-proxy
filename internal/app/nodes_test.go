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

func TestIsNodeLink(t *testing.T) {
	for _, s := range []string{"vmess://x", "vless://x", "ss://x", "hy2://x", "hysteria2://x", "tuic://x", "trojan://x"} {
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

	syncNodeBox([]string{link})
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

	syncNodeBox(nil)
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
