package app

// 出口地区归类与地区过滤。
//
// 需求: 设置页不再逐个罗列节点, 而是按「美国 / 日本 / 台湾 / 香港 / 新加坡 /
// 欧洲 / 其他地区」7 组聚合; 用户勾选地区后, 整个网关(zen / cline 池 /
// 通用 Provider / 订阅抓取)的出站只走所选地区的出口 IP; 支持多选。
//
// 地区来源: 连通检测实测的出口国家(exitCountry, 由 IP 回显服务判定)。
// 未检测或无法判定国家的出口一律归入「其他地区」—— 勾选具体地区时它们不会
// 被使用, 这是有意的保守策略(宁可不用, 也不违反用户"只走美国"的意图)。
// 分组思路与 krisxu23/freesub 的地区分类一致(按国家码归类)。

import (
	"encoding/json"
	"log"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"free-router/internal/kit"
)

// exitRegionDef 地区定义
type exitRegionDef struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// exitRegionDefs 固定 7 组, 顺序即界面顺序
var exitRegionDefs = []exitRegionDef{
	{"us", "美国"},
	{"jp", "日本"},
	{"tw", "台湾"},
	{"hk", "香港"},
	{"sg", "新加坡"},
	{"eu", "欧洲"},
	{"other", "其他地区"},
}

// euCountryCodes 归入「欧洲」的国家码(覆盖常见机房/VPN 地区)
var euCountryCodes = map[string]bool{
	"FR": true, "DE": true, "GB": true, "NL": true, "IE": true, "ES": true,
	"IT": true, "SE": true, "CH": true, "AT": true, "BE": true, "DK": true,
	"NO": true, "FI": true, "PL": true, "CZ": true, "RO": true, "HU": true,
	"PT": true, "UA": true, "LT": true, "LV": true, "EE": true, "RS": true,
	"BG": true, "HR": true, "SK": true, "SI": true, "GR": true, "IS": true,
	"LU": true, "MD": true, "AL": true, "BA": true, "MK": true, "ME": true,
	"CY": true, "MT": true,
}

// regionOfCountry 把国家码映射到地区 ID。
// 香港/澳门合并为「香港」, 台湾单列, 其余不在名单内的归「其他地区」。
func regionOfCountry(cc string) string {
	switch strings.ToUpper(strings.TrimSpace(cc)) {
	case "US":
		return "us"
	case "JP":
		return "jp"
	case "TW":
		return "tw"
	case "HK", "MO":
		return "hk"
	case "SG":
		return "sg"
	}
	if euCountryCodes[strings.ToUpper(strings.TrimSpace(cc))] {
		return "eu"
	}
	return "other"
}

func validExitRegion(id string) bool {
	for _, d := range exitRegionDefs {
		if d.ID == id {
			return true
		}
	}
	return false
}

func exitRegionLabel(id string) string {
	for _, d := range exitRegionDefs {
		if d.ID == id {
			return d.Label
		}
	}
	return id
}

// normalizeExitRegions 去重 + 只保留合法 ID, 保持界面顺序
func normalizeExitRegions(in []string) []string {
	seen := map[string]bool{}
	for _, id := range in {
		id = strings.ToLower(strings.TrimSpace(id))
		if validExitRegion(id) {
			seen[id] = true
		}
	}
	out := make([]string, 0, len(seen))
	for _, d := range exitRegionDefs {
		if seen[d.ID] {
			out = append(out, d.ID)
		}
	}
	return out
}

// ============ 出口国家持久化 ============
//
// 实测出口国家只存在于内存的连通检测结果里, 重启就没了。而地区过滤完全依赖它:
// 重启后所有节点都会变成「其他地区」, 用户若只勾选了"美国", 出口池会直接变空。
// 因此把 (节点 -> 国家码) 单独落盘: 启动即可用, 新一轮检测后覆盖更新。

var nodeCountryMu sync.RWMutex
var nodeCountryMap = map[string]string{}
var nodeCountryLoaded bool
var nodeCountryDirty bool

func nodeCountryFile() string {
	if nodeCountryFileOverride != "" {
		return nodeCountryFileOverride
	}
	return kit.ResolveDataPath("node-regions.json")
}

// 测试注入点: 把国家映射文件重定向到临时路径
var nodeCountryFileOverride string

func setNodeCountryFileForTest(path string) { nodeCountryFileOverride = path }

func loadNodeCountriesLocked() {
	if nodeCountryLoaded {
		return
	}
	nodeCountryLoaded = true
	raw, err := os.ReadFile(nodeCountryFile())
	if err != nil || len(raw) == 0 {
		return
	}
	var doc struct {
		Countries map[string]string `json:"countries"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return
	}
	for k, v := range doc.Countries {
		nodeCountryMap[k] = v
	}
}

func persistNodeCountries() {
	nodeCountryMu.Lock()
	if !nodeCountryDirty {
		nodeCountryMu.Unlock()
		return
	}
	nodeCountryDirty = false
	snapshot := make(map[string]string, len(nodeCountryMap))
	for k, v := range nodeCountryMap {
		snapshot[k] = v
	}
	nodeCountryMu.Unlock()

	doc := map[string]any{"syncedAt": time.Now().Unix(), "countries": snapshot}
	raw, err := json.Marshal(doc)
	if err != nil {
		return
	}
	if err := kit.WriteFileAtomicDefault(nodeCountryFile(), raw); err != nil {
		log.Printf("  nodes: 出口国家落盘失败: %v", err)
	}
}

// rememberNodeCountry 记下某个出口实测到的国家码(空值忽略)。
// 写内存很快; 落盘由 throttledNodeCountryPersist 合并成低频写。
func rememberNodeCountry(key, country string) {
	country = strings.ToUpper(strings.TrimSpace(country))
	if key == "" || country == "" {
		return
	}
	nodeCountryMu.Lock()
	loadNodeCountriesLocked()
	if nodeCountryMap[key] == country {
		nodeCountryMu.Unlock()
		return
	}
	nodeCountryMap[key] = country
	nodeCountryDirty = true
	nodeCountryMu.Unlock()
}

func rememberedNodeCountry(key string) string {
	nodeCountryMu.Lock()
	defer nodeCountryMu.Unlock()
	loadNodeCountriesLocked()
	return nodeCountryMap[key]
}

// nodeExitRegion 出口所属地区: 优先最近一次连通检测的实测结果, 其次是落盘的
// 历史实测值(重启后仍可用); 都没有则归「其他地区」(手填代理也走这里)。
func nodeExitRegion(key string) string {
	// 健康/国家一律按 nodeLocalKey(去 # 名称)存储, 而过滤路径传进来的是
	// effectiveProxyList 的原始行(可能带 #名称)—— 入口统一规范化, 否则手动
	// 添加的带名节点恒被归「其他地区」, 随后被地区过滤静默排除(审查 P1;
	// exitRegionSummary 的同类问题此前已修, 这里补过滤路径)。nodeLocalKey
	// 只截断 # 后缀, 对已规范化的键幂等。
	key = nodeLocalKey(key)
	if r, ok := healthResultOf(key); ok && strings.TrimSpace(r.ExitCountry) != "" {
		return regionOfCountry(r.ExitCountry)
	}
	if cc := rememberedNodeCountry(key); cc != "" {
		return regionOfCountry(cc)
	}
	return "other"
}

// enabledExitRegions 已勾选的地区(空 = 不限制, 全部出口可用)
func enabledExitRegions() []string {
	cfg := getZenConfig()
	if cfg == nil {
		return nil
	}
	return cfg.EnabledRegions
}

func exitRegionFilterActive() bool {
	return len(enabledExitRegions()) > 0
}

// exitRegionAllowed 该出口是否在勾选地区内。
// 未启用过滤时恒为 true; 启用后按实测国家判定。
func exitRegionAllowed(key string) bool {
	regions := enabledExitRegions()
	if len(regions) == 0 {
		return true
	}
	want := nodeExitRegion(key)
	for _, r := range regions {
		if r == want {
			return true
		}
	}
	return false
}

// ============ 出口列表缓存 ============
//
// effectiveProxyList 在拨号路径上被频繁调用, 池子上千节点时逐个查地区会
// 累积成可观开销。这里缓存过滤结果: 配置变更(勾选地区/订阅/代理)时显式失效,
// 另外给 2 秒 TTL 兜住"订阅刷新带来新节点"这类外部变化。

var exitListCache struct {
	mu      sync.Mutex
	regKey  string
	listLen int
	at      time.Time
	out     []string
}

const exitListCacheTTL = 2 * time.Second

func invalidateExitListCache() {
	exitListCache.mu.Lock()
	exitListCache.at = time.Time{}
	exitListCache.out = nil
	exitListCache.mu.Unlock()
}

// filterByExitRegion 只保留勾选地区的出口(未启用过滤时原样返回)。
// 兜底: 过滤后一个出口都不剩时退回完整列表并告警 —— 宁可暂时不遵守地区限制,
// 也不能让全网关没有出口(典型场景: 刚重启还没跑连通检测/勾选的地区暂无节点)。
func filterByExitRegion(list []string) []string {
	regions := enabledExitRegions()
	if len(regions) == 0 {
		return list
	}
	regKey := strings.Join(regions, ",")
	exitListCache.mu.Lock()
	if exitListCache.regKey == regKey && exitListCache.listLen == len(list) &&
		time.Since(exitListCache.at) < exitListCacheTTL && exitListCache.out != nil {
		out := exitListCache.out
		exitListCache.mu.Unlock()
		return append([]string(nil), out...)
	}
	exitListCache.mu.Unlock()

	out := make([]string, 0, len(list))
	for _, item := range list {
		if exitRegionAllowed(item) {
			out = append(out, item)
		}
	}
	if len(out) == 0 && len(list) > 0 {
		warnRegionFilterEmpty(regKey, len(list))
		out = append(out, list...)
	} else if len(out) > 0 {
		// 过滤恢复正常(有匹配项): 清掉告警记忆, 下次再变空还能再警 —— 否则同一
		// 地区组合告警一次后, 之后每次回退都静默(审查 P2 的 warn-once 问题)。
		regionFilterWarnMu.Lock()
		regionFilterWarnedFor = ""
		regionFilterWarnMu.Unlock()
	}

	exitListCache.mu.Lock()
	exitListCache.regKey = regKey
	exitListCache.listLen = len(list)
	exitListCache.at = time.Now()
	exitListCache.out = append([]string(nil), out...)
	exitListCache.mu.Unlock()
	return out
}

// warnRegionFilterEmpty 同一情形连续只告警一次, 避免每个请求刷屏;
// 过滤恢复正常后由 filterByExitRegion 清掉记忆, 再次变空可再警。
var regionFilterWarnMu sync.Mutex
var regionFilterWarnedFor string

func warnRegionFilterEmpty(regKey string, total int) {
	regionFilterWarnMu.Lock()
	defer regionFilterWarnMu.Unlock()
	if regionFilterWarnedFor == regKey {
		return
	}
	regionFilterWarnedFor = regKey
	log.Printf("  exit-region: 勾选地区(%s)在当前 %d 个出口里没有匹配项, 已临时回退全部出口; "+
		"请运行「连通检测」获取出口国家, 或调整所选地区", regKey, total)
}

// exitRegionSummary 每个地区的出口统计, 供设置页展示(数量 + 可用数)。
func exitRegionSummary() []map[string]any {
	list := make([]string, 0, len(getZenConfig().Proxies)+len(subNodeKeysSnapshot()))
	cfg := getZenConfig()
	if cfg != nil {
		list = append(list, cfg.Proxies...)
	}
	list = append(list, subNodeKeysSnapshot()...)

	type stat struct{ total, ok int }
	stats := map[string]*stat{}
	for _, d := range exitRegionDefs {
		stats[d.ID] = &stat{}
	}
	for _, item := range list {
		// 健康/地区数据都按 nodeLocalKey(去 # 名称的规范化键)存储, 原始行
		// (如 socks5://1.2.3.4:1080#我的节点)永远查不到 —— 手动代理会全落进
		// other、ok 计数恒 0(P3-4)。入列前统一规范化; nodeExitRegion/healthOf
		// 的匹配逻辑本就按规范化键工作, 对它同样成立。
		item = nodeLocalKey(item)
		st := stats[nodeExitRegion(item)]
		if st == nil {
			continue
		}
		st.total++
		if healthOf(item) == "ok" {
			st.ok++
		}
	}
	out := make([]map[string]any, 0, len(exitRegionDefs))
	for _, d := range exitRegionDefs {
		s := stats[d.ID]
		out = append(out, map[string]any{
			"id":    d.ID,
			"label": d.Label,
			"total": s.total,
			"ok":    s.ok,
		})
	}
	return out
}

// exitRegionIDs 全部合法地区 ID(供测试与校验使用)
func exitRegionIDs() []string {
	out := make([]string, 0, len(exitRegionDefs))
	for _, d := range exitRegionDefs {
		out = append(out, d.ID)
	}
	sort.Strings(out)
	return out
}
