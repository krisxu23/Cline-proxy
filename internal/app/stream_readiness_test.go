package app

import (
	"io"
	"strings"
	"testing"
)

// 照抄 OmniRoute open-sse/utils/streamReadiness.ts 的行为级测试。
//
// 判定链（:130-139 / :328-352）：
//   1. ping/keepalive/heartbeat 事件跳过（event: 行 + payload 的 type 字段都查）
//   2. 空 data 与 [DONE] 跳过
//   3. JSON 解析成功 → hasNonPingStructuredPayload：
//        a. 空对象 {} → false
//        b. error-only 帧（有 error 键、无任何 CONTENT_BEARING_KEYS）→ false
//        c. 其余 → true
//   4. JSON 解析失败 → 非空字符串即 true
//
// ★ hasStreamReadinessSignal 是**提交前预检**：error-only 帧不算就绪。
//   hasUsefulStreamContent 是**提交后观测**：error 算有用内容（客户端该看到）。
//   两者对 error-only 帧的判定**故意相反**，测试分别锁定。

// --- A. hasStreamReadinessSignal：就绪判定 --------------------------------

func TestHasStreamReadinessSignal_空与终止(t *testing.T) {
	cases := []struct {
		name string
		text string
	}{
		{"空字符串", ""},
		{"只有空行", "\n\n\n"},
		{"只有注释行", ": this is a comment\n\n"},
		{"只有DONE", "data: [DONE]\n\n"},
		{"只有空data", "data: \n\n"},
		{"空JSON对象", "data: {}\n\n"},
		{"JSON null", "data: null\n\n"},
		{"空数组", "data: []\n\n"},
		{"只有空白data", "data:    \n\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if hasStreamReadinessSignal(c.text) {
				t.Fatalf("hasStreamReadinessSignal(%q) = true, want false", c.text)
			}
		})
	}
}

func TestHasStreamReadinessSignal_有效内容(t *testing.T) {
	cases := []struct {
		name string
		text string
	}{
		{"OpenAI delta", "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"},
		{"Claude content_block_start", "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"content_block\":{\"type\":\"text\"}}\n\n"},
		{"Gemini candidates", "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hi\"}]}}]}\n\n"},
		{"Responses output", "data: {\"type\":\"output_text.delta\",\"output\":[{\"content\":\"hi\"}]}\n\n"},
		{"纯工具调用", "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"function\":{\"name\":\"f\"}}]}}]}\n\n"},
		{"reasoning_content", "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"think\"}}]}\n\n"},
		{"thinking数组", "data: {\"choices\":[{\"delta\":{\"thinking\":[\"step\"]}}]}\n\n"},
		{"非JSON文本", "data: hello world\n\n"},
		{"非JSON数字", "data: 42\n\n"},
		{"非空字符串payload", "data: \"str\"\n\n"},
		{"非空数组", "data: [1,2,3]\n\n"},
		{"error加choices", "data: {\"error\":{\"message\":\"x\"},\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"},
		{"compaction条目", "data: {\"type\":\"compaction\",\"encrypted_content\":\"abc123\"}\n\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if !hasStreamReadinessSignal(c.text) {
				t.Fatalf("hasStreamReadinessSignal(%q) = false, want true", c.text)
			}
		})
	}
}

// ★ 反直觉项：error-only 帧不算就绪（#7503 的核心修复点）。
func TestHasStreamReadinessSignal_errorOnly帧不算就绪(t *testing.T) {
	cases := []struct {
		name string
		text string
	}{
		{"error对象", "data: {\"error\":{\"message\":\"rate limit exceeded\"}}\n\n"},
		{"error字符串", "data: {\"error\":\"rate limit\"}\n\n"},
		{"error空对象", "data: {\"error\":{}}\n\n"},
		{"error带无关键", "data: {\"error\":{\"message\":\"x\"},\"id\":\"abc\"}\n\n"},
		{"Claude error事件", "event: error\ndata: {\"type\":\"error\",\"error\":{\"message\":\"overloaded\"}}\n\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if hasStreamReadinessSignal(c.text) {
				t.Fatalf("error-only 帧 %q 不应判为就绪", c.text)
			}
		})
	}
}

// ★ ping 事件两种形态都跳过：event: 行 / payload 的 type 字段。
func TestHasStreamReadinessSignal_ping事件跳过(t *testing.T) {
	cases := []struct {
		name string
		text string
	}{
		{"event行ping", "event: ping\ndata: {}\n\n"},
		{"event行keepalive", "event: keepalive\ndata: {}\n\n"},
		{"event行heartbeat", "event: heartbeat\ndata: {}\n\n"},
		{"event行大写", "event: PING\ndata: {}\n\n"},
		{"type字段ping", "data: {\"type\":\"ping\"}\n\n"},
		{"type字段keepalive", "data: {\"type\":\"keepalive\"}\n\n"},
		{"object字段ping", "data: {\"object\":\"ping\"}\n\n"},
		{"ping后接有效帧", "event: ping\ndata: {}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := hasStreamReadinessSignal(c.text)
			// 前 7 项期望 false；最后一项期望 true（ping 后确有有效帧）
			want := c.name == "ping后接有效帧"
			if got != want {
				t.Fatalf("hasStreamReadinessSignal(%q) = %v, want %v", c.text, got, want)
			}
		})
	}
}

// ★ 多行 data 拼接：SSE 允许一个事件的 data 跨多行，用 \n 拼接成完整 JSON。
func TestHasStreamReadinessSignal_多行data拼接(t *testing.T) {
	text := "data: {\"choices\":[{\"delta\":\ndata: {\"content\":\"hi\"}}]}\n\n"
	if !hasStreamReadinessSignal(text) {
		t.Fatalf("跨行 data 拼接后是有效帧，应判就绪: %q", text)
	}
}

// ★ \r\n 行尾：SSE 规范允许 CRLF，状态机必须正确处理。
func TestHasStreamReadinessSignal_crlf(t *testing.T) {
	if hasStreamReadinessSignal("data: {}\r\n\r\n") {
		t.Fatal("CRLF 空对象不应判就绪")
	}
	if !hasStreamReadinessSignal("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\r\n\r\n") {
		t.Fatal("CRLF 有效帧应判就绪")
	}
}

// ★ 无尾行终止符：最后一个事件没有 \n\n 收尾，finishStreamReadinessSignal 也要处理。
func TestHasStreamReadinessSignal_无尾行终止符(t *testing.T) {
	if !hasStreamReadinessSignal("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}") {
		t.Fatal("无尾行终止符的有效帧应判就绪")
	}
	if hasStreamReadinessSignal("data: {}") {
		t.Fatal("无尾行终止符的空对象不应判就绪")
	}
	// 无尾行终止符的 error-only 帧也不算就绪
	if hasStreamReadinessSignal("data: {\"error\":{\"message\":\"x\"}}") {
		t.Fatal("无尾行终止符的 error-only 帧不应判就绪")
	}
}

// ★ 分块喂入：appendStreamReadinessSignal 必须正确跨 chunk 拼接。
func TestAppendStreamReadinessSignal_分块(t *testing.T) {
	full := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"
	// 按 7 字节一块喂
	st := &streamReadinessState{}
	ready := false
	for i := 0; i < len(full); i += 7 {
		end := i + 7
		if end > len(full) {
			end = len(full)
		}
		if appendStreamReadinessSignal(st, full[i:end]) {
			ready = true
			break
		}
	}
	if !ready {
		// 可能正好在最后一块的 finish 阶段才就绪
		if !finishStreamReadinessSignal(st) {
			t.Fatal("分块喂入的有效帧最终应判就绪")
		}
	}
}

// --- B. upstreamDiagnostic 捕获 -------------------------------------------

// ★ error-only 帧的诊断信息要存进 upstreamDiagnostic，供换站日志。
func TestUpstreamDiagnostic_捕获(t *testing.T) {
	cases := []struct {
		name string
		text string
		want string
	}{
		{"error.message", "data: {\"error\":{\"message\":\"rate limit exceeded\"}}\n\n", "rate limit exceeded"},
		{"error字符串", "data: {\"error\":\"bad gateway\"}\n\n", "bad gateway"},
		{"Claude error事件", "event: error\ndata: {\"type\":\"error\",\"error\":{\"message\":\"overloaded\"}}\n\n", "overloaded"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := &streamReadinessState{}
			appendStreamReadinessSignal(st, c.text)
			finishStreamReadinessSignal(st)
			if st.upstreamDiagnostic != c.want {
				t.Fatalf("upstreamDiagnostic = %q, want %q", st.upstreamDiagnostic, c.want)
			}
			// 有效帧不应设置诊断
		})
	}
}

func TestUpstreamDiagnostic_有效帧不设置(t *testing.T) {
	st := &streamReadinessState{}
	appendStreamReadinessSignal(st, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
	finishStreamReadinessSignal(st)
	if st.upstreamDiagnostic != "" {
		t.Fatalf("有效帧不应设置诊断, got %q", st.upstreamDiagnostic)
	}
}

// ★ 只捕获第一个诊断（后续 error-only 帧不覆盖）。
func TestUpstreamDiagnostic_只捕获第一个(t *testing.T) {
	st := &streamReadinessState{}
	appendStreamReadinessSignal(st, "data: {\"error\":{\"message\":\"first\"}}\n\ndata: {\"error\":{\"message\":\"second\"}}\n\n")
	finishStreamReadinessSignal(st)
	if st.upstreamDiagnostic != "first" {
		t.Fatalf("upstreamDiagnostic = %q, want %q", st.upstreamDiagnostic, "first")
	}
}

// --- C. hasUsefulStreamContent：提交后观测 --------------------------------

// ★ error 帧的有用性按 OmniRoute hasUsefulValue 递归判定:
//   - {"error":"字符串"} → usefulValueKeys 命中 error 且为非空字符串 → true
//   - {"error":{"message":"x"}} → error 是 record, 递归后 message 不在键表里 → false
//     (它与就绪判定一致: error-only 帧不算内容; 客户端侧的错误可见性由
//     frameHasStructuredStreamError / SawError 承担, 不由这里管)
func TestHasUsefulStreamContent_error帧(t *testing.T) {
	if !hasUsefulStreamContent("data: {\"error\":\"rate limit\"}\n\n") {
		t.Fatal("error 为非空字符串时应算有用内容")
	}
	if hasUsefulStreamContent("data: {\"error\":{\"message\":\"rate limit\"}}\n\n") {
		t.Fatal("error 只含 message 子对象时不算有用内容(message 不在 usefulValueKeys)")
	}
	if !hasUsefulStreamContent("data: {\"error\":{\"message\":\"x\",\"text\":\"y\"}}\n\n") {
		t.Fatal("error 子对象里的 text 键应算有用内容")
	}
	// 反证: 带 text 的 error 对象确实算(走的是 text 键)
	if !hasUsefulStreamContent("data: {\"error\":{\"text\":\"y\"}}\n\n") {
		t.Fatal("error 子对象含 usefulValueKeys 键时应算有用内容")
	}
}

func TestHasUsefulStreamContent_基础(t *testing.T) {
	falseCases := []struct {
		name string
		text string
	}{
		{"空", ""},
		{"只有DONE", "data: [DONE]\n\n"},
		{"空data", "data: \n\n"},
		{"空对象", "data: {}\n\n"},
		{"ping事件", "event: ping\ndata: {}\n\n"},
		{"keepalive事件行", "event: keepalive\ndata: {}\n\n"},
		{"注释行", ": comment\n\n"},
	}
	for _, c := range falseCases {
		t.Run(c.name, func(t *testing.T) {
			if hasUsefulStreamContent(c.text) {
				t.Fatalf("hasUsefulStreamContent(%q) = true, want false", c.text)
			}
		})
	}
	trueCases := []struct {
		name string
		text string
	}{
		{"delta内容", "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"},
		{"非JSON文本", "data: plain text\n\n"},
		{"reasoning", "data: {\"choices\":[{\"delta\":{\"reasoning\":\"think\"}}]}\n\n"},
	}
	for _, c := range trueCases {
		t.Run(c.name, func(t *testing.T) {
			if !hasUsefulStreamContent(c.text) {
				t.Fatalf("hasUsefulStreamContent(%q) = false, want true", c.text)
			}
		})
	}
}

// --- D. frameHasStructuredStreamError -------------------------------------

func TestFrameHasStructuredStreamError(t *testing.T) {
	trueCases := []struct {
		name  string
		frame string
	}{
		{"event error行", "event: error\ndata: {\"type\":\"error\"}\n\n"},
		{"type error", "data: {\"type\":\"error\"}\n\n"},
		{"event response.failed", "event: response.failed\ndata: {}\n\n"},
		{"type response.failed", "data: {\"type\":\"response.failed\"}\n\n"},
		{"error对象带message", "data: {\"error\":{\"message\":\"x\"}}\n\n"},
		// ★ 参考实现 isSubstantiveErrorValue 要求对象非空: {} 是空对象 → false
		{"error字符串", "data: {\"error\":\"x\"}\n\n"},
		{"response failed嵌套", "data: {\"response\":{\"status\":\"failed\",\"error\":{\"message\":\"x\"}}}\n\n"},
	}
	for _, c := range trueCases {
		t.Run(c.name, func(t *testing.T) {
			if !frameHasStructuredStreamError(c.frame) {
				t.Fatalf("frameHasStructuredStreamError(%q) = false, want true", c.frame)
			}
		})
	}
	falseCases := []struct {
		name  string
		frame string
	}{
		{"空", ""},
		{"普通delta", "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"},
		{"DONE", "data: [DONE]\n\n"},
		{"error为null", "data: {\"error\":null}\n\n"},
		{"error空对象", "data: {\"error\":{}}\n\n"},
		{"非JSON", "data: not json\n\n"},
		{"ping事件", "event: ping\ndata: {\"type\":\"ping\"}\n\n"},
		{"response非failed", "data: {\"response\":{\"status\":\"completed\"}}\n\n"},
		{"注释行", ": comment\n\n"},
	}
	for _, c := range falseCases {
		t.Run(c.name, func(t *testing.T) {
			if frameHasStructuredStreamError(c.frame) {
				t.Fatalf("frameHasStructuredStreamError(%q) = true, want false", c.frame)
			}
		})
	}
}

// --- E. streamContentWatcher ----------------------------------------------

func TestStreamContentWatcher_基本内容(t *testing.T) {
	w := newStreamContentWatcher()
	w.Note("data: {\"choices\":[{\"delta\":{\"content\":\"h\"}")
	w.Note("}\n\n")
	w.Note("data: {\"choices\":[{\"delta\":{\"content\":\"i\"}}]}\n\n")
	w.Finish()
	if !w.SawContent() {
		t.Fatal("应观测到内容")
	}
	if !w.SawSseFrame() {
		t.Fatal("应观测到 SSE 帧")
	}
	if w.SawError() {
		t.Fatal("不应有错误")
	}
	if w.SawLegitEmptyTerminal() {
		t.Fatal("不应有合法空终止")
	}
}

func TestStreamContentWatcher_合法空终止(t *testing.T) {
	w := newStreamContentWatcher()
	w.Note("data: {\"choices\":[{\"finish_reason\":\"length\"}]}\n\n")
	w.Finish()
	if !w.SawLegitEmptyTerminal() {
		t.Fatal("finish_reason=length 是合法空终止")
	}
	if w.SawContent() {
		t.Fatal("该帧无内容")
	}
}

func TestStreamContentWatcher_stop_reason(t *testing.T) {
	w := newStreamContentWatcher()
	w.Note("data: {\"stop_reason\":\"tool_use\"}\n\n")
	w.Finish()
	if !w.SawLegitEmptyTerminal() {
		t.Fatal("stop_reason=tool_use 是合法空终止")
	}
}

func TestStreamContentWatcher_非法空终止(t *testing.T) {
	w := newStreamContentWatcher()
	w.Note("data: {\"choices\":[{\"finish_reason\":\"stop\"}]}\n\n")
	w.Finish()
	if w.SawLegitEmptyTerminal() {
		t.Fatal("finish_reason=stop 不是合法空终止（可能是假空）")
	}
}

func TestStreamContentWatcher_错误(t *testing.T) {
	w := newStreamContentWatcher()
	w.Note("event: error\ndata: {\"type\":\"error\",\"error\":{\"message\":\"x\"}}\n\n")
	w.Finish()
	if !w.SawError() {
		t.Fatal("应观测到结构化错误")
	}
	// ★ error 子对象只含 message(不在 usefulValueKeys) → 不算内容:
	//   错误的可见性由 SawError 承担, SawContent 只认真实输出。
	if w.SawContent() {
		t.Fatal("只含 message 的 error 帧不应算内容")
	}
	// error 为非空字符串时既算错误也算内容
	w2 := newStreamContentWatcher()
	w2.Note("data: {\"error\":\"boom\"}\n\n")
	w2.Finish()
	if !w2.SawError() {
		t.Fatal("error 非空字符串应算结构化错误")
	}
	if !w2.SawContent() {
		t.Fatal("error 非空字符串应算有用内容")
	}
}

func TestStreamContentWatcher_ping跳过(t *testing.T) {
	w := newStreamContentWatcher()
	w.Note("event: ping\ndata: {}\n\n")
	w.Note("event: keepalive\ndata: {}\n\n")
	w.Finish()
	if w.SawContent() {
		t.Fatal("ping/keepalive 不算内容")
	}
	if w.SawLegitEmptyTerminal() {
		t.Fatal("ping 不算合法空终止")
	}
}

// ★ 超大单帧：超过缓冲上限时仍要扫描（只可能偏向 sawContent=true，不偏向假空）。
func TestStreamContentWatcher_超大帧(t *testing.T) {
	w := newStreamContentWatcher()
	// 造一个超过 streamWatcherMaxBuffered 的帧，内容在最后
	var sb strings.Builder
	sb.WriteString("data: {\"choices\":[{\"delta\":{\"content\":\"")
	sb.WriteString(strings.Repeat("a", streamWatcherMaxBuffered))
	sb.WriteString("\"}}]}")
	frame := sb.String()
	w.Note(frame + "\n\n")
	w.Finish()
	if !w.SawContent() {
		t.Fatal("超大帧的内容不应被丢失")
	}
}

// --- F. sanitizeErrorMessage ----------------------------------------------

func TestSanitizeErrorMessage(t *testing.T) {
	cases := []struct {
		name    string
		message any
		want    string
	}{
		{"nil", nil, ""},
		{"空字符串", "", ""},
		{"简单", "rate limit exceeded", "rate limit exceeded"},
		{"多行只取首行", "first line\nsecond line", "first line"},
		// ★ looksLikeAbsolutePath 只认源码扩展名(ts/tsx/js/jsx/mjs/cjs),
		//   .go / .json 等不脱敏 —— 参考实现 error.ts:24 的 SOURCE_EXT
		{"Unix源码路径脱敏", "failed at /home/user/project/main.ts:12", "failed at <path>"},
		{"Windows源码路径脱敏", "failed at C:\\Users\\admin\\app\\main.tsx:12", "failed at <path>"},
		{"非源码路径不脱敏", "failed at /home/user/project/file.go:12", "failed at /home/user/project/file.go:12"},
		// ★ 敏感词键表是 api_key|access_token|authorization|cookie|secret
		//   (error.ts:46-53), password / 裸 token 不在其中
		{"api_key脱敏", "auth failed api_key=secret12345", "auth failed api_key=[REDACTED]"},
		{"access_token脱敏", "error access_token=abcdef123456 invalid", "error access_token=[REDACTED] invalid"},
		{"secret脱敏", "config secret=s3cr3tpassword here", "config secret=[REDACTED] here"},
		{"password不脱敏", "auth failed password=secret12345", "auth failed password=secret12345"},
		// ★ Bearer 会被两条规则各命中一次: authorization=... 先脱成
		//   "Authorization: [REDACTED]", 再被 Bearer 规则脱掉后半段
		{"Bearer脱敏", "Authorization: Bearer sk-1234567890", "Authorization: [REDACTED] [REDACTED]"},
		{"数字", float64(42), "42"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := sanitizeErrorMessage(c.message)
			if got != c.want {
				t.Fatalf("sanitizeErrorMessage(%v) = %q, want %q", c.message, got, c.want)
			}
		})
	}
}

// ★ 截断：超过 maxErrorLen 的先截断再取首行。
func TestSanitizeErrorMessage_截断(t *testing.T) {
	long := strings.Repeat("x", maxErrorLen+100)
	got := sanitizeErrorMessage(long)
	if len(got) > maxErrorLen {
		t.Fatalf("截断后长度 %d 仍超过上限 %d", len(got), maxErrorLen)
	}
	// 截断在取首行之前，结果应是 maxErrorLen 个 x
	if got != strings.Repeat("x", maxErrorLen) {
		t.Fatalf("截断结果长度 %d 与预期不符", len(got))
	}
}

// ★ 截断与首行的顺序：先截断到 maxErrorLen，再取首行。
func TestSanitizeErrorMessage_截断后取首行(t *testing.T) {
	long := strings.Repeat("x", maxErrorLen+50) + "\nsecond"
	got := sanitizeErrorMessage(long)
	if strings.Contains(got, "second") {
		t.Fatal("应只保留首行")
	}
	if len(got) != maxErrorLen {
		t.Fatalf("长度 %d, want %d", len(got), maxErrorLen)
	}
}

// --- G. hasUsefulValue 递归细节 -------------------------------------------

// ★ compaction 特例：encrypted_content 非空才算，空的仍须让空内容守卫触发。
func TestHasUsefulValue_compaction(t *testing.T) {
	if !hasUsefulValue(map[string]any{"type": "compaction", "encrypted_content": "abc"}) {
		t.Fatal("compaction + 非空 encrypted_content 应算有效")
	}
	if hasUsefulValue(map[string]any{"type": "compaction", "encrypted_content": ""}) {
		t.Fatal("compaction + 空 encrypted_content 不应算有效（#8649 空内容守卫）")
	}
	if hasUsefulValue(map[string]any{"type": "compaction"}) {
		t.Fatal("compaction 无 encrypted_content 不应算有效")
	}
}

// ★ 嵌套递归：usefulNestedKeys 整体递归判。
func TestHasUsefulValue_嵌套(t *testing.T) {
	if !hasUsefulValue(map[string]any{
		"choices": []any{map[string]any{"delta": map[string]any{"content": "hi"}}},
	}) {
		t.Fatal("嵌套 choices.delta.content 应算有效")
	}
	// 嵌套但值全空 → 无效
	if hasUsefulValue(map[string]any{
		"choices": []any{map[string]any{"delta": map[string]any{"content": ""}}},
	}) {
		t.Fatal("嵌套但内容为空不应算有效")
	}
}

// ★ hasNonPingStructuredPayload 的数组分支：非空数组即 true。
func TestHasNonPingStructuredPayload_数组(t *testing.T) {
	if !hasNonPingStructuredPayload([]any{1, 2}, "") {
		t.Fatal("非空数组应算有效")
	}
	if hasNonPingStructuredPayload([]any{}, "") {
		t.Fatal("空数组不应算有效")
	}
}

// ★ getPayloadType 的键优先级：type > event > object > eventType。
func TestGetPayloadType(t *testing.T) {
	if got := getPayloadType(map[string]any{"type": "a", "event": "b", "object": "c"}, ""); got != "a" {
		t.Fatalf("type 优先, got %q", got)
	}
	if got := getPayloadType(map[string]any{"event": "b", "object": "c"}, ""); got != "b" {
		t.Fatalf("event 次之, got %q", got)
	}
	if got := getPayloadType(map[string]any{"object": "c"}, ""); got != "c" {
		t.Fatalf("object 再次, got %q", got)
	}
	if got := getPayloadType(map[string]any{}, "evt"); got != "evt" {
		t.Fatalf("回退到 eventType, got %q", got)
	}
	// 非 map 回退到 eventType
	if got := getPayloadType("str", "evt"); got != "evt" {
		t.Fatalf("非 map 回退到 eventType, got %q", got)
	}
}

// --- H. JSON 解析失败的非空 data ------------------------------------------

// ★ data 是 JSON 片段（解析失败）但非空 → 算有效内容。
func TestHasStreamReadinessSignal_非JSON非空(t *testing.T) {
	if !hasStreamReadinessSignal("data: {broken\n\n") {
		t.Fatal("非 JSON 非空 data 应算有效内容")
	}
	// 但空 data 不算
	if hasStreamReadinessSignal("data: \n\n") {
		t.Fatal("空 data 不应算有效内容")
	}
}

// --- I. 状态机重置 ---------------------------------------------------------

// ★ 一个事件处理完后 currentEvent/dataLines 必须重置，不能污染下一个事件。
func TestStateMachine_事件间重置(t *testing.T) {
	st := &streamReadinessState{}
	// 先喂一个 error-only 事件（设置 currentEvent 为 error，捕获诊断）
	appendStreamReadinessSignal(st, "event: error\ndata: {\"error\":{\"message\":\"x\"}}\n\n")
	finishStreamReadinessSignal(st)
	if st.currentEvent != "" {
		t.Fatalf("事件处理后 currentEvent 应重置, got %q", st.currentEvent)
	}
	if len(st.dataLines) != 0 {
		t.Fatalf("事件处理后 dataLines 应重置, got %v", st.dataLines)
	}
	// 再喂一个 ping 事件: 事件处理前 currentEvent 应是 ping, 处理后重置为空。
	// (processStreamReadinessEvent 处理完会清掉 currentEvent, 所以在它之前断言)
	st2 := &streamReadinessState{}
	appendStreamReadinessSignal(st2, "event: ping\ndata: ")
	if st2.currentEvent != "ping" {
		t.Fatalf("currentEvent 应为 ping, got %q", st2.currentEvent)
	}
	// 补完整 data 后, 事件被处理, currentEvent 重置
	appendStreamReadinessSignal(st2, "{}\n\n")
	if st2.currentEvent != "" {
		t.Fatalf("ping 事件处理后 currentEvent 应重置, got %q", st2.currentEvent)
	}
	if hasStreamReadinessSignal("event: ping\ndata: {}\n\n") {
		t.Fatal("ping 事件不应判就绪")
	}
	// ★ 残留检测: 上一个事件的 currentEvent 不能污染下一个事件
	st3 := &streamReadinessState{}
	appendStreamReadinessSignal(st3, "event: error\ndata: {\"error\":\"x\"}\n\n")
	appendStreamReadinessSignal(st3, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
	// 第二个事件没有 event: 行, currentEvent 应为空而不是残留 "error"
	if st3.currentEvent != "" {
		t.Fatalf("无 event 行的事件 currentEvent 应为空, got %q", st3.currentEvent)
	}
}

// --- J. probeStreamFirstEvent 集成 -----------------------------------------

// ★ error-only 帧的流不应被判为就绪（这是本次升级的核心修复点）。
func TestProbeStreamFirstEvent_errorOnly帧(t *testing.T) {
	body := "data: {\"error\":{\"message\":\"rate limit exceeded\"}}\n\n"
	empty, nb, diag := probeStreamFirstEvent(strBodyOf(body))
	if !empty {
		t.Fatal("error-only 帧的流应判为空流（不就绪）")
	}
	if nb != nil {
		nb.Close()
	}
	if diag != "rate limit exceeded" {
		t.Fatalf("诊断信息 = %q, want %q", diag, "rate limit exceeded")
	}
}

// ★ ping 后接有效帧：应判就绪，且已消费字节必须完整接回。
func TestProbeStreamFirstEvent_ping后有效帧(t *testing.T) {
	raw := "event: ping\ndata: {}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"
	empty, nb, _ := probeStreamFirstEvent(strBodyOf(raw))
	if empty {
		t.Fatal("ping 后接有效帧不应判空流")
	}
	if nb == nil {
		t.Fatal("就绪时必须返回可继续读取的响应体")
	}
	got := readAllAndClose(nb)
	if got != raw {
		t.Fatalf("接回的响应体必须与原文一致:\n got=%q\nwant=%q", got, raw)
	}
}

// ★ 数据不丢失：有效帧的情况下，探测已消费的前缀必须原样接回。
func TestProbeStreamFirstEvent_数据不丢失(t *testing.T) {
	raw := "event: content_block_start\ndata: {\"type\":\"content_block_start\"}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\nrest bytes"
	empty, nb, _ := probeStreamFirstEvent(strBodyOf(raw))
	if empty {
		t.Fatal("有效帧不应判空流")
	}
	got := readAllAndClose(nb)
	if got != raw {
		t.Fatalf("接回的响应体必须与原文一致:\n got=%q\nwant=%q", got, raw)
	}
}

// ★ P0-1 主修复的证据: 纯脚手架帧的流必须判为空流(不提交 → 可换站)。
//
// 2026-09-17 审查前, 这两种形态都能骗过探测(原判据只要"非空对象、非 error-only"),
// 于是探测提交 → 整条流零产出 → 客户端拿到"成功但空"的回合。而提交前是**唯一**
// 能换站重试的时机, 所以这道判据是整条空流防护里最关键的一环。
func TestProbeStreamFirstEvent_纯脚手架帧必须判空(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{
			"role 骨架帧",
			"data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\ndata: [DONE]\n\n",
		},
		{
			"finish_reason=stop 空 delta",
			"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n",
		},
		{
			"空 delta 无任何字段",
			"data: {\"choices\":[{\"index\":0,\"delta\":{}}]}\n\ndata: [DONE]\n\n",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			empty, nb, _ := probeStreamFirstEvent(strBodyOf(c.body))
			if !empty {
				if nb != nil {
					nb.Close()
				}
				t.Fatalf("纯脚手架帧的流应判为空流(提交前换站), 实得 empty=false")
			}
		})
	}
}

// ★ 反向保护: 收紧判据**不得误杀**这几类必须判就绪的流。
//
// 前两类是"内容帧"; 后两类是"合法空终止态"与"非 OpenAI 形态" ——
// 后者是探测的保守边界: 判据只处理 OpenAI 形态, 其余形状一律不介入。
func TestProbeStreamFirstEvent_必须判就绪的形态(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"内容帧", "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"},
		{"工具调用帧", "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"id\":\"c1\"}]}}]}\n\n"},
		{"推理帧", "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"think\"}}]}\n\n"},
		// finish_reason=length 是"被 token 上限截断", 本就不该有正文, 是合法成功。
		// 收紧判据时若漏掉白名单, 这类回合会被误判成空流并白白换站。
		{"合法空终止态 length", "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"length\"}]}\n\n"},
		// 非 OpenAI 形态: 判据不介入, 沿用原判据(就绪)。
		{"Claude 形态", "event: content_block_start\ndata: {\"type\":\"content_block_start\"}\n\n"},
		{"Gemini 形态", "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hi\"}]}}]}\n\n"},
		{"非 JSON 文本", "data: hello world\n\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			empty, nb, _ := probeStreamFirstEvent(strBodyOf(c.body))
			if empty {
				t.Fatalf("%s 必须判就绪, 实得 empty=true", c.name)
			}
			if nb != nil {
				nb.Close()
			}
		})
	}
}

// strBody 把字符串包成 io.ReadCloser, 供 probeStreamFirstEvent 集成测试用。
type strBody struct {
	*strings.Reader
}

func (strBody) Close() error { return nil }

func strBodyOf(s string) io.ReadCloser {
	return &strBody{Reader: strings.NewReader(s)}
}

func readAllAndClose(r io.ReadCloser) string {
	b, _ := io.ReadAll(r)
	r.Close()
	return string(b)
}
