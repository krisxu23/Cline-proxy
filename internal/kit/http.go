package kit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"time"
)

var ExecCommand = exec.Command

var HTTPTransport = &http.Transport{
	MaxIdleConns: 100,
	// 网关对同一上游的并发远超 10: 超额请求完成后连接不回池, 下一波重建
	// TCP(+TLS) 握手。按典型上游并发上调(成本只是空闲连接内存)。
	MaxIdleConnsPerHost: 128,
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

// MaxUpstreamBodyBytes 上游响应体的读取上限(8 MiB)。
//
// 上游是第三方服务: 一个被劫持或"发疯"的端点可以持续吐字节, 而 io.ReadAll
// 无界 —— 单次请求就能把网关内存吃满(OOM 之后整个进程连带所有账号池一起挂)。
// 8 MiB 远大于任何正常的错误体 / 目录体 / token 响应, 又足以封住无界增长。
// 注意: 流式(SSE)路径**不**经这里, 它按帧增量读取, 不受此上限影响。
const MaxUpstreamBodyBytes = 8 << 20

// ReadBodyLimit 按上限读取响应体。截断时在末尾追加标记, 便于排障时区分
// "上游就发了这么多" 与 "我们只读了这么多"。
func ReadBodyLimit(resp *http.Response, limit int64) string {
	if limit <= 0 {
		limit = MaxUpstreamBodyBytes
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return fmt.Sprintf("<read error: %v>", err)
	}
	if int64(len(data)) > limit {
		return string(data[:limit]) + fmt.Sprintf("...<truncated at %d bytes>", limit)
	}
	return string(data)
}

// ReadBody 读取响应体(带 MaxUpstreamBodyBytes 上限)。
// 需要更紧的上限时用 ReadBodyLimit。
func ReadBody(resp *http.Response) string {
	return ReadBodyLimit(resp, MaxUpstreamBodyBytes)
}

func Truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// RunCommand 启动一个进程后立即返回(fire-and-forget), 用于"拉起外部程序"
// 这类不需要等待结果的场景(如打开浏览器)。
//
// 但**必须有人收尸**: Start 之后不调用 Wait, 子进程退出后会留下僵尸进程
// (Windows 上则是句柄不释放)。所以这里起一个 goroutine 专门 Wait, 只记日志
// 不向调用方回传错误 —— 调用方本来也不关心退出码。
//
// 说明: 参数均由调用方以 Go 变量传入(不经过 shell 拼接), 不存在命令注入面。
func RunCommand(name string, args ...string) error {
	cmd := ExecCommand(name, args...)
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() {
		if err := cmd.Wait(); err != nil {
			log.Printf("kit: 外部进程 %s 退出异常: %v", name, err)
		}
	}()
	return nil
}
