package app

// 每节点稳定端口 + 每节点流量统计 (P2, 参照 easy_proxies 的
// "stable per-node ports across subscription updates" 与 WebUI 流量图):
//
//   1. **稳定端口**: 旧实现在每次重建 sing-box 时给节点随机分配 freeLocalPort,
//      订阅一更新端口就漂移, 节点本地的 127.0.0.1:port 没法当作稳定的调试/固定
//      入口。现在把 key→端口 的分配持久化到 data/node-ports.json, 重建时优先
//      复用旧端口(端口被占才重新分配并更新记录)。
//   2. **流量统计**: 在节点拨号返回的 net.Conn 上包一层计数器(上行=写出,
//      下行=读入), 面板即可展示每节点的累计收发字节。计数是内存态, 重启归零
//      (轻量优先; 历史曲线属后续增强)。

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"cline-go-proxy/internal/kit"
)

// ============ 稳定端口 ============

const nodePortsFile = "node-ports.json"

var (
	nodeStableMu     sync.Mutex
	nodeStablePorts  map[string]int // nodeLocalKey -> 上次分配的端口
	nodeStableLoaded bool
)

// loadNodeStablePorts 惰性加载持久化的端口分配。
func loadNodeStablePorts() {
	if nodeStableLoaded {
		return
	}
	nodeStableLoaded = true
	nodeStablePorts = map[string]int{}
	data, err := os.ReadFile(kit.ResolveDataPath(nodePortsFile))
	if err != nil {
		return
	}
	_ = json.Unmarshal(data, &nodeStablePorts)
}

// persistNodeStablePorts 落盘(失败仅告警, 下次重建会重试)。
func persistNodeStablePorts() {
	if b := mustJSONIndent(nodeStablePorts); b != nil {
		if err := kit.WriteFileAtomicDefault(kit.ResolveDataPath(nodePortsFile), b); err != nil {
			log.Printf("node ports persist failed: %v", err)
		}
	}
}

// assignStablePort 给节点分配本地端口: 优先复用历史端口(且该端口当前未被占),
// 否则分配新的空闲端口并更新记录。返回 (端口, 是否复用了旧端口)。
func assignStablePort(key string) (int, bool, error) {
	loadNodeStablePorts()
	nodeStableMu.Lock()
	defer nodeStableMu.Unlock()
	if old, ok := nodeStablePorts[key]; ok && old > 0 && tcpPortFree(old) {
		return old, true, nil
	}
	p, err := freeLocalPort()
	if err != nil {
		return 0, false, err
	}
	nodeStablePorts[key] = p
	persistNodeStablePorts()
	return p, false, nil
}

// tcpPortFree 探测本地端口是否可用(尝试监听后立即释放)。
func tcpPortFree(port int) bool {
	l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false
	}
	l.Close()
	return true
}

// ============ 每节点流量统计 ============

type nodeTrafficCounter struct {
	up   atomic.Int64
	down atomic.Int64
	last atomic.Int64 // 最近一次有流量的 unix ms(面板判断活跃)
}

var (
	nodeTrafficMu sync.Mutex
	nodeTraffic   = map[string]*nodeTrafficCounter{}
)

// nodeTrafficCounterOf 只读获取计数器(不存在返回 false)。
func nodeTrafficCounterOf(key string) (*nodeTrafficCounter, bool) {
	nodeTrafficMu.Lock()
	defer nodeTrafficMu.Unlock()
	c, ok := nodeTraffic[key]
	return c, ok
}

func trafficCounterFor(key string) *nodeTrafficCounter {
	nodeTrafficMu.Lock()
	defer nodeTrafficMu.Unlock()
	c, ok := nodeTraffic[key]
	if !ok {
		c = &nodeTrafficCounter{}
		nodeTraffic[key] = c
	}
	return c
}

// nodeTrafficSnapshot 返回全部节点的累计流量(字节), 按 key 排序。
func nodeTrafficSnapshot() []map[string]any {
	nodeTrafficMu.Lock()
	defer nodeTrafficMu.Unlock()
	keys := make([]string, 0, len(nodeTraffic))
	for k := range nodeTraffic {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]map[string]any, 0, len(keys))
	for _, k := range keys {
		c := nodeTraffic[k]
		out = append(out, map[string]any{
			"key":       k,
			"upBytes":   c.up.Load(),
			"downBytes": c.down.Load(),
			"lastMs":    c.last.Load(),
		})
	}
	return out
}

// nodeTrafficReset 清零流量统计(面板"重置计数"按钮)。
func nodeTrafficReset() {
	nodeTrafficMu.Lock()
	defer nodeTrafficMu.Unlock()
	nodeTraffic = map[string]*nodeTrafficCounter{}
}

// countingConn 带上下行计数的 net.Conn 包装。
type countingConn struct {
	net.Conn
	up   *atomic.Int64
	down *atomic.Int64
	last *atomic.Int64
}

func (c *countingConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	if n > 0 {
		c.up.Add(int64(n))
		c.last.Store(time.Now().UnixMilli())
	}
	return n, err
}

func (c *countingConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if n > 0 {
		c.down.Add(int64(n))
		c.last.Store(time.Now().UnixMilli())
	}
	return n, err
}

// countNodeConn 给节点连接包上流量计数(拨号层调用)。
func countNodeConn(key string, c net.Conn) net.Conn {
	tc := trafficCounterFor(key)
	return &countingConn{Conn: c, up: &tc.up, down: &tc.down, last: &tc.last}
}
