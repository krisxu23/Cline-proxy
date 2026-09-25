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

	"free-router/internal/kit"
)

// RequestLog 单条代理请求记录（对话/API 调用历史）
//
// 字段口径参照 OmniRoute 的 call_logs 摘要设计: 一行就能回答
// "谁、走哪个上游、什么模型、多快、多少 token、为什么失败"。
// 注意这里只记元数据 —— 不含 prompt、响应正文与请求头, 避免日志变成泄露面。
type RequestLog struct {
	Time   time.Time `json:"time"`
	ID     string    `json:"id,omitempty"` // 请求 id(X-Request-Id), 与 zen-stats/决策轨迹关联
	Client string    `json:"client"`
	Method string    `json:"method"`
	Path   string    `json:"path"`
	Model  string    `json:"model,omitempty"` // 客户端请求的模型(可能是别名/组合名)
	// ResolvedModel 实际发往上游的模型: 与 Model 不同即说明发生了别名/组合/兜底改写,
	// 面板上两者的差异就是"这次到底打了谁"的直接证据。
	ResolvedModel string `json:"resolvedModel,omitempty"`
	Route         string `json:"route"`              // zen | cline | admin | other
	Upstream      string `json:"upstream,omitempty"` // zen | cline | clinepass | provider/<name> | chain
	Exit          string `json:"exit,omitempty"`
	Status        int    `json:"status"`
	DurationMs    int64  `json:"durationMs"`
	// DurationLegacy 兼容旧落盘记录(历史字段名 duration_ms), 载入时回填到 DurationMs。
	DurationLegacy int64  `json:"duration_ms,omitempty"`
	TTFTMs         int64  `json:"ttftMs,omitempty"` // 首字节耗时(流式体验的关键指标)
	Stream         bool   `json:"stream,omitempty"`
	Protocol       string `json:"protocol,omitempty"` // openai | anthropic | responses
	Attempts       int    `json:"attempts,omitempty"` // 候选链真实尝试次数
	// Skipped 被跳过的候选及原因("zen/mimo=冷却中(rateLimit)"), 是"为什么没用那站"的答案。
	Skipped          []string `json:"skipped,omitempty"`
	ErrClass         string   `json:"errClass,omitempty"`
	ErrMsg           string   `json:"errMsg,omitempty"`
	PromptTokens     int      `json:"promptTokens,omitempty"`
	CompletionTokens int      `json:"completionTokens,omitempty"`
	ReasoningTokens  int      `json:"reasoningTokens,omitempty"`
	CacheTokens      int      `json:"cacheTokens,omitempty"`
	Note             string   `json:"note,omitempty"`
	// UsageReported 上游是否真的上报了 usage(区分"报了 0"与"没报")。
	UsageReported bool `json:"usageReported,omitempty"`
}

const (
	maxReqLogs = 500
	// 请求日志按天分段(P2-17): requests-YYYYMMDD.jsonl, 保留 7 天(logRetentionDays),
	// 取代旧的"10MB truncate 清零"。内存仍只保留最近 500 条。

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
	reqLogWriter  *dailyLogFileWriter // 仅由写协程持有(按天分段)
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

// reqLogWriterLoop 常驻写协程: 串行消费 channel, 按天分段写入(P2-17)。
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
			if reqLogWriter != nil {
				reqLogWriter.close()
			}
			ack <- struct{}{}
		}
	}
}

// writeReqLog 仅在写协程内执行: 追加一行到当天的分段文件。
// 打开/写入失败只打日志并允许后续重试, 不永久放弃。
func writeReqLog(l RequestLog) {
	data, err := json.Marshal(l)
	if err != nil {
		return
	}
	if reqLogWriter == nil {
		reqLogWriter = &dailyLogFileWriter{base: reqLogsFile}
	}
	if _, err := reqLogWriter.write(data); err != nil {
		log.Printf("reqlog: write failed (will retry): %v", err)
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

// LoadRequestLogsFromFile 启动时从落盘文件读取尾部记录(按天分段后的今天文件,
// 不足 500 条时再回读昨天文件补齐)。与请求日志中间件同规则: 管理面板只读
// 轮询等噪音不载入, 只保留对话/写操作/错误。
func LoadRequestLogsFromFile() {
	now := time.Now()
	loaded := loadDailyRequestLogs(dailyLogPath(reqLogsFile, now))
	if len(loaded) < maxReqLogs {
		// 今天刚开张: 回读昨天的分段文件补齐, 保证面板冷启动也有历史
		prev := append(loadDailyRequestLogs(dailyLogPath(reqLogsFile, now.AddDate(0, 0, -1))), loaded...)
		if len(prev) > maxReqLogs {
			prev = prev[len(prev)-maxReqLogs:]
		}
		loaded = prev
	}
	reqLogs = loaded
}

// loadDailyRequestLogs 读单个分段文件的尾部记录。
func loadDailyRequestLogs(path string) []RequestLog {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil
	}
	// 文件比尾读窗口还小时直接全量读, 避免不必要的 seek。
	size := st.Size()
	if size <= reqLogTailBytes {
		raw, err := io.ReadAll(f)
		if err != nil {
			return nil
		}
		return parseRequestLogsFromLines(string(raw))
	}
	// 大文件: 只 seek 到尾部窗口起点读取, 首行可能不完整 —— 解析时会自动跳过
	// 那些解不出来的坏行。
	//
	// ★ 起点是 size-reqLogTailBytes, 不是 reqLogTailBytes(2026-09-24 修复):
	//   原实现 Seek(reqLogTailBytes, SeekStart) 读的是"距文件头 64KB 处"的**中段**,
	//   与注释承诺的"尾部窗口"不符 —— 当日日志一超过 64KB, 冷启动补载拿到的就是
	//   一段陈旧的中段内容。同仓 stats.go 的 size-tail 写法是对的, 这里对齐。
	if _, err := f.Seek(size-int64(reqLogTailBytes), io.SeekStart); err != nil {
		return nil
	}
	buf := make([]byte, reqLogTailBytes)
	n, err := f.Read(buf)
	if n == 0 {
		return nil
	}
	raw := buf[:n]
	// 从第一个 '\n' 之后开始解析, 丢掉那条被截断的半行。
	if i := bytes.IndexByte(raw, '\n'); i >= 0 {
		raw = raw[i+1:]
	}
	return parseRequestLogsFromLines(string(raw))
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
			// 兼容旧记录: 历史字段是 duration_ms, 新字段是 durationMs。
			if l.DurationMs == 0 && l.DurationLegacy > 0 {
				l.DurationMs = l.DurationLegacy
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

// protocolFromPath 按入口路径判定客户端协议, 供请求日志标注。
// 上游一律 OpenAI 形状, 所以"协议"指的是客户端那一侧。
func protocolFromPath(path string) string {
	switch {
	case strings.HasSuffix(path, "/messages") || strings.Contains(path, "/v1/messages"):
		return "anthropic"
	case strings.Contains(path, "/responses"):
		return "responses"
	case strings.HasSuffix(path, "/chat/completions"):
		return "openai"
	default:
		return "openai"
	}
}

// clientIPFromRequest 直连公网 peer 时 RemoteAddr 即是客户端(代理头一律忽略,
// 防伪造); 只有直连方是回环/私网/未指定(本地反代/可信代理)时, 才信任
// X-Forwarded-For / X-Real-IP, 且只取其中第一个公网 IP, 私网/回环/非法项跳过。
func clientIPFromRequest(r *http.Request) string {
	peer := r.RemoteAddr
	if host, _, err := net.SplitHostPort(peer); err == nil {
		peer = host
	}
	if peer == "" {
		return r.RemoteAddr
	}
	if ip := net.ParseIP(peer); ip == nil || !isTrustedProxyPeer(ip) {
		// 直连公网(或不可解析): 原样返回, 代理头视为可伪造, 永不覆盖。
		if ip != nil {
			return peer
		}
		return r.RemoteAddr
	}
	// 可信代理: XFF 先于 X-Real-IP, 首个公网 IP 胜出; 全是私网/非法则回退 peer。
	for _, h := range strings.Split(r.Header.Get("X-Forwarded-For"), ",") {
		if pub := firstPublicIP(h); pub != "" {
			return pub
		}
	}
	if pub := firstPublicIP(r.Header.Get("X-Real-IP")); pub != "" {
		return pub
	}
	return peer
}

// isTrustedProxyPeer 回环/私网/未指定/链路本地/组播均视为本地可信代理。
func isTrustedProxyPeer(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast()
}

// firstPublicIP 单个候选项为公网 IP 时返回其规范形式, 否则返回空串。
func firstPublicIP(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(s); err == nil {
		s = host
	}
	s = strings.Trim(s, "[]")
	ip := net.ParseIP(s)
	if ip == nil || isTrustedProxyPeer(ip) {
		return ""
	}
	return ip.String()
}

// isRequestNoise 该条记录是否为管理轮询等噪音(与中间件过滤同一判定)
func isRequestNoise(l RequestLog) bool {
	// 上游真的上报了 usage(2xx): 一定是成功的对话, 永远不是噪声 ——
	// 即使路由误标为 other 也必须落盘, 否则成功的对话会凭空消失。
	if l.UsageReported && l.Status < http.StatusBadRequest {
		return false
	}
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
	// firstWriteAt 首次向客户端写出正文的时间, 用于计算 TTFT(首字节耗时)。
	firstWriteAt time.Time
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if w.firstWriteAt.IsZero() {
		w.firstWriteAt = time.Now()
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
		// ContentLength==-1(Transfer-Encoding: chunked, 长度未知)**不能**收窄
		// limit: io.LimitReader(r.Body, -1) 会立即 EOF → 只读到 0 字节 →
		// model 恒空 → route 退化为 other → 成功的对话请求被 isRequestNoise
		// 当噪声丢弃(P3-12)。只有"已知长度且小于探针上限"才收窄,
		// -1/0 一律保持 8KB 探针上限。
		if r.ContentLength > 0 && r.ContentLength < limit {
			limit = r.ContentLength
		}
		bodyBytes, _ := io.ReadAll(io.LimitReader(r.Body, limit))
		if len(bodyBytes) > 0 {
			// 放回剩余部分, 避免影响下游处理。
			// chunked(ContentLength<0)长度未知, 必须走"读回剩余再拼接"的分支,
			// 否则只放回探针读到的前 8KB, 下游拿到的 body 被截断(转发即坏)。
			if r.ContentLength < 0 || r.ContentLength > int64(len(bodyBytes)) {
				// 终审 P3: 零拷贝拼回 —— 旧实现 io.ReadAll(rest) + append 会把
				// 整份剩余 body 再进内存并复制一次(chunked 请求最多 64MiB×2 瞬时)。
				// MultiReader 串接已读前缀与剩余 body, ContentLength 语义不变。
				r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(bodyBytes), r.Body))
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

		// 请求轨迹: 上游名/实际模型/尝试与跳过轨迹/错误类别/token 用量由链路上各层
		// 回填(见 req_trace.go), 这里只负责注入与最终落盘。
		clientIP := clientIPFromRequest(r)
		tr := &reqTrace{RequestID: newRequestID(r.Header.Get("X-Request-Id")), ClientIP: clientIP}
		r = r.WithContext(withReqTrace(r.Context(), tr))
		// 回写请求 id: 客户端可据此在自己的日志里对齐网关日志与面板详情。
		w.Header().Set("X-Request-Id", tr.RequestID)

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
		case strings.HasSuffix(r.URL.Path, "/chat/completions") ||
			strings.HasSuffix(r.URL.Path, "/messages") ||
			strings.Contains(r.URL.Path, "/v1/messages") ||
			strings.Contains(r.URL.Path, "/responses"):
			// 对话调用按路径兜底(P3-12): chunked/超探针上限时 model 可能提取
			// 不到, 但这类请求绝不是可丢弃的 other 噪声 —— 否则成功的对话请求
			// 会被 isRequestNoise 按 other+status<400 整类丢弃, 凭空消失。
			route = "cline"
		}

		// 路由判定优先采用 handler 实际选择的出口(X-Proxy-Route):
		// zen 熔断降级到 cline 池时, 仅凭 model 前缀会把请求误标为 zen。
		routeHdr := sw.Header().Get("X-Proxy-Route")
		if up := upstreamFromRouteHeader(routeHdr); up == "zen" || up == "cline" {
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

		client := clientIPFromRequest(r)
		snap := tr.snapshot()
		upstream := snap.Upstream
		if upstream == "" {
			upstream = upstreamFromRouteHeader(routeHdr)
		}
		protocol := snap.Protocol
		if protocol == "" {
			protocol = protocolFromPath(r.URL.Path)
		}
		ttft := int64(0)
		if !sw.firstWriteAt.IsZero() {
			ttft = sw.firstWriteAt.Sub(start).Milliseconds()
		}
		// Note 是给人看的一句话摘要: 优先错误, 其次"跳过了哪些站"。
		note := snap.ErrMsg
		if note == "" && len(snap.Skipped) > 0 {
			note = "跳过 " + strings.Join(snap.Skipped, ", ")
		}
		AppendReqLog(RequestLog{
			Time:          time.Now(),
			ID:            snap.RequestID,
			Client:        client,
			Method:        r.Method,
			Path:          r.URL.Path,
			Model:         model,
			ResolvedModel: snap.Resolved,
			Route:         route,
			Upstream:      upstream,
			Exit:          exit.name,
			// 流式提交后改判优先: 空流守卫 502 时 wire 状态已是 200,
			// 用轨迹里的交付状态才能记对(2026-09-24 空 200 事件)。
			Status:           deliveredStatusOr(snap.Delivered, sw.status),
			DurationMs:       time.Since(start).Milliseconds(),
			TTFTMs:           ttft,
			Stream:           snap.Stream,
			Protocol:         protocol,
			Attempts:         snap.Attempts,
			Skipped:          snap.Skipped,
			ErrClass:         snap.ErrClass,
			ErrMsg:           snap.ErrMsg,
			PromptTokens:     snap.PromptTokens,
			CompletionTokens: snap.CompletionTokens,
			ReasoningTokens:  snap.ReasoningTokens,
			CacheTokens:      snap.CacheTokens,
			Note:             note,
			UsageReported:    snap.UsageReported,
		})
	})
}

// deliveredStatusOr 提交后改判优先: 流式空流守卫等在 WriteHeader(200) 之后才
// 得出 502, wire 状态改不了, 请求日志必须用轨迹里的交付状态, 否则面板 200 与
// 统计 502 自相矛盾。无改判(0)时回退 wire 状态。
func deliveredStatusOr(delivered, wire int) int {
	if delivered >= 400 {
		return delivered
	}
	return wire
}
