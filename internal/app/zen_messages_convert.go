package app

// /messages 端点的请求/响应转换。
//
// 背景: zen 的部分模型(官方文档明确列出 union-alpha)只在 **Anthropic Messages**
// 端点 /messages 提供服务。网关对客户端说 OpenAI 形态, 因此这一层负责两个方向的翻译:
//
//	出站: OpenAI chat 请求体 → Anthropic Messages 请求体
//	回程: Anthropic 响应/SSE → OpenAI chat 响应/SSE
//
// ★ 这一层**就是 internal/translate 包**——它早就写好了(openai→claude 请求方向 +
//   claude→openai 响应方向), 但生产调用点为零。本文件是它的第一个接线点。
//   详见 docs/opencode-zen-facts.md 第 5 节与体检报告 N-1 项。
//
// 接线方式(与 /responses 路径同构, 见 zen_responses_convert.go):
//   请求侧: 转换 body → 发给 /messages
//   流式:   Anthropic SSE 经 io.Pipe 实时转成 chat SSE, 再交给既有的
//           OpenAI 流式处理链 —— 这样空流守卫、坏帧清洗、心跳全都照常生效,
//           不需要为 /messages 单独写一套。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"

	"free-router/internal/kit"
	"free-router/internal/translate"
)

// translateZenMessagesRequest OpenAI chat 请求体 → Anthropic Messages 请求体。
//
// 失败时返回错误: 宁可网关 500 也不把畸形请求发给上游换回难以理解的 400。
func translateZenMessagesRequest(model string, body map[string]any, stream bool) (map[string]any, error) {
	out, err := translate.OpenAIChatToClaudeRequest(model, body, stream)
	if err != nil {
		return nil, fmt.Errorf("messages request translate: %w", err)
	}
	if out == nil {
		return nil, fmt.Errorf("messages request translate: 产出为空")
	}
	return out, nil
}

// wrapClaudeStreamToChat 把 /messages 的 Anthropic SSE 实时转成 chat 形态 SSE。
//
// 用 io.Pipe 做流式转换, 而不是先缓冲整个响应 —— 后者会引入首字节延迟并让
// 长回答占用内存。转换完成后关闭管道; 转换出错时把错误带进管道,
// 上游的读侧会看到 err 而中止, 不会静默截断。
func wrapClaudeStreamToChat(resp *http.Response, model string) *http.Response {
	pr, pw := io.Pipe()
	go func() {
		err := translate.ClaudeSSEToOpenAISSE(resp.Body, pw, model)
		resp.Body.Close()
		pw.CloseWithError(err)
	}()
	return synthesizeChatSSEResponse(pr)
}

// convertClaudeResponseToChat 非流式: 拉全响应体, 翻译回 chat completions 形状。
func convertClaudeResponseToChat(resp *http.Response, fallbackModel string) (*http.Response, error) {
	raw := kit.ReadBody(resp)
	resp.Body.Close()
	chat, err := translate.ClaudeResponseToOpenAIChat([]byte(raw))
	if err != nil {
		return nil, fmt.Errorf("messages response translate: %w", err)
	}
	// 与 /responses 路径同构: 拦住"空回合", 让调用方换出口/换候选,
	// 而不是把一个空消息交付给客户端(那会让 agent 静默结束)。
	if claudeChatEmpty(chat) {
		return nil, fmt.Errorf("上游 %s 经 /messages 返回空回合(无正文且无工具调用): 通常是输出预算不足或上游配额/地区问题", fallbackModel)
	}
	// 补 model 字段: chat 形态的调用方会读它
	if _, ok := chat["model"].(string); !ok {
		chat["model"] = fallbackModel
	}
	out, err := json.Marshal(chat)
	if err != nil {
		return nil, fmt.Errorf("marshal converted chat body: %w", err)
	}
	return synthesizeChatJSONResponse(out), nil
}

// claudeChatEmpty 转换后的 chat 响应是否"什么都没产出"。
//
// 判据与 stream_delivery.go 的流级口径一致: 正文 / 工具调用任一存在即算产出。
func claudeChatEmpty(chat map[string]any) bool {
	choices, _ := chat["choices"].([]any)
	if len(choices) == 0 {
		return true
	}
	c0, _ := choices[0].(map[string]any)
	if c0 == nil {
		return true
	}
	msg, _ := c0["message"].(map[string]any)
	if msg == nil {
		return true
	}
	return !messageDeliversUserContent(msg)
}

// tryZenMessagesFallback chat 端点失败后, 用**同一出口**向 /messages 发一次等价请求。
//
// 与 tryZenResponsesFallback(zen_responses_quirks.go:140)同构, 差别:
//   - 路径 /messages, 鉴权用 x-api-key + anthropic-version
//   - 请求体先转成 Anthropic 形态, 响应转回 chat 形态
//
// 为什么要这一步: 官方文档的端点矩阵描述的是**官方客户端走哪条路**, 不等于
// "只有那条路能通"。静态表只登记有依据的条目(见 zenStaticEndpoint), 其余模型
// 靠这里实测纠正 —— 试通了就持久化登记, 之后直接走 /messages。
//
// 纪律: **负向结论不落盘**(与 zen_responses.go 一致)。上游随时可能修复端点支持。
// key 为调用方按出口选好的那把(经 zenSelectKey: TrimSpace / 去重 / 跳过退役),
// 与 tryZenResponsesFallback 同口径 —— 不再裸取 getZenConfig().Key(P2-15)。
func tryZenMessagesFallback(ctx context.Context, base string, chatBody map[string]any, stream bool, client *http.Client, key string) *http.Response {
	modelID, _ := chatBody["model"].(string)
	if modelID == "" || zenEndpointChatOnlyKnown(modelID) {
		return nil
	}
	// 没有可用 key: 不构造空 x-api-key 头打过去换回一个 401(见 P2-15)。
	if key == "" {
		log.Printf("  zen: model %s 的 /messages 回退跳过(没有可用的 zen key)", modelID)
		return nil
	}
	msgBody, err := translateZenMessagesRequest(modelID, chatBody, stream)
	if err != nil {
		log.Printf("  zen: model %s 的 /messages 回退请求转换失败: %v", modelID, err)
		return nil
	}
	raw, err := json.Marshal(msgBody)
	if err != nil {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, "POST", base+"/messages", bytes.NewReader(raw))
	if err != nil {
		return nil
	}
	outbound := map[string]string{}
	// /messages 同样用官方 CLI 身份 —— 门禁判的是"来自 OpenCode", 与端点无关。
	applyOpencodeHeaders(outbound, nil, defaultOpencodeIdentity(), bodyFingerprint(chatBody))
	req.Header.Set("x-api-key", key)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("Content-Type", "application/json")
	for k, v := range outbound {
		req.Header.Set(k, v)
	}
	req.Header.Set("x-opencode-model", modelID)

	resp, err := client.Do(req)
	if err != nil {
		return nil // 网络问题不记负向结论, 换出口后 chat 重试照旧
	}
	if resp.StatusCode != http.StatusOK {
		rawBody := kit.ReadBody(resp)
		resp.Body.Close()
		// 只对表明「端点不支持该模型」的失败记负向(404 / not found / unsupported);
		// 401(key)、429(额度)、408 及其它 4xx 是瞬时/凭据问题, 与端点无关,
		// 不落 memo 按正常错误返回; 5xx 可能只是这条线路坏, 同样不记(P2-14)。
		if zenEndpointUnsupported(resp.StatusCode, rawBody) {
			zenMemoEndpointChatOnly(modelID)
		}
		log.Printf("  zen: model %s 的 /messages 回退未命中(%d): %s", modelID, resp.StatusCode, kit.Truncate(rawBody, 200))
		return nil
	}
	log.Printf("  zen: model %s 只在 /messages 端点提供服务, 已自动切换并登记", modelID)
	zenLearnMessagesOnly(modelID)
	if stream {
		return wrapClaudeStreamToChat(resp, modelID)
	}
	conv, err := convertClaudeResponseToChat(resp, modelID)
	if err != nil {
		log.Printf("  zen: model %s /messages 响应转换失败: %v", modelID, err)
		return nil
	}
	return conv
}
