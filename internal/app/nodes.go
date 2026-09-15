package app

import (
	"context"
	"flag"
	"fmt"
	"log"
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
func syncNodeBox() {
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
		failKeepOld(fmt.Errorf("启动失败: %v", err))
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
