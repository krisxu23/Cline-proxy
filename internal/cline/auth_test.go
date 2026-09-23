package cline

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 本包此前零测试文件, 而它管的是账号生命周期(device auth → 注册 → 刷新),
// 一旦 URL/表单组装写错, 只有真机跑到登录那一刻才会暴露。这里用注入基址把
// 三类请求的"路径 + 方法 + 表单/JSON 字段"钉住。

func withBase(t *testing.T, workos, cline string) {
	t.Helper()
	ow, oc := workosBaseURL, clineAPIBase
	workosBaseURL, clineAPIBase = workos, cline
	t.Cleanup(func() { workosBaseURL, clineAPIBase = ow, oc })
}

func TestWorkosDeviceAuthRequestShape(t *testing.T) {
	var path, form, method, ct string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, method, ct = r.URL.Path, r.Method, r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		form = string(b)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"device_code": "dc-1", "user_code": "UC-1", "interval": 5, "expires_in": 900,
		})
	}))
	defer srv.Close()
	withBase(t, srv.URL, srv.URL)

	d, err := WorkosDeviceAuth()
	if err != nil {
		t.Fatalf("device auth 失败: %v", err)
	}
	if path != "/user_management/authorize/device" {
		t.Fatalf("路径不对: %s", path)
	}
	if method != "POST" {
		t.Fatalf("方法不对: %s", method)
	}
	if !strings.Contains(ct, "application/x-www-form-urlencoded") {
		t.Fatalf("content-type 不对: %s", ct)
	}
	if form != "client_id="+workosClientID {
		t.Fatalf("表单不对: %q", form)
	}
	if d.DeviceCode != "dc-1" || d.UserCode != "UC-1" || d.ExpiresIn != 900 {
		t.Fatalf("响应解析不对: %+v", d)
	}
}

func TestWorkosDeviceAuthNon200SurfacesUpstreamBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		_, _ = w.Write([]byte("upstream boom"))
	}))
	defer srv.Close()
	withBase(t, srv.URL, srv.URL)

	_, err := WorkosDeviceAuth()
	if err == nil {
		t.Fatal("非 200 必须报错")
	}
	if !strings.Contains(err.Error(), "503") || !strings.Contains(err.Error(), "upstream boom") {
		t.Fatalf("错误信息要带上游状态码与正文片段, got %v", err)
	}
}

func TestPollWorkosTokenImmediateSuccess(t *testing.T) {
	var path, form string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		form = string(b)
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "at", "refresh_token": "rt"})
	}))
	defer srv.Close()
	withBase(t, srv.URL, srv.URL)

	// 200 立刻返回, 不进入轮询等待(interval 传 1 也不会 sleep)。
	a, err := PollWorkosToken("dc-1", 1, 30)
	if err != nil {
		t.Fatalf("轮询失败: %v", err)
	}
	if a.AccessToken != "at" || a.RefreshToken != "rt" {
		t.Fatalf("响应解析不对: %+v", a)
	}
	if path != "/user_management/authenticate" {
		t.Fatalf("路径不对: %s", path)
	}
	for _, want := range []string{"grant_type=urn%3Aietf%3Aparams%3Aoauth%3Agrant-type%3Adevice_code", "device_code=dc-1", "client_id=" + workosClientID} {
		if !strings.Contains(form, want) {
			t.Fatalf("表单缺少 %q: %q", want, form)
		}
	}
}

func TestPollWorkosTokenFatalErrorReturnsImmediately(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "access_denied", "error_description": "用户拒绝"})
	}))
	defer srv.Close()
	withBase(t, srv.URL, srv.URL)

	start := time.Now()
	_, err := PollWorkosToken("dc-1", 1, 30)
	if err == nil {
		t.Fatal("access_denied 必须立刻报错, 不能一直轮询到超时")
	}
	if !strings.Contains(err.Error(), "用户拒绝") {
		t.Fatalf("应回传 error_description, got %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("致命错误不得进入 sleep 轮询")
	}
}

func TestRegisterAndRefreshRequestShapes(t *testing.T) {
	var paths []string
	var bodies []map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		var m map[string]string
		_ = json.NewDecoder(r.Body).Decode(&m)
		bodies = append(bodies, m)
		if strings.HasSuffix(r.URL.Path, "/register") {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{"accessToken": "at", "refreshToken": "rt", "expiresAt": 123},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{"accessToken": "at2", "refreshToken": "rt2", "expiresAt": "2026-01-02T03:04:05Z"},
		})
	}))
	defer srv.Close()
	withBase(t, srv.URL, srv.URL)

	reg, err := RegisterWithCline("wa", "wr")
	if err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	if reg.Data.AccessToken != "at" || reg.Data.RefreshToken != "rt" {
		t.Fatalf("注册响应解析不对: %+v", reg)
	}
	ref, err := RefreshClineToken("rt")
	if err != nil {
		t.Fatalf("刷新失败: %v", err)
	}
	if ref.Data.AccessToken != "at2" {
		t.Fatalf("刷新响应解析不对: %+v", ref)
	}
	if len(paths) != 2 || paths[0] != "/auth/register" || paths[1] != "/auth/refresh" {
		t.Fatalf("路径不对: %v", paths)
	}
	if bodies[0]["accessToken"] != "wa" || bodies[0]["refreshToken"] != "wr" {
		t.Fatalf("注册体不对: %v", bodies[0])
	}
	if bodies[1]["refreshToken"] != "rt" || bodies[1]["grantType"] != "refresh_token" {
		t.Fatalf("刷新体不对: %v", bodies[1])
	}
}

func TestRegisterAndRefreshNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()
	withBase(t, srv.URL, srv.URL)

	if _, err := RegisterWithCline("a", "b"); err == nil {
		t.Fatal("500 必须报错")
	}
	if _, err := RefreshClineToken("rt"); err == nil {
		t.Fatal("500 必须报错")
	}
}

func TestParseExpiry(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want int64
	}{
		{"float64 毫秒", float64(1710000000000), 1710000000000},
		{"int64 毫秒", int64(1710000000000), 1710000000000},
		{"int 毫秒", 1710000000000, 1710000000000},
		{"RFC3339 字符串", "2026-01-02T03:04:05Z", time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC).UnixMilli()},
		{"无法解析", "not-a-time", 0},
		{"nil", nil, 0},
	}
	for _, c := range cases {
		if got := ParseExpiry(c.in); got != c.want {
			t.Errorf("%s: got %d want %d", c.name, got, c.want)
		}
	}
}

func TestIsWindowsMatchesRuntime(t *testing.T) {
	// 该判定用于选浏览器打开方式, 逻辑本身是环境变量嗅探, 这里只固化"不 panic 且稳定"。
	first := IsWindows()
	if again := IsWindows(); again != first {
		t.Fatal("同一环境下判定必须稳定")
	}
}
