package app

import (
	"cline-go-proxy/internal/cline"
	"log"
	"net/url"
	"sync"
	"time"
)

// 节点连通检测与周期循环(真实 TLS 探活/测速/MITM/地区识别)。
// 由 nodes.go 拆分而来(P2 结构整理): nodes.go 只保留 Box 生命周期/订阅同步/
// TLS 清洗, 各职责独立成文件便于评审与测试。同包内无需导出调整。
// ===== 节点连通检测: 经节点出口向 opencode zen / cline 上游发起真实 TLS 连接 =====
var (
	nodeHealthMu      sync.RWMutex
	nodeHealth        = map[string]nodeHealthState{}
	nodeHealthRunMu   sync.Mutex
	nodeHealthRunning bool
	// healthRunAgain 本轮检测进行中又有人触发时置位: 上一轮结束后补跑一次。
	// 订阅刷新会反复触发 syncNodeBox → 排检测, 原来"重入即静默丢弃"会让新刷进来的
	// 节点一直挂到下一个 30 分钟周期才被测, 面板上表现为"永远是白色未检测"。
	healthRunAgain bool
)

// nodeStartupHealthDelay 启动后第一次连通检测前的等待。
// 上百个节点同时做 TLS 握手会占满出口与 CPU, 面板在这期间会明显卡顿甚至超时,
// 因此让节点先就绪、面板先可用, 再开始检测。
const nodeStartupHealthDelay = 12 * time.Second

type nodeHealthState struct {
	Ok     bool
	At     time.Time
	Result nodeTestResult // 增强测试引擎的完整结果(活性/出口IP/测速/MITM/分类)
}

// healthCheckTargets 连通检测目标: zen 主端点与 cline 上游的 host
func healthCheckTargets() []string {
	var hosts []string
	if raw := zenBaseURLList(getZenConfig()); len(raw) > 0 {
		if u, err := url.Parse(raw[0]); err == nil && u.Hostname() != "" {
			hosts = append(hosts, u.Hostname())
		}
	}
	if u, err := url.Parse(cline.ClineAPIBase); err == nil && u.Hostname() != "" {
		hosts = append(hosts, u.Hostname())
	}
	return hosts
}

// checkNodeHealth 经单个节点出口跑增强测试(活性/出口IP/测速/MITM/分类)。
// 增强测试的"健康"判定: 活且无 MITM 风险且未断流。
// 上游可达性矩阵(probeUpstreamMatrixAsync)仍独立运行, 两者互补。
func checkNodeHealth(key string) bool {
	r := testNodeComprehensiveFn(key)
	// 健康 = 节点存活 + 无 MITM 劫持 + 未断流
	ok := r.Alive && !r.MITMRisk && !r.IsStalled
	return ok
}

// checkAllNodeHealth 并发检测全部节点出口, 并发数随节点规模放大(上限 48)。
// 同时只允许一轮: 节点集合在启动阶段会被订阅解析触发多次重建, 每轮重建都会
// 排一次检测, 不设防会让上百个节点的探测成倍重复, 把出口池与 CPU 一起打满
// (面板的"加载失败"与卡顿就是被这种重复风暴拖出来的)。
//
// 但"重入即丢弃"必须补偿: 置 healthRunAgain 让上一轮结束后立刻补跑, 否则订阅
// 新刷进来的节点要等到下一个 30 分钟周期才被测。
func checkAllNodeHealth() {
	nodeHealthRunMu.Lock()
	if nodeHealthRunning {
		healthRunAgain = true
		nodeHealthRunMu.Unlock()
		return
	}
	nodeHealthRunning = true
	healthRunAgain = false
	nodeHealthRunMu.Unlock()
	defer func() {
		nodeHealthRunMu.Lock()
		nodeHealthRunning = false
		again := healthRunAgain
		healthRunAgain = false
		nodeHealthRunMu.Unlock()
		if again {
			checkAllNodeHealth()
		}
	}()

	nodeMu.Lock()
	keys := make([]string, 0, len(nodePorts))
	for k := range nodePorts {
		keys = append(keys, k)
	}
	nodeMu.Unlock()
	if len(keys) == 0 {
		// 出口池为空 = sing-box 实例没起来(构建失败)。必须显式告警: 静默 return
		// 会让面板上一切节点永远停在"未检测", 而看不出是池子挂了。
		log.Printf("  nodes: 没有可检测的出口(sing-box 实例未就绪), 本轮检测跳过")
		return
	}
	// 并发随规模走: 固定 10 并发跑 800+ 节点, 一轮要几十分钟, 远超 30 分钟周期,
	// 导致绝大多数节点在两次周期之间始终没被测到。
	workers := nodeTestWorkers
	if w := len(keys) / 4; w > workers {
		workers = w
	}
	if workers > nodeTestMaxWorkers {
		workers = nodeTestMaxWorkers
	}
	var okCount int32
	var mu sync.Mutex
	sem := make(chan struct{}, workers)

	// 按**上游服务器**分组(见 nodeRemoteEndpoints 注释)。取不到远端信息的节点
	// 自成一组 —— 不能让它与别的节点共享结论。
	groupIdx := make(map[string]int, len(keys)/4+1)
	groups := make([][]string, 0, len(keys))
	for _, k := range keys {
		g := nodeRemoteEndpointOf(k)
		if g == "" {
			g = "\x00solo\x00" + k
		}
		i, ok := groupIdx[g]
		if !ok {
			i = len(groups)
			groupIdx[g] = i
			groups = append(groups, nil)
		}
		groups[i] = append(groups[i], k)
	}

	record := func(key string, r nodeTestResult) {
		ok := r.Alive && !r.MITMRisk && !r.IsStalled
		mu.Lock()
		if ok {
			okCount++
		}
		mu.Unlock()
		nodeHealthMu.Lock()
		nodeHealth[key] = nodeHealthState{Ok: ok, At: time.Now(), Result: r}
		nodeHealthMu.Unlock()
		// 记下实测出口国家: 地区过滤依赖它, 落盘后重启仍可用(见 exit_region.go)
		rememberNodeCountry(key, r.ExitCountry)
	}

	// 阶段 1: 每组只先探**代表变体**。
	serverDead := make([]bool, len(groups))
	var wg sync.WaitGroup
	for i := range groups {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := groups[i][0]
			sem <- struct{}{}
			defer func() { <-sem }()
			r := testNodeComprehensiveFn(key)
			record(key, r)
			serverDead[i] = !r.Alive
		}(i)
	}
	wg.Wait()

	// 阶段 2: 代表变体连不上(Alive=false)的组, 其余变体**不再探测** —— 同一台
	// 服务器连不上, 它的 SNI 变体不可能连得上。
	// 必须显式记为不可用, 而不是留空: nodeUsable 对"未探测"是按可用处理的,
	// 留空会让几十个死变体继续参与选路。
	// (代表探测成功时仍逐个探: SNI 变体在 MITM/测速/断流上确实可能不同。)
	skipped := 0
	for i := range groups {
		if serverDead[i] {
			for _, k := range groups[i][1:] {
				nodeHealthMu.Lock()
				nodeHealth[k] = nodeHealthState{Ok: false, At: time.Now()}
				nodeHealthMu.Unlock()
				skipped++
			}
			continue
		}
		for _, k := range groups[i][1:] {
			wg.Add(1)
			go func(key string) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				record(key, testNodeComprehensiveFn(key))
			}(k)
		}
	}
	wg.Wait()
	persistNodeCountries()
	// 整批检测完再失效一次出口列表缓存(逐节点失效会把缓存打穿)
	invalidateExitListCache()
	if skipped > 0 {
		log.Printf("  nodes: %d 个变体与其服务器代表同判不可达, 已跳过探测(%d 台服务器分组)",
			skipped, len(groups))
	}
	log.Printf("  nodes: 增强检测完成(%d 并发), %d/%d 个出口可达(去重服务器 %d 台)",
		workers, okCount, len(keys), len(groups))
	// 出口级去重(P2, freesub 语义): 按最新结果折叠同出口 IP 的重复节点,
	// 选路只保留每组最快的 —— 之后 nodeUsable 对折叠副本返回 false。
	recomputeExitFold()
	// 连通性刷新后, 同步刷新地区受限模型的节点能力标记, 以及
	// "节点 × 每个上游"的可达性矩阵(后者用于选节点时跳过到该上游不通的节点)。
	probeRegionModelsAsync()
	probeUpstreamMatrixAsync()
}

// startNodeHealthLoop 每 30 分钟复检
func startNodeHealthLoop() {
	t := time.NewTicker(30 * time.Minute)
	go func() {
		defer t.Stop()
		for {
			select {
			case <-t.C:
				checkAllNodeHealth()
			case <-appRootCtx.Done():
				// 收到退出信号: 停止复检协程, 让进程能够真正停下。
				return
			}
		}
	}()
}
