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

// DecodeJSONLimit 按上限把响应体解成 out。
//
// 与 ReadBody/ReadBodyLimit 同一动机(见 MaxUpstreamBodyBytes 的注释): 上游响应体
// 由**不受我们控制**的一方决定(zen 侧还要经第三方免费出口节点那一跳)。裸
// `json.NewDecoder(resp.Body).Decode(...)` 会**边读边分配** —— 一个不封顶的响应
// 就能把网关吃成 OOM, 而且比 io.ReadAll 更隐蔽: 它不先攒字节, 直接在解码过程中
// 长出来。2026-09-24 审计 P0-2: 主路径(chat 流式/非流式)早已用 LimitReader 封顶,
// 这里把剩下的散点收敛到同一口径。
//
// 用 LimitedReader 而不是"Decoder + InputOffset()"来判超限: 后者在体被截断时
// 报的是 `unexpected EOF`(InputOffset 停在上一个**完整 token** 的位置, 不是被截断
// 处), 于是"上游疯了"与"我们解析错了"分不开 —— 那正是这条防线最需要能区分的两件事。
// 判据是"限额是否被读穿": 读穿了说明上游真的吐了这么多字节。
//
// 注意它**不**拒"小 JSON + 尾部垃圾": Decoder 只解第一个值、不会继续读, 也就不会
// 因此分配内存(旧行为同样忽略尾部内容), 所以没有可被利用的增长面。
func DecodeJSONLimit(r io.Reader, out any, limit int64) error {
	if limit <= 0 {
		limit = MaxUpstreamBodyBytes
	}
	lr := &io.LimitedReader{R: r, N: limit + 1}
	derr := json.NewDecoder(lr).Decode(out)
	if lr.N <= 0 {
		return fmt.Errorf("upstream response body exceeds %d bytes", limit)
	}
	return derr
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
