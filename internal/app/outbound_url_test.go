package app

import (
	"strings"
	"testing"
)

// SSRF 面(审计 P3-10): 链路本地/元数据/未指定地址**永远**拦截;
// 私网与回环默认拦截, 但可用 FREE_ROUTER_ALLOW_PRIVATE_UPSTREAM=1 放开
// (本机/内网自建上游是真实用法, 需要逃生口)。

func TestValidateOutboundURLRejectsBadSchemeAndEmptyHost(t *testing.T) {
	for _, bad := range []string{
		"file:///etc/passwd",
		"gopher://evil",
		"ftp://example.com/x",
		"https:///nohost",
		"",
	} {
		if err := validateOutboundURL(bad); err == nil {
			t.Errorf("%q 应被拒绝", bad)
		}
	}
	if err := validateOutboundURL("https://api.example.com/v1"); err != nil {
		t.Errorf("正常公网地址不应被拒: %v", err)
	}
}

func TestMetadataAndLinkLocalAlwaysBlocked(t *testing.T) {
	// 即使开了私网上游开关, 元数据地址也必须继续拦 —— 它没有任何合法用途。
	t.Setenv(AllowPrivateUpstreamEnv, "1")
	for _, bad := range []string{
		"http://169.254.169.254/latest/meta-data/",
		"http://metadata.google.internal/computeMetadata/v1/",
		"http://instance-data/latest/",
		"http://0.0.0.0:8080/v1",
		"http://100.64.1.1/v1",
		"http://[fe80::1]/v1",
	} {
		if err := validateOutboundURL(bad); err == nil {
			t.Errorf("%q 必须永远被拒绝(元数据/链路本地/未指定/CGNAT)", bad)
		}
	}
}

func TestPrivateAndLoopbackBlockedByDefault(t *testing.T) {
	t.Setenv(AllowPrivateUpstreamEnv, "")
	for _, bad := range []string{
		"http://127.0.0.1:11434/v1",
		"http://localhost:8080/v1", // 注: 名字解析不在此校验, 但字面 IP 必拦
		"http://10.0.0.5/v1",
		"http://192.168.1.10:8000/v1",
		"http://172.16.31.4/v1",
		"http://[::1]:11434/v1",
	} {
		if err := validateOutboundURL(bad); err == nil {
			// localhost 是主机名而非 IP, 不解析即放过 —— 这条单独说明
			if strings.Contains(bad, "localhost") {
				continue
			}
			t.Errorf("%q 默认应被拒绝", bad)
		}
	}
}

func TestPrivateAndLoopbackAllowedWithEnvOptIn(t *testing.T) {
	t.Setenv(AllowPrivateUpstreamEnv, "1")
	for _, ok := range []string{
		"http://127.0.0.1:11434/v1",
		"http://10.0.0.5/v1",
		"http://192.168.1.10:8000/v1",
		"http://[::1]:11434/v1",
	} {
		if err := validateOutboundURL(ok); err != nil {
			t.Errorf("显式放开后 %q 应被接受: %v", ok, err)
		}
	}
}

func TestPrivateOptInAcceptsTrueAndYes(t *testing.T) {
	for _, v := range []string{"true", "TRUE", "yes", "Yes"} {
		t.Setenv(AllowPrivateUpstreamEnv, v)
		if err := validateOutboundURL("http://10.1.2.3/v1"); err != nil {
			t.Errorf("环境变量 %q 应视为放开: %v", v, err)
		}
	}
	t.Setenv(AllowPrivateUpstreamEnv, "0")
	if err := validateOutboundURL("http://10.1.2.3/v1"); err == nil {
		t.Error("0 不应视为放开")
	}
}

func TestFilterOutboundURLsFailsFast(t *testing.T) {
	t.Setenv(AllowPrivateUpstreamEnv, "")
	out, err := filterOutboundURLs([]string{"https://a.example.com/", "", "https://b.example.com"}, "订阅")
	if err != nil {
		t.Fatalf("合法列表不应失败: %v", err)
	}
	if len(out) != 2 || out[0] != "https://a.example.com" {
		t.Fatalf("应去空并去尾部斜杠: %v", out)
	}
	if _, err := filterOutboundURLs([]string{"https://a.example.com", "http://127.0.0.1:1/x"}, "订阅"); err == nil {
		t.Fatal("列表里有一条非法就必须整体失败(不能静默丢弃)")
	}
}
