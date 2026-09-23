package app

// 流式早断守卫 (参照 OmniRoute 的 STREAM_EARLY_EOF 语义, MIT; Go 侧实现):
//
// 上游可能回 200 + `Content-Type: text/event-stream` 却**立即结束**(空流):
// 常见于节点半死或上游静默拒绝。候选链的换站时机是"首字节之前", 一旦我们
// 把 200 写回客户端就只能吃下空答案 —— 用户侧表现为"模型返回空内容"。
//
// 守卫: 提交前读首个非空 data: 事件 ——
//   - 读到有效事件 → 提交, 已读字节原样接回, 由空闲中断与心跳继续兜底;
//   - EOF **或空闲超时**前都没读到有效事件 → 判本站空流, 换下一站。
//   (注意: 空闲超时也走换站而非提交 —— 90 秒零字节基本等于节点已死;
//    旧注释曾承诺"超时→提交", 与实现不符, 2026-09-15 R2 审计 F6 修正。)
//
// 数据安全: 探测只用一个 bufio 顺序读; 结束后把"已消费字节 + 仍在缓冲区的
// 续读句柄"一起接回响应体, 不存在被丢弃的在途读(探测读受空闲上限约束,
// 不会永久挂起)。

import (
	"bufio"
	"bytes"
	"io"
	"time"
)

// probeBufMaxBytes 探测阶段回放缓冲 buf 的字节上限(P1-1)。
// 上游持续发 ping/heartbeat 但迟迟不发首个真实事件时, buf 会随心跳无限
// 增长 → 内存无上界。超过上限即判探测失败(走空流换站)。
const probeBufMaxBytes = 1 << 20

// prefixedBody 把探测阶段已消费的前缀与续读句柄合回一个响应体;
// Close 透传到最底层(避免上游连接泄漏)。
type prefixedBody struct {
	prefix *bytes.Reader
	rest   io.Reader
	closer io.Closer
}

func (b *prefixedBody) Read(p []byte) (int, error) {
	if b.prefix != nil && b.prefix.Len() > 0 {
		return b.prefix.Read(p)
	}
	return b.rest.Read(p)
}

func (b *prefixedBody) Close() error {
	if b.closer != nil {
		return b.closer.Close()
	}
	return nil
}

// probeStreamFirstEvent 探测首个**有效** SSE 事件（照抄升级）。
//
// ★ 旧实现只看 `data:` 行非空，会把 error-only 帧
// （`data: {"error":{"message":"rate limit"}}`）误判为"流已就绪"。
// 现在改用 stream_readiness.go 的 appendStreamReadinessSignal +
// finishStreamReadinessSignal 状态机：
//   - 跳过 ping/keepalive/heartbeat 事件
//   - 跳过空 data 与 [DONE]
//   - JSON 解析后判 hasNonPingStructuredPayload（error-only 帧不算就绪）
//   - 顺带捕获 upstreamDiagnostic 供换站日志
//
// 返回 (是否空流, 可继续读取的响应体, 上游诊断信息)。空流时 body 已无需使用。
func probeStreamFirstEvent(body io.ReadCloser) (bool, io.ReadCloser, string) {
	// 内层空闲上限: 上游彻底静默时探测不会永久挂起(中继稍后还会包一层,
	// 双层无害 —— 内层先到就断)。
	idle := newIdleAbortReader(body, streamIdleTimeout())
	br := bufio.NewReader(idle)

	st := &streamReadinessState{}
	var buf bytes.Buffer
	// 整体 deadline(P1-1): 下面的 idle 只约束**单次** read, 上游每隔一小段
	// 时间就发一个心跳(间隔 < idle 上限)但不发真实事件时, 循环会永远转下去
	// → 客户端无限挂起。探测阶段另设总时限, 到点即判空流换下一站。
	deadline := time.Now().Add(streamIdleTimeout())
	for {
		line, err := br.ReadString('\n')
		buf.WriteString(line)
		// 喂给状态机（按行），同时处理 \r\n
		if line != "" {
			if appendStreamReadinessSignal(st, line) {
				return false, &prefixedBody{
					prefix: bytes.NewReader(buf.Bytes()),
					rest:   br,
					closer: idle,
				}, st.upstreamDiagnostic
			}
		}
		if err != nil {
			// EOF(或空闲超时)前都没读到有效事件: 视作空流, 换下一站
			// 先冲刷残留的 pendingLine，处理最后一个不完整帧
			if finishStreamReadinessSignal(st) {
				return false, &prefixedBody{
					prefix: bytes.NewReader(buf.Bytes()),
					rest:   br,
					closer: idle,
				}, st.upstreamDiagnostic
			}
			return true, nil, st.upstreamDiagnostic
		}
		// 总时限 / 缓冲上限(P1-1): 任一超限即判探测失败(走空流换站)。
		// 超限时同样先冲刷 pendingLine —— 已经读到手的完整事件不能丢,
		// 字节回放语义(routing_dispatch 依赖)与 EOF 路径保持一致。
		if time.Now().After(deadline) || buf.Len() > probeBufMaxBytes {
			if finishStreamReadinessSignal(st) {
				return false, &prefixedBody{
					prefix: bytes.NewReader(buf.Bytes()),
					rest:   br,
					closer: idle,
				}, st.upstreamDiagnostic
			}
			return true, nil, st.upstreamDiagnostic
		}
	}
}
