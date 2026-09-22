package app

import (
	"cline-go-proxy/internal/kit"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"strings"
)

func loadZenConfig() *zenConfigData {
	path := kit.ResolveDataPath(".zen-config.json")
	cfg := defaultZenConfig()
	data, err := os.ReadFile(path)
	switch {
	case err != nil:
		// 首次运行没有文件属于正常情况, 其它读取错误要留记录。
		if !os.IsNotExist(err) {
			log.Printf("zen config read failed (%s): %v", path, err)
		}
	default:
		next := defaultZenConfig()
		if uerr := json.Unmarshal(data, next); uerr != nil {
			// json.Unmarshal 不是事务性的: 半截 JSON 会保留已经解出来的字段、
			// 丢掉其余部分(最典型的是 providers 整段消失), 得到的是一份
			// "看着正常但少了东西"的配置, 面板上完全看不出异常。
			// 宁可整体退回默认值, 并把坏文件改名留证。
			log.Printf("zen config parse failed (%s): %v; 退回默认配置", path, uerr)
			quarantineBadConfig(path)
		} else {
			cfg = next
		}
	}
	// P3-17: 先 TrimSpace 再兜底 —— 旧判定只查 == "" 会放行 " " 这种纯空白 key,
	// 落盘后 zenSelectKey TrimSpace 取到空串, 请求不带鉴权头 → 全量 401。
	cfg.Key = strings.TrimSpace(cfg.Key)
	if cfg.Key == "" {
		cfg.Key = "public"
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = zenAPIBase
	}
	// 旧配置迁移: 只有 baseURL 没有 baseURLs 时,按默认端点表填充(官方 + 镜像)
	if len(cfg.BaseURLs) == 0 {
		cfg.BaseURLs = defaultZenBaseURLs()
	}
	// 旧配置迁移: 出口模式与订阅刷新间隔后加, 缺失时回填默认值。
	// 出口模式默认走节点: 与 zens/cline 渠道既有行为一致(池为空时天然退回直连)。
	if cfg.ExitMode != exitModeDirect && cfg.ExitMode != exitModeProxy {
		cfg.ExitMode = exitModeProxy
	}
	if cfg.SubsRefreshMins <= 0 {
		cfg.SubsRefreshMins = defaultSubsRefreshMins
	}
	// 旧配置迁移: DNS 模式后加, 缺失时回填默认(DoH 阿里)。
	cfg.DNSMode = normalizeDNSMode(cfg.DNSMode)
	// 旧配置迁移: 通用 Provider 里 Google 的手填字段统一收归代码推导。
	for name, pc := range cfg.Providers {
		cfg.Providers[name] = normalizeProviderConfig(pc)
	}
	// schema 迁移链(P1-14): 载入即升级到当前版本; 发生实际变更时落盘一次,
	// 避免每次启动都重复迁移。
	if migrateZenConfig(cfg) {
		if err := kit.WriteFileAtomicDefault(path, mustJSONIndent(cfg)); err != nil {
			log.Printf("zen config: 迁移结果落盘失败(下次启动会重试): %v", err)
		} else {
			log.Printf("zen config: schema 已迁移并落盘 (v%d)", cfg.SchemaVersion)
		}
	}
	return cfg
}

// mustJSONIndent 缩进序列化; 失败返回 nil(调用方需判空)。
func mustJSONIndent(v any) []byte {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil
	}
	return b
}

func saveZenConfig() {
	zenConfigMu.Lock()
	defer zenConfigMu.Unlock()
	data, err := json.MarshalIndent(zenConfig, "", "  ")
	if err != nil {
		// marshal 失败绝不落盘: 写 nil 会清空 zen 配置, 宁可保留旧文件。
		log.Printf("zen config marshal failed: %v", err)
		return
	}
	if err := kit.WriteFileAtomicDefault(kit.ResolveDataPath(".zen-config.json"), data); err != nil {
		log.Printf("zen config save failed: %v", err)
	}
}

// getZenConfig 返回当前 zen 配置的深拷贝。
//
// 必须是克隆体而不是裸指针: 调用方遍布请求热路径(选出口、解析路由别名、
// 构造上游请求头、后台订阅刷新), 它们在锁外长时间持有引用。返回裸指针
// 等于让这些读与 mutateProvidersConfig 的写并发访问同一张 map, 会触发
// Go 运行时的 concurrent map read/write —— fatal error, recover 无效。
func getZenConfig() *zenConfigData {
	zenConfigMu.Lock()
	defer zenConfigMu.Unlock()
	return zenConfig.clone()
}

// setZenConfig 整体替换 zen 配置并落盘。
// 存入的是克隆体, 调用方之后再改自己那份不会影响全局。
func setZenConfig(c *zenConfigData) {
	zenConfigMu.Lock()
	zenConfig = c.clone()
	zenConfigMu.Unlock()
	applyZenConfigSideEffects()
}

// applyZenConfigSideEffects 配置变更后的锁外后处理(落盘 + 各缓存/单例刷新)。
// 统一由 setZenConfig / updateZenConfig 在**放锁之后**调用, 保持一致的锁/IO 纪律:
// 计算与赋值在临界区内, 慢 I/O 与重建在临界区外。
func applyZenConfigSideEffects() {
	// 勾选地区/代理/订阅都可能变, 出口列表缓存立即失效(否则最长 2 秒内还在用旧池)
	invalidateExitListCache()
	// 手动启用的模型集合也要同步: isZenFreeModel 热路径读的是缓存集合
	refreshZenEnabledModels()
	saveZenConfig()
	rebuildZenTransport()
	rebuildZenSem()
	syncNodeBox()
}

// updateZenConfig 在**同一临界区**内完成「读当前配置 → 修改 → 写回」(P3-16)。
//
// 此前 admin 面板的保存是两步: getZenConfig() 拿快照 → 锁外改几十行 →
// setZenConfig(next) 整体替换并落盘。读写窗口不在同一临界区, 多标签页/脚本
// 与 UI 并发保存时, 窗口期内另一方的修改被旧快照整体覆盖并落盘, 重启后仍丢失。
//
// 约定(与 mutateProvidersConfig 相同):
//   - 回调在持锁状态下执行, 里面**不得**再调用 getZenConfig / setZenConfig 等
//     配置读写函数(sync.Mutex 不可重入, 会自锁), 只做校验与纯内存改动;
//   - 回调返回非 nil 错误时不写回(配置保持原样), 错误原样返回给调用方;
//   - 落盘与单例刷新在放锁之后执行, 持锁期间不做慢 I/O。
func updateZenConfig(fn func(cfg *zenConfigData) error) error {
	zenConfigMu.Lock()
	next := zenConfig.clone()
	if next == nil {
		next = defaultZenConfig()
	}
	err := fn(next)
	if err == nil {
		zenConfig = next
	}
	zenConfigMu.Unlock()
	if err != nil {
		return err
	}
	applyZenConfigSideEffects()
	return nil
}

// validateProxyList 校验代理列表格式: http/https/socks5/socks5h 代理 URL,
// 以及 vmess/vless/trojan/ss/hy2/tuic 等节点链接。
// 节点链接解析失败只记录并跳过(同步节点时同样跳过), 不阻塞整批导入;
// 普通 http/socks5 代理仍严格校验。
func validateProxyList(proxies []string) error {
	for _, p := range proxies {
		line := strings.TrimSpace(p)
		if line == "" {
			continue
		}
		if isNodeLink(line) {
			if _, err := nodeOutbound(line, "validate"); err != nil {
				log.Printf("  代理列表: 节点解析失败已跳过: %v", err)
			}
			continue
		}
		if scheme, _, ok := strings.Cut(line, "://"); ok && (scheme == "naive" || strings.HasPrefix(scheme, "naive+")) {
			return fmt.Errorf("naive 节点 %q 不受支持: 其出站依赖 Chromium cronet 原生库, 无法随本程序纯 Go 构建", line)
		}
		u, err := url.Parse(line)
		if err != nil {
			return fmt.Errorf("代理格式无效 %q: %v", line, err)
		}
		switch u.Scheme {
		case "http", "https", "socks5", "socks5h":
		default:
			return fmt.Errorf("代理 %q 协议不受支持（支持 http/https/socks5/socks5h）", line)
		}
		if u.Host == "" {
			return fmt.Errorf("代理 %q 缺少 host:port", line)
		}
		if _, _, err := net.SplitHostPort(u.Host); err != nil {
			return fmt.Errorf("代理 %q 缺少端口: %v", line, err)
		}
	}
	return nil
}

// ============ zen 上游调用 ============
