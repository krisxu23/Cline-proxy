package app

// 流式保活 (P1-11, 参照 OmniRoute 的 sseHeartbeat):
//
// 上游"接了单但迟迟不出字"(排队/推理中/网络卡)时, 客户端会把静默当成挂死。
// 心跳的注入点是**读上游的环节**: 上游静默超过间隔时, 向客户端发出一个
// 协议合法的空 delta 帧(OpenAI 形状), 让客户端知道链路活着。
//
// 正确性要点: 只在"行边界"(上一段数据以 \n 结尾, 或还没有任何数据)注入,
// 绝不把保活帧插进一行 SSE 的中间; 注入的帧与正文同走一个写出通道,
// 客户端按普通空 delta 处理, 无需感知。

import (
	"io"
	"sync"
	"time"
)

// heartbeatReader 包装上游响应体: 上游静默超过 interval 时, 向读者注入
// frame() 生成的保活帧。frame 必须返回完整的 SSE 帧(含结尾空行)。
type heartbeatReader struct {
	src      io.Reader
	interval time.Duration
	frame    func() []byte

	ch   chan []byte
	done chan struct{}
	once sync.Once

	// boundary 只由读协程(Read)维护: 上一段数据是否以 \n 结尾。
	// 只有行边界才允许注入(见上)。
	boundary bool
}

func newHeartbeatReader(src io.Reader, interval time.Duration, frame func() []byte) io.ReadCloser {
	h := &heartbeatReader{
		src:      src,
		interval: interval,
		frame:    frame,
		ch:       make(chan []byte, 8),
		done:     make(chan struct{}),
		boundary: true, // 流开始时必然在边界
	}
	go h.pump()
	return h
}

func (h *heartbeatReader) pump() {
	defer close(h.ch)
	buf := make([]byte, 8192)
	for {
		n, err := h.src.Read(buf)
		if n > 0 {
			b := make([]byte, n)
			copy(b, buf[:n])
			select {
			case h.ch <- b:
			case <-h.done:
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func (h *heartbeatReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	timer := time.NewTimer(h.interval)
	defer timer.Stop()
	for {
		select {
		case b, ok := <-h.ch:
			if !ok {
				return 0, io.EOF
			}
			// 更新行边界标记(只看本段最后一个字节)
			if n := len(b); n > 0 {
				h.boundary = b[n-1] == '\n'
			}
			return copy(p, b), nil
		case <-timer.C:
			// 上游静默到期: 只有在行边界才注入保活帧;
			// 行中间的静默继续等(下一轮 timer 重新计时)。
			if h.boundary {
				return copy(p, h.frame()), nil
			}
			timer.Reset(h.interval)
		case <-h.done:
			// 上游被关闭(客户端断开/超时): 排干剩余数据后 EOF。
			select {
			case b, ok := <-h.ch:
				if !ok {
					return 0, io.EOF
				}
				return copy(p, b), nil
			default:
				return 0, io.EOF
			}
		}
	}
}

// Close 停止保活(不影响底层 src —— 底层由调用方负责关闭)。
func (h *heartbeatReader) Close() error {
	h.once.Do(func() { close(h.done) })
	return nil
}

// streamHeartbeatInterval 保活间隔; 0 = 关闭。
// 默认 15 秒: 主流 agent 客户端(OpenCode/Cline)的读超时普遍在 30~120 秒,
// 15 秒留出足够余量; 配置 streamHeartbeatSecs 可调, 0 关闭。
func streamHeartbeatInterval() time.Duration {
	cfg := getZenConfig()
	if cfg == nil || cfg.StreamHeartbeatSecs <= 0 {
		return 0
	}
	return time.Duration(cfg.StreamHeartbeatSecs) * time.Second
}

// openAIHeartbeatFrame OpenAI 形状的保活帧: 一个没有任何内容的 delta chunk,
// 所有 OpenAI 协议客户端都会安全忽略。
var openAIHeartbeatFrame = []byte("data: {\"choices\":[{\"index\":0,\"delta\":{}}]}\n\n")
