package app

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLogFanoutTruncatesOnWrite 锁住"运行期轮转"这个行为。
//
// 背景: 此前截断逻辑只写在 initLogFile 里, 也就是**启动时执行一次**,
// 于是"长期不重启"就等于"主日志无上限"; 而订阅源返回异常内容时一次刷新
// 就能吐出几千行(实测两轮刷新各 ~4600 行), 足以把磁盘写满。
// 交付目录的真实日志跨 25+ 次重启、7011 行, "已截断"一次都没出现过。
// 现在 logFanout.Write 在写入路径上自己维护上限, 与 writeStreamLog 语义对齐。
func TestLogFanoutTruncatesOnWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cline-proxy.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0600)
	if err != nil {
		t.Fatalf("创建日志文件失败: %v", err)
	}
	defer f.Close()

	// 起始大小故意压在上限边缘: 再写一点点就会越界。
	w := &logFanout{
		dsts: []io.Writer{f},
		file: f,
		path: path,
		size: maxLogBytes - 4,
	}
	if _, err := w.Write([]byte("0123456789")); err != nil {
		t.Fatalf("Write 失败: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取日志失败: %v", err)
	}
	if !strings.Contains(string(got), "已截断") {
		t.Fatalf("超过上限后应写入截断标记, 实际内容: %q", string(got))
	}
	if strings.Contains(string(got), "0123456789") {
		t.Fatalf("超过上限后旧内容应被清空, 实际内容: %q", string(got))
	}
	if w.size != int64(len(got)) {
		t.Fatalf("size 记账与实际文件不符: w.size=%d, 文件=%d 字节", w.size, len(got))
	}
}

// TestLogFanoutKeepsContentBelowLimit 确认没超限时正常追加、不误截断。
func TestLogFanoutKeepsContentBelowLimit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cline-proxy.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0600)
	if err != nil {
		t.Fatalf("创建日志文件失败: %v", err)
	}
	defer f.Close()

	w := &logFanout{dsts: []io.Writer{f}, file: f, path: path, size: 0}
	if _, err := w.Write([]byte("keep-me\n")); err != nil {
		t.Fatalf("Write 失败: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取日志失败: %v", err)
	}
	if string(got) != "keep-me\n" {
		t.Fatalf("未超限时内容应原样保留, 实际: %q", string(got))
	}
	if w.size != int64(len(got)) {
		t.Fatalf("size 记账错误: w.size=%d, 文件=%d 字节", w.size, len(got))
	}
}

// TestLogFanoutNoFileDoesNotPanic 确认没有主日志文件(纯 stderr 场景)时安全。
func TestLogFanoutNoFileDoesNotPanic(t *testing.T) {
	var buf strings.Builder
	w := &logFanout{dsts: []io.Writer{&buf}, file: nil, size: maxLogBytes * 2}
	if _, err := w.Write([]byte("x")); err != nil {
		t.Fatalf("Write 失败: %v", err)
	}
	if buf.String() != "x" {
		t.Fatalf("应把内容分发给 dst, 实际: %q", buf.String())
	}
}
