package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"free-router/internal/kit"
	"gopkg.in/yaml.v3"
)

// 订阅链接: 代理池的「订阅链接」里填订阅地址, 定期抓取并展开为节点,
// 与手动代理合并后一起进入轮询。支持三种内容格式:
// sing-box JSON 配置(outbounds) / Clash YAML(proxies) / base64 或明文节点链接列表。

const (
	// defaultSubsRefreshMins 订阅刷新的默认间隔(分钟), 面板可改。
	defaultSubsRefreshMins = 30
	// subRefreshMinMins 允许的最短刷新间隔: 再短就是在打订阅端, 无意义。
	subRefreshMinMins = 1
	// subRefreshMaxMins 允许的最长刷新间隔(30 天): 更长的需求应改用手动刷新。
	subRefreshMaxMins = 24 * 60 * 30
)

var (
	// 锁顺序(唯一合法): nodeMu -> subMu。详见 nodes.go 的 nodeMu 声明处注释(§2.3 第 10 项)。
	// 即: 只有在已经持有 nodeMu 的情况下才能再取 subMu; 反之禁止。resolveSubscriptions
	// 只在持 subMu 时做快照, 慢操作(saveSubCache)放到释放 subMu 之后。
	subMu       sync.Mutex
	subFetchMu  sync.Mutex            // 串行化抓取轮次, 保证后到的清理/更新不被在途抓取覆盖
	subNodes    []any                 // 解析后的节点: 节点链接 string 或 sing-box 出站 map
	subNodeKeys []string              // 与 subNodes 一一对应的池内键(节点链接或 sbox://tag)
	subStatus   = map[string]string{} // 订阅 URL -> 最近抓取结果
	// subNodesByURL 订阅 URL -> 该订阅上一轮解析出的节点(P2-10 归属表, 随 subMu 走)。
	// 部分订阅抓取失败时, 失败订阅沿用这里记着的上一轮结果, 而不是整体丢弃 ——
	// 旧实现只防了"全部失败", 1 成 1 败时失败订阅的节点会当轮从出口池消失并落盘。
	subNodesByURL = map[string][]any{}
)

func subCacheFile() string { return kit.ResolveDataPath("subs_cache.json") }

// subNodeKeysSnapshot 当前订阅节点的池内键
func subNodeKeysSnapshot() []string {
	subMu.Lock()
	defer subMu.Unlock()
	return append([]string(nil), subNodeKeys...)
}

func subStatusSnapshot() map[string]string {
	subMu.Lock()
	defer subMu.Unlock()
	out := make(map[string]string, len(subStatus))
	for k, v := range subStatus {
		out[k] = v
	}
	return out
}

// saveSubCache 把给定的订阅节点快照原子落盘。调用方负责在持 subMu 期间先拷出快照,
// 再释放 subMu 后调用本函数 —— 避免文件 I/O 这种慢操作长时间占用 subMu(§2.3 第 10 项)。
func saveSubCache(nodes []any) {
	b, err := json.Marshal(map[string]any{"nodes": nodes})
	if err != nil {
		// marshal 失败绝不能落盘, 否则写空文件会清空订阅缓存。
		log.Printf("subs cache marshal failed: %v", err)
		return
	}
	if err := kit.WriteFileAtomicDefault(subCacheFile(), b); err != nil {
		log.Printf("subs cache save failed: %v", err)
	}
}

// loadSubCache 启动时恢复上次解析的订阅节点, 无需等待网络
func loadSubCache() {
	path := subCacheFile()
	b, err := os.ReadFile(path)
	if err != nil {
		// 文件不存在是首次启动的正常情况; 其余读失败若不打日志, "重启后出口池空"
		// 就没有任何线索可查(P3)。
		if !os.IsNotExist(err) {
			log.Printf("  订阅缓存读取失败(%s): %v", path, err)
		}
		return
	}
	var c struct {
		Nodes []any `json:"nodes"`
	}
	if json.Unmarshal(b, &c) != nil {
		// 文件存在但 JSON 损坏: 原实现静默放弃恢复, 重启后出口池空、日志无线索(P3)。
		log.Printf("  订阅缓存损坏, 忽略恢复(%s, %d 字节)", path, len(b))
		return
	}
	if len(c.Nodes) > 0 {
		subMu.Lock()
		subNodes = c.Nodes
		rebuildSubKeysLocked()
		// 长度必须在锁内读取: 否则与 resolveSubscriptions 并发时会读到未同步的 subNodes。
		n := len(subNodes)
		subMu.Unlock()
		log.Printf("  订阅缓存: %d 个节点已恢复", n)
		// 缓存节点立即进入出口池, 不等首次订阅抓取
		syncNodeBox()
	}
}

func rebuildSubKeysLocked() {
	subNodeKeys = make([]string, 0, len(subNodes))
	for _, e := range subNodes {
		subNodeKeys = append(subNodeKeys, subEntryKey(e))
	}
}

// subEntryKey 节点条目在代理池中的键: 链接原文(去名称)或 sbox://tag
func subEntryKey(e any) string {
	switch v := e.(type) {
	case string:
		return nodeLocalKey(v)
	case map[string]any:
		tag, _ := v["tag"].(string)
		return "sbox://" + tag
	default:
		return ""
	}
}

// subSourceID 订阅源标识: 订阅 URL 的 8 位短哈希。生成订阅内节点 tag 时混入,
// 避免两个订阅各自的同名/同下标节点共用一个 key、后者在合并去重时被静默丢弃(P2)。
// 用哈希而非 host: 同 host 不同路径/token 的两条订阅也要互相区分; URL 不变则
// 标识跨重启稳定(稳定端口表键只在升级换 tag 时漂移一次, 可接受)。
func subSourceID(u string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(u))
	return fmt.Sprintf("%08x", h.Sum32())
}

// errReasonNoURL 剥掉错误里内嵌的完整请求 URL: *url.Error 的 Error() 形如
// `Get "https://user:token@host/path?token=…": dial tcp …`, 直接入状态接口或
// 日志等于把订阅凭据明文外泄(对照 maskURLForLog: 一条落盘等于泄露)。只留内层原因。
func errReasonNoURL(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		return ue.Err.Error()
	}
	return err.Error()
}

// resolveSubscriptions 抓取全部订阅并重建节点池
func resolveSubscriptions(urls []string) {
	// 串行化: 防止在途抓取与新的清理/更新交错覆盖
	subFetchMu.Lock()
	defer subFetchMu.Unlock()

	clean := make([]string, 0, len(urls))
	for _, u := range urls {
		if u = strings.TrimSpace(u); u != "" {
			clean = append(clean, u)
		}
	}
	if len(clean) == 0 {
		subMu.Lock()
		subNodes = nil
		subNodeKeys = nil
		subStatus = map[string]string{}
		subNodesByURL = map[string][]any{} // 订阅列表被清空: 归属表一并清掉
		subMu.Unlock()
		// 慢操作(文件 I/O)放到释放 subMu 之后, 不在持锁期间做(§2.3 第 10 项)。
		saveSubCache(nil)
		syncNodeBox()
		return
	}
	// 清理已移除订阅的状态记录
	subMu.Lock()
	for k := range subStatus {
		found := false
		for _, u := range clean {
			if u == k {
				found = true
				break
			}
		}
		if !found {
			delete(subStatus, k)
			delete(subNodesByURL, k) // 被移除的订阅不再保留其节点归属
		}
	}
	subMu.Unlock()
	// 并发抓取(上限 4): 串行时 N 个慢订阅的超时叠加, 全部抖动一次就数分钟无出口。
	// per-sub 超时仍由 fetchSubscription 内的 subsFetchTimeout 约束; 归属合并语义不变。
	type subResult struct {
		u     string
		nodes []any
		err   error
	}
	results := make([]subResult, len(clean))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i, u := range clean {
		wg.Add(1)
		go func(i int, u string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			nodes, err := fetchSubscription(u)
			results[i] = subResult{u: u, nodes: nodes, err: err}
		}(i, u)
	}
	wg.Wait()
	fetched := map[string][]any{} // 本轮成功的订阅 -> 新节点
	for _, r := range results {
		if r.err != nil {
			// *url.Error 的 Error() 内嵌完整请求 URL(含 userinfo 与 ?token=):
			// 状态接口会原样回给面板、日志一条落盘等于泄露 —— 只留遮蔽后的 URL
			// 与剥掉 URL 的内层原因。
			reason := errReasonNoURL(r.err)
			log.Printf("  订阅 %s 抓取失败: %s", maskURLForLog(r.u), reason)
			subMu.Lock()
			subStatus[r.u] = "❌ 抓取失败: " + maskURLForLog(r.u) + " — " + reason
			subMu.Unlock()
			continue
		}
		log.Printf("  订阅 %s: 解析出 %d 个节点", maskURLForLog(r.u), len(r.nodes))
		subMu.Lock()
		subStatus[r.u] = fmt.Sprintf("✅ %s · %d 节点", time.Now().Format("01-02 15:04"), len(r.nodes))
		subMu.Unlock()
		fetched[r.u] = r.nodes
	}
	subMu.Lock()
	prevCount := len(subNodes)
	if len(fetched) == 0 && prevCount > 0 {
		// 全部订阅本次都抓取失败: 保留上一次的节点与缓存(旧行为不变)。
		// 出口池全靠订阅供给, 一次网络抖动不该把它清空 —— 否则面板上
		// "暂无出口节点"、请求全部失败, 而空结果还会覆盖订阅缓存,
		// 连重启都救不回来, 只能干等下一次刷新成功。
		log.Printf("  订阅: %d 个订阅本次全部抓取失败, 保留原有 %d 个节点", len(clean), prevCount)
		subMu.Unlock()
		return
	}
	// 归属键集必须用**本轮更新前**的 subNodesByURL 计算: 成功订阅的新结果稍后才覆盖。
	claimed := map[string]bool{}
	for _, ns := range subNodesByURL {
		for _, e := range ns {
			if k := subEntryKey(e); k != "" {
				claimed[k] = true
			}
		}
	}
	// 旧节点里归属不到任何订阅的(典型: 重启后从 subs_cache 恢复的扁平列表,
	// 归属表是空的): 有订阅失败时按"未认领的上一轮结果"兜底保留。
	var orphan []any
	if len(fetched) < len(clean) {
		for _, e := range subNodes {
			if k := subEntryKey(e); k != "" && !claimed[k] {
				orphan = append(orphan, e)
			}
		}
	}
	for u, ns := range fetched {
		subNodesByURL[u] = ns
	}
	// 按订阅分组合并(P2-10): 成功订阅用本轮新结果, 失败订阅沿用它上一轮的结果。
	// 旧实现只把成功订阅的节点放进 merged —— 1 成 1 败时失败订阅的全部节点当轮
	// 从出口池消失, 残缺结果还被 saveSubCache 落盘, 重启也恢复不回来。
	merged := make([]any, 0, prevCount)
	seen := map[string]bool{}
	add := func(list []any) {
		for _, e := range list {
			k := subEntryKey(e)
			if k == "" || seen[k] {
				continue
			}
			seen[k] = true
			merged = append(merged, e)
		}
	}
	for _, u := range clean {
		if ns, ok := fetched[u]; ok {
			add(ns)
			continue
		}
		if ns, ok := subNodesByURL[u]; ok {
			add(ns)
			continue
		}
		add(orphan) // 该订阅在本进程内从未成功过: 兜底保留未认领的旧节点
	}
	subNodes = merged
	rebuildSubKeysLocked()
	// 先在持锁期间拷出快照, 再释放 subMu 后做慢操作(文件 I/O), 不在持锁期间做(§2.3 第 10 项)。
	snap := append([]any(nil), subNodes...)
	subMu.Unlock()
	saveSubCache(snap)
	syncNodeBox()
}

// subsRefreshInterval 当前生效的订阅刷新间隔, 越界值夹回允许区间。
func subsRefreshInterval() time.Duration {
	mins := getZenConfig().SubsRefreshMins
	if mins < subRefreshMinMins {
		// 低于下限夹到下限(而非回落默认), 与写入侧(越界直接 400)行为一致。
		mins = subRefreshMinMins
	}
	if mins > subRefreshMaxMins {
		mins = subRefreshMaxMins
	}
	return time.Duration(mins) * time.Minute
}

// refreshSubsLoop 后台定期刷新订阅。
// 间隔取自配置(默认 30 分钟)且每次触发前重新读取, 因此在面板上改完即生效,
// 不需要重启进程。
func refreshSubsLoop(subs []string) {
	resolveSubscriptions(subs)
	last := time.Now()
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			if time.Since(last) < subsRefreshInterval() {
				continue
			}
			cfg := getZenConfig()
			if len(cfg.Subs) == 0 {
				last = time.Now()
				continue
			}
			resolveSubscriptions(cfg.Subs)
			last = time.Now()
		case <-appRootCtx.Done():
			// 收到退出信号: 停止订阅刷新协程, 让进程能够真正停下。
			return
		}
	}
}

// fetchSubscription 抓取单个订阅。
//
// 出口跟随全局出口模式(统一走网关出口客户端): 代理模式经节点, 直连模式经
// sing-box 的 direct 出站。原先这里在节点失败后"退回直连"——那是一处隐式
// 绕开模式的旁路, 现在统一交给出口决策: 只有当确实没有可用节点时, 才由
// 兜底开关决定是否直连(是否经 sing-box 的 direct 出站)。
// fetchSubscription 抓取单个订阅。
// 出口跟随全局出口模式(直连经 sing-box direct 出站, 代理经节点出站);
// 抓取失败且打开了"节点全挂时直连兜底"时, 再用 Go 原生直连救一次 ——
// 订阅是整个出口池的唯一来源, 它不能跟着出口一起死。
func fetchSubscription(u string) ([]any, error) {
	// 订阅源标识: 生成订阅内节点 tag 时混入, 让两个订阅的同名/同下标节点各有
	// 独立 key, 不再在合并去重时被静默丢弃(P2)。
	src := subSourceID(u)
	ctx, cancel := context.WithTimeout(context.Background(), subsFetchTimeout)
	defer cancel()
	body, err := doFetch(ctx, u, getZenHTTPClient())
	if err == nil {
		return parseSubContent(string(body), src)
	}
	if !rescueDirectEnabled() {
		return nil, err
	}
	log.Printf("  订阅 %s 经出口抓取失败(%s), 用直连兜底再试一次", maskURLForLog(u), errReasonNoURL(err))
	dctx, dcancel := context.WithTimeout(context.Background(), subsFetchTimeout)
	defer dcancel()
	body, err = doFetch(dctx, u, &http.Client{Timeout: subsFetchTimeout, Transport: rescueDirectTransport})
	if err != nil {
		return nil, err
	}
	return parseSubContent(string(body), src)
}

// rescueDirectTransport 直连兜底抓取专用: DefaultTransport 克隆 + dialWithSSRFGuard。
// 主路径经 zenTransport(其基础拨号统一挂 guard); 兜底路径不经它, 这里单独接上,
// 否则兜底拨号就是 SSRF 防线的缺口。共享单例而非每次抓取克隆 —— 复用连接池,
// 免得每次兜底新建一个池、旧池的空闲连接无人回收。
var rescueDirectTransport = func() *http.Transport {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = dialWithSSRFGuard(tr.DialContext)
	return tr
}()

// subsFetchTimeout 单次订阅抓取的整体超时。
const subsFetchTimeout = 60 * time.Second

// subMaxBodyBytes 单个订阅响应体的读取上限(8MiB)。
const subMaxBodyBytes = 8 << 20

func doFetch(ctx context.Context, u string, client *http.Client) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	// 常见订阅端按 UA 分发格式: clash UA 得 YAML, 我们的解析器三种格式通吃
	req.Header.Set("User-Agent", "clash.meta/1.18.1")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	// 读 limit+1 判触顶: 直接 LimitReader(8MiB) 会把超大订阅静默截断, 截断结果
	// 照常解析出前 N 条并记"✅ N 节点", 尾部节点无声丢失(P3)。
	b, err := io.ReadAll(io.LimitReader(resp.Body, subMaxBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > subMaxBodyBytes {
		return nil, fmt.Errorf("订阅内容超过 %dMiB 上限", subMaxBodyBytes>>20)
	}
	return b, nil
}

// parseSubContent 识别订阅内容格式并解析为节点列表。
// src 是订阅源标识(subSourceID), 传给 map 类内容生成带命名空间的 tag。
func parseSubContent(body string, src string) ([]any, error) {
	trimmed := strings.TrimSpace(body)
	if trimmed == "" {
		return nil, fmt.Errorf("订阅内容为空")
	}
	if strings.HasPrefix(trimmed, "{") {
		return parseSingBoxSub(trimmed, src)
	}
	if strings.Contains(trimmed, "\nproxies:") || strings.HasPrefix(trimmed, "proxies:") {
		if nodes, err := parseClashSub(trimmed, src); err == nil && len(nodes) > 0 {
			return nodes, nil
		}
	}
	text := trimmed
	if !strings.Contains(text, "://") {
		if dec, err := b64Decode(strings.Join(strings.Fields(text), "")); err == nil {
			text = string(dec)
		}
	}
	var nodes []any
	seen := map[string]bool{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if !strings.Contains(line, "://") || strings.Contains(line, "<") {
			continue
		}
		// 只保留节点/代理形态的行(仅 authority, 无资源路径), 排除普通网页链接
		if !isNodeOrProxyLine(line) {
			continue
		}
		key := subEntryKey(line)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		nodes = append(nodes, line)
	}
	if len(nodes) == 0 {
		return nil, fmt.Errorf("未识别的订阅内容格式")
	}
	return nodes, nil
}

// isNodeOrProxyLine 判断行是否为节点/代理链接。
// 本意: 收下"节点/手动代理形态"的行(仅 authority), 排除普通网页链接。
func isNodeOrProxyLine(line string) bool {
	scheme, rest, ok := strings.Cut(line, "://")
	if !ok {
		return false
	}
	if i := strings.IndexAny(rest, "#"); i >= 0 {
		rest = rest[:i]
	}
	// 节点 scheme 的载荷不是网页资源路径: vmess 是整段 base64, 而**标准 base64
	// 字母表就含 "/"**, 用"含 / 即资源路径"判据会把几乎全部未用 URL-safe 变体的
	// vmess 载荷误杀; vless/trojan 的 query 里也可能有未编码的 "/?ed=2048"(P3-27)。
	// 这些 scheme 以节点行收下, 真正解析不了的交给 nodeOutbound 逐条剔除。
	if nodeSchemes[scheme] {
		return true
	}
	// 手动代理(http/socks5 等)与普通网页同形, 仍按"仅 authority、无资源路径"
	// 区分 —— 否则 https://example.com/path 会被误收成代理节点。
	if scheme == "http" || scheme == "https" || scheme == "socks5" || scheme == "socks5h" {
		return !strings.Contains(rest, "/")
	}
	return false
}

// parseSingBoxSub sing-box JSON 配置: 取可用出站(跳过 direct/block/分组等)
// tag 混入订阅源标识(src): 两个订阅各自含同名节点时键必须独立, 否则后者在
// resolveSubscriptions 合并去重时被静默丢弃(P2)。
func parseSingBoxSub(body string, src string) ([]any, error) {
	var cfg struct {
		Outbounds []map[string]any `json:"outbounds"`
	}
	if err := json.Unmarshal([]byte(body), &cfg); err != nil {
		return nil, err
	}
	skip := map[string]bool{"direct": true, "block": true, "dns": true, "selector": true, "urltest": true, "group": true}
	var nodes []any
	used := map[string]bool{}
	for _, ob := range cfg.Outbounds {
		typ, _ := ob["type"].(string)
		if typ == "" || skip[typ] {
			continue
		}
		name, _ := ob["tag"].(string)
		if name == "" {
			name = fmt.Sprint(ob["server"])
		}
		tag := "sub-" + src + "-" + sanitizeNodeName(name)
		for used[tag] {
			tag += "x"
		}
		used[tag] = true
		ob["tag"] = tag
		nodes = append(nodes, ob)
	}
	if len(nodes) == 0 {
		return nil, fmt.Errorf("sing-box 配置中没有可用出站")
	}
	return nodes, nil
}

func sanitizeNodeName(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '.' || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, s)
}

// parseClashSub Clash YAML proxies → sing-box 出站
func parseClashSub(body string, src string) ([]any, error) {
	var doc struct {
		Proxies []map[string]any `yaml:"proxies"`
	}
	if err := yaml.Unmarshal([]byte(body), &doc); err != nil {
		return nil, err
	}
	var nodes []any
	used := map[string]bool{}
	for i, m := range doc.Proxies {
		ob, err := clashToOutbound(m, i, src)
		if err != nil {
			log.Printf("  订阅: 跳过 Clash 节点 %v: %v", m["name"], err)
			continue
		}
		tag := ob["tag"].(string)
		for used[tag] {
			tag += "x"
		}
		used[tag] = true
		ob["tag"] = tag
		nodes = append(nodes, ob)
	}
	if len(nodes) == 0 {
		return nil, fmt.Errorf("Clash 配置中没有可转换的节点")
	}
	return nodes, nil
}

// clashToOutbound 常见 Clash 节点字段 → sing-box 出站配置
// src 为订阅源标识, 混入 tag 使跨订阅的同名/同下标节点键独立(P2, 见 parseClashSub)。
func clashToOutbound(m map[string]any, i int, src string) (map[string]any, error) {
	typ, _ := m["type"].(string)
	server, _ := m["server"].(string)
	port, err := yInt(m["port"])
	if err != nil || server == "" {
		return nil, fmt.Errorf("bad server/port")
	}
	name, _ := m["name"].(string)
	tag := fmt.Sprintf("sub-%s-%d-%s", src, i, sanitizeNodeName(name))
	ob := map[string]any{"tag": tag, "server": server, "server_port": port}
	tls := clashTLS(m, server, typ)
	switch typ {
	case "ss":
		cipher, _ := m["cipher"].(string)
		password, _ := m["password"].(string)
		ob["type"] = "shadowsocks"
		// 大写/别名 cipher 与分享链接路径同规归一, 否则 sing-box 不认、
		// 整条节点被 validateOutboundEntry 剔除(P2)。
		ob["method"] = normalizeSSMethod(cipher)
		ob["password"] = password
		if pname, _ := m["plugin"].(string); pname != "" {
			ob["plugin"] = clashPluginName(pname)
			ob["plugin_opts"] = serializeClashPluginOpts(m["plugin-opts"])
		}
	case "vmess":
		uuid, _ := m["uuid"].(string)
		ob["type"] = "vmess"
		ob["uuid"] = uuid
		ob["alter_id"] = yIntOr(m["alterId"], 0)
		ob["security"] = yStrOr(m["cipher"], "auto")
		if tls != nil {
			ob["tls"] = tls
		}
		if tr := clashTransport(m); tr != nil {
			ob["transport"] = tr
		}
	case "vless":
		uuid, _ := m["uuid"].(string)
		ob["type"] = "vless"
		ob["uuid"] = uuid
		if flow, _ := m["flow"].(string); flow != "" {
			ob["flow"] = flow
		}
		if tls != nil {
			ob["tls"] = tls
		}
		if tr := clashTransport(m); tr != nil {
			ob["transport"] = tr
		}
	case "trojan":
		password, _ := m["password"].(string)
		ob["type"] = "trojan"
		ob["password"] = password
		if tls != nil {
			ob["tls"] = tls
		}
		if tr := clashTransport(m); tr != nil {
			ob["transport"] = tr
		}
	case "hysteria":
		auth := yStrOr(m["auth-str"], yStrOr(m["auth_str"], yStrOr(m["auth"], "")))
		if auth == "" {
			return nil, fmt.Errorf("missing auth")
		}
		ob["type"] = "hysteria"
		ob["auth_str"] = auth
		if v, err := yInt(m["up"]); err == nil && v > 0 {
			ob["up_mbps"] = v
		} else {
			ob["up_mbps"] = 50
		}
		if v, err := yInt(m["down"]); err == nil && v > 0 {
			ob["down_mbps"] = v
		} else {
			ob["down_mbps"] = 100
		}
		if o, _ := m["obfs"].(string); o != "" {
			ob["obfs"] = o
		}
		if tls != nil {
			ob["tls"] = tls
		}
	case "hysteria2":
		password, _ := m["password"].(string)
		ob["type"] = "hysteria2"
		ob["password"] = password
		if o, _ := m["obfs"].(string); o != "" {
			ob["obfs"] = map[string]any{"type": o, "password": yStrOr(m["obfs-password"], "")}
		}
		if tls != nil {
			ob["tls"] = tls
		}
	case "tuic":
		uuid, _ := m["uuid"].(string)
		ob["type"] = "tuic"
		ob["uuid"] = uuid
		ob["password"] = yStrOr(m["password"], "")
		if cc, _ := m["congestion-controller"].(string); cc != "" {
			ob["congestion_control"] = cc
		}
		if tls != nil {
			if alpn := yStrList(m["alpn"]); len(alpn) > 0 {
				tls["alpn"] = alpn
			}
			ob["tls"] = tls
		}
	case "anytls":
		password, _ := m["password"].(string)
		ob["type"] = "anytls"
		ob["password"] = password
		if tls != nil {
			ob["tls"] = tls
		}
	case "socks5":
		ob["type"] = "socks"
		ob["version"] = "5"
		ob["username"] = yStrOr(m["username"], "")
		ob["password"] = yStrOr(m["password"], "")
	case "http":
		ob["type"] = "http"
		ob["username"] = yStrOr(m["username"], "")
		ob["password"] = yStrOr(m["password"], "")
		if b, _ := m["tls"].(bool); b {
			if tls != nil {
				ob["tls"] = tls
			} else {
				ob["tls"] = map[string]any{"enabled": true, "server_name": server}
			}
		}
	default:
		return nil, fmt.Errorf("不支持的类型 %q", typ)
	}
	return ob, nil
} // clashTLS Clash 节点的 TLS 相关字段 → sing-box tls 块
func clashTLS(m map[string]any, server, typ string) map[string]any {
	b, _ := m["tls"].(bool)
	reality, _ := m["reality-opts"].(map[string]any)
	// trojan/hysteria2/tuic 协议恒为 TLS, Clash 配置中没有 tls 字段
	tlsAlways := typ == "trojan" || typ == "hysteria2" || typ == "tuic"
	if !b && reality == nil && !tlsAlways {
		return nil
	}
	sni := yStrOr(m["servername"], yStrOr(m["sni"], server))
	tls := map[string]any{"enabled": true, "server_name": sni}
	if sb, _ := m["skip-cert-verify"].(bool); sb {
		tls["insecure"] = true
	}
	if fp, _ := m["client-fingerprint"].(string); fp != "" {
		tls["utls"] = map[string]any{"enabled": true, "fingerprint": fp}
	}
	if alpn := yStrList(m["alpn"]); len(alpn) > 0 {
		tls["alpn"] = alpn
	}
	if reality != nil {
		tls["reality"] = map[string]any{
			"enabled":    true,
			"public_key": yStrOr(reality["public-key"], ""),
			"short_id":   yStrOr(reality["short-id"], ""),
		}
	}
	return tls
}

// clashTransport Clash network/ws-opts/grpc-opts → sing-box transport
func clashTransport(m map[string]any) map[string]any {
	network, _ := m["network"].(string)
	switch network {
	case "ws":
		t := map[string]any{"type": "ws", "path": "/"}
		if w, ok := m["ws-opts"].(map[string]any); ok {
			if p, _ := w["path"].(string); p != "" {
				t["path"] = p
			}
			if h, ok := w["headers"].(map[string]any); ok {
				if host, _ := h["Host"].(string); host != "" {
					t["headers"] = map[string]any{"Host": host}
				}
			}
		}
		return t
	case "grpc":
		svc := ""
		if g, ok := m["grpc-opts"].(map[string]any); ok {
			svc, _ = g["grpc-service-name"].(string)
		}
		return map[string]any{"type": "grpc", "service_name": svc}
	case "h2", "http":
		t := map[string]any{"type": "http"}
		if h, ok := m["h2-opts"].(map[string]any); ok {
			if host := yStrList(h["host"]); len(host) > 0 {
				t["host"] = host
			}
		}
		return t
	}
	return nil
}

func clashPluginName(n string) string {
	switch n {
	case "obfs", "simple-obfs":
		return "obfs-local"
	default:
		return n
	}
}

// serializeClashPluginOpts Clash plugin-opts map → SIP002 k=v; 字符串
func serializeClashPluginOpts(v any) string {
	m, ok := v.(map[string]any)
	if !ok {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		switch val := m[k].(type) {
		case bool:
			if val {
				parts = append(parts, k)
			}
		case string:
			if val != "" {
				parts = append(parts, k+"="+val)
			}
		default:
			parts = append(parts, k+"="+fmt.Sprint(val))
		}
	}
	return strings.Join(parts, ";")
}

func yStrOr(v any, def string) string {
	if s, ok := v.(string); ok && s != "" {
		return s
	}
	return def
}

func yIntOr(v any, def int) int {
	if n, err := yInt(v); err == nil {
		return n
	}
	return def
}

func yInt(v any) (int, error) {
	switch x := v.(type) {
	case int:
		return x, nil
	case int64:
		return int(x), nil
	case float64:
		return int(x), nil
	case string:
		return strconv.Atoi(strings.TrimSpace(x))
	default:
		return 0, fmt.Errorf("not a number")
	}
}

func yStrList(v any) []string {
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, it := range list {
		if s, ok := it.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}
