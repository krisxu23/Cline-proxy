package app

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 保存 zen 设置时, 请求体里没提到的字段必须原样保留。
//
// 回归自一处真实事故: 这个 handler 曾经用字段字面量重建配置对象,
// `next := &zenConfigData{...}` 只列举了自己想得到的 16 个字段,
// 而结构体有 23 个。于是用户在 zen 设置页改任意一项(例如重试次数),
// 下面这些字段就全部被零值覆盖并落盘:
//   Routes(候选链) / Router(自动路由) / Usage(每日配额账本) /
//   CooldownMs / DNSMode / DNSCustomDNS / RescueDirect
// 其中 RescueDirect 归 nil 会被"缺省视为 true"解释成重新打开
// "节点全挂时直连兜底", 悄悄绕开统一出口。
func TestZenConfigUpdatePreservesUntouchedFields(t *testing.T) {
	off := false
	withTestConfig(t, &zenConfigData{
		Enabled:      true,
		Key:          "public",
		BaseURL:      zenAPIBase,
		ExitMode:     exitModeProxy,
		Retries:      3,
		DNSMode:      dnsModeDoHCF,
		DNSCustomDNS: "https://dns.example/dns-query",
		RescueDirect: &off,
		Routes: map[string][]string{
			"free-best": {"zen:mimo-v2.5-free", "cline:*"},
		},
		Router: zenRouterConfig{
			Alias:     "free-best",
			Providers: []string{upstreamZen, upstreamCline},
		},
		Usage: zenUsageConfig{
			RetentionDays: 7,
			Timezone:      "Asia/Shanghai",
			DailyLimits:   map[string]int{"free-best": 200},
		},
		CooldownMs: map[string]int64{classRateLimit: 1234},
	})

	req := httptest.NewRequest("POST", "/admin/api/zen/config/update", strings.NewReader(`{"retries": 5}`))
	rec := httptest.NewRecorder()
	handleZenConfigUpdate(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	got := getZenConfig()
	if got.Retries != 5 {
		t.Fatalf("retries = %d, 提交的字段没有生效", got.Retries)
	}
	if len(got.Routes["free-best"]) != 2 {
		t.Errorf("Routes(候选链)被清空: %#v", got.Routes)
	}
	if got.Router.Alias != "free-best" || len(got.Router.Providers) != 2 {
		t.Errorf("Router(自动路由)被清空: %#v", got.Router)
	}
	if got.Usage.RetentionDays != 7 || got.Usage.DailyLimits["free-best"] != 200 {
		t.Errorf("Usage(每日配额账本)被清空: %#v", got.Usage)
	}
	if got.CooldownMs[classRateLimit] != 1234 {
		t.Errorf("CooldownMs 被清空: %#v", got.CooldownMs)
	}
	if got.DNSMode != dnsModeDoHCF || got.DNSCustomDNS != "https://dns.example/dns-query" {
		t.Errorf("DNS 段被清空: mode=%q custom=%q", got.DNSMode, got.DNSCustomDNS)
	}
	if got.RescueDirect == nil {
		t.Error("RescueDirect 被重置成 nil —— 等于重新打开直连兜底, 绕开统一出口")
	} else if *got.RescueDirect != false {
		t.Errorf("RescueDirect = %v, 用户显式关闭的设置被改掉", *got.RescueDirect)
	}
}

// getZenConfig 必须返回克隆体: 拿到后改它不能影响全局。
//
// 否则热路径上那些"拿配置 → 长时间持有"的调用方(选出口、构请求头)
// 会与 mutateProvidersConfig 的写并发访问同一张 map, 触发 Go 运行时的
// concurrent map read/write —— fatal error, recover 无效, 进程直接退出。
func TestGetZenConfigReturnsDetachedCopy(t *testing.T) {
	withTestConfig(t, &zenConfigData{
		Retries:    3,
		Routes:     map[string][]string{"a": {"zen:x"}},
		CooldownMs: map[string]int64{classTimeout: 1},
		Usage:      zenUsageConfig{DailyLimits: map[string]int{"a": 1}},
		Providers:  map[string]providerConfig{"p": {BaseURL: "https://example.com", APIKeys: []providerAPIKey{{Key: "k", Enabled: true}}}},
	})

	leak := getZenConfig()
	leak.Retries = 999
	leak.Routes["a"] = append(leak.Routes["a"], "zen:y")
	leak.Routes["b"] = []string{"zen:z"}
	delete(leak.CooldownMs, classTimeout)
	leak.Usage.DailyLimits["a"] = 42
	pc := leak.Providers["p"]
	pc.APIKeys[0].Key = "tampered"
	leak.Providers["p"] = pc

	fresh := getZenConfig()
	if fresh.Retries != 3 {
		t.Errorf("标量被改写: retries = %d", fresh.Retries)
	}
	if len(fresh.Routes["a"]) != 1 || fresh.Routes["b"] != nil {
		t.Errorf("Routes 与全局共享: %#v", fresh.Routes)
	}
	if _, ok := fresh.CooldownMs[classTimeout]; !ok {
		t.Errorf("CooldownMs 与全局共享: %#v", fresh.CooldownMs)
	}
	if fresh.Usage.DailyLimits["a"] != 1 {
		t.Errorf("Usage.DailyLimits 与全局共享: %#v", fresh.Usage.DailyLimits)
	}
	if fresh.Providers["p"].APIKeys[0].Key != "k" {
		t.Errorf("Providers 内层切片与全局共享: %#v", fresh.Providers["p"].APIKeys)
	}
}

// 并发读配置与并发写配置不得触发 data race(配合 -race 运行)。
func TestConcurrentConfigReadWrite(t *testing.T) {
	withTestConfig(t, &zenConfigData{Retries: 3, Routes: map[string][]string{"a": {"zen:x"}}})

	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 200; j++ {
				cfg := getZenConfig()
				_ = cfg.Retries
				for range cfg.Routes["a"] {
				}
			}
		}()
	}
	for i := 0; i < 4; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 100; j++ {
				mutateProvidersConfig(func(cfg *zenConfigData) {
					if cfg.Routes == nil {
						cfg.Routes = map[string][]string{}
					}
					cfg.Routes["a"] = append(cfg.Routes["a"], "zen:y")
				})
			}
		}()
	}
	for i := 0; i < 12; i++ {
		<-done
	}
}
