package kit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"time"
)

var ExecCommand = exec.Command

var HTTPTransport = &http.Transport{
	MaxIdleConns:        100,
	MaxIdleConnsPerHost: 10,
	IdleConnTimeout:     90 * time.Second,
	DisableCompression:  false,
}

// HTTPClient 不走超时: 它被流式转发路径(clinepass 的 SSE 长连接)复用, 而
// http.Client.Timeout 约束的是「从发请求到读完响应体」的整段时长 —— 长流式响应
// 会被它中途切断。这类调用方(如 clinepass)必须自己通过 request context 设上限
// (clinepass 已传 r.Context()), 故这里刻意保持零超时。
var HTTPClient = &http.Client{
	Transport: HTTPTransport,
}

// HTTPClientTimeout 带默认超时(30s)的客户端, 专供**短请求**(device auth、OAuth
// 等)使用。此前 kit.HTTPClient 无超时, 调用方忘设 context 会永久挂住; 这些短请求
// 本就不该无限等待, 给一个上限更稳。
var HTTPClientTimeout = &http.Client{
	Transport: HTTPTransport,
	Timeout:   30 * time.Second,
}

func HTTPPostForm(rawURL string, form url.Values) (*http.Response, error) {
	req, err := http.NewRequest("POST", rawURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return HTTPClientTimeout.Do(req)
}

func HTTPPostJSON(rawURL string, body any) (*http.Response, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest("POST", rawURL, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return HTTPClientTimeout.Do(req)
}

func ReadBody(resp *http.Response) string {
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Sprintf("<read error: %v>", err)
	}
	return string(data)
}

func Truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

func RunCommand(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	return cmd.Start()
}
