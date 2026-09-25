package app

import (
	"fmt"
	"log"
	"net/http"
)

// GatewayEntryStream-2 Task1: 入口 gate + trace。
//
// 背景: 冷启动时节点池为空, zen/region 受限路径若静默走 direct 直连,
// 直连连接会被缓存进共享 h2 池并污染后续选路(地区受限模型随后永远 403)。
// 入口必须显式设门: 受限模型池空即 503 retryable, 绝不静默直连。

// gateAllowsDirectForModel 池空时该模型是否允许 direct。
// region 受限模型一律不允许(直连大陆 IP 对它 100% 失败, 还会污染 h2 池);
// unrestricted 才允许, 并打明确日志(调用方据此决定 fail-closed 还是放行)。
func gateAllowsDirectForModel(modelID string) bool {
	if isRegionRestrictedModel(modelID) {
		return false
	}
	log.Printf("  gate: 出口池为空, 模型 %s 无地区限制, 允许 direct 兜底", modelID)
	return true
}

// chainNeedsPoolGate 候选链/请求是否受"池空门禁"约束:
// 链里任一 zen 候选是地区受限模型, 或请求的模型本身受限。
func chainNeedsPoolGate(chain []routeCandidate, requested string) bool {
	if isRegionRestrictedModel(requested) {
		return true
	}
	if m := zenModelIDOf(map[string]any{"model": requested}); m != "" && isRegionRestrictedModel(m) {
		return true
	}
	for _, c := range chain {
		if c.Upstream == upstreamZen && isRegionRestrictedModel(c.Model) {
			return true
		}
	}
	return false
}

// writePoolEmptyGate 池空门禁的 HTTP 收口。
// 池非空返回 false(调用方继续正常流程); 池空时先尝试 loadSubCache 重建,
// 仍空则: 受限模型写 503 retryable JSON(带 error code)并返回 true;
// 非受限模型返回 false(调用方走 direct, 日志由 gateAllowsDirectForModel 打出)。
func writePoolEmptyGate(w http.ResponseWriter, r *http.Request, modelID string) bool {
	if len(effectiveProxyList()) > 0 {
		return false
	}
	// 冷启动重建: 缓存节点立即进池, 不等首次订阅抓取。
	loadSubCache()
	syncNodeBox()
	if len(effectiveProxyList()) > 0 {
		return false
	}
	resolved := zenModelIDOf(map[string]any{"model": modelID})
	if resolved == "" {
		resolved = modelID
	}
	if gateAllowsDirectForModel(resolved) {
		return false
	}
	log.Printf("  gate: 出口池为空, 地区受限模型 %s 拒绝静默直连, 回 503 retryable", resolved)
	if tr := traceFrom(r.Context()); tr != nil {
		tr.SetError("no_exit", fmt.Sprintf("出口池为空且模型 %s 地区受限, 拒绝直连(可重试)", resolved))
	}
	writeJSON(w, http.StatusServiceUnavailable, map[string]any{
		"error": map[string]any{
			"message":   fmt.Sprintf("no exit available for region-restricted model %q; retry after nodes sync", resolved),
			"type":      "no_exit_available",
			"code":      "pool_empty_region_restricted",
			"retryable": true,
		},
	})
	return true
}

// recordChainTryFailure 一次候选尝试失败的统一记账: 冷却落位 +
// 决策轨迹补候选结局。替代各分支手写的 markCandidateCooldown +
// dec.addCandidate 散装组合 —— 散装写法的分支极易漏掉其中一半,
// 造成"面板候选恒绿 / 冷却未落"的静默丢弃。
func recordChainTryFailure(cand routeCandidate, class, reason string, status int, body []byte, dec *routeDecision) {
	applyCandidateFailure(cand, class, reason, body)
	recordUsageForCandidate(cand, false)
	if dec != nil {
		dec.addCandidate(cand.String(), "tried", reason, status, class)
	}
}

// noteZenEarlyFailure callZenAPI 在循环前提前返回时的记账补位:
// 请求没发出去不等于"没发生" —— 面板轨迹与请求日志仍需一条候选记录与
// 错误类别, 否则这些路径在面板里是"0 次尝试"的静默丢弃。
func noteZenEarlyFailure(dec *routeDecision, tr *reqTrace, model, reason string) {
	if dec != nil {
		dec.addCandidate(decZenCandidate(model), "tried", reason, 0, classEmpty)
	}
	if tr != nil {
		tr.AddAttempt()
		tr.SetError(classEmpty, reason)
	}
}

// poolEmptyDirectBlocked 拨号层守卫: 池空 + 地区受限模型时禁止直连兜底。
// 返回 true 表示已拦截(调用方直接返回错误, 不得继续 catch-all/Go 直连,
// 否则直连连接会写入共享 h2 池污染后续选路)。重建仍空才拦截:
// 冷启动瞬间缓存尚未来得及恢复时给一次重建机会。
func poolEmptyDirectBlocked(modelID string) bool {
	if modelID == "" || !isRegionRestrictedModel(modelID) {
		return false
	}
	if len(effectiveProxyList()) > 0 {
		return false
	}
	loadSubCache()
	syncNodeBox()
	if len(effectiveProxyList()) > 0 {
		return false
	}
	log.Printf("  gate: 出口池为空, 地区受限模型 %s 拒绝直连兜底(防 h2 池污染), fail-closed", modelID)
	return true
}
