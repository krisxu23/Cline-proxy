package app

import (
	"encoding/json"
	"log"
	"os"
	"sort"
	"sync"
	"time"

	"cline-go-proxy/internal/kit"
)

// 每日配额账本(spec §4.4)。
//
// 维度是 upstream:model, 记当日请求数 / 成功 / 失败, 落盘 data/usage-ledger.json,
// 按配置的时区划分日界, 保留若干天后自动清理。
//
// 用途有两层:
//   - 限额: 配置 dailyLimits 后, 到量的候选当天直接跳过, 不必等撞 429;
//   - 观测: 面板展示"今天哪个模型用了多少", 免费额度还剩多少。
//
// Gemini 的 429 响应里解析出的每日限额会自动回填(优先级低于手动配置)。

const (
	defaultUsageRetentionDays = 7
	defaultUsageTimezone      = "Asia/Shanghai"
	usageFlushInterval        = 30 * time.Second
)

// zenUsageConfig 配置的 usage 段。
type zenUsageConfig struct {
	RetentionDays int            `json:"retentionDays"`
	Timezone      string         `json:"timezone"`
	DailyLimits   map[string]int `json:"dailyLimits,omitempty"`
}

// usageCounter 单个候选在单日的计数。
type usageCounter struct {
	Req  int `json:"req"`
	OK   int `json:"ok"`
	Fail int `json:"fail"`
}

// usageLedgerFile 落盘结构: 日期 -> 候选 -> 计数。
type usageLedgerFile struct {
	Days map[string]map[string]*usageCounter `json:"days"`
}

var (
	usageMu         sync.Mutex
	usageDays       = map[string]map[string]*usageCounter{}
	usageAutoLimits = map[string]int{}
	usageDirty      bool
	usageLoaded     bool
)

func usageConfig() zenUsageConfig {
	if cfg := getZenConfig(); cfg != nil {
		return cfg.Usage
	}
	return zenUsageConfig{}
}

func usageTimezone() string {
	if tz := usageConfig().Timezone; tz != "" {
		return tz
	}
	return defaultUsageTimezone
}

func usageRetentionDays() int {
	if n := usageConfig().RetentionDays; n > 0 {
		return n
	}
	return defaultUsageRetentionDays
}

func usageLocation() *time.Location {
	loc, err := time.LoadLocation(usageTimezone())
	if err != nil || loc == nil {
		return time.Local
	}
	return loc
}

// usageDayNow 账本日界: 按配置时区计算, 与 Gemini 的太平洋重置点是两个时区。
func usageDayNow() string {
	return time.Now().In(usageLocation()).Format("2006-01-02")
}

func usageLedgerPath() string {
	return kit.ResolveDataPath("usage-ledger.json")
}

// loadUsageLedger 启动时载入; 失败只记日志, 不阻塞启动(账本丢一次不影响可用性)。
func loadUsageLedger() {
	path := usageLedgerPath()
	raw, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("  usage: 读取账本失败: %v", err)
		}
		markUsageLoaded()
		return
	}
	var f usageLedgerFile
	if err := json.Unmarshal(raw, &f); err != nil {
		log.Printf("  usage: 账本解析失败(将重建): %v", err)
		markUsageLoaded()
		return
	}
	usageMu.Lock()
	if f.Days != nil {
		usageDays = f.Days
	}
	usageMu.Unlock()
	markUsageLoaded()
}

func markUsageLoaded() {
	usageMu.Lock()
	usageLoaded = true
	usageMu.Unlock()
}

// recordUsageForCandidate 记一次调用结果。req 每次调用都加, ok/fail 二选一。
func recordUsageForCandidate(cand routeCandidate, ok bool) {
	if cand.Upstream == "" || cand.Model == "" {
		return
	}
	day := usageDayNow()
	key := candidateKey(cand.Upstream, cand.Model)

	usageMu.Lock()
	defer usageMu.Unlock()
	if usageDays[day] == nil {
		usageDays[day] = map[string]*usageCounter{}
	}
	c := usageDays[day][key]
	if c == nil {
		c = &usageCounter{}
		usageDays[day][key] = c
	}
	c.Req++
	if ok {
		c.OK++
	} else {
		c.Fail++
	}
	usageDirty = true
}

// usageLimitFor 该候选当日限额; 0 表示不限制。手动配置优先于自动回填。
func usageLimitFor(key string) int {
	if n := usageConfig().DailyLimits[key]; n > 0 {
		return n
	}
	usageMu.Lock()
	defer usageMu.Unlock()
	return usageAutoLimits[key]
}

// usageLimitReached 该候选当日用量是否已达限额。
func usageLimitReached(key string) bool {
	limit := usageLimitFor(key)
	if limit <= 0 {
		return false
	}
	usageMu.Lock()
	defer usageMu.Unlock()
	c := usageDays[usageDayNow()][key]
	return c != nil && c.Req >= limit
}

// autoFillDailyLimit 用上游回包的限额回填(Gemini 429 里的 dailyRequestLimit)。
// 只填没有手动配置的项, 避免覆盖用户的显式设置。
func autoFillDailyLimit(key string, limit int) {
	if key == "" || limit <= 0 {
		return
	}
	if n := usageConfig().DailyLimits[key]; n > 0 {
		return
	}
	usageMu.Lock()
	usageAutoLimits[key] = limit
	usageMu.Unlock()
}

// saveUsageLedger 落盘。调用方需保证 usageLoaded, 避免用空表覆盖真实账本。
func saveUsageLedger() {
	usageMu.Lock()
	if !usageLoaded || !usageDirty {
		usageMu.Unlock()
		return
	}
	pruneUsageLocked()
	days := make(map[string]map[string]*usageCounter, len(usageDays))
	for d, m := range usageDays {
		cp := make(map[string]*usageCounter, len(m))
		for k, v := range m {
			c := *v
			cp[k] = &c
		}
		days[d] = cp
	}
	usageDirty = false
	usageMu.Unlock()

	raw, err := json.Marshal(usageLedgerFile{Days: days})
	if err != nil {
		log.Printf("  usage: 账本序列化失败: %v", err)
		return
	}
	path := usageLedgerPath()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		log.Printf("  usage: 账本写入失败: %v", err)
		return
	}
	// 先写临时文件再改名: 进程被强杀时不会留下半个 JSON。
	if err := os.Rename(tmp, path); err != nil {
		log.Printf("  usage: 账本落盘失败: %v", err)
	}
}

// pruneUsageLocked 丢掉超过保留期的日期(调用方持锁)。
func pruneUsageLocked() {
	keep := usageRetentionDays()
	if keep <= 0 || len(usageDays) <= keep {
		return
	}
	days := make([]string, 0, len(usageDays))
	for d := range usageDays {
		days = append(days, d)
	}
	sort.Strings(days)
	// 按日期字符串排序即时间顺序(YYYY-MM-DD), 保留最后 keep 天
	for _, d := range days[:len(days)-keep] {
		delete(usageDays, d)
	}
}

// startUsageLedger 载入账本并起一个定时落盘。
// 不用"每次请求都写盘": 高并发下会把磁盘打满, 而账本丢最后几十秒并不致命。
func startUsageLedger() {
	loadUsageLedger()
	go func() {
		t := time.NewTicker(usageFlushInterval)
		defer t.Stop()
		for range t.C {
			saveUsageLedger()
		}
	}()
}

// usageSnapshot 面板用: 今日各候选的用量与限额。
func usageSnapshot() map[string]any {
	day := usageDayNow()
	usageMu.Lock()
	rows := make([]map[string]any, 0, len(usageDays[day]))
	for k, c := range usageDays[day] {
		limit := 0
		if n := usageConfig().DailyLimits[k]; n > 0 {
			limit = n
		} else if n := usageAutoLimits[k]; n > 0 {
			limit = n
		}
		row := map[string]any{
			"key":  k,
			"req":  c.Req,
			"ok":   c.OK,
			"fail": c.Fail,
		}
		if limit > 0 {
			row["limit"] = limit
			row["remaining"] = maxInt(0, limit-c.Req)
		}
		rows = append(rows, row)
	}
	usageMu.Unlock()

	sort.Slice(rows, func(i, j int) bool {
		ri, _ := rows[i]["req"].(int)
		rj, _ := rows[j]["req"].(int)
		if ri != rj {
			return ri > rj
		}
		return rows[i]["key"].(string) < rows[j]["key"].(string)
	})
	return map[string]any{
		"day":        day,
		"timezone":   usageTimezone(),
		"retention":  usageRetentionDays(),
		"rows":       rows,
		"autoLimits": autoLimitSnapshot(),
	}
}

func autoLimitSnapshot() map[string]int {
	usageMu.Lock()
	defer usageMu.Unlock()
	out := make(map[string]int, len(usageAutoLimits))
	for k, v := range usageAutoLimits {
		out[k] = v
	}
	return out
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// resetUsageLedger 测试辅助: 清空内存账本。
func resetUsageLedger() {
	usageMu.Lock()
	usageDays = map[string]map[string]*usageCounter{}
	usageAutoLimits = map[string]int{}
	usageDirty = false
	usageLoaded = true
	usageMu.Unlock()
}
