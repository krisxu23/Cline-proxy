package app

// 供应商协议形态(APIFormat)。
//
// 三种形态(与 opencode 官方文档的端点矩阵一致, 见 docs/opencode-zen-facts.md):
//
//	chat        /chat/completions   OpenAI 兼容        ← 默认
//	messages    /messages           Anthropic Messages
//	responses   /responses          OpenAI Responses
//
// 与 zen 端点层(zen_endpoints.go)是**同一个概念**, 但作用对象不同:
//
//	zen_endpoints.go          对**内置的 opencode 上游**按模型自动选端点(含学习)
//	providers_api_format.go   对**用户添加的通用 Provider** 按供应商显式选形态
//
// 转换实现复用 internal/translate 与 internal/app/translate_registry —— 与 zen 的
// /messages、/responses 路径**共用同一套代码**, 不另写一份。这是"同一概念只有
// 一处实现"的落实。

import (
	"fmt"
	"net/http"

	"free-router/internal/app/translate_registry"
	"free-router/internal/translate"
)

const (
	apiFormatChat      = "chat"
	apiFormatMessages  = "messages"
	apiFormatResponses = "responses"
)

// apiFormatPath 该协议形态对应的**相对**路径。
//
// ★ BaseURL 必须含版本段(与既有约定一致), 例如 https://opencode.ai/zen/v1。
// 官方文档里 Anthropic 的完整路径自带 /v1, 所以这里追加的是相对路径 ——
// 写成 /v1/messages 会拼出 .../v1/v1/messages。
func (c providerConfig) apiFormatPath() string {
	switch c.APIFormat {
	case apiFormatMessages:
		return "/messages"
	case apiFormatResponses:
		return "/responses"
	default:
		return "/chat/completions"
	}
}

// usesAnthropicAuth 该配置是否需要 Anthropic 方言的鉴权头(x-api-key + 版本头)。
//
// 两个来源任一命中:
//   - APIFormat == "messages": 协议本身就是 Anthropic, 鉴权自然是它的方言
//   - APIType == "anthropic": 历史字段, 表示"走 chat 路径但用 Anthropic 鉴权"
//     (某些中转站如此) —— 保留原语义, 不改变既有配置的行为
func (c providerConfig) usesAnthropicAuth() bool {
	return c.APIFormat == apiFormatMessages || c.APIType == "anthropic"
}

// convertOutboundRequest 按 APIFormat 把 **chat 形态**的请求体转成目标协议形态。
//
// ★ 入参 params 保持 chat 形态不被修改: 下游还要用它(旁路键回填、响应侧工具名
// 还原等)。返回的是**新的** map, 只用于序列化上行。
func (c providerConfig) convertOutboundRequest(model string, params map[string]any, stream bool) (map[string]any, error) {
	switch c.APIFormat {
	case apiFormatMessages:
		out, err := translate.OpenAIChatToClaudeRequest(model, params, stream)
		if err != nil {
			return nil, fmt.Errorf("provider %s: Anthropic Messages 请求转换失败: %w", model, err)
		}
		if out == nil {
			return nil, fmt.Errorf("provider %s: Anthropic Messages 请求转换产出为空", model)
		}
		return out, nil
	case apiFormatResponses:
		out := translate_registry.TranslateRequest(translate_registry.Chat, translate_registry.Responses, params)
		if problems := translate_registry.ValidateOutbound(translate_registry.Responses, out); len(problems) > 0 {
			return nil, fmt.Errorf("provider %s: Responses 出站体形态不合法: %v", model, problems)
		}
		return out, nil
	default:
		return params, nil // chat: 原样
	}
}

// convertProviderResponse 按 APIFormat 把上游响应转回 **chat 形态**。
//
// 流式走 io.Pipe 实时转换(见 wrapClaudeStreamToChat), 因此下游的空流守卫、
// 坏帧清洗、心跳全部照常生效, 不需要为每种协议单独实现一套。
func (c providerConfig) convertProviderResponse(resp *http.Response, model string, stream bool) (*http.Response, error) {
	switch c.APIFormat {
	case apiFormatMessages:
		if stream {
			return wrapClaudeStreamToChat(resp, model), nil
		}
		return convertClaudeResponseToChat(resp, model)
	case apiFormatResponses:
		if stream {
			return wrapResponsesStreamToChat(resp, model), nil
		}
		return convertResponsesResponseToChat(resp, model)
	default:
		return resp, nil // chat: 原样
	}
}
