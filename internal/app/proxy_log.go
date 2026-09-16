package app

import (
	"cline-go-proxy/internal/kit"
	"fmt"
	"io"
	"log"
	"os"
	"sync"
)

// maxLogBytes cline-proxy.log 的上限(10 MiB)。
//
// GUI 构建下没有控制台, 这个文件是唯一的诊断通道; 无限追加会在长期运行后
// 撑爆磁盘, 也让翻日志越来越难。超限时截断成空而不是滚动保留历史 ——
// 诊断日志要的是最近一段, 不值得为历史轮转引入额外复杂度。
const maxLogBytes = 10 << 20

// initLogFile 将日志同时输出到控制台与 cline-proxy.log（追加模式），
// 控制台窗口滚动内容有限，文件可完整保留最近 maxLogBytes 的日志。
func initLogFile() {
	path := kit.ResolveDataPath("cline-proxy.log")
	// 0600 是「不写敏感信息」之外的第二道防线: 日志会记录订阅节点、上游返回文本等
	// 可能带凭据的内容。注意 Windows 不实现数字权限位(实测文件仍是 644), 真正生效的
	// 是把令牌本身挡在日志外 —— 见下方「访问令牌已省略」那条 Printf。
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		log.Printf("  open log file failed: %v", err)
		return
	}
	truncated := false
	initialSize := int64(0)
	if st, serr := f.Stat(); serr == nil {
		initialSize = st.Size()
		if initialSize > maxLogBytes {
			// 必须用 os.Truncate(path) 而不是 f.Truncate(0): Windows 下 O_APPEND
			// 句柄没有 GENERIC_WRITE, 句柄级 Truncate 必然 Access denied ——
			// 这正是"启动期截断从来没生效过"的根因。
			if terr := os.Truncate(path, 0); terr == nil {
				truncated = true
				initialSize = 0
			}
		}
	}
	// 用指针而不是值: logFanout 现在带 mutex, 值传递会复制锁(go vet 也会报)。
	// file/path/size 交给它之后, 运行期的轮转就在写入路径上自动完成。
	log.SetOutput(&logFanout{
		dsts: []io.Writer{f, os.Stderr},
		file: f,
		path: path,
		size: initialSize,
	})
	if truncated {
		log.Printf("log file 超过 %d MiB, 已截断(只保留最近内容)", maxLogBytes>>20)
	}
	log.Printf("========== proxy started, log file: %s ==========", path)
}

// logFanout 逐目标分发日志, 单个目标写入失败不影响其它目标。
// 桌面模式(GUI 子系统, 双击启动)下 os.Stderr 句柄无效, io.MultiWriter
// 会在首个 writer 出错时短路, 导致文件日志一并丢失, 故不走 MultiWriter。
//
// 除分发外, 它还在**写入路径上**维护主日志文件的大小上限: 累计写入超过
// maxLogBytes 就把文件截断成空。这一点是关键 —— 早期实现只在启动时检查
// 一次, 于是"长期不重启"等于"日志无上限"; 而订阅源返回异常内容时, 一次
// 刷新就能吐出几千行(实测两轮刷新各 ~4600 行), 足以把磁盘写满。
// 现在与 writeStreamLog 的语义对齐(后者本来就在每次写入时检查)。
type logFanout struct {
	mu   sync.Mutex
	dsts []io.Writer
	file *os.File // 需要做大小检查与截断的目标(主日志文件), 可为 nil
	path string   // 主日志文件路径, 供 os.Truncate 使用(见 truncateIfNeeded)
	size int64    // 当前文件已写入字节数(含本次启动前已有内容)
}

func (w *logFanout) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, dst := range w.dsts {
		dst.Write(p)
	}
	w.size += int64(len(p))
	w.truncateIfNeeded()
	return len(p), nil
}

// truncateIfNeeded 在持锁状态下调用: 超过上限则清空并写入一条醒目标记。
// 注意这里**绝不能走 log.Printf** —— 那会重入 Write 造成死锁, 所以直接写文件。
//
// 为什么用 os.Truncate(path) 而不是 w.file.Truncate(0):
// Windows 下以 os.O_APPEND 打开的句柄只获得 FILE_APPEND_DATA 权限, 句柄级
// Truncate(SetEndOfFile) 会直接返回 "Access is denied"。而本仓库所有日志截断点
// (主日志、流日志、stats、请求日志)都开在 O_APPEND 句柄上并带 `if err == nil`
// 兜底 —— 于是"轮转"在 Windows 上**一直是静默失效**的。os.Truncate 自己开一个
// 不带 O_APPEND 的句柄, 两个平台都可靠。
// 也正因为句柄是 O_APPEND, 截断后无需 Seek: 每次写都会自动落到文件末尾(=0)。
func (w *logFanout) truncateIfNeeded() {
	if w.file == nil || w.size <= maxLogBytes {
		return
	}
	if err := os.Truncate(w.path, 0); err != nil {
		return
	}
	marker := fmt.Sprintf("log file 超过 %d MiB, 已截断(只保留最近内容)\n", maxLogBytes>>20)
	if _, err := w.file.WriteString(marker); err != nil {
		w.size = 0
		return
	}
	w.size = int64(len(marker))
}

// ============================================================================
// 生命周期与优雅退出
// ============================================================================

var (
	streamLogMu   sync.Mutex
	streamLogFile *os.File
)

// streamLogFileName 流式诊断日志文件名。截断需要按路径重新开句柄(见 writeStreamLog),
// 所以抽成常量而不是内联字面量, 避免两处写法漂移。
const streamLogFileName = "cline-proxy-stream.log"

func writeStreamLog(line string) {
	streamLogMu.Lock()
	defer streamLogMu.Unlock()
	if streamLogFile == nil {
		f, err := os.OpenFile(kit.ResolveDataPath(streamLogFileName), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			return
		}
		streamLogFile = f
	}
	// 超过上限则截断成空, 只保留最近内容(与 cline-proxy.log 的轮转思路一致)。
	if st, serr := streamLogFile.Stat(); serr == nil && st.Size() > maxLogBytes {
		// 必须用 os.Truncate 而不是句柄级 Truncate: O_APPEND 句柄在 Windows 上
		// 拿不到 GENERIC_WRITE, 句柄级截断会 "Access is denied" —— 旧实现因此
		// 一直静默失效(详见 logFanout.truncateIfNeeded 的注释)。
		if terr := os.Truncate(kit.ResolveDataPath(streamLogFileName), 0); terr != nil {
			log.Printf("streamlog: truncate 失败: %v", terr)
		}
	}
	streamLogFile.WriteString(line)
}

func closeStreamLog() {
	streamLogMu.Lock()
	defer streamLogMu.Unlock()
	if streamLogFile != nil {
		streamLogFile.Close()
		streamLogFile = nil
	}
}
