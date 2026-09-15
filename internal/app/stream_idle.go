package app

// 上游流空闲中断 (参照 OmniRoute open-sse 的流式 idle/early-EOF 保护机制,
// MIT; Go 侧实现):
//
// 分层超时只覆盖到"响应头返回"为止, 正文阶段的挂起没有任何限制 —— 上游(或
// 途经的免费节点)在流中途彻底静默时, 客户端会无限期等待(实测: 用户侧表现为
// "请求一直转圈, 最后超时"), 而网关本可以主动断开并触发换站。
//
// idleAbortReader 在每次 Read 上施加"空闲上限": 超过默认 90 秒没有任何字节
// 到达就关闭底层流并让 Read 返回错误, 中继随后走既有的断流收尾路径
// (合成 finish chunk + [DONE]), 客户端不会拿到"半截流 + 永久挂起"。
//
// 实现要点: 用带缓冲的结果 channel + timer 轮询, 避免 goroutine 泄漏 ——
// 每次 Read 只有一个在途读 goroutine, 且它在返回时会阻塞在 channel 上直到
// 结果被取走(缓冲 1 保证不泄漏)。

import (
	"io"
	"sync"
	"time"
)

// streamIdleTimeout 上游流"无字节"容忍上限(可用配置覆盖)。
func streamIdleTimeout() time.Duration {
	if v := getZenConfig().StreamIdleSecs; v > 0 {
		return time.Duration(v) * time.Second
	}
	return 90 * time.Second
}

type idleAbortReader struct {
	src     io.Closer
	reader  io.Reader
	timeout time.Duration

	mu     sync.Mutex
	closed bool
	once   sync.Once
}

// newIdleAbortReader 包装上游流: 空闲超时即 Close 上游(中断阻塞的读)。
func newIdleAbortReader(rc io.ReadCloser, timeout time.Duration) *idleAbortReader {
	if timeout <= 0 {
		timeout = 90 * time.Second
	}
	return &idleAbortReader{src: rc, reader: rc, timeout: timeout}
}

func (r *idleAbortReader) Read(p []byte) (int, error) {
	type result struct {
		n   int
		err error
	}
	ch := make(chan result, 1) // 缓冲 1: 即使超时后读才返回也不会泄漏
	go func() {
		n, err := r.reader.Read(p)
		ch <- result{n, err}
	}()

	timer := time.NewTimer(r.timeout)
	defer timer.Stop()
	select {
	case res := <-ch:
		return res.n, res.err
	case <-timer.C:
		// 空闲超时: 关闭上游让在途的 Read 立即返回, 并向上报告超时
		r.mu.Lock()
		if !r.closed {
			r.closed = true
			r.once.Do(func() { _ = r.src.Close() })
		}
		r.mu.Unlock()
		return 0, errStreamIdleTimeout
	}
}

// errStreamIdleTimeout 空闲超时错误(中继据此走断流收尾)。
var errStreamIdleTimeout = &streamIdleError{}

type streamIdleError struct{}

func (e *streamIdleError) Error() string {
	return "upstream stream idle timeout (no bytes within limit)"
}

// Close 幂等关闭(中继正常收尾时调用)。
func (r *idleAbortReader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	var err error
	r.once.Do(func() { err = r.src.Close() })
	return err
}
