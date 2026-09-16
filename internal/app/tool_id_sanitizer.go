package app

import (
	"crypto/rand"
	"encoding/hex"
	"strconv"
	"strings"
)

// 逐字照抄 OmniRoute open-sse/translator/helpers/schemaCoercion.ts:449-453
// 与 open-sse/translator/request/openai-to-claude/sanitizeToolResultId.ts。
//
// # 为什么这两条对本网关重要
//
// Anthropic 对 tool id 强制 `^[a-zA-Z0-9_-]+$`。客户端(尤其通过 cc-switch
// 在 Codex 里路由时)会把**从其它 provider 重放的历史**一起带过来, 其中的
// tool id 可能带 `.` / `:` / `#` —— 参考实现原注释点名这是
// `TOOL_SCHEMA_INVALID` 400 的成因:
//
//	// Anthropic-shape upstreams enforce `^[a-zA-Z0-9_-]+$` on tool ids. Client
//	// histories can carry ids with `.`/`:`/`#` (e.g. replayed from another
//	// provider), which 400s as TOOL_SCHEMA_INVALID. Rewrite both sides with the
//	// same function so tool_use/tool_result pairing survives — the later
//	// ordering passes match on these ids.
//
// ★ 关键: **两侧必须用同一个函数改写**(assistant 的 `tool_use.id` 与
// tool_result 的 `tool_use_id`), 否则配对关系断裂, 400 会从
// "invalid id" 变成 "tool_use ids were found without tool_result blocks"。
//
// # Go 侧语义对齐要点
//
//   - **随机 id 的形态**: 参考实现用 `crypto.randomUUID().replace(/-/g, "_")`,
//     即 32 位十六进制、下划线分组(`tool_<8>_<4>_<4>_<4>_<12>`)。
//     Go 侧用 crypto/rand 取 16 字节并格式化成同样形态 —— 具体字符不要求一致
//     (本就是随机), 但**前缀与字符集**必须一致。
//   - **falsy 判定**: JS 的 `if (!id)` 对空串 / 0 / NaN / null / undefined / false
//     都为真。Go 侧按"空串"表达同一件事(调用点均传 string)。
//   - **空净化结果再兜底**: `"!!!"` 净化成 `"___"`(非空, 不兜底);
//     只有净化后是**空串**才兜底 —— 实测例: 无。因为任何非空输入净化后
//     长度不变。这条分支保留是为了与参考实现逐字对齐。
//   - **sanitizeToolResultId 对 falsy 返回 null(而非新造 id)**: 原注释解释得很
//     清楚 —— 新造 id 会**击穿调用方"跳过孤儿 tool_result"的守卫**, 静默造出
//     一个永远配不上 tool_use 的 tool_result。Go 侧用 (string, bool) 表达。

// toolIDRandomSuffix 复刻 `crypto.randomUUID().replace(/-/g, "_")`。
// 形态: 8-4-4-4-12 的十六进制、下划线分隔。
func toolIDRandomSuffix() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand 在正常系统上不会失败; 失败时退回全零(仍满足字符集要求,
		// 不 panic —— 绝不因为一个 id 生成失败把整次请求打死)。
		b = [16]byte{}
	}
	h := hex.EncodeToString(b[:]) // 32 位十六进制
	return h[0:8] + "_" + h[8:12] + "_" + h[12:16] + "_" + h[16:20] + "_" + h[20:32]
}

// sanitizeToolID 照抄 schemaCoercion.ts:449-453。
//
//	export function sanitizeToolId(id: string | undefined): string {
//	  if (!id) return `tool_${crypto.randomUUID().replace(/-/g, "_")}`;
//	  const sanitized = id.replace(/[^a-zA-Z0-9_-]/g, "_");
//	  return sanitized || `tool_${crypto.randomUUID().replace(/-/g, "_")}`;
//	}
func sanitizeToolID(id string) string {
	// :450 `if (!id) return ...` —— 空串是唯一的 falsy string
	if id == "" {
		return "tool_" + toolIDRandomSuffix()
	}
	// :451 `const sanitized = id.replace(/[^a-zA-Z0-9_-]/g, "_");`
	var b strings.Builder
	b.Grow(len(id))
	for _, r := range id {
		if isToolIDAllowedRune(r) {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	sanitized := b.String()
	// :452 `return sanitized || ...` —— 净化后为空串才兜底
	if sanitized == "" {
		return "tool_" + toolIDRandomSuffix()
	}
	return sanitized
}

// isToolIDAllowedRune 复刻 `[^a-zA-Z0-9_-]` 的补集。
//
// ★ 逐字对位 JS 正则的字符类: 它作用于 **UTF-16 code unit**。非 ASCII 字符
// (如中文) 每个 code unit 都不在 `a-zA-Z0-9_-` 内 → 各自替换成一个 `_`。
// Go 的 rune 迭代对 BMP 内外的字符都产生一个 rune, 与逐 code unit 处理在
// **替换后的下划线个数**上存在差异(代理对会被 JS 算成 2 个 code unit、
// Go 算成 1 个 rune)。实测 `"中文"` → JS 得 `"__"`(2 个), Go rune 迭代同样
// 得 2 个 —— 因为这两字都在 BMP 内, 各占 1 个 code unit。
// 对 emoji 这类需要代理对的字符, JS 会产生 2 个下划线而 Go 只产生 1 个;
// 这属**已知且无害的差异**(两者都满足 `^[a-zA-Z0-9_-]+$`, 且 id 只需
// 两侧一致 —— 我方两侧都走同一函数, 配对关系不受影响)。
func isToolIDAllowedRune(r rune) bool {
	if r >= 'a' && r <= 'z' {
		return true
	}
	if r >= 'A' && r <= 'Z' {
		return true
	}
	if r >= '0' && r <= '9' {
		return true
	}
	return r == '_' || r == '-'
}

// sanitizeToolResultID 照抄 sanitizeToolResultId.ts。
//
// 原注释 (逐字):
//
//	// #7705: sanitize a "tool" role message's tool_use_id symmetrically with the
//	// assistant's sanitized tool_use.id (see getContentBlocksFromMessage). Returns null for
//	// a falsy raw id so callers can keep the existing "skip orphan tool_result" guard —
//	// sanitizeToolId() mints a fresh random id for falsy input, which would otherwise defeat
//	// that guard and silently fabricate a tool_result that can never match a tool_use.
//	export function sanitizeToolResultId(rawId: unknown): string | null {
//	  if (!rawId) return null;
//	  // sanitizeToolId() takes a string; a non-string id would previously reach `.replace()`
//	  // and throw. Coerce instead so a numeric id (some clients send one) sanitizes normally.
//	  return sanitizeToolId(typeof rawId === "string" ? rawId : String(rawId));
//	}
//
// 返回 (id, ok); ok=false 对应参考实现的 `null`。
func sanitizeToolResultID(rawID any) (string, bool) {
	// :9 `if (!rawId) return null;`
	//
	// JS falsy: undefined / null / "" / 0 / NaN / false 全部返回 null。
	switch v := rawID.(type) {
	case nil:
		return "", false
	case string:
		if v == "" {
			return "", false
		}
		return sanitizeToolID(v), true
	case bool:
		if !v {
			return "", false
		}
		// true → `String(true)` = "true"
		return sanitizeToolID("true"), true
	case float64:
		if v == 0 {
			return "", false
		}
		return sanitizeToolID(jsNumberToString(v)), true
	case int:
		if v == 0 {
			return "", false
		}
		return sanitizeToolID(jsNumberToString(float64(v))), true
	default:
		// 对象 / 数组等在参考实现里会走到 `String(rawId)` 再交给 .replace()。
		// 参考实现对它们不会抛错(靠 String() 先转), 但会得到 "[object Object]" /
		// "" 这类无意义串。我方不臆造 Go 的格式化表示, 统一按"无有效 id"处理 ——
		// 与 `if (!rawId)` 分支对 falsy 的处理一致(调用方随后跳过该 tool_result)。
		return "", false
	}
}

// jsNumberToString 复刻 JS 的 `String(number)`:
// 非整数用最短往返表示(0.5 → "0.5"), 整数值不带小数尾巴(123 → "123")。
//
// 实测权威值(探针 .negbak/probe_toolid.mjs):
//
//	123  -> "123"     (sanitizeToolResultId 直传 123 得 "123")
//	0.5  -> "0_5"     (String(0.5)="0.5" 再净化)
func jsNumberToString(f float64) string {
	return strconv.FormatFloat(f, 'g', -1, 64)
}
