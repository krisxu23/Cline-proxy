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
// 实现要点(终审 P3): **每条流一个常驻读协程**, 而不是每次 Read 起一个 ——
// 中继层与探测层各包一层, 旧实现稳态每 ~4KiB 上游读 2 次 goroutine spawn/
// teardown + 2 个 timer + 每字节多拷 2 遍(10MB 流 ≈ 2500+ goroutine 往返)。
// 现在: 常驻协程读进私有缓冲, 经 channel 交给 Read; 读侧用**同一个 timer**
// 逐批 reset 后 select 等待。缓冲复用走 ack 握手 —— 消费方排空一批后才放行
// 下一次底层读, 保证常驻协程绝不会在消费方还引用着旧批时覆盖它(原 per-Read
// scratch 是为修 -race 而生, 握手是同一保证的零分配形态)。超时/Close 统一走
// done 通道, 在途读、待发结果、待 ack 三种阻塞点都能退出, 不泄漏协程。

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

// idleReadBufSize 常驻读协程的私有缓冲。中继层 bufio 默认 4096、上层
// controlSanitizingReader 同量级 —— 16KiB 覆盖常见批大小, 偶发更大批由
// remain 分批交付, 语义不变。
const idleReadBufSize = 16 << 10

type idleReadResult struct {
	data []byte // 指向常驻协程的私有缓冲, ack 前不得复用
	err  error
}

type idleAbortReader struct {
	src     io.Closer
	reader  io.Reader
	timeout time.Duration

	resCh  chan idleReadResult // 常驻协程 → 消费方: 一批结果
	ackCh  chan struct{}       // 消费方 → 常驻协程: 该批已排空, 可复用缓冲
	doneCh chan struct{}       // Close/超时后所有阻塞点的退出通道
	once   sync.Once

	timer *time.Timer // 单 timer, 每次等待前 reset(终审 P3)

	remain    []byte // 已收到但尚未交付给调用方的余量
	remainErr error  // remain 排空后要上抛的终端错误(io.EOF / 底层错误)
	terminal  error  // 终端错误已上抛, 后续 Read 直接返回
}

// newIdleAbortReader 包装上游流: 空闲超时即 Close 上游(中断阻塞的读)。
// 常驻读协程在此启动(每流一个)。
func newIdleAbortReader(rc io.ReadCloser, timeout time.Duration) *idleAbortReader {
	if timeout <= 0 {
		timeout = 90 * time.Second
	}
	r := &idleAbortReader{
		src:     rc,
		reader:  rc,
		timeout: timeout,
		resCh:   make(chan idleReadResult),
		ackCh:   make(chan struct{}),
		doneCh:  make(chan struct{}),
		timer:   time.NewTimer(timeout),
	}
	if !r.timer.Stop() {
		<-r.timer.C
	}
	go r.pump()
	return r
}

// pump 常驻读协程: 读一批 → 发送 → 等 ack(复用保护) → 下一批。
// 三个阻塞点(发送、等 ack、底层读)都 select doneCh, Close 后必退。
func (r *idleAbortReader) pump() {
	buf := make([]byte, idleReadBufSize)
	for {
		n, err := r.reader.Read(buf)
		select {
		case r.resCh <- idleReadResult{data: buf[:n], err: err}:
		case <-r.doneCh:
			return
		}
		if err != nil {
			return // 终端批不复用缓冲, 也无需 ack
		}
		select {
		case <-r.ackCh: // 消费方排空本批后才允许覆盖 buf
		case <-r.doneCh:
			return
		}
	}
}

func (r *idleAbortReader) Read(p []byte) (int, error) {
	if r.terminal != nil {
		return 0, r.terminal
	}
	for {
		// 已有余量: 先交付, 不碰 channel/timer。
		if len(r.remain) > 0 {
			n := copy(p, r.remain)
			r.remain = r.remain[n:]
			if len(r.remain) == 0 {
				if r.remainErr != nil {
					r.terminal = r.remainErr
				} else {
					r.ack() // 本批排空, 放行常驻协程复用缓冲
				}
			}
			return n, nil
		}

		armTimer(r.timer, r.timeout)
		select {
		case res := <-r.resCh:
			stopTimer(r.timer)
			if len(res.data) == 0 && res.err == nil {
				// 空批必须走 ack() 而不是裸发送(2026-09-24 审查): 裸发送没有
				// doneCh 保护, 一旦 Close/fail 恰好发生在空批这一刻, 这里会永久
				// 阻塞 —— pump 协程等 ack、Read 等数据, 两边互等泄漏。
				r.ack()
				continue
			}
			n := copy(p, res.data)
			rest := res.data[n:]
			if len(rest) == 0 {
				if res.err == nil {
					r.ack()
				} else if n > 0 {
					// 批数据已交付, 终端错误留到下一次 Read。
					r.terminal = res.err
				} else {
					return 0, res.err
				}
			} else {
				r.remain = rest
				r.remainErr = res.err
			}
			return n, nil
		case <-r.timer.C:
			r.fail(errStreamIdleTimeout)
			return 0, errStreamIdleTimeout
		case <-r.doneCh:
			// Close 先于本批到达: 优雅收尾按 EOF 处理。
			r.terminal = io.EOF
			return 0, io.EOF
		}
	}
}

// ack 放行常驻协程复用刚消费完的缓冲(done 已关时协程可能已退出, 不得死等)。
func (r *idleAbortReader) ack() {
	select {
	case r.ackCh <- struct{}{}:
	case <-r.doneCh:
	}
}

// fail 空闲超时: 关上游(中断在途读)+ 关 done(收掉发送/ack 阻塞点),
// 并把超时错误设为终端 —— 此后 Read 立即复现同一错误, 不会再挂起。
func (r *idleAbortReader) fail(err error) {
	r.terminal = err
	r.closeOnce()
}

func armTimer(t *time.Timer, d time.Duration) {
	if !t.Stop() {
		select {
		case <-t.C:
		default: // 已被上一轮 select 消费
		}
	}
	t.Reset(d)
}

func stopTimer(t *time.Timer) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
}

// errStreamIdleTimeout 空闲超时错误(中继据此走断流收尾)。
var errStreamIdleTimeout = &streamIdleError{}

type streamIdleError struct{}

func (e *streamIdleError) Error() string {
	return "upstream stream idle timeout (no bytes within limit)"
}

// Close 幂等关闭(中继正常收尾时调用): 关上游 + 通知常驻协程退出。
func (r *idleAbortReader) Close() error {
	r.closeOnce()
	return nil
}

func (r *idleAbortReader) closeOnce() {
	var err error
	r.once.Do(func() {
		close(r.doneCh)
		err = r.src.Close()
	})
	// once 已执行过的并发调用拿不到 err, 与旧实现语义一致(Close 幂等返回 nil)。
	_ = err
}
