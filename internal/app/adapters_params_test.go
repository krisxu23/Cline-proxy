package app

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"free-router/internal/providers"
)

// P2-9: 接口写的是 io.Writer, 就必须真的支持任意 io.Writer。
// 旧实现遇到非 ResponseWriter 直接报错(接口说谎), 而 clinepass 链式转发路径
// 拿到的正是 *io.PipeWriter, 只能吃下这个事实。

func TestStreamResponseWriterPassesThroughRealResponseWriter(t *testing.T) {
	rec := httptest.NewRecorder()
	got := streamResponseWriter(rec)
	if _, ok := got.(*httptest.ResponseRecorder); !ok {
		t.Fatalf("已具备 Flush 的 ResponseWriter 必须原样返回, got %T", got)
	}
}

func TestStreamResponseWriterWrapsPlainWriter(t *testing.T) {
	var buf bytes.Buffer
	w := streamResponseWriter(&buf)
	if _, ok := w.(http.Flusher); !ok {
		t.Fatal("包装后必须实现 http.Flusher —— 中继靠这个断言决定是否继续转发")
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write([]byte("data: {}\n\n")); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if buf.String() != "data: {}\n\n" {
		t.Fatalf("字节必须直达底层 writer, got %q", buf.String())
	}
	w.(http.Flusher).Flush() // 底层是 bytes.Buffer: 应为 no-op 而非 panic
}

func TestStreamResponseWriterAddsFlushWhenMissing(t *testing.T) {
	w := streamResponseWriter(noFlushWriter{})
	if _, ok := w.(http.Flusher); !ok {
		t.Fatal("是 ResponseWriter 但缺 Flush 时, 必须补一个 no-op Flush")
	}
}

type noFlushWriter struct{}

func (noFlushWriter) Header() http.Header         { return http.Header{} }
func (noFlushWriter) Write(p []byte) (int, error) { return len(p), nil }
func (noFlushWriter) WriteHeader(int)             {}

// 用管道验证真实场景: *io.PipeWriter 不是 ResponseWriter, 但流必须能透出去。
func TestStreamResponseWriterOverPipe(t *testing.T) {
	pr, pw := io.Pipe()
	w := streamResponseWriter(pw)
	go func() {
		_, _ = w.Write([]byte("chunk"))
		_ = pw.Close()
	}()
	data, err := io.ReadAll(pr)
	if err != nil {
		t.Fatalf("读管道失败: %v", err)
	}
	if string(data) != "chunk" {
		t.Fatalf("管道内容不对: %q", data)
	}
}

// P2-7: params 往返保真 —— Extra(client 原始 params)为基底, 未被建模的字段
// 不得丢失, model/stream 以强类型字段为准。

func TestChatRequestToParamsKeepsExtraFields(t *testing.T) {
	msgs := []any{map[string]any{"role": "user", "content": "hi"}}
	extra := map[string]any{
		"messages":         msgs,
		"tool_choice":      "auto",
		"reasoning_effort": "high",
		"max_tokens":       float64(100.7), // 客户端原值不被 int 截断
		"temperature":      float64(0),     // 显式 0 必须保留
	}
	got := chatRequestToParams(providers.ChatRequest{
		Model: "deepseek/x", Stream: true, Extra: extra,
	})
	for k, want := range extra {
		if got[k] == nil {
			t.Fatalf("字段 %s 在往返中丢失", k)
		}
		_ = want
	}
	if got["tool_choice"] != "auto" || got["reasoning_effort"] != "high" {
		t.Fatalf("未建模字段必须原样保留: %v", got)
	}
	if got["max_tokens"] != float64(100.7) {
		t.Fatalf("数值不得被截断, got %v", got["max_tokens"])
	}
	if got["temperature"] != float64(0) {
		t.Fatalf("显式 0 不得被丢弃, got %v", got["temperature"])
	}
	if msgsOut, ok := got["messages"].([]any); !ok || len(msgsOut) != 1 {
		t.Fatalf("messages 必须原样保留: %v", got["messages"])
	}
}

func TestChatRequestToParamsModelAndStreamWin(t *testing.T) {
	got := chatRequestToParams(providers.ChatRequest{
		Model: "resolved-model", Stream: true,
		Extra: map[string]any{"model": "raw/with-prefix", "stream": false},
	})
	if got["model"] != "resolved-model" {
		t.Fatalf("model 必须以调用方决策为准, got %v", got["model"])
	}
	if got["stream"] != true {
		t.Fatalf("stream 必须以强类型字段为准, got %v", got["stream"])
	}
}

func TestChatRequestToParamsFallsBackToTypedFields(t *testing.T) {
	got := chatRequestToParams(providers.ChatRequest{
		Model: "m", Stream: false, MaxTokens: 128, Temperature: 0.5, TopP: 0.9,
		Messages: []byte(`[{"role":"user","content":"x"}]`),
	})
	if got["max_tokens"] != float64(128) || got["temperature"] != 0.5 || got["top_p"] != 0.9 {
		t.Fatalf("无 Extra 时应由强类型字段补齐: %v", got)
	}
	if msgs, ok := got["messages"].([]any); !ok || len(msgs) != 1 {
		t.Fatalf("messages 应可由 RawMessage 解出: %v", got["messages"])
	}
}

func TestChatRequestToParamsDoesNotInjectMaxTokensWhenCompletionVariantPresent(t *testing.T) {
	// 客户端用 max_completion_tokens 表达上限时, 不应再补一个 max_tokens
	//(同时带两个字段会让部分上游按更小值截断或直接 400)。
	got := chatRequestToParams(providers.ChatRequest{
		Model: "m", Extra: map[string]any{"max_completion_tokens": float64(64)},
		MaxTokens: 128,
	})
	if _, has := got["max_tokens"]; has {
		t.Fatalf("已存在 max_completion_tokens 时不得注入 max_tokens: %v", got)
	}
}

func TestChatRequestToParamsHandlesNilExtra(t *testing.T) {
	got := chatRequestToParams(providers.ChatRequest{Model: "m"})
	if got["model"] != "m" || got["stream"] != false {
		t.Fatalf("nil Extra 也要产出可用形状: %v", got)
	}
	if strings.Contains(toString(got), "<nil>") {
		t.Fatalf("不应出现 nil 值: %v", got)
	}
}

func toString(m map[string]any) string {
	var sb strings.Builder
	for k, v := range m {
		sb.WriteString(k + "=" + strings.TrimSpace(fmt.Sprint(v)) + ";")
	}
	return sb.String()
}
