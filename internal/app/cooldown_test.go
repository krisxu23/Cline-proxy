package app

import (
	"testing"
	"time"
)

// 错误类别到冷却类别的映射: 每一类都对应"换下一站"的语义。
func TestClassifyCandidateFailure(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"429 限流", 429, `{"error":{"message":"slow down"}}`, classRateLimit},
		// 404 一律永久剔除: 目录里有但此处不提供, 或已下架(spec §4.1)
		{"404 目录里有但此处不提供", 404, `{"error":{"message":"Requested entity was not found."}}`, classPermanent},
		{"403 拒绝", 403, `{"error":{"message":"forbidden"}}`, classForbidden},
		{"401 鉴权", 401, `{"error":{"message":"bad key"}}`, classForbidden},
		{"500 上游错误", 500, `{"error":{"message":"boom"}}`, classServerError},
		{"503 上游不可用", 503, `{"error":{"message":"unavailable"}}`, classServerError},
		{"连接层失败", 0, ``, classTimeout},
		{"400 普通错误", 400, `{"error":{"message":"bad request"}}`, classServerError},
		{"400 非聊天模型", 400, `{"error":{"message":"This model only supports Interactions API"}}`, classPermanent},
		// 无免费层必须是永久剔除, 否则会被反复选中并浪费候选位
		{"无免费层", 429, `{"error":{"message":"Quota exceeded for metric: generativelanguage.googleapis.com/generate_content_free_tier_requests, limit: 0"}}`, classPermanent},
		{"已下架", 404, `{"error":{"message":"This model is no longer available"}}`, classPermanent},
	}
	for _, c := range cases {
		got, _ := classifyCandidateFailure(c.status, []byte(c.body))
		if got != c.want {
			t.Errorf("%s: classifyCandidateFailure(%d) = %q, want %q", c.name, c.status, got, c.want)
		}
	}
}

// 冷却到期后应自动恢复, 不需要任何外部清理动作。
func TestCandidateCooldownExpiry(t *testing.T) {
	prev := getZenConfig()
	defer setConfigForTest(prev)
	resetCandidateState()
	defer resetCandidateState()

	cfg := *prev
	// 把 rateLimit 压到 40ms, 让测试不必真的等 10 分钟
	cfg.CooldownMs = map[string]int64{classRateLimit: 40}
	setConfigForTest(&cfg)

	if why := candidateSkipReason("gemini", "m1"); why != "" {
		t.Fatalf("fresh candidate must be usable, got %q", why)
	}
	markCandidateCooldown("gemini", "m1", classRateLimit, "429")
	if why := candidateSkipReason("gemini", "m1"); why == "" {
		t.Fatal("cooled candidate must be skipped")
	}
	// 其他候选不受影响: 冷却粒度是单个候选, 不是整个上游
	if why := candidateSkipReason("gemini", "m2"); why != "" {
		t.Fatalf("cooldown must not leak to a sibling model, got %q", why)
	}
	if why := candidateSkipReason("zen", "m1"); why != "" {
		t.Fatalf("cooldown must not leak to another upstream, got %q", why)
	}

	time.Sleep(70 * time.Millisecond)
	if why := candidateSkipReason("gemini", "m1"); why != "" {
		t.Fatalf("expired cooldown must release the candidate, got %q", why)
	}
	clearExpiredCandidateCooldowns()
	if n := len(candidateCoolingSnapshot()); n != 0 {
		t.Fatalf("expired entries must be swept, got %d", n)
	}
}

// 配置的 cooldownMs 必须能覆盖默认时长。
func TestCandidateCooldownConfigOverride(t *testing.T) {
	prev := getZenConfig()
	defer setConfigForTest(prev)
	resetCandidateState()
	defer resetCandidateState()

	cfg := *prev
	cfg.CooldownMs = map[string]int64{classRateLimit: 3600 * 1000}
	setConfigForTest(&cfg)

	if got := cooldownDurationMs(classRateLimit); got != 3600*1000 {
		t.Fatalf("config override must win: got %d", got)
	}
	// 未覆盖的类别仍用默认值
	if got := cooldownDurationMs(classServerError); got != defaultCooldownMs[classServerError] {
		t.Fatalf("unset class must fall back to the default: got %d", got)
	}
}

// quotaDay 冷却到提供方时区的下一个日界 —— Google 免费层按太平洋时间重置,
// 与账本的本地日界是两个独立时区。
func TestQuotaDayCooldownEndsAtPacificMidnight(t *testing.T) {
	prev := getZenConfig()
	defer setConfigForTest(prev)
	resetCandidateState()
	defer resetCandidateState()

	setConfigForTest(prev)
	markQuotaDayCooldown("gemini", "m1", pacificTZ, "当日额度耗尽")

	snap := candidateCoolingSnapshot()
	if len(snap) != 1 {
		t.Fatalf("expected one cooling entry, got %d", len(snap))
	}
	if snap[0]["class"] != classQuotaDay {
		t.Fatalf("class = %v, want %s", snap[0]["class"], classQuotaDay)
	}

	tz, err := time.LoadLocation(pacificTZ)
	if err != nil {
		t.Fatalf("embedded tzdata must resolve %s: %v", pacificTZ, err)
	}
	now := time.Now().In(tz)
	want := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, tz).AddDate(0, 0, 1)
	if got := snap[0]["untilUnixMs"].(int64); got != want.UnixMilli() {
		t.Fatalf("reset point = %d, want %d", got, want.UnixMilli())
	}
	if r := nextMidnight(time.Now(), "Asia/Shanghai"); r.Hour() != 0 {
		t.Fatalf("local day boundary must be midnight, got %d", r.Hour())
	}
}

// 永久剔除不受冷却时长影响, 也不会被"清过期"误清掉。
func TestCandidatePermanentRejection(t *testing.T) {
	prev := getZenConfig()
	defer setConfigForTest(prev)
	resetCandidateState()
	defer resetCandidateState()

	setConfigForTest(prev)
	markCandidatePermanent("gemini", "gone", "该模型无免费层")

	why := candidateSkipReason("gemini", "gone")
	if why == "" {
		t.Fatal("permanently rejected candidate must be skipped")
	}
	clearExpiredCandidateCooldowns()
	if candidateSkipReason("gemini", "gone") == "" {
		t.Fatal("sweeping expired cooldowns must not clear permanent rejections")
	}

	// 快照/恢复: discovery 落盘与启动恢复依赖这一对
	snap := candidatePermSnapshot()
	resetCandidateState()
	if candidateSkipReason("gemini", "gone") != "" {
		t.Fatal("reset must drop permanent rejections for a clean slate")
	}
	candidatePermRestore(snap)
	if candidateSkipReason("gemini", "gone") == "" {
		t.Fatal("restore must bring permanent rejections back")
	}
}
