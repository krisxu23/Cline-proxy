package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"free-router/internal/kit"
)

const (
	providerAttemptTimeout   = 180 * time.Second
	providerResponseMaxBytes = 64 << 20
	// providerExitRetries 网络错误时换出口重试次数(共 1 + N 次拨号)。
	providerExitRetries = 2
	// providerExitCooldown 网络错误后该出口的冷却时长。短冷却即可:
	// 目的是让轮询跳过"对这个上游不通"的节点, 而不是长期禁用可用节点。
	providerExitCooldown = 2 * time.Minute
)

// rotateProviderExit 冷却刚失败的那个出口, 并记录一次换出口重试。
// budget 是本轮允许的换出口次数(对话路径为 providerExitRetries, 目录路径为
// 池内健康出口数), 打印出来才能一眼看出"还能换几个", 而不是永远显示分母 2。
// 直连模式下池内没有可选出口, 调用方不会走到这里。
func rotateProviderExit(provider, reason string, attempt, budget int) {
	// 这里手上只有全局轮询位置, 所以用 ByIndex 现场解析出是哪个出口 ——
	// 落库的键仍是出口标识, 列表之后重排不会让这条冷却漂到别的出口上。
	if idx := lastZenProxyIdx(); idx >= 0 {
		cooldownZenProxyByIndex(idx, providerExitCooldown)
	}
	log.Printf("  providers: %s %s, retry %d/%d on the next exit", provider, reason, attempt, budget)
}

// isExitRegionRejected 上游按"出口所在地区"拒绝服务。
//
// 实测: 从不受支持的地区直连 Google, 目录与对话接口都回
// 400 FAILED_PRECONDITION "User location is not supported for the API use."。
// 这类失败与"这个节点到该上游不通"同源 —— 换一个地区的出口即可成功,
// 因此要和网络错误一样触发换出口, 而不是把 400 原样透传给调用方。
func isExitRegionRejected(status int, body []byte) bool {
	if status != http.StatusBadRequest && status != http.StatusForbidden {
		return false
	}
	low := strings.ToLower(string(body))
	return strings.Contains(low, "location is not supported") ||
		strings.Contains(low, "user location")
}

// providerExitClient 所有通用 Provider 的上游请求都经此客户端发出。
// 它复用 zen 上游的传输层, 因此与 cline 池 / opencode 共用同一条出口链路:
// 出口模式(直连 / 节点) + 代理策略 + 节点连通性/冷却/地区能力全部一致生效。
// 早期实现给 Provider 单独配了直连客户端, 于是节点池对它完全不生效 ——
// 这正是「节点测试绿色、Provider 却 502/超时」的原因。
func providerExitClient() *http.Client {
	return getZenHTTPClient()
}

// providerError 上游错误(状态码 + 截断响应体)。
type providerError struct {
	Provider string
	Status   int
	Body     string
}

func (e *providerError) Error() string {
	return fmt.Sprintf("provider %s: HTTP %d: %s", e.Provider, e.Status, kit.Truncate(e.Body, 500))
}

// providerErrorStatus 上游 4xx 原样透传, 其余(网络错误/5xx)按 502。
func providerErrorStatus(err error) int {
	if pe, ok := err.(*providerError); ok {
		if pe.Status >= 400 && pe.Status < 500 {
			return pe.Status
		}
		return http.StatusBadGateway
	}
	return http.StatusBadGateway
}

// parseProviderModel 解析 "provider:model" 前缀; provider 必须已在配置中声明。
func parseProviderModel(model string) (string, string, bool) {
	m := strings.TrimSpace(model)
	i := strings.Index(m, ":")
	if i <= 0 {
		return "", "", false
	}
	name, rest := m[:i], m[i+1:]
	if rest == "" || providerByName(name) == nil {
		return "", "", false
	}
	return name, rest, true
}

// sseTapReader 边读边按行回调(用于流式提取 Gemini 签名)。
type sseTapReader struct {
	rc      io.ReadCloser
	onLine  func(string)
	pending []byte
}

func (t *sseTapReader) Read(p []byte) (int, error) {
	n, err := t.rc.Read(p)
	if n > 0 {
		t.pending = append(t.pending, p[:n]...)
		for {
			i := bytes.IndexByte(t.pending, '\n')
			if i < 0 {
				break
			}
			line := string(t.pending[:i])
			t.pending = t.pending[i+1:]
			t.onLine(line)
		}
	}
	return n, err
}

func (t *sseTapReader) Close() error {
	if len(t.pending) > 0 {
		t.onLine(string(t.pending))
		t.pending = nil
	}
	return t.rc.Close()
}

// Chat 转发 OpenAI 兼容请求到该 provider; 流式响应按 SSE 原样透传。
// 多 key 轮换: 外层逐 key, 内层沿用原有的签名重放/exit 重试逻辑;
// 429 切下一个 key 且不记罚, 401/403/5xx 与网络错误记罚后换下一个,
// 首个 200 即返回, 其余 4xx 直接透传(请求本身的问题, 换 key 无用)。
func (p *modelProvider) Chat(ctx context.Context, params map[string]any, stream bool) (*http.Response, error) {
	cfg, _ := providerConfigFor(p.name)
	keys := enabledAPIKeys(cfg, p.name)
	if len(keys) == 0 {
		return nil, fmt.Errorf("provider %s is not configured", p.name)
	}
	if model, _ := params["model"].(string); model != "" {
		if rest, ok := strings.CutPrefix(model, p.name+":"); ok && rest != "" {
			params["model"] = rest
		}
	}

	needsSig := providerNeedsThoughtSignatures(p)
	if needsSig {
		injectThoughtSignatures(params, p.sigCache)
	}
	client := providerExitClient()
	// 把模型写进请求上下文: 拨号层据此为地区受限模型挑选已验证的节点出口,
	// 与 opencode 渠道同一套选路规则。
	model, _ := params["model"].(string)
	ctx = context.WithValue(ctx, ctxKeyZenModel, p.name+":"+model)
	if !stream {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, providerAttemptTimeout)
		defer cancel()
	}

	// key 级健康(P2): 与 pickHealthyKeys 完全同一判定(同一常量, 零语义分叉),
	// 被降级的 key(5 分钟窗口内)排到队尾; 全部降级时仍按原序尝试(总好于
	// 不发请求)。成功一次即清零(recordKeySuccess), 窗口过期自动恢复。
	isDemoted := func(key string) bool {
		keyHealthMu.Lock()
		defer keyHealthMu.Unlock()
		if st, ok := keyHealth[p.name][key]; ok && st != nil &&
			st.failures >= keyFailThreshold && time.Now().UnixMilli()-st.demotedAt < keyCooldownMs {
			return true
		}
		return false
	}
	order := make([]int, 0, len(keys))
	demoted := make([]int, 0)
	for i, key := range keys {
		if isDemoted(key) {
			demoted = append(demoted, i)
			continue
		}
		order = append(order, i)
	}
	order = append(order, demoted...)

	var lastErr error
	for _, i := range order {
		key := keys[i]
		resp, err := p.chatWithKey(ctx, cfg, params, key, stream, needsSig, client)
		if err == nil {
			recordKeySuccess(p.name, key)
			return resp, nil
		}
		if pe, ok := err.(*providerError); ok {
			switch {
			case pe.Status == http.StatusTooManyRequests:
				lastErr = err
			case pe.Status == http.StatusUnauthorized || pe.Status == http.StatusForbidden || pe.Status >= 500:
				recordKeyResult(p.name, key, pe.Status, false)
				lastErr = err
			default:
				return nil, err
			}
		} else {
			recordKeyResult(p.name, key, 0, true)
			lastErr = err
		}
	}
	// 注意: 这里曾按 keys 下标(`i == len(keys)-1`)提前返回 —— 当 keys 的最后一个
	// 元素不在 order 队尾(被降级的 key 排在 demoted 段)时, 迭代到它就 return,
	// order 中排其后尚未尝试的降级 key 被跳过, 实际 failover 次数少于设计。
	// order 恰好覆盖全部下标, 循环自然走完后返回 lastErr 即可。
	return nil, lastErr
}

// chatWithKey 单 key 的原有尝试逻辑: Gemini 签名重放 + models/ 前缀 + exit 轮换。
func (p *modelProvider) chatWithKey(ctx context.Context, cfg providerConfig, params map[string]any, key string, stream, needsSig bool, client *http.Client) (*http.Response, error) {
	model, _ := params["model"].(string)

	// 客户端原始工具名快照: 必须赶在下方 remapToolNamesInRequest/cloak 改写
	// tools 之前取(取数即序列化), 供每次出站 attempt 挂到内部头。
	toolsSnapshot := snapshotStreamRequestTools(params)

	// 上游不支持参数剥离（照抄 OmniRoute open-sse/translator/paramSupport.ts）。
	//
	// 这里是 generic provider 出站的**真正咽喉**, 与参考实现
	// handlers/chatCore/upstreamBody.ts:227 → services/targetRequestSanitizer.ts:80
	// 的位置同构: translated body 已在手、target 已解析、即将 fetch。
	//
	// 为什么必须在此处而不是别处: 规则表 11 条里 6 条带 provider 限定
	// (github / nvidia / volcengine / zai / glm / azure-*), 而另外 5 条按 model
	// 正则匹配。只有在"真实 provider + 原始 model id"都可用时才判定得准 ——
	// p.name 是真实 provider, params["model"] 是客户端要的原始 id
	// (未被 normalizeRequestModel 改写)。
	//
	// 语义: 客户端为它选中的模型带上控制参数, 但路由/回退可能把模型换成另一系列。
	// 属于源模型、对目标模型非法的参数会招致上游 400 (例如 claude-opus-4 系列的
	// temperature 已被弃用、GitHub Copilot 的 Claude 拒绝 thinking +
	// reasoning_effort), 在发出前剔除可避免整次请求报废。
	stripUnsupportedParams(normalizeProviderID(p.name), model, params)

	// 严格 provider 的 system 消息提升（照抄 OmniRoute
	// open-sse/translator/helpers/strictSystemHoist.ts +
	// src/lib/memory/injection.ts）。
	//
	// 参考实现在 translateRequest() 的**每个**出站路径上调用它
	// (translator/index.ts:413 / :787), 包括 OpenAI→OpenAI 同格式透传 ——
	// 透传时格式专用译码器根本不执行, 客户端插在数组中间的 system 消息会原样
	// 到达上游。Codex / OpenCode / Kilo Code 风格 agent 客户端都会这么发。
	//
	// 严格清单 (injection.ts:84): xiaomi-mimo / mimo / tokenrouter ——
	// 其中 **tokenrouter 是本网关用户实际在用的 provider**, 故这条会真实触发。
	//
	// 语义要点: 目标 provider 不在严格清单时**原样返回同一切片**(no-op),
	// 这是 prompt-cache 前缀稳定性的硬要求(#3890), 故不能无条件重建切片。
	// 本构造点有真实 provider(p.name), 判定得准。
	//
	// 只在键存在且是数组时处理: 参考实现由 `result.messages && Array.isArray(...)`
	// 守卫(translator/index.ts:412)。
	if msgs, ok := params["messages"].([]any); ok {
		params["messages"] = hoistLeadingSystemMessage(msgs, p.name, strictSystemProviderIDs())
	}

	// 工具 description 归一（照抄 OmniRoute translator/helpers/schemaCoercion.ts
	// :77-81/:226-254/:444-447）。
	//
	// ★ 作用域是**所有格式**, 不带 provider 限定 —— 这是照抄的:
	//   参考实现在 translator/index.ts:606-607 与 :628-629 两处调用它, 两处都在
	//   `if (result.tools !== undefined)` / `if (result.tools)` 之下, **没有**
	//   targetFormat 或 provider 的前置条件。它属于"翻译收尾的统一归一"。
	//   我方对位: 出站咽喉处对 tools 无条件跑一次(no-op 幂等)。
	//
	// 语义: agent harness 深截断工具定义时会把 description 写成 null 或数字,
	// 严格上游(Anthropic / MiniMax)回 400。null -> ""、数字 -> String(数字),
	// 键不存在则完全不动。
	//
	// 注意与 sanitizeOpenAITools 的分工: 后者管 `parameters`(JSON Schema 结构),
	// 本函数只碰 `description` 字段 —— 两者不重叠, 都可跑。
	if tools, ok := params["tools"]; ok && tools != nil {
		params["tools"] = sanitizeToolDescriptions(tools)
	}

	// 工具配对/相邻性守卫（照抄 OmniRoute services/contextManager.ts:717-1000,
	// 调用链逐字对位 executors/base.ts:1320-1339）。
	//
	// 参考实现原注释: "Clients can ship truncated histories mid-tool-call which
	// Anthropic rejects with `messages.N: tool_use ids were found without
	// tool_result blocks immediately after: toolu_...`. fixToolPairs strips
	// orphans, then stripTrailingAssistantOrphanToolUse catches the case where
	// the request body itself ends on an unmatched assistant(tool_use) —
	// invalid for an upstream-send turn since the body must end on a user
	// message. Both are idempotent on clean histories."
	//
	// ★ 作用域同样是照抄的: 整条链在参考实现里被包在
	//   `if (((this.provider === "claude" && (isClaudeCodeClient ||
	//   hasClaudeOAuthToken)) || usesClaudeCodeProtocol) && ...)`
	//   (executors/base.ts:955-960) —— **仅 Claude 原生 / Claude Code 协议路径**。
	//
	//   曾有一版把它无条件挂到所有 provider 上, 结果把 gemini 路径里合法的
	//   "以 assistant(tool_calls) 结尾"的历史也当成孤儿调用删掉了
	//   (两条既有测试当场变红)。Anthropic 之外的协议对 tool_result 位置没有
	//   这种强约束, 越界施加只会破坏正常请求 —— 照抄必须连作用域一起抄。
	//
	// 顺序是照抄的, 每一步都有理由, 不要重排:
	//  1. fixToolPairs  —— 清"某处有 tool_use 但全无结果"的孤儿;
	//  2. fixToolAdjacency —— Claude 严格要求 tool_result 在**紧邻**下一条
	//     (OpenAI 允许分散在多条后续消息, 故仅 Claude 走这步);
	//  3. fixToolPairs 再跑一遍 —— adjacency 可能剥出新的孤儿(讨论 #2410);
	//  4. stripTrailingAssistantOrphanToolUse —— 请求体必须**以 user 回合结尾**,
	//     结尾是 assistant(tool_use) 会触发同一个 400;
	//  5. stripTrailingAssistantForProvider —— Mistral 连"纯文本 assistant 结尾"
	//     都拒绝 (#3396)。
	// 干净历史上这五步全是 no-op(幂等), 所以放在咽喉处对正常请求零代价。
	//
	// isClaude 对位参考实现的 `this.provider === "claude" || usesClaudeCodeProtocol`。
	// 我方用 providerConfig.APIType == "anthropic" 表达同一件事
	// (见 providers_config.go:174-181 的鉴权分派)。
	if cfg.APIType == "anthropic" {
		// 工具名伪装（照抄 OmniRoute services/claudeCodeToolRemapper.ts:113-185
		// `remapToolNamesInRequest` + :373-483 `cloakThirdPartyToolNames`）。
		//
		// ★ 作用域是照抄的: 参考实现把这两步包在
		//
		//	((this.provider === "claude" && (isClaudeCodeClient || hasClaudeOAuthToken)) ||
		//	 usesClaudeCodeProtocol) && typeof transformedBody === "object"
		//
		//   （executors/base.ts:955-960）—— **仅 Claude 原生 OAuth / Claude Code 协议路径**。
		//   我方用 providerConfig.APIType == "anthropic" 表达同一件事（与本文件上方
		//   工具配对链共用同一个 gate）。
		//
		// ★ 顺序是照抄的（executors/base.ts:962-970）:
		//	stripProxyToolPrefix(tb)      —— 本网关无 proxy_ 前缀通道，跳过（另见说明）;
		//	remapToolNamesInRequest(tb)   —— 固定 Claude Code 工具名大小写归一;
		//	cloakThirdPartyToolNames(tb)  —— 通用伪装 + 记 _toolNameMap;
		//	sanitizeClaudeToolSchemas     —— 已在入站 anthropicToolsToOpenAI 处做（schema 层面）。
		//
		// 业务意义（本步是本网关"任务无声中断"的一条真实根因）:
		//   Anthropic 在**第一方 Messages API**（原生 Claude OAuth）上用**工具名指纹**
		//   识别第三方 agent harness。真 Claude Code 用 `Bash`/`Read` 大驼峰，而
		//   Codex / OpenCode / Cline 发来的历史普遍是 snake_case（`read_file` /
		//   `run_command` / `list_directory`）。被识别 → 上游拒绝服务，且错误**伪装成
		//   `400 out of extra usage`**（看着像计费问题，实为 SSE 流被拒）。
		//
		// 两步的分工（不可合并）:
		//   remapToolNamesInRequest 只归一**固定清单**里的名字，返回"是否需要小写化回写";
		//   cloakThirdPartyToolNames 把**任何**看起来不像真 Claude Code 工具的名字
		//   确定性改名（有 canonical 用 canonical，否则 PascalCase），并记入
		//   per-request 的 `_toolNameMap`，供响应路径还原。
		//
		// ★ Go 侧必须显式摘掉 `_toolNameMap`（参考实现靠 `enumerable: false`）:
		//   下方 json.Marshal(params) 会把它序列化进上行 body，Anthropic 400
		//   `Extra inputs are not permitted`。摘下来的映射存回 params 的旁路，
		//   由 handleProviderChat 读走交给响应侧的 restoreClaudeToolName。
		{
			nameMapChanged := remapToolNamesInRequest(params)
			cloakMap := cloakThirdPartyToolNames(params, nil)
			// 合并两张反向映射（remap 写的是 params["_toolNameMap"] 里的同一张表，
			// cloak 返回的是它，故此处只需取一次 —— 与参考实现
			// cliproxyapi.ts:363-368 的 "new Map(cloakMap) 再并 mcpMap" 同构，
			// 我方无 mcp 改写通道，故并集即 cloakMap）。
			_ = nameMapChanged
			if cloakMap != nil && cloakMap.len() > 0 {
				params[toolNameMapSideChannelKey] = cloakMap
				log.Printf("  providers: %s cloaked third-party tool names (%d aliases)", p.name, cloakMap.len())
			}
		}

		if msgs, ok := params["messages"].([]any); ok {
			// tool id 双侧对称净化（照抄 claudeHelper.ts:432-450 的 Pass 1.4）。
			//
			// ★ 必须排在整条链**最前**, 且必须在 splitMisplacedToolResults 之前。
			// 理由有两层:
			//
			//  1. 参考实现的顺序是「先净化 id, 后搬块」。它把 Pass 1.4 放在
			//     prepareClaudeRequest 的消息遍历里, 而 splitMisplacedToolResults
			//     是更后面的 Pass 1.45（claudeHelper.ts:484-486）。顺序颠倒时,
			//     "某个 tool_result 的 id 是否被更早的 assistant 发出过"这个判定
			//     会拿**未净化**的 id 去比对**未净化**的集合 —— 两边都没净化其实
			//     仍然配对, 但净化后集合与块上的值就不同步了, 于是搬完再净化会
			//     让 splitMisplacedToolResults 的"孤儿丢弃"判定基于过期的 id 表。
			//     先净化保证后续每一步看到的是同一份 id。
			//
			//  2. 本函数同时剔除「空名 tool_use」与「缺 id 的 tool_result」——
			//     这两类块在后续的 fixToolPairs / fixToolAdjacency 里会被当成
			//     孤儿处理, 不如在最早的位置删掉, 让后续判定面对干净输入。
			//
			// Anthropic 对 tool id 强制 `^[a-zA-Z0-9_-]+$`; 客户端重放的历史
			// (尤其 Codex 经 cc-switch 从别的 provider 带过来的) 可能含 `.`/`:`/`#`,
			// 上游回 400 TOOL_SCHEMA_INVALID —— 在 agent 客户端里只表现为
			// 任务无声中断。两侧同函数改写才能保住 tool_use/tool_result 配对。
			fixed, idChanged := sanitizeClaudeToolIDs(msgs)
			if idChanged {
				params["messages"] = fixed
				log.Printf("  providers: %s sanitized tool ids in messages[] (%d msgs)", p.name, len(fixed))
				msgs = fixed
			}

			// splitMisplacedToolResults（照抄 claudeHelper.ts:103-156, #2815）
			// 排在整条链**最前**: 它处理的是"tool_result 出现在 assistant 回合"
			// 这一结构性问题, 必须先把块搬到正确的 user 回合, 后面的
			// 配对/相邻性判定才有意义 —— 否则 fixToolPairs 看到的是一份
			// 块位置本身就错的历史。
			//
			// 参考实现在 normalizeClaudeUpstreamMessages 的**最后**一步调用它
			// (claudeUpstreamMessages.ts:169-172), 那是"先清理内容再搬块";
			// 我方链路上没有前置的内容清理步骤(空块过滤在别处), 故等价于
			// 把这一步提到链首 —— 两者对最终块位置的结果一致, 但链首要更安全:
			// 后续每一步都建立"块已在正确回合"的前提上。
			//
			// Anthropic 对 assistant 里的 tool_result 直接 400, 而 400 在
			// agent 客户端里常常只显示成任务无声中断。
			fixed = splitMisplacedToolResults(fixed)
			// fixToolUseOrdering（照抄 claudeHelper.ts:160-284）—— 三步:
			//  ① 删 assistant 里 tool_use **之后**的 text 块（Claude 位置约束）;
			//  ② 合并相邻同 role 回合（tool_result 提前）;
			//  ③ 把失去配对的 tool_result 降级成 user 文本, 并给缺 tool_result 的
			//     tool_use 补空占位。
			//
			// 位置是照抄的: 参考实现在 prepareClaudeRequest 里按
			//   Pass 1(过滤空消息) → Pass 1.4(净化 id + 剔空名) →
			//   Pass 1.45(splitMisplacedToolResults) → Pass 1.5(本函数)
			// 的顺序执行（claudeHelper.ts:403/422/484/488）。
			//
			// ★ 必须在 splitMisplacedToolResults **之后**: 后者的动机是"把块搬到
			//   正确的 user 回合", 本函数的 Pass 3 则要基于"块已归位"来判断配对,
			//   顺序颠倒会让 Pass 3 看到一份块位置本身就错的历史。
			// ★ 必须在 fixToolPairs/fixToolAdjacency **之前**: 本函数已把孤儿
			//   tool_result 全部转成文本、并给缺失结果补了占位, 因此后续两步在
			//   正常输入上变成 no-op, 不会与它争着改同一批块。
			fixed = fixToolUseOrdering(fixed)
			fixed = fixToolPairs(fixed)
			fixed = fixToolPairs(fixToolAdjacency(fixed))
			fixed = stripTrailingAssistantOrphanToolUse(fixed)
			params["messages"] = stripTrailingAssistantForProvider(fixed, normalizeProviderID(p.name))
		}

		// prompt-cache 断点重锚（照抄 claudeHelper.ts:364-396 / :499-513 / :527-544 / :724-740）。
		//
		// ★ 作用域是照抄的: 参考实现整段在 prepareClaudeRequest 里, 而后者**只**
		//   在 `targetFormat === FORMATS.CLAUDE` 时被调用(translator/index.ts:567)。
		//   我方对位即 anthropic 分支内部 —— 不能外溢到 OpenAI 形态出站, 那里
		//   的 `cache_control` 字段名根本不在协议里。
		//
		// ★ 模式选择也是照抄: 参考实现的 `preserveCacheControl` 由调用方传
		//   (relay 路径传 true 表示"客户端自己管 marker")。本网关的出站咽喉
		//   没有该开关的上游来源 —— 客户端发来的 body 里若已有 marker, 说明它
		//   自己管; 若一个都没有, 则按 Claude Code 约定补断点。
		//
		//   这正是参考实现 `opts.fallbackToHeuristicWhenNoMarkers` 的语义
		//   (claudeHelper.ts:356-362): `preserveCacheControl && 无任何 marker`
		//   -> 降级为不 preserve, 照常补断点。
		//
		// 业务意义: Anthropic 的 prompt cache 是显式断点制 —— 客户端不带 marker
		// 时, 每一轮都把整个前缀按**未缓存**计费。长会话(agent 客户端动辄几十轮)
		// 下这是数倍成本差。Codex / Cline 默认都不带 marker。
		reanchorClaudePromptCache(params, normalizeProviderID(p.name))

		// thinking 块处理（照抄 claudeHelper.ts:546-719，prepareClaudeRequest Pass 2）。
		//
		// ★ 位置: 参考实现里这一段与"给最后一条 assistant 打 cache_control"
		//   **在同一个反向循环内**（claudeHelper.ts:530-721），而该循环排在
		//   "给倒数第二条 user 打 cache_control"（:503-513）之后。
		//   故此处必须排在 reanchorClaudePromptCache 之后 —— 后者正是把
		//   system/倒数第二 user/最后 assistant/tools 四处断点都锚完的那一步。
		//
		// ★ 作用域: `prepareClaudeRequest` 只在 `targetFormat === FORMATS.CLAUDE`
		//   时被调用（translator/index.ts:567），即本 anthropic 分支内部。
		//
		// 业务意义: 上游（claude 原生 / anthropic-compatible 中转 / kimi-coding）
		// 在 `thinking.type === "enabled"` 时执行严格形态契约 —— assistant 回合
		// 只要含 tool_use，同一 content[] 就必须有一个 thinking / redacted_thinking
		// 块排在它之前。客户端重放历史时常丢掉它，上游随即 400：
		//   "thinking is enabled but reasoning_content is missing in assistant
		//    tool call message at index N" 或 "Invalid signature in thinking block"。
		// 400 在 agent 客户端里常常只表现为**任务无声中断** —— 与用户报的现象一致。
		//
		// 注: 本网关没有照抄 reasoningCache 的内存+DB 双级缓存服务，故此处
		// lookupReasoning 传 nil —— 语义上等价于参考实现的 cache miss，
		// 落到 NON_ANTHROPIC_THINKING_PLACEHOLDER 兜底（:674/:716）。
		//
		// ★ modelTargetsClaude 不能写死 true：它决定 supportsRedactedThinking
		//   （claudeHelper.ts:385），进而决定块形状是 redacted_thinking 还是
		//   plain thinking。参考实现取自 per-model 的 getModelTargetFormat 注册表
		//   （providerModels.ts:172）；本网关无该表，故保守地复用
		//   supportsPromptCachingForProvider —— 即只把 claude / anthropic-compatible-*
		//   视为"真 Anthropic Messages 端点"（能校验签名 blob）。其余上游走
		//   plain thinking + 占位符，这正是它们能接受的形态。
		//   探针 T11–T13 实测了 targetsClaude=false 分支的权威值。
		if msgs, ok := params["messages"].([]any); ok {
			upstreamProvider := normalizeProviderID(p.name)
			thinkingChanged := applyClaudeThinkingBlocks(
				msgs, params, upstreamProvider,
				supportsPromptCachingForProvider(upstreamProvider),
				nil, nil,
			)
			if thinkingChanged {
				log.Printf("  providers: %s applied claude thinking-block normalization", p.name)
			}
		}
	} else if msgs, ok := params["messages"]; ok {
		// 空 reasoning_content 回放（照抄 translator/index.ts:610-619 +
		// schemaCoercion.ts:455-487 + services/reasoningCache.ts:83-121）。
		//
		// ★ 作用域的三重限定, 逐条对位参考实现, 缺一条就是越界施加:
		//
		//  1. **仅 OpenAI 形态出站**。参考实现是 `targetFormat === FORMATS.OPENAI`;
		//     我方用 `cfg.APIType != "anthropic"`(即本 else 分支)表达同一件事。
		//     Anthropic 形态的等价逻辑是"往 content[] 里插 thinking block", 那在
		//     anthropic 分支里已由 claude_helper.go 的另一套流程处理, 不能在这里
		//     补 `reasoning_content` 字段 —— 字段名都不在 Anthropic 协议里。
		//
		//  2. **`!requiresExplicitReasoningReplay`**, 由 applyEmptyReasoningReplay
		//     内部照抄(注意实参是 `allowLegacyFallback=false` 且 thinkingEnabled 取
		//     真实配置, 与 inject 内部的判定**故意不对称**)。
		//
		//  3. **messages 必须是数组**, 由 applyEmptyReasoningReplay 内部把关。
		//
		// 业务意义: DeepSeek V4 / Kimi thinking / Xiaomi MiMo 这类上游有反向契约 ——
		// 多轮请求里 assistant 回合若带 `tool_calls`, 就必须同时带 `reasoning_content`
		// (哪怕空串), 否则上游 400 "The reasoning_content in the thinking mode must
		// be passed back to the API."。客户端(尤其 Codex 经 cc-switch 路由)会把该
		// 字段丢掉, 而 400 在 agent 客户端里常常只表现为**任务无声中断** ——
		// 与用户报的现象一致。
		//
		// provider/model 必须用**上游真实值**: p.name 是真实 provider 名,
		// params["model"] 是客户端要的原始 id(未被 normalizeRequestModel 改写)。
		// 判定表里既有 provider 白名单也有 model 正则, 两者都依赖这两个值。
		replayed, replayChanged := applyEmptyReasoningReplay(msgs, normalizeProviderID(p.name), model, params)
		if replayChanged {
			params["messages"] = replayed
			log.Printf("  providers: %s injected empty reasoning_content for tool-call turns", p.name)
		}
	}

	// Gemini 会拒绝缓存里失效的签名(400)。此时用跳过哨兵重放一次,
	// 否则同一段会话会一直失败到该缓存项被淘汰为止。
	attempts := 1
	if needsSig {
		attempts = 2
	}
	// Anthropic 形态过期 thinking 的一次性修复重放标志(见下方重试环内的守卫)。
	staleThinkingReplayed := false

	// 照抄 OmniRoute modelStrip.ts:19-55 —— 剥掉该模型不支持的
	// 内容类型（image / audio）。不剥的话上游直接 400 拒绝
	// （"model does not support images"），agent 客户端表现为任务无声中断。
	//
	// 位置：所有消息处理（工具顺序归一、prompt-cache 重锚、thinking 块归一、
	// 工具名伪装）之后、marshal 之前。此时 p.name（真实 provider）与
	// params["model"]（未被改写的原始模型 id）都已就位。
	modelStr, _ := params["model"].(string)
	if stripTypes := stripTypesForModel(p.name, modelStr); len(stripTypes) > 0 {
		if msgs, ok := params["messages"].([]any); ok {
			stripped, removed := stripIncompatibleMessageContent(msgs, stripTypes)
			if removed > 0 {
				params["messages"] = stripped
				log.Printf("  [model-strip] provider=%s model=%s removed %d incompatible content parts",
					p.name, modelStr, removed)
			}
		}
	}

	// `_toolNameMap` 绝不能上行（照抄 OmniRoute chatCore.ts:2610
	// `delete translatedBody._toolNameMap;` + cliproxyapi.ts:417 的 replacer）。
	//
	// 参考实现靠 `Object.defineProperty(..., { enumerable: false })` 让
	// `JSON.stringify` 忽略它；Go 的 `json.Marshal` 没有这个概念，**必须显式 delete**。
	// 否则 Anthropic 会回 400 `Extra inputs are not permitted`
	// （哪怕值是 `{}` —— 键名本身非法）。
	//
	// 摘下来的映射不丢：转存到独立的旁路键 `toolNameMapSideChannelKey`，
	// 该键在每次 marshal 前被移除、marshal 后回填，因此**永远不出现在 payload 里**。
	// 调用方（handleProviderChat / callChainUpstream）在 Chat 返回后读它交给响应侧还原。
	wireToolNameMap := detachToolNameMap(params)

	for attempt := 1; attempt <= attempts; attempt++ {
		// 旁路键在 marshal 前移除 —— 它只是 Go 侧的进程内通道，绝不参与线序化。
		delete(params, toolNameMapSideChannelKey)
		// 协议形态转换: APIFormat 不是 chat 时, 把 chat 形态的 params 转成目标形态
		// 再序列化。★ params 本身保持 chat 形态不变 —— 下游还要用它(旁路键回填、
		// 响应侧工具名还原)。
		outbound, cerr := cfg.convertOutboundRequest(p.name, params, stream)
		if cerr != nil {
			return nil, cerr
		}
		payload, err := json.Marshal(outbound)
		// 旁路键回填必须在 marshal **之后**: chat 形态下 outbound 就是 params 本身
		// (convertOutboundRequest 原样返回), 提前回填会把它序列化进上行 body。
		if wireToolNameMap != nil && wireToolNameMap.len() > 0 {
			params[toolNameMapSideChannelKey] = wireToolNameMap
		}
		if err != nil {
			return nil, fmt.Errorf("marshal provider body: %w", err)
		}
		origin := localOrigin()
		// 新请求要重建 body, 并重新取一次出口: 因此逐次构造而不是复用 req。
		send := func() (*http.Response, error) {
			req, rerr := http.NewRequestWithContext(ctx, "POST", cfg.chatEndpoint(), bytes.NewReader(payload))
			if rerr != nil {
				return nil, rerr
			}
			req.Header.Set("Content-Type", "application/json")
			cfg.applyAuthWithKey(req.URL.String(), key, req.Header.Set)
			for k, spec := range cfg.Headers {
				if v := cfg.resolveHeader(spec, origin); v != "" {
					req.Header.Set(k, v)
				}
			}
			attachStreamRequestTools(req, toolsSnapshot)
			return client.Do(req)
		}

		// 两种失败都可能只是"这个出口不行", 而不是"这个上游不行", 因此统一按出口轮换:
		//   - 网络错误: 所选出口到该上游不通;
		//   - 地区拒绝: 出口所在地区被上游拒服务(Google 的 location not supported)。
		// 与 zen 渠道同一套自愈逻辑 —— 否则池里一个不合适的节点会被反复选中。
		var (
			resp *http.Response
			body []byte
		)
		for exitRetries := 0; ; {
			r, sendErr := send()
			if sendErr != nil {
				// 直连模式没有第二个出口可换, 重复拨号只是白等一轮超时。
				if exitModeDirectNow() || exitRetries >= providerExitRetries {
					return nil, sendErr
				}
				exitRetries++
				rotateProviderExit(p.name, fmt.Sprintf("network error (%v)", sendErr), exitRetries, providerExitRetries)
				continue
			}
			resp = r
			if resp.StatusCode == http.StatusOK {
				out, ferr := p.finishResponse(resp, stream, needsSig)
				if ferr != nil {
					return nil, ferr
				}
				// 协议形态转换: APIFormat 不是 chat 时, 把上游响应转回 chat 形态。
				//
				// 放在 finishResponse 之后: 后者负责读完非流式响应体(必须在超时
				// 上下文内完成)与挂 Gemini 签名提取, 那两件事都该看到**上游原始
				// 字节**。转换本身对流式走 io.Pipe 实时做, 因此下游的空流守卫、
				// 坏帧清洗、心跳全部照常生效。
				modelStr, _ := params["model"].(string)
				return cfg.convertProviderResponse(out, modelStr, stream)
			}
			body, _ = io.ReadAll(io.LimitReader(resp.Body, 64<<10))
			resp.Body.Close()
			if exitRetries < providerExitRetries && !exitModeDirectNow() &&
				isExitRegionRejected(resp.StatusCode, body) {
				exitRetries++
				rotateProviderExit(p.name, "exit region rejected: "+kit.Truncate(string(body), 160), exitRetries, providerExitRetries)
				continue
			}
			break
		}
		if needsSig && attempt < attempts && isMissingThoughtSignatureError(resp.StatusCode, string(body)) {
			log.Printf("  providers: %s rejected a thought-signature, replaying with the skip sentinel", p.name)
			markAllSignaturesSkipped(params)
			continue
		}
		// Anthropic 形态 400 的被动修复重放(每请求最多一次): 过期/失效
		// thinking 剥离后重放 —— 详见 stale_thinking.go 的成因与作用域说明。
		// 主动防御(applyClaudeThinkingBlocks, 进环前一次)救不了: 最新
		// assistant 的真签名块按协议不敢改写, 占位块上游也可能不认。
		//
		// 作用域限 Anthropic 形态: OpenAI 形态的 reasoning 契约是反向的
		// (缺了要补空串), 剥字段会打出另一类 400, 绝不能碰。
		//
		// attempt >= attempts 时把预算顶开一格: 这是修复重放, 不是普通重试 ——
		// 与 Gemini 哨兵重放不同, 它在首次(也可能是最后一次)尝试失败时才
		// 知道需要发生, 必须显式给它留一发。staleThinkingReplayed 保证只有一发。
		if !staleThinkingReplayed &&
			(cfg.APIType == "anthropic" || cfg.APIFormat == apiFormatMessages) &&
			isStaleThinkingError(resp.StatusCode, string(body)) && stripStaleThinking(params) {
			staleThinkingReplayed = true
			log.Printf("  providers: %s rejected stale thinking (400), stripping thinking config/blocks and replaying once", p.name)
			if attempt >= attempts {
				attempts++
			}
			continue
		}
		// Gemini 的 OpenAI 兼容层既接受裸模型名, 也接受带 models/ 前缀的名字。
		// 裸名被拒时补一次前缀形式, 免得用户为了一个命名约定去翻文档。
		if isGoogleProvider(cfg) && resp.StatusCode == http.StatusNotFound && !strings.HasPrefix(model, "models/") {
			log.Printf("  providers: %s rejected model %q as-is, retrying with the models/ prefix", p.name, model)
			params["model"] = "models/" + model
			model = "models/" + model
			attempts++
			continue
		}
		p.recordRejection(model, resp.StatusCode, body)
		return nil, &providerError{Provider: p.name, Status: resp.StatusCode, Body: string(body)}
	}
	return nil, fmt.Errorf("provider %s: no attempt completed", p.name)
}

// finishResponse 收尾成功响应: 非流式必须在超时上下文内读完整个响应体,
// 流式则为 Gemini 挂上签名提取。
func (p *modelProvider) finishResponse(resp *http.Response, stream, needsSig bool) (*http.Response, error) {
	if !stream {
		// 返回即触发调用方的 defer cancel, 而请求上下文控制整个响应生命周期,
		// 未读完的 body 会被 "context canceled" 提前截断(客户端表现为 500 parse_error)。
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, providerResponseMaxBytes+1))
		resp.Body.Close()
		if readErr != nil {
			return nil, readErr
		}
		if len(body) > providerResponseMaxBytes {
			return nil, fmt.Errorf("provider %s: response exceeds %d bytes", p.name, providerResponseMaxBytes)
		}
		if needsSig {
			var parsed map[string]any
			if json.Unmarshal(body, &parsed) == nil {
				rememberSignaturesFromPayload(parsed, p.sigCache)
			}
		}
		resp.Body = io.NopCloser(bytes.NewReader(body))
		return resp, nil
	}
	if needsSig {
		ext := newSignatureStreamExtractor(p.sigCache)
		resp.Body = &sseTapReader{rc: resp.Body, onLine: func(line string) { ext.push(line) }}
	}
	return resp, nil
}

// recordRejection 记录永久拒绝与"无免费层"到该 provider 的拒绝集合。
func (p *modelProvider) recordRejection(model string, status int, body []byte) {
	var payload map[string]any
	json.Unmarshal(body, &payload)
	reason := permanentRejectionReason(status, payload)
	if reason == "" && status == http.StatusTooManyRequests {
		if qf := parseQuotaFailure(payload); qf != nil && qf.NoFreeTier {
			reason = "no free tier"
		}
	}
	if reason == "" || strings.TrimSpace(model) == "" {
		return
	}
	// 写时复制: isFree/freeModelIDs 在解锁后读取该 map 的内容, 就地插入
	// 会与之并发触发 fatal 的 map 读写竞争, 因此整体替换而不是原地增删。
	p.mu.Lock()
	next := make(map[string]string, len(p.rejected)+1)
	for k, v := range p.rejected {
		next[k] = v
	}
	next[model] = reason
	p.rejected = next
	p.mu.Unlock()
}

// handleProviderChat "provider:model" 前缀直选分支。
func handleProviderChat(w http.ResponseWriter, r *http.Request, params map[string]any, name string) {
	p := providerByName(name)
	if p == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": map[string]string{"message": fmt.Sprintf("provider %q is not configured", name), "type": "api_error"},
		})
		return
	}
	cfg, _ := providerConfigFor(name)
	if len(enabledAPIKeys(cfg, name)) == 0 {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": map[string]string{"message": fmt.Sprintf("provider %q has no api key", name), "type": "api_error"},
		})
		return
	}
	model, _ := params["model"].(string)
	pm := strings.TrimPrefix(model, name+":")

	if cfg.Catalog {
		p.mu.Lock()
		// 目录为空时按短退避刷新。后台周期用的 15 分钟间隔不能直接用在请求路径上:
		// refreshCatalog 见到 attemptedAt/catalogErr 就会跳过, 沿用间隔会让该 provider
		// 一直 400 到下一个后台周期。
		//
		// ★ 门限分级(见 catalogRequestGateMs): 暂态失败保持 1 分钟快速重试,
		// 连续失败(catalogFastRetryStreak 次以上)才切到退避间隔。
		// 旧实现恒用 1 分钟, 于是一个永久拉不到目录的 provider 被每个请求触发刷新,
		// 每分钟烧掉一轮出口轮换 —— 9 个 provider × 每次 16 轮换 = 144 次冷却/分钟,
		// 而健康出口只有 25~95 个, 出口池被抽干, 所有请求退化成直连并失败
		// (2026-09-17 实证: 日志满屏"节点池无可用出口")。
		gateMs := catalogRequestGateMs(p.catalogFailStreak)
		empty := len(p.catalog) == 0
		need := empty && time.Now().UnixMilli()-p.attemptedAt >= gateMs
		p.mu.Unlock()
		if need {
			// 刷新放后台: 请求路径不再同步等(最长 90s), 客户端断开也不会把
			// 取消传进刷新(否则计成一次连续失败, 退避最长锁到 6h)。
			// refreshCatalog 自带 inflight 去重, 并发触发不会重复打上游。
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), providerCatalogTimeout)
				defer cancel()
				if err := p.refreshCatalog(ctx, true); err != nil {
					log.Printf("  providers: request-path catalog refresh (%s) failed: %v", name, err)
				}
			}()
		}
		if empty {
			// 目录空着 isFree 必判 false, 回 400 会把"目录加载中"误报成
			// "非法模型" —— 明确 503 让客户端稍后重试。
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{
				"error": map[string]string{
					"message": fmt.Sprintf("provider %q catalog is loading, retry shortly", name),
					"type":    "api_error",
				},
			})
			return
		}
	}

	if !p.isFree(pm) {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]string{
				"message": fmt.Sprintf("model %q is not a free model on provider %q", model, name),
				"type":    "invalid_request_error",
			},
		})
		return
	}

	isStream, _ := params["stream"].(bool)
	params["model"] = pm
	// 路由头(P2 修复): 不设置时请求日志会把本路径判为 "other" 噪音过滤掉,
	// 面板日志里完全看不到 provider 请求。upstream=provider/<名> 供日志回填。
	w.Header().Set("X-Proxy-Route", "upstream="+providerUpstream(name))
	// 通用 Provider 也计入统计: 上游按 provider/<名> 归组, 模型记为 <名>:<模型>,
	// 这样「按上游」能看出是哪个 Provider 在消耗 token。
	tracker := newZenStatsTrackerCtx(r.Context(), zenStatsRecord{
		TS:           time.Now().UnixMilli(),
		Upstream:     providerUpstream(name),
		Model:        model,
		Stream:       isStream,
		PromptTokens: estimateJSON(params),
	})
	status := http.StatusOK
	defer func() { tracker.finish(status < 400, status) }()

	resp, err := p.Chat(r.Context(), params, isStream)
	if err != nil {
		status = providerErrorStatus(err)
		log.Printf("  provider api error (%s): %v", name, err)
		writeJSON(w, status, map[string]any{
			"error": map[string]string{"message": err.Error(), "type": "api_error"},
		})
		return
	}
	defer resp.Body.Close()
	status = resp.StatusCode
	usageFn := func(u map[string]any) { tracker.observeUsage(u) }
	// 工具名还原映射: 从同一 params 对象的旁路键读出并移除。
	// 出处 responseTranslator.ts:165/173 `restoreOpenAIToolNames(responseBody, toolNameMap)` ——
	// 客户端是 OpenAI 形态（/v1/chat/completions）而上游是 anthropic 形状时必须还原，
	// 否则客户端收到自己从未声明过的工具名。无伪装时为 nil, 行为不变。
	responseToolNameMap := takeToolNameMap(params)
	if isStream {
		status = handleStreamResponseWithToolNameMap(w, resp, usageFn, responseToolNameMap)
		return
	}
	status = handleNonStreamResponseWithToolNameMap(w, resp, usageFn, responseToolNameMap)
}
