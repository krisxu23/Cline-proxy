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
func probeRegionModelsAsync() {
	for _, m := range regionModelSnapshot() {
		go probeModelAllNodes(m)
	}
}

// probeModelAllNodes 对全部已就绪节点探测指定模型的地区可用性。
func probeModelAllNodes(modelID string) {
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
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for _, k := range keys {
		wg.Add(1)
		go func(key string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			ok := probeNodeModel(key, modelID)
			setRegionNodeOK(modelID, key, ok)
			if ok {
				atomic.AddInt32(&okCount, 1)
			}
		}(k)
	}
	wg.Wait()
	log.Printf("  zen: model %s region probe done, %d/%d nodes usable",
		modelID, atomic.LoadInt32(&okCount), len(keys))
}

// probeNodeModel 经单个节点出口向上游发最小请求, 判断该出口地区是否被该模型接受。
// 仅 200 视为可用; 其余(含 403 RegionError)一律记为不可用, 保持保守。
func probeNodeModel(key, modelID string) bool {
	local := nodeLocalAddr(key)
	if local == "" {
		return false
	}
	proxyURL, err := url.Parse(local)
	if err != nil {
		return false
	}
	payload, err := json.Marshal(map[string]any{
		"model":      modelID,
		"messages":   []any{map[string]any{"role": "user", "content": "hi"}},
		"max_tokens": 1,
		"stream":     false,
	})
	if err != nil {
		return false
	}
	req, err := http.NewRequest("POST", zenAPIBase+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return false
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
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return true
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return false
}

// pickZenProxyForModel 选择出口: 地区受限模型只从通过该模型校验的节点中轮询,
// 其余模型沿用常规轮询。无合规节点时返回直连("", -1), 由上游给出真实原因。
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
		return "", -1
	}
	cand := make([]int, 0, len(list))
	for i, p := range list {
		if !zenProxyAvailable(i) || !nodeDialable(p) || !nodeUsable(p) || !supported(p) {
			continue
		}
		if ok, known := regionNodeUsable(modelID, nodeLocalKey(p)); known && ok {
			cand = append(cand, i)
		}
	}
	if len(cand) == 0 {
		return "", -1
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
