package app

// 配置对象的深拷贝与统一写入口。
//
// 背景: getZenConfig() / getProxyConfig() 此前是"锁内返回裸指针、锁外被读",
// 而 mutateProvidersConfig 又原地改同一个对象。请求转发过程中并发保存配置
// (或后台请求头自动同步) 会命中 Go 运行时的 concurrent map read/write ——
// 这是 fatal error, recover 拦不住, 整个进程直接退出; windowsgui 构建下
// 没有控制台, 用户只看到窗口凭空消失。
//
// 现在统一成:
//   - 读: getZenConfig() / getProxyConfig() 一律返回克隆体, 调用方怎么用都不会
//     碰到全局那一份, 也不需要再关心锁。
//   - 写: 只能走 mutateProvidersConfig / mutateProxyConfig —— 锁内克隆 →
//     回调修改克隆 → 整体替换 → 落盘。
//
// 写成"整体替换"而不是"就地改字段"还有一个重要原因: 每个 handler 都基于
// 完整的当前配置做修改。此前 admin_zen.go 的保存逻辑是手工重建一个
// zenConfigData 字面量, 只列出自己想得到的 16 个字段, 其余 7 个字段
// (Routes/Router/Usage/CooldownMs/DNSMode/DNSCustomDNS/RescueDirect)
// 被零值静默覆盖并落盘 —— 用户在 zen 设置页改任意一项, 候选链与自动路由
// 配置就没了。基于克隆改就不存在"漏字段"这种可能。

// cloneBoolPtr 复制 *bool, 保留 nil 与 true/false 的区别。
// RescueDirect / Enabled 都靠 nil 表达"未设置, 取默认值", 不能压成 false。
func cloneBoolPtr(p *bool) *bool {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

// cloneStrings 复制字符串切片, 保留 nil 与空切片的区别(落盘 omitempty 依赖它)。
func cloneStrings(in []string) []string {
	if in == nil {
		return nil
	}
	return append(make([]string, 0, len(in)), in...)
}

// providerHeaderSpec / providerAPIKey / providerModelEntry / zenCompactConfig
// 都是纯值类型, 复制即深拷贝, 无需逐个展开。
func (h providerHeaderSpec) clone() providerHeaderSpec   { return h }
func (k providerAPIKey) clone() providerAPIKey           { return k }
func (m providerModelEntry) clone() providerModelEntry   { return m }
func (c zenCompactConfig) clone() zenCompactConfig       { return c }

func (c providerConfig) clone() providerConfig {
	out := c
	out.Enabled = cloneBoolPtr(c.Enabled)
	out.FreeModels = cloneStrings(c.FreeModels)
	out.DisabledModels = cloneStrings(c.DisabledModels)
	if c.Headers != nil {
		out.Headers = make(map[string]providerHeaderSpec, len(c.Headers))
		for k, v := range c.Headers {
			out.Headers[k] = v.clone()
		}
	}
	if c.APIKeys != nil {
		out.APIKeys = make([]providerAPIKey, len(c.APIKeys))
		for i, k := range c.APIKeys {
			out.APIKeys[i] = k.clone()
		}
	}
	if c.Models != nil {
		out.Models = make([]providerModelEntry, len(c.Models))
		for i, m := range c.Models {
			out.Models[i] = m.clone()
		}
	}
	return out
}

func (u zenUsageConfig) clone() zenUsageConfig {
	out := u
	if u.DailyLimits != nil {
		out.DailyLimits = make(map[string]int, len(u.DailyLimits))
		for k, v := range u.DailyLimits {
			out.DailyLimits[k] = v
		}
	}
	return out
}

func (r zenRouterConfig) clone() zenRouterConfig {
	out := r
	out.Providers = cloneStrings(r.Providers)
	return out
}

// clone 深拷贝整份 zen 配置; 接收者为 nil 时返回 nil。
func (c *zenConfigData) clone() *zenConfigData {
	if c == nil {
		return nil
	}
	out := *c
	out.BaseURLs = cloneStrings(c.BaseURLs)
	out.Proxies = cloneStrings(c.Proxies)
	out.Subs = cloneStrings(c.Subs)
	out.RescueDirect = cloneBoolPtr(c.RescueDirect)
	out.Compaction = c.Compaction.clone()
	out.Usage = c.Usage.clone()
	out.Router = c.Router.clone()
	if c.Providers != nil {
		out.Providers = make(map[string]providerConfig, len(c.Providers))
		for k, v := range c.Providers {
			out.Providers[k] = v.clone()
		}
	}
	if c.Routes != nil {
		out.Routes = make(map[string][]string, len(c.Routes))
		for k, v := range c.Routes {
			out.Routes[k] = cloneStrings(v)
		}
	}
	if c.CooldownMs != nil {
		out.CooldownMs = make(map[string]int64, len(c.CooldownMs))
		for k, v := range c.CooldownMs {
			out.CooldownMs[k] = v
		}
	}
	return &out
}

// clone 深拷贝整份客户端配置(策略 + 请求头)。
func (c *proxyConfigData) clone() *proxyConfigData {
	if c == nil {
		return nil
	}
	out := *c
	if c.Headers != nil {
		out.Headers = make(map[string]string, len(c.Headers))
		for k, v := range c.Headers {
			out.Headers[k] = v
		}
	}
	return &out
}
