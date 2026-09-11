package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestHeadersFromVersions(t *testing.T) {
	h := headersFromVersions(clineVersionInfo{CLI: "3.0.61", Core: "0.0.82"})
	want := map[string]string{
		"User-Agent":         "Cline/3.0.61",
		"X-CLIENT-VERSION":   "3.0.61",
		"X-PLATFORM-VERSION": "3.0.61",
		"X-CORE-VERSION":     "0.0.82",
		"HTTP-Referer":       "https://cline.bot",
		"X-Title":            "Cline",
		"X-IS-MULTIROOT":     "false",
		"X-CLIENT-TYPE":      "cline-cli",
		"X-PLATFORM":         "terminal",
	}
	if len(h) != len(want) {
		t.Fatalf("header set = %v", headerNames(h))
	}
	for k, v := range want {
		if h[k] != v {
			t.Errorf("%s = %q, want %q", k, h[k], v)
		}
	}
}

// 官方字段以官方值为准, 用户自己追加的头不能被抹掉。
func TestMergeOfficialHeadersKeepsCustomEntries(t *testing.T) {
	cur := map[string]string{
		"X-CLIENT-VERSION": "3.0.50",     // 过期版本
		"X-MY-TRACE":       "keep-me",    // 用户自定义
		"User-Agent":       "Cline/1.0.0",
	}
	merged := mergeOfficialHeaders(cur, map[string]string{
		"X-CLIENT-VERSION": "3.0.61",
		"User-Agent":       "Cline/3.0.61",
	})
	if merged["X-CLIENT-VERSION"] != "3.0.61" || merged["User-Agent"] != "Cline/3.0.61" {
		t.Fatalf("official values must win: %v", merged)
	}
	if merged["X-MY-TRACE"] != "keep-me" {
		t.Fatal("custom headers must survive the sync")
	}
}

// 官方 registry 里 cline 包的顶层 version 是 CLI 版本, dependencies 里带核心版本。
func TestFetchVersionFromRegistryPayload(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"name":    "cline",
			"version": "3.0.61",
			"dependencies": map[string]string{
				"@cline/core": "0.0.82",
				"@cline/sdk":  "0.0.82",
			},
		})
	}))
	defer srv.Close()

	info, err := fetchVersionFrom(context.Background(), srv.Client(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if info.CLI != "3.0.61" || info.Core != "0.0.82" {
		t.Fatalf("parsed versions: %+v", info)
	}
	// 带 v 前缀的 tag 形式也要能处理
	if got := cleanVersion("v3.0.61"); got != "3.0.61" {
		t.Fatalf("cleanVersion = %q", got)
	}
}

// @cline/core 单独的 package 返回的 version 就是核心版本。
func TestFetchVersionFromCorePackage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"name": "@cline/core", "version": "0.0.82"})
	}))
	defer srv.Close()

	info, err := fetchVersionFrom(context.Background(), srv.Client(), srv.URL+"/@cline%2fcore/latest")
	if err != nil {
		t.Fatal(err)
	}
	if info.Core != "0.0.82" {
		t.Fatalf("core package must map to the core version: %+v", info)
	}
}

// 真实网络校验: 默认跳过, 用 CLINE_PROXY_NET_TESTS=1 打开。
// 官方版本号是外部事实, 只有真跑一次才知道上游换了包名或字段。
func TestFetchClineVersionsLive(t *testing.T) {
	if os.Getenv("CLINE_PROXY_NET_TESTS") != "1" {
		t.Skip("set CLINE_PROXY_NET_TESTS=1 to query the official registries")
	}
	info, err := fetchClineVersions(context.Background())
	if err != nil {
		t.Fatalf("live version lookup failed: %v", err)
	}
	if info.CLI == "" {
		t.Fatalf("no CLI version resolved: %+v", info)
	}
	t.Logf("source=%s cli=%s core=%s", info.Source, info.CLI, info.Core)
}
