package app

import (
	"bytes"
	"cline-go-proxy/internal/kit"
	"context"
	"encoding/json"
	"io"
	"log"
	"os"
	"sync"
	"time"
)

// ============================================================================
// 请求级统计与日志入库
// 落盘: zen-stats.jsonl 追加式(每请求一行)
// 内存: 今日/累计聚合,管理后台展示
// ============================================================================

type zenStatsRecord struct {
	TS               int64  `json:"ts"`
	ReqID            string `json:"reqId,omitempty"` // 请求 id: 与 requests.jsonl / 决策轨迹关联
	Upstream         string `json:"upstream"`        // zen / cline / clinepass / provider/<name>
	Model            string `json:"model"`           // 上游实际模型 ID
	Stream           bool   `json:"stream"`
	Compacted        bool   `json:"compacted"`
	OK               bool   `json:"ok"`
	Status           int    `json:"status"`
	LatencyMs        int64  `json:"latencyMs"`
	PromptTokens     int    `json:"promptTokens"`     // 入站估算
	CompletionTokens int    `json:"completionTokens"` // 上游 usage 或估算
	// ReasoningTokens / CacheTokens 推理与缓存 token 单列(P2-18):
	// 推理不占用可见正文、缓存不重复计费, 混在 completion/prompt 里会误导。
	ReasoningTokens  int `json:"reasoningTokens,omitempty"`
	CacheTokens      int `json:"cacheTokens,omitempty"`
	CompactionTokens int `json:"compactionTokens"` // 摘要生成消耗
	RateLimited      int `json:"rateLimited"`      // 本次请求触发限流的次数
}

// 上游标识常量。四类上游都要记账: 只统计 opencode 会让面板上的总量失真。
const (
	upstreamZen       = "zen"
	upstreamCline     = "cline"
	upstreamClinePass = "clinepass"
	upstreamProvider  = "provider"
)

// providerUpstream 通用 Provider 的上游标识: 归到 provider 类但保留具体名字,
// 「按上游」里就能直接看出是哪个 Provider 在消耗 token。
func providerUpstream(name string) string {
	if name == "" {
		return upstreamProvider
	}
	return upstreamProvider + "/" + name
}

type zenStatsAgg struct {
	Date         string                    `json:"date"`
	Requests     int64                     `json:"requests"`
	PromptTok    int64                     `json:"promptTokens"`
	CompleteTok  int64                     `json:"completionTokens"`
	ReasoningTok int64                     `json:"reasoningTokens"`
	CacheTok     int64                     `json:"cacheTokens"`
	Compaction   int64                     `json:"compactionTokens"`
	RateLimited  int64                     `json:"rateLimited"` // 限流命中次数
	ByModel      map[string]*zenStatsModel `json:"byModel"`
	ByUpstream   map[string]*zenStatsModel `json:"byUpstream"`
}

type zenStatsModel struct {
	Requests     int64 `json:"requests"`
	PromptTok    int64 `json:"promptTokens"`
	CompleteTok  int64 `json:"completionTokens"`
	ReasoningTok int64 `json:"reasoningTokens"`
	CacheTok     int64 `json:"cacheTokens"`
}

var (
	statsFile   *os.File
	statsFileMu sync.Mutex
	statsToday  *zenStatsAgg
	statsTotal  *zenStatsAgg
	statsAggMu  sync.Mutex

	// 可重试初始化: 用 Mutex + 状态替代 sync.Once, 打开失败后能重试而不永久降级。
	statsInitMu      sync.Mutex
	statsRollStarted bool
	statsFilePath    string // 可赋值变量, 便于测试注入不可写路径
)

type zenStatsTracker struct {
	rec      zenStatsRecord
	started  time.Time
	finished bool
	// trace 可选: 有请求轨迹时, usage 会同时镜像进轨迹 —— 这样面板上
	// "这条请求花了多少 token" 不必再去 zen-stats.jsonl 里做二次关联。
	// nil 安全(方法本身容忍 nil)。
	trace *reqTrace
}

func newZenStatsTracker(rec zenStatsRecord) *zenStatsTracker {
	return &zenStatsTracker{rec: rec, started: time.Now()}
}

// newZenStatsTrackerCtx 带请求上下文的构造: 自动回填请求 id、上游名/实际模型与
// token 镜像。所有上游调用点都应优先用它, 保证请求日志与用量记录能互相关联。
func newZenStatsTrackerCtx(ctx context.Context, rec zenStatsRecord) *zenStatsTracker {
	rec.ReqID = reqIDFrom(ctx)
	tr := traceFrom(ctx)
	if tr != nil {
		tr.SetUpstream(rec.Upstream, rec.Model)
		if rec.Stream {
			tr.SetProtocol("", true)
		}
	}
	return &zenStatsTracker{rec: rec, started: time.Now(), trace: tr}
}

func (t *zenStatsTracker) finish(ok bool, status int) {
	if t.finished {
		return
	}
	t.finished = true
	t.rec.OK = ok
	t.rec.Status = status
	t.rec.LatencyMs = time.Since(t.started).Milliseconds()
	recordZenStats(t.rec)
}

// observeUsage 把上游返回的 usage 计入本次记录。
// 流式响应里 usage 可能出现在多个 chunk, 因此重复调用取最后一次的值;
// 上游不给 usage 时保留入站估算, 不让记录变成 0。
func (t *zenStatsTracker) observeUsage(u map[string]any) {
	if t == nil || u == nil {
		return
	}
	// 镜像进请求轨迹(多协议字段名归一在 req_trace.go 里)。
	t.trace.ObserveUsage(u)
	// 记账同样走统一归一(P2-18): 三协议字段名兼容, 推理/缓存单列,
	// cache 读写不重复计入 prompt。
	if p, c, r, cache, ok := parseUsageFields(u); ok {
		if p > 0 {
			t.rec.PromptTokens = p
		}
		t.rec.CompletionTokens = c
		t.rec.ReasoningTokens = r
		t.rec.CacheTokens = cache
		return
	}
	// 兜底: 未识别字段名时沿用旧逻辑
	if v, ok := u["prompt_tokens"].(float64); ok && v > 0 {
		t.rec.PromptTokens = int(v)
	}
	if v, ok := u["completion_tokens"].(float64); ok {
		t.rec.CompletionTokens = int(v)
	}
}

// initStats 幂等且可重试地初始化统计:
//   - 内存聚合对象始终建立, 即便落盘文件打不开也能在内存里统计(面板不再恒为 null);
//   - 日界翻转协程只启动一次;
//   - 落盘文件打开失败仅 log.Printf 并允许后续重试, 不永久关闭统计功能。
func initStats() {
	statsInitMu.Lock()
	defer statsInitMu.Unlock()

	if statsToday == nil {
		statsToday = newZenStatsAgg()
	}
	if statsTotal == nil {
		statsTotal = newZenStatsAgg()
	}

	if !statsRollStarted {
		statsRollStarted = true
		go rollStatsDate()
	}

	// 已成功打开则直接返回(幂等)。
	if statsFile != nil {
		return
	}

	path := statsFilePath
	if path == "" {
		path = kit.ResolveDataPath("zen-stats.jsonl")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		log.Printf("zen stats file open failed (will retry on next record): %v", err)
		return
	}
	statsFileMu.Lock()
	statsFile = f
	statsFileMu.Unlock()

	// 从落盘文件重建的聚合要在 statsAggMu 内一次性 swap 进全局变量,
	// 不能在本函数里直接 aggregateRecord(statsToday, ...) —— 那时 statsInitMu
	// 已释放, recordZenStats 可以并发拿 statsAggMu 写同一张 map。
	total, today := loadStatsFromFile(path)
	statsAggMu.Lock()
	if total != nil {
		statsTotal = total
	}
	if today != nil {
		statsToday = today
	}
	statsAggMu.Unlock()
}

func newZenStatsAgg() *zenStatsAgg {
	return &zenStatsAgg{
		Date:       time.Now().Format("2006-01-02"),
		ByModel:    map[string]*zenStatsModel{},
		ByUpstream: map[string]*zenStatsModel{},
	}
}

// clone 深拷贝一份聚合体, 供 admin 快照使用。
//
// 必须克隆: zenStatsSnapshot 之前的实现是"锁内返回裸指针, 锁外被 marshal",
// 而 admin 拿到 map 后立刻 json.Marshal 遍历 ByModel / ByUpstream, 期间
// recordZenStats 会在 statsAggMu 下继续 aggregateRecord 往同一张 map 里写
// —— Go 运行时对 concurrent map read/write 抛的是 fatal error, recover 无效,
// 进程直接退出。面板开着 + 任何一次成功记账就能触发, 是日常场景。
// 与 config_clone.go 的规矩保持一致: 快照就是"锁内克隆、锁外读克隆"。
func (a *zenStatsAgg) clone() *zenStatsAgg {
	if a == nil {
		return nil
	}
	out := &zenStatsAgg{
		Date:         a.Date,
		Requests:     a.Requests,
		PromptTok:    a.PromptTok,
		CompleteTok:  a.CompleteTok,
		ReasoningTok: a.ReasoningTok,
		CacheTok:     a.CacheTok,
		Compaction:   a.Compaction,
		RateLimited:  a.RateLimited,
		ByModel:      make(map[string]*zenStatsModel, len(a.ByModel)),
		ByUpstream:   make(map[string]*zenStatsModel, len(a.ByUpstream)),
	}
	for k, v := range a.ByModel {
		out.ByModel[k] = cloneStatsModel(v)
	}
	for k, v := range a.ByUpstream {
		out.ByUpstream[k] = cloneStatsModel(v)
	}
	return out
}

func cloneStatsModel(m *zenStatsModel) *zenStatsModel {
	if m == nil {
		return nil
	}
	return &zenStatsModel{
		Requests:     m.Requests,
		PromptTok:    m.PromptTok,
		CompleteTok:  m.CompleteTok,
		ReasoningTok: m.ReasoningTok,
		CacheTok:     m.CacheTok,
	}
}

// statsReadTailBytes 启动重建聚合时最多读取文件尾部的字节数。zen-stats.jsonl 可能
// 非常大, 全量读重建既占内存又慢; 记录按时间顺序追加, 读尾部足够覆盖"今天+昨日"。
const statsReadTailBytes = 64 << 20

// maxZenStatsBytes zen-stats.jsonl 的轮转上限。此前无上限, 长期运行会无限增长;
// 超过则截断成空, 只保留最近记录(诊断统计要的是最近一段, 不值得为历史做日期分文件)。
const maxZenStatsBytes = 64 << 20

// loadStatsFromFile 从 JSONL 重建累计统计(仅今日的计入今日)。
//
// 返回 (total, today): 由调用方在 statsAggMu 内一次性 swap 进全局变量,
// 不在函数内部直接写 statsToday —— 那会与 recordZenStats 在 statsAggMu 下的
// 写并发, 命中 concurrent map write。
func loadStatsFromFile(path string) (*zenStatsAgg, *zenStatsAgg) {
	total := newZenStatsAgg()
	today := newZenStatsAgg()
	data, err := readFileTail(path, statsReadTailBytes)
	if err != nil || len(data) == 0 {
		return nil, nil
	}
	todayDate := time.Now().Format("2006-01-02")
	lines := splitLines(string(data))
	for _, line := range lines {
		var rec zenStatsRecord
		if json.Unmarshal([]byte(line), &rec) != nil {
			continue
		}
		aggregateRecord(total, &rec)
		if time.UnixMilli(rec.TS).Format("2006-01-02") == todayDate {
			aggregateRecord(today, &rec)
		}
	}
	return total, today
}

// readFileTail 读取文件尾部至多 maxBytes 字节, 自动跳过被截断的半行。
// 文件比窗口还小时直接全量读, 避免不必要的 seek。
func readFileTail(path string, maxBytes int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := st.Size()
	if size == 0 {
		return nil, nil
	}
	var raw []byte
	if size <= maxBytes {
		raw, err = io.ReadAll(f)
		if err != nil {
			return nil, err
		}
	} else {
		if _, err = f.Seek(size-maxBytes, io.SeekStart); err != nil {
			return nil, err
		}
		buf := make([]byte, maxBytes)
		n, err := f.Read(buf)
		if err != nil && err != io.EOF {
			return nil, err
		}
		raw = buf[:n]
	}
	// 从第一个 '\n' 之后开始解析, 丢掉那条被截断的半行。
	if i := bytes.IndexByte(raw, '\n'); i >= 0 {
		raw = raw[i+1:]
	}
	return raw, nil
}

func aggregateRecord(agg *zenStatsAgg, rec *zenStatsRecord) {
	if agg == nil || rec == nil {
		return
	}
	agg.Requests++
	agg.PromptTok += int64(rec.PromptTokens)
	agg.CompleteTok += int64(rec.CompletionTokens)
	agg.ReasoningTok += int64(rec.ReasoningTokens)
	agg.CacheTok += int64(rec.CacheTokens)
	agg.Compaction += int64(rec.CompactionTokens)
	agg.RateLimited += int64(rec.RateLimited)
	// 旧记录可能没有 upstream 字段(早期只写了 zen), 归到 zen 而不是空键,
	// 否则「按上游」里会多出一条名为 "" 的行。
	upstream := rec.Upstream
	if upstream == "" {
		upstream = upstreamZen
	}
	bump := func(m map[string]*zenStatsModel, key string) {
		if key == "" {
			return
		}
		e := m[key]
		if e == nil {
			e = &zenStatsModel{}
			m[key] = e
		}
		e.Requests++
		e.PromptTok += int64(rec.PromptTokens)
		e.CompleteTok += int64(rec.CompletionTokens)
		e.ReasoningTok += int64(rec.ReasoningTokens)
		e.CacheTok += int64(rec.CacheTokens)
	}
	bump(agg.ByModel, rec.Model)
	bump(agg.ByUpstream, upstream)
}

func splitLines(s string) []string {
	out := []string{}
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			if i > start {
				out = append(out, s[start:i])
			}
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

func recordZenStats(rec zenStatsRecord) {
	initStats()
	statsFileMu.Lock()
	if statsFile != nil {
		b, err := json.Marshal(rec)
		if err == nil {
			statsFile.Write(append(b, '\n'))
		}
		// 轮转: 超过上限则截断成空, 只保留最近记录(与 cline-proxy.log / requests.jsonl 同思路)。
		// 必须用 os.Truncate 而不是句柄级 Truncate: statsFile 是 O_APPEND 打开的,
		// Windows 下该句柄没有 GENERIC_WRITE, 句柄级截断会 "Access is denied"。
		if st, serr := statsFile.Stat(); serr == nil && st.Size() > maxZenStatsBytes {
			tp := statsFilePath
			if tp == "" {
				tp = kit.ResolveDataPath("zen-stats.jsonl")
			}
			if terr := os.Truncate(tp, 0); terr != nil {
				log.Printf("zen-stats: truncate 失败: %v", terr)
			}
		}
	}
	statsFileMu.Unlock()

	statsAggMu.Lock()
	aggregateRecord(statsToday, &rec)
	aggregateRecord(statsTotal, &rec)
	statsAggMu.Unlock()
}

func rollStatsDate() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			today := time.Now().Format("2006-01-02")
			statsAggMu.Lock()
			if statsToday.Date != today {
				statsToday = newZenStatsAgg()
			}
			statsAggMu.Unlock()
		case <-appRootCtx.Done():
			// 收到退出信号: 停止日期滚动协程, 让进程能够真正停下。
			return
		}
	}
}

func zenStatsSnapshot() map[string]any {
	initStats()
	statsAggMu.Lock()
	defer statsAggMu.Unlock()
	// 锁内克隆, 锁外读克隆 —— 见 zenStatsAgg.clone 的注释。
	return map[string]any{
		"today": statsToday.clone(),
		"total": statsTotal.clone(),
	}
}
