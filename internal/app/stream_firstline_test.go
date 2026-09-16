package app

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// R2 审计 F2 回归: 首行 SSE 与主循环收敛为同一实现后, 首行必须同样
// 走 normalize / onUsage / sawDone 更新。

func streamUpstream(body string) *http.Response {
	return &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestStreamFirstLineDoneNoDuplicateDone(t *testing.T) {
	mw := &mockFlushWriter{}
	// 极短流: 首行即 [DONE]。旧首行分支不更新 sawDone, 收尾会再补一个
	// [DONE] —— 客户端收到重复终止帧。
	//
	// 注意(2026-09-16 空流防护引入后的行为变化): 上游什么都没发只给 [DONE],
	// 在 OmniRoute 语义下 (rejectEmptyChoicesStream: 既无有价值的 chunk,
	// 也无有效 usage) 就是**空流**, 必须判成可见失败而不是干净的收尾。
	// 因此这条流现在以 empty_content 错误帧结尾 —— 它自身带 [DONE],
	// 与网关的错误帧 [DONE] 合计会出现两个 [DONE], 这是"失败可见"的
	// 必然结果, 不再是缺陷。本用例改为锁住真正的不变量:
	//   1. 正常收尾路径不得重复补 [DONE](即不含错误帧时只有一个);
	//   2. 若判为空流, 必须带 empty_content 错误帧(失败不得静默)。
	handleStreamResponseWithUsage(mw, streamUpstream("data: [DONE]\n\n"), nil)
	out := mw.String()

	if strings.Contains(out, "empty_content") {
		// 判空流: 必须显式失败, 且不得只剩一个光秃秃的 [DONE]
		if n := strings.Count(out, "[DONE]"); n < 1 {
			t.Fatalf("判空流后仍应交付终止帧, got %q", out)
		}
		return
	}
	// 未判空流: 严格锁住"只补一次 [DONE]"
	if n := strings.Count(out, "[DONE]"); n != 1 {
		t.Fatalf("首行即 [DONE] 时全流应只出现一个 [DONE], got %d in %q", n, out)
	}
}

// 空流防护的对照用例: 同样走首行路径, 但上游给了真实内容 ——
// 不得判空, 且 [DONE] 只出现一次(收尾不得重复补)。
func TestStreamFirstLineDoneNoDuplicateDone_有内容(t *testing.T) {
	mw := &mockFlushWriter{}
	body := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"
	handleStreamResponseWithUsage(mw, streamUpstream(body), nil)
	out := mw.String()

	if strings.Contains(out, "empty_content") {
		t.Fatalf("有真实内容的流不得判空, 实得 %q", out)
	}
	if n := strings.Count(out, "[DONE]"); n != 1 {
		t.Fatalf("有内容时 [DONE] 应只出现一次, got %d in %q", n, out)
	}
}

func TestStreamFirstLineUsageAndNormalize(t *testing.T) {
	mw := &mockFlushWriter{}
	var usageSeen map[string]any
	onUsage := func(u map[string]any) { usageSeen = u }
	// 首行带 usage(Cline 包裹形态 {data:{...}} —— 真实形态内层含 id/choices):
	// 旧首行分支既不记 usage 也不解包裹, 会把脏形态原样漏给客户端。
	first := `data: {"data":{"id":"chatcmpl-1","model":"gpt-4o","usage":{"prompt_tokens":7,"completion_tokens":3},"choices":[]}}` + "\n\n"
	rest := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"
	handleStreamResponseWithUsage(mw, streamUpstream(first+rest), onUsage)
	if usageSeen == nil {
		t.Fatalf("首行 usage 必须被记录, out=%q", mw.String())
	}
	if usageSeen["prompt_tokens"] != float64(7) {
		t.Fatalf("usage 内容不对: %v", usageSeen)
	}
	out := mw.String()
	if strings.Contains(out, `{"data":`) {
		t.Fatalf("Cline {data:{...}} 包裹形态必须被 normalize 解开后再写客户端: %q", out)
	}
	if !strings.Contains(out, `"model":"gpt-4o"`) {
		t.Fatalf("首行 model 应进入输出帧: %q", out)
	}
}

func TestStreamFirstLineBadLineLoggedNotSwallowed(t *testing.T) {
	// 首行坏 JSON: 统一实现会走坏行门卫(丢弃并打日志), 后续正常帧不受影响,
	// 收尾仍补齐 finish chunk + [DONE]。
	mw := &mockFlushWriter{}
	body := "data: {\"choices\"0}]\n\ndata: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}],\"finish_reason\":\"stop\"}\n\ndata: [DONE]\n\n"
	handleStreamResponseWithUsage(mw, streamUpstream(body), nil)
	out := mw.String()
	if strings.Contains(out, `{"choices"0}]`) {
		t.Fatalf("坏行不得透传给客户端: %q", out)
	}
	if !strings.Contains(out, "ok") || !strings.Contains(out, "[DONE]") {
		t.Fatalf("坏行后的正常内容必须保留: %q", out)
	}
}
