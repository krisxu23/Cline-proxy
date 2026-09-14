package app

// 出口地区归类与地区过滤的单测。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestRegionOfCountry(t *testing.T) {
	cases := map[string]string{
		"US": "us", "us": "us", " us ": "us",
		"JP": "jp",
		"TW": "tw",
		"HK": "hk", "MO": "hk",
		"SG": "sg",
		"FR": "eu", "DE": "eu", "GB": "eu", "NL": "eu", "SE": "eu", "PL": "eu",
		"CN": "other", "RU": "other", "BR": "other", "IN": "other",
		"": "other", "XYZ": "other",
	}
	for cc, want := range cases {
		if got := regionOfCountry(cc); got != want {
			t.Fatalf("regionOfCountry(%q) = %q, 期望 %q", cc, got, want)
		}
	}
}

func TestNormalizeExitRegions(t *testing.T) {
	// 去重 + 丢弃非法 + 按界面顺序重排
	got := normalizeExitRegions([]string{"tw", "us", "bogus", "tw", " HK ", "eu"})
	want := []string{"us", "tw", "hk", "eu"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("normalizeExitRegions = %v, 期望 %v", got, want)
	}
	if got := normalizeExitRegions(nil); len(got) != 0 {
		t.Fatalf("空输入应为空, got %v", got)
	}
}

// setNodeHealthForTest 直接写入一个节点的检测结果(含出口国家)
func setNodeHealthForTest(t *testing.T, key, country string, ok bool) {
	t.Helper()
	nodeHealthMu.Lock()
	if nodeHealth == nil {
		nodeHealth = map[string]nodeHealthState{}
	}
	nodeHealth[key] = nodeHealthState{
		Ok:     ok,
		At:     time.Now(),
		Result: nodeTestResult{ExitCountry: country},
	}
	nodeHealthMu.Unlock()
	t.Cleanup(func() {
		nodeHealthMu.Lock()
		delete(nodeHealth, key)
		nodeHealthMu.Unlock()
	})
}

func TestNodeExitRegionAndAllowed(t *testing.T) {
	setNodeHealthForTest(t, "vmess://us-node", "US", true)
	setNodeHealthForTest(t, "vmess://jp-node", "JP", true)
	setNodeHealthForTest(t, "vmess://never-probed", "", false)

	if got := nodeExitRegion("vmess://us-node"); got != "us" {
		t.Fatalf("美国节点地区应为 us, got %q", got)
	}
	// 无实测国家 -> other(保守: 勾选具体地区时不会被用到)
	if got := nodeExitRegion("vmess://never-probed"); got != "other" {
		t.Fatalf("未检测节点应归 other, got %q", got)
	}
	if got := nodeExitRegion("http://127.0.0.1:1080"); got != "other" {
		t.Fatalf("手填代理应归 other, got %q", got)
	}

	// 未启用过滤: 一律可用
	withTestConfig(t, &zenConfigData{Enabled: true, ExitMode: exitModeProxy})
	if !exitRegionAllowed("vmess://jp-node") || exitRegionFilterActive() {
		t.Fatal("未勾选地区时不应过滤")
	}

	// 勾选美国: 只有美国节点可用, 未检测节点被排除
	withTestConfig(t, &zenConfigData{Enabled: true, ExitMode: exitModeProxy, EnabledRegions: []string{"us"}})
	if !exitRegionAllowed("vmess://us-node") {
		t.Fatal("勾选美国后美国节点应可用")
	}
	if exitRegionAllowed("vmess://jp-node") {
		t.Fatal("勾选美国后日本节点不应可用")
	}
	if exitRegionAllowed("vmess://never-probed") {
		t.Fatal("勾选美国后未检测节点不应可用(国家未知)")
	}

	// 多选: 美国+日本
	withTestConfig(t, &zenConfigData{Enabled: true, ExitMode: exitModeProxy, EnabledRegions: []string{"us", "jp"}})
	if !exitRegionAllowed("vmess://us-node") || !exitRegionAllowed("vmess://jp-node") {
		t.Fatal("多选美国+日本时两个节点都应可用")
	}
}

func TestFilterByExitRegionKeepsPlainProxiesByDefault(t *testing.T) {
	setNodeHealthForTest(t, "vmess://us-node", "US", true)
	list := []string{"http://127.0.0.1:1080", "vmess://us-node"}

	withTestConfig(t, &zenConfigData{Enabled: true, ExitMode: exitModeProxy})
	if got := filterByExitRegion(list); len(got) != 2 {
		t.Fatalf("未勾选时应原样返回, got %v", got)
	}

	withTestConfig(t, &zenConfigData{Enabled: true, ExitMode: exitModeProxy, EnabledRegions: []string{"us"}})
	invalidateExitListCache()
	got := filterByExitRegion(list)
	// 手填代理无国家信息 -> other, 勾选美国时被排除
	if len(got) != 1 || got[0] != "vmess://us-node" {
		t.Fatalf("勾选美国后应只留美国节点, got %v", got)
	}
}

// 关键接线断言: 地区过滤必须真的作用在全局出口池(effectiveProxyList)上 ——
// 只测 filterByExitRegion 是不够的, 漏接线时它照样全绿。
func TestEffectiveProxyListRespectsExitRegion(t *testing.T) {
	setNodeHealthForTest(t, "vmess://us-node", "US", true)
	setNodeHealthForTest(t, "vmess://jp-node", "JP", true)
	proxies := []string{"vmess://us-node", "vmess://jp-node"}

	withTestConfig(t, &zenConfigData{Enabled: true, ExitMode: exitModeProxy, Proxies: proxies})
	invalidateExitListCache()
	if got := effectiveProxyList(); len(got) != 2 {
		t.Fatalf("未勾选地区时出口池应为全部 2 个, got %v", got)
	}

	withTestConfig(t, &zenConfigData{Enabled: true, ExitMode: exitModeProxy, Proxies: proxies, EnabledRegions: []string{"jp"}})
	invalidateExitListCache()
	got := effectiveProxyList()
	if len(got) != 1 || got[0] != "vmess://jp-node" {
		t.Fatalf("勾选日本后出口池应只剩日本节点, got %v", got)
	}
}

// 重启后地区仍要可用: 实测国家必须落盘, 不能只活在内存里。
// 否则重启后所有出口都变「其他地区」, 用户只勾了美国就会没有出口。
func TestNodeCountryPersistedAcrossRestart(t *testing.T) {
	tmp := t.TempDir() + "/node-regions.json"
	setNodeCountryFileForTest(tmp)
	t.Cleanup(func() {
		setNodeCountryFileForTest("")
		nodeCountryMu.Lock()
		nodeCountryMap = map[string]string{}
		nodeCountryLoaded = false
		nodeCountryDirty = false
		nodeCountryMu.Unlock()
	})

	rememberNodeCountry("vmess://sg-only", "SG")
	persistNodeCountries()
	if got := nodeExitRegion("vmess://sg-only"); got != "sg" {
		t.Fatalf("内存里应记为 sg, got %q", got)
	}

	// 模拟重启: 清空内存后重新从磁盘加载
	nodeCountryMu.Lock()
	nodeCountryMap = map[string]string{}
	nodeCountryLoaded = false
	nodeCountryMu.Unlock()
	if got := nodeExitRegion("vmess://sg-only"); got != "sg" {
		t.Fatalf("重启后应从磁盘恢复为 sg, got %q", got)
	}
}

// 空池兜底: 勾选的地区一个出口都没有时, 必须退回全部出口而不是让网关没有出口。
func TestFilterByExitRegionFallsBackWhenNoMatch(t *testing.T) {
	setNodeHealthForTest(t, "vmess://jp-a", "JP", true)
	setNodeHealthForTest(t, "vmess://jp-b", "JP", true)
	list := []string{"vmess://jp-a", "vmess://jp-b"}

	withTestConfig(t, &zenConfigData{Enabled: true, ExitMode: exitModeProxy, EnabledRegions: []string{"us"}})
	invalidateExitListCache()
	got := filterByExitRegion(list)
	if len(got) != 2 {
		t.Fatalf("无匹配出口时应回退完整列表(否则网关没有出口), got %v", got)
	}

	// 有匹配时正常过滤
	withTestConfig(t, &zenConfigData{Enabled: true, ExitMode: exitModeProxy, EnabledRegions: []string{"jp"}})
	invalidateExitListCache()
	if got := filterByExitRegion(list); len(got) != 2 {
		t.Fatalf("勾选日本应保留 2 个, got %v", got)
	}
}

func TestExitRegionSummaryCounts(t *testing.T) {
	setNodeHealthForTest(t, "vmess://us-ok", "US", true)
	setNodeHealthForTest(t, "vmess://us-fail", "US", false)
	setNodeHealthForTest(t, "vmess://de-ok", "DE", true)
	withTestConfig(t, &zenConfigData{
		Enabled:  true,
		ExitMode: exitModeProxy,
		Proxies:  []string{"vmess://us-ok", "vmess://us-fail", "vmess://de-ok", "socks5://127.0.0.1:1080"},
	})
	sum := map[string]map[string]any{}
	for _, row := range exitRegionSummary() {
		sum[row["id"].(string)] = row
	}
	if sum["us"]["total"] != 2 || sum["us"]["ok"] != 1 {
		t.Fatalf("美国统计应为 2 总 / 1 可用, got %v", sum["us"])
	}
	if sum["eu"]["total"] != 1 || sum["eu"]["ok"] != 1 {
		t.Fatalf("欧洲统计应为 1 总 / 1 可用, got %v", sum["eu"])
	}
	// 手填代理无国家 -> other
	if sum["other"]["total"] != 1 {
		t.Fatalf("其他地区应含手填代理, got %v", sum["other"])
	}
	// 7 个地区一个都不少
	if len(sum) != 7 {
		t.Fatalf("应固定返回 7 个地区, got %d", len(sum))
	}
}

func TestZenConfigUpdateRejectsUnknownRegion(t *testing.T) {
	withTestConfig(t, &zenConfigData{Enabled: true, ExitMode: exitModeProxy})
	body, _ := json.Marshal(map[string]any{"enabledRegions": []string{"us", "mars"}})
	req := httptest.NewRequest(http.MethodPost, "/admin/api/opencode/config/update", strings.NewReader(string(body)))
	w := httptest.NewRecorder()
	handleZenConfigUpdate(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("非法地区应 400, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "地区无效") {
		t.Fatalf("错误信息应说明地区无效, got %s", w.Body.String())
	}
}

func TestZenConfigUpdateSavesRegions(t *testing.T) {
	withTestConfig(t, &zenConfigData{Enabled: true, ExitMode: exitModeProxy})
	body, _ := json.Marshal(map[string]any{"enabledRegions": []string{"tw", "us", "tw"}})
	req := httptest.NewRequest(http.MethodPost, "/admin/api/opencode/config/update", strings.NewReader(string(body)))
	w := httptest.NewRecorder()
	handleZenConfigUpdate(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("保存应成功, got %d: %s", w.Code, w.Body.String())
	}
	cfg := getZenConfig()
	if !reflect.DeepEqual(cfg.EnabledRegions, []string{"us", "tw"}) {
		t.Fatalf("应去重并按界面顺序保存, got %v", cfg.EnabledRegions)
	}
	// 空数组 = 清除限制
	body2, _ := json.Marshal(map[string]any{"enabledRegions": []string{}})
	req2 := httptest.NewRequest(http.MethodPost, "/admin/api/opencode/config/update", strings.NewReader(string(body2)))
	w2 := httptest.NewRecorder()
	handleZenConfigUpdate(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("清除限制应成功, got %d", w2.Code)
	}
	if len(getZenConfig().EnabledRegions) != 0 {
		t.Fatalf("空数组应清除限制, got %v", getZenConfig().EnabledRegions)
	}
}
