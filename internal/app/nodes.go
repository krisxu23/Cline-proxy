package app

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sagernet/sing-box/include"
)

// 高级代理节点出口: 代理池直接粘贴 vmess/vless/trojan/ss/hy2/tuic 分享链接,
// 内嵌 sing-box 把每个节点转成本地 mixed 入站端口, 拨号层当作普通 http 代理使用。
// 轮询/冷却/策略逻辑与 http/socks5 代理完全一致。

// nodeBoxInstance 是 sing-box 实例的抽象。用接口而不是具体 *box.Box, 是为了让测试
// 能注入替身(不实例化真实 sing-box)来验证 failKeepOld 等核心并发逻辑, 见 §2.3 第 9/22 项。
type nodeBoxInstance interface {
	Start() error
	Close() error
}

// 锁顺序(唯一合法): nodeMu -> subMu。
// 只有在已经持有 nodeMu 的情况下才能再取 subMu; 反向(subMu 内取 nodeMu)禁止。
// syncNodeBox 在持 nodeMu 时短暂取 subMu 仅做快照并立即释放, 绝不在持 subMu 期间做慢操作;
// resolveSubscriptions 也只在持 subMu 时快照, 慢操作(saveSubCacheLocked)放到释放 subMu 之后。
// 见 §2.3 第 10 项。
var (
	nodeMu        sync.Mutex
	nodeBox       nodeBoxInstance
	nodePorts     map[string]int // 节点链接(去 # 名称) -> 本地 mixed 端口
	nodePortsKeys string         // 当前运行实例对应的链接集合, 用于配置变化比对
	catchAllPort  int            // 常驻 catch-all 入站的本地端口(0 = 未就绪)
)

// startNodeInstanceFn 是可替换的实例构建入口: 测试可注入错误以验证 failKeepOld(§2.6 第 22 项),
// 或注入替身以避开真实 sing-box 实例化(配合 CLINE_PROXY_SKIP_NODEBOX)。默认指向 startNodeInstance。
//
// 配套的 startNodeInstanceInjected 记录「钩子是否被测试替换过」。这里不能用函数值做相等
// 比较 —— Go 里 func 只能和 nil 比, `startNodeInstanceFn == startNodeInstance` 编译不过。
// 所以单独一个 bool 追踪, 统一由 setStartNodeInstanceFn 维护, 测试不要直接给 var 赋值。
var startNodeInstanceFn = startNodeInstance
var startNodeInstanceInjected bool

// setStartNodeInstanceFn 替换实例构建入口并记下「已被替换」; 传 nil 恢复默认实现。
func setStartNodeInstanceFn(fn func(ctx context.Context, inbounds, outbounds, rules []map[string]any) (nodeBoxInstance, error)) {
	if fn == nil {
		startNodeInstanceFn = startNodeInstance
		startNodeInstanceInjected = false
		return
	}
	startNodeInstanceFn = fn
	startNodeInstanceInjected = true
}

// testNodeComprehensiveFn 是可替换的单节点探测入口, 默认指向 testNodeComprehensive。
// 测试可注入替身以绕开真实网络探测(节点连通检测依赖出口链路), 用于锁住 healthRunAgain
// 重入补跑等并发行为(§2.6 第 23 项)。这里只做直接赋值, 没有「是否被替换」的判定需求,
// 所以不需要配套的注入标记。
var testNodeComprehensiveFn = testNodeComprehensive

// catchAllInTag 常驻兜底入站: 让"任何非节点直选"的网络行为也经 sing-box 出去。
// 它的 route.final 是 direct, 因此出口模式为直连时, 流量依然在 sing-box 内部
// 走 direct 出站(而不是绕开 sing-box 用 Go 原生拨号)。
const catchAllInTag = "in-catchall"

// catchAllLocalAddr catch-all 入站的 SOCKS5 地址; 未就绪返回空串。
func catchAllLocalAddr() string {
	nodeMu.Lock()
	defer nodeMu.Unlock()
	if catchAllPort == 0 {
		return ""
	}
	return fmt.Sprintf("127.0.0.1:%d", catchAllPort)
}

var nodeSchemes = map[string]bool{
	"vmess": true, "vless": true, "trojan": true,
	"ss": true, "hy2": true, "hysteria2": true, "tuic": true,
	"hysteria": true, "anytls": true,
	"ssh": true, "shadowtls": true, "snell": true,
	"sbox": true, // 订阅提供的原始 sing-box 出站的池内伪链接
}

func isNodeLink(s string) bool {
	scheme, _, ok := strings.Cut(strings.TrimSpace(s), "://")
	return ok && nodeSchemes[scheme]
}

// nodeLocalKey 取去掉 # 名称的链接作为节点唯一键
func nodeLocalKey(link string) string {
	link = strings.TrimSpace(link)
	if i := strings.Index(link, "#"); i >= 0 {
		link = link[:i]
	}
	return link
}

// nodeLocalAddr 节点对应的本地 mixed 入站地址, 未运行返回 ""
func nodeLocalAddr(link string) string {
	nodeMu.Lock()
	defer nodeMu.Unlock()
	if p, ok := nodePorts[nodeLocalKey(link)]; ok {
		return fmt.Sprintf("http://127.0.0.1:%d", p)
	}
	return ""
}

// nodeBoxSkipEnv 设置后, 测试进程不再实例化真实 sing-box。
//
// 为什么需要: sing-box 的 Box 起来后会拉起自己的后台 goroutine(网络接口变更
// 监听、systemd-resolved 的 DBus 连接), 这些**第三方内部实现**之间存在数据竞争
// —— CI 的 -race 任务实测报了 15 处, 全部是
// route.(*NetworkManager).Start() 与 route.(*NetworkManager).updateInterface()
// 之间的竞争, 没有任何一处涉及本仓库代码。它们无法在网关侧修掉, 但会让整个
// -race 任务变红, 反而掩盖我们自己代码里真正需要被发现的问题。
//
// 所以 -race 任务带上这个变量, 让测试不建实例; 确实需要真实实例的用例通过
// requireNodeBox(t) 显式跳过, 它们继续在普通构建下运行。
const nodeBoxSkipEnv = "CLINE_PROXY_SKIP_NODEBOX"

// nodeBoxSkipRequested 是否应跳过实例化 sing-box。
//
// 只在"当前是测试二进制"且"显式设了环境变量"时成立 —— 加了测试二进制的判定,
// 生产进程无论环境变量怎么设都不会命中, 不存在"误设变量导致节点静默失效"的风险。
func nodeBoxSkipRequested() bool {
	return flag.Lookup("test.v") != nil && os.Getenv(nodeBoxSkipEnv) != ""
}

// syncNodeBox 按代理列表节点链接 + 订阅解析节点重建 sing-box 实例
// (setZenConfig/订阅刷新/启动时调用)
//
// 并发模型(修复 §2.3 第 9 项): 锁内只做 map 快照 + 原子替换; box 的构建
// (startNodeInstance 内含 freeLocalPort / box.New)与 Start 都是可达秒级的 I/O,
// 全部挪到锁外执行, 持锁期间不再阻塞任何读节点 / 取出口的请求。
// 失败路径一律不改全局状态, 旧实例继续服务(§4 已确认 failKeepOld 性质不变)。
// nodeBuildMu 重建互斥(P2 修复): syncNodeBox 的构建/启动阶段在 nodeMu 之外,
// 两次并发 sync(订阅刷新 vs 面板操作)会同时 buildNodeParts 并复用同一批
// 稳定端口 → 后启动的实例 bind 冲突。整个 sync 串行化, 该 bug 实测为
// "节点全部 0 可用"事故的直接原因之一。
var nodeBuildMu sync.Mutex

func syncNodeBox() {
	nodeBuildMu.Lock()
	defer nodeBuildMu.Unlock()
	// 普通测试进程: 跳过真实 sing-box 实例化(sing-box 后台 goroutine 自身有竞争, 见 nodeBoxSkipEnv)。
	// 但若测试注入了 startNodeInstanceFn, 说明要走注入的错误/替身路径, 此时仍需放行(注入点不会真正实例化 sing-box)。
	if nodeBoxSkipRequested() && !startNodeInstanceInjected {
		log.Printf("  nodes: %s 已设置, 跳过 sing-box 实例化(仅用于测试进程)", nodeBoxSkipEnv)
		return
	}
	// ---- 快照阶段: 仅持 nodeMu 做 map 快照 + 重建判定 ----
	nodeMu.Lock()
	cfg := getZenConfig()
	var entries []any
	seen := map[string]bool{}
	for _, p := range cfg.Proxies {
		line := strings.TrimSpace(p)
		if !isNodeLink(line) {
			continue
		}
		key := nodeLocalKey(line)
		if seen[key] {
			continue
		}
		seen[key] = true
		entries = append(entries, line)
	}
	// 锁序 nodeMu -> subMu: 仅在持 nodeMu 时取 subMu, 仅做快照后立刻释放,
	// 绝不在持 subMu 期间做慢操作(§2.3 第 10 项)。
	subMu.Lock()
	entries = append(entries, subNodes...)
	subMu.Unlock()

	// 空入口守卫(P2 修复): 正在服务的实例(有出口)遇到"解析结果为空"时,
	// 多半是订阅刷新的中间态(抓取中 subNodes 被清空)—— 若照常重建, 会用
	// 0 出口实例顶掉服务中的实例, 探测循环因"没有可检测的出口"跳过,
	// 面板全部 0 可用(实测事故)。此时保留实例不重建。
	// 注意只在**已有可用实例**时拦: 启动期(nodeBox 为 nil)必须放行, 否则
	// 首次订阅抓取完成前连 catch-all 实例都没有(实测冒烟发现的反例)。
	if len(entries) == 0 && (len(cfg.Proxies) > 0 || len(cfg.Subs) > 0) && nodeBox != nil && len(nodePorts) > 0 {
		log.Printf("  nodes: 节点解析结果为空(疑似订阅刷新中间态), 保留当前 %d 出口实例不重建", len(nodePorts))
		nodeMu.Unlock()
		return
	}
	var keys []string
	for _, e := range entries {
		if k := subEntryKey(e); k != "" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	joined := strings.Join(keys, "|")
	// 链接集合没变且实例已在, 无需重建。注意必须同时判断实例存在:
	// 零节点启动时 joined 与初值都是空串, 但实例还没建, 要建出只含
	// catch-all 的实例(直连模式也要经过 sing-box)。
	if joined == nodePortsKeys && nodeBox != nil {
		nodeMu.Unlock()
		return
	}
	// 快照旧实例, 失败回滚 / 成功后关闭都基于此快照。
	prevBox, prevPorts, prevCatchAll := nodeBox, nodePorts, catchAllPort
	hasPrev := prevBox != nil
	nodeMu.Unlock()

	// ---- 构建阶段: 锁外完成可达秒级的 I/O(freeLocalPort / box.New / Start) ----
	// 三条失败路径均不修改全局状态: 旧实例继续服务, 出口池不被清空, catch-all 保留旧值。
	ports, inbounds, outbounds, rules, hasMap := buildNodeParts(entries)

	// 常驻 catch-all 入站: 与节点数量无关, 保证"只要网关联网就经过 sing-box"。
	// 零节点时也建实例 —— 直连模式下流量仍走 sing-box 的 direct 出站。
	newCatchAll := prevCatchAll
	if cp, cerr := freeLocalPort(); cerr != nil {
		// 缺 catch-all 的新实例比旧实例更糟, 此时宁可保留旧实例。
		if hasPrev {
			log.Printf("  nodes: catch-all 入站端口分配失败(%v), 保留上一个可用实例", cerr)
			return
		}
		log.Printf("  nodes: catch-all 入站端口分配失败: %v", cerr)
	} else {
		newCatchAll = cp
		inbounds = append(inbounds, map[string]any{
			"type": "mixed", "tag": catchAllInTag,
			"listen": "127.0.0.1", "listen_port": cp,
		})
	}
	outbounds = append(outbounds, map[string]any{"type": "direct", "tag": "direct"})

	// failKeepOld 构建/启动失败时保留上一个可用实例继续服务
	failKeepOld := func(why error) {
		if hasPrev {
			log.Printf("  nodes: %v; 保留上一个可用实例(%d 个出口)继续服务", why, len(prevPorts))
		} else {
			log.Printf("  nodes: 启动失败: %v", why)
		}
	}

	ctx := include.Context(context.Background())
	instance, err := startNodeInstanceFn(ctx, inbounds, outbounds, rules)
	if err != nil && hasMap {
		// 订阅提供的原始出站可能有个别不合法: 退回仅手动节点链接重建,
		// 避免单个坏节点拖垮全部出口
		log.Printf("  nodes: 全量构建失败(%v), 退回仅手动节点重建", err)
		var p2, inb2, outb2, rules2, _ = buildNodeParts(stringEntries(entries))
		if len(outb2) > 0 {
			// 退回重建同样要保留 catch-all, 否则"全部经 sing-box"在这条路径上失效
			if newCatchAll != 0 {
				inb2 = append(inb2, map[string]any{
					"type": "mixed", "tag": catchAllInTag,
					"listen": "127.0.0.1", "listen_port": newCatchAll,
				})
			}
			outb2 = append(outb2, map[string]any{"type": "direct", "tag": "direct"})
			if inst2, err2 := startNodeInstanceFn(ctx, inb2, outb2, rules2); err2 == nil {
				instance, ports, err = inst2, p2, nil
			} else {
				err = err2
			}
		}
	}
	if err != nil {
		failKeepOld(err)
		return
	}
	if err := instance.Start(); err != nil {
		instance.Close()
		// 自愈(P2 修复): Start 失败说明这批端口里有被占的(半启动实例/其它
		// 进程), 把本次使用的稳定端口记录全部清除 —— 下次重建重新分配,
		// 避免反复撞同一批坏端口(实测事故: 同一端口连续失败数小时)。
		purgeStablePorts(ports)
		failKeepOld(fmt.Errorf("启动失败(已清除本次稳定端口记录, 下次重建换端口): %v", err))
		// 当前没有可用实例时, 30 秒后自动重试一次(给订阅刷新/端口释放留时间)
		if len(prevPorts) == 0 && len(entries) > 0 {
			go func() {
				time.Sleep(30 * time.Second)
				syncNodeBox()
			}()
		}
		return
	}

	// ---- 替换阶段: 重新持锁做原子替换 ----
	// 期间若没有其它 sync 替换过实例(nodeBox 仍等于快照里的旧实例), 才关闭旧实例并换上新实例;
	// 否则说明并发的另一次 sync 已经接手, 本次刚建好的新实例直接丢弃, 保留现有实例。
	// 这保证了 failKeepOld 的核心不变量: 先建新实例、成功才 Close 旧实例, 无半替换窗口,
	// 且任何失败路径都不修改全局状态(§2.6 第 22 项测试锁住此性质)。
	nodeMu.Lock()
	if nodeBox == prevBox {
		if prevBox != nil {
			prevBox.Close()
		}
		nodeBox = instance
		nodePorts = ports
		nodePortsKeys = joined
		catchAllPort = newCatchAll
		log.Printf("  nodes: %d 个高级节点出口已就绪", len(ports))
	} else {
		// 期间已被其它 sync 替换: 丢弃本次刚建好的新实例, 保留现有实例。
		instance.Close()
	}
	nodeMu.Unlock()

	// 稍后再做连通检测: 上百个节点同时拨号会占满出口与 CPU, 让面板先可用。
	// 在此之前 healthOf 返回 unknown, 出口照常参与轮询(未探测≠不可用)。
	go func() {
		time.Sleep(nodeStartupHealthDelay)
		checkAllNodeHealth()
	}()
}

// sanitizeOutboundShape 修正出站里会让 sing-box **整条剔除**的字段取值。
//
// 与 sanitizeOutboundTLS(修 TLS 块形态) 并列, 同在 buildNodeParts 的收口处调用。
// 动机(2026-09-18 实测日志): 订阅源普遍使用 v2ray/Xray 的写法, 而 sing-box 只认
// 自己那套取值, 不认识就报错整条剔除。实测被剔除 72 条, 前四类都属"值方言不同、
// 语义完全可以表达" —— 直接映射/省略即可, 不该丢节点:
//
//	22× unknown uTLS fingerprint: unsafe   ← v2ray 的"不校验指纹", sing-box 无对应值
//	12× unsupported flow: none              ← 显式 none 等价于不设 flow
//	 2× unsupported flow: xtls-rprx-vision-udp443  ← 旧名, 现名去掉 -udp443
//	 9× unknown transport type: tcp / raw    ← 就是"无传输层"(裸 TCP), 应省略该字段
//
// **刻意不处理** `unknown transport type: xhttp`: 那是 Xray 的传输, sing-box 没有
// 对应实现, 剥掉后只会退化成裸 TCP 并在连接阶段失败 —— 静默降级比明确剔除更难排查。
func sanitizeOutboundShape(ob map[string]any) {
	// 1) VLESS flow: 只保留 sing-box 认的 xtls-rprx-vision; none/空/未知一律省略。
	if flow, ok := ob["flow"].(string); ok {
		switch normalizeVLESSFlow(flow) {
		case "":
			delete(ob, "flow")
		default:
			ob["flow"] = normalizeVLESSFlow(flow)
		}
	}
	// 2) transport: **只**剥掉"表示无传输层"的写法(tcp/raw/空) —— 那是 v2ray 对
	//    裸 TCP 的表达, 省略该字段就是等价语义。其余未知取值(xhttp/kcp/…)一概
	//    原样留下, 让 sing-box 把它剔除: 它们是真不支持的传输, 剥掉只会静默退化
	//    成裸 TCP, 节点"通过校验却连不上", 比明确剔除更难排查。
	if tr, ok := ob["transport"].(map[string]any); ok {
		if t, _ := tr["type"].(string); isNoTransportMarker(t) {
			delete(ob, "transport")
		}
	}
	// 3) TLS 块内的 uTLS 指纹: 不认识的指纹整块省略(v2ray 的 unsafe/未知拼写)。
	//    注意只动 utls, 不动 tls 本身 —— 省略掉 utls 就是"不做指纹伪装", 语义可表达。
	if tls, ok := ob["tls"].(map[string]any); ok {
		if u, ok := tls["utls"].(map[string]any); ok {
			fp, _ := u["fingerprint"].(string)
			if normalizeUTLSFingerprint(fp) == "" {
				delete(tls, "utls")
			} else {
				u["fingerprint"] = normalizeUTLSFingerprint(fp)
			}
		}
	}
	// 4) xtls 是 v2ray 的旧字段, sing-box 无此块(它用 flow + reality), 省略。
	delete(ob, "xtls")

	// 5) SS: method 字段里塞了 base64("<method>:<password>") 的畸形写法。
	//
	// 实测(2026-09-18) 19 条来自 **sing-box JSON 订阅**(该路径原样透传 outbound),
	// 上游把整段 ss:// userinfo 放进了 method, 于是 sing-box 报
	// "unknown method: <base64>" 整条剔除。分享链接路径本身是对的, 只有这条路径中招。
	//
	// 判据是**明确的**(不是猜测): base64 解出来的前半段必须命中 ssMethodCanonical
	// 已登记的 SS 方法名。密码一律以解出的 userinfo 为准 —— 那段 userinfo 是原始
	// ss:// 的权威来源, 畸形 outbound 里单独的 password 字段反而不可信。
	if typ, _ := ob["type"].(string); typ == "shadowsocks" {
		if m, _ := ob["method"].(string); m != "" && !isKnownSSMethod(m) {
			// 先直接解; 失败再试 URL 解码后解 —— 订阅里存在把 base64 的
			// padding 写成 %3D 的形态(实测 node 1117: "…MQ%3D%3D")。
			dec, err := b64String(m)
			if err != nil {
				if unescaped, uerr := url.QueryUnescape(m); uerr == nil {
					dec, err = b64String(unescaped)
				}
			}
			if err == nil {
				// 用 Cut(首个冒号): 2022-blake3 家族的密码本身含冒号, 不能全切。
				if method, pw, ok := strings.Cut(dec, ":"); ok && isKnownSSMethod(method) {
					ob["method"] = normalizeSSMethod(method)
					ob["password"] = pw
				}
			}
		}
	}
}

// isKnownSSMethod 该取值是否为 ssMethodCanonical 已登记的 SS 方法名(大小写不敏感)。
// 用作"畸形 method"的识别判据 —— 只有解出来确实是个合法方法名才做还原。
func isKnownSSMethod(m string) bool {
	_, ok := ssMethodCanonical[strings.ToLower(strings.TrimSpace(m))]
	return ok
}

// singboxUTLSFingerprints sing-box 认可的 uTLS 指纹全集。
// 来源: sing-box option/uTLSFingerprint 的常量表。
var singboxUTLSFingerprints = map[string]bool{
	"chrome": true, "firefox": true, "safari": true, "ios": true,
	"android": true, "edge": true, "360": true, "qq": true,
	"random": true, "randomized": true,
}

// normalizeUTLSFingerprint 归一 uTLS 指纹; 不认识返回 ""(调用方据此省略 utls 块)。
// 大小写不敏感: 订阅里 "Chrome" / "CHROME" 都存在。
func normalizeUTLSFingerprint(fp string) string {
	f := strings.ToLower(strings.TrimSpace(fp))
	if f == "" || !singboxUTLSFingerprints[f] {
		return ""
	}
	return f
}

// normalizeVLESSFlow 归一 VLESS flow。返回 "" 表示"不设 flow"(等价 none)。
//
// xtls-rprx-vision-udp443 是 xtls-rprx-vision 的旧写法(多带 UDP443 语义),
// sing-box 只认后者。其余未知取值一律归空(宁可不设, 也不把上游不认的值发上去)。
func normalizeVLESSFlow(flow string) string {
	f := strings.ToLower(strings.TrimSpace(flow))
	switch f {
	case "":
		return ""
	case "xtls-rprx-vision", "xtls-rprx-vision-udp443":
		return "xtls-rprx-vision"
	default:
		return ""
	}
}

// isNoTransportMarker 该取值是否意为"没有传输层"(v2ray 用 tcp 表示裸 TCP)。
// 只有这类写法才可安全省略 transport 字段 —— 省略即等价语义。
func isNoTransportMarker(t string) bool {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "", "tcp", "raw":
		return true
	}
	return false
}

// isSingboxTransportType sing-box 支持的 transport.type 取值。
// 用于测试与文档, 不用于净化决策: 净化只剥 isNoTransportMarker, 其余未知取值
// 原样留给 sing-box 剔除(见 sanitizeOutboundShape 第 2 点)。
func isSingboxTransportType(t string) bool {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "http", "ws", "quic", "grpc", "httpupgrade":
		return true
	}
	return false
}

// sanitizeOutboundTLS 修正出站里会让 sing-box 直接崩溃的 TLS 写法。
//
// sing-box v1.14.0 的 vless / trojan 出站是这么写的:
//
//	if options.TLS != nil {
//	    outbound.tlsConfig, err = tls.NewClientWithOptions(...)  // Enabled=false 时返回 (nil, nil)
//	    outbound.tlsDialer = tls.NewDialer(dialer, outbound.tlsConfig)  // 却无条件建 dialer
//	}
//
// 于是"tls 对象存在但 enabled 不是 true"会构造出 config == nil 的 TLS dialer,
// 第一条连接走到 sing/common/tls.ClientHandshake 就空指针 panic —— 进程直接死
// (vmess 与各 transport 有 nil 保护, 不受影响; vless/trojan 没有)。
//
// 订阅方生成的 sing-box JSON 经常省略 enabled(只给 server_name / utls), 本意
// 显然是启用 TLS, 所以补 enabled=true; 只有显式 false 才把整块删掉(等价语义)。
func sanitizeOutboundTLS(ob map[string]any) {
	raw, ok := ob["tls"]
	if !ok {
		// anytls 在 sing-box 里强制要求 TLS, 缺块直接 "TLS required" 整条剔除;
		// 订阅下发的 raw JSON 经常缺这块(代理软件默认启用所以能连), 按 server 补默认块。
		// ponytail: 仅 anytls(线上实锤), tuic/hy2 等若出现同类剔除再加。
		if typ, _ := ob["type"].(string); typ == "anytls" {
			if server, _ := ob["server"].(string); server != "" {
				ob["tls"] = map[string]any{
					"enabled":     true,
					"server_name": server,
					"utls":        map[string]any{"enabled": true, "fingerprint": "chrome"},
				}
			}
		}
		return
	}
	if raw == nil {
		// 显式 null: 删掉, 别在配置里留一个 "tls": null
		delete(ob, "tls")
		return
	}
	block, ok := raw.(map[string]any)
	if !ok {
		delete(ob, "tls")
		return
	}
	if enabled, isBool := block["enabled"].(bool); isBool {
		if !enabled {
			delete(ob, "tls")
		}
		return
	}
	// 缺 enabled 字段(或类型不对): 按启用处理, 这是订阅的常见写法
	block["enabled"] = true
}

// nodeExcludedByFilter 订阅过滤管道(P2): 节点显示名命中排除关键词(不区分
// 大小写)即返回 true。关键词来自配置 nodeExcludeKeywords, 面板可编辑。
func nodeExcludedByFilter(displayName string) bool {
	kws := getZenConfig().NodeExcludeKeywords
	if len(kws) == 0 {
		return false
	}
	lower := strings.ToLower(displayName)
	for _, k := range kws {
		k = strings.ToLower(strings.TrimSpace(k))
		if k != "" && strings.Contains(lower, k) {
			return true
		}
	}
	return false
}
