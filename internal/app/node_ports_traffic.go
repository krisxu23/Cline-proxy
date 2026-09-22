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
	"math/rand"
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
	nodePortAllocMu  sync.Mutex // 顺序分配器游标互斥
)

// loadNodeStablePorts 惰性加载持久化的端口分配。
//
// 调用方**必须已持 nodeStableMu**: 惰性初始化(置 loaded 标记 / 建 map /
// 写入)落在锁外时, 启动阶段的并发首访会同时初始化并写同一个 map —— Go 对此
// 是 fatal error: concurrent map writes(不可 recover), 进程直接死亡(P2-5)。
// 全部五个调用点均已调整为「先 Lock 再 load」。
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
// 持锁拷贝快照、放锁后再做文件 I/O: 读 map 必须在锁内(裸读会与并发写竞争),
// 慢 I/O 不占锁(§2.3 第 10 项)。本函数自身取锁, 调用方不得已持 nodeStableMu。
func persistNodeStablePorts() {
	nodeStableMu.Lock()
	loadNodeStablePorts()
	snap := make(map[string]int, len(nodeStablePorts))
	for k, p := range nodeStablePorts {
		snap[k] = p
	}
	nodeStableMu.Unlock()
	if b := mustJSONIndent(snap); b != nil {
		if err := kit.WriteFileAtomicDefault(kit.ResolveDataPath(nodePortsFile), b); err != nil {
			log.Printf("node ports persist failed: %v", err)
		}
	}
}

// assignStablePort 给节点分配本地端口: 优先复用历史端口(且该端口当前未被占),
// 否则分配新的空闲端口并更新记录。返回 (端口, 是否复用了旧端口)。
func assignStablePort(key string) (int, bool, error) {
	nodeStableMu.Lock()
	loadNodeStablePorts()
	if old, ok := nodeStablePorts[key]; ok && old > 0 && !portReservedByService(old) && tcpPortFree(old) {
		nodeStableMu.Unlock()
		return old, true, nil
	}
	p, err := freeNodePortInRange()
	if err != nil {
		nodeStableMu.Unlock()
		return 0, false, err
	}
	nodeStablePorts[key] = p
	nodeStableMu.Unlock()
	// 落盘放到放锁之后(persist 自己持锁取快照), 持锁期间不做慢 I/O。
	persistNodeStablePorts()
	return p, false, nil
}

// purgeStablePorts 清除一批节点的稳定端口记录(Start 失败自愈: 这些端口
// 可能被半启动实例或其它进程占用, 下次重建应重新分配而不是复用)。
func purgeStablePorts(ports map[string]int) {
	if len(ports) == 0 {
		return
	}
	nodeStableMu.Lock()
	loadNodeStablePorts()
	bad := map[int]bool{}
	for _, p := range ports {
		bad[p] = true
	}
	for k, p := range nodeStablePorts {
		if bad[p] {
			delete(nodeStablePorts, k)
		}
	}
	nodeStableMu.Unlock()
	persistNodeStablePorts()
}

// 节点入站端口的自定义区间: 避开 Windows 默认临时源端口区(49152-65535)。
// 入站端口若落在临时区, 网关自己的出站连接会随机抢占同一端口作为源端口,
// 造成 sing-box bind 偶发 "Only one usage" 失败(实测 17:28 重建失败)。
const (
	nodePortMin = 20000
	nodePortMax = 49000
)

// 服务端口保留集合(P2 修复): 管理页/API 的监听端口(如 3457)落在本区间内,
// 若被节点入站抢占, 管理页会"无法访问"。由 StartProxy 启动时登记。
var (
	reservedPortMu sync.Mutex
	reservedPorts  = map[int]bool{}
)

// reserveServicePort 登记服务保留端口, 节点入站分配永远避开。
func reserveServicePort(port int) {
	if port <= 0 {
		return
	}
	reservedPortMu.Lock()
	defer reservedPortMu.Unlock()
	reservedPorts[port] = true
	// 已写进稳定端口表的记录一并清除(升级后首次启动的自愈);
	// load 必须在 nodeStableMu 内执行(P2-5, 见 loadNodeStablePorts 注释)。
	nodeStableMu.Lock()
	loadNodeStablePorts()
	for k, p := range nodeStablePorts {
		if reservedPorts[p] {
			delete(nodeStablePorts, k)
		}
	}
	nodeStableMu.Unlock()
}

// portReservedByService 该端口是否为服务保留端口。
func portReservedByService(port int) bool {
	reservedPortMu.Lock()
	defer reservedPortMu.Unlock()
	return reservedPorts[port]
}

// stablePortOf 查询某 key 的稳定端口记录; 记录不存在/被服务保留/与本批
// 已分配冲突/当前被占, 都返回 0(走顺序分配)。
func stablePortOf(key string, used map[int]bool) int {
	nodeStableMu.Lock()
	loadNodeStablePorts() // 锁内惰性加载(P2-5)
	p, ok := nodeStablePorts[key]
	nodeStableMu.Unlock()
	if !ok || p <= 0 || portReservedByService(p) || used[p] || !tcpPortFree(p) {
		return 0
	}
	return p
}

// recordStablePorts 批量写入稳定端口记录(不落盘, 由调用方统一 persist)。
func recordStablePorts(m map[string]int) {
	nodeStableMu.Lock()
	defer nodeStableMu.Unlock()
	loadNodeStablePorts() // 锁内惰性加载(P2-5)
	for k, p := range m {
		nodeStablePorts[k] = p
	}
}

// nodePortCursor 顺序分配游标: 全进程递增, 避免随机分配的生日碰撞
// (4269 次随机选 29001 端口 → 期望 ~314 对重复, 实测每次重建必死)。
var nodePortCursor = nodePortMin

// allocateNodePortSequential 在 [nodePortMin,nodePortMax] 里顺序扫描,
// 跳过服务保留/本批已用/被占端口, 返回第一个可用者并标记 used。
func allocateNodePortSequential(used map[int]bool) (int, error) {
	nodePortAllocMu.Lock()
	defer nodePortAllocMu.Unlock()
	span := nodePortMax - nodePortMin + 1
	for i := 0; i < span; i++ {
		nodePortCursor++
		if nodePortCursor > nodePortMax {
			nodePortCursor = nodePortMin
		}
		p := nodePortCursor
		if used[p] || portReservedByService(p) || !tcpPortFree(p) {
			continue
		}
		used[p] = true
		return p, nil
	}
	return 0, fmt.Errorf("no free node port in [%d,%d]", nodePortMin, nodePortMax)
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

// freeNodePortInRange 在 [min,max] 里找一个当前空闲的端口。
func freeNodePortInRange() (int, error) {
	for attempt := 0; attempt < 64; attempt++ {
		p := nodePortMin + rand.Intn(nodePortMax-nodePortMin+1)
		if portReservedByService(p) {
			continue
		}
		if tcpPortFree(p) {
			return p, nil
		}
	}
	return 0, fmt.Errorf("no free port in [%d,%d]", nodePortMin, nodePortMax)
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

// ============ 流量历史采样(P2, 面板速率/曲线数据源) ============

const (
	trafficSampleInterval = time.Minute
	trafficHistoryMax     = 120 // 最近 2 小时
)

type trafficSample struct {
	TS        int64 `json:"ts"`
	TotalUp   int64 `json:"totalUp"`
	TotalDown int64 `json:"totalDown"`
}

var (
	trafficHistMu  sync.Mutex
	trafficHistory []trafficSample
	trafficOnce    sync.Once
)

// startTrafficSampler 惰性启动每分钟采样协程(记录总量序列, 面板算速率)。
func startTrafficSampler() {
	trafficOnce.Do(func() {
		go func() {
			t := time.NewTicker(trafficSampleInterval)
			defer t.Stop()
			for range t.C {
				var up, down int64
				nodeTrafficMu.Lock()
				for _, c := range nodeTraffic {
					up += c.up.Load()
					down += c.down.Load()
				}
				nodeTrafficMu.Unlock()
				trafficHistMu.Lock()
				trafficHistory = append(trafficHistory, trafficSample{
					TS: time.Now().UnixMilli(), TotalUp: up, TotalDown: down,
				})
				if len(trafficHistory) > trafficHistoryMax {
					trafficHistory = trafficHistory[len(trafficHistory)-trafficHistoryMax:]
				}
				trafficHistMu.Unlock()
			}
		}()
	})
}

// nodeTrafficHistory 返回速率序列(字节/秒), 由相邻采样差分得到; 最近一条在前。
func nodeTrafficHistory() []map[string]any {
	startTrafficSampler()
	trafficHistMu.Lock()
	h := append([]trafficSample(nil), trafficHistory...)
	trafficHistMu.Unlock()
	out := make([]map[string]any, 0, len(h))
	for i := len(h) - 1; i > 0; i-- {
		dt := h[i].TS - h[i-1].TS
		rate := int64(0)
		if dt > 0 {
			rate = (h[i].TotalDown - h[i-1].TotalDown + h[i].TotalUp - h[i-1].TotalUp) * 1000 / dt
		}
		if rate < 0 {
			rate = 0 // 计数重置后差分为负, 钳零
		}
		out = append(out, map[string]any{"ts": h[i].TS, "bps": rate})
	}
	return out
}
