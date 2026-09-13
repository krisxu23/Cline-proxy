package app

import (
	"encoding/json"
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
}

var (
	candidateCoolMu sync.Mutex
	candidateCools  = map[string]candidateCool{}
	// candidatePerms 永久剔除(无免费层 / 已下架 / 非 chat 模型)。
	// 进程内保存, 面板可查看与清空。
	candidatePerms = map[string]string{}
)

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
	if upstream == "" || model == "" || class == "" {
		return
	}
	d := cooldownDurationMs(class)
	if d <= 0 {
		return
	}
	k := candidateKey(upstream, model)
	candidateCoolMu.Lock()
	candidateCools[k] = candidateCool{
		until:  time.Now().UnixMilli() + d,
		class:  class,
		reason: kit.Truncate(reason, 200),
	}
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
	candidatePerms[k] = kit.Truncate(reason, 200)
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
	if why, ok := candidatePerms[k]; ok {
		return "永久剔除: " + why
	}
	if c, ok := candidateCools[k]; ok {
		if c.until > time.Now().UnixMilli() {
			return "冷却中(" + c.class + "): " + c.reason
		}
	}
	return ""
}

// clearExpiredCandidateCooldowns 清掉已过期的冷却项, 避免 map 无限增长。
func clearExpiredCandidateCooldowns() {
	now := time.Now().UnixMilli()
	candidateCoolMu.Lock()
	for k, c := range candidateCools {
		if c.until <= now {
			delete(candidateCools, k)
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
		for range t.C {
			clearExpiredCandidateCooldowns()
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
	candidatePerms = map[string]string{}
	candidateCoolMu.Unlock()
}

// classifyCandidateFailure 把一次上游失败归到冷却类别。
//
// status = 0 表示连接层失败(超时/中断), 归入 timeout。
// 400 一般不因冷却而好转, 但若解析出永久性拒绝(无免费层/已下架/非 chat)
// 就直接永久剔除, 否则按 serverError 短冷却一次。
func classifyCandidateFailure(status int, body []byte) (string, string) {
	var payload map[string]any
	_ = json.Unmarshal(body, &payload)
	reason := kit.Truncate(string(body), 200)

	// 这两类永久拒绝必须先判, 且与状态码无关:
	// Gemini 用 429 表达"全部免费层 limit=0"(= 该模型没有免费层),
	// 用 404 表达"目录里有但此处不提供 / 已下架"(spec §4.1)。
	// 若不先判, 429 会被当成普通限流, 于是每次遍历都白白重试一个永远不通的候选。
	if qf := parseQuotaFailure(payload); qf != nil && qf.NoFreeTier {
		return classPermanent, "该模型无免费层"
	}
	if p := permanentRejectionReason(status, payload); p != "" {
		return classPermanent, p
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
		st.failures++
		if st.failures >= keyFailThreshold {
			st.demotedAt = time.Now().UnixMilli()
		}
	} else {
		delete(m, key)
	}
}

// pickHealthyKeys returns observed healthy keys for the provider (empty if
// none observed). providerConfig.APIKey is a single key string today — there
// is no established multi-key config format (clinepass keeps its own key-pool
// file), so key sourcing arrives with provider-chat wiring; this stays
// observed-map-only until then.
func pickHealthyKeys(provider string) []string {
	now := time.Now().UnixMilli()
	keyHealthMu.Lock()
	defer keyHealthMu.Unlock()
	var out []string
	for k, st := range keyHealth[provider] {
		if st != nil && st.failures >= keyFailThreshold && now-st.demotedAt < keyCooldownMs {
			continue
		}
		out = append(out, k)
	}
	sort.Strings(out)
	return out
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
