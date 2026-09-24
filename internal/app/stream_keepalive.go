package app

// 流式保活 (P1-11, 参照 OmniRoute 的 sseHeartbeat / earlyStreamKeepalive, MIT):
//
// 上游"接了单但迟迟不出字"(排队/推理中/网络卡)时, 客户端会把静默当成挂死。
//
// 关键教训(2026-09-15 bai:qwen3.8-flash 事故): 心跳**绝不能**注入到"上游→中继"
// 的输入字节流里。旧实现用 heartbeatReader 包装上游 body, 在分片传输一个巨型
// JSON 帧(实测含 C2PA _manifest 的图片响应, 单帧几十 KB 分多次 TCP 到达)时,
// 某个分片恰好以 \n 结尾 → 边界判定为真 → 15s 静默里把心跳帧**插进帧中间** →
// 中继按 \n 切行时把"半帧 + 心跳 + 半帧"黏成非法单行丢弃, 整条流被掏空, 客户端
// 最终拿到裸 _manifest JSON 报 "Bad control character"。
//
// 正确做法(本文件, 与 OmniRoute 一致): 心跳只发往**客户端输出侧**, 由中继维护
// "距上次向客户端成功写出真实字节的空闲计时", 到点写一个协议合法的空 delta 帧。
// 上游字节流全程不掺任何心跳, 从根上杜绝截帧。首字节未到(上游完全静默)同样覆盖:
// 计时从流开始就起算, 一个真实字节都没写出时照样按时发心跳。
//
// 边界规则: 心跳只允许插在 **SSE 事件边界**(输出以空行结尾, 或尚未输出任何字节)。
// 事件中间的静默(上一笔写出以单个 \n 结尾, 如 event: 行)继续等待, 宁可不发 ——
// 这是旧 heartbeatReader"末字节 \n 即边界"误判(半帧恰好以 \n 结尾)的根治版。

import (
	"net/http"
	"sync"
	"time"
)

// sseHeartbeat 输出侧保活器: 包一层 ResponseWriter 的写入, 事件边界上的空闲
// 超过 interval 时向客户端写 frame()。写与 flush 全程持锁(ResponseWriter 非并发安全)。
// interval<=0 时为直通(不启动定时, write/flush 原样透传)。
type sseHeartbeat struct {
	mu       sync.Mutex
	w        http.ResponseWriter
	flusher  http.Flusher
	interval time.Duration
	frame    func() []byte

	last     time.Time
	boundary bool // 输出当前是否停在 SSE 事件边界
	prev     byte // 最近一次写出的最后一个字节(判定跨笔写出的 \n\n)
	closed   bool
	done     chan struct{}
	once     sync.Once
	// committed 响应头是否已提交; commit 是"首次真实正文写出前"的一次性提交动作。
	//
	// ★ 为什么心跳必须等提交后才发(2026-09-24 修复"空白回复导致 agent 卡死"):
	// http.Flusher.Flush() 会**隐式提交**响应头。若在还没写出任何正文时就发心跳,
	// 200 就被钉死了 —— 此后发现"上游空回包"也无法再返回真 502, 只能往已提交的
	// 200 流里塞错误帧。而客户端(agent 工具)认不出那种自定义错误帧, 只看到一条
	// 没有内容的流 → 空白回复 → 停止工作。所以: 提交前一律不发心跳。
	committed bool
	commit    func()
	// writeErr 粘性写错误: 客户端断开(broken pipe)后第一次 Write 失败即记录,
	// 上层循环据此提前收手, 不再把整条上游流读完还往黑洞里写。
	writeErr error
}

func newSSEHeartbeat(w http.ResponseWriter, flusher http.Flusher, interval time.Duration, frame func() []byte) *sseHeartbeat {
	h := &sseHeartbeat{
		w:        w,
		flusher:  flusher,
		interval: interval,
		frame:    frame,
		last:     time.Now(),
		boundary: true, // 流开始时必然在边界(尚无输出)
		done:     make(chan struct{}),
	}
	if interval > 0 {
		go h.pump()
	}
	return h
}

// track 更新"事件边界"判定(假定调用方已持锁): 输出的最后两个字节都是 \n 才算边界。
// 单笔不足两字节时用上一笔的末字节补齐, 从而正确识别 "…\n" + "\n" 的跨笔空行。
func (h *sseHeartbeat) track(b []byte) {
	if len(b) == 0 {
		return
	}
	last := b[len(b)-1]
	var second byte
	if len(b) >= 2 {
		second = b[len(b)-2]
	} else {
		second = h.prev
	}
	h.boundary = second == '\n' && last == '\n'
	h.prev = last
}

// writeLocked 向客户端写一段字节并刷新空闲计时与边界状态(调用方须已持锁)。
func (h *sseHeartbeat) writeLocked(b []byte) (int, error) {
	h.last = time.Now()
	h.track(b)
	if h.closed {
		return 0, nil
	}
	if h.writeErr != nil {
		return 0, h.writeErr
	}
	n, err := h.w.Write(b)
	if err != nil {
		h.writeErr = err
	}
	return n, err
}

// WriteErr 返回首次客户端写错误(无错误返回 nil)。供流循环提前退出用。
func (h *sseHeartbeat) WriteErr() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.writeErr
}

// pump 周期性检查空闲: 事件边界上距上次写出超过 interval 就补一帧心跳(并刷新
// last, 避免上游持续静默时连发)。非边界(事件中途)继续等, 宁可不发。Close 后停止。
func (h *sseHeartbeat) pump() {
	t := time.NewTicker(h.interval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			h.mu.Lock()
			// h.committed: 提交前不发心跳(见 committed 字段的说明)。
			if !h.closed && h.committed && h.boundary && time.Since(h.last) >= h.interval {
				if b := h.frame(); len(b) > 0 {
					_, _ = h.writeLocked(b)
					h.flush_() // 已持锁, 走无锁版本(公共 flush() 会二次加锁死锁)
				}
			}
			h.mu.Unlock()
		case <-h.done:
			return
		}
	}
}

// write 向客户端写真实数据并刷新空闲计时。Close 后为 no-op(客户端已断开)。
func (h *sseHeartbeat) write(b []byte) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.commitLocked() // 真实正文写出前提交响应头(只做一次)
	return h.writeLocked(b)
}

// Committed 报告响应头是否已提交(2026-09-24)。调用方据此决定"空流"还能不能
// 以真 502 结束: 未提交 → 直接返回 502 换候选; 已提交 → 只能往流里补错误帧。
func (h *sseHeartbeat) Committed() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.committed
}

// deferCommit 设置"首次真实正文写出前"的延迟提交动作(2026-09-24)。
// 必须在任何写发生之前调用。fn 为 nil 时退化为"首次写出时不做额外动作"
// (调用方已自行提交头, 与旧行为等价)。
func (h *sseHeartbeat) deferCommit(fn func()) {
	h.mu.Lock()
	h.commit = fn
	h.mu.Unlock()
}

// commitLocked 提交响应头(只做一次), 并把空闲计时起点挪到提交这一刻 ——
// 否则提交前的等待会被算进第一个心跳间隔。
func (h *sseHeartbeat) commitLocked() {
	if h.committed {
		return
	}
	h.committed = true
	h.last = time.Now()
	if h.commit != nil {
		h.commit()
	}
}

// flush 单独刷新(紧跟 write 之后)。持锁保证与 pump 的心跳写不交错。
func (h *sseHeartbeat) flush() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.flush_()
}

// flush_ 假定调用方已持锁。
func (h *sseHeartbeat) flush_() {
	if h.flusher != nil {
		h.flusher.Flush()
	}
}

// writeFlush 写真实数据并立即刷新(合并一次加锁)。
func (h *sseHeartbeat) writeFlush(b []byte) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		h.last = time.Now()
		h.track(b)
		return 0, nil
	}
	h.commitLocked() // 同 write: 真实正文写出前提交响应头
	n, err := h.writeLocked(b)
	h.flush_()
	return n, err
}

// Close 停止保活并置 closed, 使后续 write/flush 与在途 pump 都不再写客户端。
// 不关闭上游(底层 body 由中继的 defer 负责)。
func (h *sseHeartbeat) Close() {
	h.mu.Lock()
	h.closed = true
	h.mu.Unlock()
	h.once.Do(func() { close(h.done) })
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
// 所有 OpenAI 协议客户端都会安全忽略。只发往客户端, 不掺进上游字节流。
var openAIHeartbeatFrame = []byte("data: {\"choices\":[{\"index\":0,\"delta\":{}}]}\n\n")
