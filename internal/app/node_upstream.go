package app

import (
	"context"
	"crypto/tls"
	"log"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"cline-go-proxy/internal/cline"
)

// 节点 × 上游 可达性矩阵。
//
// 起因: 原先的连通检测只验证"节点能不能到 zen / cline", 于是反复出现
// "节点列表全绿、某个 Provider 却 502"的困惑 —— 不同上游对出口地区与线路的
// 要求并不一样(例如某些供应商直接封大陆或封数据中心 IP)。
//
// 这里把探测目标扩展到**每一个已配置上游**(zen / cline / 每个通用 Provider),
// 用"TCP + TLS 握手"这种不计费、不消耗额度的方式逐节点验证, 结果既用于
// 节点选择(跳过到该上游不通的节点), 也在面板上标注出来。

const (
	nodeUpstreamProbeInterval = 10 * time.Minute
	nodeUpstreamProbeWorkers  = 8
	nodeUpstreamProbeTimeout  = 12 * time.Second
)

var (
	nodeUpstreamMu      sync.RWMutex
	nodeUpstreamOK      = map[string]map[string]bool{} // upstream -> nodeKey -> 是否可达
	nodeUpstreamLastAt  time.Time
	nodeUpstreamProbing bool
)

// upstreamTargets 需要探测的上游及其 host。
func upstreamTargets() map[string]string {
	out := map[string]string{}
	if raw := zenBaseURLList(getZenConfig()); len(raw) > 0 {
		if u, err := url.Parse(raw[0]); err == nil && u.Hostname() != "" {
			out[upstreamZen] = u.Hostname()
		}
	}
	if u, err := url.Parse(cline.ClineAPIBase); err == nil && u.Hostname() != "" {
		out[upstreamCline] = u.Hostname()
	}
	for _, name := range providerNames() {
		pc, ok := providerConfigFor(name)
		if !ok || pc.APIKey == "" {
			continue
		}
		if h := providerUpstreamHost(pc); h != "" {
			out[name] = h
		}
	}
	return out
}

// providerUpstreamHost 从 Provider 的 Base URL 取主机名。
func providerUpstreamHost(pc providerConfig) string {
	u, err := url.Parse(strings.TrimSpace(pc.BaseURL))
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// upstreamOfModel 从模型标识推断它属于哪个上游:
// "bai:glm-5.3-flash" -> bai; "zen:mimo-v2.5-free" -> zen; "cline:*" -> cline。
// 推断不出时返回空串, 表示不按上游过滤。
func upstreamOfModel(modelID string) string {
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return ""
	}
	up, _, ok := strings.Cut(modelID, ":")
	if !ok {
		return ""
	}
	up = strings.TrimSpace(up)
	switch up {
	case upstreamZen:
		return upstreamZen
	case upstreamCline:
		return upstreamCline
	}
	if _, ok := providerConfigFor(up); ok {
		return up
	}
	return ""
}

// nodeSupportsUpstream 该节点能否到达该上游。没有探测数据时返回 true ——
// 不能用"未探测"当作"不可用", 否则刚启动时会无节点可选。
func nodeSupportsUpstream(upstream, nodeKey string) bool {
	if upstream == "" {
		return true
	}
	nodeUpstreamMu.RLock()
	defer nodeUpstreamMu.RUnlock()
	m, ok := nodeUpstreamOK[upstream]
	if !ok {
		return true
	}
	v, known := m[nodeKey]
	if !known {
		return true
	}
	return v
}

// probeNodeUpstream 经指定出口与上游做一次 TCP + TLS 握手。
// 用握手而不是真实请求: 不消耗额度, 也不需要各家的鉴权格式一致。
// 入口用 dialViaProxy, 因此普通 http/socks5 代理与 sing-box 节点都覆盖。
func probeNodeUpstream(exit, host string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), nodeUpstreamProbeTimeout)
	defer cancel()
	conn, err := dialViaProxy(ctx, exit, "tcp", host+":443")
	if err != nil {
		return false
	}
	defer conn.Close()
	hsCtx, hsCancel := context.WithTimeout(ctx, nodeUpstreamProbeTimeout/2)
	defer hsCancel()
	return tls.Client(conn, &tls.Config{ServerName: host}).HandshakeContext(hsCtx) == nil
}

// probeUpstreamMatrixAsync 后台把所有"出口 × 上游"组合探一遍。
// 节流: 上次探测距今不足间隔时直接跳过, 避免按钮连点把出口打爆。
func probeUpstreamMatrixAsync() {
	// 出口 = 手动代理/节点 + 订阅节点(与轮询用的列表同一份)
	exits := effectiveProxyList()
	if len(exits) == 0 {
		return
	}
	targets := upstreamTargets()
	if len(targets) == 0 {
		return
	}

	nodeUpstreamMu.Lock()
	if nodeUpstreamProbing || time.Since(nodeUpstreamLastAt) < nodeUpstreamProbeInterval {
		nodeUpstreamMu.Unlock()
		return
	}
	nodeUpstreamProbing = true
	nodeUpstreamMu.Unlock()
	defer func() {
		nodeUpstreamMu.Lock()
		nodeUpstreamProbing = false
		nodeUpstreamLastAt = time.Now()
		nodeUpstreamMu.Unlock()
	}()

	// 先把外层 map 与全部内层 map 建好再并发写。
	// 原实现里外层 map 由主协程边循环边赋值, 而工作协程同时在读它 ——
	// Go 对 map 的并发读写是 fatal error(不可 recover), 会直接打死进程。
	names := make([]string, 0, len(targets))
	for name := range targets {
		names = append(names, name)
	}
	sort.Strings(names)
	next := make(map[string]map[string]bool, len(names))
	for _, name := range names {
		next[name] = make(map[string]bool, len(exits))
	}

	sem := make(chan struct{}, nodeUpstreamProbeWorkers)
	var wg sync.WaitGroup
	var mu sync.Mutex
	okCount := 0
	for _, upstream := range names {
		host := targets[upstream]
		for _, exit := range exits {
			wg.Add(1)
			go func(upstream, host, exit string) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				ok := probeNodeUpstream(exit, host)
				mu.Lock()
				next[upstream][nodeLocalKey(exit)] = ok
				if ok {
					okCount++
				}
				mu.Unlock()
			}(upstream, host, exit)
		}
	}
	wg.Wait()

	nodeUpstreamMu.Lock()
	nodeUpstreamOK = next
	nodeUpstreamMu.Unlock()
	log.Printf("  nodes: 上游可达性探测完成, %d 个上游 × %d 个出口, %d 个组合可达",
		len(targets), len(exits), okCount)
}

// nodeUpstreamSnapshot 面板展示用: 单个节点支持的上游集合。
func nodeUpstreamSnapshot(nodeKey string) map[string]bool {
	nodeUpstreamMu.RLock()
	defer nodeUpstreamMu.RUnlock()
	out := map[string]bool{}
	for upstream, m := range nodeUpstreamOK {
		if ok, known := m[nodeKey]; known {
			out[upstream] = ok
		}
	}
	return out
}

// nodeUpstreamOverview 面板概览: upstream -> 可用节点数 / 已探测节点数。
func nodeUpstreamOverview() map[string]any {
	nodeUpstreamMu.RLock()
	defer nodeUpstreamMu.RUnlock()
	out := map[string]any{}
	for upstream, m := range nodeUpstreamOK {
		usable := 0
		for _, ok := range m {
			if ok {
				usable++
			}
		}
		out[upstream] = map[string]any{"usable": usable, "probed": len(m)}
	}
	return out
}

// nodeUpstreamStale 探测结果是否已过期(面板提示用)。
func nodeUpstreamStale() bool {
	nodeUpstreamMu.RLock()
	defer nodeUpstreamMu.RUnlock()
	return nodeUpstreamLastAt.IsZero() || time.Since(nodeUpstreamLastAt) > nodeUpstreamProbeInterval
}
