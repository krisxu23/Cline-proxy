package app

import (
	"context"
	"testing"
)

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
