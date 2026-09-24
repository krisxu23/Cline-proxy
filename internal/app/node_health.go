package app

import (
	"encoding/json"
	"free-router/internal/cline"
	"free-router/internal/kit"
	"log"
	"net/url"
	"os"
	"sort"
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
	Ok     bool           `json:"ok"`
	At     time.Time      `json:"at"`
	Result nodeTestResult `json:"result"` // 增强测试引擎的完整结果(活性/出口IP/测速/MITM/分类)
}

// ===== 健康表落盘: 让重启后立刻拥有上次的健康视图 =====
//
// 为什么必须落盘(2026-09-23 实证): 健康表此前只存在于内存, 重启/订阅重建后
// `healthOf` 对每个节点返回 "unknown", 而 nodeUsable 的口径是"未检测 ≠ 不可用"
// (见 nodes.go 的启动探测注释)—— 于是**全部节点在首轮探测完成前都参与选路**。
// 实测每次重建后有约 2 分半的窗口(12s 启动延迟 + 约 2.5min 全量探测):
//
//	19:03:55  1263 个高级节点出口已就绪
//	19:06:24  增强检测完成 ... 92/1263 个出口可达   ← 2 分 29 秒
//
// 池子里约 85% 是死节点(1131/1327), 这个窗口里它们全都在候选集里 —— 表现就是
// "节点明明一堆, 就是连不上"。用户侧的正确预期是: 非首次启动时本地**存着上一次
// 的节点信息与健康结论**, 可以直接用; 后台再全量复检覆盖。
//
// 与订阅节点缓存(subs_cache.json)同一套思路, 只是缓存的对象是"健康结论"。
const nodeHealthFile = "node-health.json"

// nodeHealthCacheTTL 落盘健康结论的最大可信年龄。
//
// 超过它的一律丢弃、回到"未检测"由首轮探测重新判定。存在的意义是挡住"网关停了
// 一周再开"这种场景 —— 那时的结论已经没有参考价值, 但也不该因此把全部节点当成
// 可用(那正是本机制要修的问题), 所以宁可让它们等首轮探测。
const nodeHealthCacheTTL = 7 * 24 * time.Hour

var nodeHealthLoadOnce sync.Once

// nodeHealthFileOverride 落盘路径覆盖(仅测试用; 空 = data/node-health.json)。
var nodeHealthFileOverride string

func nodeHealthPath() string {
	if nodeHealthFileOverride != "" {
		return nodeHealthFileOverride
	}
	return kit.ResolveDataPath(nodeHealthFile)
}

// ensureNodeHealthLoaded 惰性恢复落盘的健康结论(仅一次)。
//
// 用 sync.Once 而不是"在某个启动点调用一次": 读取点分散在 healthOf /
// checkAllNodeHealth / recomputeExitFold 三处, 漏掉任一处就会出现"部分路径看不到
// 缓存"的诡异行为; Once 让"第一次读"本身就完成加载, 无遗漏。
func ensureNodeHealthLoaded() {
	nodeHealthLoadOnce.Do(loadNodeHealthCache)
}

// loadNodeHealthCache 加载体(单独抽出便于测试反复调用 —— Once 只能跑一次)。
//
// 合并语义是**只补空缺**: 已经存在的键(本进程已探测出的新结论)一律保留 ——
// 万一加载发生在首轮探测之后, 也不能用旧结论覆盖新结论。
func loadNodeHealthCache() {
	data, err := os.ReadFile(nodeHealthPath())
	if err != nil {
		return // 首次运行/文件被删: 正常路径, 静默
	}
	var snap map[string]nodeHealthState
	if err := json.Unmarshal(data, &snap); err != nil {
		log.Printf("  nodes: 健康表缓存解析失败已忽略(将走全量探测): %v", err)
		return
	}
	cutoff := time.Now().Add(-nodeHealthCacheTTL)
	restored, stale := 0, 0
	nodeHealthMu.Lock()
	for k, v := range snap {
		if v.At.Before(cutoff) {
			stale++
			continue
		}
		if _, ok := nodeHealth[k]; !ok {
			nodeHealth[k] = v
			restored++
		}
	}
	nodeHealthMu.Unlock()
	log.Printf("  nodes: 已从本地缓存恢复 %d 条出口健康结论(重启后立即可用, 后台会全量复检覆盖); 过期丢弃 %d 条",
		restored, stale)
}

// persistNodeHealth 落盘当前健康表(失败仅告警, 下一轮探测会重试)。
// 持锁拷贝快照、放锁后再做文件 I/O —— 与 persistNodeStablePorts 同一约定。
func persistNodeHealth() {
	nodeHealthMu.RLock()
	snap := make(map[string]nodeHealthState, len(nodeHealth))
	for k, v := range nodeHealth {
		snap[k] = v
	}
	nodeHealthMu.RUnlock()
	if b := mustJSONIndent(snap); b != nil {
		if err := kit.WriteFileAtomicDefault(nodeHealthPath(), b); err != nil {
			log.Printf("node health persist failed: %v", err)
		}
	}
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

// pruneStaleNodeHealth 删除健康表里不在当前出口池中的残留条目。
// 订阅删除节点后, 旧 key 永远不会再被 record 覆写或清理 —— 见调用点注释。
func pruneStaleNodeHealth(active []string) {
	on := make(map[string]bool, len(active))
	for _, k := range active {
		on[k] = true
	}
	nodeHealthMu.Lock()
	for k := range nodeHealth {
		if !on[k] {
			delete(nodeHealth, k)
		}
	}
	nodeHealthMu.Unlock()
}

// checkAllNodeHealth 并发检测全部节点出口, 并发数随节点规模放大(上限 48)。
// 同时只允许一轮: 节点集合在启动阶段会被订阅解析触发多次重建, 每轮重建都会
// 排一次检测, 不设防会让上百个节点的探测成倍重复, 把出口池与 CPU 一起打满
// (面板的"加载失败"与卡顿就是被这种重复风暴拖出来的)。
//
// 但"重入即丢弃"必须补偿: 置 healthRunAgain 让上一轮结束后立刻补跑, 否则订阅
// 新刷进来的节点要等到下一个 30 分钟周期才被测。
func checkAllNodeHealth() {
	// 先把落盘的健康结论并进来: 首轮探测要跑约 2.5 分钟, 这期间选路读的就是
	// 这张表 —— 不先恢复的话, 全部节点都是 "unknown", nodeUsable 一律放行。
	ensureNodeHealthLoaded()
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
	// 清理已消失节点(订阅删除/重建剔除)的残留健康条目: 旧 key 不会被本轮任何
	// record 覆写, 不删就永久驻留, 其旧 ExitIP 还会继续参与 exit-fold 折叠,
	// 让存活的兄弟节点被误判为重复而跳过选路(2026-09-22 审查 P3)。
	pruneStaleNodeHealth(keys)
	if len(keys) == 0 {
		// 出口池为空 = sing-box 实例没起来(构建失败)。必须显式告警: 静默 return
		// 会让面板上一切节点永远停在"未检测", 而看不出是池子挂了。
		log.Printf("  nodes: 没有可检测的出口(sing-box 实例未就绪), 本轮检测跳过")
		return
	}
	// 冷却表/429 计数/国家映射与健康表同一时机按 active 集合清理(审查 P3):
	// 订阅摘除的 key 在这三张表里同样只增不减, 永久驻留。放在空池早退之后 ——
	// 空池是 sing-box 实例故障不是订阅摘除, 此时清表会把仍在生效的 429 冷却误删。
	pruneStaleExitKeys(keys)
	// 每节点流量表同一时机裁剪(2026-09-24 审查): 它此前只增不减, 订阅 churn 下
	// 摘除的节点永久驻留内存, 面板快照越来越长。
	if n := pruneNodeTraffic(keys); n > 0 {
		log.Printf("  nodes: 每节点流量表已按当前出口裁剪, 移除 %d 条不在订阅里的记录", n)
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
	// 四关失败分解(2026-09-22 审查: 汇总行只有 N/M, 逐关失败原因不落盘,
	// "上千个节点只剩几十个"无法自查 —— 至少把分解计数写进这一行)。
	var stageDead, stageMITM, stageStalled, stageSpeedFail, stageSlow int
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
	// 组内排序: keys 来自 map 迭代(顺序随机), 不排则代表(groups[i][0])与复探
	// 变体(groups[i][1])每轮掷骰 —— 同一死组有时复探到第三个变体, 判死路径与
	// 日志都不可复现。
	for i := range groups {
		sort.Strings(groups[i])
	}

	record := func(key string, r nodeTestResult) {
		ok := r.Alive && !r.MITMRisk && !r.IsStalled
		mu.Lock()
		if ok {
			okCount++
		} else {
			switch {
			case !r.Alive:
				stageDead++
			case r.MITMRisk:
				stageMITM++
			case r.IsStalled:
				stageStalled++
			}
		}
		if r.Alive && r.SpeedTestFailed {
			stageSpeedFail++ // 与判死无关, 独立统计(端点抽风量)
		}
		if r.Alive && r.SpeedBPS > 0 && r.SpeedBPS < nodeSpeedSlowBPS {
			stageSlow++ // 降权池大小
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

	// 阶段 1.5: 代表失败的组先复探确认, 不让一次瞬态失败(三源同超时抖动、或
	// 探测恰逢 syncNodeBox 重建 nodePorts)把整组 SNI 变体判死 30 分钟(审查 P2)。
	// 抽同组 1 个变体复探: 两个都连不上才维持整组判死, 任一成功则回退逐探。
	// "本地入站未就绪"类失败不级联: 代表的本地 mixed 入站没起来(nodeLocalAddr
	// 为空, socks5ProxyURL 报 "outbound not running")时, 失败发生在拨本地
	// sing-box 阶段、与远端服务器无关, 该结果代表不了同组其他变体 —— 直接放行。
	reprobed := make([]string, len(groups)) // 每组复探过的变体, 阶段 2 跳过不重复探
	var confirmWg sync.WaitGroup
	for i := range groups {
		if !serverDead[i] || len(groups[i]) == 0 {
			continue
		}
		if nodeLocalAddr(groups[i][0]) == "" {
			serverDead[i] = false
			continue
		}
		if len(groups[i]) < 2 {
			continue // 独苗组没有可复探的变体, 维持原判定
		}
		confirmWg.Add(1)
		go func(i int) {
			defer confirmWg.Done()
			alt := groups[i][1]
			sem <- struct{}{}
			defer func() { <-sem }()
			r := testNodeComprehensiveFn(alt)
			reprobed[i] = alt
			record(alt, r)
			// 复探方自己本地入站没就绪时失败发生在本地、不代表远端, 不作为判死确认
			if r.Alive || nodeLocalAddr(alt) == "" {
				serverDead[i] = false
			}
		}(i)
	}
	confirmWg.Wait()

	// 阶段 2: 经复探确认连不上(Alive=false)的组, 其余变体**不再探测** —— 同一台
	// 服务器连不上, 它的 SNI 变体不可能连得上(代表单次失败只触发阶段 1.5 复探)。
	// 必须显式记为不可用, 而不是留空: nodeUsable 对"未探测"是按可用处理的,
	// 留空会让几十个死变体继续参与选路。
	// (代表探测成功时仍逐个探: SNI 变体在 MITM/测速/断流上确实可能不同。)
	skipped := 0
	for i := range groups {
		if serverDead[i] {
			for _, k := range groups[i][1:] {
				if k == reprobed[i] {
					continue // 复探已 record: 保留完整 Result, 不覆盖成零值也不重复计数
				}
				nodeHealthMu.Lock()
				nodeHealth[k] = nodeHealthState{Ok: false, At: time.Now()}
				nodeHealthMu.Unlock()
				skipped++
			}
			continue
		}
		for _, k := range groups[i][1:] {
			if k == reprobed[i] {
				continue // 复探已记账, 不重复探
			}
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
	// 健康结论一并落盘: 下次启动/重建就能立刻拿到"哪些出口真的可用", 不必让
	// 全部节点(实测约 85% 是死的)在首轮探测的 ~2.5 分钟窗口里当可用参与选路。
	persistNodeHealth()
	// 整批检测完再失效一次出口列表缓存(逐节点失效会把缓存打穿)
	invalidateExitListCache()
	if skipped > 0 {
		log.Printf("  nodes: %d 个变体与其服务器代表同判不可达, 已跳过探测(%d 台服务器分组)",
			skipped, len(groups))
	}
	log.Printf("  nodes: 增强检测完成(%d 并发), %d/%d 个出口可达(去重服务器 %d 台); 失败分解: 活性挂 %d · MITM %d · 真断流 %d · 测速端点全挂 %d · 慢速降权 %d",
		workers, okCount, len(keys), len(groups), stageDead, stageMITM, stageStalled, stageSpeedFail, stageSlow)
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
