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

	"cline-go-proxy/internal/kit"
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
func isRegionError(body string) bool {
	return strings.Contains(body, "RegionError") ||
		strings.Contains(body, "not available in your country")
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
// 统一出口下地区探测已降级为 provider 可选开关, 这里直接返回保留引用不断裂。
func probeRegionModelsAsync() {
	return
	for _, m := range regionModelSnapshot() {
		go probeModelAllNodes(m)
	}
}

// probeModelAllNodes 对全部已就绪节点探测指定模型的地区可用性。
// 统一出口下已停用, 直接返回保留引用不断裂。
func probeModelAllNodes(modelID string) {
	return
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
	for _, k := range keys {
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
	return false, false
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
	sess, user, ua := kit.FreshZenIdentity()
	req.Header.Set("Authorization", "Bearer "+cfg.Key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", ua)
	req.Header.Set("x-opencode-session", sess)
	req.Header.Set("x-opencode-request", user)
	req.Header.Set("x-opencode-client", "cli")
	req.Header.Set("x-opencode-model", modelID)

	client := &http.Client{
		Timeout: 20 * time.Second,
		Transport: &http.Transport{
			Proxy:               http.ProxyURL(proxyURL),
			TLSHandshakeTimeout: 10 * time.Second,
		},
	}
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
	if status == 0 {
		return false, false
	}
	// 任何其他 HTTP 响应都证明"请求抵达了模型接口且地区被放行"
	return true, true
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
	// 上游可达性: 已探测出"该节点到该上游不通"时跳过它 —— 这正是
	// "节点全绿但某个 Provider 502"的成因, 不该继续把请求交给它。
	upstream := upstreamOfModel(modelID)
	supported := func(p string) bool {
		if upstream == "" {
			return true
		}
		return nodeSupportsUpstream(upstream, nodeLocalKey(p))
	}
	if !isRegionRestrictedModel(modelID) {
		return pickZenProxyWhere(supported)
	}
	list := effectiveProxyList()
	if len(list) == 0 {
		log.Printf("  zen: 地区受限模型 %s 选路失败: 出口池为空, 回退直连", modelID)
		return "", -1
	}
	var cooled, notDialable, notUsable, unsupported, regionBad int
	cand := make([]int, 0, len(list))
	for i, p := range list {
		switch {
		case !zenProxyAvailable(i):
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
		return pickZenProxyWhere(supported)
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
