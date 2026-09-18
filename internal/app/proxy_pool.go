package app

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"
	"golang.org/x/net/proxy"
)

var (
	zenHTTPClient  = &http.Client{Transport: buildZenTransport()}
	zenProxyCount  atomic.Uint64
	zenTransportMu sync.Mutex

	// zenProxyCooldowns 出口冷却表: **稳定出口标识 -> 冷却截止**。
	//
	// 键必须是出口自身的稳定标识, 不能用 effectiveProxyList() 的下标。
	// 下标不是出口的属性 —— 列表是每次调用现场拼的(cfg.Proxies +
	// subNodeKeysSnapshot() + filterByExitRegion), 订阅刷新会重建节点快照,
	// 地区过滤开关会改变列表长度。任一种变化都让下标整体漂移, 于是此前写进去的
	// 冷却时间会落在**别的出口**上: 真正出问题的出口留在轮询里继续被选中,
	// 表现是"时好时坏、无法复现"(2026-09-17 审查 P0-3)。
	zenProxyCooldowns   = map[string]time.Time{}
	zenProxyCooldownsMu sync.Mutex
)

// zenProxyCooldownKey 出口的稳定冷却键。
// 节点出口用 nodeLocalKey(本地入站地址, 由订阅条目确定性派生);
// 普通代理用 URL 本身。两者都与列表位置无关。
func zenProxyCooldownKey(proxy string) string {
	if proxy == "" {
		return ""
	}
	if key := nodeLocalKey(proxy); key != "" {
		return key
	}
	return proxy
}

// cooldownZenProxy 标记某出口冷却,冷却期内轮询跳过。
func cooldownZenProxy(proxy string, d time.Duration) {
	key := zenProxyCooldownKey(proxy)
	if key == "" {
		return
	}
	if d <= 0 {
		d = 10 * time.Minute
	}
	zenProxyCooldownsMu.Lock()
	zenProxyCooldowns[key] = time.Now().Add(d)
	zenProxyCooldownsMu.Unlock()
}

// cooldownZenProxyByIndex 按**当前**列表下标解析出出口再冷却。
//
// 仅保留给"手上只有轮询位置"的旧调用点。下标在这里只用于现场定位是哪个出口,
// 落库的键仍是出口标识 —— 所以列表之后重排也不会让这条冷却漂到别的出口上。
// 新增调用点应优先直接传出口标识。
func cooldownZenProxyByIndex(idx int, d time.Duration) {
	if idx < 0 {
		return
	}
	list := effectiveProxyList()
	if idx >= len(list) {
		return
	}
	cooldownZenProxy(list[idx], d)
}

// cooldownActualExit 冷却本次请求真实使用的出口(由拨号层经 ctx 回写),
// 而不是全局轮询位置 —— 后者可能属于别的并发请求, 冷却它会误伤。
func cooldownActualExit(ctx context.Context, d time.Duration) {
	key := reqExitKey(ctx)
	if key == "" {
		return // 直连没有可冷却的出口
	}
	// reqExitKey 本身就是出口标识, 直接入表, 不再按下标反查。
	cooldownZenProxy(key, d)
}

func zenProxyAvailable(proxy string) bool {
	key := zenProxyCooldownKey(proxy)
	if key == "" {
		return true
	}
	zenProxyCooldownsMu.Lock()
	defer zenProxyCooldownsMu.Unlock()
	until, ok := zenProxyCooldowns[key]
	if !ok {
		return true
	}
	if time.Now().After(until) {
		delete(zenProxyCooldowns, key)
		return true
	}
	return false
}

// zenProxyCooldownStatus 冷却中的出口 -> 截止时刻(供管理面板展示)。
//
// 展示键用出口 URL 本身(面板上认得出是哪个节点), 而不是内部冷却键;
// 池里已经不存在的出口(订阅刷新后消失)保留内部键, 以免冷却信息在面板上凭空消失。
func zenProxyCooldownStatus() map[string]string {
	list := effectiveProxyList()
	display := make(map[string]string, len(list))
	for _, p := range list {
		if k := zenProxyCooldownKey(p); k != "" {
			display[k] = p
		}
	}
	now := time.Now()
	zenProxyCooldownsMu.Lock()
	defer zenProxyCooldownsMu.Unlock()
	out := map[string]string{}
	for key, until := range zenProxyCooldowns {
		if !now.Before(until) {
			continue
		}
		label := key
		if p, ok := display[key]; ok {
			label = p
		}
		out[label] = until.Format("15:04:05")
	}
	return out
}

// ============ 出口额度冷却(429 专用, 指数升级) ============

// 背景: opencode 的免费额度按**出口 IP** 计 —— 一个 IP 用完了就 429, 换一个
// IP 就能继续用(用户实测)。所以 429 冷却的对象是**出口**, 不是候选(模型)。
//
// 为什么用指数升级而不是固定时长: 额度**重置窗口未知**(标 `[未知]`, 见
// docs/opencode-zen-facts.md 第 4 节)。固定 10 分钟太短 —— 若实际按日重置,
// 到期后再试同一 IP 还是 429, 白烧一次尝试; 固定 24 小时又太长 —— 若实际按
// 小时重置, 出口白闲置一整天。
//
// 升级策略不依赖知道窗口, 自己能收敛:
//
//	连续第 1 次 429 → 10 分钟
//	连续第 2 次      → 20 分钟
//	连续第 3 次      → 40 分钟 … 上限 6 小时
//	该出口成功一次   → 计数清零, 回到 10 分钟
//
// 这样: 额度若确实 10 分钟就恢复, 出口很快回来; 若按日重置, 计数会升级并稳定
// 在上限。两种情况都不需要人工配置。
const (
	zenQuotaCooldownBase = 10 * time.Minute
	zenQuotaCooldownCap  = 6 * time.Hour
	// zenQuotaRetryAfterCap Retry-After 的独立上限。
	//
	// ★ 为什么不能沿用 zenQuotaCooldownCap(6h): Retry-After 是**上游亲口给的**
	// 重置时刻, 权威性高于我们的指数猜测。实测(2026-09-18, 部署实例日志)两次采样:
	//
	//	13:06:25  retry 1/6 after 18h53m35s  → 次日 08:00:00
	//	14:12:36  retry 1/6 after 17h47m25s  → 次日 08:00:01
	//
	// 两次独立采样都精确落在次日 08:00(= 00:00 UTC) —— 额度按日重置。
	// 旧实现在最后无条件 `if d > zenQuotaCooldownCap { d = cap }`, 把 18h53m
	// 截成 6h: 6 小时后这个**已知耗尽到明天**的出口会被放回池子, 再撞一次 429,
	// 13 小时内反复打同一个已耗尽的 IP —— 既是白烧尝试, 也毫无必要地增加风控面。
	//
	// 上限仍然保留(24h), 但只用于挡**异常值**(上游返回离谱数字时别把出口永久锁死)。
	zenQuotaRetryAfterCap = 24 * time.Hour
)

// zenProxyQuotaStrikes 出口的连续 429 计数(与冷却表共用 zenProxyCooldownsMu)。
var zenProxyQuotaStrikes = map[string]int{}

// cooldownZenProxyQuota 429 时冷却出口, 时长按连续命中次数指数升级。
//
// retryAfter > 0 时取它与升级时长的**较大者** —— 上游明确说了等多久就听它的,
// 但绝不因此缩短冷却(缩短只会换来又一次 429)。
//
// 两者各有独立上限(见 zenQuotaRetryAfterCap 的实测依据): Retry-After 是权威值,
// 上限只挡异常；指数升级那条是我们自己的猜测, 上限沿用 zenQuotaCooldownCap。
//
// 返回实际冷却时长(0 表示没有可冷却的出口, 如直连)。
func cooldownZenProxyQuota(proxy string, retryAfter time.Duration) time.Duration {
	key := zenProxyCooldownKey(proxy)
	if key == "" {
		return 0
	}
	zenProxyCooldownsMu.Lock()
	defer zenProxyCooldownsMu.Unlock()
	n := zenProxyQuotaStrikes[key] + 1
	zenProxyQuotaStrikes[key] = n
	d := zenQuotaCooldownBase
	for i := 1; i < n && d < zenQuotaCooldownCap; i++ {
		d *= 2
	}
	if d > zenQuotaCooldownCap {
		d = zenQuotaCooldownCap
	}
	if retryAfter > 0 {
		ra := retryAfter
		if ra > zenQuotaRetryAfterCap {
			ra = zenQuotaRetryAfterCap
		}
		if ra > d {
			d = ra
		}
	}
	zenProxyCooldowns[key] = time.Now().Add(d)
	return d
}

// clearZenProxyQuotaStrike 该出口成功一次 → 连续 429 计数清零。
// 不清零的话计数会单调递增, 出口一旦被限流就再也回不到短冷却。
func clearZenProxyQuotaStrike(proxy string) {
	key := zenProxyCooldownKey(proxy)
	if key == "" {
		return
	}
	zenProxyCooldownsMu.Lock()
	defer zenProxyCooldownsMu.Unlock()
	delete(zenProxyQuotaStrikes, key)
}

// cooldownActualExitQuota 429 专用: 冷却本次请求**真实使用**的出口。
//
// 用 reqExitKey(ctx) 而不是全局轮询位置 —— 后者可能属于别的并发请求, 冷却它会
// 误伤(与 cooldownActualExit 同一理由)。
func cooldownActualExitQuota(ctx context.Context, retryAfter time.Duration) time.Duration {
	key := reqExitKey(ctx)
	if key == "" {
		return 0 // 直连没有可冷却的出口
	}
	return cooldownZenProxyQuota(key, retryAfter)
}

// clearActualExitQuotaStrike 本次请求真实使用的出口成功了 → 清零它的 429 计数。
func clearActualExitQuotaStrike(ctx context.Context) {
	if key := reqExitKey(ctx); key != "" {
		clearZenProxyQuotaStrike(key)
	}
}

// rebuildZenTransport 代理池或配置变化时重建 zen 上游 HTTP 客户端。
//
// 必须显式关掉旧 transport 的空闲连接: 它持有已经建好的 TCP/TLS 连接,
// 直接丢弃引用会让这些连接无人回收。setZenConfig 每次保存配置都会走到这里,
// 所以"反复改配置"就是成批泄漏连接。
func rebuildZenTransport() {
	zenTransportMu.Lock()
	if old, ok := zenHTTPClient.Transport.(*http.Transport); ok {
		old.CloseIdleConnections()
	}
	zenHTTPClient = &http.Client{Transport: buildZenTransport()}
	zenTransportMu.Unlock()
}

func getZenHTTPClient() *http.Client {
	zenTransportMu.Lock()
	defer zenTransportMu.Unlock()
	return zenHTTPClient
}

// directHTTPClient 真正的直连客户端: 拨号器里没有出口决策, 不经过节点、也不经过
// catch-all 入站。专供"控制面"请求(目录/模型同步这类必须拿到结果、且对出口地区不
// 敏感的调用)做兜底。
//
// 为什么需要它(2026-09-16 实证): 出口池近乎全挂时(exitReachable 38/4482), 所有经
// 出口的请求都是 `socks5: general SOCKS server failure`, 而 opencode.ai 直连 1.3s
// 就 200 —— 于是"拉取上游免费模型"整条链路瘫掉, 面板只能吃 65 个模型的历史缓存。
// 此前 catalogFallbackClient() 返回的是同一个出口客户端, 所谓"直连兜底"等于再撞一次
// 同样的死节点(日志里那句"直连兜底也失败: socks5: ..."就是这么来的)。
func directHTTPClient() *http.Client {
	directTransportOnce.Do(func() {
		directTransport = &http.Transport{
			DialContext:         (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			MaxIdleConns:        16,
			MaxIdleConnsPerHost: 4,
			IdleConnTimeout:     90 * time.Second,
			TLSHandshakeTimeout: 15 * time.Second,
			ForceAttemptHTTP2:   true,
		}
	})
	directClientOnce.Do(func() {
		// 控制面兜底客户端: 给总超时, 避免目录/模型同步在慢响应或僵持连接上
		// 无限挂起(拨号与 TLS 已有超时, 但整请求没有上限)。
		// 数据面请求不走这个客户端, 加超时不影响长流。
		directClient = &http.Client{Transport: directTransport, Timeout: 60 * time.Second}
	})
	directClientOnceMu.Lock()
	defer directClientOnceMu.Unlock()
	return directClient
}

var (
	directTransportOnce sync.Once
	directTransport     *http.Transport
	directClientOnce    sync.Once
	directClient        *http.Client
	directClientOnceMu  sync.Mutex
)

// effectiveProxyList 生效的出口列表: 手动代理/节点 + 订阅解析出的节点
func effectiveProxyList() []string {
	cfg := getZenConfig()
	list := make([]string, 0, len(cfg.Proxies)+len(cfg.Subs)*4)
	list = append(list, cfg.Proxies...)
	list = append(list, subNodeKeysSnapshot()...)
	// 地区过滤: 用户在设置页勾选地区后, 全网关出站只走所选地区的出口。
	return filterByExitRegion(list)
}

// nodeDialable 节点出口是否已就绪(普通代理恒为可拨)
func nodeDialable(p string) bool {
	if !isNodeLink(p) {
		return true
	}
	return nodeLocalAddr(p) != ""
}

// ============ 全局出口模式 ============
//
// 出口模式作用于整个网关: zen(cline 池 / opencode / 通用 Provider) 的所有上游
// 请求与订阅抓取共用同一个出口决策。direct = 全部直连; proxy = 全部走
// 节点列表里的代理/节点出口(pickZenProxy 内部仍会跳过冷却与不可达节点)。

const (
	exitModeDirect = "direct"
	exitModeProxy  = "proxy"
)

// exitModeDirectNow 当前是否为直连模式。
func exitModeDirectNow() bool {
	return getZenConfig().ExitMode == exitModeDirect
}

// pickZenProxy 按策略选择代理,返回 (代理URL, 索引);无代理返回 ("", -1)。
// 跳过冷却中或未就绪的节点;全部不可用时返回直连。
// 每次调用递增计数,保证 round_robin 顺序与日志索引一致。
func pickZenProxy() (string, int) {
	return pickZenProxyWhere(nil)
}

// pickZenProxyWhere 在 pickZenProxy 的基础上追加一个候选过滤条件。
// extra 为 nil 时等价于原行为; 上游可达性过滤(上游对出口地区有要求时)走这里。
//
// 策略(P2 出口选路增强, 参照 easy_proxies/mihomo url-test/glider lha):
//
//	round_robin(默认) / random / fill / **latency**(实测延迟最低优先 + 1.2 倍
//	容差防抖)。所有策略都会跳过: 冷却中 / 未就绪 / 检测不可达 / 人工拉黑 /
//	extra 过滤的出口。
func pickZenProxyWhere(extra func(p string) bool) (string, int) {
	if exitModeDirectNow() {
		return "", -1
	}
	list := effectiveProxyList()
	n := len(list)
	if n == 0 {
		return "", -1
	}
	// 先收集全部可用候选, 再按策略挑选(替代旧的"起点+线性探测"写法,
	// 语义相同但 latency 策略需要完整的候选集合)。
	avail := make([]int, 0, n)
	for i := 0; i < n; i++ {
		if zenProxyAvailable(list[i]) && nodeDialable(list[i]) && nodeUsable(list[i]) &&
			!nodeManuallyBlacklisted(nodeLocalKey(list[i])) &&
			(extra == nil || extra(list[i])) {
			avail = append(avail, i)
		}
	}
	if len(avail) == 0 {
		// 整池都不满足条件时返回直连决策(交给上层), 不退而求其次。
		return "", -1
	}
	idx := int(zenProxyCount.Add(1)-1) % len(avail)
	switch getZenConfig().ProxyStrategy {
	case "random":
		idx = avail[int(time.Now().UnixNano()%int64(len(avail)))]
	case "fill":
		idx = avail[0]
	case "latency":
		idx = latencyPick(list, avail)
	default: // round_robin: 按可用候选顺序轮转(保持与日志索引一致)
		idx = avail[idx]
	}
	return list[idx], idx
}

// pickUnifiedExit 统一出口选择: 地区不再做全局特例, 按常规轮询选出口。
// 空池/直连模式返回 ("", -1), 由 zenDialContext 按 rescueDirect 决定 fail-closed。
// 开启粘性会话(P2, Resin 思路)时: 同一客户端来源 IP 在 TTL 内复用同一出口
// (出口仍须通过健康复核), 服务于"同 IP 连续请求"的上游场景。
func pickUnifiedExit(ctx context.Context, modelID string) (string, int) {
	if exitModeDirectNow() {
		return "", -1
	}
	clientIP := ""
	if tr := traceFrom(ctx); tr != nil {
		clientIP = tr.ClientIP
	}
	// ★ 2026-09-17 审查 P1-1: 模型相关过滤(上游可达性 + 地区能力探测结果)
	// 此前在生产路径上**完全没生效** —— 这里传的是空操作过滤器, 而唯一会读
	// regionNodeOK 的 pickZenProxyForModel 生产零调用。现在两条路径共用同一份判据。
	modelFilter := exitFilterForModel(modelID)
	list := effectiveProxyList()
	if stickySessionEnabled() && clientIP != "" && len(list) > 0 {
		if pin, ok := stickyPinFor(clientIP); ok {
			// 健康复核: 钉住的出口必须仍在池里且可用, 否则按正常选路走
			for i, p := range list {
				if p == pin.Proxy && zenProxyAvailable(p) && nodeDialable(p) && nodeUsable(p) &&
					!nodeManuallyBlacklisted(nodeLocalKey(p)) && modelFilter(p) {
					stickyPinSave(clientIP, p, nodeLocalKey(p)) // 续期
					return p, i
				}
			}
			stickyMu.Lock()
			delete(stickyPinMap, clientIP)
			stickyMu.Unlock()
		}
	}
	// 走模型相关选路(内部已含 exitFilterForModel): 地区受限模型会避开
	// 已探测确认被拒的出口, 全被拒时退回常规轮询而不是直连。
	p, idx := pickZenProxyForModel(modelID)
	if p != "" {
		if stickySessionEnabled() && clientIP != "" {
			stickyPinSave(clientIP, p, nodeLocalKey(p))
		}
		return p, idx
	}
	return "", -1
}

// nodeUsable 已检测为不可达的节点不再参与轮询, 未检测的按可用处理。
// 连通检测结果需要这层过滤才生效: 否则轮询会持续撞上失效节点。
func nodeUsable(p string) bool {
	if !isNodeLink(p) {
		return true
	}
	key := nodeLocalKey(p)
	if healthOf(key) == "fail" {
		return false
	}
	// 出口级去重(P2): 同出口 IP 的折叠副本不参与选路(主力仍健康时);
	// 主力劣化后下一轮折叠会重新选举, 本节点自动转正。
	if nodeFoldedDuplicate(key) {
		return false
	}
	return true
}

// lastZenProxyIdx 最近一次选择的代理索引(日志用)
func lastZenProxyIdx() int {
	v := int64(zenProxyCount.Load())
	if v <= 0 {
		return -1
	}
	return int((v - 1) % int64(max(1, len(effectiveProxyList()))))
}

func maskProxyURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	u.User = url.User("***")
	return u.String()
}

// maskURLForLog 专用于日志: maskProxyURL 只遮 User, 但订阅链接更常见的是
// ?token=xxx / #password=xxx 这类查询参数与片段 —— 那些它一概不遮。
//
// 日志文件在 data/ 下, 常被云同步盘和一键备份整目录收走, 一条落盘等于
// 长期访问权限外泄。所以这里把 User、查询参数、片段全部替换, 只留
// scheme+host+path 让人能认出是哪条订阅。
func maskURLForLog(raw string) string {
	if raw == "" {
		return raw
	}
	if u, err := url.Parse(raw); err == nil && u.Scheme != "" && u.Host != "" {
		if u.User != nil {
			u.User = url.User("***")
		}
		if u.RawQuery != "" {
			u.RawQuery = "***"
		}
		if u.RawFragment != "" {
			u.RawFragment = "***"
		}
		return u.String()
	}
	// 非标准 URL(vmess://vless:// 这类把凭据编码进整条字符串的写法):
	// 只留 scheme, 其余整段遮掉。日志需要的是"哪条订阅出了问题", 不是内容。
	if i := strings.IndexAny(raw, "://"); i >= 0 {
		return raw[:i+3] + "***"
	}
	return "***"
}

func buildZenTransport() *http.Transport {
	t := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
		// 分层超时(P1-11): TLS 握手与响应头阶段单独设限, 不再依赖客户端
		// 自己的超时兜底 —— 上游卡死握手/卡死响应头时, 网关能主动断开并
		// 让链路换下一站。正文阶段不设限(流式回答可以持续很久)。
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 120 * time.Second,
		DisableCompression:    false,
	}
	t.DialContext = zenDialContext
	// https 走 HTTP/2 + uTLS Chrome 指纹: 完整浏览器指纹(含 h2),避免 Go 原生指纹被 CF 风控
	t.RegisterProtocol("https", zenHTTP2Transport())
	return t
}

func zenHTTP2Transport() *http2.Transport {
	return &http2.Transport{
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			raw, err := zenDialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				raw.Close()
				return nil, err
			}
			uconn := utls.UClient(raw, &utls.Config{
				ServerName: host,
				NextProtos: []string{"h2", "http/1.1"},
			}, utls.HelloChrome_120)
			if err := uconn.HandshakeContext(ctx); err != nil {
				raw.Close()
				return nil, err
			}
			return uconn, nil
		},
	}
}

// reqExit 单次请求实际使用的出口, 由日志中间件经 request context 收集
type reqExit struct {
	name string // 展示用
	key  string // 原始出口标识(节点链接/代理 URL), 反馈环据此标记
}

// reqExitKey 拨号层实际使用的出口标识; 空串表示直连。
func reqExitKey(ctx context.Context) string {
	info, ok := ctx.Value(ctxKeyReqExit).(*reqExit)
	if !ok {
		return ""
	}
	return info.key
}

type ctxKeyReqExitType struct{}

var ctxKeyReqExit = ctxKeyReqExitType{}

// setReqExit 把本次请求选择的出口写入请求上下文(若存在)
func setReqExit(ctx context.Context, proxy string) {
	info, ok := ctx.Value(ctxKeyReqExit).(*reqExit)
	if !ok {
		return
	}
	info.key = proxy
	switch {
	case proxy == "":
		info.name = "直连"
	case isNodeLink(proxy):
		info.name = "节点: " + nodeDisplayName(proxy)
	default:
		info.name = "代理: " + maskProxyURL(proxy)
	}
}

// describeEffectiveExit 当前代理池最近一次轮换命中的出口描述(请求日志回填用)
func describeEffectiveExit() string {
	list := effectiveProxyList()
	if len(list) == 0 {
		return ""
	}
	idx := lastZenProxyIdx()
	if idx < 0 || idx >= len(list) {
		return ""
	}
	p := list[idx]
	switch {
	case p == "":
		return "直连"
	case isNodeLink(p):
		return "节点: " + nodeDisplayName(p)
	default:
		return "代理: " + maskProxyURL(p)
	}
}

// maxDialExitCandidates 单次拨号内最多依次尝试几个候选出口。
//
// 为什么要"一次多试几个": 上层确实是"失败→换下一个出口重试"的循环(retries 默认 6,
// 每次重新选路), 但出口池的质量可能极差 —— 实测 4482 个节点只有 38 个真的可达,
// 而健康表仍把 3831 个标成可用。一次只拨一个节点时, 固定的重申次数会在死节点区里
// 耗尽, 用户侧表现为"节点明明一堆, 就是连不上"。
// 死节点的拨号失败很快(握手即拒), 所以多试几个的代价很小。
const maxDialExitCandidates = 3

// zenDialContext 网关全部出站的唯一拨号入口。
//
// 出口策略(B 方案: 模式跟随):
//  1. 依次尝试至多 maxDialExitCandidates 个候选出口(每次失败立即冷却该节点,
//     后续请求自动跳过), 任一成功即返回;
//  2. 候选耗尽(或池里没有可用节点)后按"节点全挂兜底"开关决定是否继续:
//     catch-all 入站(直连模式下由 sing-box 走 direct 出站) → Go 原生直连;
//  3. 直连兜底被显式关闭时, 直接返回最后一次错误。
//
// 关键点: **不再"选到一个节点就只试它"** —— 选路依据的是可能过期的健康数据,
// 而拨号层是唯一能拿到真实结果的地方, 所以由它兜住"节点看着健康、实际已死"。
func zenDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	modelID, _ := ctx.Value(ctxKeyZenModel).(string)

	var lastErr error
	for i := 0; i < maxDialExitCandidates; i++ {
		p, _ := pickUnifiedExit(ctx, modelID)
		if p == "" {
			break // 池里没有可用节点: 交给下面的兜底链
		}
		setReqExit(ctx, p)
		conn, err := dialViaProxy(ctx, p, network, addr)
		if err == nil {
			return conn, nil
		}
		lastErr = err
		cooldownActualExit(ctx, 2*time.Minute)
		log.Printf("  exit: 出口拨号失败(%v) — 已冷却并换下一个(第 %d/%d 个候选)",
			err, i+1, maxDialExitCandidates)
		if ctx.Err() != nil {
			return nil, lastErr // 客户端已断开: 不再继续试
		}
	}

	// 代理模式但一个可用节点都没有: 是否允许直连兜底由配置决定。
	if !exitModeDirectNow() && !rescueDirectEnabled() {
		if lastErr != nil {
			return nil, lastErr
		}
		return nil, fmt.Errorf("没有可用节点，且已禁用直连兜底")
	}
	if !exitModeDirectNow() {
		// 显式直连模式不需要这条日志(那是正当配置); 只有"代理模式节点全挂、
		// 悄悄走直连"才要大声打出来, 否则用户看到行为异常却无从排查
		// (2026-09-17 审查 R2-9)。
		log.Printf("  exit: 节点池无可用出口, 触发直连兜底(rescueDirect=%v, 可在 zen 设置里关闭)", rescueDirectEnabled())
	}
	if local := catchAllLocalAddr(); local != "" {
		u := &url.URL{Scheme: "socks5", Host: local}
		conn, err := dialSOCKS5(ctx, u, network, addr)
		if err == nil {
			return conn, nil
		}
		lastErr = err
		log.Printf("  exit: catch-all 拨号失败(%v), 回退 Go 原生直连", err)
	}
	// 保命路径: sing-box 实例不可用时不能让整个网关失去联网能力。
	d := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	conn, err := d.DialContext(ctx, network, addr)
	if err == nil {
		return conn, nil
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, err
}

// rescueDirectEnabled 节点全部不可用时是否允许直连兜底(缺省 true)。
// 用指针区分"未设置"与"显式关闭": 旧配置文件没有这个字段, 不能因此变成禁用。
func rescueDirectEnabled() bool {
	cfg := getZenConfig()
	if cfg == nil || cfg.RescueDirect == nil {
		return true
	}
	return *cfg.RescueDirect
}

// dialViaProxy 统一拨号:http/https 走 CONNECT,socks5 走 SOCKS5 握手,
// vmess/vless/trojan/ss/hy2/tuic 节点经内嵌 sing-box 的本地入站转发
func dialViaProxy(ctx context.Context, raw, network, addr string) (net.Conn, error) {
	if isNodeLink(raw) {
		return dialNodeProxy(ctx, raw, network, addr)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("bad proxy url: %w", err)
	}
	switch u.Scheme {
	case "http", "https":
		return dialHTTPProxy(ctx, u, network, addr)
	case "socks5", "socks5h", "socks":
		auth := &proxy.Auth{}
		if u.User != nil {
			auth.User = u.User.Username()
			auth.Password, _ = u.User.Password()
		}
		d, err := proxy.SOCKS5("tcp", u.Host, auth, proxy.Direct)
		if err != nil {
			return nil, err
		}
		type ctxDialer interface {
			DialContext(context.Context, string, string) (net.Conn, error)
		}
		if cd, ok := d.(ctxDialer); ok {
			return cd.DialContext(ctx, network, addr)
		}
		// 旧接口无 ctx:包装
		type result struct {
			c   net.Conn
			err error
		}
		ch := make(chan result, 1)
		go func() {
			c, err := d.Dial(network, addr)
			ch <- result{c, err}
		}()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case r := <-ch:
			return r.c, r.err
		}
	default:
		return nil, fmt.Errorf("unsupported proxy scheme %q", u.Scheme)
	}
}

// dialHTTPProxy 通过 http(s) 代理建立 CONNECT 隧道
func dialHTTPProxy(ctx context.Context, u *url.URL, network, addr string) (net.Conn, error) {
	d := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	rawConn, err := d.DialContext(ctx, "tcp", u.Host)
	if err != nil {
		return nil, err
	}
	if u.Scheme == "https" {
		tlsConn := tls.Client(rawConn, &tls.Config{MinVersion: tls.VersionTLS12, ServerName: u.Hostname()})
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			rawConn.Close()
			return nil, err
		}
		rawConn = tlsConn
	}

	req := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Opaque: addr},
		Host:   addr,
		Header: make(http.Header),
	}
	if u.User != nil {
		cred := base64.StdEncoding.EncodeToString([]byte(u.User.String()))
		req.Header.Set("Proxy-Authorization", "Basic "+cred)
	}
	if err := req.Write(rawConn); err != nil {
		rawConn.Close()
		return nil, err
	}

	br := bufio.NewReader(rawConn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		rawConn.Close()
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		rawConn.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("proxy CONNECT %s: %s %s", u.Host, resp.Status, strings.TrimSpace(string(b)))
	}
	return rawConn, nil
}
