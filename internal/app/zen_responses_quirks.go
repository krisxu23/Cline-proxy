package app

import (
	"bytes"
	"cline-go-proxy/internal/kit"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strings"
)

// museSparkMinOutputTokens muse-spark 家族的最小输出预算。
//
// 参照 OmniRoute 的 MUSE_SPARK_MIN_OUTPUT_TOKENS=512: 该家族会先把预算烧在
// 不可见的服务端推理上, 预算太小则可见正文恒为空(我们实测 300 与 800 都是空正文),
// 客户端会误判成"模型坏了"。只在调用方给了预算且小于该值时抬高。
const museSparkMinOutputTokens = 512

// museSparkDefaultOutputTokens 调用方**没有**给输出预算时的兜底预算。
//
// 为什么必须兜底(2026-09-16 实锤): Codex / cc-switch 这类客户端根本不发
// max_tokens, 于是我们此前按"与上游默认一致"**完全不发** max_output_tokens,
// 上游默认预算被隐藏推理吃光 → 收到 `response.completed` 但 output_tokens=0、
// 可见正文为空。实测当天 394 次成功请求里有 12 次是这种空回合, 客户端表现为
// "任务无缘无故中断, 没有提示也没有报错"(agent 收到一个空回合就结束了这一轮)。
//
// 8192 的取法: 远大于 512 下限, 足以覆盖一次 agent 回合的可见输出(工具调用 +
// 说明文字), 又远小于该模型 output=32768 的上限, 不至于让单次请求无限拉长。
const museSparkDefaultOutputTokens = 8192

// applyMuseSparkBudget 为 muse-spark 家族补足输出预算。
//
// 规则: 调用方给了就按其值(小于下限抬到下限); 没给则补 museSparkDefaultOutputTokens。
// 该家族的"假截断"(finish_reason=length 但其实答完了)修正依赖 requestedBudget,
// 兜底之后这个值也始终存在, 修正才有依据。
func applyMuseSparkBudget(out map[string]any, modelID string) {
	if !isMuseSparkModel(modelID) {
		return
	}
	if n, ok := anyToIntOK(out["max_output_tokens"]); ok && n > 0 {
		if n < museSparkMinOutputTokens {
			out["max_output_tokens"] = museSparkMinOutputTokens
		}
		return
	}
	out["max_output_tokens"] = museSparkDefaultOutputTokens
}

// anyToIntOK anyToInt 的存在性版本(anyToInt 把 0 与非法都归成 0, 这里要区分)。
func anyToIntOK(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return int(i), true
		}
	}
	return 0, false
}

// isMuseSparkModel 是否 muse-spark 家族(该家族有若干上游特有的行为需要单独处理)。
func isMuseSparkModel(modelID string) bool {
	return strings.HasPrefix(strings.TrimSpace(modelID), "muse-spark")
}

// normalizeMuseSparkFinish 修正 muse-spark 的 finish_reason。
//
// 上游只要推理吃掉了一部分预算就报 finish_reason:"length", 哪怕可见回答已经完整
// (OmniRoute 实测: 128000 预算的请求只产出约 270 tokens 也报 length)。OpenAI 协议
// 客户端会把 length 当成"被截断" —— Claude Code 会直接以
// "response exceeded the N output token maximum" 中止一次已经交付完的回答。
//
// 规则: 完成量明显小于请求预算(不足 90%)时改写 length → stop; 真正撞到预算上限的
// 截断保持 length。请求方没给预算(无法判断)时不动。
func normalizeMuseSparkFinish(finish, modelID string, completion, requestedBudget int) string {
	if finish != "length" || requestedBudget <= 0 || !isMuseSparkModel(modelID) {
		return finish
	}
	if completion < requestedBudget*9/10 {
		return "stop"
	}
	return finish
}

// zenRequestedBudget 取客户端请求的输出预算(max_tokens / max_completion_tokens)。
// 未给预算返回 0 —— muse 的假截断判定在无预算时不生效(无法区分真截断)。
func zenRequestedBudget(params map[string]any) int {
	for _, key := range []string{"max_tokens", "max_completion_tokens"} {
		if v, ok := params[key]; ok {
			switch n := v.(type) {
			case float64:
				if n > 0 {
					return int(n)
				}
			case int:
				if n > 0 {
					return n
				}
			}
		}
	}
	return 0
}

// applyResponsesReasoning 把 chat 侧的 reasoning_effort 映射为 Responses 的
// reasoning.effort。客户端未指定(或值非法)时默认 low。
//
// 原因: 该模型上游默认 effort=high, 对简单提问也会把全部 max_output_tokens
// 烧在推理上 —— 预算 800 时正文为空(实测), 客户端以为模型坏了。降到 low
// 后推理占用大幅缩小, 同样的预算就能拿到正文; 客户端若显式传了
// reasoning_effort 则尊重其选择。
func applyResponsesReasoning(body map[string]any, effort string) {
	eff := strings.ToLower(strings.TrimSpace(effort))
	switch eff {
	case "minimal", "low", "medium", "high", "xhigh":
	default:
		eff = "low"
	}
	body["reasoning"] = map[string]any{"effort": eff}
}

// zenReasoningEffortOf 从 chat 请求参数里取客户端指定的推理强度(两种写法)。
func zenReasoningEffortOf(params map[string]any) string {
	for _, key := range []string{"reasoning_effort", "reasoningEffort"} {
		if v, ok := params[key].(string); ok && strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// tryZenResponsesFallback chat/completions 吃 500 时的自适应回退: 用同一出口、
// 同一会话身份向 /responses 发一次等价请求。成功 -> 登记该模型为 Responses
// 专用(持久化)并返回合成好的 chat 形态响应; 失败 -> 返回 nil, 调用方继续
// 原有的换出口重试流程。
func tryZenResponsesFallback(ctx context.Context, base string, respBody map[string]any, stream bool, client *http.Client, requestedBudget int) *http.Response {
	modelID, _ := respBody["model"].(string)
	if modelID == "" || zenChatOnlyKnown(modelID) {
		return nil
	}
	raw, err := json.Marshal(respBody)
	if err != nil {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, "POST", base+"/responses", bytes.NewReader(raw))
	if err != nil {
		return nil
	}
	outbound := map[string]string{}
	// /responses 同样用官方 CLI 身份(ses_ 形态 session, 官方客户端无 UUID 分支)。
	applyOpencodeHeaders(outbound, nil, defaultOpencodeIdentity(), bodyFingerprint(respBody))
	req.Header.Set("Authorization", "Bearer "+getZenConfig().Key)
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
		raw := kit.ReadBody(resp)
		resp.Body.Close()
		// 4xx 说明模型/端点层面明确不买账(而非出口线路问题), 进程内记住
		// 不再对它回退; 5xx 可能只是这条线路坏, 不记。
		if resp.StatusCode < 500 {
			zenMemoChatOnly(modelID)
		}
		log.Printf("  zen: model %s 的 /responses 回退未命中(%d): %s", modelID, resp.StatusCode, kit.Truncate(raw, 200))
		return nil
	}
	log.Printf("  zen: model %s 只在 /responses 端点提供服务, 已自动切换并登记", modelID)
	zenLearnResponsesOnly(modelID)
	if stream {
		return wrapResponsesStreamToChat(resp, modelID, requestedBudget)
	}
	conv, err := convertResponsesResponseToChat(resp, modelID, requestedBudget)
	if err != nil {
		log.Printf("  zen: model %s /responses 响应转换失败: %v", modelID, err)
		return nil
	}
	return conv
}
