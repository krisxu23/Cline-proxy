package app

// 错误规则表 (P1-10, 参照 OmniRoute 的 config/errorConfig.ts + providerErrorRules.ts):
//
// 上游的错误表达五花八门 —— 状态码相同的 403 可能是"地区封锁"(该换出口/长冷却)
// 也可能是"key 被拒"(短冷却也没用), 只看状态码会把这些混为一谈。
// 规则表按"正文特征优先于状态码"的顺序匹配, 命中即返回专用类别;
// 反误伤豁免(exclude)先于 patterns 检查 —— 例如 Cloudflare 的 1010 指纹拒绝
// 长得像地区文案, 但它其实是"客户端指纹被拒", 长冷却毫无意义。
//
// 纪律: 新规则必须带测试(见 error_rules_test.go), 且 patterns 一律小写匹配。

import (
	"strings"
	"time"
)

// classGeoBlocked 地区封锁: 该出口所在地区不被上游接受。
// 冷却 24 小时(OmniRoute 同款分档): 换出口可能立刻可用, 但同一出口反复试
// 没有意义; 比	auth 类(60 分钟)长一个量级。
const classGeoBlocked = "geoBlocked"

// errorRule 单条声明式错误规则。
type errorRule struct {
	name     string   // 人可读名, 落进冷却原因
	patterns []string // 正文包含任一即命中(小写)
	exclude  []string // 正文包含任一则一票否决(反误伤豁免)
	class    string   // 归入的错误类别
}

// candidateErrorRules 候选链错误分类的声明式规则, 按顺序匹配, 首个命中生效。
var candidateErrorRules = []errorRule{
	{
		name: "地区封锁",
		// opencode zen 用 type:RegionError; 其余是常见措辞
		patterns: []string{
			"regionerror",
			"region not supported",
			"unsupported_region",
			"not available in your region",
			"geo-restricted",
			"not available in your country",
		},
		// Cloudflare 1010(指纹拒绝)与质询页会夹带类似文案, 不能当地区封锁
		exclude: []string{"error code: 1010", "just a moment", "attention required"},
		class:   classGeoBlocked,
	},
	{
		name:     "上下文超限",
		patterns: []string{"context_length_exceeded", "maximum context length", "context window"},
		class:    classPermanent, // 换站也大概率超, 且不是暂态; 由调用方按永久拒绝处理
	},
}

// matchErrorRules 按规则表分类; 未命中返回 ("", "")。
// 传入的 body 会先做小写化缓存, 避免每条规则重复转换。
func matchErrorRules(body string) (string, string) {
	if body == "" {
		return "", ""
	}
	lower := strings.ToLower(body)
	for _, rule := range candidateErrorRules {
		vetoed := false
		for _, ex := range rule.exclude {
			if strings.Contains(lower, ex) {
				vetoed = true
				break
			}
		}
		if vetoed {
			continue
		}
		for _, p := range rule.patterns {
			if strings.Contains(lower, p) {
				return rule.class, rule.name + ": " + kitTruncateHead(body)
			}
		}
	}
	return "", ""
}

// kitTruncateHead 错误原因串截断(与既有 cooldown 的 200 字节口径一致)。
func kitTruncateHead(s string) string {
	const n = 200
	if len(s) <= n {
		return strings.TrimSpace(s)
	}
	return strings.TrimSpace(s[:n])
}

func init() {
	// 把新类别挂进默认冷却分档(24h), 与 config 的 cooldownMs 覆盖机制兼容。
	defaultCooldownMs[classGeoBlocked] = int64((24 * time.Hour) / time.Millisecond)
}
