package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"cline-go-proxy/internal/kit"
)

// RequestLog 单条代理请求记录（对话/API 调用历史）
type RequestLog struct {
	Time     time.Time `json:"time"`
	Client   string    `json:"client"`
	Method   string    `json:"method"`
	Path     string    `json:"path"`
	Model    string    `json:"model,omitempty"`
	Route    string    `json:"route"` // zen | cline | admin | other
	Exit     string    `json:"exit,omitempty"`
	Status   int       `json:"status"`
	Duration int64     `json:"duration_ms"`
	Note     string    `json:"note,omitempty"`
}

const (
	maxReqLogs     = 500
	maxReqLogsFile = 10 << 20 // 10MB 上限，超出后清空落盘文件（内存仍保留最近 500 条）

	// reqLogBodyProbeBytes 中间件读取请求体的上限, 只用于提取 model 字段。
	// 多模态请求 body 可达数 MB, 全量读入会把整份内容存内存两次, 拖慢请求。
	// 8KB 覆盖绝大多数常规请求(纯文本 chat), 大 body 场景退化为空 model 可接受。
	reqLogBodyProbeBytes = 8 << 10
)

var (
	reqLogsMu sync.Mutex
	reqLogs   []RequestLog

	// 落盘异步化: 单写协程 + 有界缓冲 channel。AppendReqLog 永不阻塞调用方,
	// 缓冲满则丢弃并记一次丢弃计数(请求路径延迟优先于日志完整性)。
	reqLogCh      chan RequestLog
	reqLogFlushCh chan chan struct{}
	reqLogCloseCh chan chan struct{}
	reqLogDropped int64
	reqLogOnce    sync.Once
	reqLogFile    *os.File // 仅由写协程持有
)

// 写协程的缓冲容量: 远大于常规突发, 正常流量下几乎不丢; 异常突发时丢弃而非阻塞。
const reqLogChanCap = 8192

var reqLogsFile = kit.ResolveDataPath("requests.jsonl")

// AppendReqLog 记录一条请求日志：内存环形保留 + 异步追加落盘。
// 永不阻塞调用方: 通过 select/default 投递到缓冲 channel, 满了就丢弃。
func AppendReqLog(l RequestLog) {
	reqLogsMu.Lock()
	reqLogs = append(reqLogs, l)
	if len(reqLogs) > maxReqLogs {
		reqLogs = reqLogs[len(reqLogs)-maxReqLogs:]
	}
	reqLogsMu.Unlock()

	startReqLogWriter()
	select {
	case reqLogCh <- l:
	default:
		// 缓冲满: 丢弃本条, 记一次丢弃(延迟优先于日志完整性)。
		atomic.AddInt64(&reqLogDropped, 1)
	}
}

// startReqLogWriter 惰性启动单写协程(全局仅一次)。写协程是唯一对落盘文件
// 做写入/轮转的地方, 避免多协程并发 append 与"截断 vs 追加"的竞争。
func startReqLogWriter() {
	reqLogOnce.Do(func() {
		reqLogCh = make(chan RequestLog, reqLogChanCap)
		reqLogFlushCh = make(chan chan struct{}, 64)
		reqLogCloseCh = make(chan chan struct{}, 16)
		go reqLogWriterLoop()
	})
}

// reqLogWriterLoop 常驻写协程: 串行消费 channel, 写入文件, 并在超限时轮转。
func reqLogWriterLoop() {
	for {
		select {
		case l := <-reqLogCh:
			writeReqLog(l)
		case ack := <-reqLogFlushCh:
			// 把已经入队的日志全部写完, 再回 ack。
		drain:
			for {
				select {
				case l := <-reqLogCh:
					writeReqLog(l)
				default:
					break drain
				}
			}
			ack <- struct{}{}
		case ack := <-reqLogCloseCh:
			// 关闭句柄并回 ack(优雅退出/测试清理)。下次写入会惰性重开。
			if reqLogFile != nil {
				reqLogFile.Close()
				reqLogFile = nil
			}
			ack <- struct{}{}
		}
	}
}

// writeReqLog 仅在写协程内执行: 惰性打开文件, 追加一行, 超限则串行轮转。
// 句柄打开失败只打日志并允许后续重试, 不永久放弃。
func writeReqLog(l RequestLog) {
	if reqLogFile == nil {
		f, err := os.OpenFile(reqLogsFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
		if err != nil {
			log.Printf("reqlog: open file failed (will retry): %v", err)
			return
		}
		reqLogFile = f
	}
	data, err := json.Marshal(l)
	if err != nil {
		return
	}
	if _, err := reqLogFile.Write(append(data, '\n')); err != nil {
		log.Printf("reqlog: write failed (will retry): %v", err)
		reqLogFile.Close()
		reqLogFile = nil
		return
	}
	// 轮转与追加串行, 消除旧实现"截断竞态丢行/半行"的问题。
	// 必须用 os.Truncate 而不是句柄级 Truncate: reqLogFile 是 O_APPEND 打开的,
	// Windows 下该句柄没有 GENERIC_WRITE, 句柄级截断会 "Access is denied",
	// 旧代码因此把轮转失败误判成"文件句柄坏了"并反复 close+重开。
	if st, err := reqLogFile.Stat(); err == nil && st.Size() > maxReqLogsFile {
		if err := os.Truncate(reqLogsFile, 0); err != nil {
			log.Printf("reqlog: truncate failed: %v", err)
			reqLogFile.Close()
			reqLogFile = nil
			return
		}
	}
}

// flushReqLogs 等待写协程把已入队的日志写完(发哨兵并等 ack)。
// 供测试与优雅退出使用。
func flushReqLogs() {
	startReqLogWriter()
	ack := make(chan struct{})
	reqLogFlushCh <- ack
	<-ack
}

// closeReqLogs 等待已入队日志写完并关闭文件句柄(优雅退出/测试清理用)。
// 写协程常驻, 句柄关闭后下次写入会惰性重开。
func closeReqLogs() {
	flushReqLogs()
	startReqLogWriter()
	ack := make(chan struct{})
	reqLogCloseCh <- ack
	<-ack
}

// LoadRequestLogs 返回最近的请求日志（内存优先，启动后从落盘文件补载）。
//
// 必须克隆: 之前直接返回 reqLogs 底层 slice 的裸引用, admin 侧拿到的切片 header
// 与全局共享同一底层 array; 只要后续 AppendReqLog 未触发扩容就 append 到同一
// array 里, 就是并发读写。虽然 append 未扩容时"越界"通常不会崩进程, 但 TSAN
// 会报, 且语义上就是 data race —— 参照 config_clone.go / stats.clone() 的规矩,
// 锁内克隆、锁外读克隆。
func LoadRequestLogs() []RequestLog {
	reqLogsMu.Lock()
	defer reqLogsMu.Unlock()
	out := make([]RequestLog, len(reqLogs))
	copy(out, reqLogs)
	return out
}

// reqLogTailBytes 启动补载时只读文件尾部这么多字节。文件上限 10MB, 全量读入会
// 让启动多占 10MB 内存并逐行解析数千条 —— 实际面板只需要展示最近 500 条, 读尾
// 部一小段(64KB 已经覆盖数万条日志)完全够, 且启动峰值内存稳定。
const reqLogTailBytes = 64 << 10

// LoadRequestLogsFromFile 启动时从落盘文件读取尾部记录
// 与请求日志中间件同规则: 管理面板只读轮询等噪音不载入, 只保留对话/写操作/错误。
func LoadRequestLogsFromFile() {
	f, err := os.Open(reqLogsFile)
	if err != nil {
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return
	}
	// 文件比尾读窗口还小时直接全量读, 避免不必要的 seek。
	size := st.Size()
	if size <= reqLogTailBytes {
		raw, err := io.ReadAll(f)
		if err != nil {
			return
		}
		reqLogs = parseRequestLogsFromLines(string(raw))
		return
	}
	// 大文件: 只 seek 到尾部窗口起点读取, 首行可能不完整 —— 解析时会自动跳过
	// 那些解不出来的坏行。
	offset := int64(reqLogTailBytes)
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return
	}
	buf := make([]byte, reqLogTailBytes)
	n, err := f.Read(buf)
	if n == 0 {
		return
	}
	raw := buf[:n]
	// 从第一个 '\n' 之后开始解析, 丢掉那条被截断的半行。
	if i := bytes.IndexByte(raw, '\n'); i >= 0 {
		raw = raw[i+1:]
	}
	_ = err
	reqLogs = parseRequestLogsFromLines(string(raw))
}

// parseRequestLogsFromLines 逐行解析并过滤噪音, 与旧的 LoadRequestLogsFromFile
// 逻辑一致, 抽成函数便于测试。
func parseRequestLogsFromLines(s string) []RequestLog {
	var out []RequestLog
	for _, line := range splitLinesSafe(s) {
		if line == "" {
			continue
		}
		var l RequestLog
		if json.Unmarshal([]byte(line), &l) == nil {
			if isRequestNoise(l) {
				continue
			}
			out = append(out, l)
		}
	}
	if len(out) > maxReqLogs {
		out = out[len(out)-maxReqLogs:]
	}
	return out
}

// upstreamFromRouteHeader 从 X-Proxy-Route 响应头提取实际上游名。
// 头格式: "upstream=cline; model=zen/x; failover=zen-degraded"。
func upstreamFromRouteHeader(v string) string {
	for _, part := range strings.Split(v, ";") {
		part = strings.TrimSpace(part)
		if rest, ok := strings.CutPrefix(part, "upstream="); ok {
			return strings.TrimSpace(rest)
		}
	}
	return ""
}

// isRequestNoise 该条记录是否为管理轮询等噪音(与中间件过滤同一判定)
func isRequestNoise(l RequestLog) bool {
	if l.Route == "admin" && l.Method == "GET" {
		return true
	}
	if (l.Route == "meta" || l.Route == "other") && l.Status < http.StatusBadRequest {
		return true
	}
	return false
}

func splitLinesSafe(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

// ============ 请求日志中间件 ============

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

// Flush 透传底层 Flusher: SSE 流式中继依赖 http.Flusher 断言,
// 包装层若不实现该接口会导致流式响应静默退化为空响应体。
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// requestLogMiddleware 记录所有进入代理的请求（API 调用与对话历史）。
func requestLogMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}

		// 只读前 reqLogBodyProbeBytes 用来提取 model 字段: 多模态请求把图片 base64
		// 塞进 body 时可能几 MB, 全量 io.ReadAll 会把整份 body 读进内存两次(这里
		// 一次、下游 handler 再一次)。model 字段一般在 JSON 前几十字节内, 读一小段
		// 就够 —— 若截断后 JSON 解析失败, 退化为空 model, 不影响功能。
		model := ""
		limit := int64(reqLogBodyProbeBytes)
		if r.ContentLength != 0 && r.ContentLength < limit {
			limit = r.ContentLength
		}
		bodyBytes, _ := io.ReadAll(io.LimitReader(r.Body, limit))
		if len(bodyBytes) > 0 {
			// 放回剩余部分, 避免影响下游处理。
			if r.ContentLength > int64(len(bodyBytes)) {
				rest, _ := io.ReadAll(r.Body)
				r.Body = io.NopCloser(bytes.NewReader(append(bodyBytes, rest...)))
			} else {
				r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
			}
			var probe struct {
				Model string `json:"model"`
			}
			if json.Unmarshal(bodyBytes, &probe) == nil {
				model = probe.Model
			}
		}

		// 出口收集器: zen 拨号层经 request context 回写实际使用的出口
		exit := &reqExit{}
		r = r.WithContext(context.WithValue(r.Context(), ctxKeyReqExit, exit))

		next.ServeHTTP(sw, r)

		if sw.status == 0 {
			sw.status = http.StatusOK
		}
		route := "other"
		switch {
		case strings.HasPrefix(r.URL.Path, "/admin"):
			route = "admin"
		case strings.HasPrefix(model, "zen/"):
			route = "zen"
		case model != "":
			route = "cline"
		case strings.Contains(r.URL.Path, "models") || strings.Contains(r.URL.Path, "health"):
			route = "meta"
		}

		// 路由判定优先采用 handler 实际选择的出口(X-Proxy-Route):
		// zen 熔断降级到 cline 池时, 仅凭 model 前缀会把请求误标为 zen。
		if up := upstreamFromRouteHeader(sw.Header().Get("X-Proxy-Route")); up == "zen" || up == "cline" {
			route = up
		}

		// 请求日志只抓重点: 对话/模型调用、配置写操作与错误记录。
		// 管理面板只读轮询与健康心跳等噪音不落盘, 避免占满最近 500 条容量。
		if isRequestNoise(RequestLog{Route: route, Method: r.Method, Status: sw.status}) {
			return
		}

		// 出口回填: keep-alive 复用连接时不会重新拨号(拨号层此时不写 exit),
		// 用当前轮换命中的出口补齐, 保证每次对话请求都能看到实际出口。
		if exit.name == "" {
			if p := describeEffectiveExit(); p != "" {
				exit.name = p
			}
		}

		client := r.RemoteAddr
		if host, _, err := net.SplitHostPort(client); err == nil {
			client = host
		}
		AppendReqLog(RequestLog{
			Time:     time.Now(),
			Client:   client,
			Method:   r.Method,
			Path:     r.URL.Path,
			Model:    model,
			Route:    route,
			Exit:     exit.name,
			Status:   sw.status,
			Duration: time.Since(start).Milliseconds(),
		})
	})
}
