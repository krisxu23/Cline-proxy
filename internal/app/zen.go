package app

import (
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"
)

// ZenModel opencode zen 免费模型定义
type ZenModel struct {
	ID      string   `json:"id"`
	Aliases []string `json:"aliases,omitempty"`
	Context int      `json:"context"`
	Output  int      `json:"output"`
	Source  string   `json:"source"` // seed=内置 / synced=动态同步
}

var zenSeedModels = []ZenModel{
	{"deepseek-v4-flash-free", []string{"deepseek-v4-flash", "deepseek-v4"}, 200000, 128000, "seed"},
	{"mimo-v2.5-free", []string{"mimo-v2.5", "mimo"}, 200000, 32000, "seed"},
	{"ling-3.0-flash-free", []string{"ling-3.0-flash", "ling"}, 200000, 32768, "seed"},
	{"nemotron-3-ultra-free", []string{"nemotron-3-ultra", "nemotron"}, 1000000, 128000, "seed"},
	{"north-mini-code-free", []string{"north-mini-code", "north-mini"}, 256000, 64000, "seed"},
	{"laguna-s-2.1-free", []string{"laguna-s-2.1", "laguna"}, 200000, 32768, "seed"},
	{"longcat-2.0-free", []string{"longcat-2.0", "longcat"}, 200000, 32768, "seed"},
	{"big-pickle", nil, 200000, 32000, "seed"},
}

var (
	zenModelsMu sync.RWMutex
	zenModels   = make(map[string]*ZenModel) // 主表:ID
	zenAliases  = make(map[string]*ZenModel) // 别名表
)

const zenAPIBase = "https://opencode.ai/zen/v1"

func initZenModels() {
	zenModelsMu.Lock()
	defer zenModelsMu.Unlock()
	if len(zenModels) > 0 {
		return
	}
	for _, m := range zenSeedModels {
		cp := m
		zenModels[cp.ID] = &cp
		for _, a := range cp.Aliases {
			zenAliases[a] = &cp
		}
	}
	// 种子之后恢复上次同步到的模型: 目录同步失败(节点抖动)时,
	// 用户正在使用的同步模型(如 muse-spark-*)不至于从目录里消失。
	loadZenModelsCache()
}

// zenRejectMessage 统一解释 zen 模型被拒的真实原因。之前固定说 "paid zen
// model", 但目录同步失败时已同步的模型也会暂时缺席(与付费无关), 误导排查。
func zenRejectMessage(model string) string {
	return fmt.Sprintf("model %q is not an available free zen model: it is not in the current zen catalog "+
		"(either it requires a paid plan, or the zen catalog sync has not succeeded yet — "+
		"it retries automatically; cached models stay available)", model)
}

// resolveZenModel 解析模型名到 zen 模型。支持 "zen/<id>"、"opencode/<id>"
// 前缀与裸别名。别名优先: 同步来的付费同名模型(如 deepseek-v4-flash)不会
// 覆盖 free 别名解析。
func resolveZenModel(id string) (*ZenModel, bool) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, false
	}
	zenModelsMu.RLock()
	defer zenModelsMu.RUnlock()
	if m, ok := zenAliases[id]; ok {
		return m, true
	}
	for _, prefix := range []string{"zen/", "opencode/"} {
		if strings.HasPrefix(id, prefix) {
			short := strings.TrimPrefix(id, prefix)
			if m, ok := zenAliases[short]; ok {
				return m, true
			}
			if m, ok := zenModels[short]; ok {
				return m, true
			}
		}
	}
	if m, ok := zenModels[id]; ok {
		return m, true
	}
	return nil, false
}

// isZenFreeModel 免费判定: seed 白名单 / ID 带 -free 后缀 / 用户手动启用。
//
// 第三条是为 opencode 的**免费但未标注**测试模型加的: 上游会不定期放进
// 新模型(如 union-alpha), 它们是免费的但 ID 不带 -free 后缀, 也不会出现在
// seed 白名单里 —— 目录同步能拉到它们(进 zenModels), 但纯靠后缀判定永远
// 不会放行, 用户侧表现就是"这个模型永远拉取不到"。面板上勾选的模型写入
// cfg.EnabledModels, 这里放行。
func isZenFreeModel(m *ZenModel) bool {
	if m == nil {
		return false
	}
	if m.Source == "seed" || strings.HasSuffix(m.ID, "-free") {
		return true
	}
	return zenModelEnabled(m.ID)
}

// isAutoFreeZenModel 自动免费判定: 只看 seed 白名单 / ID 带 -free 后缀,
// **不含**用户手动启用。
//
// 与 isZenFreeModel 的区别: 后者把"用户手动启用"也算作免费 —— 那是给
// 路由/可用性判断用的(启用即放行); 面板展示"自动免费锁定勾选"必须用它,
// 否则一个手动启用的测试模型会被显示成自动免费, 勾选框被锁死, 用户再也
// 无法从面板取消(2026-09-17 审查 C1)。
func isAutoFreeZenModel(m *ZenModel) bool {
	if m == nil {
		return false
	}
	return m.Source == "seed" || strings.HasSuffix(m.ID, "-free")
}

// zenModelEnabled 该模型 ID 是否被用户在面板上手动启用。
// 热路径(routeModel 每次请求都调)读克隆配置太重, 用一个读多写少的集合缓存,
// 配置变更时由 refreshZenEnabledModels 重建。
var (
	zenEnabledMu       sync.RWMutex
	zenEnabledModelSet = map[string]bool{}
)

// refreshZenEnabledModels 依当前 zen 配置重建手动启用集合。
// 在配置加载后、以及每次 setZenConfig 之后调用。
func refreshZenEnabledModels() {
	cfg := getZenConfig()
	set := make(map[string]bool, len(cfg.EnabledModels))
	for _, id := range cfg.EnabledModels {
		if id = strings.TrimSpace(id); id != "" {
			set[id] = true
		}
	}
	zenEnabledMu.Lock()
	zenEnabledModelSet = set
	zenEnabledMu.Unlock()
}

func zenModelEnabled(id string) bool {
	if id == "" {
		return false
	}
	zenEnabledMu.RLock()
	defer zenEnabledMu.RUnlock()
	return zenEnabledModelSet[id]
}

// resolveZenFreeModel 只解析免费 zen 模型(连续硬失败被暂停的除外)。
func resolveZenFreeModel(id string) (*ZenModel, bool) {
	m, ok := resolveZenModel(id)
	if !ok || !isZenFreeModel(m) {
		return nil, false
	}
	if zenModelUnavailable(m.ID) {
		return nil, false
	}
	return m, true
}

// zenFreeCatalog 当前全部免费 zen 模型(自动路由页勾选用), 按 ID 排序。
func zenFreeCatalog() []ZenModel {
	initZenModels()
	zenModelsMu.RLock()
	out := make([]ZenModel, 0, len(zenModels))
	for _, m := range zenModels {
		if isZenFreeModel(m) && !zenModelUnavailable(m.ID) {
			out = append(out, *m)
		}
	}
	zenModelsMu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// zenAllCatalog 目录里的**全部** zen 模型, 不做免费/健康过滤, 按 ID 排序。
//
// 面板要展示"拉到了哪些模型、哪些还没启用": opencode 会不定期放进免费但
// 未标注 -free 的测试模型(如 union-alpha), zenFreeCatalog 把它们滤掉,
// 用户在面板上就看不到、也无从启用。这里给全量, 由前端按 on/free 渲染开关。
func zenAllCatalog() []ZenModel {
	initZenModels()
	zenModelsMu.RLock()
	out := make([]ZenModel, 0, len(zenModels))
	for _, m := range zenModels {
		out = append(out, *m)
	}
	zenModelsMu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// stripDisplayPrefix 去掉模型列表展示用前缀 "cline/", 仅当剩余部分
// 确实在 cline 模型表中才剥离, 避免误伤原生 vendor/model 名称
func stripDisplayPrefix(model string) string {
	rest, ok := strings.CutPrefix(strings.TrimSpace(model), "cline/")
	if !ok || rest == "" {
		return model
	}
	initModelsCache()
	modelsMu.Lock()
	_, inCline := modelsCache[rest]
	modelsMu.Unlock()
	if inCline {
		return rest
	}
	return model
}

// routeModel 决定请求走哪个上游: "zen" / "cline" / "reject"
// zen 免费模型 -> zen; zen 付费模型 -> reject(400); 其他 -> cline
// 显式 "zen/" 前缀只接受免费 zen 模型,付费或未知一律 reject,不会误入 cline 池
// 故障转移: zen 连续失败期间,zen 免费模型请求临时路由到 cline 账号池
func routeModel(id string) string {
	id = strings.TrimSpace(id)
	initZenModels()
	cfg := getZenConfig()
	if strings.HasPrefix(id, "zen/") {
		zm, ok := resolveZenModel(id)
		if !ok || !isZenFreeModel(zm) {
			return "reject"
		}
		if cfg.Failover && zenFailedNow() && clinePoolReady() {
			log.Printf("  failover: zen degraded, %q routed to cline pool", id)
			return "cline"
		}
		return "zen"
	}
	if zm, ok := resolveZenModel(id); ok {
		if isZenFreeModel(zm) {
			// 与 cline 模型表冲突时(几乎不可能)走 cline
			initModelsCache()
			modelsMu.Lock()
			_, inCline := modelsCache[id]
			modelsMu.Unlock()
			if !inCline {
				if cfg.Failover && zenFailedNow() && clinePoolReady() {
					log.Printf("  failover: zen degraded, %q routed to cline pool", id)
					return "cline"
				}
				return "zen"
			}
		} else {
			return "reject"
		}
	}
	if strings.HasPrefix(id, "opencode/") {
		short := strings.TrimPrefix(id, "opencode/")
		if zm, ok := resolveZenModel(short); ok {
			if isZenFreeModel(zm) {
				if cfg.Failover && zenFailedNow() && clinePoolReady() {
					log.Printf("  failover: zen degraded, %q routed to cline pool", id)
					return "cline"
				}
				return "zen"
			}
			return "reject"
		}
	}
	return "cline"
}

// clinePoolReady cline 账号池是否存在可用账号(active 或冷却已到期)。
// zen 熔断降级前用它判断可行性: 池内无可用账号时降级只会立即失败,
// 此时保持 zen 路由继续尝试上游是更优选择。
func clinePoolReady() bool {
	snap := poolSnapshot()
	now := time.Now()
	for _, a := range snap.Accounts {
		if a.Status == "active" {
			return true
		}
		if a.Status == "cooldown" && !a.CooldownUntil.IsZero() && now.After(a.CooldownUntil) {
			return true
		}
	}
	return false
}

// ============ zen 配置 ============

type zenCompactConfig struct {
	Auto         bool   `json:"auto"`         // 官方风格摘要压缩开关
	Buffer       int    `json:"buffer"`       // 预留输出缓冲 token,默认 20000
	KeepTokens   int    `json:"keepTokens"`   // 尾部保留 token 预算,默认 8000
	SummaryModel string `json:"summaryModel"` // 摘要模型,空=用请求模型
	MaxSummary   int    `json:"maxSummary"`   // 摘要最大输出 token,默认 4096
}

type zenConfigData struct {
	// SchemaVersion 配置结构版本, 由 migrateZenConfig 链式升级(见 config_migrate.go)。
	SchemaVersion int    `json:"schemaVersion,omitempty"`
	Enabled       bool   `json:"enabled"`
	Key           string `json:"key"`
	// Keys 额外的 zen API Key(多 key)。
	//
	// 动机: 一个 key 从上千个 IP 打是明显的代理特征; 把出口按 key 分成若干组、
	// 每组固定用一把 key, 更接近正常用户的网络特征。同时提供冗余 —— 一把 key
	// 收到 401 时其他 key 照常工作。
	//
	// ★ 收益边界: 隐蔽性收益取决于 key 数量(2-3 把 key 对上上千个出口, 每把仍
	//   会从数百个 IP 打, 改善有限); **冗余性是确定的收益**。详见 zen_keys.go。
	//
	// 配对是确定性的(按出口标识哈希), 不需要额外配置出口分组。
	Keys            []string                  `json:"keys,omitempty"`
	BaseURL         string                    `json:"baseURL"`                  // 主端点(兼容旧配置字段)
	BaseURLs        []string                  `json:"baseURLs"`                 // 全部端点: 主端点 + CDN 镜像, 重试时轮换
	Proxies         []string                  `json:"proxies"`                  // http(s)/socks5 代理与节点链接,轮询出口
	Subs            []string                  `json:"subs,omitempty"`           // 订阅链接, 定期抓取展开为节点并入池
	ProxyStrategy   string                    `json:"proxyStrategy"`            // round_robin / random / fill
	ExitMode        string                    `json:"exitMode"`                 // direct / proxy: 全网关统一出口
	EnabledRegions  []string                  `json:"enabledRegions,omitempty"` // 只使用这些地区的出口(空=全部); 见 exit_region.go
	DNSMode         string                    `json:"dnsMode,omitempty"`        // system / doh-ali / doh-cf / custom
	DNSCustomDNS    string                    `json:"dnsCustom,omitempty"`      // DNSMode=custom 时的 DoH 地址
	RescueDirect    *bool                     `json:"rescueDirect,omitempty"`   // 节点全不可用时允许直连兜底; 缺省(含旧配置)视为 true
	SubsRefreshMins int                       `json:"subsRefreshMinutes"`       // 订阅刷新间隔(分钟), 默认 30
	MaxConcurrency  int                       `json:"maxConcurrency"`           // zen 上游最大并发,防 worker 瞬时超限,默认 8
	Retries         int                       `json:"retries"`                  // 限流/网络错误重试次数,默认 3
	Failover        bool                      `json:"failover"`                 // zen 连续失败后故障转移到 cline 账号池,默认 true
	FailoverCount   int                       `json:"failoverCount"`            // 触发故障转移的连续失败次数,默认 3
	FailoverMinutes int                       `json:"failoverMinutes"`          // 故障转移窗口(分钟),默认 5
	Compaction      zenCompactConfig          `json:"compaction"`
	Providers       map[string]providerConfig `json:"providers,omitempty"`  // 通用 OpenAI 兼容上游
	Routes          map[string][]string       `json:"routes,omitempty"`     // 路由别名 -> 有序候选链(如 free-best)
	CooldownMs      map[string]int64          `json:"cooldownMs,omitempty"` // 候选层冷却时长覆盖(按错误类别)
	// StreamHeartbeatSecs 流式保活间隔(秒): 上游静默超过该时长时向客户端注入
	// 空 delta 帧, 防止客户端把"上游排队/推理中"当成挂死。0 = 关闭。
	StreamHeartbeatSecs int `json:"streamHeartbeatSecs,omitempty"`
	// StickySessions 粘性会话(P2, Resin 思路): 开启后同一客户端来源 IP 在
	// TTL(30 分钟)内复用同一出口节点, 服务于"同 IP 连续请求"的上游场景。
	StickySessions bool `json:"stickySessions,omitempty"`
	// StreamIdleSecs 上游流空闲上限(秒): 流中途连续无字节超过该值
	// 即主动断开并收尾(0/缺省 = 90 秒), 防止客户端无限等待。
	StreamIdleSecs int `json:"streamIdleSecs,omitempty"`
	// NodeExcludeKeywords 节点名排除关键词(不区分大小写): 订阅节点显示名命中
	// 任一关键词即不进入出口池(典型: 官网/过期/剩余流量 等信息位节点)。
	NodeExcludeKeywords []string `json:"nodeExcludeKeywords,omitempty"`
	// EnabledModels 手动启用的 zen 模型 ID(不带 zen/ 前缀)。
	// opencode 会不定期放进**免费但没标 -free 后缀**的测试模型
	// (如 union-alpha): 它们能被目录同步拉下来, 但过不了 isZenFreeModel 的
	// "-free 后缀 / seed 白名单" 判定, 永远不会出现在列表里。用户在面板上
	// 勾选的模型写到这里, isZenFreeModel 即放行。
	EnabledModels []string        `json:"enabledModels,omitempty"`
	Usage         zenUsageConfig  `json:"usage"`  // 每日配额账本
	Router        zenRouterConfig `json:"router"` // 自动路由模型名与参与范围
}

// zenEndpointMirrors 官方源之外的 CDN 镜像端点(实测镜像透传官方完整路径,须带 /v1)。
var zenEndpointMirrors = []string{
	"https://opencode.ai.cmliussss.net/zen/v1",
	"https://opencode.fastly.cmliussss.net/zen/v1",
	"https://opencode.gcore.cmliussss.net/zen/v1",
}

// defaultZenBaseURLs 官方 + 全部镜像,官方在前。
func defaultZenBaseURLs() []string {
	return append([]string{zenAPIBase}, zenEndpointMirrors...)
}

// zenBaseURLList 返回去重后的端点列表: 主端点在前,镜像在后。
// 兼容三种来源: 旧配置只有 BaseURL、新配置 BaseURLs、以及空值默认。
func zenBaseURLList(cfg *zenConfigData) []string {
	seen := map[string]bool{}
	out := make([]string, 0, 1+len(zenEndpointMirrors))
	add := func(u string) {
		u = strings.TrimRight(strings.TrimSpace(u), "/")
		if u == "" || seen[u] {
			return
		}
		seen[u] = true
		out = append(out, u)
	}
	add(cfg.BaseURL)
	for _, u := range cfg.BaseURLs {
		add(u)
	}
	if len(out) == 0 {
		add(zenAPIBase)
		for _, u := range zenEndpointMirrors {
			add(u)
		}
	}
	return out
}

func defaultZenConfig() *zenConfigData {
	rescue := true
	return &zenConfigData{
		Enabled:         true,
		Key:             "public",
		BaseURL:         zenAPIBase,
		BaseURLs:        defaultZenBaseURLs(),
		ProxyStrategy:   "round_robin",
		ExitMode:        exitModeProxy,
		DNSMode:         defaultDNSMode,
		RescueDirect:    &rescue,
		SubsRefreshMins: defaultSubsRefreshMins,
		MaxConcurrency:  8,
		Retries:         3,
		Failover:        true,
		FailoverCount:   3,
		FailoverMinutes: 5,
		Compaction: zenCompactConfig{
			Auto:       true,
			Buffer:     20000,
			KeepTokens: 8000,
			MaxSummary: 4096,
		},
	}
}
