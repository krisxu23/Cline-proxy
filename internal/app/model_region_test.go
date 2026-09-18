package app

import (
	"context"
	"testing"
)

// TestIsRegionErrorPhrasings 地区封锁的识别应覆盖与 error_rules.go 同源的
// 常见措辞(审查 R2-6/I4: 只认两条会让漏网的措辞落进通用 403, 拿不到
// "标记出口 + 换出口重试"的处理)。
func TestIsRegionErrorPhrasings(t *testing.T) {
	cases := []string{
		`{"type":"error","error":{"type":"RegionError","message":"region is not supported"}}`,
		`not available in your country`,
		`{"error":{"type":"unsupported_region"}}`,
		`not available in your region`,
		`geo-restricted`,
	}
	for _, c := range cases {
		if !isRegionError(c) {
			t.Fatalf("isRegionError(%q) = false, want true", c)
		}
	}
	for _, c := range []string{
		`{"error":{"code":"1010"}}`,
		`rate limit exceeded`,
		``,
	} {
		if isRegionError(c) {
			t.Fatalf("isRegionError(%q) = true, want false", c)
		}
	}
}

// 注册一个地区受限模型(不触发探测 goroutine, 直接置位以便测试选路)。
func markRegionModelForTest(t *testing.T, modelID string) {
	t.Helper()
	regionModelMu.Lock()
	regionModels[modelID] = true
	regionModelMu.Unlock()
	t.Cleanup(func() {
		regionModelMu.Lock()
		delete(regionModels, modelID)
		delete(regionNodeOK, modelID)
		regionModelMu.Unlock()
	})
}

func regionProxyConfig(t *testing.T) {
	t.Helper()
	withTestConfig(t, &zenConfigData{
		ExitMode: exitModeProxy,
		Proxies: []string{
			"http://127.0.0.1:19301",
			"http://127.0.0.1:19302",
			"http://127.0.0.1:19303",
		},
	})
}

// 未探测的节点必须参与候选: 没有数据不等于不可用,
// 否则刚启动/探测未完成时必然无候选、请求退回直连(大陆 IP 必然 403)。
func TestPickZenProxyForModelUnknownNodesParticipate(t *testing.T) {
	regionProxyConfig(t)
	markRegionModelForTest(t, "m-spark")

	p, idx := pickZenProxyForModel("m-spark")
	if p == "" {
		t.Fatal("没有探测数据时也必须选出节点, 不能退回直连")
	}
	if idx < 0 || idx > 2 {
		t.Fatalf("idx 越界: %d", idx)
	}
}

// 已探测确认"地区被拒"的节点要被跳过。
func TestPickZenProxyForModelSkipsProbedBad(t *testing.T) {
	regionProxyConfig(t)
	markRegionModelForTest(t, "m-spark")
	list := effectiveProxyList()
	for _, p := range list[:2] {
		setRegionNodeOK("m-spark", nodeLocalKey(p), false) // 前两个地区被拒
	}
	setRegionNodeOK("m-spark", nodeLocalKey(list[2]), true)

	for i := 0; i < 6; i++ {
		p, _ := pickZenProxyForModel("m-spark")
		if p == list[0] || p == list[1] {
			t.Fatalf("被拒节点 %s 不应再被选中", p)
		}
	}
}

// 全部节点都确认被拒: 退回常规轮询而不是直连 ——
// 对大陆禁售模型直连是 100% 失败, 任何一个节点都比它强。
func TestPickZenProxyForModelAllBadFallsBackToAnyNode(t *testing.T) {
	regionProxyConfig(t)
	markRegionModelForTest(t, "m-spark")
	for _, p := range effectiveProxyList() {
		setRegionNodeOK("m-spark", nodeLocalKey(p), false)
	}

	p, _ := pickZenProxyForModel("m-spark")
	if p == "" {
		t.Fatal("全部被拒时应退回常规轮询选出节点, 而不是退回必败的直连")
	}
}

// 探测结果分类: 只有 403+RegionError 算"地区被拒",
// 429/402/5xx 等说明请求已抵达且地区放行 —— 此前只认 200,
// 探测自己撞出的限流把整池节点误标为不可用(实测 0/144)。
func TestClassifyRegionProbe(t *testing.T) {
	regionBody := `{"error":{"type":"RegionError","message":"This model is not available in your country."}}`
	cases := []struct {
		name           string
		status         int
		body           string
		wantOK, wantKd bool
	}{
		{"200 成功", 200, `{"choices":[]}`, true, true},
		{"403 地区拒绝", 403, regionBody, false, true},
		{"403 FreeTier 出口拒绝(同构地区被拒)", 403, `{"type":"error","error":{"type":"FreeTierError","message":"Error from provider (Console): OpenCode's free tier can only be used from within OpenCode"}}`, false, true},
		{"429 限流(地区已放行)", 429, `{"error":{"message":"slow down"}}`, true, true},
		{"402 额度(地区已放行)", 402, `{"error":{"message":"quota"}}`, true, true},
		{"500 上游错误(地区已放行)", 500, `boom`, true, true},
		{"403 但不是地区错误", 403, `{"error":{"message":"bad key"}}`, true, true},
	}
	for _, c := range cases {
		ok, known := classifyRegionProbe(c.status, c.body)
		if ok != c.wantOK || known != c.wantKd {
			t.Errorf("%s: got (ok=%v,known=%v), want (%v,%v)", c.name, ok, known, c.wantOK, c.wantKd)
		}
	}
}

// reqExitKey: 拨号层记录的真实出口要能被反馈环读到。
func TestReqExitKeyRoundTrip(t *testing.T) {
	exit := &reqExit{}
	ctx := context.WithValue(context.Background(), ctxKeyReqExit, exit)
	setReqExit(ctx, "vless://node-a")
	if got := reqExitKey(ctx); got != "vless://node-a" {
		t.Fatalf("reqExitKey = %q", got)
	}
	setReqExit(ctx, "")
	if got := reqExitKey(ctx); got != "" {
		t.Fatalf("直连时 reqExitKey 应为空, 得到 %q", got)
	}
}

// isFreeTierError 识别 opencode 免费 tier 的出口风控拒绝。
// 实测(2026-09-17): 同一 mimo 走香港/大陆节点 200、美/法节点与直连 403,
// 与请求头无关 —— "within OpenCode" 按出口 IP 判定。
func TestIsFreeTierError(t *testing.T) {
	trueCases := []string{
		`{"type":"error","error":{"type":"FreeTierError","message":"Error from provider (Console): OpenCode's free tier can only be used from within OpenCode"}}`,
		`upstream said: can only be used from within OpenCode`,
		`FreeTierError: quota`,
	}
	for _, c := range trueCases {
		if !isFreeTierError(c) {
			t.Errorf("应识别为 FreeTierError: %s", c)
		}
	}
	falseCases := []string{
		`{"error":{"message":"rate limited"}}`,
		`{"type":"error","error":{"type":"RegionError","message":"region not supported"}}`,
		``,
		`{"error":{"message":"free tier ok"}}`, // 无特征词
	}
	for _, c := range falseCases {
		if isFreeTierError(c) {
			t.Errorf("不应识别为 FreeTierError: %s", c)
		}
	}
}

// FreeTierError 的 (模型,出口) 组合标记应被选路层消费: 被拒出口跳过。
func TestFreeTierExitMarkingSkipsNode(t *testing.T) {
	regionProxyConfig(t)
	markRegionModelForTest(t, "mimo-test")
	// 节点 A(http://127.0.0.1:19301) 被标记为该模型不可用
	setRegionNodeOK("mimo-test", "http://127.0.0.1:19301", false)
	for i := 0; i < 8; i++ {
		p, _ := pickZenProxyForModel("mimo-test")
		if p == "http://127.0.0.1:19301" {
			t.Fatalf("被 FreeTier 拒绝的出口不应再被选中(第 %d 次): %s", i, p)
		}
	}
}

// isRegionError 必须与 error_rules.go 的地区封锁规则**完全一致**(单一来源)。
//
// 收口前这里是第二份各自复制的短语表, 且缺 error_rules 的反误伤豁免 ——
// Cloudflare 1010 指纹拒绝的正文只要提到 "region" 就会被误判成地区封锁,
// 进而把一个只是"当前出口指纹被拒"的模型登记为地区受限并触发全节点探测。
func TestIsRegionErrorSharesRuleTable(t *testing.T) {
	// 真地区封锁: 命中
	trueCases := []string{
		`{"type":"error","error":{"type":"RegionError","message":"nope"}}`,
		`{"error":{"message":"Model is not available in your region"}}`,
		`{"error":{"message":"region not supported"}}`,
		`{"error":{"message":"unsupported_region"}}`,
		`{"error":{"message":"geo-restricted"}}`,
		`{"error":{"message":"not available in your country"}}`,
	}
	for _, c := range trueCases {
		if !isRegionError(c) {
			t.Errorf("应判为地区封锁: %s", c)
		}
	}

	// ★ 反误伤: CF-1010 指纹拒绝即便夹带地区文案也不得判为地区封锁
	// (豁免来自 error_rules.go 的地区规则, 这次收口后才生效)。
	vetoCases := []string{
		`{"error":{"message":"error code: 1010 region not supported"}}`,
		`{"error":{"message":"just a moment... region not supported"}}`,
		`{"error":{"message":"attention required — not available in your region"}}`,
	}
	for _, c := range vetoCases {
		if isRegionError(c) {
			t.Errorf("CF 1010/质询页应被豁免, 不得判为地区封锁: %s", c)
		}
	}

	// 不相关正文不命中
	for _, c := range []string{"", "rate limit exceeded", `{"error":{"message":"context window exceeded"}}`} {
		if isRegionError(c) {
			t.Errorf("不应判为地区封锁: %s", c)
		}
	}

	// 与规则表判定必须一致(单一来源断言): 逐例比对
	for _, c := range append(append([]string{}, trueCases...), vetoCases...) {
		class, _ := matchErrorRules(c)
		if got, want := isRegionError(c), class == classGeoBlocked; got != want {
			t.Errorf("isRegionError 与规则表分叉: body=%s got=%v want=%v", c, got, want)
		}
	}
}
