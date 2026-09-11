package app

import (
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
)

// 统一候选链 —— 把客户端给的模型名解析成一条有序的候选列表, 由调度层逐站尝试。
//
// 站点的 Upstream 有三类, 与现有上游实现一一对应:
//   - "zen"    : opencode zen(匿名可用, 经节点池出口)
//   - "cline"  : Cline 账号池(Model == "*" 表示"池内当前可用模型")
//   - provider : 通用 OpenAI 兼容上游(名字即 provider 名)
//
// 解析优先级(spec §4.2):
//  1. 路由别名(配置 routes 段, 如 free-best) → 展开候选链;
//  2. 其余 → 交给现有 routeModel 逻辑, 本包不介入(matched=false)。
//
// provider:model 前缀直选由调用方在更早的分支处理, 这里不再重复。

// routeCandidate 候选链的一站。
type routeCandidate struct {
	Upstream string
	Model    string
}

func (c routeCandidate) String() string { return c.Upstream + ":" + c.Model }

// upstreamError 统一的上游失败 —— 由 zenUpstreamError 泛化而来。
//
// 候选链上每一站都可能是 zen / cline 池 / 通用 provider, 上层需要同一套
// "上游 4xx 原样透传、其余 502" 的判定, 而不是只认 zen 的错误类型。
type upstreamError struct {
	Upstream string
	Status   int
	Body     string
}

func (e *upstreamError) Error() string {
	if e.Upstream == "" {
		return fmt.Sprintf("upstream %d: %s", e.Status, e.Body)
	}
	return fmt.Sprintf("%s upstream %d: %s", e.Upstream, e.Status, e.Body)
}

// upstreamErrorStatus 上游 4xx 按原状态返回(如 403 地域限制、429 限流),
// 网络错误与上游 5xx 统一 502。
//
// 三种错误类型都要认: 通用 Provider 的 Chat 把非 200 包装成 providerError,
// zen 用自己的 zenUpstreamError, 候选链自身用 upstreamError。
func upstreamErrorStatus(err error) int {
	switch e := err.(type) {
	case *upstreamError:
		if e.Status >= 400 && e.Status < 500 {
			return e.Status
		}
	case *zenUpstreamError:
		if e.Status >= 400 && e.Status < 500 {
			return e.Status
		}
	case *providerError:
		if e.Status >= 400 && e.Status < 500 {
			return e.Status
		}
	}
	return http.StatusBadGateway
}

// chainErrorStatusBody 从上游错误里取出 HTTP 状态与响应体。
//
// 必要性: 通用 Provider 的 Chat 对非 200 直接返回 providerError(而不是响应),
// 若不从这里取状态码, "429 限流""404 已下架"都会被当成网络故障,
// 结果是该候选只被短冷却、并且错误码被统一成 502。
func chainErrorStatusBody(err error) (int, []byte) {
	switch e := err.(type) {
	case *providerError:
		return e.Status, []byte(e.Body)
	case *zenUpstreamError:
		return e.Status, []byte(e.Body)
	case *upstreamError:
		return e.Status, []byte(e.Body)
	}
	return 0, nil
}

// defaultAutoRouterAlias 自动路由的默认模型名。
// 用户可以改成任何自己喜欢的名字(配置 router.alias), 页面上的"自动路由模型名"就是它。
const defaultAutoRouterAlias = "auto-router"

// legacyFreeBestAlias 早于可配置别名的名字。保留识别, 已有配置继续可用。
const legacyFreeBestAlias = "free-best"

// clinePoolPlaceholder cline 池占位: 具体用哪个模型由池内轮询决定。
const clinePoolPlaceholder = "*"

// zenRouterConfig 自动路由配置。
type zenRouterConfig struct {
	Alias string `json:"alias"` // 自动路由模型名, 默认 auto-router
	// Providers 参与自动路由的供应商名。它同时是页面上的勾选状态:
	// 用户勾了哪些供应商, 页面就列出哪些供应商的模型供进一步勾选。
	Providers []string `json:"providers,omitempty"`
}

// autoRouterAlias 当前生效的自动路由模型名。
func autoRouterAlias() string {
	if cfg := getZenConfig(); cfg != nil {
		if a := strings.TrimSpace(cfg.Router.Alias); a != "" {
			return a
		}
	}
	return defaultAutoRouterAlias
}

// isAutoRouterAlias 该模型名是否是自动路由别名(含历史名)。
func isAutoRouterAlias(id string) bool {
	id = strings.TrimSpace(id)
	return id != "" && (id == autoRouterAlias() || id == legacyFreeBestAlias)
}

// resolveRouteChain 解析候选链。
//
// matched=false 表示这不是一条候选链, 调用方应回退到原有路由(行为完全不变)。
// matched=true 且 errMsg 非空表示别名被识别但当前无法展开(如一个 provider 都没配),
// 调用方应回 400 并带上 errMsg —— 比静默回退成 zen 更容易排查。
func resolveRouteChain(model string) (cands []routeCandidate, matched bool, errMsg string) {
	id := strings.TrimSpace(model)
	if id == "" {
		return nil, false, ""
	}
	cfg := getZenConfig()

	// 显式配置的候选链优先(自动路由别名自己配过的那一条也在这里命中)。
	// 空列表不算显式配置: 页面允许"一个模型都不勾", 语义是回落到默认链,
	// 而不是让别名彻底失效。
	if cfg != nil {
		if list, ok := cfg.Routes[id]; ok && len(list) > 0 {
			return appendChainTail(expandRouteList(id, list)), true, ""
		}
	}
	if !isAutoRouterAlias(id) {
		return nil, false, ""
	}

	// 没有显式候选 -> 默认链: 已配置 provider 的免费模型。
	chain := defaultAutoRouterChain()
	chain = appendChainTail(chain)
	if len(chain) == 0 {
		return nil, true, fmt.Sprintf(
			"model %q has no candidates: no provider with an API key is configured, "+
				"and no model has been selected for it", id)
	}
	return chain, true, ""
}

// appendChainTail 把 discovery 收录的模型接到候选链尾部。
//
// 手动配置的条目永远在前且顺序不变 —— 发现的结果只是补充, 不能插队;
// 同一个 upstream:model 已在前面的站点里出现时也不再重复追加。
func appendChainTail(manual []routeCandidate) []routeCandidate {
	tail := discoveredCandidates()
	if len(tail) == 0 {
		return manual
	}
	seen := make(map[string]bool, len(manual)+len(tail))
	for _, c := range manual {
		seen[c.String()] = true
	}
	out := manual
	for _, c := range tail {
		if seen[c.String()] {
			continue
		}
		seen[c.String()] = true
		out = append(out, c)
	}
	return out
}

// expandRouteList 把配置里的一串条目展开成候选。
//
// 条目形态:
//   - "cline:*"          -> cline 池占位
//   - "upstream:model"   -> 该上游的该模型
//   - "model"(无前缀)     -> 交给 routeModel 判断归属, reject 的条目直接丢弃
func expandRouteList(alias string, list []string) []routeCandidate {
	out := make([]routeCandidate, 0, len(list))
	for _, raw := range list {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			continue
		}
		if up, m, ok := strings.Cut(entry, ":"); ok {
			up = strings.TrimSpace(up)
			m = strings.TrimSpace(m)
			if up == "" || m == "" {
				log.Printf("  routes: %s 跳过无法解析的条目 %q", alias, raw)
				continue
			}
			out = append(out, routeCandidate{Upstream: up, Model: m})
			continue
		}
		// 无前缀: 沿用现有归属判定, 付费/未知的 zen 模型会被 reject
		switch routeModel(entry) {
		case "zen":
			out = append(out, routeCandidate{Upstream: upstreamZen, Model: entry})
		case "cline":
			out = append(out, routeCandidate{Upstream: upstreamCline, Model: entry})
		default:
			log.Printf("  routes: %s 跳过不可路由的条目 %q", alias, raw)
		}
	}
	return out
}

// defaultAutoRouterChain 默认链: 已配置 provider 的全部免费模型。
//
// 顺序取 provider 名的字典序 —— 配置里 providers 是 map, 本身没有声明顺序,
// 用字典序保证每次展开结果一致(否则同一请求两次可能命中不同站点)。
// 之后由调度层按各自可用性跳过。
func defaultAutoRouterChain() []routeCandidate {
	var out []routeCandidate
	for _, name := range providerNames() {
		cfg, ok := providerConfigFor(name)
		if !ok || cfg.APIKey == "" {
			continue
		}
		p := providerByName(name)
		if p == nil {
			continue
		}
		for _, m := range p.freeModelIDs() {
			if m.ID == "" {
				continue
			}
			out = append(out, routeCandidate{Upstream: name, Model: m.ID})
		}
	}
	return out
}

// candidateSkip 候选当前是否应跳过; 返回原因(空串 = 可用)。
// 跳过条件: 候选层冷却中 / 永久剔除 / 当日配额已尽 / 上游未配置 /
// 无可用账号 / 非免费 zen 模型。
func candidateSkip(c routeCandidate) string {
	if why := candidateSkipReason(c.Upstream, c.Model); why != "" {
		return why
	}
	if usageLimitReached(candidateKey(c.Upstream, c.Model)) {
		return "当日配额已尽"
	}
	switch c.Upstream {
	case upstreamZen:
		if c.Model == clinePoolPlaceholder {
			return "占位符不能用于 zen"
		}
		if _, ok := resolveZenFreeModel(c.Model); !ok {
			return "非免费 zen 模型"
		}
		return ""
	case upstreamCline:
		if !clinePoolReady() {
			return "cline 账号池无可用账号"
		}
		return ""
	default:
		cfg, ok := providerConfigFor(c.Upstream)
		if !ok {
			return "provider 未配置"
		}
		if cfg.APIKey == "" {
			return "provider 缺少 API key"
		}
		return ""
	}
}

// describeRouteChain 面板用: 展示一条候选链的当前实际顺序, 标注每站是否可用。
func describeRouteChain(alias string) map[string]any {
	cands, matched, errMsg := resolveRouteChain(alias)
	out := map[string]any{
		"alias":   alias,
		"matched": matched,
		"error":   errMsg,
		"hops":    []map[string]any{},
	}
	if !matched || errMsg != "" {
		return out
	}
	hops := make([]map[string]any, 0, len(cands))
	for _, c := range cands {
		hops = append(hops, map[string]any{
			"upstream": c.Upstream,
			"model":    c.Model,
			"skip":     candidateSkip(c),
		})
	}
	out["hops"] = hops
	return out
}

// routeAliasNames 可用作模型名的路由别名。
// 自动路由别名排最前(它是用户实际要填的那个), 其余按字典序。
func routeAliasNames() []string {
	seen := map[string]bool{}
	out := make([]string, 0, 4)
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		out = append(out, s)
	}

	primary := autoRouterAlias()
	add(primary)

	rest := make([]string, 0, 4)
	if primary != legacyFreeBestAlias {
		rest = append(rest, legacyFreeBestAlias)
	}
	if cfg := getZenConfig(); cfg != nil {
		for k := range cfg.Routes {
			if k != primary {
				rest = append(rest, k)
			}
		}
	}
	sort.Strings(rest)
	for _, s := range rest {
		add(s)
	}
	return out
}
