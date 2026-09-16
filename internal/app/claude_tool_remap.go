package app

import (
	"os"
	"regexp"
	"strconv"
	"strings"
)

// 逐字照抄 OmniRoute:
//
//	open-sse/services/claudeCodeToolRemapper.ts（483 行）
//	open-sse/services/claudeCodeExtraRemap.ts（18 行）
//
// 参考实现文件头原注释（逐字）:
//
//	/**
//	 * Claude Code tool name remapping.
//	 *
//	 * Anthropic uses tool name fingerprinting to detect third-party clients.
//	 * Real Claude Code uses TitleCase tool names (Bash, Read, Write, etc.)
//	 * while third-party clients like OpenCode use lowercase.
//	 *
//	 * This module remaps tool names in both directions:
//	 * - Request path: lowercase → TitleCase (before sending to Anthropic)
//	 * - Response path: TitleCase → lowercase (for clients expecting lowercase)
//	 */
//
// # 为什么这条对本网关重要（本文件最核心的业务价值）
//
// Anthropic 在**第一方 Messages API**（原生 Claude OAuth 路径）上用**工具名指纹**
// 识别第三方 agent harness。原注释点名了两种失败模式，二者对外都表现为一条
// **误导性的 `400 out of extra usage` 占位错误**（SSE 流被直接拒绝，并非真实计费事件）:
//
//	1. 特定黑名单名字（例如 `mixture_of_agents`）**单独出现**就被拒;
//	2. 足够大的一**组**可识别的 snake_case agent 工具名会被**集体**拒绝，
//	   哪怕每个名字单独看都能过。
//
// 本网关从 Codex / OpenCode / Cline 收来的历史里，工具名普遍是 snake_case
// （`read_file` / `run_command` / `list_directory`）。不伪装 → 上游拒绝服务 →
// 客户端只看到"莫名中断"或"额度用尽"。
//
// # 三条形态规则（探针 R1–R6 逐条锁定）
//
//   - `isAnthropicServerToolType`：Anthropic **服务端工具**的 `name` 是与其 `type`
//     绑定的保留字面量（`web_search_20250305` 必须配 `name: "web_search"`）。
//     这类名字**任何改写都不能碰** —— 只改一侧会得到
//     `Tool 'WebSearch' not found in provided tools`（改了历史、没改声明）或
//     `tools.N.<type>.name: Input should be '<literal>'`（改了声明）。
//     判定 = **版本化形态**（`^[a-z][a-z0-9_]*_\d{8}$`）或非版本化的两个别名。
//   - `needsThirdPartyCloak`：已经是"真 Claude Code 形态"（PascalCase 单词、
//     无分隔符）的名字**不动**；`mcp__<server>__<tool>` 命名空间**整段豁免**
//     （伪装它会造成 round-trip 不对称 → `Tool reference 'mcp__…' not found` 400）。
//     其余（首字母小写 / 含 `_` / 含 `-`）一律伪装。
//   - `_toolNameMap`：per-request 的"别名 → 原名"映射，用于响应路径还原。
//     参考实现用 `Object.defineProperty(..., enumerable: false)` 挂到 body 上,
//     保证它**不会**序列化进上行请求体（否则 Anthropic 400
//     `Extra inputs are not permitted`）—— 另有注释明确禁止设置
//     `_claudeCodeRequiresLowercaseToolNames`（无读者、且会泄漏进 body）。
//
// # 反直觉的权威值（探针实测，禁止"顺手修正"）
//
//   - 别名冲突时追加**数字后缀**（`read_file` + 已占用的 `Read` → `Read2`），
//     **不是**改用 `ReadFile`（R6b）。
//   - 全 PascalCase 时**不**附加 `_toolNameMap`（R6d `hasMap=false`）—— 惰性创建。
//   - `toPascalCase("___")` 返回**原串** `___`（`pascal || name` 兜底）（R6l）。
//   - `remapToolNamesInRequest` 返回 `hasLowercase && !hasTitleCase` —— 混用时
//     返回 **false**（R3c），它不是"是否有改动"的意思。

// ──────────────────── 映射表（照抄 claudeCodeExtraRemap.ts + claudeCodeToolRemapper.ts:15-65）────────────────────

// toolRenameMap 对应参考实现的 `TOOL_RENAME_MAP`（`...EXTRA_TOOL_RENAME_MAP` 在前，
// 故 `subagents` / `session_status` 两条**不会**被后面的同名键覆盖）。
//
// ★ 键名全是**小写原形**，值是**发给 Anthropic 的 TitleCase 别名**。
var toolRenameMap = map[string]string{
	// claudeCodeExtraRemap.ts:15-18 —— "Extra tool name remapping for third-party
	// agent detection bypass. Anthropic detects non-Claude-Code clients by checking
	// for specific tool names that only third-party agents use (e.g. subagents,
	// session_status). This module adds aliases for those tool names so they look
	// like legitimate Claude Code tools."
	"subagents":      "SubDispatch",
	"session_status": "CheckStatus",

	// claudeCodeToolRemapper.ts:17-64
	"bash":             "Bash",
	"read":             "Read",
	"write":            "Write",
	"edit":             "Edit",
	"glob":             "Glob",
	"grep":             "Grep",
	"task":             "Task",
	"agent":            "Agent",
	"webfetch":         "WebFetch",
	"websearch":        "WebSearch",
	"todowrite":        "TodoWrite",
	"todoread":         "TodoRead",
	"question":         "Question",
	"askuserquestion":  "AskUserQuestion",
	"skill":            "Skill",
	"slashcommand":     "SlashCommand",
	"multiedit":        "MultiEdit",
	"notebook":         "Notebook",
	"notebookedit":     "NotebookEdit",
	"notebookread":     "NotebookRead",
	"lsp":              "Lsp",
	"apply_patch":      "ApplyPatch",
	"applypatch":       "ApplyPatch",
	"bashoutput":       "BashOutput",
	"killshell":        "KillShell",
	"killbash":         "KillBash",
	"enterplanmode":    "EnterPlanMode",
	"exitplanmode":     "ExitPlanMode",
	"enterworktree":    "EnterWorktree",
	"exitworktree":     "ExitWorktree",
	"artifact":         "Artifact",
	"designsync":       "DesignSync",
	"monitor":          "Monitor",
	"sendmessage":      "SendMessage",
	"listagents":       "ListAgents",
	"pushnotification": "PushNotification",
	"reportfindings":   "ReportFindings",
	"schedulewakeup":   "ScheduleWakeup",
	"croncreate":       "CronCreate",
	"crondelete":       "CronDelete",
	"cronlist":         "CronList",
	"taskoutput":       "TaskOutput",
	"taskstop":         "TaskStop",
	"taskcreate":       "TaskCreate",
	"taskupdate":       "TaskUpdate",
	"tasklist":         "TaskList",
	"taskget":          "TaskGet",
	"workflow":         "Workflow",
}

// toolReverseMap 照抄 `const REVERSE_MAP: Record<string, string> = {};
// for (const [k, v] of Object.entries(TOOL_RENAME_MAP)) REVERSE_MAP[v] = k;`
//
// ★ 后写覆盖先写 —— Go 里按同样的遍历顺序手工展开。注意有两个键**映射到同一个
// 值** `ApplyPatch`（`apply_patch` 与 `applypatch`），参考实现里后者后遍历到，
// 故 `REVERSE_MAP["ApplyPatch"] == "applypatch"`。此处照抄该结果。
var toolReverseMap = map[string]string{
	"SubDispatch":     "subagents",
	"CheckStatus":     "session_status",
	"Bash":            "bash",
	"Read":            "read",
	"Write":           "write",
	"Edit":            "edit",
	"Glob":            "glob",
	"Grep":            "grep",
	"Task":            "task",
	"Agent":           "agent",
	"WebFetch":        "webfetch",
	"WebSearch":       "websearch",
	"TodoWrite":       "todowrite",
	"TodoRead":        "todoread",
	"Question":        "question",
	"AskUserQuestion": "askuserquestion",
	"Skill":           "skill",
	"SlashCommand":    "slashcommand",
	"MultiEdit":       "multiedit",
	"Notebook":        "notebook",
	"NotebookEdit":    "notebookedit",
	"NotebookRead":    "notebookread",
	"Lsp":             "lsp",
	// ★ `ApplyPatch` 的归属: 参考实现 `apply_patch` 先、`applypatch` 后，
	//   故终值是后者。探针 R5j 实测 `restoreClaudeToolName("applypatch", {})`
	//   走 canonical 分支得 `ApplyPatch`，与这里一致。
	"ApplyPatch":       "applypatch",
	"BashOutput":       "bashoutput",
	"KillShell":        "killshell",
	"KillBash":         "killbash",
	"EnterPlanMode":    "enterplanmode",
	"ExitPlanMode":     "exitplanmode",
	"EnterWorktree":    "enterworktree",
	"ExitWorktree":     "exitworktree",
	"Artifact":         "artifact",
	"DesignSync":       "designsync",
	"Monitor":          "monitor",
	"SendMessage":      "sendmessage",
	"ListAgents":       "listagents",
	"PushNotification": "pushnotification",
	"ReportFindings":   "reportfindings",
	"ScheduleWakeup":   "schedulewakeup",
	"CronCreate":       "croncreate",
	"CronDelete":       "crondelete",
	"CronList":         "cronlist",
	"TaskOutput":       "taskoutput",
	"TaskStop":         "taskstop",
	"TaskCreate":       "taskcreate",
	"TaskUpdate":       "taskupdate",
	"TaskList":         "tasklist",
	"TaskGet":          "taskget",
	"Workflow":         "workflow",
}

// claudeBuiltinToolNames 照抄 `const CLAUDE_BUILTIN_TOOL_NAMES = new Set(Object.values(TOOL_RENAME_MAP));`
var claudeBuiltinToolNames = func() map[string]bool {
	m := make(map[string]bool, len(toolRenameMap))
	for _, v := range toolRenameMap {
		m[v] = true
	}
	return m
}()

// harnessCanonicalMap 照抄 claudeCodeToolRemapper.ts:304-317。
var harnessCanonicalMap = map[string]string{
	"read_file":      "Read",
	"write_file":     "Write",
	"search_files":   "Grep",
	"grep_search":    "Grep",
	"list_directory": "Glob",
	"run_command":    "Bash",
	"terminal":       "Bash",
	"todo":           "TodoWrite",
	"todo_write":     "TodoWrite",
	"todo_read":      "TodoRead",
	"patch":          "Edit",
	"multi_edit":     "MultiEdit",
}

// versionedServerToolType 照抄 `const VERSIONED_SERVER_TOOL_TYPE = /^[a-z][a-z0-9_]*_\d{8}$/;`
var versionedServerToolType = regexp.MustCompile(`^[a-z][a-z0-9_]*_\d{8}$`)

// nonVersionedServerToolTypes 照抄 `new Set(["web_search", "web_search_preview"])`。
var nonVersionedServerToolTypes = map[string]bool{
	"web_search":         true,
	"web_search_preview": true,
}

// isAnthropicServerToolType 照抄 claudeCodeToolRemapper.ts:358-361。
//
// ★ 参考实现原注释（逐字）:
//
//	/**
//	 * Anthropic server-side tool types whose `name` is a reserved literal that the
//	 * Messages API validates exactly (e.g. type `web_search_20250305` REQUIRES
//	 * `name: "web_search"`). These look like third-party harness tools to the name
//	 * cloak (`web_search` has a `_`, fails the PascalCase test) so without this
//	 * guard the cloak rewrites the name to `WebSearch` and Anthropic 400s with
//	 * `tools.N.web_search_20250305.name: Input should be 'web_search'`.
//	 *
//	 * Detection mirrors the codebase's existing convention: a versioned built-in
//	 * tool type carries an 8-digit date suffix (`web_search_20250305`,
//	 * `code_execution_20250522`, `bash_20250124`, …).
//	 */
//
// 探针 R2: `""`/null/undefined → false；`websearch_20250305` → **true**
// （只按日期后缀形态判，不校验前缀是不是已知工具名）；`tool_2025030`
// （7 位）→ false；`Web_Search_20250305`（含大写）→ false。
func isAnthropicServerToolType(t any) bool {
	s, ok := t.(string)
	if !ok || len(s) == 0 {
		return false
	}
	return versionedServerToolType.MatchString(s) || nonVersionedServerToolTypes[s]
}

// needsThirdPartyCloak 照抄 claudeCodeToolRemapper.ts:329-339。
//
// 参考实现原注释（逐字）:
//
//	/**
//	 * A name is left untouched when it already reads as a genuine Claude Code tool:
//	 * a PascalCase single token with no separators (Bash, Read, TodoWrite).
//	 */
//
// 以及 `mcp__` 豁免的注释:
//
//	// `mcp__<server>__<tool>` names are genuine Claude Code MCP tool names that
//	// Anthropic accepts natively. Cloaking them to PascalCase is unnecessary and,
//	// via round-trip asymmetry (a history tool_use keeping the original name while
//	// tools[] is cloaked), produces "Tool reference 'mcp__…' not found in available
//	// tools" 400s on the native claude OAuth path. Leave the MCP namespace alone.
//
// 探针 R1 逐个锁定。★ `ApplyPatch`（已是映射表的值）→ false；`applypatch`（键）
// → **true**（首字母小写即判）。
func needsThirdPartyCloak(name string) bool {
	if name == "" {
		return false
	}
	if claudeBuiltinToolNames[name] {
		return false
	}
	if strings.HasPrefix(name, "mcp__") {
		return false
	}
	// `/[a-z]/.test(name.charAt(0)) || name.includes("_") || name.includes("-")`
	//
	// ★ 三条件任一成立即为 true。注意第一条只判**首字符是否小写字母**，
	//   所以 `_x` / `-x` 也命中（靠后两条）。
	first := name[0]
	if first >= 'a' && first <= 'z' {
		return true
	}
	return strings.ContainsAny(name, "_-")
}

// toPascalCaseToolName 照抄 claudeCodeToolRemapper.ts:319-323。
//
//	function toPascalCaseToolName(name: string): string {
//	  const parts = name.split(/[_\s-]+/).filter(Boolean);
//	  const pascal = parts.map((p) => p.charAt(0).toUpperCase() + p.slice(1)).join("");
//	  return pascal || name;
//	}
//
// 探针 R6l: `___` → 原串 `___`（`filter(Boolean)` 后无片段 → `pascal === ""`
// → 落到 `|| name`）。
func toPascalCaseToolName(name string) string {
	parts := splitToolNameSeparators(name)
	var sb strings.Builder
	for _, p := range parts {
		if p == "" {
			continue
		}
		// `p.charAt(0).toUpperCase() + p.slice(1)`
		sb.WriteString(strings.ToUpper(p[:1]))
		sb.WriteString(p[1:])
	}
	pascal := sb.String()
	if pascal == "" {
		return name
	}
	return pascal
}

// splitToolNameSeparators 复刻 `name.split(/[_\s-]+/)` 后 `.filter(Boolean)` 的效果。
//
// ★ 注意 `String.prototype.split` 遇连续分隔符会产生空串, 由 `filter(Boolean)` 滤掉;
// 这里直接跳过空片段, 语义等价。
func splitToolNameSeparators(name string) []string {
	var out []string
	var cur strings.Builder
	for _, r := range name {
		if r == '_' || r == '-' || r == ' ' || r == '\t' || r == '\n' || r == '\r' ||
			r == '\v' || r == '\f' || r == '\u00a0' || r == '\u2028' || r == '\u2029' {
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
			continue
		}
		cur.WriteRune(r)
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// ──────────────────── _toolNameMap（per-request 别名映射）────────────────────

// toolNameMap 对位参考实现挂在 body 上的 `_toolNameMap`（一个 `Map<string,string>`）。
//
// ★ 关键差异: 参考实现用 `Object.defineProperty(..., { enumerable: false })`
//
//	挂到 body 上, 使 `JSON.stringify(body)` **不会**带上它。Go 侧把它挂在
//	`params` 里必须显式 delete(见 attachToolNameMap 的说明), 否则会随
//	`json.Marshal(params)` 发给上游, Anthropic 400
//	`Extra inputs are not permitted`。
//
// 查找顺序与 `Map.get` 一致（命中即止）；遍历顺序与插入顺序一致（Go 的 map
// 无序，故用切片保序 —— 参考实现的 `Map` 是插入序，`restoreClaudeToolName`
// 的"首个大小写不敏感匹配"依赖该顺序）。
type toolNameMap struct {
	entries []toolNameEntry
}

type toolNameEntry struct {
	alias  string
	origin string
}

func (m *toolNameMap) len() int {
	if m == nil {
		return 0
	}
	return len(m.entries)
}

// set 对应 `map.set(k, v)` —— 同键覆盖（保持首次插入位置，与 JS Map 一致）。
func (m *toolNameMap) set(alias, origin string) {
	for i := range m.entries {
		if m.entries[i].alias == alias {
			m.entries[i].origin = origin
			return
		}
	}
	m.entries = append(m.entries, toolNameEntry{alias: alias, origin: origin})
}

// get 对应 `map.get(k)`。
func (m *toolNameMap) get(alias string) (string, bool) {
	if m == nil {
		return "", false
	}
	for i := range m.entries {
		if m.entries[i].alias == alias {
			return m.entries[i].origin, true
		}
	}
	return "", false
}

// keys 对应 `[...map.keys()]`（插入序）。
func (m *toolNameMap) keys() []string {
	if m == nil {
		return nil
	}
	out := make([]string, 0, len(m.entries))
	for _, e := range m.entries {
		out = append(out, e.alias)
	}
	return out
}

// toolNameMapKey 是 body/params 上承载该映射的键名（对位参考实现的 `_toolNameMap`）。
//
// ★ 它必须以 `_` 开头且在上行前被剥离 —— 见 detachToolNameMap。
const toolNameMapKey = "_toolNameMap"

// toolNameMapSideChannelKey 是 Go 侧专有的**进程内旁路键**，参考实现没有对应物。
//
// 为什么需要它: 参考实现把 `_toolNameMap` 用 `Object.defineProperty(..., {
// enumerable: false })` 挂在同一个 body 对象上，让 `JSON.stringify` 自然忽略、
// 而 `chatCore` 仍能从同一个对象上读到。Go 的 `json.Marshal` 不认 "non-enumerable"，
// 要么删掉（读不到）、要么留着（会发上线）。
//
// 解法: 在出站咽喉处先 `detachToolNameMap(params)` 把映射摘下来，转存到这个
// 独立键；每次 `json.Marshal(params)` **之前**再 delete 掉本键，marshal 之后再回填。
// 于是它对线序化永远不可见，而调用方仍能在 `Chat()` 返回后从同一个 params map
// 里读到它 —— 与参考实现"同对象读写"的语义等价。
//
// 键名带 `__go` 前缀，避免与参考实现可能新增的 `_` 前缀字段撞名。
const toolNameMapSideChannelKey = "__goToolNameMap"

// ──────────────────── remapToolNamesInRequest ────────────────────

// remapToolNamesInRequest 照抄 claudeCodeToolRemapper.ts:113-185。
//
// 返回值: `hasLowercase && !hasTitleCase`。
//
// ★ 返回值的语义**不是**"是否发生过改动" —— 混用（既有小写又有 TitleCase）时
// 返回 **false**，尽管工具名确实被改写了（探针 R3c）。照抄时不要"顺手修正"
// 成"有改动即 true"，调用方依赖的是这个组合条件。
func remapToolNamesInRequest(body map[string]any) bool {
	hasLowercase := false
	hasTitleCase := false
	serverToolNames := collectServerToolNames(body["tools"])

	// Remap tool definitions
	tools, _ := body["tools"].([]any)
	for _, toolRaw := range tools {
		tool, ok := toolRaw.(map[string]any)
		if !ok || tool == nil {
			continue
		}
		// Server tools (bash_20250124 / web_search_20250305 / …) keep their
		// type-bound literal name.
		if isAnthropicServerToolType(tool["type"]) {
			continue
		}
		// `String(tool.name || "")` —— 缺失/null/false 都变空串。
		name := jsStringOrEmptyOrFalse(tool["name"])
		if mapped, ok := toolRenameMap[name]; ok {
			tool["name"] = mapped
			trackToolName(body, mapped, name)
			hasLowercase = true
		} else if _, ok := toolReverseMap[name]; ok {
			hasTitleCase = true
		}
	}

	// Remap tool_result references in messages
	messages, _ := body["messages"].([]any)
	for _, msgRaw := range messages {
		msg, ok := msgRaw.(map[string]any)
		if !ok {
			continue
		}
		content, _ := msg["content"].([]any)
		if content == nil {
			continue
		}
		for _, blockRaw := range content {
			block, ok := blockRaw.(map[string]any)
			if !ok {
				continue
			}
			if blockType(block) != "tool_use" {
				continue
			}
			name, isStr := block["name"].(string)
			if !isStr {
				continue
			}
			if serverToolNames[name] {
				continue
			}
			if mapped, ok := toolRenameMap[name]; ok {
				block["name"] = mapped
				trackToolName(body, mapped, name)
				hasLowercase = true
			} else if _, ok := toolReverseMap[name]; ok {
				hasTitleCase = true
			}
		}
	}

	// Remap tool_choice
	toolChoice, _ := body["tool_choice"].(map[string]any)
	if toolChoice != nil {
		if tcType, _ := toolChoice["type"].(string); tcType == "tool" {
			if name, ok := toolChoice["name"].(string); ok && !serverToolNames[name] {
				if mapped, ok := toolRenameMap[name]; ok {
					toolChoice["name"] = mapped
					trackToolName(body, mapped, name)
					hasLowercase = true
				} else if _, ok := toolReverseMap[name]; ok {
					hasTitleCase = true
				}
			}
		}
	}

	// NOTE: do not set body._claudeCodeRequiresLowercaseToolNames here.
	// The flag has no readers and would leak into the outgoing Anthropic
	// request body, causing HTTP 400 (Extra inputs are not permitted).
	// The response-side remap is unconditional via remapToolNamesInResponse.

	return hasLowercase && !hasTitleCase
}

// collectServerToolNames 照抄 claudeCodeToolRemapper.ts:101-111。
func collectServerToolNames(tools any) map[string]bool {
	names := map[string]bool{}
	arr, ok := tools.([]any)
	if !ok {
		return names
	}
	for _, toolRaw := range arr {
		t, ok := toolRaw.(map[string]any)
		if !ok || t == nil {
			continue
		}
		if isAnthropicServerToolType(t["type"]) {
			if s, ok := t["name"].(string); ok {
				names[s] = true
			}
		}
	}
	return names
}

// getRequestToolNameMap 照抄 claudeCodeToolRemapper.ts:72-81（惰性创建）。
func getRequestToolNameMap(body map[string]any) *toolNameMap {
	if existing, ok := body[toolNameMapKey].(*toolNameMap); ok && existing != nil {
		return existing
	}
	m := &toolNameMap{}
	body[toolNameMapKey] = m
	return m
}

// trackToolName 照抄 claudeCodeToolRemapper.ts:83-89。
func trackToolName(body map[string]any, titleCaseName, originalName string) {
	getRequestToolNameMap(body).set(titleCaseName, originalName)
}

// jsStringOrEmptyOrFalse 复刻 `String(v || "")` —— 先把 falsy 归成 `""` 再取字符串。
//
// ★ 与既有 `jsStringOrEmpty`（复刻 `String(v ?? "")`）不同: 这里 `false` / `0` /
//
//	`NaN` 也是 falsy, 会被归成空串。用于 `String(tool.name || "")`。
func jsStringOrEmptyOrFalse(v any) string {
	if !jsTruthy(v) {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return jsStringOf(v)
}

// ──────────────────── remapToolNamesInResponse ────────────────────

// remapToolNamesInResponse 照抄 claudeCodeToolRemapper.ts:187-206。
//
// ★ 参考实现的两条 `replaceAll` 分别匹配**无空格**与**有空格**两种 JSON 书写，
//
//	顺序固定（先无空格）。这里逐条搬。
func remapToolNamesInResponse(text string, forceLowercase bool, nameMap *toolNameMap) string {
	if !forceLowercase {
		return text
	}

	if nameMap.len() > 0 {
		for _, e := range nameMap.entries {
			text = strings.ReplaceAll(text,
				`"name":"`+e.alias+`"`, `"name":"`+e.origin+`"`)
			text = strings.ReplaceAll(text,
				`"name": "`+e.alias+`"`, `"name": "`+e.origin+`"`)
		}
	}
	for titleCase, lower := range toolReverseMap {
		text = strings.ReplaceAll(text,
			`"name":"`+titleCase+`"`, `"name":"`+lower+`"`)
		text = strings.ReplaceAll(text,
			`"name": "`+titleCase+`"`, `"name": "`+lower+`"`)
	}
	return text
}

// ──────────────────── restoreClaudeToolName ────────────────────

// restoreClaudeToolName 照抄 claudeCodeToolRemapper.ts:229-279。
//
// 参考实现原注释（逐字）:
//
//	/**
//	 * Restore a tool name for Claude-format clients (#9008).
//	 *
//	 * Preference order:
//	 * 1. Exact `_toolNameMap` hit where the value differs from the key
//	 *    (sanitized → original request-side alias)
//	 * 2. Canonical casing upgrade for known Claude Code tools
//	 *    (`croncreate` → `CronCreate`, `bash` → `Bash`, …)
//	 * 3. Case-insensitive non-identity match against map keys/values
//	 *    (Gemini/Antigravity may echo a lowercased name for a PascalCase
//	 *    Claude Code tool)
//	 * 4. Identity echo kept ONLY when no canonical upgrade exists
//	 * 5. No-map fallbacks: REVERSE_MAP TitleCase → lowercase (#7926 XML /
//	 *    OpenCode-style lowercase tools), then the static table
//	 *
//	 * Identity entries (key === value) never pin a known tool below its
//	 * canonical casing. Some upstream gateways echo the very lowercase name
//	 * they emitted into the alias channel; honouring that echo is what let a
//	 * literal `croncreate` reach Claude Code as an unknown tool even though
//	 * the request declared `CronCreate`.
//	 */
func restoreClaudeToolName(rawName string, nameMap *toolNameMap) string {
	if rawName == "" {
		return rawName
	}

	// Undefined when rawName already IS the canonical form — an input that
	// maps to itself must keep flowing to the #7926 legacy paths below.
	lower := strings.ToLower(rawName)
	canonicalRaw, canonicalRawOK := toolRenameMap[lower]
	canonical := ""
	if canonicalRawOK && canonicalRaw != rawName {
		canonical = canonicalRaw
	}

	if nameMap.len() > 0 {
		exact, hasExact := nameMap.get(rawName)
		if hasExact && (exact != rawName || canonical == "") {
			return exact
		}

		identityMatch := ""
		foundIdentity := false
		for _, e := range nameMap.entries {
			if strings.ToLower(e.alias) != lower && strings.ToLower(e.origin) != lower {
				continue
			}
			if e.origin != rawName {
				return e.origin
			}
			identityMatch = e.origin
			foundIdentity = true
		}
		if foundIdentity && canonical == "" {
			return identityMatch
		}
	}

	// Canonical echo is terminal: when the upstream echoes back the exact
	// canonical form the request declared, keep it verbatim.
	if canonicalRawOK && canonicalRaw == rawName {
		return rawName
	}

	if canonical != "" {
		return canonical
	}

	// When no request toolNameMap is provided (e.g. non-Claude client):
	// If rawName is already TitleCase, apply REVERSE_MAP for #7926 backward
	// compatibility (Bash → bash).
	if nameMap.len() == 0 {
		if rev, ok := toolReverseMap[rawName]; ok {
			return rev
		}
	}

	if rev, ok := toolReverseMap[rawName]; ok {
		return rev
	}
	return rawName
}

// ──────────────────── cloakThirdPartyToolNames ────────────────────

// cloakThirdPartyToolNames 照抄 claudeCodeToolRemapper.ts:373-483。
//
// 参考实现原注释（逐字）:
//
//	/**
//	 * Anthropic fingerprints third-party agent harnesses by their tool NAMES on the
//	 * first-party Messages API (native Claude OAuth). Two failure modes, both
//	 * surfaced as a misleading `400 out of extra usage` placeholder (the SSE stream
//	 * is refused, not a real billing event):
//	 *   1. Specific blacklisted names (e.g. `mixture_of_agents`) are refused even in
//	 *      isolation.
//	 *   2. A large enough SET of recognizable snake_case agent tool names is
//	 *      refused collectively, even though each name passes on its own.
//	 *
//	 * `remapToolNamesInRequest` only normalizes the fixed set of Claude Code tool
//	 * names. This generalizes that cloak: any tool name that does not already look
//	 * like a genuine Claude Code tool (PascalCase, no separators) is deterministically
//	 * aliased — to its Claude Code canonical equivalent when one exists, otherwise to
//	 * a PascalCase form of the original. The per-request alias is tracked in the
//	 * non-enumerable `_toolNameMap`, so `remapToolNamesInResponse` restores the
//	 * caller's original names transparently. Disable with
//	 * `CLAUDE_DISABLE_TOOL_NAME_CLOAK=true`.
//	 */
//
// `skip` 对位参考实现的 `CloakOptions.skip`:
//
//	/**
//	 * Names matching this predicate are left untouched, so a caller that owns a
//	 * more specific rewrite keeps authority over them and the two reverse
//	 * maps stay disjoint / single-hop.
//	 */
//
// 返回 per-request 的 `_toolNameMap`（惰性创建：无任何 cloak 时**不**附加该键，
// 探针 R6d）。
func cloakThirdPartyToolNames(body map[string]any, skip func(string) bool) *toolNameMap {
	// Operator kill-switch (documented in .env.example / ENVIRONMENT.md). Checked
	// here so every call site — native base.ts AND the CLIProxyAPI executor —
	// honours it, rather than each caller having to remember to guard.
	if claudeDisableToolNameCloak() {
		return &toolNameMap{}
	}
	shouldCloak := func(name string) bool {
		return needsThirdPartyCloak(name) && !(skip != nil && skip(name))
	}
	tools, _ := body["tools"].([]any)
	serverToolNames := collectServerToolNames(tools)

	used := map[string]bool{}
	for _, toolRaw := range tools {
		if tool, ok := toolRaw.(map[string]any); ok {
			if s, ok := tool["name"].(string); ok {
				used[s] = true
			}
		}
	}
	existingMap, hasExisting := body[toolNameMapKey].(*toolNameMap)
	if !hasExisting || existingMap == nil {
		existingMap = nil
	} else {
		for _, alias := range existingMap.keys() {
			used[alias] = true
		}
	}

	// Created lazily so genuine Claude Code traffic (nothing to cloak) does not
	// get an empty _toolNameMap attached to the request body.
	nameMap := existingMap
	assigned := map[string]string{} // original -> alias

	aliasFor := func(original string) string {
		if existing, ok := assigned[original]; ok {
			return existing
		}
		// Prefer the established Claude Code rename maps (TOOL_RENAME_MAP spreads
		// EXTRA_TOOL_RENAME_MAP) so the CPA path matches the native path exactly:
		// subagents->SubDispatch, session_status->CheckStatus, webfetch->WebFetch, …
		// Then harness-canonical (read_file->Read), then a generic PascalCase.
		base := ""
		if v, ok := toolRenameMap[original]; ok {
			base = v
		} else if v, ok := harnessCanonicalMap[original]; ok {
			base = v
		} else {
			base = toPascalCaseToolName(original)
		}
		alias := base
		suffix := 2
		for alias != original && used[alias] {
			alias = base + itoa(suffix)
			suffix++
		}
		delete(used, original)
		used[alias] = true
		assigned[original] = alias
		if nameMap == nil {
			nameMap = getRequestToolNameMap(body)
		}
		nameMap.set(alias, original)
		return alias
	}

	// Non-mutating: clone changed entries rather than rewriting the caller's
	// objects in place (mirrors applyMcpToolNameRewrite — transformRequest must
	// not corrupt an input body that may be logged or replayed on fallback).
	if tools != nil {
		newTools := make([]any, 0, len(tools))
		for _, toolRaw := range tools {
			tool, ok := toolRaw.(map[string]any)
			if !ok {
				newTools = append(newTools, toolRaw)
				continue
			}
			// Never rewrite the reserved name of an Anthropic server-side tool — its
			// `type` (web_search_20250305, …) binds the API to an exact `name`.
			if isAnthropicServerToolType(tool["type"]) {
				newTools = append(newTools, tool)
				continue
			}
			if name, ok := tool["name"].(string); ok && shouldCloak(name) {
				cloned := shallowCopyStringAny(tool)
				cloned["name"] = aliasFor(name)
				newTools = append(newTools, cloned)
				continue
			}
			newTools = append(newTools, tool)
		}
		body["tools"] = newTools
	}

	messages, _ := body["messages"].([]any)
	if messages != nil {
		newMessages := make([]any, 0, len(messages))
		for _, msgRaw := range messages {
			message, ok := msgRaw.(map[string]any)
			if !ok {
				newMessages = append(newMessages, msgRaw)
				continue
			}
			content, hasContent := message["content"].([]any)
			if !hasContent {
				newMessages = append(newMessages, message)
				continue
			}
			changed := false
			newContent := make([]any, 0, len(content))
			for _, blockRaw := range content {
				block, ok := blockRaw.(map[string]any)
				if !ok {
					newContent = append(newContent, blockRaw)
					continue
				}
				if blockType(block) == "tool_use" {
					if name, ok := block["name"].(string); ok &&
						!serverToolNames[name] && shouldCloak(name) {
						changed = true
						cloned := shallowCopyStringAny(block)
						cloned["name"] = aliasFor(name)
						newContent = append(newContent, cloned)
						continue
					}
				}
				newContent = append(newContent, blockRaw)
			}
			if changed {
				clonedMsg := shallowCopyStringAny(message)
				clonedMsg["content"] = newContent
				newMessages = append(newMessages, clonedMsg)
			} else {
				newMessages = append(newMessages, message)
			}
		}
		body["messages"] = newMessages
	}

	toolChoice, _ := body["tool_choice"].(map[string]any)
	if toolChoice != nil {
		if tcType, _ := toolChoice["type"].(string); tcType == "tool" {
			if name, ok := toolChoice["name"].(string); ok &&
				!serverToolNames[name] && shouldCloak(name) {
				cloned := shallowCopyStringAny(toolChoice)
				cloned["name"] = aliasFor(name)
				body["tool_choice"] = cloned
			}
		}
	}

	if nameMap == nil {
		return &toolNameMap{}
	}
	return nameMap
}

// claudeDisableToolNameCloak 对位参考实现的
// `process.env.CLAUDE_DISABLE_TOOL_NAME_CLOAK === "true"`。
//
// 参考实现原注释: `// Operator kill-switch (documented in .env.example /
// ENVIRONMENT.md). Checked here so every call site — native base.ts AND the
// CLIProxyAPI executor — honours it, rather than each caller having to
// remember to guard.`
func claudeDisableToolNameCloak() bool {
	return osGetenv("CLAUDE_DISABLE_TOOL_NAME_CLOAK") == "true"
}

// detachToolNameMap 把 `_toolNameMap` 从 params 里**摘除**。
//
// ★ 这是 Go 侧必须补的一步，参考实现靠 `enumerable: false` 实现同一目的。
//
//	若不摘，`json.Marshal(params)` 会把它序列化进上行 body，Anthropic 400
//	`Extra inputs are not permitted`（对比参考实现注释里对
//	`_claudeCodeRequiresLowercaseToolNames` 的同类警告）。
//
// 返回摘下来的映射（调用方留在内存里供响应路径使用）。
func detachToolNameMap(params map[string]any) *toolNameMap {
	m, _ := params[toolNameMapKey].(*toolNameMap)
	delete(params, toolNameMapKey)
	return m
}

// takeToolNameMap 读出并**移除**旁路键里的映射，供响应侧还原工具名。
//
// 对位参考实现的 `const toolNameMap = translatedBody._toolNameMap; delete
// translatedBody._toolNameMap;`（chatCore.ts:2592-2610）—— 同样是"读出即摘"，
// 避免同一次请求的映射泄漏到下一次。
//
// 返回 nil 表示本次请求没有任何伪装（例如纯 PascalCase 的真 Claude Code 流量），
// 调用方应据此跳过还原。
func takeToolNameMap(params map[string]any) *toolNameMap {
	m, _ := params[toolNameMapSideChannelKey].(*toolNameMap)
	delete(params, toolNameMapSideChannelKey)
	return m
}

// caseInsensitiveToolNameLookup 照抄 toolCallHelper.ts:100-129（大小写不敏感查表）。
//
// 参考实现原注释（逐字）:
//
//	/**
//	 * Case-insensitive tool name lookup. Upstreams (Gemini/Antigravity, some
//	 * relays) may echo a lowercased form of a name they were sent in PascalCase
//	 * — e.g. an alias channel keyed `Read` is echoed back as `read`. An exact
//	 * `Map.get` misses that and the client sees a tool it never declared.
//	 */
//
// 查表顺序与参考实现一致: ① alias 精确 → ② alias 忽略大小写 → ③ origin 忽略大小写。
func caseInsensitiveToolNameLookup(name string, m *toolNameMap) (string, bool) {
	if m == nil || m.len() == 0 {
		return "", false
	}
	if v, ok := m.get(name); ok {
		return v, true
	}
	lower := strings.ToLower(name)
	for _, e := range m.entries {
		if strings.ToLower(e.alias) == lower {
			return e.origin, true
		}
	}
	for _, e := range m.entries {
		if strings.ToLower(e.origin) == lower {
			return e.origin, true
		}
	}
	return "", false
}

// restoreOpenAIToolNames 照抄 toolCallHelper.ts:131-157。
//
// 参考实现原注释（逐字）:
//
//	需要把响应里 `choices[].message.tool_calls[].function.name` 与
//	`choices[].delta.tool_calls[].function.name` 上的**别名**换回客户端
//	声明的原名。不还原时，OpenAI 形态客户端会收到它从未声明过的工具名。
//
// 返回是否发生改动（对位参考实现的 `changed`）。
func restoreOpenAIToolNames(body map[string]any, m *toolNameMap) bool {
	if m == nil || m.len() == 0 {
		return false
	}
	choices, ok := body["choices"].([]any)
	if !ok {
		return false
	}
	changed := false
	restoreCalls := func(calls any) {
		list, ok := calls.([]any)
		if !ok {
			return
		}
		for _, tc := range list {
			tcMap, ok := tc.(map[string]any)
			if !ok {
				continue
			}
			fn, ok := tcMap["function"].(map[string]any)
			if !ok {
				continue
			}
			name, ok := fn["name"].(string)
			if !ok {
				continue
			}
			original, found := caseInsensitiveToolNameLookup(name, m)
			if !found || original == name {
				continue
			}
			fn["name"] = original
			changed = true
		}
	}
	for _, ch := range choices {
		chMap, ok := ch.(map[string]any)
		if !ok {
			continue
		}
		if delta, ok := chMap["delta"].(map[string]any); ok {
			restoreCalls(delta["tool_calls"])
		}
		if msg, ok := chMap["message"].(map[string]any); ok {
			restoreCalls(msg["tool_calls"])
		}
	}
	return changed
}

// itoa 对位参考实现里 JS 模板串的数字插值（`${base}${suffix}`）。
//
// 参考实现原文:
//
//	let alias = base
//	let suffix = 2
//	while (alias !== original && used.has(alias)) {
//	    alias = `${base}${suffix}`
//	    suffix++
//	}
func itoa(n int) string {
	return strconv.Itoa(n)
}

// osGetenv 对位参考实现里的 `process.env.X`。
func osGetenv(key string) string {
	return os.Getenv(key)
}
