// Package translate_registry 网关请求体转换的**统一注册表 + 出站体形态不变量校验** (P1-9)。
//
// 命名说明(2026-09-16 去歧义): 本包原名 `internal/app/translate`, 与
// `internal/translate`(OmniRoute 移植的协议互转实现, 批次⑤待接线)同名不同路径,
// 极易改错包。现按职责改名为 translate_registry —— 本包只做"取转换函数 + 校验
// 出站体", 具体协议互转实现归 internal/translate。
//
// 背景: 网关要在三种协议形状之间转换 —— chat(completions) / responses /
// anthropic。历史上转换逻辑散落在 proxy.go / responses.go / zen_responses.go
// 各处, 曾发生过"同一函数被调两次, Responses 体被当 chat 体再转一遍,
// input 被转成空数组"的事故(编译通过、测试全绿, 只有真机 400 才暴露)。
//
// 本包提供两道防线:
//
//  1. **单一入口**: 各方向的转换函数在 app 包里注册到本包(按 from:to 键),
//     调用方一律经 TranslateRequest 取转换结果 —— 未来把实现逐个搬进来时,
//     调用方代码不再变。
//
//  2. **出站体形态不变量**: 每种形状都有一组硬性不变量(见 ValidateOutbound),
//     发往上游之前校验, 违例即拦截 —— 把"转换坏了"从上游 400 变成网关
//     500 + 明确错误信息, 并在单测里固化(防止同类事故再次静默通过)。
//
// 不变量清单(ValidateOutbound):
//
//	responses: 必须有 model; input 必须是非空数组(除非带 previous_response_id);
//	           不得残留 chat 形状的 messages / max_tokens / max_completion_tokens。
//	chat:      必须有 model 与非空 messages; 不得残留 responses 的 input。
//	anthropic: 必须有 model、非空 messages、正整数 max_tokens;
//	           不得残留 responses 形状字段 input。
//
// 与 internal/translate 的分工(R2 审计 F4): 本包是**注册表与出站校验**(已接
// 生产, 由 zen_call.go / zen_responses.go 消费); internal/translate 是 OmniRoute
// open-sse/translator 的**格式互转实现**(批次⑤预铺, 尚未接线)。两包职责不同,
// 且已按职责命名区分, 引用时仍以完整 import 路径为准。
package translate_registry

import (
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
)

// Kind 协议形状标识。
type Kind string

const (
	Chat      Kind = "chat"      // OpenAI chat completions
	Responses Kind = "responses" // OpenAI Responses
	Anthropic Kind = "anthropic" // Anthropic messages
)

type key struct{ from, to Kind }

var (
	reqMu     sync.Mutex
	requestFn = map[key]func(map[string]any) map[string]any{}
)

// RegisterRequest 注册 from→to 的请求体转换实现(同一方向重复注册以最后一次为准)。
func RegisterRequest(from, to Kind, fn func(map[string]any) map[string]any) {
	reqMu.Lock()
	defer reqMu.Unlock()
	requestFn[key{from, to}] = fn
}

// HasRequest 该方向是否注册了转换实现。
func HasRequest(from, to Kind) bool {
	reqMu.Lock()
	defer reqMu.Unlock()
	return requestFn[key{from, to}] != nil
}

// 未注册方向告警去重: 每个方向只告警一次。热路径上不能每请求刷日志,
// 但"完全静默"会让漏注册长期无人发现(审计 P1-6)。
var warnedUnregistered sync.Map

// TranslateRequest 按 from:to 转换请求体; 未注册该方向时原样返回(幂等转换语义:
// 同形状 = 不需要转换)。body 为 nil 时返回 nil。
func TranslateRequest(from, to Kind, body map[string]any) map[string]any {
	if body == nil {
		return nil
	}
	if from == to {
		return body
	}
	// 读注册表必须与 RegisterRequest 互斥(与 F1 同类: 裸读 map 并发注册即竞争)。
	reqMu.Lock()
	fn := requestFn[key{from, to}]
	reqMu.Unlock()
	if fn != nil {
		return fn(body)
	}
	// 未注册方向: 原样返回并让出站校验兜底 —— 宁可上游报错, 也不在这里猜。
	if _, loaded := warnedUnregistered.LoadOrStore(key{from, to}, true); !loaded {
		log.Printf("translate_registry: 转换方向 %s→%s 未注册, 请求体原样透传(补注册或见 internal/translate)", from, to)
	}
	return body
}

// RegisteredDirections 已注册的转换方向(诊断/测试用, 排序后返回)。
func RegisteredDirections() []string {
	reqMu.Lock()
	defer reqMu.Unlock()
	out := make([]string, 0, len(requestFn))
	for k := range requestFn {
		out = append(out, string(k.from)+"→"+string(k.to))
	}
	sort.Strings(out)
	return out
}

// ValidateOutbound 出站体形态不变量校验; 返回问题列表(空=通过)。
// 违例说明上游会收到畸形请求 —— 调用方应拦截并发 500, 而不是把垃圾发给上游。
func ValidateOutbound(kind Kind, body map[string]any) []string {
	if body == nil {
		return []string{"outbound body is nil"}
	}
	var problems []string
	model, _ := body["model"].(string)
	if strings.TrimSpace(model) == "" {
		problems = append(problems, "缺少 model")
	}
	switch kind {
	case Responses:
		if _, hasPRID := body["previous_response_id"]; !hasPRID {
			input, _ := body["input"].([]any)
			if len(input) == 0 {
				problems = append(problems, "input 为空数组(必须是至少一条消息, 或使用 previous_response_id)")
			}
		}
		for _, leftover := range []string{"messages", "max_tokens", "max_completion_tokens"} {
			if _, has := body[leftover]; has {
				problems = append(problems, "残留 chat 形状字段 "+leftover+"(未转换干净, 疑似重复转换)")
			}
		}
	case Chat:
		msgs, _ := body["messages"].([]any)
		if len(msgs) == 0 {
			problems = append(problems, "messages 为空数组")
		}
		if _, has := body["input"]; has {
			problems = append(problems, "残留 responses 形状字段 input")
		}
	case Anthropic:
		msgs, _ := body["messages"].([]any)
		if len(msgs) == 0 {
			problems = append(problems, "messages 为空数组")
		}
		if mt, ok := body["max_tokens"].(float64); !ok || mt <= 0 {
			problems = append(problems, "max_tokens 必须是正数(anthropic 协议必填)")
		}
		if _, has := body["input"]; has {
			problems = append(problems, "残留 responses 形状字段 input")
		}
	default:
		problems = append(problems, fmt.Sprintf("未知形状 %q", kind))
	}
	return problems
}
