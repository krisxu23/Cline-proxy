package app

// 订阅过滤 / 流量历史 / 控制字符清洗 / 非 SSE 合成 / 空闲中断 / 早断探测 的测试。

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"
)

func TestNodeExcludedByFilter(t *testing.T) {
	cfg := getZenConfig()
	defer setZenConfig(cfg)

	next := cfg.clone()
	next.NodeExcludeKeywords = []string{"官网", "Expired", " 剩余流量 "}
	setZenConfig(next)

	cases := map[string]bool{
		"🇭🇰 香港 官网节点":    true,
		"JP-01 expired": true,
		"US 剩余流量 10G":   true,
		"🇸🇬 SG-premium": false,
	}
	for name, want := range cases {
		if got := nodeExcludedByFilter(name); got != want {
			t.Fatalf("nodeExcludedByFilter(%q)=%v, want %v", name, got, want)
		}
	}
	next = getZenConfig().clone()
	next.NodeExcludeKeywords = nil
	setZenConfig(next)
	if nodeExcludedByFilter("任意 节点") {
		t.Fatal("无关键词时不应过滤")
	}
}

func TestNodeTrafficHistoryDifferential(t *testing.T) {
	nodeTrafficReset()
	defer nodeTrafficReset()
	startTrafficSampler()
	time.Sleep(10 * time.Millisecond)

	hist := []trafficSample{
		{TS: time.Now().UnixMilli() - 2000, TotalUp: 0, TotalDown: 0},
		{TS: time.Now().UnixMilli() - 1000, TotalUp: 1000, TotalDown: 2000},
		{TS: time.Now().UnixMilli(), TotalUp: 1000, TotalDown: 2500},
	}
	trafficHistMu.Lock()
	trafficHistory = hist
	trafficHistMu.Unlock()

	series := nodeTrafficHistory()
	if len(series) != 2 {
		t.Fatalf("差分应产生 2 个速率点, got %d", len(series))
	}
	if series[0]["bps"].(int64) != 500 {
		t.Fatalf("最近段速率应为 500, got %v", series[0]["bps"])
	}
	if series[1]["bps"].(int64) != 3000 {
		t.Fatalf("前段速率应为 3000, got %v", series[1]["bps"])
	}
}

func TestControlSanitizingReaderAndHelper(t *testing.T) {
	// 字符串内的裸控制字符(含 TAB)非法 → 清洗为空格后可解析;
	// 字符串外的 \t \n \r 是合法 JSON 空白, 必须保留。
	raw := "{\"a\":\"AAA\x01BBB\x0bCCC\",\n\t\"b\":\"x\ty\"}"
	clean, dirty := sanitizeJSONControlChars([]byte(raw))
	if !dirty {
		t.Fatal("应检出字符串内的裸控制字符")
	}
	var obj map[string]any
	if err := json.Unmarshal(clean, &obj); err != nil {
		t.Fatalf("清洗后应可解析: %v", err)
	}
	if obj["a"] != "AAA BBB CCC" {
		t.Fatalf("字符串内控制字符应被替换为空格, got %v", obj["a"])
	}
	if obj["b"] != "x y" {
		t.Fatalf("字符串内 TAB 也应被替换(JSON 不允许), got %v", obj["b"])
	}

	// 读取层清洗器: 分块读取也要保持字符串状态正确
	src := &controlSanitizingReader{src: strings.NewReader(raw)}
	buf := make([]byte, 8)
	var got []byte
	for {
		n, err := src.Read(buf)
		got = append(got, buf[:n]...)
		if err != nil {
			break
		}
	}
	if strings.ContainsRune(string(got), '\x01') || strings.ContainsRune(string(got), '\x0b') {
		t.Fatalf("读取层应清洗掉裸控制字符: %q", string(got))
	}
	if err := json.Unmarshal(got, &obj); err != nil {
		t.Fatalf("读取层清洗后应可解析: %v", err)
	}

	if _, dirty := sanitizeJSONControlChars([]byte(`{"a":"b\tc"}`)); dirty {
		t.Fatal("完全合法的输入不应报告改动")
	}
}

func TestSynthesizeSSEFromJSONBody(t *testing.T) {
	body := []byte(`{"id":"chatcmpl-x","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hello","reasoning_content":"think"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`)
	sse, ok := synthesizeOpenAISSEFromJSON(body)
	if !ok {
		t.Fatal("完整 JSON body 应能合成 SSE")
	}
	out := string(sse)
	for _, want := range []string{"data: ", "chat.completion.chunk", "hello", "think", "\"finish_reason\":\"stop\"", "data: [DONE]"} {
		if !strings.Contains(out, want) {
			t.Fatalf("合成 SSE 缺少 %q:\n%s", want, out)
		}
	}
	if _, ok := synthesizeOpenAISSEFromJSON([]byte(`{"error":"boom"}`)); ok {
		t.Fatal("错误 body 不应合成 SSE")
	}
	if frame, ok := ndjsonLineToSSE(`{"choices":[{"delta":{"content":"a"}}]}`); !ok || !strings.Contains(string(frame), "data: ") {
		t.Fatal("NDJSON 行应转成 data: 帧")
	}
	if _, ok := ndjsonLineToSSE("not json"); ok {
		t.Fatal("非 JSON 行不应转换")
	}
	if !looksLikeJSONBody(`{"a":1}`) || looksLikeJSONBody("data: {}") || looksLikeJSONBody(": comment") {
		t.Fatal("形态判定不符合预期")
	}
}

func TestIdleAbortReaderAbortsOnStall(t *testing.T) {
	pr, pw := io.Pipe()
	r := newIdleAbortReader(pr, 150*time.Millisecond)
	defer r.Close()

	go func() {
		pw.Write([]byte("data: {}\n\n"))
	}()

	buf := make([]byte, 64)
	n, err := r.Read(buf)
	if err != nil || n == 0 {
		t.Fatalf("首次读取应成功: n=%d err=%v", n, err)
	}
	start := time.Now()
	_, err = r.Read(buf)
	if err == nil {
		t.Fatal("静默超时应返回错误")
	}
	if el := time.Since(start); el < 100*time.Millisecond || el > 2*time.Second {
		t.Fatalf("中断时机异常: %v", el)
	}
	pw.Close()
	if err := r.Close(); err != nil {
		t.Fatalf("Close 应幂等: %v", err)
	}
}

func TestIdleAbortReaderPassesThroughActiveStream(t *testing.T) {
	pr, pw := io.Pipe()
	r := newIdleAbortReader(pr, 2*time.Second)
	defer r.Close()
	go func() {
		for i := 0; i < 3; i++ {
			pw.Write([]byte("x"))
			time.Sleep(20 * time.Millisecond)
		}
		pw.Close()
	}()
	got := 0
	buf := make([]byte, 8)
	for {
		n, err := r.Read(buf)
		got += n
		if err != nil {
			break
		}
	}
	if got != 3 {
		t.Fatalf("活跃流不应被中断, 读到 %d 字节", got)
	}
}

func TestProbeStreamFirstEvent(t *testing.T) {
	// 空流(200 后立即 EOF) → 判失败换站
	if empty, _ := probeStreamFirstEvent(io.NopCloser(strings.NewReader(""))); !empty {
		t.Fatal("空流应判为 early EOF")
	}
	// 只有 [DONE] 无内容 → 同样判空流
	if empty, _ := probeStreamFirstEvent(io.NopCloser(strings.NewReader("data: [DONE]\n\n"))); !empty {
		t.Fatal("仅 [DONE] 应判为空流")
	}
	// 正常首事件 → 放行, 且已消费字节必须能原样读回(不丢数据)
	raw := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"
	empty, nb := probeStreamFirstEvent(io.NopCloser(strings.NewReader(raw)))
	if empty {
		t.Fatal("正常首事件不应判空流")
	}
	got, _ := io.ReadAll(nb)
	nb.Close()
	if string(got) != raw {
		t.Fatalf("接回的响应体必须与原文一致:\n got=%q\nwant=%q", string(got), raw)
	}
}
