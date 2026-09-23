package app

import (
	"context"
	"crypto/tls"
	"log"
	"net"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"free-router/internal/cline"
)

// 节点 × 上游 可达性矩阵。
//
// 起因: 原先的连通检测只验证"节点能不能到 zen / cline", 于是反复出现
// "节点列表全绿、某个 Provider 却 502"的困惑 —— 不同上游对出口地区与线路的
// 要求并不一样(例如某些供应商直接封大陆或封数据中心 IP)。
//
// 这里把探测目标扩展到**每一个已配置上游**(zen / cline / 每个通用 Provider),
// 用"TCP 连通 + (https 时)TLS 握手"这种不计费、不消耗额度的方式逐节点验证,
// 结果既用于节点选择(跳过到该上游不通的节点), 也在面板上标注出来。

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

// upstreamTarget 探测目标: 主机 + 端口 + 是否做 TLS 握手。
// 只存 host 会丢掉 scheme —— 上游配 http:// 明文时无条件握手必然失败,
// 可达性矩阵全 false, 出口池被整体误过滤(P2-1)。
type upstreamTarget struct {
	host   string
	port   string
	useTLS bool
}

// upstreamTargetOf 从上游 base URL 解析探测目标:
// http:// 明文(TCP 连通即算可达, 默认端口 80)、https:// 才做 TLS 握手(默认 443),
// URL 里的显式端口优先于按 scheme 推出的默认端口。
func upstreamTargetOf(raw string) (upstreamTarget, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return upstreamTarget{}, false
	}
	host := u.Hostname() // Hostname() 已剥方括号, 拨号时由 net.JoinHostPort 重新加
	if host == "" {
		return upstreamTarget{}, false
	}
	t := upstreamTarget{host: host, port: "443", useTLS: true}
	if strings.EqualFold(u.Scheme, "http") {
		t.port = "80"
		t.useTLS = false
	}
	if p := u.Port(); p != "" {
		t.port = p
	}
	return t, true
}

// upstreamTargets 需要探测的上游及其目标(host + 端口 + scheme)。
func upstreamTargets() map[string]upstreamTarget {
	out := map[string]upstreamTarget{}
	if raw := zenBaseURLList(getZenConfig()); len(raw) > 0 {
		if t, ok := upstreamTargetOf(raw[0]); ok {
			out[upstreamZen] = t
		}
	}
	if t, ok := upstreamTargetOf(cline.ClineAPIBase); ok {
		out[upstreamCline] = t
	}
	for _, name := range providerNames() {
		pc, ok := providerConfigFor(name)
		if !ok || len(enabledAPIKeys(pc, name)) == 0 {
			continue
		}
		if t, ok := upstreamTargetOf(pc.BaseURL); ok {
			out[name] = t
		}
	}
	return out
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

// probeNodeUpstream 经指定出口与上游做一次连通探测:
// https 目标做 TCP + TLS 握手; http 目标(明文)以 TCP 连通为准, 不做握手 ——
// 无条件握手会让配了 http:// 的上游必然失败、矩阵全 false(P2-1)。
// 用握手/连通而不是真实请求: 不消耗额度, 也不需要各家的鉴权格式一致。
// 入口用 dialViaProxy, 因此普通 http/socks5 代理与 sing-box 节点都覆盖。
func probeNodeUpstream(exit string, t upstreamTarget) bool {
	ctx, cancel := context.WithTimeout(context.Background(), nodeUpstreamProbeTimeout)
	defer cancel()
	// net.JoinHostPort 会给 IPv6 字面量补方括号; 裸拼 host+":443" 对
	// "2001:db8::1" 会得到畸形地址, 拨号必败(P2-1)。
	conn, err := dialViaProxy(ctx, exit, "tcp", net.JoinHostPort(t.host, t.port))
	if err != nil {
		return false
	}
	defer conn.Close()
	if !t.useTLS {
		return true // http 明文: TCP 连通即算可达
	}
	hsCtx, hsCancel := context.WithTimeout(ctx, nodeUpstreamProbeTimeout/2)
	defer hsCancel()
	return tls.Client(conn, &tls.Config{ServerName: t.host}).HandshakeContext(hsCtx) == nil
}

// probeUpstreamMatrixAsync 派发一次"出口 × 上游"可达性探测。
// 节流: 上次探测距今不足间隔(或已有探测在途)时直接跳过, 避免按钮连点把出口打爆。
//
// P2-2: 本函数曾把 wg.Wait 内联在自己体内 —— 名为 Async 实为同步, 唯一调用方
// checkAllNodeHealth(带重入保护)会被拖到全部组合探完才能返回, 大池子(数百出口
// × ≥2 上游)单轮可达小时级, 期间节点连通性/出口IP/测速全部停更, 健康循环事实性
// 卡死。现在把探测体丢进 goroutine, 调用立即返回。
//
// 节流标志仍在**派发之前**置位(nodeUpstreamProbing=true), goroutine 收尾时复位
// —— 标志先立起来才挡得住并发再次触发, 否则会退化成无限并发探测。
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

	go func() {
		defer func() {
			nodeUpstreamMu.Lock()
			nodeUpstreamProbing = false
			nodeUpstreamLastAt = time.Now()
			nodeUpstreamMu.Unlock()
		}()
		probeUpstreamMatrixWork(exits, targets)
	}()
}

// probeUpstreamMatrixWork 探测本体: 在 worker 协程里探完全部组合后整批替换结果。
// 只由 probeUpstreamMatrixAsync 的派发协程调用。
func probeUpstreamMatrixWork(exits []string, targets map[string]upstreamTarget) {
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
		t := targets[upstream]
		for _, exit := range exits {
			wg.Add(1)
			go func(upstream string, t upstreamTarget, exit string) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				ok := probeNodeUpstream(exit, t)
				mu.Lock()
				next[upstream][nodeLocalKey(exit)] = ok
				if ok {
					okCount++
				}
				mu.Unlock()
			}(upstream, t, exit)
		}
	}
	wg.Wait() // 只等本探测体内部的工作协程, 调用方(健康循环)不被它拖住

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
