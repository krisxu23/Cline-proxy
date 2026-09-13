package app

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

// 账本日界跟着配置的时区走, 与 Gemini 的太平洋重置点是两个独立时区。
func TestUsageDayBoundaryUsesConfiguredTimezone(t *testing.T) {
	withTestConfig(t, &zenConfigData{Usage: zenUsageConfig{Timezone: "America/Los_Angeles"}})
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatalf("embedded tzdata must resolve the zone: %v", err)
	}
	if got := usageLocation().String(); got != loc.String() {
		t.Fatalf("usageLocation = %s, want %s", got, loc)
	}
	want := time.Now().In(loc).Format("2006-01-02")
	if got := usageDayNow(); got != want {
		t.Fatalf("usageDayNow = %s, want %s", got, want)
	}
	// 未配置时用默认时区, 而不是机器的本地时区
	withTestConfig(t, &zenConfigData{})
	if got := usageTimezone(); got != defaultUsageTimezone {
		t.Fatalf("default timezone = %s, want %s", got, defaultUsageTimezone)
	}
}

// 计数: 每次调用 +1, 成功进 ok, 失败进 fail。
func TestRecordUsageCounters(t *testing.T) {
	withTestConfig(t, &zenConfigData{})
	resetUsageLedger()
	defer resetUsageLedger()

	cand := routeCandidate{Upstream: "gemini", Model: "m1"}
	recordUsageForCandidate(cand, true)
	recordUsageForCandidate(cand, true)
	recordUsageForCandidate(cand, false)
	recordUsageForCandidate(routeCandidate{Upstream: "", Model: ""}, true) // 空候选不入账

	snap := usageSnapshot()
	rows, _ := snap["rows"].([]map[string]any)
	if len(rows) != 1 {
		t.Fatalf("expected exactly one ledger row, got %d: %+v", len(rows), rows)
	}
	r := rows[0]
	if r["req"] != 3 || r["ok"] != 2 || r["fail"] != 1 {
		t.Fatalf("counters = %+v, want req=3 ok=2 fail=1", r)
	}
}

// dailyLimits 到量即判定为"已尽", 未到量不算。
func TestUsageLimitReached(t *testing.T) {
	withTestConfig(t, &zenConfigData{Usage: zenUsageConfig{
		DailyLimits: map[string]int{"gemini:m1": 2},
	}})
	resetUsageLedger()
	defer resetUsageLedger()

	cand := routeCandidate{Upstream: "gemini", Model: "m1"}
	if usageLimitReached(candidateKey(cand.Upstream, cand.Model)) {
		t.Fatal("a fresh candidate must not be over its limit")
	}
	recordUsageForCandidate(cand, true)
	if usageLimitReached(candidateKey(cand.Upstream, cand.Model)) {
		t.Fatal("1/2 must not be over the limit")
	}
	recordUsageForCandidate(cand, true)
	if !usageLimitReached(candidateKey(cand.Upstream, cand.Model)) {
		t.Fatal("2/2 must be over the limit")
	}
	// 限额只作用于自己那个候选
	if usageLimitReached("gemini:other") {
		t.Fatal("limit must not leak to another candidate")
	}
	// 到量的候选在链上被跳过
	if why := candidateSkip(cand); why == "" {
		t.Fatal("over-limit candidate must be skipped by the chain")
	}
}

// 自动回填的限额不能覆盖用户的手动配置。
func TestAutoFillLimitYieldsToManualConfig(t *testing.T) {
	withTestConfig(t, &zenConfigData{Usage: zenUsageConfig{
		DailyLimits: map[string]int{"gemini:manual": 50},
	}})
	resetUsageLedger()
	defer resetUsageLedger()

	autoFillDailyLimit("gemini:manual", 5)
	if got := usageLimitFor("gemini:manual"); got != 50 {
		t.Fatalf("manual config must win: got %d, want 50", got)
	}
	autoFillDailyLimit("gemini:auto", 7)
	if got := usageLimitFor("gemini:auto"); got != 7 {
		t.Fatalf("auto-filled limit = %d, want 7", got)
	}
}

// 超过保留期的日期要被清掉, 且按日期(而非插入顺序)判断。
func TestUsagePruningKeepsRecentDays(t *testing.T) {
	withTestConfig(t, &zenConfigData{Usage: zenUsageConfig{RetentionDays: 2}})
	resetUsageLedger()
	defer resetUsageLedger()

	usageMu.Lock()
	usageDays["2026-09-01"] = map[string]*usageCounter{"a:b": {Req: 1}}
	usageDays["2026-09-02"] = map[string]*usageCounter{"a:b": {Req: 1}}
	usageDays["2026-09-10"] = map[string]*usageCounter{"a:b": {Req: 1}}
	usageDays["2026-09-11"] = map[string]*usageCounter{"a:b": {Req: 1}}
	pruneUsageLocked()
	_, hasOldest := usageDays["2026-09-01"]
	_, hasSecond := usageDays["2026-09-02"]
	_, hasRecent := usageDays["2026-09-11"]
	n := len(usageDays)
	usageMu.Unlock()

	if n != 2 || hasOldest || hasSecond || !hasRecent {
		t.Fatalf("pruning must keep the 2 newest days: n=%d oldest=%v second=%v recent=%v",
			n, hasOldest, hasSecond, hasRecent)
	}
}

// TestSaveUsageLedgerAtomic 验证账本落盘收敛到原子写(临时文件+rename), 落盘后是完整可解析的 JSON。
func TestSaveUsageLedgerAtomic(t *testing.T) {
	usageMu.Lock()
	usageDays = map[string]map[string]*usageCounter{
		"2026-09-13": {"up:model": {Req: 3, OK: 2, Fail: 1}},
	}
	usageLoaded = true
	usageDirty = true
	usageMu.Unlock()
	defer func() {
		usageMu.Lock()
		usageDays = map[string]map[string]*usageCounter{}
		usageLoaded = false
		usageDirty = false
		usageMu.Unlock()
	}()

	saveUsageLedger()

	data, err := os.ReadFile(usageLedgerPath())
	if err != nil {
		t.Fatalf("ledger not written: %v", err)
	}
	var got usageLedgerFile
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("ledger not valid JSON: %v\nraw=%s", err, data)
	}
	if got.Days["2026-09-13"]["up:model"].Req != 3 {
		t.Fatalf("ledger content mismatch: %+v", got)
	}
}

// TestSaveUsageLedgerSkipsWhenNotLoaded 验证 usageLoaded 为假时不落盘, 避免用空表覆盖真实账本。
func TestSaveUsageLedgerSkipsWhenNotLoaded(t *testing.T) {
	usageMu.Lock()
	usageLoaded = false
	usageDirty = true
	usageMu.Unlock()
	defer func() {
		usageMu.Lock()
		usageLoaded = false
		usageDirty = false
		usageMu.Unlock()
	}()

	path := usageLedgerPath()
	os.Remove(path)
	saveUsageLedger()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("should not write ledger when not loaded")
	}
}
