package app

import (
	"context"
	"net"
	"net/url"
	"strings"
)

// 节点展示视图(nodeView/健康查询/显示名/拨号)。
// 由 nodes.go 拆分而来(P2 结构整理): nodes.go 只保留 Box 生命周期/订阅同步/
// TLS 清洗, 各职责独立成文件便于评审与测试。同包内无需导出调整。
// nodeView 出口节点在管理界面的展示条目
type nodeView struct {
	Name      string          `json:"name"`
	Type      string          `json:"type"`
	Source    string          `json:"source"`
	Running   bool            `json:"running"`
	Health    string          `json:"health"`              // ok / fail / unknown
	Regions   []string        `json:"regions,omitempty"`   // 该出口已验证可用的地区受限模型
	Upstreams map[string]bool `json:"upstreams,omitempty"` // 该出口到各上游的可达性

	// 增强检测数据(来自 nodeTestResult)
	LatencyMs   int64  `json:"latencyMs,omitempty"`
	ExitIP      string `json:"exitIp,omitempty"`
	ExitCountry string `json:"exitCountry,omitempty"`
	ExitASNorg  string `json:"exitAsnOrg,omitempty"`
	Region      string `json:"region,omitempty"` // 地区 ID(us/jp/tw/hk/sg/eu/other), 见 exit_region.go
	SpeedBPS    int64  `json:"speedBps,omitempty"`
	IsStalled   bool   `json:"isStalled"`
	MITMRisk    bool   `json:"mitmRisk"`
	IsWarp      bool   `json:"isWarp"`
	NetworkType string `json:"networkType,omitempty"` // datacenter/residential/mobile/cdn/unknown

	// 稳定端口与流量(P2): localPort 是该节点在本机的固定 mixed 入站端口
	// (订阅更新不漂移, 可当调试/固定入口); up/downBytes 是经此节点的累计流量。
	LocalPort   int   `json:"localPort,omitempty"`
	UpBytes     int64 `json:"upBytes,omitempty"`
	DownBytes   int64 `json:"downBytes,omitempty"`
	Blacklisted bool  `json:"blacklisted,omitempty"` // 人工拉黑中
}

// healthOf 节点最近一次连通检测结果
func healthOf(key string) string {
	nodeHealthMu.RLock()
	defer nodeHealthMu.RUnlock()
	st, ok := nodeHealth[key]
	if !ok {
		return "unknown"
	}
	if st.Ok {
		return "ok"
	}
	return "fail"
}

// healthResultOf 节点最近一次增强检测结果(可能为零值)
func healthResultOf(key string) (nodeTestResult, bool) {
	nodeHealthMu.RLock()
	defer nodeHealthMu.RUnlock()
	st, ok := nodeHealth[key]
	if !ok {
		return nodeTestResult{}, false
	}
	return st.Result, true
}

// nodeLinkScheme 取节点链接/代理的协议名
func nodeLinkScheme(line string) string {
	scheme, _, ok := strings.Cut(line, "://")
	if !ok {
		return "unknown"
	}
	return scheme
}

// nodeDisplayName 节点链接的展示名: #名称(解码) 或 host:port
func nodeDisplayName(line string) string {
	if i := strings.LastIndex(line, "#"); i >= 0 {
		name := line[i+1:]
		if dec, err := url.QueryUnescape(name); err == nil {
			name = dec
		}
		if name != "" {
			return name
		}
	}
	if u, err := url.Parse(line); err == nil && u.Host != "" {
		return u.Host
	}
	if i := strings.Index(line, "://"); i >= 0 {
		return line[i+3:]
	}
	return line
}

// subNodeDisplayName 订阅出站 tag(sub-<序>-名称)的展示名
func subNodeDisplayName(tag string) string {
	parts := strings.SplitN(tag, "-", 3)
	if len(parts) == 3 && parts[2] != "" {
		return parts[2]
	}
	return tag
}

// withHealthResult 将增强检测结果填入 nodeView(非节点行不调用)
func withHealthResult(v nodeView, key string) nodeView {
	if r, ok := healthResultOf(key); ok {
		v.LatencyMs = r.LatencyMs
		v.ExitIP = r.ExitIP
		v.ExitCountry = r.ExitCountry
		v.ExitASNorg = r.ExitASNorg
		v.SpeedBPS = r.SpeedBPS
		v.IsStalled = r.IsStalled
		v.MITMRisk = r.MITMRisk
		v.IsWarp = r.IsWarp
		v.NetworkType = r.NetworkType
	}
	if p, ok := nodePorts[key]; ok {
		v.LocalPort = p
	}
	if tc, ok := nodeTrafficCounterOf(key); ok {
		v.UpBytes = tc.up.Load()
		v.DownBytes = tc.down.Load()
	}
	v.Blacklisted = nodeManuallyBlacklisted(key)
	v.Region = nodeExitRegion(key)
	return v
}

// nodeViews 出口池全量条目: 手动代理/节点 + 订阅节点, 按池内顺序
func nodeViews() []nodeView {
	cfg := getZenConfig()
	out := make([]nodeView, 0, len(cfg.Proxies)+8)
	for _, p := range cfg.Proxies {
		line := strings.TrimSpace(p)
		if line == "" {
			continue
		}
		if isNodeLink(line) {
			key := nodeLocalKey(line)
			v := nodeView{
				Name: nodeDisplayName(line), Type: nodeLinkScheme(line),
				Source: "手动", Running: nodeLocalAddr(line) != "", Health: healthOf(key),
				Regions: regionNodeSupport(key), Upstreams: nodeUpstreamSnapshot(key),
			}
			out = append(out, withHealthResult(v, key))
			continue
		}
		out = append(out, nodeView{
			Name: maskProxyURL(line), Type: nodeLinkScheme(line),
			Source: "手动", Running: true, Health: "ok",
		})
	}
	subMu.Lock()
	entries := append([]any(nil), subNodes...)
	subMu.Unlock()
	for _, e := range entries {
		switch v := e.(type) {
		case string:
			key := nodeLocalKey(v)
			nv := nodeView{
				Name: nodeDisplayName(v), Type: nodeLinkScheme(v),
				Source: "订阅", Running: nodeLocalAddr(v) != "", Health: healthOf(key),
				Regions: regionNodeSupport(key), Upstreams: nodeUpstreamSnapshot(key),
			}
			out = append(out, withHealthResult(nv, key))
		case map[string]any:
			tag, _ := v["tag"].(string)
			typ, _ := v["type"].(string)
			key := "sbox://" + tag
			nv := nodeView{
				Name: subNodeDisplayName(tag), Type: typ,
				Source: "订阅", Running: nodeLocalAddr(key) != "", Health: healthOf(key),
				Regions: regionNodeSupport(key), Upstreams: nodeUpstreamSnapshot(key),
			}
			out = append(out, withHealthResult(nv, key))
		}
	}
	return out
}

// dialNodeProxy 经节点本地 mixed 入站的 SOCKS5 侧建立到 addr 的隧道。
// 入站是 mixed(HTTP + SOCKS5 同端口), 统一走 SOCKS5: 支持 UDP 能力、
// 不在回环上暴露明文 CONNECT 主机名、无 HTTP 报文解析歧义。
func dialNodeProxy(ctx context.Context, link, network, addr string) (net.Conn, error) {
	u, err := socks5ProxyURL(link)
	if err != nil {
		return nil, err
	}
	conn, err := dialSOCKS5(ctx, u, network, addr)
	if err != nil {
		return nil, err
	}
	// 每节点流量统计(P2): 拨号成功即包计数器, 上行=写出/下行=读入
	return countNodeConn(nodeLocalKey(link), conn), nil
}
