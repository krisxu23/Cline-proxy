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
	// 极短流: 首行即 [DONE]。
	//
	// 注意(2026-09-24 契约变更, 用户规格: 上游的错误必须在网关内部消化):
	// 上游什么都没发只给 [DONE] 就是空流。现在空流在响应头**提交前**一个字节都
	// 不交付, 而是返回真 502 让路由层换站重试 —— 客户端看到的是"这次请求失败、
	// 可以重试", 而不是一条带自定义错误帧的空流(agent 工具认不出那种帧, 这正是
	// 用户报的"空白回复卡死")。
	// 于是"重复 [DONE]"这条旧缺陷在这个形态下已不可观测, 用例改为锁住新契约;
	// "全流只出现一个 [DONE]"由下面的有内容对照用例继续锁定。
	status := handleStreamResponseWithUsage(mw, streamUpstream("data: [DONE]\n\n"), nil)
	if status != http.StatusBadGateway {
		t.Fatalf("首行即 [DONE](零内容)必须判空并返回 502, 实得 status=%d", status)
	}
	if out := mw.String(); out != "" {
		t.Fatalf("空流在响应头提交前不得交付任何字节, 实得 %q", out)
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
