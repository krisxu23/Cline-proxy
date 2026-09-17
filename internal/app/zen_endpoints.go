package app

// zen 出站端点选择。
//
// 官方文档(https://opencode.ai/docs/zh-cn/zen)给出 **4 种**端点, 不同模型走不同
// 的 API 形态 —— 这不是可选项, 是硬约束。7 个免费模型跨 **3 个**端点:
//
//	/chat/completions   OpenAI 兼容       big-pickle · mimo-v2.5-free ·
//	                                      ling-3.0-flash-fin-free ·
//	                                      nemotron-3-ultra-free ·
//	                                      nemotron-3.5-lightning-free
//	/responses          OpenAI Responses  muse-spark-1.3-contributor-free
//	/messages           Anthropic Messages union-alpha
//	/models/<model-id>  Google 原生       全部 Gemini(当前无免费模型)
//
// 完整端点矩阵、依据与隐私条款见 docs/opencode-zen-facts.md 第 1、2 节。
//
// ★ 本文件补齐的缺口(2026-09-17 审查): 此前出站只有 /chat/completions 与
//   /responses 两个分支, **缺 /messages 与 /models/<id>** —— 于是 union-alpha
//   (免费, 只在 /messages)会被发到 /chat/completions 必然失败, 而且
//   zenUseResponsesAPI 的学习逻辑只在 5xx 时试 /responses, 不会试 /messages,
//   所以这个失败**学不到**, 每次请求都白撞一次。

import (
	"encoding/json"
	"os"
	"strings"
	"sync"
	"time"

	"cline-go-proxy/internal/kit"
)

// zenEndpointKind zen 出站端点形态。
type zenEndpointKind int

const (
	zenEndpointChat      zenEndpointKind = iota // /chat/completions(默认)
	zenEndpointResponses                        // /responses
	zenEndpointMessages                         // /messages
	zenEndpointGemini                           // /models/<model-id>
)

func (k zenEndpointKind) String() string {
	switch k {
	case zenEndpointResponses:
		return "responses"
	case zenEndpointMessages:
		return "messages"
	case zenEndpointGemini:
		return "gemini"
	default:
		return "chat"
	}
}

// pathFor 该端点在 zen base URL 下的**相对**路径。
//
// ★ 拼接的坑: base(https://opencode.ai/zen/v1)**已含 /v1**, 而官方文档里
//
//	/messages 写的是完整路径 https://opencode.ai/zen/v1/messages ——
//	所以相对路径是 "/messages", 不是 "/v1/messages"。
//	写成后者会得到 .../zen/v1/v1/messages。
func (k zenEndpointKind) pathFor(modelID string) string {
	switch k {
	case zenEndpointResponses:
		return "/responses"
	case zenEndpointMessages:
		return "/messages"
	case zenEndpointGemini:
		return "/models/" + modelID
	default:
		return "/chat/completions"
	}
}

// usesAnthropicAuth 该端点是否要 Anthropic 方言的鉴权头。
//
// Anthropic 协议用 x-api-key + anthropic-version, 不是 Bearer。
// [推断] 依据是 Anthropic 协议惯例 —— 官方文档未写明 zen 的 /messages 鉴权方式。
// 若实测发现 zen 也收 Bearer, 这里改成同时发两种头即可(见 docs 第 3 节 [未知] 项)。
func (k zenEndpointKind) usesAnthropicAuth() bool {
	return k == zenEndpointMessages
}

// ============ 静态端点表 ============

// zenStaticEndpoint 官方文档端点矩阵里**有明确依据**的条目。
//
// ★ 只登记有依据的条目, 不按模型名前缀批量推断:
//   - muse-* 免费家族 → responses: 已**实测**(chat/completions 一律被后端崩成 500)
//   - union-alpha → messages: 官方文档端点矩阵明确列出
//
// 其余模型(含付费的 Claude / Qwen / GPT / Grok 系列)返回 false 表示"不介入",
// 沿用默认 chat 端点。理由: 官方文档的端点矩阵描述的是**官方客户端走哪条路**,
// 不等于"只有那条路能通" —— 按模型名前缀批量改路由, 可能把当前能用的付费模型
// 改坏。这类模型交给下面的**学习机制**在实测失败后自行纠正。
func zenStaticEndpoint(modelID string) (zenEndpointKind, bool) {
	if zenStaticResponsesOnly(modelID) {
		return zenEndpointResponses, true
	}
	if isZenMessagesOnlyStatic(modelID) {
		return zenEndpointMessages, true
	}
	return zenEndpointChat, false
}

// isZenMessagesOnlyStatic 官方文档明确走 /messages 的模型。
// 目前只有 union-alpha(隐身模型, 免费档)。
func isZenMessagesOnlyStatic(modelID string) bool {
	return strings.EqualFold(strings.TrimSpace(modelID), "union-alpha")
}

// ============ 端点学习(实测纠正) ============

// 静态表只覆盖有依据的条目; 其余模型靠实测纠正: 首选端点失败时依次试其他形态,
// 试通了就记下来。
//
// ★ 纪律(沿用 zen_responses.go 的既有设计): **负向结论不落盘**。上游随时可能
//
//	修复端点支持, 写死了就永远错了。负向只记在进程内。
var (
	zenMsgOnlyMu            sync.Mutex
	zenMsgOnlyLoaded        bool
	zenMsgOnly              = map[string]bool{} // 已确认走 /messages 的模型(持久化)
	zenEndpointChatOnlyMemo = map[string]bool{} // 各形态都失败 → 只走 chat(仅进程内)
)

func zenMessagesOnlyFile() string {
	if zenMessagesOnlyFileOverride != "" {
		return zenMessagesOnlyFileOverride
	}
	return kit.ResolveDataPath("zen-messages-only.json")
}

// 测试注入点: 把登记文件重定向到临时路径。
var zenMessagesOnlyFileOverride string

func setZenMessagesOnlyFileForTest(path string) {
	zenMessagesOnlyFileOverride = path
}

func loadZenMessagesOnlyLocked() {
	if zenMsgOnlyLoaded {
		return
	}
	zenMsgOnlyLoaded = true
	raw, err := os.ReadFile(zenMessagesOnlyFile())
	if err != nil || len(raw) == 0 {
		return
	}
	var doc struct {
		Models []string `json:"models"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return
	}
	for _, m := range doc.Models {
		zenMsgOnly[m] = true
	}
}

func persistZenMessagesOnlyLocked() {
	models := make([]string, 0, len(zenMsgOnly))
	for m := range zenMsgOnly {
		models = append(models, m)
	}
	doc := map[string]any{"syncedAt": time.Now().Unix(), "models": models}
	if raw, err := json.Marshal(doc); err == nil {
		_ = kit.WriteFileAtomicDefault(zenMessagesOnlyFile(), raw)
	}
}

// zenLearnMessagesOnly 成功经 /messages 调通的模型, 持久化登记。
func zenLearnMessagesOnly(zenModelID string) {
	if zenModelID == "" {
		return
	}
	zenMsgOnlyMu.Lock()
	defer zenMsgOnlyMu.Unlock()
	loadZenMessagesOnlyLocked()
	if !zenMsgOnly[zenModelID] {
		zenMsgOnly[zenModelID] = true
		persistZenMessagesOnlyLocked()
	}
}

func zenMessagesOnlyKnown(zenModelID string) bool {
	if zenModelID == "" {
		return false
	}
	zenMsgOnlyMu.Lock()
	defer zenMsgOnlyMu.Unlock()
	loadZenMessagesOnlyLocked()
	return zenMsgOnly[zenModelID]
}

// zenMemoEndpointChatOnly 所有形态都失败 → 本轮进程不再对该模型做端点回退。
func zenMemoEndpointChatOnly(zenModelID string) {
	if zenModelID == "" {
		return
	}
	zenMsgOnlyMu.Lock()
	defer zenMsgOnlyMu.Unlock()
	zenEndpointChatOnlyMemo[zenModelID] = true
}

func zenEndpointChatOnlyKnown(zenModelID string) bool {
	zenMsgOnlyMu.Lock()
	defer zenMsgOnlyMu.Unlock()
	return zenEndpointChatOnlyMemo[zenModelID]
}

// zenEndpointFor 该模型在 zen 上游应该走哪个端点。
//
// 优先级: 实测学习结果 > 官方静态表 > 默认 chat。
func zenEndpointFor(modelID string) zenEndpointKind {
	if modelID == "" {
		return zenEndpointChat
	}
	// 1. 实测学习(最可信)
	if zenMessagesOnlyKnown(modelID) {
		return zenEndpointMessages
	}
	if zenUseResponsesAPI(modelID) {
		return zenEndpointResponses
	}
	// 2. 官方静态表
	if k, ok := zenStaticEndpoint(modelID); ok {
		return k
	}
	// 3. 默认
	return zenEndpointChat
}

// zenEndpointFallbacks 首选端点失败后依次尝试的**其他**形态。
//
// 顺序: chat → responses → messages。理由:
//   - chat 是最通用的形态, 先试成本最低
//   - responses 已有成熟转换层(translate_registry)
//   - messages 需要 Anthropic 转换层, 放最后
//
// 全部形态都失败时返回空切片(调用方据此记"只走 chat")。
func zenEndpointFallbacks(primary zenEndpointKind) []zenEndpointKind {
	all := []zenEndpointKind{zenEndpointChat, zenEndpointResponses, zenEndpointMessages}
	out := make([]zenEndpointKind, 0, len(all)-1)
	for _, k := range all {
		if k != primary {
			out = append(out, k)
		}
	}
	return out
}
