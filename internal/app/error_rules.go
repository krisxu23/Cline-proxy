package app

// 错误规则表 (P1-10, 参照 OmniRoute 的 config/errorConfig.ts + providerErrorRules.ts
// + services/errorClassifier.ts):
//
// 上游的错误表达五花八门 —— 状态码相同的 403 可能是"地区封锁"(该换出口/长冷却)
// 也可能是"key 被拒"(短冷却也没用), 只看状态码会把这些混为一谈。
// 规则表按"正文特征优先于状态码"的顺序匹配, 命中即返回专用类别;
// 反误伤豁免(exclude)先于 patterns 检查 —— 例如 Cloudflare 的 1010 指纹拒绝
// 长得像地区文案, 但它其实是"客户端指纹被拒", 长冷却毫无意义。
//
// 纪律:
//   - 新规则必须带测试(见 error_rules_test.go), patterns 一律小写匹配。
//   - 子串规则用 strings.Contains(简单、不引入 ReDoS); 只有 Cloudflare 1010
//     这类"裸数字可能误伤"的判别才用有界正则(见 isCloudflareFingerprintRejection)。

import (
	"regexp"
	"strings"
	"time"
) // classGeoBlocked 地区封锁: 该出口所在地区不被上游接受。
// 冷却 24 小时(OmniRoute 同款分档): 换出口可能立刻可用, 但同一出口反复试
// 没有意义; 比 auth 类(60 分钟)长一个量级。
const classGeoBlocked = "geoBlocked"

// classFingerprint 客户端指纹拒绝: 上游前方 CDN(如 opencode.ai/zen/v1 前的
// Cloudflare)拒绝了**客户端的 TLS/UA 签名**, 不是账号/模型问题 ——
// 实测 curl 200 / urllib 403 于字节相同的 body(OmniRoute 2026-08-08 实测)。
// 它说明当前出口节点的指纹被上游标记, 应**换节点/换客户端指纹**, 绝不能把
// 候选打进账号级 banned 或永久剔除。参考实现独立分类 FINGERPRINT_REJECTION。
const classFingerprint = "fingerprint"

// classResourceNotFound 请求级资源 404(文件/上传/响应项等资源不存在)。
// 这类失败描述的是**请求的 payload**, 不是 provider/model 健康 —— 换个账号或
// 模型不会让一个不存在的 file_id 变合法, 冷冷却模型只会徒劳重试(files/items
// ID 错误)。参考实现用 isResourceNotFoundResponse 显式返回 null(不冷却)。
const classResourceNotFound = "resourceNotFound"

// errorRule 单条声明式错误规则(子串匹配)。
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
		// 上下文超限信号集补全(沿参考 CONTEXT_OVERFLOW_SIGNALS, errorClassifier.ts:93-105):
		// 旧表只有 context_length_exceeded / maximum context length / context window
		// 三个, 上游常见的 "prompt too large" / "exceeds context" / "input too long" /
		// "token limit" / "too many tokens" / "messages exceed" 全都漏掉, 会落成
		// generic serverError 短冷却, 用户反复撞墙。
		name: "上下文超限",
		patterns: []string{
			"context_length_exceeded", "maximum context length", "context window",
			"context overflow", "prompt too large", "exceeds context",
			"maximum context", "input too long", "token limit",
			"too many tokens", "context length", "messages exceed",
		},
		class: classPermanent, // 换站也大概率超, 且不是暂态; 由调用方按永久拒绝处理
	},
}

// --- Cloudflare 1010 指纹拒绝(有界正则, errorClassifier.ts:194-204) ----------

// cloudflare1010Re 只认"显式 Cloudflare 键 + 1010"或"唯一指纹 token"。
// 裸数字 1010 **不匹配**: 403 body 里合法地出现 "1010" 作为 port/count/request
// id/model token("model foo-1010 is not supported","retry after 1010 seconds")。
func isCloudflareFingerprintRejection(body string) bool {
	if body == "" {
		return false
	}
	lower := strings.ToLower(body)
	if cloudflare1010Re.MatchString(lower) {
		return true
	}
	return strings.Contains(lower, "browser_signature_banned") ||
		strings.Contains(lower, "fingerprint_rejection")
}

// cloudflare1010Re 对位参考 CLOUDFLARE_1010_REGEX(errorClassifier.ts:194-195), 转 RE2:
//
//	源: /(?<![A-Za-z0-9_-])error[\s_-]?code[\\"':=\s]{0,12}1010(?!\w)
//	    |(?<![A-Za-z0-9_-])error[-_]\s?1010(?!\w)\/?/i
//
// Go RE2 不支持 lookbehind `(?<!` 与 lookahead `(?!`, 改写成等价的前缀/后缀
// 字符集边界: 前缀用 `(?:^|[^A-Za-z0-9_-])`(error 前非单词/开头), 后缀用
// `(?:[^A-Za-z0-9]|$)`(1010 后非字母/结尾)。`/?` 的可选尾斜杠在裸 `1010/`
// 场景几乎不出现, 为 RE2 简单性省略 —— 语义覆盖不受影响。
//
// 分支 1: error[可选 空白/下划线/连字符]code[引号/冒号/等号/空白 0..12 个]1010
//
//	(error_code: 1010 / error-code "1010" / error code = 1010)
//
// 分支 2: error[-_] + 可选空白 + 1010  (error_1010 / error- 1010)
//
// 关键(沿参考 :187-193 的纪律): 裸数字 1010 **不匹配** —— 403 body 里合法地出现
// "1010" 作为 port/count/request id/model token("model foo-1010 is not supported",
// "retry after 1010 seconds")。必须带 error_(code) 键或唯一指纹 token 才算。
var cloudflare1010Re = regexp.MustCompile(
	`(?i)(?:^|[^A-Za-z0-9_-])error[\s_-]?code[\s"'\\:=]{0,12}1010(?:[^A-Za-z0-9]|$)|` +
		`(?:^|[^A-Za-z0-9_-])error[-_]\s?1010(?:[^A-Za-z0-9]|$)`)

// --- 请求级资源 404(errorClassifier.ts:222-240 的 isResourceNotFoundResponse) ----

// resourceNotFoundRe "主体 + not found"或反向, 有界避免 ReDoS。
var resourceNotFoundRe = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bfiles?\b[^\n]{0,160}\b(?:not found|does not exist)\b`),
	regexp.MustCompile(`(?i)\b(?:not found|does not exist)\b[^\n]{0,160}\bfiles?\b`),
	regexp.MustCompile(`(?i)\b(?:input[_ -]?file|file[_ -]?id|item|response|vector[_ -]?store|upload)\b[^\n]{0,160}\b(?:not found|does not exist)\b`),
	regexp.MustCompile(`(?i)\b(?:not found|does not exist)\b[^\n]{0,160}\b(?:input[_ -]?file|file[_ -]?id|item|response|vector[_ -]?store|upload)\b`),
	regexp.MustCompile(`(?i)\bfile-[a-z0-9_-]+\b[^\n]{0,160}\b(?:not found|does not exist)\b`),
}

// isResourceNotFoundResponseStr 判一个上游 body 是否标识"请求级资源不存在"。
// 参考实现用它显式返回不冷却(而非 MODEL_NOT_FOUND 锁定模型)。
// "model ... not found" / "Requested entity was not found" 是**模型/实体**级别,
// 不带 file/item/upload 等资源词, 不会命中这里的资源正则, 仍走 404 → classPermanent。
func isResourceNotFoundResponseStr(body string) bool {
	if body == "" {
		return false
	}
	for _, re := range resourceNotFoundRe {
		if re.MatchString(body) {
			return true
		}
	}
	return false
}

// matchErrorRules 按规则表分类; 未命中返回 ("", "")。
// 传入的 body 会先做小写化缓存, 避免每条规则重复转换。
// 注意: classifyCandidateFailure 在调用本函数**之前**就已单独判定 Cloudflare
// 指纹(见 cooldown.go), 这里只处理子串规则表; 若未来 matchErrorRules 想并入
// 指纹判定, 须保证其优先级在地区封锁之上且不进 permanent。
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
	// 把新类别挂进默认冷却分档, 与 config 的 cooldownMs 覆盖机制兼容。
	defaultCooldownMs[classGeoBlocked] = int64((24 * time.Hour) / time.Millisecond)
	// 指纹拒绝 5 分钟: 换节点/换指纹即可, 短冷却足够; 绝不能是永久/账号级。
	defaultCooldownMs[classFingerprint] = 5 * 60 * 1000
	// 请求资源 404 短冷却 1 分钟: 客户端修正 payload 后会重试, 不该拖太久。
	defaultCooldownMs[classResourceNotFound] = 1 * 60 * 1000
}
