package app

// 订阅过滤管道 / 流量历史采样 / 控制字符清洗 的测试。

import (
	"encoding/json"
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
		"🇭🇰 香港 官网节点":    true,  // 命中"官网"
		"JP-01 expired": true,  // 大小写不敏感
		"US 剩余流量 10G":   true,  // 带空格的关键词也 trim 后命中
		"🇸🇬 SG-premium": false, // 不命中
	}
	for name, want := range cases {
		if got := nodeExcludedByFilter(name); got != want {
			t.Fatalf("nodeExcludedByFilter(%q)=%v, want %v", name, got, want)
		}
	}
	// 无关键词: 全部放行
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

	// 直接注入采样序列(绕过 1 分钟等待): 三个采样点, 第二段产生速率
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
	// 最近在前: 1000ms 内 down 增 500 → 500 B/s
	if series[0]["bps"].(int64) != 500 {
		t.Fatalf("最近段速率应为 500, got %v", series[0]["bps"])
	}
	// 第一段 up+down 增 3000 → 3000 B/s
	if series[1]["bps"].(int64) != 3000 {
		t.Fatalf("前段速率应为 3000, got %v", series[1]["bps"])
	}
}

func TestControlSanitizingReaderAndHelper(t *testing.T) {
	// 字符串内的裸控制字符(含 TAB)非法 → 清洗为空格后必须可解析;
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
	// 字符串外的换行/制表符保留(它们是合法空白)
	if !strings.Contains(string(clean), "CCC\",\n\t\"b\"") {
		t.Fatalf("字符串外 \\n \\t 不应被破坏: %q", string(clean))
	}

	// 读取层清洗器: 分块读取(8 字节一块)也要保持字符串状态正确
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

	// 干净输入不应被改动
	if _, dirty := sanitizeJSONControlChars([]byte(`{"a":"b\tc"}`)); dirty {
		t.Fatal("完全合法的输入不应报告改动")
	}
}

func TestSynthesizeSSEFromJSONBody(t *testing.T) {
	// 上游忽略 stream:true 直接回完整 JSON body(实测 B.AI 图片响应形态):
	// 必须能合成为标准 SSE 流并保留 content / reasoning_content / finish_reason
	body := []byte(`{"id":"chatcmpl-x","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hello","reasoning_content":"think"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`)
	sse, ok := synthesizeOpenAISSEFromJSON(body)
	if !ok {
		t.Fatal("完整 JSON body 应能合成 SSE")
	}
	out := string(sse)
	for _, want := range []string{"data: ", "chat.completion.chunk", "hello", "think", "\"finish_reason\":\"stop\"", "data: [DONE]"} {
		if !strings.Contains(out, want) {
			t.Fatalf("合成 SSE 缺少 %q://n%s", want, out)
		}
	}
	// 非 chat-completion 形状不应合成
	if _, ok := synthesizeOpenAISSEFromJSON([]byte(`{"error":"boom"}`)); ok {
		t.Fatal("错误 body 不应合成 SSE")
	}
	// NDJSON 单行转换
	if frame, ok := ndjsonLineToSSE(`{"choices":[{"delta":{"content":"a"}}]}`); !ok || !strings.Contains(string(frame), "data: ") {
		t.Fatal("NDJSON 行应转成 data: 帧")
	}
	if _, ok := ndjsonLineToSSE("not json"); ok {
		t.Fatal("非 JSON 行不应转换")
	}
	// 形态判定
	if !looksLikeJSONBody(`{"a":1}`) || looksLikeJSONBody("data: {}") || looksLikeJSONBody(": comment") {
		t.Fatal("形态判定不符合预期")
	}
}
