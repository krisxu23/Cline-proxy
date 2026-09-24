package app

// 日志按天分段轮转 (P2-17, 参照 OmniRoute 的 callLogRotation: 保留 7 天、
// 分块删除; 不学它的 SQLite/迁移体系)。
//
// 旧实现是"超过 10MB 直接 truncate 清零" —— 排查昨夜问题的历史就没了。
// 新实现: 文件按天分段(<名字>-YYYYMMDD.<ext>), 每天一个文件;
// 换天时顺手删除超过保留期(默认 7 天)的旧文件。内存聚合逻辑不变。

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"free-router/internal/kit"
)

const (
	logRetentionDays = 7 // 日志保留天数
)

// dailyLogPath 返回按天分段的日志文件绝对路径:
// base 形如 ".../requests.jsonl", 产出 ".../requests-20260915.jsonl"。
func dailyLogPath(base string, day time.Time) string {
	dir := filepath.Dir(base)
	ext := filepath.Ext(base)
	prefix := strings.TrimSuffix(filepath.Base(base), ext)
	return filepath.Join(dir, fmt.Sprintf("%s-%s%s", prefix, day.Format("20060102"), ext))
}

// cleanupOldDailyLogs 删除超过保留期的按天分段日志。只处理与 base 同目录、
// 同前缀的文件; 删除失败仅记日志不中断。
func cleanupOldDailyLogs(base string, now time.Time) {
	dir := filepath.Dir(base)
	ext := filepath.Ext(base)
	prefix := strings.TrimSuffix(filepath.Base(base), ext)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := now.AddDate(0, 0, -logRetentionDays)
	cutStamp := cutoff.Format("20060102")
	var victims []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, prefix+"-") || !strings.HasSuffix(name, ext) {
			continue
		}
		stamp := strings.TrimSuffix(strings.TrimPrefix(name, prefix+"-"), ext)
		if len(stamp) != 8 || stamp >= cutStamp {
			continue // 非日期后缀或仍在保留期内
		}
		victims = append(victims, filepath.Join(dir, name))
	}
	// 稳定顺序删除(老文件优先), 便于日志观察
	sort.Strings(victims)
	for _, v := range victims {
		if err := os.Remove(v); err != nil && !os.IsNotExist(err) {
			// 终审 P3: 原实现是 time.Sleep(0) no-op —— 注释承诺"失败仅记录"
			// 却既不记录也不做任何事。改为真实记录。
			log.Printf("日志清理: 删除过期文件 %s 失败: %v", v, err)
		}
	}
}

// dailyLogFileWriter 按天分段的追加写句柄管理器: 换天自动切换文件,
// 并在换天时清理过期文件。非并发安全 —— 由调用方串行使用(各写协程内)。
type dailyLogFileWriter struct {
	base   string
	day    string
	file   *os.File
	lastGC time.Time
}

// write 追加一行; 自动处理换天与过期清理。返回实际写入的文件路径(测试用)。
func (w *dailyLogFileWriter) write(line []byte) (string, error) {
	// 单次取时刻: 原实现一次调用里取了三次 time.Now(), 跨午夜时可能出现
	// "判天用旧日期、开文件用新日期、清理基准又是另一个"的错乱路径
	// (2026-09-24 审查)。now 一次取定, 全函数复用。
	now := time.Now()
	day := now.Format("20060102")
	if w.file == nil || w.day != day {
		if w.file != nil {
			w.file.Close()
			w.file = nil
		}
		w.day = day
		f, err := os.OpenFile(dailyLogPath(w.base, now), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
		if err != nil {
			return "", err
		}
		w.file = f
		// 换天 = 清理过期文件的天然时机(每天至多一次)
		if now.Sub(w.lastGC) > 12*time.Hour {
			cleanupOldDailyLogs(w.base, now)
			w.lastGC = now
		}
	}
	if _, err := w.file.Write(append(line, '\n')); err != nil {
		// 写失败: 关闭句柄, 下次写入重开
		w.file.Close()
		w.file = nil
		return "", err
	}
	return dailyLogPath(w.base, now), nil
}

func (w *dailyLogFileWriter) close() {
	if w.file != nil {
		w.file.Close()
		w.file = nil
	}
}

// resolveDailyPath 测试/诊断辅助: 给定 base 与时间, 返回当日分段路径。
func resolveDailyPath(base string) string {
	return kit.ResolveDataPath(dailyLogPath(base, time.Now()))
}
