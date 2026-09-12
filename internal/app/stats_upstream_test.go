package app

import "testing"

// 统计必须覆盖全部四类上游, 并且能按上游与按模型两个维度分组 ——
// 面板上的「总 token」只有在每一路都记账时才是真的总量。
func TestStatsAggregateByUpstreamAndModel(t *testing.T) {
	agg := newZenStatsAgg()
	recs := []zenStatsRecord{
		{Upstream: upstreamZen, Model: "mimo-2.5-free", PromptTokens: 100, CompletionTokens: 20},
		{Upstream: upstreamZen, Model: "mimo-2.5-free", PromptTokens: 50, CompletionTokens: 10},
		{Upstream: upstreamCline, Model: "deepseek/deepseek-v4-flash", PromptTokens: 7, CompletionTokens: 3},
		{Upstream: upstreamClinePass, Model: "cline-pass/gpt-4o", PromptTokens: 5, CompletionTokens: 1},
		{Upstream: providerUpstream("gemini"), Model: "gemini:gemini-3.8-flash", PromptTokens: 11, CompletionTokens: 4},
		{Upstream: providerUpstream("bai"), Model: "bai:glm-5.3-flash", PromptTokens: 2, CompletionTokens: 1},
		// 早期记录没有 upstream 字段: 归到 zen, 不产生空键行
		{Model: "legacy", PromptTokens: 1, CompletionTokens: 1},
	}
	for i := range recs {
		aggregateRecord(agg, &recs[i])
	}

	if agg.Requests != 7 {
		t.Fatalf("requests = %d, want 7", agg.Requests)
	}
	if got := agg.PromptTok + agg.CompleteTok; got != 100+20+50+10+7+3+5+1+11+4+2+1+1+1 {
		t.Fatalf("total tokens = %d", got)
	}
	want := map[string]int64{
		upstreamZen:                3,
		upstreamCline:              1,
		upstreamClinePass:          1,
		providerUpstream("gemini"): 1,
		providerUpstream("bai"):    1,
	}
	if len(agg.ByUpstream) != len(want) {
		t.Fatalf("byUpstream keys = %v", agg.ByUpstream)
	}
	for k, n := range want {
		e := agg.ByUpstream[k]
		if e == nil {
			t.Fatalf("missing upstream %q in %v", k, agg.ByUpstream)
		}
		if e.Requests != n {
			t.Errorf("upstream %q requests = %d, want %d", k, e.Requests, n)
		}
	}
	if agg.ByUpstream[""] != nil {
		t.Fatal("records without an upstream must not create an empty key")
	}
	if m := agg.ByModel["mimo-2.5-free"]; m == nil || m.Requests != 2 || m.PromptTok != 150 || m.CompleteTok != 30 {
		t.Fatalf("byModel aggregation: %+v", m)
	}
	// 按上游的 token 之和必须等于总量, 否则「合计」框与分项框对不上
	var sumPT, sumCT int64
	for _, e := range agg.ByUpstream {
		sumPT += e.PromptTok
		sumCT += e.CompleteTok
	}
	if sumPT != agg.PromptTok || sumCT != agg.CompleteTok {
		t.Fatalf("byUpstream totals (%d/%d) must equal the grand totals (%d/%d)",
			sumPT, sumCT, agg.PromptTok, agg.CompleteTok)
	}
}

// 上游返回 usage 时以 usage 为准; 上游不给时保留入站估算, 不能归零。
func TestStatsObserveUsage(t *testing.T) {
	tr := newZenStatsTracker(zenStatsRecord{Upstream: upstreamProvider, Model: "x", PromptTokens: 999})
	tr.observeUsage(map[string]any{"prompt_tokens": float64(120), "completion_tokens": float64(30)})
	if tr.rec.PromptTokens != 120 || tr.rec.CompletionTokens != 30 {
		t.Fatalf("usage must override the estimate: %+v", tr.rec)
	}
	// 空 usage: 保持估算值
	tr.observeUsage(nil)
	if tr.rec.PromptTokens != 120 {
		t.Fatalf("nil usage must not clobber the record: %+v", tr.rec)
	}
}
