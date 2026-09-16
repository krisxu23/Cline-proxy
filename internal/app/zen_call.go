package app

import (
	"bytes"
	"cline-go-proxy/internal/app/translate_registry"
	"cline-go-proxy/internal/kit"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

// buildZenBody 构造 zen 请求体:只带 OpenAI 兼容字段,改写模型为 zen ID
func buildZenBody(params map[string]any, stream bool) map[string]any {
	body := map[string]any{}
	for _, key := range passThroughKeys {
		if val, ok := params[key]; ok {
			body[key] = val
		}
	}
	for _, key := range []string{"model", "messages", "max_tokens", "max_completion_tokens", "stream"} {
		if val, ok := params[key]; ok {
			body[key] = val
		}
	}
	if stream {
		body["stream"] = true
	}
	if model, ok := params["model"].(string); ok {
		if m, ok := resolveZenModel(model); ok {
			body["model"] = m.ID
		}
	}
	delete(body, "reasoning_effort")
	delete(body, "reasoningEffort")
	return body
}

// callZenAPI 调用 zen 上游,带限流防御: 并发信号量 + 指数退避重试 + 端点轮换
// + 代理冷却 + 故障计数。每次重试自动切换到下一个端点(官方 → CDN 镜像)。
// 返回 (响应, 命中限流次数, 错误)
func callZenAPI(ctx context.Context, params map[string]any, stream bool) (*http.Response, int, error) {
	cfg := getZenConfig()
	body := buildZenBody(params, stream)
	// 拨号层需要知道目标模型: 地区受限模型只走通过地区校验的出口节点
	ctx = context.WithValue(ctx, ctxKeyZenModel, zenModelIDOf(params))

	// Responses 专用模型(muse-*-free 家族等): 上游只在 /responses 端点提供
	// 服务, chat/completions 会被后端崩成 500。已登记的模型直接走 Responses
	// 形态, 响应在这里翻译回 chat 格式, 对上层调用方完全透明。
	// 注意用 body["model"](buildZenBody 已解析为 zen ID), 未注册模型也能命中。
	zenResolvedModel, _ := body["model"].(string)
	useRespAPI := zenUseResponsesAPI(zenResolvedModel)
	// reasoning_effort 会被 buildZenBody 删除(chat 端点上上游不接受), 这里
	// 先留存, 供 Responses 形态使用。
	reasoningEffort := zenReasoningEffortOf(params)
	// 客户端请求的输出预算: muse-spark 的 finish_reason 假截断判定需要它。
	zenBudget := zenRequestedBudget(params)
	// muse-spark 家族: 客户端没给预算时, 转换阶段会补 museSparkDefaultOutputTokens,
	// 因此这里的"假截断"修正必须按**实际发给上游的预算**比较 —— 否则预算恒为 0,
	// normalizeMuseSparkFinish 永远不生效, 客户端会把一个正常答完的回合当成
	// "超出输出上限"而中止(该家族 finish_reason 恒报 length 的已知怪癖)。
	if zenBudget == 0 && isMuseSparkModel(zenResolvedModel) {
		zenBudget = museSparkDefaultOutputTokens
	}
	if useRespAPI {
		body = translate_registry.TranslateRequest(translate_registry.Chat, translate_registry.Responses, body)
		applyResponsesReasoning(body, reasoningEffort)
		// 出站体形态不变量(P1-9): 违例说明转换层有 bug(如重复转换把 input
		// 转空), 宁可网关 500 也不把畸形请求发给上游换回难以理解的 400。
		if problems := translate_registry.ValidateOutbound(translate_registry.Responses, body); len(problems) > 0 {
			return nil, 0, fmt.Errorf("responses outbound shape invalid: %s", strings.Join(problems, "; "))
		}
	}

	bodyJSON, err := json.Marshal(body)
	if err != nil {
		return nil, 0, fmt.Errorf("marshal zen body: %w", err)
	}

	baseURLs := zenBaseURLList(cfg)

	zenStateMu.Lock()
	sem := zenSem
	zenStateMu.Unlock()
	// 并发准入(P1-12): 等待信号量必须感知请求取消 —— 客户端断开后继续在
	// 队列里干等, 只会占住资源、放大排队延迟。此前是无 ctx 的阻塞式获取。
	rateLimited := 0
	select {
	case sem <- struct{}{}:
	case <-ctx.Done():
		return nil, rateLimited, ctx.Err()
	}
	defer func() { <-sem }()

	retries := cfg.Retries
	if retries <= 0 {
		retries = 3
	}
	// 地区受限模型的失败大多是"节点到上游某条线路不通", 换节点就能成;
	// 池子上百个节点时 3 次尝试命中率太低, 给它更宽的轮换预算。
	if isRegionRestrictedModel(zenModelIDOf(params)) && retries < 6 {
		retries = 6
	}
	// 轮换重试不退避: 节点级失败换一个出口就是全新的机会, 睡眠只会把请求
	// 拖过客户端超时(实测 59~150s, 客户端 30s 就断开了)。退避只保留给
	// 429 —— 那种才需要等限流窗口过去。
	delay := time.Second
	rateLimited = 0
	regionRetried := false // 地区拒绝最多主动换出口重试一次, 避免 hopeless 模型烧光重试
	respTried := false     // Responses 端点自适应回退每次请求只试一次

	for attempt := 0; ; attempt++ {
		// 客户端已断开(超时/取消): 立即停止, 再重试也没有人接收结果。
		if ctx.Err() != nil {
			return nil, rateLimited, fmt.Errorf("zen request aborted: %v", ctx.Err())
		}
		// 端点轮换: 第 N 次尝试用第 N % len(baseURLs) 个端点,
		// 官方地址失败后自然落到 CDN 镜像。
		base := baseURLs[attempt%len(baseURLs)]
		endpoint := base + "/chat/completions"
		if useRespAPI {
			endpoint = base + "/responses"
		}
		req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(bodyJSON))
		if err != nil {
			return nil, rateLimited, fmt.Errorf("create zen request: %w", err)
		}
		// 客户端身份轮换: 每次请求模拟全新 opencode 客户端,规避 session/UA 维度限流
		sess, user, ua := kit.FreshZenIdentity()
		req.Header.Set("Authorization", "Bearer "+cfg.Key)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", ua)
		req.Header.Set("x-opencode-session", sess)
		req.Header.Set("x-opencode-request", user)
		req.Header.Set("x-opencode-client", "cli")

		model, _ := params["model"].(string)
		if m, ok := resolveZenModel(model); ok {
			req.Header.Set("x-opencode-model", m.ID)
		}
		log.Printf("  zen upstream: model=%s stream=%v msgs=%d via=%s endpoint=%s attempt=%d session=%s",
			body["model"], stream, getMsgCount(params), describeZenProxy(), base, attempt+1, kit.Truncate(sess, 24))

		client := getZenHTTPClient()
		// 地区受限模型绝不能吃共享连接池: 池里的 h2 连接是启动早期建立的
		// (model sync 等任务在节点就绪前就发起了第一批请求, 当时走的是直连
		// 的大陆 IP), 此后所有 zen 请求都复用这条连接, DialTLSContext 不会再
		// 被调用 —— 按模型的出口选择被整个绕过, RegionError 403 就是这么来的
		// (其他 zen 模型不限地区所以正常, 只有受限模型暴露)。
		// 每次用全新传输真实拨号, 让 zenDialContext 现场选出口。
		if isRegionRestrictedModel(zenModelIDOf(params)) {
			fresh := *client
			fresh.Transport = zenHTTP2Transport()
			client = &fresh
		}
		resp, err := client.Do(req)
		if err != nil {
			// 客户端断开导致的取消: 直接终止, 不再重试
			if ctx.Err() != nil {
				return nil, rateLimited, fmt.Errorf("zen request aborted: %v", ctx.Err())
			}
			// 网络错误: 冷却本次真实出口(而非全局轮询位置), 立即换出口/端点重试
			cooldownActualExit(ctx, 2*time.Minute)
			if attempt < retries {
				log.Printf("  zen network error (%v), retry %d/%d via next exit (next endpoint: %s)",
					err, attempt+1, retries, baseURLs[(attempt+1)%len(baseURLs)])
				continue
			}
			// 传输层失败(拨号/socks5/握手/超时)说明的是**出口线路**, 不是模型本身。
			// 此前这里记了一次模型硬失败 —— 出口池大面积失效时, 5 次连败就把一个
			// 完全健康的免费模型"暂停使用 30 分钟", 它随即从免费模型列表里消失
			// (2026-09-16 用户实证: opencode 官方客户端里 muse-spark 仍在免费列表,
			//  我们这边没了)。出口的冷却已由 cooldownActualExit 处理, 这里不记。
			log.Printf("  zen: 全部出口尝试失败(model=%s, 最后一次: %v) — 属线路故障, 不计入模型健康门",
				zenModelIDOf(params), err)
			return nil, rateLimited, fmt.Errorf("zen request: %w", err)
		}
		if resp.StatusCode == http.StatusOK {
			markZenSuccess()
			recordZenModelResult(zenModelIDOf(params), false)
			if useRespAPI {
				if stream {
					resp = wrapResponsesStreamToChat(resp, zenResolvedModel, zenBudget)
				} else {
					converted, cerr := convertResponsesResponseToChat(resp, zenResolvedModel, zenBudget)
					if cerr != nil {
						return nil, rateLimited, fmt.Errorf("zen responses convert: %w", cerr)
					}
					resp = converted
				}
			}
			return resp, rateLimited, nil
		}

		bodyBytes := kit.ReadBody(resp)
		resp.Body.Close()
		// 首次遇到地区限制时自动登记该模型, 并触发节点能力探测
		if isRegionError(bodyBytes) {
			modelID := zenModelIDOf(params)
			markModelRegionRestricted(modelID)
			// 把这次真实使用的出口标记为"该模型地区不可用", 选路下一轮就会
			// 避开它, 并换一个出口立即重试 —— 撞地区限制时原地重试只会再 403。
			// 注意 via= 打印的是全局轮询位置, 不代表这条请求的真实出口。
			actual := reqExitKey(ctx)
			if key := nodeLocalKey(actual); key != "" && !regionRetried && attempt < retries {
				setRegionNodeOK(modelID, key, false)
				regionRetried = true
				log.Printf("  zen: model %s region rejected via %s, 已标记该出口并换出口重试",
					modelID, describeExitRaw(actual))
				continue
			}
			log.Printf("  zen: model %s region rejected via %s", modelID, describeExitRaw(actual))
		}

		if isRateLimited(resp.StatusCode, bodyBytes) {
			rateLimited++
			// 冷却本次真实出口代理(429 是等限流窗口, 保留退避睡眠)
			cooldownActualExit(ctx, func() time.Duration {
				d := parseRetryAfter(resp.Header.Get("Retry-After"))
				if d <= 0 {
					d = 10 * time.Minute
				}
				return d
			}())
			if attempt < retries {
				wait := delay
				if retryAfter := parseRetryAfter(resp.Header.Get("Retry-After")); retryAfter > wait {
					wait = retryAfter
				}
				log.Printf("  zen rate limited (%d), retry %d/%d after %v (next endpoint: %s)",
					resp.StatusCode, attempt+1, retries, wait, baseURLs[(attempt+1)%len(baseURLs)])
				time.Sleep(kit.WithRetryJitter(wait))
				delay *= 2
				continue
			}
			markZenFail()
			return nil, rateLimited, &zenUpstreamError{Status: resp.StatusCode, Body: kit.Truncate(bodyBytes, 500)}
		}

		// 5xx: 先给 Responses 端点一次自适应回退的机会(上游把"该模型只在
		// /responses 提供"崩成 500), 再冷却本次真实出口换下一个重试, 不退避
		// —— 上游 500 常与出口线路相关, 换一个出口就是全新的机会。
		if resp.StatusCode >= http.StatusInternalServerError {
			if !useRespAPI && !respTried {
				respTried = true
				altBody := translate_registry.TranslateRequest(translate_registry.Chat, translate_registry.Responses, body)
				applyResponsesReasoning(altBody, reasoningEffort)
				if problems := translate_registry.ValidateOutbound(translate_registry.Responses, altBody); len(problems) == 0 {
					if alt := tryZenResponsesFallback(ctx, base, altBody, stream, client, zenBudget); alt != nil {
						markZenSuccess()
						recordZenModelResult(zenModelIDOf(params), false)
						return alt, rateLimited, nil
					}
				}
			}
			if attempt < retries {
				cooldownActualExit(ctx, 2*time.Minute)
				log.Printf("  zen upstream %d via %s, retry %d/%d via next exit (next endpoint: %s)",
					resp.StatusCode, describeExitRaw(reqExitKey(ctx)), attempt+1, retries,
					baseURLs[(attempt+1)%len(baseURLs)])
				continue
			}
		}

		markZenFailOnStatus(resp.StatusCode)
		if resp.StatusCode >= http.StatusInternalServerError {
			recordZenModelResult(zenModelIDOf(params), true)
		}
		return nil, rateLimited, &zenUpstreamError{Status: resp.StatusCode, Body: kit.Truncate(bodyBytes, 500)}
	}
}

// zenUpstreamError 携带上游 HTTP 状态码与响应体,
// 上层据此把 4xx(如 RegionError 地域限制)按原状态返回, 而非统一 502。
type zenUpstreamError struct {
	Status int
	Body   string
}

func (e *zenUpstreamError) Error() string {
	return fmt.Sprintf("zen API %d: %s", e.Status, e.Body)
}

// zenErrorStatus 上游 4xx 按原状态返回(如 403 RegionError 地域限制),
// 网络错误与上游 5xx 统一 502。
// 判定逻辑已泛化到 upstreamErrorStatus(候选链上的每一站共用同一套规则),
// 这里保留原名以免改动全部既有调用点。
func zenErrorStatus(err error) int {
	return upstreamErrorStatus(err)
}

// markZenFailOnStatus 仅上游级故障计入熔断: 5xx/408/429 代表上游不可用;
// 400/401/404 等客户端类错误是模型或请求本身的问题, 不应触发全局故障转移。
func markZenFailOnStatus(status int) {
	if status >= http.StatusInternalServerError || status == http.StatusRequestTimeout || status == http.StatusTooManyRequests {
		markZenFail()
	}
}

func describeZenProxy() string {
	list := effectiveProxyList()
	if len(list) == 0 {
		return "direct"
	}
	idx := lastZenProxyIdx()
	if idx < 0 {
		idx = 0
	}
	idx %= len(list)
	p := list[idx]
	if isNodeLink(p) {
		return fmt.Sprintf("node[%d]=%s", idx+1, nodeDisplayName(p))
	}
	return fmt.Sprintf("proxy[%d]=%s", idx+1, kit.Truncate(maskProxyURL(p), 60))
}

// describeExitRaw 展示某条请求真实使用的出口。
// 与 describeZenProxy 不同: 那是全局轮询位置, 会把别的请求选的节点安在这条
// 请求头上, 排查"明明走了节点为什么还 403"时极具误导性。
func describeExitRaw(p string) string {
	switch {
	case p == "":
		return "直连"
	case isNodeLink(p):
		return "节点: " + nodeDisplayName(p)
	default:
		return "代理: " + kit.Truncate(maskProxyURL(p), 60)
	}
}
