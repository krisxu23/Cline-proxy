package app

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

// 逐字照抄 OmniRoute open-sse/translator/helpers/toolCallShim.ts。
//
// 参考实现原注释（逐字）:
//
//	// Defensive shims for tool calls whose strict-schema fields can be malformed
//	// by upstream models (e.g. MiMo emitting empty objects/strings instead of
//	// arrays for Capy's submit_pr_review).
//	//
//	// Applied on the assembled OpenAI tool-call arguments after streaming, just
//	// before they are re-emitted as a single Claude input_json_delta.
//	//
//	// To add a new shim: register a (input) => input transformer in TOOL_SHIMS
//	// keyed by the tool name. The transformer must accept arbitrary input and
//	// return a JSON-safe value.
//
// # 为什么这条对本网关重要
//
// 上游模型（尤其非 Anthropic 系：GPT 系 / DeepSeek / MiMo …）在工具参数上会
// 产出**结构合法但取值荒唐**的结果：
//
//   - Claude Code 的 `Read` 工具 `limit` 上限 2000 行，模型偶尔吐出
//     `limit: 25999999999999999`；Claude Code 直接拒收 -> **重试循环**，
//     每轮都把整个上下文再发一遍，token 成倍烧掉。
//   - `limit`/`offset` 被**字符串化**（`"5000"`）—— 部分模型把所有数字都写成字符串。
//   - `offset` 为负数。
//   - 非 PDF 文件带上 `pages`（只有 PDF 才认），或 PDF 上 `pages` 写成非法值。
//   - `submit_pr_review` 的 `functionalChanges` / `findings` 本该是数组，
//     模型却给 `{}` 或 `""`。
//
// 这些都不会让请求本身失败 —— 它们让**工具调用失败**，而工具调用失败在 agent
// 客户端里往往表现为重试风暴或任务无声中断。这与用户报的现象同源。
//
// # 关键语义点（探针 S1–S12 逐条锁定）
//
//   - `coerceToArray`：**只有**真数组，或"能 parse 成数组的 JSON 字符串"能留下；
//     JSON 对象串（`'{"a":1}'`）、普通串、数字、对象、布尔**全部**变空数组 `[]`
//     （S1）。注意 `'  [1]  '` 前有空白时 `JSON.parse` 仍成功 -> `[1]`（S1 末栏）。
//   - Read 的 `limit` 处理有**两阶段**（S2/S3）：
//     ① 字符串且匹配 `^\d+$` -> 转 number（`"5000"` -> 5000）；
//     ② 是 number 且 `> 2000` -> 2000；`< 1` -> **删除该键**。
//     ★ `"5000.5"` 和 `"abc"` **不匹配** `^\d+$` -> 原样保留（S3 第 2/3 栏）。
//   - `offset`：字符串且匹配 `^-?\d+$` -> 转 number（含负数！`"-3"` -> -3，
//     随后被负数规则归 0）；是 number 且 `< 0` -> 0（S4）。
//   - `pages`：仅当 `file_path` 是**字符串**且小写后以 `.pdf` 结尾、且 `pages` 是
//     **字符串**且匹配 `^\d+(?:-\d+)?$` 时保留；否则 **删除**（S5）。
//     注意 `"5-1"`（逆序）也**匹配**该正则 -> 保留（S5）。
//     `pages: 1`（数字）不匹配 -> 删除。
//   - shim 只对**对象**输入生效；数组 / 字符串 / 数字 / null / 布尔**原样返回**
//     （S6）—— 注意这与 `applyToolCallShimToBuffer` 的"解析失败用 `{}`"是两回事。
//   - 名字解析**大小写不敏感**（S8），但**精确匹配优先**（先查同名键，再逐个
//     小写比对）。S11 里 `42` / `{}` 都因 `typeof name !== "string"` 而为 false。
//   - `applyToolCallShimToBuffer`：无 shim -> **原样返回 raw**（S9）；
//     有 shim 但 raw 空或不可 parse -> 以 **`{}`** 为输入（S10）。

// shimFn 对位参考实现的 `type ShimFn = (input: unknown) => unknown`。
type shimFn func(input any) any

// readMaxLimit 照抄 toolCallShim.ts 的 `const READ_MAX_LIMIT = 2000`。
const readMaxLimit = 2000

// coerceToArray 照抄 toolCallShim.ts:
//
//	function coerceToArray(v: unknown): unknown[] {
//	  if (Array.isArray(v)) return v;
//	  if (v == null) return [];
//	  if (typeof v === "string") {
//	    if (v === "") return [];
//	    try {
//	      const parsed = JSON.parse(v);
//	      return Array.isArray(parsed) ? parsed : [];
//	    } catch {
//	      return [];
//	    }
//	  }
//	  // Plain object or other non-array → empty
//	  return [];
//	}
//
// 探针 S1 十二栏全部锁定。★ 注意 `'{"a":1}'`（能 parse 成对象）-> `[]`。
func coerceToArray(v any) []any {
	if arr, ok := v.([]any); ok {
		return arr
	}
	if v == nil {
		return []any{}
	}
	if s, ok := v.(string); ok {
		if s == "" {
			return []any{}
		}
		var parsed any
		// JS 的 JSON.parse 接受前后空白（探针 S1 末栏 `'  [1]  '` -> `[1]`）。
		if err := json.Unmarshal([]byte(s), &parsed); err == nil {
			if arr, ok := parsed.([]any); ok {
				return arr
			}
		}
		return []any{}
	}
	return []any{}
}

// readDigitsRe 对位 Read 里 `limit` 的 `/^\d+$/`（**不接受**负号与小数点）。
var readDigitsRe = regexp.MustCompile(`^\d+$`)

// readSignedDigitsRe 对位 Read 里 `offset` 的 `/^-?\d+$/`。
var readSignedDigitsRe = regexp.MustCompile(`^-?\d+$`)

// pdfPagesRe 对位 `isValidPdfPagesArg` 的 `/^\d+(?:-\d+)?$/`。
var pdfPagesRe = regexp.MustCompile(`^\d+(?:-\d+)?$`)

// isValidPdfPagesArg 照抄 toolCallShim.ts:
//
//	// `pages` is only meaningful for PDFs and only as `"N"` or `"N-M"` (1-based).
//	// Reference: claude-code-tools docs + upstream decolua/9router#1144.
//	function isValidPdfPagesArg(filePath: unknown, pages: unknown): boolean {
//	  return typeof filePath === "string" && filePath.toLowerCase().endsWith(".pdf")
//	    && typeof pages === "string" && /^\d+(?:-\d+)?$/.test(pages);
//	}
//
// ★ 三个条件**全**要满足。`"5-1"`（逆序）也匹配（探针 S5）。
func isValidPdfPagesArg(filePath, pages any) bool {
	fp, ok := filePath.(string)
	if !ok {
		return false
	}
	// `filePath.toLowerCase().endsWith(".pdf")`
	if !strings.HasSuffix(strings.ToLower(fp), ".pdf") {
		return false
	}
	p, ok := pages.(string)
	if !ok {
		return false
	}
	return pdfPagesRe.MatchString(p)
}

// sanitizeReadArgs 照抄 toolCallShim.ts 的 `sanitizeReadArgs`（就地改 args）。
//
// 参考实现原注释（关于那个局部变量 limit 的反直觉点，逐字）:
//
//	// Read into a local: assigning back to `args.limit` (declared `unknown`) resets the
//	// `typeof` narrowing, so the second comparison would no longer see a number. The two
//	// branches are mutually exclusive (READ_MAX_LIMIT is 2000), so testing the original
//	// value keeps the behavior identical.
func sanitizeReadArgs(args map[string]any) {
	// Coerce numeric-string limit/offset (some non-Anthropic models stringify everything).
	if s, ok := args["limit"].(string); ok && readDigitsRe.MatchString(s) {
		if n, err := strconv.Atoi(s); err == nil {
			args["limit"] = float64(n)
		}
	}
	if s, ok := args["offset"].(string); ok && readSignedDigitsRe.MatchString(s) {
		if n, err := strconv.Atoi(s); err == nil {
			args["offset"] = float64(n)
		}
	}

	if limit, ok := args["limit"].(float64); ok {
		// ★ 参考实现是**两条独立的 `if`**（不是 if/else if）：
		//     if (limit > READ_MAX_LIMIT) args.limit = READ_MAX_LIMIT;
		//     if (limit < 1) delete args.limit;
		//   两条互斥（READ_MAX_LIMIT = 2000），故与 if/else if 等价；
		//   此处按原样写成分立判断，保持结构可逐行对照。
		if limit > readMaxLimit {
			args["limit"] = float64(readMaxLimit)
		}
		if limit < 1 {
			delete(args, "limit")
		}
	}
	if off, ok := args["offset"].(float64); ok && off < 0 {
		args["offset"] = float64(0)
	}

	// `if ("pages" in args && !isValidPdfPagesArg(args.file_path, args.pages)) { delete args.pages; }`
	if pages, exists := args["pages"]; exists {
		if !isValidPdfPagesArg(args["file_path"], pages) {
			delete(args, "pages")
		}
	}
}

// toolShims 对位参考实现的 `const TOOL_SHIMS: Record<string, ShimFn> = { Read, submit_pr_review }`。
//
// ★ 键名大小写与参考实现一致（`Read` 大写 R，`submit_pr_review` 全小写）。
var toolShims = map[string]shimFn{
	//	// Claude Code Read rejects bad params and retries — wasting tokens with non-Anthropic
	//	// models that emit oversized limits, negative offsets, stringified numbers, or stray
	//	// `pages` on non-PDF files. Buffer and emit one cleaned JSON delta so the client never
	//	// sees the bad fields. See `sanitizeReadArgs` for the per-field rules.
	"Read": func(input any) any {
		obj, ok := input.(map[string]any)
		if !ok {
			// `if (typeof input !== "object" || input === null || Array.isArray(input)) return input;`
			//
			// ★ JS 里 `typeof null === "object"`，故参考实现显式挡了 `=== null`。
			//   Go 的 map 断言天然排除 nil，但为对齐"数组/字符串/数字/布尔原样返回"
			//   （探针 S6），此处统一走 !ok 分支。
			return input
		}
		// `const patched = { ...input };`
		patched := shallowCopyRecord(obj)
		sanitizeReadArgs(patched)
		return patched
	},
	//	submit_pr_review: (input) => {
	//	  if (typeof input !== "object" || input === null || Array.isArray(input)) return input;
	//	  const patched = { ...input };
	//	  for (const key of ["functionalChanges", "findings"]) patched[key] = coerceToArray(patched[key]);
	//	  return patched;
	//	},
	"submit_pr_review": func(input any) any {
		obj, ok := input.(map[string]any)
		if !ok {
			return input
		}
		patched := shallowCopyRecord(obj)
		// ★ 顺序是照抄的: 先 functionalChanges, 后 findings。
		//   两个键**无条件写入**(即使原本不存在)。
		for _, key := range []string{"functionalChanges", "findings"} {
			patched[key] = coerceToArray(patched[key])
		}
		return patched
	},
}

// resolveToolCallShim 照抄 toolCallShim.ts:
//
//	function resolveToolCallShim(name: string | undefined | null): ShimFn | undefined {
//	  if (typeof name !== "string" || !name) return undefined;
//	  if (Object.prototype.hasOwnProperty.call(TOOL_SHIMS, name)) return TOOL_SHIMS[name];
//	  const lower = name.toLowerCase();
//	  for (const [key, fn] of Object.entries(TOOL_SHIMS)) {
//	    if (key.toLowerCase() === lower) return fn;
//	  }
//	  return undefined;
//	}
//
// ★ 两步: ① 精确命中；② 大小写不敏感（按 map 遍历）。
//
//	参考实现原样使用 `name`（未 trim）—— 故 `" Read"` 也不命中。
//
// 返回 `(fn, true)` 表示命中，`(nil, false)` 表示无 shim。
func resolveToolCallShim(name string) (shimFn, bool) {
	if name == "" {
		// `typeof name !== "string" || !name` —— 空串在 JS 里是假值。
		return nil, false
	}
	// ① 精确匹配
	if fn, ok := toolShims[name]; ok {
		return fn, true
	}
	// ② 大小写不敏感
	lower := strings.ToLower(name)
	for key, fn := range toolShims {
		if strings.ToLower(key) == lower {
			return fn, true
		}
	}
	return nil, false
}

// hasToolCallShim 照抄 toolCallShim.ts 的 `export function hasToolCallShim`。
//
// 供流式路径判断"这条工具调用是否需要缓冲参数、攒完再一次性清洗"。
func hasToolCallShim(name string) bool {
	_, ok := resolveToolCallShim(name)
	return ok
}

// applyToolCallShimToBuffer 照抄 toolCallShim.ts:
//
//	/**
//	 * Apply the registered shim for a tool call's raw assembled arguments string.
//	 * Returns a stringified JSON value safe to emit as input_json_delta.partial_json.
//	 * If the buffer is unparseable, returns the empty-object JSON `{}` after applying
//	 * the shim with `{}` as input (so required arrays still get injected).
//	 */
//	export function applyToolCallShimToBuffer(name: string, raw: string): string {
//	  const shim = resolveToolCallShim(name);
//	  if (!shim) return raw;
//	  let parsed: unknown;
//	  try { parsed = raw && raw.length > 0 ? JSON.parse(raw) : {}; }
//	  catch { parsed = {}; }
//	  const patched = shim(parsed);
//	  return JSON.stringify(patched);
//	}
//
// ★ 无 shim -> **原样返回 raw**（探针 S9），不做任何 JSON 往返。
// ★ 有 shim 但 raw 空/不可 parse -> 以 `{}` 为输入（探针 S10）。
//
// ★ Go 的 `json.Marshal` 按字典序输出键，Node 的 `JSON.stringify` 保持插入顺序
//
//	（探针 S12 实测差异）。键序对 JSON 语义无影响，故接受该差异；
//	若有需要逐字节比对的场景，请用逐字段断言（见测试）。
func applyToolCallShimToBuffer(name, raw string) string {
	shim, ok := resolveToolCallShim(name)
	if !ok {
		return raw
	}
	var parsed any = map[string]any{}
	if raw != "" {
		var v any
		if err := json.Unmarshal([]byte(raw), &v); err == nil {
			parsed = v
		}
		// parse 失败时保持 `{}`（与参考实现的 catch 分支一致）。
	}
	patched := shim(parsed)
	b, err := json.Marshal(patched)
	if err != nil {
		// 参考实现此处不会失败（JSON.stringify 对任意值都有定义）。
		// 唯一能让 Go 失败的是不可序列化类型，而本函数只处理 JSON 反序列化出来的值。
		return "{}"
	}
	return string(b)
}
