package app

import (
	"context"
	"log"
	"os"
	"time"
)

// Shutdown 触发进程级优雅退出: 取消 rootCtx → 后台循环停止 → server 关闭 →
// 刷盘 → 关 sing-box 节点。托盘"退出"与未来可能的 RPC 控制面都走这里。
func Shutdown() {
	if appRootCancel != nil {
		appRootCancel()
	}
}

// GracefulExit 同步执行完整优雅退出并终止进程, 供托盘"退出"菜单调用:
// 停服 → 刷请求日志(超时兜底) → 关闭 sing-box 节点 → 关闭诊断日志 → os.Exit。
func GracefulExit() {
	Shutdown()
	doGracefulShutdown()
	os.Exit(0)
}

// doGracefulShutdown 实际执行收口工作, 用 sync.Once 保证只跑一次(信号与显式
// 退出可能同时触发)。
func doGracefulShutdown() {
	shutdownOnce.Do(func() {
		// 1) 停服: server.Shutdown 会停止接收新连接并等待在途请求完成, 自带 5s 超时兜底。
		if appServer != nil {
			sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := appServer.Shutdown(sctx); err != nil {
				log.Printf("shutdown: server.Shutdown 超时: %v", err)
			}
		}
		// 2) 请求日志: 把 channel 里已入队的尽量刷出去; 超时也不阻塞退出
		//    (超时意味着写协程卡死, 强行退出比无限等待更可取)。
		closeReqLogsTimed(3 * time.Second)
		// 3) 账号池与用量账本: 这两份数据平时靠后台 ticker / flush channel 批量落盘,
		//    退出时若还没轮到, 就会丢掉最后 ≤30s 的 token 计数与用量 —— 而它们正是
		//    用户此刻在面板上看着的数字。两者内部各自加锁, 可直接调用。
		flushPoolLocked()
		saveUsageLedger()
		// 4) sing-box 节点: 释放端口与句柄。nodeBox 为 nil(如跳过节点盒的测试)时直接跳过。
		closeNodeBoxTimed(3 * time.Second)
		// 5) 流式诊断日志句柄。
		closeStreamLog()
	})
}

// closeReqLogsTimed 在超时内等待请求日志写协程刷盘并关闭句柄; 超时则尽力而为。
func closeReqLogsTimed(timeout time.Duration) {
	done := make(chan struct{})
	go func() {
		closeReqLogs()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		log.Printf("shutdown: 请求日志关闭超时, 部分已排队日志可能未落盘")
	}
}

// closeNodeBoxTimed 在超时内关闭 sing-box 节点盒; 超时则放弃等待。
//
// 必须先持 nodeMu 把句柄"摘下来"再锁外 Close:
//   - 退出可能与订阅刷新触发的 syncNodeBox 交错(那条 ticker 在收到退出信号前
//     仍会跑), 而 syncNodeBox 是在 nodeMu 下读写 nodeBox 的, 无锁读属于数据竞争;
//   - 摘下并置 nil 之后, 并发的 syncNodeBox 在替换阶段会看到 nodeBox != prevBox,
//     从而走"丢弃本次新实例"的分支 —— 正是退出时想要的语义, 也避免了同一个
//     Box 被 Close 两次。
func closeNodeBoxTimed(timeout time.Duration) {
	nodeMu.Lock()
	box := nodeBox
	nodeBox = nil
	nodeMu.Unlock()
	if box == nil {
		return
	}
	done := make(chan struct{})
	go func() {
		box.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		log.Printf("shutdown: 节点盒关闭超时, 端口可能需手动回收")
	}
}

// ============================================================================
// 流式诊断日志(free-router-stream.log)的共享写句柄
//
// 每次 anthropic 流式请求都会把出站 SSE 事件追加进该文件(内容只有模型输出文本,
// 不含请求头与 API key, 不是凭据泄露), 此前无轮转上限会无限增长。复用
// free-router.log 的 10MiB 截断思路: 多并发请求共用同一句柄并加锁, 超过上限则
// 截断成空, 只保留最近内容。
// ============================================================================
