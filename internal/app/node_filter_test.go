package app

// 订阅过滤管道与流量历史采样测试。

import (
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
