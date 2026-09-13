package app

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cline-go-proxy/internal/kit"
)

// TestInitStatsRetry 验证 stats 文件打开失败后不会永久降级:
// (a) 指向不可写位置(一个已存在的目录)调用 initStats 不 panic;
// (b) 失败被 log.Printf 记录, 且 statsFile 仍为 nil(未打开);
// (c) 之后指向可写路径调用 initStats 能恢复工作(非一次失败永久失效)。
func TestInitStatsRetry(t *testing.T) {
	dir := t.TempDir() // 目录路径不能作为普通文件打开 -> 必然失败

	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	// 复位包级状态, 使本测试可独立、可重复运行。
	statsInitMu.Lock()
	if statsFile != nil {
		statsFile.Close()
		statsFile = nil
	}
	statsRollStarted = false
	statsToday = nil
	statsTotal = nil
	statsInitMu.Unlock()
	t.Cleanup(func() {
		// 关闭 stats 文件句柄, 否则 TempDir 清理时会因文件被占用而失败。
		statsFileMu.Lock()
		if statsFile != nil {
			statsFile.Close()
			statsFile = nil
		}
		statsFileMu.Unlock()
		statsFilePath = kit.ResolveDataPath("zen-stats.jsonl")
	})

	// (a) 不可写路径: 不 panic
	statsFilePath = dir
	initStats()

	// (b) 失败被记录, 且文件未打开
	if !strings.Contains(buf.String(), "open failed") {
		t.Fatalf("expected failure to be logged, got: %q", buf.String())
	}
	if statsFile != nil {
		t.Fatalf("statsFile should stay nil after failed open")
	}

	// (c) 改为可写路径后恢复
	writable := filepath.Join(dir, "zen-stats.jsonl")
	statsFilePath = writable
	initStats()
	if statsFile == nil {
		t.Fatalf("statsFile should recover after pointing to a writable path")
	}

	// 记录一条并验证聚合对象可用(面板不再恒为 null)。
	recordZenStats(zenStatsRecord{
		TS:       time.Now().UnixMilli(),
		Upstream: "zen",
		Model:    "gpt-4",
		OK:       true,
		Status:   200,
	})
	snap := zenStatsSnapshot()
	if snap["total"] == nil || snap["today"] == nil {
		t.Fatalf("snapshot should not be nil after recovery: %v", snap)
	}
}
