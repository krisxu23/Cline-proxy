package kit

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ReadBody 此前是裸 io.ReadAll(无界), 上游被劫持/发疯时单请求即可吃满内存。
// 本组用例把"有上限"这件事固化下来。

func TestReadBodyLimitTruncatesWithMarker(t *testing.T) {
	resp := &http.Response{Body: io.NopCloser(strings.NewReader(strings.Repeat("a", 100)))}
	got := ReadBodyLimit(resp, 10)
	if !strings.HasPrefix(got, strings.Repeat("a", 10)) {
		t.Fatalf("应保留前 10 字节, got %q", got)
	}
	if !strings.Contains(got, "truncated at 10") {
		t.Fatalf("截断必须带标记(排障时要能区分上游就是这么短还是我们截了): %q", got)
	}
}

func TestReadBodyLimitKeepsShortBodyIntact(t *testing.T) {
	resp := &http.Response{Body: io.NopCloser(strings.NewReader(`{"ok":true}`))}
	if got := ReadBodyLimit(resp, 1024); got != `{"ok":true}` {
		t.Fatalf("未超限时不得改动内容, got %q", got)
	}
}

func TestReadBodyLimitZeroFallsBackToDefault(t *testing.T) {
	body := strings.Repeat("x", 32)
	resp := &http.Response{Body: io.NopCloser(strings.NewReader(body))}
	if got := ReadBodyLimit(resp, 0); got != body {
		t.Fatalf("limit<=0 应回退到默认上限, got %q", got)
	}
	if MaxUpstreamBodyBytes != 8<<20 {
		t.Fatalf("默认上限应固定为 8MiB, got %d", MaxUpstreamBodyBytes)
	}
}

func TestReadBodyLimitReadErrorIsReported(t *testing.T) {
	resp := &http.Response{Body: io.NopCloser(errReader{})}
	got := ReadBody(resp)
	if !strings.Contains(got, "<read error:") {
		t.Fatalf("读失败必须显式暴露而不是静默空串, got %q", got)
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestTruncate(t *testing.T) {
	if got := Truncate("abcdef", 3); got != "abc..." {
		t.Fatalf("got %q", got)
	}
	if got := Truncate("abc", 3); got != "abc" {
		t.Fatalf("未超长不得加省略号, got %q", got)
	}
}

func TestWriteFileAtomicDefaultRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "x.json")
	if err := WriteFileAtomicDefault(path, []byte(`{"a":1}`)); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != `{"a":1}` {
		t.Fatalf("读回不一致: %q err=%v", got, err)
	}
	// 目录里不应残留临时文件(失败路径的 defer 清理)
	entries, _ := os.ReadDir(filepath.Dir(path))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			t.Fatalf("残留临时文件: %s", e.Name())
		}
	}
}

func TestFreshZenIdentityShape(t *testing.T) {
	sess, req, ua := FreshZenIdentity()
	if !strings.HasPrefix(sess, "sess_") || len(sess) != len("sess_")+32 {
		t.Fatalf("session 形状不对: %q", sess)
	}
	if !strings.HasPrefix(req, "user_") || len(req) != len("user_")+16 {
		t.Fatalf("request 形状不对: %q", req)
	}
	found := false
	for _, c := range ZenUserAgents {
		if c == ua {
			found = true
		}
	}
	if !found {
		t.Fatalf("user-agent 必须来自白名单: %q", ua)
	}
	if sess2, _, _ := FreshZenIdentity(); sess2 == sess {
		t.Fatal("每次身份必须不同(轮换的意义所在)")
	}
	if got := RandHex(4); len(got) != 8 {
		t.Fatalf("RandHex(4) 应返回 8 个十六进制字符, got %q", got)
	}
	if RandIntn(0) != 0 {
		t.Fatal("RandIntn(0) 必须安全返回 0(不能除零 panic)")
	}
}

func TestWithRetryJitterBounds(t *testing.T) {
	if got := WithRetryJitter(0); got != 0 {
		t.Fatalf("非正延迟原样返回, got %v", got)
	}
	base := 100 * time.Millisecond
	for i := 0; i < 50; i++ {
		got := WithRetryJitter(base)
		if got < base || got > base+base/4 {
			t.Fatalf("抖动必须落在 [delay, delay*1.25], got %v", got)
		}
	}
}

func TestHTTPPostFormSendsURLEncoded(t *testing.T) {
	var gotCT, gotBody, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotCT = r.Method, r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	resp, err := HTTPPostForm(srv.URL+"/token", map[string][]string{"grant_type": {"device_code"}})
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	if gotMethod != "POST" {
		t.Fatalf("method=%s", gotMethod)
	}
	if !strings.Contains(gotCT, "application/x-www-form-urlencoded") {
		t.Fatalf("content-type=%s", gotCT)
	}
	if gotBody != "grant_type=device_code" {
		t.Fatalf("表单编码不对: %q", gotBody)
	}
}

func TestHTTPPostJSONSendsJSON(t *testing.T) {
	var gotCT string
	var got map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	resp, err := HTTPPostJSON(srv.URL+"/auth", map[string]string{"refreshToken": "rt"})
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	if !strings.Contains(gotCT, "application/json") {
		t.Fatalf("content-type=%s", gotCT)
	}
	if got["refreshToken"] != "rt" {
		t.Fatalf("body 不对: %v", got)
	}
}

func TestRunCommandMissingBinaryReturnsError(t *testing.T) {
	if err := RunCommand("cline-proxy-nonexistent-binary-xyz"); err == nil {
		t.Fatal("不存在的可执行文件必须返回错误(调用方据此提示)")
	}
}

func TestRunCommandStartsAndReaps(t *testing.T) {
	// 真实启动一个会立刻退出的进程: Start 必须成功, 且不得阻塞调用方。
	start := time.Now()
	var err error
	if IsWindowsHelper() {
		err = RunCommand("cmd", "/c", "exit", "0")
	} else {
		err = RunCommand("/bin/sh", "-c", "exit 0")
	}
	if err != nil {
		t.Fatalf("启动失败: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("fire-and-forget 不该等待子进程结束, 耗时 %v", elapsed)
	}
}

// IsWindowsHelper 判断当前是否 Windows(测试内联判断, 避免依赖 runtime 之外的东西)。
func IsWindowsHelper() bool {
	return os.PathSeparator == '\\'
}
