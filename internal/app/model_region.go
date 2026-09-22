package app

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// 地区受限模型的节点能力标记。
//
// 部分 zen 模型(如 muse-spark-1.3-contributor-free)只对特定出口地区开放,
// 上游对其它地区直接回 403 RegionError。节点连通检测只验证"能否握手上游",
// 覆盖不到这一层, 于是出现"节点全绿但该模型全挂"。
//
// 这里为每个受限模型单独探测每个节点出口, 路由时只用通过校验的节点;
// 不受限的模型(mimo 等)不受影响, 仍使用全部已就绪节点。

var (
	regionModelMu sync.RWMutex
	// regionModels 已确认存在地区限制的模型 ID
	regionModels = map[string]bool{}
	// regionNodeOK modelID -> nodeKey -> 该出口地区是否被该模型接受
	regionNodeOK = map[string]map[string]bool{}
	// regionProbing 正在探测中的模型(防止重复触发把整池节点探两遍)
	regionProbeMu sync.Mutex
	regionProbing = map[string]bool{}
)

// regionRestrictedSeed 已知有地区限制的模型, 启动时即纳入能力探测。
var regionRestrictedSeed = []string{"muse-spark-1.3-contributor-free"}

// initRegionModels 注册种子模型(供 StartProxy 调用)。
func initRegionModels() {
	regionModelMu.Lock()
	for _, m := range regionRestrictedSeed {
		regionModels[m] = true
	}
	regionModelMu.Unlock()
}

// ctxKeyZenModel 请求上下文中的 zen 模型 ID, 拨号层据此选择匹配的出口。
type ctxKeyZenModelType struct{}

var ctxKeyZenModel = ctxKeyZenModelType{}

// isRegionError 判定上游响应是否为地区限制错误。
//
// ★ 单一来源: 直接复用 error_rules.go 的地区封锁规则(classGeoBlocked),
// 不再维护第二份措辞表。
//
// 历史上这里有一份与 error_rules.go **各自复制**的 6 条短语表, 注释还声称
// "同源(isRegionBlockedBody 也用同一份)" —— 但那个函数根本不存在, 两份表
// 也早已行为分叉: error_rules.go 那份带反误伤豁免(`error code: 1010` /
// `just a moment` / `attention required`), 这份没有。后果是 Cloudflare 1010
// 指纹拒绝的正文里只要出现 "region" 字样, 就会被当地区封锁处理 —— 于是
// zen_call 会把一个**只是当前出口指纹被拒**的模型登记成"地区受限"并触发
// 全节点探测, 属于误判(2026-09-18 复核)。
//
// 收口后两处判定必然一致: 措辞表与豁免都在 error_rules.go 一处维护。
func isRegionError(body string) bool {
	class, _ := matchErrorRules(body)
	return class == classGeoBlocked
}

// isFreeTierError 判定上游响应是否为 opencode 免费 tier 的出口风控拒绝。
//
// 原文: {"type":"error","error":{"type":"FreeTierError","message":"Error from
// provider (Console): OpenCode's free tier can only be used from within OpenCode"}}
//
// 实测(2026-09-17): 同一 mimo-v2.5-free 走香港/大陆中转节点 200、走美/法节点与
// 本机直连一律 403 —— "within OpenCode" 的判定在 Console 后端按**出口 IP** 做,
// 与请求头(UA/x-opencode-*)无关(带参考实现的完整 CLI 身份头同样被拒)。
// 因此这是**出口级**失败: 该冷却/换出口, 不是冷却模型。
func isFreeTierError(body string) bool {
	return strings.Contains(body, "FreeTierError") ||
		strings.Contains(body, "can only be used from within OpenCode")
}

// markModelRegionRestricted 记录某模型存在地区限制并触发节点能力探测。
// 上游首次返回 RegionError 时自动调用, 无需预先配置。
func markModelRegionRestricted(modelID string) {
	if modelID == "" {
		return
	}
	regionModelMu.Lock()
	first := !regionModels[modelID]
	regionModels[modelID] = true
	regionModelMu.Unlock()
	if first {
		log.Printf("  zen: model %s is region-restricted, probing exit nodes", modelID)
		go probeModelAllNodes(modelID)
	}
}

// isRegionRestrictedModel 该模型是否已确认有地区限制。
func isRegionRestrictedModel(modelID string) bool {
	if modelID == "" {
		return false
	}
	regionModelMu.RLock()
	defer regionModelMu.RUnlock()
	return regionModels[modelID]
}

// regionNodeUsable 该节点是否已确认可用于指定模型(known=false 表示尚未探测)。
func regionNodeUsable(modelID, nodeKey string) (usable bool, known bool) {
	regionModelMu.RLock()
	defer regionModelMu.RUnlock()
	m, ok := regionNodeOK[modelID]
	if !ok {
		return false, false
	}
	v, ok := m[nodeKey]
	if !ok {
		return false, false
	}
	return v, true
}

func setRegionNodeOK(modelID, nodeKey string, ok bool) {
	regionModelMu.Lock()
	if regionNodeOK[modelID] == nil {
		regionNodeOK[modelID] = map[string]bool{}
	}
	regionNodeOK[modelID][nodeKey] = ok
	regionModelMu.Unlock()
}

// regionModelSnapshot 返回当前已知的受限模型列表。
func regionModelSnapshot() []string {
	regionModelMu.RLock()
	defer regionModelMu.RUnlock()
	out := make([]string, 0, len(regionModels))
	for m := range regionModels {
		out = append(out, m)
	}
	return out
}

// probeRegionModelsAsync 对全部已知受限模型异步刷新节点能力(健康检测后调用)。
func probeRegionModelsAsync() {
	for _, m := range regionModelSnapshot() {
		go probeModelAllNodes(m)
	}
}

// probeModelAllNodes 对全部已就绪节点探测指定模型的地区可用性。
// regionProbeMaxNodes 单轮全节点探测的节点数上限。
//
// 探测发的是**真实请求**(每个烧该出口一份免费额度), 所以必须限量。
// 正向知识现在由真实请求回写(zen_call.go 成功分支), 探测只是补充。
const regionProbeMaxNodes = 24

func probeModelAllNodes(modelID string) {
	regionProbeMu.Lock()
	if regionProbing[modelID] {
		regionProbeMu.Unlock()
		return
	}
	regionProbing[modelID] = true
	regionProbeMu.Unlock()
	defer func() {
		regionProbeMu.Lock()
		delete(regionProbing, modelID)
		regionProbeMu.Unlock()
	}()

	nodeMu.Lock()
	keys := make([]string, 0, len(nodePorts))
	for k := range nodePorts {
		keys = append(keys, k)
	}
	nodeMu.Unlock()
	if len(keys) == 0 {
		return
	}
	var okCount int32
	probed := int32(0)
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	// ★ 2026-09-17 审查 P1-1: 单轮探测的节点数上限。
	//
	// 探测要发**真实请求**(每个都烧该出口的一份免费额度), 对上百个节点全量扫
	// 代价过高 —— 这段代码自己的注释就记录过一次事故: 8 并发探测"自己撞出上游
	// 限流, 144 个节点全被误标为不可用"。
	//
	// 现在正向知识由**真实请求回写**(见 zen_call.go 成功分支的 setRegionNodeOK(true)),
	// 探测只是补充手段, 所以可以放心限流。
	//
	// 覆盖性靠 Go 的 map 迭代随机化: `for k := range nodePorts` 每轮顺序不同,
	// 限流后自然轮换, 不会永远只探同一批。
	limit := len(keys)
	if limit > regionProbeMaxNodes {
		limit = regionProbeMaxNodes
	}
	for _, k := range keys[:limit] {
		wg.Add(1)
		go func(key string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			regionOK, known := probeNodeModel(key, modelID)
			if !known {
				return // 拨号失败只是节点抖动, 不改写它的地区能力记录
			}
			setRegionNodeOK(modelID, key, regionOK)
			atomic.AddInt32(&probed, 1)
			if regionOK {
				atomic.AddInt32(&okCount, 1)
			}
		}(k)
	}
	wg.Wait()
	log.Printf("  zen: model %s region probe done, %d/%d 确认可用 (%d/%d 拿到结果)",
		modelID, atomic.LoadInt32(&okCount), len(keys), atomic.LoadInt32(&probed), len(keys))
}

// probeNodeModel 经单个节点出口向上游发最小请求, 判断该出口的地区是否被该模型接受。
//
// 判据是"有没有被地区拒绝", 而不是"是否 200": 429 限流 / 402 额度 / 5xx 都
// 说明请求已经抵达模型接口且地区被放行。此前只认 200, 8 并发探测自己撞出
// 上游限流, 144 个节点全被误标为不可用 —— 选路无候选后退回直连,
// 大陆 IP 对这类模型必然 403, 这正是"走了节点还报 RegionError"的真相。
//
// 返回 (regionOK, known): known=false 表示连结果都没拿到(拨号失败),
// 节点可能只是抖动, 此时不应更新它的地区能力记录。
func probeNodeModel(key, modelID string) (regionOK, known bool) {
	local := nodeLocalAddr(key)
	if local == "" {
		return false, false
	}
	proxyURL, err := url.Parse(local)
	if err != nil {
		return false, false
	}
	payload, err := json.Marshal(map[string]any{
		"model":      modelID,
		"messages":   []any{map[string]any{"role": "user", "content": "hi"}},
		"max_tokens": 1,
		"stream":     false,
	})
	if err != nil {
		return false, false
	}
	req, err := http.NewRequest("POST", zenAPIBase+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return false, false
	}
	cfg := getZenConfig()
	outbound := map[string]string{}
	applyOpencodeHeaders(outbound, nil, defaultOpencodeIdentity(), nil)
	req.Header.Set("Authorization", "Bearer "+cfg.Key)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range outbound {
		req.Header.Set(k, v)
	}
	req.Header.Set("x-opencode-model", modelID)

	client := &http.Client{
		Timeout: 20 * time.Second,
		Transport: &http.Transport{
			Proxy:               http.ProxyURL(proxyURL),
			TLSHandshakeTimeout: 10 * time.Second,
		},
	}
	// 探测是一次性的: 每轮(30 分钟)多模型×出口并发都新建 Client+Transport,
	// 不显式关掉空闲连接会攒下一批无人回收的 TCP/TLS 连接(对照
	// rebuildZenTransport 的 old.CloseIdleConnections)。defer 放在 client.Do
	// 之前, 出错早退路径(client.Do 返回 err)同样会被执行到。
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	if err != nil {
		return false, false
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return classifyRegionProbe(resp.StatusCode, string(body))
}

// classifyRegionProbe 按响应判定出口的地区能力(独立出来便于单测)。
func classifyRegionProbe(status int, body string) (regionOK, known bool) {
	if status == http.StatusForbidden && isRegionError(body) {
		return false, true // 明确的地区拒绝
	}
	if status == http.StatusForbidden && isFreeTierError(body) {
		return false, true // 明确的免费 tier 出口拒绝(与 RegionError 同构, 都是出口级失败)
	}
	if status == 0 {
		return false, false
	}
	// 任何其他 HTTP 响应都证明"请求抵达了模型接口且地区被放行"
	return true, true
}

// exitUpstreamFilter 只按"该出口到该上游是否可达"过滤。
//
// 单独抽出来是因为 pickZenProxyForModel 的**兜底**要用它而不是完整过滤:
// 当所有出口都被探测出"地区被拒"时, 退回只按可达性过滤的常规轮询,
// 而不是直连 —— 对大陆禁售模型, 直连是 100% 失败, 任何一个节点都比它强。
func exitUpstreamFilter(modelID string) func(string) bool {
	upstream := upstreamOfModel(modelID)
	return func(p string) bool {
		if upstream == "" {
			return true
		}
		return nodeSupportsUpstream(upstream, nodeLocalKey(p))
	}
}

// exitFilterForModel 模型相关的**完整**出口过滤谓词(上游可达性 + 地区能力)。
//
// ★ 2026-09-17 审查 P1-1: 此前这套判据只存在于 pickZenProxyForModel 里, 而那个
// 函数**生产零调用**(只有测试调); 生产选路走 pickUnifiedExit →
// pickZenProxyWhere(func(q string) bool { return true }) —— **空操作过滤器**。
//
// 后果: 上游可达性探测(nodeSupportsUpstream)与地区探测(regionNodeUsable)的结果
// 都不参与选路, 而探测本身要发**真实请求、烧免费额度**。一份真实成本 + 一个不
// 存在的能力 —— 这也是"探测子系统整块空转"的根因。
//
// 抽成谓词是为了让生产路径与展示路径共用同一份判据, 不再各写一套。
//
// 注意方向: 只有"已探测确认被拒"才排除, **没有探测数据照样参与** —— 未探测
// 不等于不可用, 否则刚启动/探测未完成时必然无候选。
func exitFilterForModel(modelID string) func(string) bool {
	upstreamOK := exitUpstreamFilter(modelID)
	regionRestricted := isRegionRestrictedModel(modelID)
	return func(p string) bool {
		if !upstreamOK(p) {
			return false
		}
		if regionRestricted {
			if ok, known := regionNodeUsable(modelID, nodeLocalKey(p)); known && !ok {
				return false
			}
		}
		return true
	}
}

// pickZenProxyForModel 选择出口: 地区受限模型优先从"未被探测出地区拒绝"的
// 节点中轮询, 其余模型沿用常规轮询。
//
// 注意方向: 只有"已探测确认被拒"才排除, 没有探测数据照样参与 —— 未探测
// 不等于不可用, 否则刚启动/探测未完成时必然无候选。全部节点都确认被拒时
// 退回常规轮询而不是直连: 对大陆禁售模型, 直连是 100% 失败,
// 任何一个节点都比它强。
func pickZenProxyForModel(modelID string) (string, int) {
	if exitModeDirectNow() {
		return "", -1
	}
	// 完整判据(上游可达性 + 地区能力): 与生产选路共用同一份, 见 exitFilterForModel。
	supported := exitFilterForModel(modelID)
	if !isRegionRestrictedModel(modelID) {
		return pickZenProxyWhere(supported)
	}
	// 兜底用的**上游可达性**过滤(不含地区过滤) —— 见 exitUpstreamFilter 的说明。
	fallbackFilter := exitUpstreamFilter(modelID)
	list := effectiveProxyList()
	if len(list) == 0 {
		log.Printf("  zen: 地区受限模型 %s 选路失败: 出口池为空, 回退直连", modelID)
		return "", -1
	}
	var cooled, notDialable, notUsable, unsupported, regionBad int
	cand := make([]int, 0, len(list))
	for i, p := range list {
		switch {
		case !zenProxyAvailable(p):
			cooled++
			continue
		case !nodeDialable(p):
			notDialable++
			continue
		case !nodeUsable(p):
			notUsable++
			continue
		case !supported(p):
			unsupported++
			continue
		}
		if ok, known := regionNodeUsable(modelID, nodeLocalKey(p)); known && !ok {
			regionBad++ // 已探测确认该出口地区被该模型拒绝
			continue
		}
		cand = append(cand, i)
	}
	if len(cand) == 0 {
		log.Printf("  zen: 地区受限模型 %s 无候选(池 %d: 冷却 %d 未就绪 %d 不健康 %d 上游不通 %d 地区被拒 %d), 回退常规轮询",
			modelID, len(list), cooled, notDialable, notUsable, unsupported, regionBad)
		// 兜底用**上游可达性**过滤而不是完整过滤: 地区被拒的节点仍要参与,
		// 否则全部被拒时会退回直连 —— 对禁售模型那是 100% 失败。
		return pickZenProxyWhere(fallbackFilter)
	}
	idx := cand[int(zenProxyCount.Add(1)-1)%len(cand)]
	return list[idx], idx
}

// regionNodeSupport 该节点支持的地区受限模型列表(供管理端展示)。
func regionNodeSupport(nodeKey string) []string {
	regionModelMu.RLock()
	defer regionModelMu.RUnlock()
	out := []string{}
	for modelID, m := range regionNodeOK {
		if m[nodeKey] {
			out = append(out, modelID)
		}
	}
	return out
}

// regionProbeStatus 探测进度概览(供管理端展示)。
func regionProbeStatus() map[string]any {
	regionModelMu.RLock()
	defer regionModelMu.RUnlock()
	out := map[string]any{}
	for modelID, m := range regionNodeOK {
		usable := 0
		for _, ok := range m {
			if ok {
				usable++
			}
		}
		out[modelID] = map[string]any{"probed": len(m), "usable": usable}
	}
	return out
}

// zenModelIDOf 从请求参数中解析 zen 模型 ID。
func zenModelIDOf(params map[string]any) string {
	if params == nil {
		return ""
	}
	model, _ := params["model"].(string)
	if m, ok := resolveZenModel(model); ok {
		return m.ID
	}
	return ""
}
