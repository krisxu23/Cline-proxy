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
	"strings"
)

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

// probeStreamFirstEvent 探测首个非空 SSE data 事件。
// 返回 (是否空流, 可继续读取的响应体)。空流时返回的 body 已无需使用。
func probeStreamFirstEvent(body io.ReadCloser) (bool, io.ReadCloser) {
	// 内层空闲上限: 上游彻底静默时探测不会永久挂起(中继稍后还会包一层,
	// 双层无害 —— 内层先到就断)。
	idle := newIdleAbortReader(body, streamIdleTimeout())
	br := bufio.NewReader(idle)

	var buf bytes.Buffer
	for {
		line, err := br.ReadString('\n')
		buf.WriteString(line)
		if t := strings.TrimSpace(line); strings.HasPrefix(t, "data:") {
			payload := strings.TrimSpace(strings.TrimPrefix(t, "data:"))
			if payload != "" && payload != "[DONE]" {
				return false, &prefixedBody{
					prefix: bytes.NewReader(buf.Bytes()),
					rest:   br,
					closer: idle,
				}
			}
		}
		if err != nil {
			// EOF(或空闲超时)前都没读到有效事件: 视作空流, 换下一站
			return true, nil
		}
	}
}
