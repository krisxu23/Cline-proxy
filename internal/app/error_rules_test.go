package app

// 错误规则表与流式保活的测试 (P1-10 / P1-11)。

import (
	"strings"
	"testing"
	"time"
)

func TestMatchErrorRulesGeoBlock(t *testing.T) {
	// opencode zen 的地区封锁正文
	class, note := matchErrorRules(`{"type":"error","error":{"type":"RegionError","message":"Model is not available in your region"}}`)
	if class != classGeoBlocked {
		t.Fatalf("RegionError 应归入 geoBlocked, got %q", class)
	}
	if !strings.Contains(note, "地区封锁") {
		t.Fatalf("原因串应带规则名, got %q", note)
	}
	// Cloudflare 1010 指纹拒绝: 即便文案像地区封锁也必须被豁免
	if class, _ := matchErrorRules("error code: 1010 ray-id: xxx region"); class == classGeoBlocked {
		t.Fatal("CF 1010 应被豁免, 不算地区封锁")
	}
	if class, _ := matchErrorRules("just a moment... region not supported"); class == classGeoBlocked {
		t.Fatal("质询页应被豁免, 不算地区封锁")
	}
	// 不相关正文不命中
	if class, _ := matchErrorRules("rate limit exceeded for model"); class == classGeoBlocked {
		t.Fatal("限流正文不应命中地区规则")
	}
	if class, _ := matchErrorRules(""); class != "" {
		t.Fatal("空正文不命中")
	}
}

func TestClassifyCandidateFailureGeoBlock(t *testing.T) {
	// 端到端: 403 + RegionError → geoBlocked(此前是 forbidden, 60 分钟);
	// 冷却表里 geoBlocked 应为 24 小时。
	class, _ := classifyCandidateFailure(403, []byte(`{"error":{"type":"RegionError","message":"region not supported"}}`))
	if class != classGeoBlocked {
		t.Fatalf("403+RegionError 应归入 geoBlocked, got %q", class)
	}
	if got := defaultCooldownMs[classGeoBlocked]; got != int64(24*60*60*1000) {
		t.Fatalf("geoBlocked 默认冷却应为 24h, got %v ms", got)
	}
	// 普通 403(鉴权失败)仍归 forbidden
	if class, _ := classifyCandidateFailure(403, []byte(`{"error":{"message":"invalid api key"}}`)); class != classForbidden {
		t.Fatalf("普通 403 应仍归 forbidden, got %q", class)
	}
}

func TestHeartbeatReaderInjectsOnSilence(t *testing.T) {
	// 上游 500ms 才出一字节, 心跳 50ms: 期间应注入至少一个保活帧
	frame := []byte("data: {\"heartbeat\":true}\n\n")
	slow := &slowReader{delay: 500 * time.Millisecond, chunks: [][]byte{[]byte("data: {\"real\":1}\n\n")}}
	h := newHeartbeatReader(slow, 50*time.Millisecond, func() []byte { return frame })
	defer h.Close()

	buf := make([]byte, 4096)
	var got strings.Builder
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		n, err := h.Read(buf)
		got.Write(buf[:n])
		if err != nil || strings.Contains(got.String(), "real") {
			break
		}
	}
	out := got.String()
	if !strings.Contains(out, "heartbeat") {
		t.Fatalf("静默期应注入保活帧, got %q", out)
	}
	if !strings.Contains(out, "real") {
		t.Fatalf("真实数据不能丢, got %q", out)
	}
}

func TestHeartbeatReaderNoInjectMidLine(t *testing.T) {
	// 上游先发半行(data: {...)再长时间静默: 保活帧不得插进行中间。
	// (半行之前的静默期注入保活帧是合法且期望的行为 —— 流开始时必然在行边界。)
	frame := []byte("data: {\"heartbeat\":true}\n\n")
	slow := &slowReader{delay: 500 * time.Millisecond, chunks: [][]byte{[]byte("data: {\"partial\":"), []byte("1}\n\n")}}
	h := newHeartbeatReader(slow, 50*time.Millisecond, func() []byte { return frame })
	defer h.Close()

	buf := make([]byte, 4096)
	var got strings.Builder
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		n, err := h.Read(buf)
		got.Write(buf[:n])
		if err != nil || strings.Contains(got.String(), "1}\n\n") {
			break
		}
	}
	out := got.String()
	// 关键断言: 半行与续行必须原样拼接成完整行 —— 这证明没有帧被插进行中间。
	if !strings.Contains(out, "data: {\"partial\":1}\n\n") {
		t.Fatalf("半行+续行必须原样拼接(不得插入保活帧), got %q", out)
	}
}

// slowReader 按给定延迟逐段吐数据, 吐完阻塞(模拟上游停摆)。
type slowReader struct {
	delay  time.Duration
	chunks [][]byte
	i      int
}

func (s *slowReader) Read(p []byte) (int, error) {
	if s.i >= len(s.chunks) {
		time.Sleep(time.Hour) // 模拟上游永久停摆, 由测试的 deadline 兜底
		return 0, nil
	}
	c := s.chunks[s.i]
	s.i++
	time.Sleep(s.delay)
	n := copy(p, c)
	return n, nil
}

func TestStreamHeartbeatIntervalConfig(t *testing.T) {
	cfg := getZenConfig()
	backup := cfg.clone()
	defer setZenConfig(cfg)

	next := cfg.clone()
	next.StreamHeartbeatSecs = 30
	setZenConfig(next)
	if got := streamHeartbeatInterval(); got != 30*time.Second {
		t.Fatalf("配置 30 应生效, got %v", got)
	}

	next = getZenConfig().clone()
	next.StreamHeartbeatSecs = 0
	setZenConfig(next)
	if got := streamHeartbeatInterval(); got != 0 {
		t.Fatalf("配置 0 应关闭保活, got %v", got)
	}

	_ = backup
}

func TestMigrateZenConfigV2HeartbeatDefault(t *testing.T) {
	cfg := &zenConfigData{Enabled: true, SchemaVersion: 1, SubsRefreshMins: 30, MaxConcurrency: 8, Retries: 3,
		FailoverCount: 3, FailoverMinutes: 5, ProxyStrategy: "round_robin", ExitMode: "proxy"}
	if migrateZenConfig(cfg) != true {
		t.Fatal("v1 配置应迁移到 v2")
	}
	if cfg.SchemaVersion != zenConfigSchemaVersion {
		t.Fatalf("应升到 v%d", zenConfigSchemaVersion)
	}
	if cfg.StreamHeartbeatSecs != 15 {
		t.Fatalf("v2 应回填心跳默认 15s, got %d", cfg.StreamHeartbeatSecs)
	}
}
