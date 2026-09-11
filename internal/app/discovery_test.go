package app

import (
	"testing"
)

// 默认排除规则要挡住非聊天模型与垂直领域专用模型。
func TestDiscoveryDefaultExcludes(t *testing.T) {
	withTestConfig(t, &zenConfigData{})
	cases := []struct {
		model string
		desc  string
		want  bool
	}{
		{"openai/gpt-oss-120b:free", "", false},
		{"z-ai/glm-5.3-flash:free", "", false},
		{"vendor/speech-tts", "", true},
		{"vendor/sdxl-image-v2", "", true},
		{"google/nano-banana", "", true},
		{"google/lyria-3", "", true},
		{"vendor/whisper-transcribe", "", true},
		{"vendor/robotics-ctl", "", true},
		{"vendor/computer-use-preview", "", true},
		{"vendor/deep-research-pro", "", true},
		{"vendor/model-latest", "", true},
		// 描述文本命中垂直领域措辞同样排除
		{"vendor/gpt-5-clinical", "A clinical focused model for hospitals", true},
		{"vendor/gpt-5-general", "A general purpose chat model", false},
	}
	for _, c := range cases {
		if got := discoveryShouldExclude(c.model, c.desc); got != c.want {
			t.Errorf("discoveryShouldExclude(%q, %q) = %v, want %v", c.model, c.desc, got, c.want)
		}
	}
}

// 配置了排除规则就完全替换默认规则, 而不是叠加。
func TestDiscoveryExcludeConfigReplacesDefaults(t *testing.T) {
	withTestConfig(t, &zenConfigData{Discovery: zenDiscoveryConfig{
		Exclude: zenDiscoveryExclude{ModelPatterns: []string{`^blocked/`}},
	}})
	if !discoveryShouldExclude("blocked/anything", "") {
		t.Fatal("custom pattern must apply")
	}
	// 默认规则被替换后不再生效
	if discoveryShouldExclude("vendor/speech-tts", "") {
		t.Fatal("defaults must be replaced, not merged, when configured")
	}
}

// 发现到的模型只能追加在手动链尾部, 且不重复。
func TestAppendChainTailKeepsManualFirst(t *testing.T) {
	withTestConfig(t, &zenConfigData{
		Providers: map[string]providerConfig{
			"disc": {BaseURL: "https://x.example/v1", APIKey: "sk"},
		},
	})
	resetDiscoveredForTest(t)
	setDiscoveredForTest([]discoveredModel{
		{Provider: "disc", Model: "found-1", AddedAt: "2026-09-10T00:00:00+08:00", OK: true},
		{Provider: "disc", Model: "found-2", AddedAt: "2026-09-11T00:00:00+08:00", OK: true},
	})

	manual := []routeCandidate{
		{Upstream: "gemini", Model: "gemini-3.8-flash"},
		{Upstream: "disc", Model: "found-1"}, // 与发现结果重复
	}
	got := appendChainTail(manual)
	if len(got) != 3 {
		t.Fatalf("expected 3 hops (2 manual + 1 deduped discovered), got %+v", got)
	}
	if got[0] != manual[0] || got[1] != manual[1] {
		t.Fatalf("manual hops must stay first and in order: %+v", got)
	}
	if got[2].Model != "found-2" {
		t.Fatalf("discovered hop must be appended at the tail: %+v", got)
	}
}

// 发现结果按近期用量倒序 —— 用户用得多的排在链尾更前面。
func TestDiscoveredCandidatesRankedByUsage(t *testing.T) {
	withTestConfig(t, &zenConfigData{
		Providers: map[string]providerConfig{
			"disc": {BaseURL: "https://x.example/v1", APIKey: "sk"},
		},
	})
	resetUsageLedger()
	defer resetUsageLedger()
	resetDiscoveredForTest(t)
	setDiscoveredForTest([]discoveredModel{
		{Provider: "disc", Model: "cold", AddedAt: "2026-09-01T00:00:00+08:00", OK: true},
		{Provider: "disc", Model: "hot", AddedAt: "2026-09-05T00:00:00+08:00", OK: true},
		{Provider: "disc", Model: "warm", AddedAt: "2026-09-08T00:00:00+08:00", OK: true},
	})
	// "hot" 用得最多, "warm" 次之, "cold" 没用过
	for i := 0; i < 9; i++ {
		recordUsageForCandidate(routeCandidate{Upstream: "disc", Model: "hot"}, true)
	}
	for i := 0; i < 3; i++ {
		recordUsageForCandidate(routeCandidate{Upstream: "disc", Model: "warm"}, true)
	}

	got := discoveredCandidates()
	if len(got) != 3 {
		t.Fatalf("expected 3 discovered candidates, got %+v", got)
	}
	want := []string{"hot", "warm", "cold"}
	for i, m := range want {
		if got[i].Model != m {
			t.Fatalf("ranking = %+v, want %v", got, want)
		}
	}
}

// 未通过试跑(OK=false)或 provider 已删除的发现结果不进入候选链。
func TestDiscoveredCandidatesFiltersUnusable(t *testing.T) {
	withTestConfig(t, &zenConfigData{
		Providers: map[string]providerConfig{
			"disc": {BaseURL: "https://x.example/v1", APIKey: "sk"},
		},
	})
	resetDiscoveredForTest(t)
	setDiscoveredForTest([]discoveredModel{
		{Provider: "disc", Model: "good", OK: true},
		{Provider: "disc", Model: "never-worked", OK: false},
		{Provider: "gone", Model: "orphan", OK: true},
	})
	got := discoveredCandidates()
	if len(got) != 1 || got[0].Model != "good" {
		t.Fatalf("only usable discovered models may enter the chain: %+v", got)
	}
}

// 永久剔除会随收录文件落盘并在启动时恢复(spec §5.2)。
func TestPermanentRejectionPersistsThroughDiscoverFile(t *testing.T) {
	withTestConfig(t, &zenConfigData{})
	resetCandidateState()
	defer resetCandidateState()

	markCandidatePermanent("gemini", "gone", "该模型无免费层")
	snap := candidatePermSnapshot()
	if snap["gemini:gone"] == "" {
		t.Fatal("snapshot must carry the reason")
	}
	resetCandidateState()
	candidatePermRestore(snap)
	if candidateSkipReason("gemini", "gone") == "" {
		t.Fatal("restore must re-arm the permanent rejection")
	}
}

// --- 测试辅助 ---

func setDiscoveredForTest(ms []discoveredModel) {
	discoveredMu.Lock()
	discoveredModels = ms
	discoveredLoaded = true
	discoveredMu.Unlock()
}

func resetDiscoveredForTest(t *testing.T) {
	t.Helper()
	discoveredMu.Lock()
	prev := discoveredModels
	prevLoaded := discoveredLoaded
	discoveredModels = nil
	discoveredMu.Unlock()
	t.Cleanup(func() {
		discoveredMu.Lock()
		discoveredModels = prev
		discoveredLoaded = prevLoaded
		discoveredMu.Unlock()
	})
}
