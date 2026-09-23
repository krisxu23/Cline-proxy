package app

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"cline-go-proxy/internal/kit"
)

// 候选层冷却 —— 四层冷却中的第四层。
//
// 已有三层粒度都是"某类上游整体":
//   - 账号层: cline 账号
//   - 出口层: 节点/代理
//   - 熔断层: zen 全局
//
// 这一层粒度是**单个候选**(upstream:model), 只在候选链遍历时生效, 与上面三层
// 互不干扰。目的是让"这个上游的这个模型现在不可用"不再拖累整条链: 换下一站即可。

const (
	classRateLimit   = "rateLimit"
	classTimeout     = "timeout"
	classServerError = "serverError"
	// classClientError 未匹配的其它 4xx(400/413/422…): 客户端侧坏请求,
	// 既不是上游 5xx, 也不该记成模型硬失败(P2-9)。单独成类:
	// 冷却类别正确, 且 routing_dispatch 的模型可用性门只认 serverError/empty,
	// 于是"4xx 不计"的承诺不再被兜底分类击穿。
	classClientError = "clientError"
	classEmpty       = "empty"
	classNotFound    = "notFound"
	classForbidden   = "forbidden"
	classQuotaDay    = "quotaDay"
	classPermanent   = "permanent"
)

// defaultCooldownMs 各类别的默认冷却时长, 可被配置的 cooldownMs 段逐项覆盖。
// rateLimit 取 10 分钟: 上游限流窗口普遍是分钟级, 短于窗口会反复撞墙。
var defaultCooldownMs = map[string]int64{
	classRateLimit:   10 * 60 * 1000,
	classTimeout:     5 * 60 * 1000,
	classServerError: 2 * 60 * 1000,
	classClientError: 2 * 60 * 1000, // 客户端坏请求: 短冷却, 与 serverError 同档
	classEmpty:       5 * 60 * 1000,
	classNotFound:    60 * 60 * 1000,
	classForbidden:   60 * 60 * 1000,
}

// pacificTZ Google 免费层额度的重置时区。
// 与配额账本的日界时区(默认 Asia/Shanghai)是两个互相独立的时区 ——
// Google 按太平洋时间午夜重置, 账本按本地日界统计。
const pacificTZ = "America/Los_Angeles"

type candidateCool struct {
	until  int64 // unix ms
	class  string
	reason string
	// 半开探测(P2-23, 参照 OmniRoute autoCombo/selfHealing): 冷却到期后
	// 不直接视为痊愈 —— 第一次尝试是"探针", 成功才彻底恢复; 探针失败则
	// 冷却时长翻倍重进冷却, 连续失败最多翻到 24h 上限。
	probing    bool
	probeFails int
}

var (
	candidateCoolMu sync.Mutex
	candidateCools  = map[string]candidateCool{}
	// candidatePerms 永久剔除(无免费层 / 已下架 / 非 chat 模型)。
	// 进程内保存, 面板可查看与清空。
	candidatePerms = map[string]candidatePerm{}
)

// candidatePerm 永久剔除条目: 原因 + 下次自动复查时间。
//
// "永久"实为**软永久** —— 上游(模型目录 / 免费层配额)随时可能恢复, 到期后
// 放行让候选重新参与尝试, 减少"一次瞬时失败永久吞掉健康模型、只能手动清空"
// 的风险(2026-09-17 审查 R2-7); 面板的"清空永久剔除"仍然即时生效。
type candidatePerm struct {
	reason string
	until  int64
}

// permanentRetryAfterMs 软永久剔除的复查周期: 24h 后自动重新尝试一次。
const permanentRetryAfterMs = int64(24 * 60 * 60 * 1000)

// candidateKey 冷却与账本共用的候选标识。
func candidateKey(upstream, model string) string {
	return upstream + ":" + model
}

// cooldownDurationMs 单类别的生效时长: 配置覆盖优先于默认值。
// 配置未加载时(getZenConfig 为 nil)直接用默认值 —— 冷却层不该因为
// 启动早期的一次调用就 panic。
func cooldownDurationMs(class string) int64 {
	if cfg := getZenConfig(); cfg != nil {
		if v := cfg.CooldownMs[class]; v > 0 {
			return v
		}
	}
	return defaultCooldownMs[class]
}

func cooldownDuration(class string) time.Duration {
	return time.Duration(cooldownDurationMs(class)) * time.Millisecond
}

// markCandidateCooldown 把某个候选按错误类别冷却一段时间。
func markCandidateCooldown(upstream, model, class, reason string) {
	markCandidateCooldownFor(upstream, model, class, reason, 0)
}

// markCandidateCooldownFor 同 markCandidateCooldown, 但时长可被上游明确给出的
// RetryInfo 覆盖(overrideMs<=0 时按类别默认; 覆盖值同样参与探测失败翻倍)。
func markCandidateCooldownFor(upstream, model, class, reason string, overrideMs int64) {
	if upstream == "" || model == "" || class == "" {
		return
	}
	d := cooldownDurationMs(class)
	if overrideMs > 0 {
		d = overrideMs
	}
	if d <= 0 {
		return
	}
	k := candidateKey(upstream, model)
	candidateCoolMu.Lock()
	// 半开探测失败(P2-23): 冷却时长按连续探测失败次数翻倍, 上限 24h。
	// 这样"一直坏"的候选不会被每轮都白白探一次, 而"偶尔坏"的候选在
	// 一次成功后即彻底恢复。
	fails := 0
	if prev, ok := candidateCools[k]; ok && prev.probing {
		fails = prev.probeFails + 1
	}
	escalate := int64(1) << min(fails, 4) // 1,2,4,8,16 倍
	if escalate*d > int64(24*60*60)*1000 {
		escalate = int64(24*60*60) * 1000 / d
	}
	d *= escalate
	candidateCools[k] = candidateCool{
		until:      time.Now().UnixMilli() + d,
		class:      class,
		reason:     kit.Truncate(reason, 200) + fmt.Sprintf("(探测失败%d次)", fails),
		probing:    false,
		probeFails: fails,
	}
	candidateCoolMu.Unlock()
}

// markCandidateSuccess 候选成功: 若它此前在冷却/探测态, 彻底清除恢复健康。
// 半开探测语义的另一半(见 candidateCool.probing)。
func markCandidateSuccess(upstream, model string) {
	if upstream == "" || model == "" {
		return
	}
	k := candidateKey(upstream, model)
	candidateCoolMu.Lock()
	delete(candidateCools, k)
	candidateCoolMu.Unlock()
}

// markQuotaDayCooldown 当日免费额度耗尽: 冷却到提供方时区的下一个日界。
func markQuotaDayCooldown(upstream, model string, tzName, reason string) {
	until := nextMidnight(time.Now(), tzName)
	k := candidateKey(upstream, model)
	candidateCoolMu.Lock()
	candidateCools[k] = candidateCool{
		until:  until.UnixMilli(),
		class:  classQuotaDay,
		reason: kit.Truncate(reason, 200),
	}
	candidateCoolMu.Unlock()
}

// markCandidatePermanent 永久剔除该候选, 不再消耗试跑与候选位。
func markCandidatePermanent(upstream, model, reason string) {
	if upstream == "" || model == "" {
		return
	}
	k := candidateKey(upstream, model)
	candidateCoolMu.Lock()
	candidatePerms[k] = candidatePerm{
		reason: kit.Truncate(reason, 200),
		until:  time.Now().UnixMilli() + permanentRetryAfterMs,
	}
	delete(candidateCools, k)
	candidateCoolMu.Unlock()
}

// nextMidnight 指定时区的下一个零点; 时区不可用时退回本地时区。
func nextMidnight(now time.Time, tzName string) time.Time {
	loc, err := time.LoadLocation(tzName)
	if err != nil || loc == nil {
		loc = now.Location()
	}
	t := now.In(loc)
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, 1)
}

// candidateSkipReason 该候选当前是否应跳过; 空串表示可用。
// 返回的原因是给请求日志用的, 让面板能看出某一站为什么没被选上。
func candidateSkipReason(upstream, model string) string {
	if upstream == "" || model == "" {
		return ""
	}
	k := candidateKey(upstream, model)
	candidateCoolMu.Lock()
	defer candidateCoolMu.Unlock()
	if perm, ok := candidatePerms[k]; ok {
		if perm.until > time.Now().UnixMilli() {
			return "永久剔除: " + perm.reason
		}
		// 软永久到期: 放行重试(下次成功/失败按正常冷却流程处理)。
		delete(candidatePerms, k)
	}
	if c, ok := candidateCools[k]; ok {
		if c.until > time.Now().UnixMilli() {
			return "冷却中(" + c.class + "): " + c.reason
		}
		// 冷却已到期: 转入半开探测态并放行这一次尝试。
		// 成功 → markCandidateSuccess 彻底恢复; 失败 → markCandidateCooldown
		// 按探测失败次数加倍冷却。
		if !c.probing {
			c.probing = true
			candidateCools[k] = c
		}
	}
	return ""
}

// clearExpiredCandidateCooldowns 清掉已过期的冷却项, 避免 map 无限增长。
// 半开探测态(probing)的到期项**不删**: 升级计数依赖该条目存活
// (markCandidateCooldown 读 prev.probing/prev.probeFails 做翻倍), janitor
// 提前删掉会让"一直坏"的候选每次都被当成首次失败, 永远享受 1× 最短冷却、
// 翻倍状态机被打漏(P3-2)。probing 项交给 markCandidateSuccess /
// 下次失败覆盖 / resetCandidateState 收口。
func clearExpiredCandidateCooldowns() {
	now := time.Now().UnixMilli()
	candidateCoolMu.Lock()
	for k, c := range candidateCools {
		if c.until <= now && !c.probing {
			delete(candidateCools, k)
		}
	}
	for k, p := range candidatePerms {
		if p.until <= now {
			delete(candidatePerms, k)
		}
	}
	candidateCoolMu.Unlock()
}

// startCooldownJanitor 定期清理已过期的候选冷却项。
//
// 这个清理此前只被测试调用, 生产路径里没有任何人调用 —— candidateCools
// 于是只增不减。过期项虽然会因为时间比较而失效(不会误跳过候选), 但 map
// 会随着"见过多少候选"持续增长, 长期运行是纯泄漏。
func startCooldownJanitor() {
	go func() {
		t := time.NewTicker(10 * time.Minute)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				clearExpiredCandidateCooldowns()
			case <-appRootCtx.Done():
				// 收到退出信号: 停止候选冷却清理协程, 让进程能够真正停下。
				return
			}
		}
	}()
}

// candidateCoolingSnapshot 面板用: 当前处于冷却期的候选, 按剩余时间倒序。
func candidateCoolingSnapshot() []map[string]any {
	now := time.Now().UnixMilli()
	candidateCoolMu.Lock()
	defer candidateCoolMu.Unlock()
	out := make([]map[string]any, 0, len(candidateCools))
	for k, c := range candidateCools {
		if c.until <= now {
			continue
		}
		out = append(out, map[string]any{
			"key":         k,
			"class":       c.class,
			"reason":      c.reason,
			"remainMs":    c.until - now,
			"untilUnixMs": c.until,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i]["remainMs"].(int64) > out[j]["remainMs"].(int64)
	})
	return out
}

// resetCandidateState 测试辅助: 清空冷却是与永久剔除。
func resetCandidateState() {
	candidateCoolMu.Lock()
	candidateCools = map[string]candidateCool{}
	candidatePerms = map[string]candidatePerm{}
	candidateCoolMu.Unlock()
}

// classifyCandidateFailure 把一次上游失败归到冷却类别。
//
// status = 0 表示连接层失败(超时/中断), 归入 timeout。
// 400 一般不因冷却而好转, 但若解析出永久性拒绝(无免费层/已下架/非 chat)
// 就直接永久剔除, 否则按 clientError 短冷却一次 —— 未匹配 4xx 是客户端侧
// 坏请求, 归 serverError 会被模型可用性门当成上游硬失败(P2-9)。
func classifyCandidateFailure(status int, body []byte) (string, string) {
	bodyStr := string(body)
	var payload map[string]any
	_ = json.Unmarshal([]byte(bodyStr), &payload)
	reason := kit.Truncate(bodyStr, 200)

	// 这两类永久拒绝必须先判, 且与状态码无关:
	// Gemini 用 429 表达"全部免费层 limit=0"(= 该模型没有免费层),
	// 用 404 表达"目录里有但此处不提供 / 已下架"(spec §4.1)。
	// 若不先判, 429 会被当成普通限流, 于是每次遍历都白白重试一个永远不通的候选。
	if qf := parseQuotaFailure(payload); qf != nil && qf.NoFreeTier {
		return classPermanent, "该模型无免费层"
	}
	// 请求级资源 404(file/item/upload 等不存在)显式不冷却模型: 换模型不会让
	// 不存在的 file_id 变合法。**必须排在 permanentRejectionReason 之前** ——
	// 参考实现 isResourceNotFoundResponse 对 resource 404 显式返回 null(优先级
	// 高于外层 model_not_found 判定), 否则 resource 404 会被万物皆 404→permanent
	// 的规则错打成"模型已下架"。仅对 404 生效, 不碰其它状态码的永久判定。
	if status == 404 && isResourceNotFoundResponseStr(bodyStr) {
		return classResourceNotFound, "请求资源不存在: " + kitTruncateHead(bodyStr)
	}
	if p := permanentRejectionReason(status, payload); p != "" {
		return classPermanent, p
	}
	// Cloudflare 1010 指纹拒绝: 独立分类, 短冷却换节点, 绝不进账号级 banned。
	// 判定在规则表之前(且 403 专属), 避免被 generic forbidden 之类吞掉。
	if status == 403 && isCloudflareFingerprintRejection(bodyStr) {
		return classFingerprint, "Cloudflare 指纹拒绝: " + kitTruncateHead(bodyStr)
	}
	// 声明式规则表(P1-10): 正文特征优先于状态码 —— 状态码相同的 403 可能是
	// 地区封锁(24h 长冷却)也可能是 key 被拒, 只看状态码会混为一谈。
	if class, note := matchErrorRules(bodyStr); class != "" {
		return class, note
	}
	switch {
	case status == 0:
		return classTimeout, reason
	case status == 429:
		return classRateLimit, reason
	case status == 404:
		return classNotFound, reason
	case status == 401 || status == 403:
		return classForbidden, reason
	case status >= 500:
		return classServerError, reason
	case status >= 400:
		// 未匹配的其它 4xx(400/413/422…): 客户端侧坏请求。既不能顶着
		// serverError 的名义被记成模型硬失败(健康模型会被坏请求摘除约
		// 30 分钟), 冷却类别本身也该如实反映(P2-9)。
		return classClientError, reason
	}
	return classServerError, reason
}

// isGoogleModelID provider 名到 Google 方言的判定沿用 isGoogleProvider,
// 这里只做"该候选是否属于 Google"的薄封装, 便于冷却层决定重置时区。
func candidateUsesPacificReset(upstream string) bool {
	name := upstream
	if i := strings.Index(name, ":"); i >= 0 {
		name = name[:i]
	}
	cfg, ok := providerConfigFor(name)
	return ok && isGoogleProvider(cfg)
}

// ============ Key 健康(抄 ai-gateway: 5 次失败冷却 5 分钟) ============

const (
	keyFailThreshold = 5
	keyCooldownMs    = 5 * 60 * 1000
)

var keyHealthMu sync.Mutex
var keyHealth = map[string]map[string]*keyHealthState{}

type keyHealthState struct {
	failures  int
	demotedAt int64
}

// recordKeySuccess key 请求成功: 清零失败计数并解除降级。
func recordKeySuccess(provider, key string) {
	keyHealthMu.Lock()
	defer keyHealthMu.Unlock()
	if m := keyHealth[provider]; m != nil {
		if st := m[key]; st != nil {
			st.failures = 0
			st.demotedAt = 0
		}
	}
}

func recordKeyResult(provider, key string, status int, netErr bool) {
	if status == 429 {
		return
	}
	fail := netErr || status == 401 || status == 403 || status >= 500
	keyHealthMu.Lock()
	defer keyHealthMu.Unlock()
	m := keyHealth[provider]
	if m == nil {
		m = map[string]*keyHealthState{}
		keyHealth[provider] = m
	}
	st := m[key]
	if st == nil {
		st = &keyHealthState{}
		m[key] = st
	}
	if fail {
		// 降级窗口已过期: 先把失败计数清零再计数(P3-3)。否则曾被降级的 key
		// 带着累计值(≥5)跨过 5 分钟窗口, 再遇到**一次**偶发失败就立刻重新降级
		// —— 与"连续 N 次失败才降级"的设计语义不符, 抖动期多 key 池会被
		// 雪崩式逐个排空。清零后需要重新累计满 keyFailThreshold 才再次降级。
		if st.demotedAt > 0 && time.Now().UnixMilli()-st.demotedAt >= keyCooldownMs {
			st.failures = 0
			st.demotedAt = 0
		}
		st.failures++
		if st.failures >= keyFailThreshold {
			st.demotedAt = time.Now().UnixMilli()
		}
	} else {
		delete(m, key)
	}
}

// isKeyDemoted reports whether the key is currently cooled down after
// consecutive failures (single source of the threshold; see recordKeyResult).
func isKeyDemoted(provider, key string) bool {
	keyHealthMu.Lock()
	defer keyHealthMu.Unlock()
	st := keyHealth[provider][key]
	return st != nil && st.failures >= keyFailThreshold && time.Now().UnixMilli()-st.demotedAt < keyCooldownMs
}

func resetKeyHealthState() {
	keyHealthMu.Lock()
	keyHealth = map[string]map[string]*keyHealthState{}
	keyHealthMu.Unlock()
}
