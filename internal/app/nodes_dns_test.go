package app

import (
	"context"
	"testing"

	box "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/include"
	"github.com/sagernet/sing-box/option"
	sjson "github.com/sagernet/sing/common/json"
)

func dnsServers(t *testing.T, cfg *zenConfigData) ([]any, string) {
	t.Helper()
	m, tag := buildNodeDNS(cfg)
	servers, ok := m["servers"].([]any)
	if !ok {
		t.Fatalf("dns config must contain servers: %#v", m)
	}
	return servers, tag
}

func firstServer(t *testing.T, servers []any) map[string]any {
	t.Helper()
	if len(servers) == 0 {
		t.Fatal("dns servers must not be empty")
	}
	s, _ := servers[0].(map[string]any)
	return s
}

// 默认模式必须是 DoH(阿里) 而不是明文 UDP —— 8.8.8.8:53 在大陆会被污染。
func TestBuildNodeDNSDefaultsToDoH(t *testing.T) {
	for _, cfg := range []*zenConfigData{nil, {}, {DNSMode: ""}, {DNSMode: "bogus"}} {
		servers, tag := dnsServers(t, cfg)
		if tag != dnsDefaultTag {
			t.Fatalf("default resolver tag = %q", tag)
		}
		if len(servers) != 2 {
			t.Fatalf("expected bootstrap + DoH, got %d entries", len(servers))
		}
		bootstrap := firstServer(t, servers)
		if bootstrap["type"] != "local" {
			t.Fatalf("bootstrap must be local, got %v", bootstrap["type"])
		}
		doh, _ := servers[1].(map[string]any)
		if doh["type"] != "https" || doh["server"] != "dns.alidns.com" {
			t.Fatalf("default DoH wrong: %#v", doh)
		}
		if doh["domain_resolver"] != dnsBootstrapTag {
			t.Fatal("DoH hostname must be resolved by the bootstrap resolver")
		}
	}
	// 任何模式下都不该再出现明文 8.8.8.8 UDP
	for _, mode := range []string{dnsModeDoHAli, dnsModeDoHCF, dnsModeSystem, dnsModeCustom} {
		servers, _ := dnsServers(t, &zenConfigData{DNSMode: mode, DNSCustomDNS: "https://doh.example.com/dns-query"})
		for _, s := range servers {
			sm, _ := s.(map[string]any)
			if sm["type"] == "udp" {
				t.Errorf("mode %s must not use plain UDP DNS: %#v", mode, sm)
			}
		}
	}
}

// 系统模式只用一个 local 解析器。
func TestBuildNodeDNSSystemMode(t *testing.T) {
	servers, tag := dnsServers(t, &zenConfigData{DNSMode: dnsModeSystem})
	if len(servers) != 1 || tag != dnsDefaultTag {
		t.Fatalf("system mode must be a single local resolver, got %#v tag=%q", servers, tag)
	}
}

// Cloudflare 用 IP(证书含该 IP 的 SAN), 阿里用域名(证书只覆盖域名)。
func TestBuildNodeDNSCloudflare(t *testing.T) {
	servers, _ := dnsServers(t, &zenConfigData{DNSMode: dnsModeDoHCF})
	doh, _ := servers[1].(map[string]any)
	if doh["server"] != "1.1.1.1" {
		t.Fatalf("cloudflare DoH must use 1.1.1.1, got %v", doh["server"])
	}
}

// 自定义 DoH: 解析 host/port/path; 非法输入退回默认而不是让实例起不来。
func TestBuildNodeDNSCustom(t *testing.T) {
	servers, _ := dnsServers(t, &zenConfigData{DNSMode: dnsModeCustom, DNSCustomDNS: "https://doh.example.com:8443/my-query"})
	doh, _ := servers[1].(map[string]any)
	if doh["server"] != "doh.example.com" || doh["server_port"] != 8443 || doh["path"] != "/my-query" {
		t.Fatalf("custom DoH not parsed: %#v", doh)
	}

	servers, _ = dnsServers(t, &zenConfigData{DNSMode: dnsModeCustom, DNSCustomDNS: "doh.example.com"})
	doh, _ = servers[1].(map[string]any)
	if doh["server"] != "doh.example.com" {
		t.Fatalf("bare host must be accepted: %#v", doh)
	}

	// 非法地址 -> 回退阿里, 且不返回错误(不能因为一个配置项把出口整个搞挂)
	servers, _ = dnsServers(t, &zenConfigData{DNSMode: dnsModeCustom, DNSCustomDNS: "http://plain.example.com"})
	doh, _ = servers[1].(map[string]any)
	if doh["server"] != "dns.alidns.com" {
		t.Fatalf("invalid custom DoH must fall back to AliDNS, got %#v", doh)
	}
}

func TestParseDoHURL(t *testing.T) {
	host, port, path, err := parseDoHURL("https://dns.alidns.com/dns-query")
	if err != nil || host != "dns.alidns.com" || port != 443 || path != "/dns-query" {
		t.Fatalf("got %q %d %q %v", host, port, path, err)
	}
	if _, _, _, err := parseDoHURL("ftp://x/y"); err == nil {
		t.Fatal("non-https must be rejected")
	}
	if _, _, _, err := parseDoHURL(""); err == nil {
		t.Fatal("empty must be rejected")
	}
}

// 生成的 DNS 段必须真能被 sing-box 接受 —— 写错字段会让整个出口实例起不来。
func TestNodeDNSConfigAcceptedBySingBox(t *testing.T) {
	requireNodeBox(t)
	for _, mode := range []string{dnsModeDoHAli, dnsModeDoHCF, dnsModeSystem, dnsModeCustom} {
		cfg := &zenConfigData{DNSMode: mode, DNSCustomDNS: "https://doh.example.com/dns-query"}
		dnsCfg, resolverTag := buildNodeDNS(cfg)
		boxCfg := map[string]any{
			"log":       map[string]any{"disabled": true},
			"dns":       dnsCfg,
			"outbounds": []any{map[string]any{"type": "direct", "tag": "direct"}},
			"route": map[string]any{
				"final":                   "direct",
				"default_domain_resolver": map[string]any{"server": resolverTag},
			},
		}
		data, _ := sjson.Marshal(boxCfg)
		ctx := include.Context(context.Background())
		opts, err := sjson.UnmarshalExtendedContext[option.Options](ctx, data)
		if err != nil {
			t.Fatalf("mode %s: unmarshal: %v", mode, err)
		}
		b, err := box.New(box.Options{Context: ctx, Options: opts})
		if err != nil {
			t.Fatalf("mode %s: box.New: %v", mode, err)
		}
		if err := b.Start(); err != nil {
			b.Close()
			t.Fatalf("mode %s: box.Start: %v", mode, err)
		}
		b.Close()
	}
}
