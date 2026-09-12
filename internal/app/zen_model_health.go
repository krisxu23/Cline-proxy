package app

import (
	"log"
	"sync"
	"time"
)

// zen 模型可用性门: 只放行"有效"的免费模型。
//
// 目录(/models)只保证"模型存在", 不保证"此刻能聊"(如下架、配额墙、上游 500)。
// 这里按连续硬失败计数(5xx/超时/网络/空响应; 429 与 4xx 不计, 它们说明模型存在):
// 达到阈值即标不可用 30 分钟 —— 列表不再出现, 请求快速失败不再烧重试;
// 成功一次清零, 过期后放行一次探测自愈。

const (
	zenModelFailThreshold = 5
)

var zenModelHoldMs = int64(30 * 60 * 1000)

type zenModelHealthState struct {
	consecFails int
	downUntilMs int64
}

var (
	zenModelHealthMu sync.Mutex
	zenModelHealth   = map[string]*zenModelHealthState{}
)

// recordZenModelResult 记录一次 zen 模型的上游结果。rawID 可为任意用户写法,
// 内部归一到模型 ID; 目录外 ID 直接忽略。
func recordZenModelResult(rawID string, hardFail bool) {
	id := ""
	if m, ok := resolveZenModel(rawID); ok && m != nil {
		id = m.ID
	}
	if id == "" {
		return
	}
	now := time.Now().UnixMilli()
	zenModelHealthMu.Lock()
	defer zenModelHealthMu.Unlock()
	st := zenModelHealth[id]
	if st == nil {
		st = &zenModelHealthState{}
		zenModelHealth[id] = st
	}
	if !hardFail {
		st.consecFails = 0
		st.downUntilMs = 0
		return
	}
	st.consecFails++
	if st.consecFails >= zenModelFailThreshold && now >= st.downUntilMs {
		st.downUntilMs = now + zenModelHoldMs
		log.Printf("  zen: model %s 连续 %d 次硬失败, 暂停使用 %d 分钟", id, st.consecFails, zenModelHoldMs/60000)
	}
}

// zenModelUnavailable 该模型当前是否被暂停使用(过期自动放行探测)。
func zenModelUnavailable(modelID string) bool {
	if modelID == "" {
		return false
	}
	zenModelHealthMu.Lock()
	defer zenModelHealthMu.Unlock()
	st := zenModelHealth[modelID]
	if st == nil {
		return false
	}
	if st.consecFails < zenModelFailThreshold {
		return false
	}
	return time.Now().UnixMilli() < st.downUntilMs
}

// resetZenModelHealth 测试辅助: 清空全部计数。
func resetZenModelHealth() {
	zenModelHealthMu.Lock()
	zenModelHealth = map[string]*zenModelHealthState{}
	zenModelHealthMu.Unlock()
}
