package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cline-go-proxy/internal/kit"
)

// TestAppendReqLogConcurrent 验证并发写入的正确性:
// 200 个 goroutine 各写 25 条, 共 5000 条。flushReqLogs 后读回文件,
// 要求每一行都是完整可解析的 JSON(无半行/交错行), 且 写入行数 + 丢弃数 == 5000。
func TestAppendReqLogConcurrent(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "requests.jsonl")
	reqLogsFile = tmp
	t.Cleanup(func() {
		closeReqLogs() // 释放写协程持有的文件句柄, 否则 TempDir 清理时会因文件被占用而失败
		reqLogsFile = kit.ResolveDataPath("requests.jsonl")
	})

	var wg sync.WaitGroup
	const goroutines = 200
	const perWorker = 25
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < perWorker; j++ {
				AppendReqLog(RequestLog{
					Time:   time.Now(),
					Method: "POST",
					Path:   "/v1/chat/completions",
					Model:  "zen/gpt-4",
					Route:  "zen",
					Status: 200,
				})
			}
		}()
	}
	wg.Wait()
	flushReqLogs()

	data, err := os.ReadFile(tmp)
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}

	lines, bad := 0, 0
	for _, line := range splitLinesSafe(string(data)) {
		if line == "" {
			continue
		}
		lines++
		var l RequestLog
		if json.Unmarshal([]byte(line), &l) != nil {
			bad++
		}
	}

	dropped := atomic.LoadInt64(&reqLogDropped)
	if bad > 0 {
		t.Fatalf("found %d malformed/partial lines (concurrent write race?)", bad)
	}
	total := int64(lines) + dropped
	if total != goroutines*perWorker {
		t.Fatalf("line count mismatch: lines=%d dropped=%d sum=%d, want %d",
			lines, dropped, total, goroutines*perWorker)
	}
}

// TestAppendReqLogNonBlocking 验证 AppendReqLog 永不阻塞调用方:
// (1) 预先尽量塞满 channel 后再调用一次, 断言立即返回;
// (2) 连续 10 万次调用总耗时必须是毫秒级(走 default 而非阻塞)。
func TestAppendReqLogNonBlocking(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "req-nb.jsonl")
	reqLogsFile = tmp
	t.Cleanup(func() {
		closeReqLogs() // 释放句柄, 避免 TempDir 清理因文件被占用而失败
		reqLogsFile = kit.ResolveDataPath("requests.jsonl")
	})

	// 启动写协程
	AppendReqLog(RequestLog{Method: "GET", Path: "/ping"})
	flushReqLogs()

	// 尽量把缓冲 channel 塞满(尽力, 受写协程并发消费影响)
	for i := 0; i < 100000; i++ {
		if len(reqLogCh) >= cap(reqLogCh) {
			break
		}
		select {
		case reqLogCh <- RequestLog{Method: "GET", Path: "/x"}:
		default:
		}
	}

	start := time.Now()
	AppendReqLog(RequestLog{Method: "GET", Path: "/x"})
	if elapsed := time.Since(start); elapsed > time.Millisecond {
		t.Fatalf("AppendReqLog blocked for %v (channel full path should be non-blocking)", elapsed)
	}

	// 大规模调用也必须毫秒级返回
	start = time.Now()
	for i := 0; i < 100000; i++ {
		AppendReqLog(RequestLog{Method: "GET", Path: "/x"})
	}
	if took := time.Since(start); took > 200*time.Millisecond {
		t.Fatalf("100k AppendReqLog calls took %v, expected milliseconds", took)
	}
	flushReqLogs()
}
