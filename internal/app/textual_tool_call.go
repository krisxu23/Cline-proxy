// Package app — 文本型工具调用 (textual tool call) 解析。
//
// 逐字照抄 OmniRoute open-sse/utils/textualToolCall.ts (112 行, 4 个导出)。
//
// 背景: 部分上游 (尤其是 antigravity / gemini 系与若干中转) 不返回结构化的
// `tool_calls` 字段, 而是把工具调用**写进正文文本**里, 形如:
//
//	[Tool call: read_file]
//	Arguments: {"path": "a.txt"}
//
// 若网关不识别这种形态, 这段文本会被原样透传给 agent, agent 看到的就是
// 一段"看起来像工具调用但其实是普通文本"的内容 —— 既不会执行工具, 也不会
// 报错, 表现就是**任务无声中断**。这正是本模块要解决的问题。
//
// 与原实现逐条对应的 4 个导出:
//   - stripZeroWidth          : 递归剔除零宽字符 (U+200B-U+200D, U+FEFF)
//   - isValidToolCallHeaderPrefix : 判断候选串是否是合法的工具调用头部
//   - parseTextualToolCallCandidate : 解析候选串 → 完整 / 部分 / 非候选
//   - containsTextualToolCallMarker : 文本中是否含工具调用标记
package app

import (
	"encoding/json"
	"regexp"
	"strings"
)

// 照抄 textualToolCall.ts:3 / :61 / :105 —— 零宽字符正则
// `[\u200B-\u200D\uFEFF]`。BOM (U+FEFF) 与零宽空格/非连接符/连接符
// 常常被上游偷偷混入正文, 导致 `[Tool call:` 前缀匹配失败。
var zeroWidthRe = regexp.MustCompile("[\u200B-\u200D\uFEFF]")

// toolCallHeaderRe 照抄 textualToolCall.ts:78:
//
//	/^\[Tool call:\s*([^\]\n]+)\]\s*\nArguments:\s*/
//
// 注意 `[^\]\n]+` —— 工具名里既不能有 `]` 也不能换行。
var toolCallHeaderRe = regexp.MustCompile(`^\[Tool call:\s*([^\]\n]+)\]\s*\nArguments:\s*`)

const (
	toolCallPrefix   = "[Tool call:"
	emptyToolCallPre = "(empty)" + toolCallPrefix
	argumentsLabel   = "Arguments:"
)

// stripZeroWidth 照抄 textualToolCall.ts:1-17。
//
//	export function stripZeroWidth(value: unknown): unknown {
//	  if (typeof value === "string") {
//	    return value.replace(/[\u200B-\u200D\uFEFF]/g, "");
//	  }
//	  if (Array.isArray(value)) {
//	    return value.map((item) => stripZeroWidth(item));
//	  }
//	  if (value && typeof value === "object") {
//	    return Object.fromEntries(
//	      Object.entries(value as Record<string, unknown>).map(([key, item]) => [
//	        key,
//	        stripZeroWidth(item),
//	      ])
//	    );
//	  }
//	  return value;
//	}
//
// 递归语义要点: 只沿 string / []any / map[string]any 三种结构下钻,
// 其余 (数字/布尔/nil) 原样返回。map 的**键**不做处理, 只处理值。
func stripZeroWidth(value any) any {
	switch v := value.(type) {
	case string:
		return zeroWidthRe.ReplaceAllString(v, "")
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = stripZeroWidth(item)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, item := range v {
			out[k] = stripZeroWidth(item)
		}
		return out
	default:
		return value
	}
}

// isValidToolCallHeaderPrefix 照抄 textualToolCall.ts:19-55。
//
// 判定依据逐条对应原实现:
//
//  1. 必须以 "[Tool call:" 开头 (:20)
//  2. 无 "]" 时 —— 名字部分不含换行、不含 "[" 即视为合法前缀 (:23-27)
//     (流式场景下 `]` 还没到, 需要容忍半截头部)
//  3. 有 "]" 时 —— 名字部分不含换行且 trim 后非空 (:29-30)
//  4. "]" 之后:
//     - 纯空白 / 空 → 合法 (:37-39)
//     - 空白里没有换行 → 非法 (:41-43) 例如 "[Tool call: x] garbage"
//     - 已出现的内容与 "Arguments:" 互为前缀 → 合法 (:45-52)
//     (双向 startsWith, 覆盖 "Arg" 和 "Arguments: {...}" 两种中间态)
//     - 其余 → 非法 (:54)
func isValidToolCallHeaderPrefix(candidate string) bool {
	if !strings.HasPrefix(candidate, toolCallPrefix) {
		return false
	}

	bracketIndex := strings.Index(candidate, "]")
	if bracketIndex == -1 {
		namePart := candidate[len(toolCallPrefix):]
		if strings.Contains(namePart, "\n") || strings.Contains(namePart, "[") {
			return false
		}
		return true
	}

	namePart := candidate[len(toolCallPrefix):bracketIndex]
	if strings.Contains(namePart, "\n") || strings.TrimSpace(namePart) == "" {
		return false
	}

	afterBracket := candidate[bracketIndex+1:]
	leadingWhitespace := leadingWhitespaceRe.FindString(afterBracket)
	textAfterWhitespace := afterBracket[len(leadingWhitespace):]

	if len(textAfterWhitespace) == 0 {
		return true
	}

	if !strings.Contains(leadingWhitespace, "\n") {
		return false
	}

	// 双向前缀: "Arg" 是 "Arguments:" 的前缀; "Arguments: {...}" 也以它开头。
	if strings.HasPrefix(argumentsLabel, textAfterWhitespace) {
		return true
	}
	if strings.HasPrefix(textAfterWhitespace, argumentsLabel) {
		return true
	}

	return false
}

// leadingWhitespaceRe 对应 JS `afterBracket.match(/^[\s\r\n]*/)`。
// JS 的 `\s` 已含 \r\n, 这里显式写出以保持可读性。
var leadingWhitespaceRe = regexp.MustCompile(`^[\s\r\n]*`)

// textualToolCallCandidate 对应原实现的联合返回类型:
//
//	{ kind: "complete"; name: string; args: unknown } | { kind: "partial" } | null
//
// Go 里用 kind 字段表达三态, nil 指针表示 TS 的 `null`。
type textualToolCallCandidate struct {
	kind string // "complete" | "partial"
	name string
	args any
}

// parseTextualToolCallCandidate 照抄 textualToolCall.ts:57-101。
//
// 解析流程 (与原实现逐行对应):
//
//	:60  非字符串 → null
//	:61  先做一次零宽清洗
//	:62  取**最后一个** "[Tool call:" (lastIndexOf) —— 正文里可能提到这个标记,
//	     取最后一个才是真正待解析的那次调用
//	:63-73 找不到标记时的"半截探测":
//	       - 若结尾是 "(empty)[Tool call:" 的前缀 → partial
//	       - 若结尾是 "[Tool call:" 的前缀 → partial
//	       - 否则 null
//	:75  头部不合法 → null
//	:79  头部正则不匹配 (Arguments 还没到齐) → partial
//	:82  名字或参数为空 → partial
//	:83-99 两种解码器依次尝试 JSON 解析:
//	       1. 原文直接 JSON.parse
//	       2. 若被引号包裹, 先去引号再 JSON.parse
//	       解析成功 → complete, 且 args 过一遍 stripZeroWidth
//	       全部失败 → partial
func parseTextualToolCallCandidate(text any) *textualToolCallCandidate {
	s, ok := text.(string)
	if !ok {
		return nil
	}
	normalized := zeroWidthRe.ReplaceAllString(s, "")
	toolCallIndex := strings.LastIndex(normalized, toolCallPrefix)
	if toolCallIndex < 0 {
		// 半截探测: 流式下 "(empty)[Tool call:" 或 "[Tool call:" 可能只到一半。
		lastParen := strings.LastIndex(normalized, "(")
		if lastParen != -1 && strings.HasPrefix(emptyToolCallPre, normalized[lastParen:]) {
			return &textualToolCallCandidate{kind: "partial"}
		}
		lastBracket := strings.LastIndex(normalized, "[")
		if lastBracket != -1 && strings.HasPrefix(toolCallPrefix, normalized[lastBracket:]) {
			return &textualToolCallCandidate{kind: "partial"}
		}
		return nil
	}
	candidate := normalized[toolCallIndex:]
	if !isValidToolCallHeaderPrefix(candidate) {
		return nil
	}
	headerMatch := toolCallHeaderRe.FindStringSubmatch(candidate)
	if headerMatch == nil {
		return &textualToolCallCandidate{kind: "partial"}
	}
	name := strings.TrimSpace(headerMatch[1])
	rawArgs := strings.TrimSpace(candidate[len(headerMatch[0]):])
	if name == "" || rawArgs == "" {
		return &textualToolCallCandidate{kind: "partial"}
	}

	// 原实现的两个 decoder 依次尝试 (:83-99)。
	decoded, ok := decodeToolCallArgs(rawArgs)
	if !ok {
		return &textualToolCallCandidate{kind: "partial"}
	}
	return &textualToolCallCandidate{
		kind: "complete",
		name: name,
		args: stripZeroWidth(decoded),
	}
}

// decodeToolCallArgs 对应 textualToolCall.ts:83-99 的 decoders 循环。
//
//	decoders = [
//	  (value) => value,                       // 恒等
//	  (value) => {                            // 去掉包裹引号
//	    if (value.startsWith('"') && value.endsWith('"')) {
//	      const decoded = JSON.parse(value);
//	      return typeof decoded === "string" ? decoded : value;
//	    }
//	    return value;
//	  },
//	];
//	for (const decode of decoders) {
//	  try {
//	    const decoded = decode(rawArgs);
//	    const parsed = JSON.parse(decoded);
//	    return { kind: "complete", name, args: stripZeroWidth(parsed) };
//	  } catch {}
//	}
//
// 语义: 两个 decoder 各自"先变换字符串、再 parse、成功即返回"。
//
// ⚠ 一个反直觉但必须照抄的事实 (Node 实测确认):
// 对 `"{\"a\":1}"` 这种"整体被引号包裹"的输入, **decoder 1 就已经成功了** ——
// 恒等变换后 `JSON.parse` 返回一个字符串 `{"a":1}`, 于是函数立刻返回,
// args 的类型是 **string** 而不是 object。decoder 2 在这种情况下永远执行不到,
// 属于无法触达的分支(它只在 decoder 1 抛错时才有机会运行)。
//
// 我第一版实现曾试图"修正"这一点(让 args 变成对象), 那是**擅自改动**,
// 已回退。下游 collectPassthroughTextualToolCall 会做
// `JSON.stringify(parsed.args || {})`, 对字符串 args 会输出带引号的 JSON 串,
// 这是参考实现的既有效果, 不在本次照抄范围内擅自"优化"。
func decodeToolCallArgs(rawArgs string) (any, bool) {
	type decoder func(string) string
	decoders := []decoder{
		// decoder 1: 恒等
		func(v string) string { return v },
		// decoder 2: 仅当首尾都是 `"` 时, 用一次容忍失败的 parse 探明内层是否为
		// 字符串; 是则剥掉外层引号, 否则原样返回。
		func(v string) string {
			if strings.HasPrefix(v, `"`) && strings.HasSuffix(v, `"`) {
				if decoded, ok := tryParseJSON(v); ok {
					if s, isStr := decoded.(string); isStr {
						return s
					}
				}
			}
			return v
		},
	}
	for _, decode := range decoders {
		if parsed, ok := tryParseJSON(decode(rawArgs)); ok {
			return parsed, true
		}
	}
	return nil, false
}

// tryParseJSON 对应 JS 的 `JSON.parse` + try/catch: 失败即 (nil, false)。
func tryParseJSON(s string) (any, bool) {
	var parsed any
	if err := json.Unmarshal([]byte(s), &parsed); err != nil {
		return nil, false
	}
	return parsed, true
}

// containsTextualToolCallMarker 照抄 textualToolCall.ts:103-112。
//
//	:105 零宽清洗
//	:107 不含 "[Tool call:" → false
//	:108 含 "Arguments:" → true (说明头部+参数齐了)
//	:110-111 trim 后以 "[Tool call:" 或 "(empty)[Tool call:" 开头 → true
//
// 用途: 判断一段正文是否"看起来是工具调用但不完整"。此时正文必须被丢弃,
// 不能当普通文本发给 agent。
func containsTextualToolCallMarker(text any) bool {
	s, ok := text.(string)
	if !ok {
		return false
	}
	normalized := zeroWidthRe.ReplaceAllString(s, "")
	if !strings.Contains(normalized, toolCallPrefix) {
		return false
	}
	if strings.Contains(normalized, argumentsLabel) {
		return true
	}
	trimmed := strings.TrimSpace(normalized)
	return strings.HasPrefix(trimmed, toolCallPrefix) || strings.HasPrefix(trimmed, emptyToolCallPre)
}
