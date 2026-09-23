package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"free-router/internal/app/translate_registry"
	"free-router/internal/kit"
	"golang.org/x/net/http2"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// zenRetryAfterMaxWait 429 退避睡眠的上限。
//
// 上游 Retry-After 头对纯数字秒数没有上限(proxy_util parseRetryAfter,
// "999999999"≈31 年), 无上限 time.Sleep 会全程占着并发信号量(默认仅 8 槽)
// 且循环顶的 ctx 检查打不断它。超过上限就不再占槽干等, 而是**冷却该出口后
// 立即换出口重试**(见 429 分支)。
const zenRetryAfterMaxWait = 60 * time.Second

// zenQuotaRotateBudget 429 换出口的独立预算。
//
// 429 的语义是"**这个出口**的额度没了"(opencode 免费层按出口 IP 计), 不是
// "这个请求有问题" —— 换一个出口就是全新的机会, 所以它值得比通用重试
// (retries, 默认 3)更宽的预算。
//
// 为什么不照搬参考实现(opencode2api)的"上限 = 出口池大小": 它的池是用户手填的
// 几十个代理, 所以那个上限合理; 我们的出口池是订阅聚合出来的(实测 1327 个出口、
// 其中可达 136 个), 而每次 429 轮换都要真发一次请求 —— 上百次必然拖爆客户端的
// 30s 超时(实测客户端 59~150s 就断)。所以取一个有界预算, 而不是池大小。
const zenQuotaRotateBudget = 8

// zenAttemptHeaderTimeout 单次 attempt 等待响应头的上限 —— 对齐主 transport
// 的 ResponseHeaderTimeout(120s, proxy_pool.go)。https 被 RegisterProtocol
// 整体旁路到裸 http2.Transport, 主 transport 的该字段对 zen 流量不生效:
// 上游 accept 后不回响应头会把请求连同并发信号量槽(默认 8)一起挂死。
const zenAttemptHeaderTimeout = 120 * time.Second

// zenAttemptBody 把 attempt 级回收绑到响应体关闭上, 流读取期间不能动:
//   - cancel: 响应头超时的 cancel; 拿到响应头后已停表, body 关闭即归还资源;
//   - h2: attempt 级专用 transport 的侧路引用。**当前恒为 nil** —— 出口
//     transport 现在是按出口长期缓存复用的(zenClientForExit), 它的空闲连接
//     由 IdleConnTimeout(90s) 自行回收, 不能在响应体关闭时关掉, 否则就把
//     同出口复用的收益抹掉了。字段保留给"确实需要 attempt 级回收"的场景。
type zenAttemptBody struct {
	io.ReadCloser
	cancel context.CancelFunc
	h2     *http2.Transport
}

func (b *zenAttemptBody) Close() error {
	err := b.ReadCloser.Close()
	if b.cancel != nil {
		b.cancel()
	}
	if b.h2 != nil {
		b.h2.CloseIdleConnections()
	}
	return err
}

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

	// 对齐 OmniRoute 的 OpencodeExecutor.transformRequest 末尾两步:
	//
	//  1. opencode-go 系后端(含 opencode-zen)的 ChatCompletionRequest.reasoning
	//     是结构化类型, 收下布尔值会 400 "cannot unmarshal bool into Go struct
	//     field", 转发前剥掉布尔形态(对象/字符串形态合法, 保持原样);
	//  2. thinking 家族(DeepSeek / Kimi / K2 / MiniMax / MiMo)要求历史里每条
	//     assistant 消息回传非空 reasoning_content, 否则 400 "reasoning_content
	//     must be passed back"; OpenAI 协议客户端跨回合不保留该字段, 这里补占位符。
	//
	// 模型 ID 用已解析的 zen ID(body["model"]), 未登记模型时退回请求里的原始值。
	reasoningModelID, _ := body["model"].(string)
	if reasoningModelID == "" {
		reasoningModelID, _ = params["model"].(string)
	}
	body = stripBooleanReasoning(body)
	if isThinkingMessageModel(reasoningModelID) {
		if injected, ok := injectReasoningContentForThinkingModel(body).(map[string]any); ok {
			body = injected
		}
	}
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
	// 端点选择(2026-09-17 补齐): 官方文档给出 **4 种**端点, 不同模型走不同形态。
	// 此前只有 chat / responses 两个分支, 缺 /messages —— 于是 union-alpha
	// (免费, 只在 /messages)会被发到 /chat/completions 必然失败。
	// 判据与端点矩阵见 zen_endpoints.go 与 docs/opencode-zen-facts.md。
	zenResolvedModel, _ := body["model"].(string)
	zenEndpoint := zenEndpointFor(zenResolvedModel)
	// 直连 zen 路径的决策轨迹。此前只有候选链(handleChainedChat)会建轨迹, 于是所有
	// 非链式 zen 请求在面板详情里同时出现两个假象: "决策轨迹不可用 HTTP 404"(轨迹
	// 从未登记)与"0 次尝试, 跳过 0 站"(计数从未写入)—— 两条假象同源于此: 轨迹从未创建。
	//
	// ★ 创建点必须留在所有 return 之前: 它曾位于信号量之后, 于是 4 条提前 return
	// (消息端点转换失败 / 出站形态校验失败 / marshal 失败 / 排队中客户端取消)
	// 从未登记轨迹, 面板对这些请求回 404。上移到模型解析之后, 全部 18 条 return
	// 路径都被覆盖 —— 修法是"创建覆盖所有 return", 而非在各 return 前逐个补记。
	//
	// owns 是记账开关, 不是判重: callZenAPI 同时被 callChainUpstream 调用(routing_chain.go),
	// 而链式调度自己已经做了完整的逐候选记账(handleChainedChat: tr.AddAttempt + dec.addCandidate)。
	// 此处若再来一遍, 每次上游调用会把尝试数与候选数**翻倍**。因此只有"本次新建"轨迹时
	// (直连路径)才记账; 已存在(链式已登记)一律跳过。
	trace := traceFrom(ctx)
	dec := decisionTraceFrom(reqIDFrom(ctx))
	owns := false
	if dec == nil {
		dec = decisionTraceStart(reqIDFrom(ctx), zenResolvedModel)
		owns = true
	}
	// 终局收口: 面板的 finish 状态依赖它。finish 幂等且持锁, 与链式入口的 defer 并存也安全。
	if owns {
		defer dec.finish()
	}
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
	switch zenEndpoint {
	case zenEndpointResponses:
		body = translate_registry.TranslateRequest(translate_registry.Chat, translate_registry.Responses, body)
		applyResponsesReasoning(body, reasoningEffort)
	case zenEndpointMessages:
		// Anthropic Messages 形态: 用 internal/translate 的 openai→claude 方向。
		// 失败即返回, 不把畸形请求发给上游。
		converted, cerr := translateZenMessagesRequest(zenResolvedModel, body, stream)
		if cerr != nil {
			// 请求没发出去 → 半开探测无判定, 释放探测标志(见 clearZenProbing)。
			clearZenProbing()
			return nil, 0, cerr
		}
		body = converted
	}

	// 免费层形态整形: chat/responses/messages 三个端点都要 agent 工具 + stream,
	// 缺一个即 403 FreeTierError。放在协议翻译之后, 因为翻译会丢失 chat
	// 形状的 tools(OpenAIChatToClaudeRequest 只保留 {function:{...}} 嵌套)。
	forcedStream := false
	if isFreeModelForShape := zenFreeShapeRequired(zenEndpoint, zenResolvedModel); isFreeModelForShape {
		body, forcedStream = zenApplyFreeShape(zenEndpoint, body)
	}
	switch zenEndpoint {
	case zenEndpointResponses:
		// zen 的 /responses 只接受 tool_choice:"auto"(见 zenCoerceResponsesToolChoice),
		// 降级必须在**转换后的形态**上做 —— 降级掉的是 tool_choice,不是 tools。
		zenCoerceResponsesToolChoice(body, zenResolvedModel)
		// 出站体形态不变量(P1-9): 违例说明转换层或整形层有 bug,
		// 宁可网关 500 也不把畸形请求发给上游换回难以理解的 400。
		if problems := translate_registry.ValidateOutbound(translate_registry.Responses, body); len(problems) > 0 {
			clearZenProbing()
			return nil, 0, fmt.Errorf("responses outbound shape invalid: %s", strings.Join(problems, "; "))
		}
	}

	bodyJSON, err := json.Marshal(body)
	if err != nil {
		clearZenProbing()
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
		// 排队中客户端已断开 → 半开探测无判定, 释放探测标志。
		clearZenProbing()
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
	// 429 换出口用**独立预算**(见 zenQuotaRotateBudget): 429 是对"出口"的判定,
	// 不是对"请求"的判定 —— 换一个出口就是全新的机会, 值得比通用重试更宽的预算。
	// 循环里只有 429 分支用得上这个上限, 其余分支仍按 retries 收口。
	maxAttempts := retries
	if maxAttempts < zenQuotaRotateBudget {
		maxAttempts = zenQuotaRotateBudget
	}
	// 轮换重试不退避: 节点级失败换一个出口就是全新的机会, 睡眠只会把请求
	// 拖过客户端超时(实测 59~150s, 客户端 30s 就断开了)。退避只保留给
	// 429 —— 那种才需要等限流窗口过去。
	delay := time.Second
	rateLimited = 0
	respTried := false // Responses 端点自适应回退每次请求只试一次
	msgTried := false  // Messages 端点自适应回退每次请求只试一次

	for attempt := 0; ; attempt++ {
		// 直连路径: 每次尝试记一条候选 + 一次真实尝试。请求日志的"尝试/跳过"与
		// 面板的候选链同源(reqTrace), 此前直连路径两处都为 0, 面板无从判断重试是否发生。
		// 链式路径不在此记账 —— handleChainedChat 已经自己记过(tr.AddAttempt + dec.addCandidate),
		// 再来一遍会把尝试数与候选数翻倍。
		if owns {
			trace.AddAttempt()
			dec.addCandidate(decZenCandidate(zenResolvedModel), "tried", "", 0, "")
		}
		// 客户端已断开(超时/取消): 立即停止, 再重试也没有人接收结果。
		// 请求未获判定 → 释放半开探测标志(见 clearZenProbing)。
		if ctx.Err() != nil {
			clearZenProbing()
			return nil, rateLimited, fmt.Errorf("zen request aborted: %v", ctx.Err())
		}
		// 端点轮换: 第 N 次尝试用第 N % len(baseURLs) 个端点,
		// 官方地址失败后自然落到 CDN 镜像。
		base := baseURLs[attempt%len(baseURLs)]
		// 相对路径由端点形态决定。★ base 已含 /v1, 所以 /messages 端点的相对
		// 路径是 "/messages" 而不是 "/v1/messages"(后者拼出 .../zen/v1/v1/messages)。
		endpoint := base + zenEndpoint.pathFor(zenResolvedModel)
		// attempt 级响应头超时: 只管到响应头(见 zenAttemptHeaderTimeout),
		// 拿到 resp 即停表, 流式响应体沿用既往的空闲看门狗不额外设限。
		attemptCtx, cancelAttempt := context.WithCancel(ctx)
		headerTimer := time.AfterFunc(zenAttemptHeaderTimeout, cancelAttempt)
		req, err := http.NewRequestWithContext(attemptCtx, "POST", endpoint, bytes.NewReader(bodyJSON))
		if err != nil {
			cancelAttempt()
			clearZenProbing()
			return nil, rateLimited, fmt.Errorf("create zen request: %w", err)
		}
		// 官方 opencode CLI 身份形态(见 opencode_headers.go):
		// UA=opencode/<版本>、client=cli、project/session/request 为 ses_/usr_/prj_
		// 结构 ID。门禁按此判 "from within OpenCode", 形态不对即 403 FreeTierError。
		outbound := map[string]string{}
		applyOpencodeHeaders(outbound, nil, defaultOpencodeIdentity(), bodyFingerprint(body))
		// 凭据选择: 匿名模式开 && 免费模型 → 统一 "public"(opencode2api 同款
		// 匿名档, 探针见 zen_keys.go); 否则多 key 按出口**确定性**选一把
		// (同一出口永远用同一把 key, 见 zen_keys.go), 退役中的 key 自动跳过。
		reqKey := zenSelectKeyForModel(cfg, reqExitKey(ctx), zenResolvedModel)
		// 鉴权按端点形态分流: Anthropic Messages 用 x-api-key + 版本头, 其余用
		// Bearer。漏掉这步会拿到 401, 而 401 很容易被误判成"key 不对"。
		if zenEndpoint.usesAnthropicAuth() {
			req.Header.Set("x-api-key", reqKey)
			req.Header.Set("anthropic-version", "2023-06-01")
		} else {
			req.Header.Set("Authorization", "Bearer "+reqKey)
		}
		req.Header.Set("Content-Type", "application/json")
		for k, v := range outbound {
			req.Header.Set(k, v)
		}

		model, _ := params["model"].(string)
		if m, ok := resolveZenModel(model); ok {
			req.Header.Set("x-opencode-model", m.ID)
		}
		log.Printf("  zen upstream: model=%s stream=%v msgs=%d via=%s endpoint=%s attempt=%d session=%s",
			body["model"], stream, getMsgCount(params), describeZenProxy(), base, attempt+1, kit.Truncate(outbound["x-opencode-session"], 24))

		// ★ 出口在这里**先定**, 再取它专属的 client —— 出口是"编译进 transport"
		// 的(zenClientForExit / newZenTransportPinned), 不再由拨号层现场决定。
		//
		// 为什么必须"先定出口再取 client": 共享 transport 的连接按**上游 host**
		// 复用, 复用直接绕过出口选择 —— 一条建立在出口 X 上的长连接会被后续请求
		// 继续用, 哪怕 X 已被判定额度耗尽(上游按出口 IP 计额度, "一个 IP 用完就
		// 429、换一个 IP 就能继续"正是本网关的立足点); 而且复用不触发拨号,
		// setReqExit 不写, 429 时连"该冷却哪个出口"都不知道 —— 2026-09-23 实证:
		// 95 次 429 里只有 4 次冷却到了出口, 同一个节点被连打 32 次、37 次。
		//
		// 每个出口一份 client 之后, 连接池天然按出口隔离: 复用永远发生在同出口
		// 内部, 跨出口复用从结构上不可能(与 opencode2api 的 proxyTransport 同构)。
		// 粘性会话开启时同一来源反复走同一出口, 这条复用是稳态收益。
		//
		// exit 为空(直连模式 / 池里无可用节点)时 zenClientForExit 回落到共享
		// client —— 那条路径自带多候选重试与 catch-all / Go 原生直连兜底链。
		exit, _ := pickUnifiedExit(ctx, zenModelIDOf(params))
		// ★ 出口已知, 就在这里写回 ctx —— **不能只靠拨号层写**: 出口 client 是
		// 复用的, 连接被复用时拨号根本不会发生, setReqExit 就不会被调用, reqExit
		// 留空, 429 时 cooldownActualExitQuota 直接早退返回 0 —— 冷却表写不进去,
		// "同一个 IP 打到底"的 bug 就原样回来了。拨号层那份写入保留(同值), 两条
		// 路径合起来才覆盖"新建连接"与"复用连接"两种情况。
		setReqExit(ctx, exit)
		client := zenClientForExit(exit)
		resp, err := client.Do(req)
		// 响应头已到(或本次拨号已终局), 停掉响应头超时计时任务。
		headerTimer.Stop()
		if err != nil {
			// attempt 终局: 归还 cancel。成功路径的归还挂在 resp.Body 关闭上
			// (zenAttemptBody), 流读取期间不能触发。
			cancelAttempt()
			// 客户端断开导致的取消: 直接终止, 不再重试。探测无判定, 释放标志。
			if ctx.Err() != nil {
				clearZenProbing()
				return nil, rateLimited, fmt.Errorf("zen request aborted: %v", ctx.Err())
			}
			// 网络错误: 冷却本次真实出口(而非全局轮询位置), 立即换出口/端点重试
			cooldownActualExit(ctx, 2*time.Minute)
			if attempt < retries {
				log.Printf("  zen network error (%v), retry %d/%d via next exit (next endpoint: %s)",
					err, attempt+1, retries, baseURLs[(attempt+1)%len(baseURLs)])
				continue
			}
			// 传输层失败(拨号/socks5/握手/超时/响应头超时)说明的是**出口线路**, 不是模型本身。
			// 此前这里记了一次模型硬失败 —— 出口池大面积失效时, 5 次连败就把一个
			// 完全健康的免费模型"暂停使用 30 分钟", 它随即从免费模型列表里消失
			// (2026-09-16 用户实证: opencode 官方客户端里 muse-spark 仍在免费列表,
			//  我们这边没了)。出口的冷却已由 cooldownActualExit 处理, **模型健康门**
			// (recordZenModelResult)依旧不计 —— 但**熔断/failover** 计数要进
			// (markZenFail): DNS 劫持、防火墙封上游这类纯网络故障下若 zenFailCount
			// 永不增长, Failover=true 也永远切不到健康的 cline 池, 客户端全部吃
			// 502(终审 P1)。markZenFail 同时收尾半开探测: 探测走网络错误路径
			// 不会再永久卡住 zenProbing。
			log.Printf("  zen: 全部出口尝试失败(model=%s, 最后一次: %v) — 属线路故障, 不计入模型健康门, 计入熔断",
				zenModelIDOf(params), err)
			markZenFail()
			return nil, rateLimited, fmt.Errorf("zen request: %w", err)
		}
		// 回收钩子(zenAttemptBody): attempt 级的 cancel 绑到响应体关闭上 ——
		// body 关闭(流结束、连接归还)后立即归还资源。**不再挂 h2 回收**:
		// 出口 transport 现在按出口长期缓存复用, 在响应体关闭时关它的空闲连接
		// 等于把同出口复用的收益抹掉; 那些连接由 IdleConnTimeout(90s) 自行回收。
		resp.Body = &zenAttemptBody{ReadCloser: resp.Body, cancel: cancelAttempt}
		if resp.StatusCode == http.StatusOK {
			markZenSuccess()
			// 重试成功时点名**真实出口**: 上面的 via= 打印的是全局轮询位置,
			// 会把别的请求选的节点安在这条请求头上 —— 排查"429 之后到底换没换
			// 出口"时它帮倒忙(2026-09-23 实证: 日志里 via= 两次都是同一个节点,
			// 而真实出口其实换了)。只在重试后打, 稳态不添噪。
			if attempt > 0 {
				log.Printf("  zen: 第 %d 次尝试成功, 本次真实出口 %s", attempt+1, describeExitRaw(reqExitKey(ctx)))
			}
			// 健康门的"成功"记录挪到下方转换与空回合判定全部通过之后:
			// 200 就清零 consecFails 的话, 随后转换发现空回合 return 错误,
			// 健康门却已被清零 —— zenModelUnavailable 永不触发, 模型每次空
			// 回合都白走全链路(P2-17, 口径见 zen_model_health.go)。
			// 这个出口调通了 → 清零它的连续 429 计数, 让冷却时长回到基准。
			// 不清零的话计数单调递增, 出口被限流一次后就再也回不到短冷却。
			clearActualExitQuotaStrike(ctx)
			if owns {
				dec.setWinner(decZenCandidate(zenResolvedModel))
			}
			// 地区能力**正向**学习: 这个出口对该模型可用。
			//
			// ★ 2026-09-17 审查 P1-1: 此前只有负向回写(失败时 setRegionNodeOK(false)),
			//   正向知识的唯一来源是 probeModelAllNodes —— 那要对**全部**节点发真实
			//   请求、烧免费额度。于是"哪些出口对该模型可用"这个信息, 代价是上百个请求。
			//
			//   用真实请求当探测器是零开销的: 每一次成功都在回答"这个出口对这个模型
			//   可用"。补上这一步之后, 全节点探测退化为罕见补充而非常规手段。
			//
			//   只对地区受限模型记 —— 其余模型选路时不查这张表(见 exitFilterForModel)。
			if modelID := zenModelIDOf(params); isRegionRestrictedModel(modelID) {
				if key := nodeLocalKey(reqExitKey(ctx)); key != "" {
					setRegionNodeOK(modelID, key, true)
				}
			}
			switch zenEndpoint {
			case zenEndpointResponses:
				if stream || forcedStream {
					resp = wrapResponsesStreamToChat(resp, zenResolvedModel, zenBudget)
				} else {
					converted, cerr := convertResponsesResponseToChat(resp, zenResolvedModel, zenBudget)
					if cerr != nil {
						// 空回合类转换失败按硬失败计数(zen_model_health.go:
						// 5xx/超时/网络/空响应), 否则健康门对空回合模型永不触发。
						recordZenModelResult(zenModelIDOf(params), true)
						return nil, rateLimited, fmt.Errorf("zen responses convert: %w", cerr)
					}
					resp = converted
				}
			case zenEndpointMessages:
				// Anthropic 响应 / SSE → chat 形态。流式走 io.Pipe 实时转换, 因此
				// 下游的空流守卫、坏帧清洗、心跳全部照常生效, 无需单独实现一套。
				if stream || forcedStream {
					resp = wrapClaudeStreamToChat(resp, zenResolvedModel)
				} else {
					converted, cerr := convertClaudeResponseToChat(resp, zenResolvedModel)
					if cerr != nil {
						// 同上: 空回合类转换失败计一次硬失败(P2-17)。
						recordZenModelResult(zenModelIDOf(params), true)
						return nil, rateLimited, fmt.Errorf("zen messages convert: %w", cerr)
					}
					resp = converted
				}
				// 实测调通 → 持久化登记, 之后直接走 /messages(负向结论不落盘)。
				zenLearnMessagesOnly(zenResolvedModel)
			}
			// 免费层强制了 stream, 但客户端原始请求是非流式 —— 上游返回 chat SSE,
			// 需要汇总成 JSON 再交回, 保持与非免费层调用形态一致。
			if forcedStream && !stream {
				collapsed, cerr := zenCollapseFreeStreamToJSON(resp)
				if cerr != nil {
					recordZenModelResult(zenModelIDOf(params), true)
					return nil, rateLimited, fmt.Errorf("zen free-tier collapse: %w", cerr)
				}
				resp = collapsed
			}
			// 端点转换与空回合判定全部通过 —— 到这里才是真的"模型这一轮可用"。
			recordZenModelResult(zenModelIDOf(params), false)
			return resp, rateLimited, nil
		}
		bodyBytes := kit.ReadBody(resp)
		resp.Body.Close()
		// 本次尝试的真实结局: 挪到所有分支(401/403/429/5xx/4xx)之前 —— 这些分支
		// 都会 return 或 continue, 放在末尾会让"重试后 return"的路径跳过标记。
		// 尤其 429: 上游限流时若漏标, 面板会把一次限流画成成功(状态 0 判 2xx)。
		if owns {
			dec.markCandidateResult(resp.StatusCode, errClassForStatus(resp.StatusCode))
		}
		// 401: 这把 key 失效(被吊销 / 冻结) → 临时退役, 受影响出口自动落到下一把
		// key(zenSelectKey 会跳过退役的)。这是多 key 的**确定收益**: 一把 key 坏掉
		// 不影响整体服务。退役状态不落盘 —— 上游随时可能恢复。
		if resp.StatusCode == http.StatusUnauthorized {
			zenRetireKey(reqKey)
			log.Printf("  zen: key %s 认证失败(401), 临时退役 %v 后重试", maskZenKey(reqKey), zenKeyRetireDuration)
			// 换 key 重试: 下一轮 reqKey 重新走 zenSelectKey(会跳过刚退役的
			// 这把), 受影响出口自动落到下一把 key。预算耗尽才把 401 原样交还
			// 客户端(P3-19 —— 此前日志说"重试"却不 continue, 401 直接回给客户端)。
			if attempt < retries {
				continue
			}
		}
		// FreeTierError("can only be used from within OpenCode"): opencode 免费
		// tier 的风控按**出口 IP** 判定 —— 实测(2026-09-17)同一 mimo-v2.5-free
		// 走香港/大陆中转节点 200、走美/法节点与本机直连一律 403, 且与请求头无关
		// (带参考实现的完整 CLI 身份头同样被拒)。这是**出口级**失败, 处理与
		// RegionError 完全同构: 标记 (模型,出口) 组合不可用 + 登记地区受限触发
		// 全节点能力探测(自动找出被认可的节点), 并换出口立即重试。绝不冷却模型
		// 或记模型硬失败 —— 否则好节点轮回来之前模型就被误杀了。
		if isFreeTierError(bodyBytes) {
			modelID := zenModelIDOf(params)
			markModelRegionRestricted(modelID)
			actual := reqExitKey(ctx)
			// 出口级失败: 每次命中都在预算内标记并换出口, 直到试出可用出口或预算耗尽。
			// 此前只换一次 —— 池子几千个出口、可用的屈指可数时, 一次重试几乎不可能命中。
			if key := nodeLocalKey(actual); key != "" && attempt < retries {
				setRegionNodeOK(modelID, key, false)
				log.Printf("  zen: model %s free-tier rejected via %s, 已标记该出口并换出口重试",
					modelID, describeExitRaw(actual))
				continue
			}
			log.Printf("  zen: model %s free-tier rejected via %s (重试预算已用完, 等待节点能力探测找出可用出口)",
				modelID, describeExitRaw(actual))
			// 上游已回包 → 半开探测已有可达性结论, 释放探测标志(不计熔断)。
			clearZenProbing()
			return nil, rateLimited, &zenUpstreamError{Status: resp.StatusCode, Body: kit.Truncate(bodyBytes, 500)}
		}
		// 首次遇到地区限制时自动登记该模型, 并触发节点能力探测
		if isRegionError(bodyBytes) {
			modelID := zenModelIDOf(params)
			markModelRegionRestricted(modelID)
			// 把这次真实使用的出口标记为"该模型地区不可用", 选路下一轮就会
			// 避开它, 并换一个出口立即重试 —— 撞地区限制时原地重试只会再 403。
			// 注意 via= 打印的是全局轮询位置, 不代表这条请求的真实出口。
			actual := reqExitKey(ctx)
			if key := nodeLocalKey(actual); key != "" && attempt < retries {
				setRegionNodeOK(modelID, key, false)
				log.Printf("  zen: model %s region rejected via %s, 已标记该出口并换出口重试",
					modelID, describeExitRaw(actual))
				continue
			}
			log.Printf("  zen: model %s region rejected via %s", modelID, describeExitRaw(actual))
		}

		if isRateLimited(resp.StatusCode, bodyBytes) {
			rateLimited++
			// 429: 冷却本次真实出口。
			//
			// ★ 冷却对象是**出口**而不是候选(模型): opencode 的免费额度按出口 IP
			//   计 —— 一个 IP 用完就 429, 换一个 IP 就能继续(用户实测)。所以这里
			//   绝不能标记模型失败, 那会把一个好模型在好节点轮回来之前就误杀。
			//
			// ★ 时长按连续命中次数指数升级(cooldownZenProxyQuota): 额度重置窗口
			//   未知(标 [未知]), 固定时长要么太短(白烧尝试)要么太长(出口闲置)。
			//   上游给了 Retry-After 则取较大者。
			retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"))
			cooled := cooldownActualExitQuota(ctx, retryAfter)
			if cooled > 0 {
				log.Printf("  zen: 出口 %s 额度冷却 %v(429, 连续命中升级), 换出口重试",
					describeExitRaw(reqExitKey(ctx)), cooled.Round(time.Second))
			} else {
				// reqExit 为空 = 本次没有真实出口可冷却: 要么走的是直连/catch-all
				// 兜底(本就没出口), 要么拨号压根没发生。后者过去是"同一个 IP
				// 打到底"的成因, 必须点名而不是静默 —— 静默的话面板上只能看到
				// 一连串 429, 看不出冷却表根本没写进去。
				log.Printf("  zen: 429 但本次未记录到真实出口(reqExit 为空) — 无出口可冷却/轮换(直连或兜底路径)")
			}
			wait := delay
			if retryAfter > wait {
				wait = retryAfter
			}
			if wait <= zenRetryAfterMaxWait && attempt < retries {
				log.Printf("  zen rate limited (%d), retry %d/%d after %v (next endpoint: %s)",
					resp.StatusCode, attempt+1, retries, wait, baseURLs[(attempt+1)%len(baseURLs)])
				// 睡眠必须可被客户端取消: ctx 已死还继续占槽睡, 只会拖住
				// 整个并发窗口(信号量容量默认仅 8)。取消时立即返回 ——
				// 半开探测已收到 429 但终局未到, 客户端取消视为无判定。
				select {
				case <-time.After(kit.WithRetryJitter(wait)):
				case <-ctx.Done():
					clearZenProbing()
					return nil, rateLimited, fmt.Errorf("zen request aborted: %v", ctx.Err())
				}
				delay *= 2
				continue
			}
			// 走到这里 = Retry-After 远超上限(或短等待但通用预算已用尽)。
			//
			// Retry-After 的语义是"**这个出口**多久之后恢复", 不是"整个网关要睡
			// 多久" —— opencode 免费层按出口 IP 计额度, Retry-After 实测 ≈ 到
			// 次日 08:00 重置(15h+), 永远超过 1 分钟上限。
			//
			// 所以两件事必须分开做: **不占槽睡眠**(睡 15h 等于把并发窗口锁死,
			// P1-4), 但**立刻换一个出口重试** —— 出口已经写进冷却表, 下一次尝试
			// 必然选到别的出口, 这才是"冷却"的意义。
			//
			// 此前这里直接 return 429, 于是"冷却了出口却不换出口": 客户端收到 429
			// 只好自己重发, 重发又落到同一个已耗尽的出口上 —— 同一个 IP 一路 429
			// 到底(2026-09-23 实证: 172 号连吃 32 次、175 号连吃 37 次, 95 次 429
			// 里只有 4 次真的换到了出口)。
			if attempt+1 < maxAttempts && cooled > 0 {
				log.Printf("  zen rate limited (%d), 该出口已冷却 — 不占槽等待(Retry-After %v), 立即换出口重试 %d/%d (next endpoint: %s)",
					resp.StatusCode, wait, attempt+1, maxAttempts, baseURLs[(attempt+1)%len(baseURLs)])
				continue
			}
			if cooled > 0 {
				log.Printf("  zen rate limited (%d): 换出口预算 %d 次已用尽, 返回 429", resp.StatusCode, maxAttempts)
			} else {
				// 没有真实出口可轮换(直连/兜底): "换出口"是空操作, 交还 429
				// 让客户端按自己的节奏重试(P1-4)。
				log.Printf("  zen rate limited (%d), Retry-After %v 超过 %v 上限 — 且无真实出口可轮换, 直接返回 429 让客户端自重试",
					resp.StatusCode, wait, zenRetryAfterMaxWait)
			}
			markZenFail()
			return nil, rateLimited, &zenUpstreamError{Status: resp.StatusCode, Body: kit.Truncate(bodyBytes, 500)}
		}

		// 5xx: 先给其他端点各一次自适应回退的机会(上游把"该模型只在某端点提供"
		// 崩成 500, 而不是翻译成 4xx), 再冷却本次真实出口换下一个重试, 不退避
		// —— 上游 500 常与出口线路相关, 换一个出口就是全新的机会。
		if resp.StatusCode >= http.StatusInternalServerError {
			// 端点回退: 首选 chat 时依次试 responses → messages。
			//
			// 为什么只从 chat 出发: 静态表与学习机制都已把模型定向到正确端点,
			// 能走到这里说明"我们不知道这个模型该走哪个端点" —— 也就是首选必然是
			// chat。反向(从 responses/messages 回退到 chat)只会重复同一个失败,
			// 且 body 已转成目标形态, 反向转换没有依据。
			if zenEndpoint == zenEndpointChat {
				if !respTried {
					respTried = true
					altBody := translate_registry.TranslateRequest(translate_registry.Chat, translate_registry.Responses, body)
					applyResponsesReasoning(altBody, reasoningEffort)
					if problems := translate_registry.ValidateOutbound(translate_registry.Responses, altBody); len(problems) == 0 {
						if alt := tryZenResponsesFallback(ctx, base, altBody, stream, client, reqKey, zenBudget); alt != nil {
							markZenSuccess()
							// 回退响应同样挂在本次 attempt 的 cancel 上: 不挂的话
							// attemptCtx 要等父 ctx 结束才释放(此前只在 freshH2
							// 非空时才挂, 属于漏挂)。
							alt.Body = &zenAttemptBody{ReadCloser: alt.Body, cancel: cancelAttempt}
							recordZenModelResult(zenModelIDOf(params), false)
							return alt, rateLimited, nil
						}
					}
				}
				if !msgTried {
					msgTried = true
					if alt := tryZenMessagesFallback(ctx, base, body, stream, client, reqKey); alt != nil {
						markZenSuccess()
						alt.Body = &zenAttemptBody{ReadCloser: alt.Body, cancel: cancelAttempt}
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
	} else {
		// 客户端类错误不计熔断, 但"上游能回包"已是半开探测的可达性结论 ——
		// 清掉探测在途标志, 免得一次性 CAS 放行后 zenProbing 永久卡住
		// (含 401/地区受限 403 耗尽预算后落到这里的终局)。
		clearZenProbing()
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

// decZenCandidate 决策轨迹里直连 zen 站点的候选标签。
// zen 是单一上游, 无模型轮换, 标签只含模型名, 便于面板区分上游。
func decZenCandidate(resolvedModel string) string {
	return "zen/" + resolvedModel
}
