package app

import (
	"context"
	"testing"
	"time"
)

// cline 非流式的**总超时**必须存在(2026-09-24 审计 P1-3)。
//
// 为什么必须有: 这条路径用的 getZenHTTPClient() 没有客户端级 Timeout, 而它的 https
// 侧路(h2)被 RegisterProtocol 旁路了主 transport 的 ResponseHeaderTimeout —— 剩下的
// 时间防线只有 h2 的 ReadIdleTimeout(30s)+PING(15s), 那只覆盖"对端完全不发帧"。
// 上游"响应头到了、正文停摆"(或持续吐心跳却不收尾)时, 调用方的 io.ReadAll 会一直等,
// handler 与 goroutine 长期占着, 客户端只能自己掐。
//
// 用例刻意钉住**两侧**: 非流式必须有 deadline, 流式必须没有 —— 后者若被误加,
// 长回答会在跑到一半时被掐断(流式靠 idleAbortReader 的空闲中断兜, 不该有总时限)。
func TestClineAttemptCtxNonStreamHasTotalTimeout(t *testing.T) {
	ctx, cancel := clineAttemptCtx(context.Background(), false)
	if cancel == nil {
		t.Fatal("非流式必须带总超时(cancel 不得为 nil)")
	}
	defer cancel()
	dl, ok := ctx.Deadline()
	if !ok {
		t.Fatal("非流式 ctx 必须带 deadline")
	}
	remain := time.Until(dl)
	if remain <= 0 || remain > nonStreamUpstreamTimeout {
		t.Fatalf("deadline 必须落在 (0, %v] 内(与 zen 侧同口径), 实得 %v",
			nonStreamUpstreamTimeout, remain)
	}
}

func TestClineAttemptCtxStreamHasNoTotalTimeout(t *testing.T) {
	ctx, cancel := clineAttemptCtx(context.Background(), true)
	if cancel != nil {
		cancel()
		t.Fatal("流式不得设总超时 —— 长回答会被中途掐断")
	}
	if _, ok := ctx.Deadline(); ok {
		t.Fatal("流式 ctx 不得带 deadline")
	}
}
