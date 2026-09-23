package app

// zen 上游的 Responses 专用模型适配。
//
// 背景: zen 上游的部分模型(如 muse-spark-*-contributor-free 家族)只在
// /v1/responses 端点提供服务, 走 /v1/chat/completions 会得到上游后端的
// 500 "Internal server error"(上游没有把"端点不支持"翻译成 4xx, 而是直接
// 崩成 500)。opencode 官方客户端对这类模型使用 @ai-sdk/openai(即 Responses
// API), 所以官方能用而网关原来的 chat/completions 通道全出口 502。
//
// 另外免费档要求会话身份头(x-opencode-session / x-opencode-request),
// 缺失时 /responses 返回 400 MissingSessionID —— callZenAPI 本来就带这些头,
// 这里只需沿用。
//
// 策略: 自适应学习。某模型 chat/completions 吃 500 时, 用同一出口/同一会话
// 身份向 /responses 发一次等价请求(请求体做 chat→Responses 转换); 成功则把
// 该模型记入"Responses 专用"名单并持久化, 之后直接走 /responses。负向结论
// (/responses 也失败)只在进程内记住, 不落盘 —— 上游随时可能修复, 不能写死。

import (
	"encoding/json"
	"fmt"
	"free-router/internal/app/translate_registry"
	"free-router/internal/kit"
	"os"
	"strings"
	"sync"
	"time"
)

// anyToInt 把 JSON 数值(int/float64/json.Number/字符串)安全转 int, 失败返回 0。
func anyToInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	case string:
		var i int
		fmt.Sscanf(n, "%d", &i)
		return i
	default:
		return 0
	}
}

// ============ 模型 API 形态登记 ============

var (
	zenRespOnlyMu     sync.Mutex
	zenRespOnlyLoaded bool
	zenRespOnly       = map[string]bool{} // 已确认 Responses 专用(持久化)
	zenChatOnlyMemo   = map[string]bool{} // 已确认 chat 可用/Responses 也失败(仅进程内)
)

func zenResponsesOnlyFile() string {
	if zenResponsesOnlyFileOverride != "" {
		return zenResponsesOnlyFileOverride
	}
	return kit.ResolveDataPath("zen-responses-only.json")
}

// 测试注入点: 把登记文件重定向到临时路径。
var zenResponsesOnlyFileOverride string

func setZenResponsesOnlyFileForTest(path string) {
	zenResponsesOnlyFileOverride = path
}

func loadZenResponsesOnlyLocked() {
	if zenRespOnlyLoaded {
		return
	}
	zenRespOnlyLoaded = true
	raw, err := os.ReadFile(zenResponsesOnlyFile())
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
		zenRespOnly[m] = true
	}
}

func persistZenResponsesOnlyLocked() {
	models := make([]string, 0, len(zenRespOnly))
	for m := range zenRespOnly {
		models = append(models, m)
	}
	doc := map[string]any{"syncedAt": time.Now().Unix(), "models": models}
	if raw, err := json.Marshal(doc); err == nil {
		_ = kit.WriteFileAtomicDefault(zenResponsesOnlyFile(), raw)
	}
}

// zenStaticResponsesOnly 静态规则: muse 免费档家族(muse-* 且 -free 结尾,
// 如 muse-spark-1.2/1.3-contributor-free)已实测只在上游 /responses 端点
// 提供, chat/completions 一律 500。固定直走, 无需探测。
// 注意范围必须窄: 其他 -free 模型(如 mimo-v2.5-free)在 chat/completions
// 上工作正常, 不能按后缀一刀切。
func zenStaticResponsesOnly(zenModelID string) bool {
	return strings.HasPrefix(zenModelID, "muse-") && strings.HasSuffix(zenModelID, "-free")
}

// zenUseResponsesAPI 该模型是否已知必须走 /responses 端点。
func zenUseResponsesAPI(zenModelID string) bool {
	if zenModelID == "" {
		return false
	}
	if zenStaticResponsesOnly(zenModelID) {
		return true
	}
	zenRespOnlyMu.Lock()
	defer zenRespOnlyMu.Unlock()
	loadZenResponsesOnlyLocked()
	return zenRespOnly[zenModelID]
}

// zenLearnResponsesOnly 成功经 /responses 调通的模型, 持久化登记。
func zenLearnResponsesOnly(zenModelID string) {
	if zenModelID == "" {
		return
	}
	zenRespOnlyMu.Lock()
	defer zenRespOnlyMu.Unlock()
	zenRespOnlyLoaded = true
	if !zenRespOnly[zenModelID] {
		zenRespOnly[zenModelID] = true
		persistZenResponsesOnlyLocked()
	}
	delete(zenChatOnlyMemo, zenModelID)
}

// zenMemoChatOnly /responses 也调不通(或已确认 chat 正常), 进程内记住,
// 本轮进程不再对它做 Responses 回退。
func zenMemoChatOnly(zenModelID string) {
	zenRespOnlyMu.Lock()
	defer zenRespOnlyMu.Unlock()
	zenChatOnlyMemo[zenModelID] = true
}

func zenChatOnlyKnown(zenModelID string) bool {
	zenRespOnlyMu.Lock()
	defer zenRespOnlyMu.Unlock()
	return zenChatOnlyMemo[zenModelID]
}

// ============ 请求转换: OpenAI chat -> Responses ============

// contentText 把消息 content 归一成纯文本。字符串原样; 分块数组拼接全部
// text/input_text/output_text 块(图片等非文本块忽略 —— zen 免费档 MVP 不做视觉)。
func contentText(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		var b strings.Builder
		for _, part := range v {
			pm, ok := part.(map[string]any)
			if !ok {
				continue
			}
			switch pm["type"] {
			case "text", "input_text", "output_text":
				if s, ok := pm["text"].(string); ok {
					b.WriteString(s)
				}
			}
		}
		return b.String()
	default:
		return ""
	}
}

// init 把请求体转换实现注册进 translate 注册表(P1-9): 调用方一律经
// translate_registry.TranslateRequest 取转换结果, 实现集中在本文件, 未来整体搬迁到
// translate 包时调用方无需再改。
func init() {
	translate_registry.RegisterRequest(translate_registry.Chat, translate_registry.Responses, chatBodyToResponsesBody)
}
