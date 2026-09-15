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
	handleStreamResponseWithUsage(mw, streamUpstream("data: [DONE]\n\n"), nil)
	out := mw.String()
	if n := strings.Count(out, "[DONE]"); n != 1 {
		t.Fatalf("首行即 [DONE] 时全流应只出现一个 [DONE], got %d in %q", n, out)
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
