package app

// 错误规则表与流式保活的测试 (P1-10 / P1-11)。

import (
	"bytes"
	"net/http"
	"strings"
	"sync"
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

// 上下文超限信号集补全(P1-3): 参考 CONTEXT_OVERFLOW_SIGNALS 的常见措辞都要命中
// **专属规则名**(面板能看出是超限, 而非笼统的 5xx), 但类别必须是短冷却
// classServerError —— 最常见成因是用户这轮 prompt/上下文本身超长(用户输入问题,
// 与上游健康无关); 规则表先于状态码 switch、任意状态码都命中, 判 classPermanent
// 会让一条超长 prompt 把整个 upstream:model 对**所有用户**停服 24 小时。
func TestMatchErrorRulesContextOverflow(t *testing.T) {
	signals := []string{
		"context overflow",
		"prompt too large",
		"context window",
		"maximum context",
		"exceeds context",
		"input too long",
		"token limit",
		"too many tokens",
		"context length",
		"messages exceed",
		"context_length_exceeded",
		"maximum context length",
	}
	for _, sig := range signals {
		body := `{"error":{"message":"` + sig + `"}}`
		class, note := matchErrorRules(body)
		if class != classServerError {
			t.Errorf("上下文超限信号 %q 应归 serverError(短冷却), got %q", sig, class)
		}
		// 绝不能判 permanent: 那是 24h 停服的类别(P1-3 核心断言)
		if class == classPermanent {
			t.Errorf("上下文超限信号 %q 不得归 permanent(24h 剔除), got %q", sig, class)
		}
		if !strings.Contains(note, "上下文超限") {
			t.Errorf("原因串应带专属规则名, got %q", note)
		}
	}
	// 不相关正文不命中
	if class, _ := matchErrorRules("rate limit exceeded"); class != "" {
		t.Fatal("限流正文不应命中上下文超限")
	}
}

// Cloudflare 指纹拒绝: 显式 error_code + 1010 / browser_signature_banned 命中,
// 裸数字 1010 不命中(避免 port/request-id/model-token 误伤), 绝不进 permanent。
func TestCloudflareFingerprintRejection(t *testing.T) {
	trueCases := []string{
		`{"error":{"error_code":"1010","message":"Access denied"}}`,
		`{"error":{"error-code":1010}}`,
		`error_code: 1010 blocked`,
		`browser_signature_banned`,
		`fingerprint_rejection`,
		// 被 JSON 包裹后转义的引号形式
		`{"error":"{\\"error_code\\":\\"1010\\"}"}`,
	}
	for _, c := range trueCases {
		if !isCloudflareFingerprintRejection(c) {
			t.Errorf("应识别为指纹拒绝: %s", c)
		}
	}
	falseCases := []string{
		"", `{"error":{"message":"access denied"}}`,
		// 裸数字 1010 绝不误伤: port / count / request-id / model token
		`{"error":{"message":"connect to 10.1.1.1010 timeout"}}`,
		`{"error":{"message":"retry after 1010 seconds"}}`,
		`{"error":{"message":"model foo-1010 is not supported"}}`,
		`ray-id: 101010`, // 不是 error_key + 1010
	}
	for _, c := range falseCases {
		if isCloudflareFingerprintRejection(c) {
			t.Errorf("不应识别为指纹拒绝: %s", c)
		}
	}
	// 端到端: 403 + CF 指纹 → fingerprint(短冷却), 绝不 permannt/forbidden 打标
	class, _ := classifyCandidateFailure(403, []byte(`{"error":{"error_code":"1010","message":"browser signature banned"}}`))
	if class != classFingerprint {
		t.Fatalf("403+CF指纹 应归 fingerprint, got %q", class)
	}
	if got := defaultCooldownMs[classFingerprint]; got != 5*60*1000 {
		t.Fatalf("fingerprint 默认冷却应为 5m, got %v", got)
	}
}

// 请求级资源 404(file/item/upload 等)不该冷却模型, 换模型不会让不存在的
// file_id 变合法。model not found / Requested entity 仍是模型级(保持原分类)。
func TestResourceNotFoundClassification(t *testing.T) {
	strCases := []string{
		// 参考 isResourceNotFoundResponse 正则覆盖的真实形态:
		//  file- 前缀 ID + not found / File 单独成词 + not found
		`{"error":{"message":"file-abc123 not found"}}`,
		`{"error":{"message":"File fx_xyz does not exist"}}`,
		`{"error":{"message":"File not found: f_123"}}`,
		`{"error":{"message":"The file 123.txt was not found"}}`, // files? 词 + not found
		`{"error":{"message":"upload not found"}}`,
		`{"error":{"message":"vector store vs_x not found"}}`,
		`{"error":{"message":"item not found"}}`,
	}
	for _, c := range strCases {
		if !isResourceNotFoundResponseStr(c) {
			t.Errorf("应识别为请求资源不存在: %s", c)
		}
	}
	modelCases := []string{
		`{"error":{"message":"model not found"}}`,
		`{"error":{"message":"Requested entity was not found."}}`,
		`{"error":{"message":"Requested model was not found."}}`,
		"", `{"error":{"message":"rate limit"}}`,
	}
	for _, c := range modelCases {
		if isResourceNotFoundResponseStr(c) {
			t.Errorf("不应识别为请求资源不存在(是模型/实体级): %s", c)
		}
	}
	// 端到端 classify: 资源 404 → resourceNotFound(1m 短冷却), 模型 404 仍 model 级。
	class, _ := classifyCandidateFailure(404, []byte(`{"error":{"message":"file-abc123 not found"}}`))
	if class != classResourceNotFound {
		t.Fatalf("资源 404 应归 resourceNotFound, got %q", class)
	}
	if got := defaultCooldownMs[classResourceNotFound]; got != 60*1000 {
		t.Fatalf("resourceNotFound 默认冷却应为 1m, got %v", got)
	}
	// 我方 cooldown_test 已锁定: "Requested entity was not found" 归 classPermanent
	class2, _ := classifyCandidateFailure(404, []byte(`{"error":{"message":"Requested entity was not found."}}`))
	if class2 != classPermanent {
		t.Fatalf("模型/实体 404 应保持 classPermanent, got %q", class2)
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

// mockFlushWriter 实现 http.ResponseWriter + http.Flusher, 供输出侧心跳测试。
// 写出的字节累积到 buf(带锁, 心跳泵与测试主线程并发写)。
type mockFlushWriter struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	header http.Header
	code   int
}

func (m *mockFlushWriter) Header() http.Header {
	if m.header == nil {
		m.header = http.Header{}
	}
	return m.header
}
func (m *mockFlushWriter) Write(p []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.buf.Write(p)
}
func (m *mockFlushWriter) WriteHeader(code int) { m.code = code }
func (m *mockFlushWriter) Flush()               {}
func (m *mockFlushWriter) String() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.buf.String()
}

func TestSSEHeartbeatInjectsOnIdle(t *testing.T) {
	// 心跳 50ms: 距上次真实写出超过间隔时, 向客户端补一个保活帧; 真实数据随后到达。
	mw := &mockFlushWriter{}
	hb := newSSEHeartbeat(mw, mw, 50*time.Millisecond, func() []byte { return []byte("data: {\"heartbeat\":true}\n\n") })
	defer hb.Close()

	// 开局静默 200ms(不写真实数据)→ 期间至少注入一个心跳帧。
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(mw.String(), "heartbeat") {
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(mw.String(), "heartbeat") {
		t.Fatalf("静默期应注入保活帧, got %q", mw.String())
	}
	// 写一条真实帧, 内容必须完整出现在客户端流里(心跳只发往客户端, 与真实帧互不截断)。
	hb.writeFlush([]byte("data: {\"real\":1}\n\n"))
	if !strings.Contains(mw.String(), "real") {
		t.Fatalf("真实数据不能丢, got %q", mw.String())
	}
}

func TestSSEHeartbeatNeverCorruptsRealFrames(t *testing.T) {
	// 核心回归(2026-09-15 事故): 心跳绝不能插进真实帧字节中间。输出侧设计天然成立 ——
	// 心跳与真实帧各自是独立 Write, 且全程同一把锁串行。这里用"一个被拆成两半的巨型
	// 帧"模拟分片传输: 中间有静默期(心跳会来), 两半拼起来必须是原样的合法 JSON 帧。
	mw := &mockFlushWriter{}
	hb := newSSEHeartbeat(mw, mw, 30*time.Millisecond, func() []byte { return []byte("data: {\"heartbeat\":true}\n\n") })
	defer hb.Close()

	half := `data: {"id":"chatcmpl-x","choices":[{"delta":{"content":"AB`
	rest := `CD"}}]}` + "\n\n"
	hb.writeFlush([]byte(half))
	// 事件中途静默 120ms(远超 30ms 心跳间隔): 新语义下心跳**不得**出现在两半之间
	// —— 这正是旧 heartbeatReader 截帧事故的反向断言(旧实现在这里必插帧)。
	time.Sleep(120 * time.Millisecond)
	hb.writeFlush([]byte(rest))
	out := mw.String()
	if !strings.Contains(out, half) || !strings.Contains(out, rest) {
		t.Fatalf("两半必须原样出现在输出里, got %q", out)
	}
	if iHalf, iHb := strings.Index(out, half), strings.Index(out, "heartbeat"); iHb >= 0 && iHb > iHalf {
		t.Fatalf("事件中途不得插入心跳帧(旧截帧缺陷回归), got %q", out)
	}
	joined := half + rest
	if !strings.Contains(out, joined) {
		t.Fatalf("两半之间不得有任何字节(心跳/空白), 真实帧必须完整连续, got %q", out)
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
