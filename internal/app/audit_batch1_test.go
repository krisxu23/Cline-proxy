package app

// 2026-09-24 审查批次 1(数据正确性)的回归测试。

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ★ 核心回归: 大文件补载必须读**尾部**窗口, 不是"距文件头 64KB 处"的中段。
//
// 原实现 Seek(reqLogTailBytes, io.SeekStart): 当日日志一超过 64KB, 冷启动补载
// 拿到的就是一段陈旧的中段内容(注释却承诺"尾部窗口")。
func TestLoadDailyRequestLogsReadsTailNotMiddle(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "requests-20260924.jsonl")

	// 每行约 200B × 2000 行 ≈ 400KB, 远超 64KB 尾读窗口 —— 这样"尾部"与
	// "中段"完全不相交, 能干净地区分两种实现。
	const lines = 2000
	var sb strings.Builder
	pad := strings.Repeat("x", 120)
	for i := 0; i < lines; i++ {
		rec := RequestLog{
			Time:  time.Unix(1700000000+int64(i), 0).UTC(),
			Path:  fmt.Sprintf("/fill-%04d-%s", i, pad),
			Route: "zen", Status: 200,
		}
		if i == lines-1 {
			rec.Path = "/last-marker" // 只有真正读到尾部才会出现
		}
		b, err := json.Marshal(rec)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		sb.Write(b)
		sb.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Size() <= reqLogTailBytes {
		t.Fatalf("测试前提不成立: 文件需 > %d 字节, 实际 %d", reqLogTailBytes, fi.Size())
	}

	got := loadDailyRequestLogs(path)
	if len(got) == 0 {
		t.Fatal("应读到尾部记录")
	}
	if last := got[len(got)-1]; last.Path != "/last-marker" {
		t.Fatalf("应读到文件**尾部**的最后一条, 实际是 %q —— 说明读的是中段", last.Path)
	}
}

// 池快照落盘: 迟到的旧快照不得覆盖已落盘的新快照。
func TestWritePoolSnapshotDropsStaleSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pool.json")

	// 先落"新"的(seq=2), 再让"旧"的(seq=1)迟到。
	writePoolSnapshot(path, []byte(`{"marker":"new"}`), 2)
	writePoolSnapshot(path, []byte(`{"marker":"old"}`), 1)
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(got), "new") || strings.Contains(string(got), "old") {
		t.Fatalf("迟到的旧快照覆盖了新快照: %s", got)
	}

	// 更新的快照必须能写进去 —— 否则序号机制会把后续写入全卡死。
	writePoolSnapshot(path, []byte(`{"marker":"newest"}`), 3)
	got, _ = os.ReadFile(path)
	if !strings.Contains(string(got), "newest") {
		t.Fatalf("更新的快照应能落盘: %s", got)
	}
}

// 每节点流量表按 active 集合裁剪; 空集合(实例未就绪的中间态)不得清空整表。
func TestPruneNodeTrafficKeepsActiveOnly(t *testing.T) {
	nodeTrafficMu.Lock()
	prev := nodeTraffic
	nodeTraffic = map[string]*nodeTrafficCounter{"a": {}, "b": {}, "c": {}}
	nodeTrafficMu.Unlock()
	t.Cleanup(func() {
		nodeTrafficMu.Lock()
		nodeTraffic = prev
		nodeTrafficMu.Unlock()
	})

	if n := pruneNodeTraffic([]string{"a", "c"}); n != 1 {
		t.Fatalf("应裁掉 1 条, got %d", n)
	}
	nodeTrafficMu.Lock()
	_, hasB := nodeTraffic["b"]
	left := len(nodeTraffic)
	nodeTrafficMu.Unlock()
	if hasB || left != 2 {
		t.Fatalf("只应保留 a/c, got %d 条 (b 还在=%v)", left, hasB)
	}

	if n := pruneNodeTraffic(nil); n != 0 {
		t.Fatalf("空集合不应裁剪, got %d", n)
	}
	nodeTrafficMu.Lock()
	left = len(nodeTraffic)
	nodeTrafficMu.Unlock()
	if left != 2 {
		t.Fatalf("空集合不得改动表, got %d 条", left)
	}
}

// 坏配置留证只保留最近 N 份(原实现只改名不清理, 会无限堆积)。
func TestPruneBadConfigsKeepsNewest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zen-config.json")
	var newest string
	for i := 0; i < 8; i++ {
		name := fmt.Sprintf("%s.bad-2026092%d-12000%d", path, i, i)
		if err := os.WriteFile(name, []byte("x"), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		newest = name // 时间戳递增, 最后一个最新
	}

	pruneBadConfigs(path, 5)

	left, err := filepath.Glob(path + ".bad-*")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(left) != 5 {
		t.Fatalf("应保留 5 份, got %d: %v", len(left), left)
	}
	found := false
	for _, l := range left {
		if l == newest {
			found = true
		}
	}
	if !found {
		t.Fatalf("最新留证必须保留, got %v", left)
	}
}
