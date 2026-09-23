package app

import (
	"context"
	"encoding/json"
	"fmt"
	"free-router/internal/kit"
	"free-router/internal/providers"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

var defaultModel = "deepseek/deepseek-v4-flash"

var proxyListenAddress = "127.0.0.1:3457"

const (
	defaultMaxTokens       = 128000
	defaultReasoningEffort = "high"
)

// buildVersion 由构建时注入: `-ldflags "-X free-router/internal/app.buildVersion=$(git describe)"`。
// 未注入时回退到本硬编码默认值, 这样手工 `go build`(不带 -X)也不会得到空串。
var buildVersion = "go-1.1"

// 进程级生命周期:
//   - appRootCtx 由 StartProxy 通过 signal.NotifyContext 建立, 所有后台循环都应
//     select 在 appRootCtx.Done() 上以便优雅退出(见各后台循环的收口点清单)。
//   - appServer 保存 *http.Server 引用, 供优雅关闭时调用 Shutdown。
//   - shutdownOnce 保证收口逻辑只跑一次(信号与显式退出可能同时触发)。
var (
	appRootCtx    context.Context    = context.Background()
	appRootCancel context.CancelFunc = func() {}
	appServer     *http.Server
	shutdownOnce  sync.Once
)

var passThroughKeys = []string{
	"tools", "tool_choice", "parallel_tool_calls", "functions", "function_call",
	"temperature", "top_p", "top_k", "stop", "presence_penalty", "frequency_penalty",
	"response_format", "user", "n", "logit_bias", "seed", "logprobs", "top_logprobs",
	"stream_options", "metadata",
}

type chatRequest struct {
	Model               string          `json:"model"`
	Messages            json.RawMessage `json:"messages"`
	Stream              bool            `json:"stream,omitempty"`
	MaxTokens           int             `json:"max_tokens,omitempty"`
	MaxCompletionTokens int             `json:"max_completion_tokens,omitempty"`
	Tools               json.RawMessage `json:"tools,omitempty"`
	ToolChoice          json.RawMessage `json:"tool_choice,omitempty"`
	ReasoningEffort     string          `json:"reasoning_effort,omitempty"`
	ReasoningEffortAlt  string          `json:"reasoningEffort,omitempty"`
	Extra               map[string]any  `json:"-"`
}

func StartProxy(host string, port int) error {
	if strings.TrimSpace(host) == "" {
		// 与命令行默认值保持一致: 只监听回环地址。管理接口虽然另需访问令牌,
		// 但把"默认不对外"作为第一层防线, 要局域网访问必须显式指定。
		host = "127.0.0.1"
	}
	initLogFile()
	setListenOrigin(fmt.Sprintf("http://127.0.0.1:%d", port))

	// 进程级生命周期: 用 signal.NotifyContext 建立可取消的 rootCtx。
	// 所有后台循环(rollStatsDate 等)都 select 在 appRootCtx.Done() 上, 收到
	// SIGINT/SIGTERM 后统一收口(见 doGracefulShutdown)。Shutdown() 也复用同一
	// 个 cancel, 供托盘"退出"与未来控制面复用。
	appRootCtx, appRootCancel = signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer appRootCancel()

	p := loadPool()
	// 这里刻意用 loadPool() 而不是 poolSnapshot(): 本段在 StartProxy 的启动序列里,
	// 服务与各后台循环都还没起来, 不存在并发写方; 而且下面要把 *Account 指针交给
	// refreshAccountToken 去原地刷新 token, 快照(值拷贝)满足不了这个需求。
	activeCount := 0
	for _, a := range p.Accounts {
		if a.Status == "active" {
			// Try to pre-warm tokens
			if a.AccessToken == "" || time.Now().UnixMilli() >= a.ExpiresAt {
				if err := refreshAccountToken(a); err != nil {
					log.Printf("  Pre-warm failed for %s: %v", a.Email, err)
					continue
				}
			}
			activeCount++
		}
	}
	log.Printf("Loaded %d active accounts from pool", activeCount)

	freePort(port)
	// 服务端口登记(P2 修复): 管理页/API 端口必须避开节点入站分配区间,
	// 否则节点池构建可能抢走 3457 → 管理页"无法访问"。
	reserveServicePort(port)

	// 出口基础设施必须先于一切网络任务就绪: 模型同步/目录刷新等启动即发起
	// 请求, 若此时节点未就绪, 首批请求会走 catch-all 直连(大陆 IP), 而共享
	// h2 传输池会把这条连接缓存下来给后续所有 zen 请求复用 —— 地区受限模型
	// 便永远 403。
	initRegionModels()
	syncNodeBox()
	// 订阅缓存恢复异步化(P2 修复): 它会触发一次全量节点构建(4400 节点
	// 逐个 box.New 校验, 需要数分钟)。同步执行会把 HTTP 服务启动堵在后面,
	// 管理页长时间"无法访问"(实测事故)。后台构建, 期间请求走 catch-all 直连。
	go loadSubCache()
	if subs := getZenConfig().Subs; len(subs) > 0 {
		go refreshSubsLoop(subs)
	}
	startModelsRefresher()
	startZenModelsRefresher()
	startProviderRefresher()
	startHeadersAutoSync()
	startUsageLedger()
	startNodeHealthLoop()
	startCooldownJanitor()

	// Register proxy-aware HTTP client for ClinePass provider
	providers.SetProxyDoer(func(req *http.Request) (*http.Response, error) {
		return getZenHTTPClient().Do(req)
	})

	initStats()
	LoadRequestLogsFromFile()
	go cleanupCompactStates()

	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, "/admin/", http.StatusFound)
	})

	mux.HandleFunc("/v1/health", corsHandler(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, healthInfo())
	}))
	mux.HandleFunc("/health", corsHandler(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, healthInfo())
	}))

	// Admin API (frontend + REST)
	registerAdminRoutes(mux)

	apiKeyHandler := func(next http.HandlerFunc) http.HandlerFunc {
		return corsHandler(func(w http.ResponseWriter, r *http.Request) {
			// Allow requests without key if no keys configured
			// 走 poolSnapshot: p.Keys 会被 addAccount / 生成密钥路径在持 poolMu 时
			// append(可能触发底层数组重分配), 而这段是每个网关请求都跑的中间件。
			p := poolSnapshot()
			if len(p.Keys) == 0 {
				next(w, r)
				return
			}

			key := r.Header.Get("x-api-key")
			if key == "" {
				if b := r.Header.Get("Authorization"); len(b) > 7 && b[:7] == "Bearer " {
					key = b[7:]
				}
			}

			valid := false
			for _, k := range p.Keys {
				if k == key {
					valid = true
					break
				}
			}

			if !valid {
				writeJSON(w, http.StatusUnauthorized, map[string]any{
					"error": map[string]string{
						"message": "invalid API key. Generate one at /admin/ or set x-api-key header",
						"type":    "auth_error",
					},
				})
				return
			}
			next(w, r)
		})
	}

	modelsHandler := apiKeyHandler(func(w http.ResponseWriter, r *http.Request) {
		ensureModelsFresh()
		data := apiModelList()
		// 合并 zen 免费模型
		cfg := getZenConfig()
		if cfg.Enabled {
			for _, zm := range zenModelList() {
				data = append(data, map[string]any{
					"id":       zm["id"],
					"object":   "model",
					"created":  time.Now().UnixMilli(),
					"owned_by": "opencode-zen",
					"source":   "zen-free",
					"status":   "active",
					"cost":     "free",
					"context":  zm["context"],
					"output":   zm["output"],
				})
			}
		}
		// 合并 ClinePass 订阅模型
		for _, m := range clinePassProvider().ListModels() {
			data = append(data, map[string]any{
				"id":       m.ID,
				"object":   "model",
				"created":  time.Now().UnixMilli(),
				"owned_by": "clinepass",
				"source":   "clinepass",
				"status":   "active",
				"cost":     m.Cost,
				"context":  m.Context,
				"output":   m.Output,
			})
		}
		// 合并通用 provider 免费模型
		data = append(data, providerModelList()...)
		// 合并路由别名(组合/虚拟模型): 客户端要能"发现"这些模型, 否则别名
		// 只能靠用户手抄; context 取候选池上限, 避免客户端按偏小值提前压缩。
		data = append(data, routeAliasModels()...)
		writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
	})
	mux.HandleFunc("/v1/models", modelsHandler)
	mux.HandleFunc("/models", modelsHandler)

	chatHandler := apiKeyHandler(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": map[string]string{"message": err.Error(), "type": "parse_error"},
			})
			return
		}

		var params map[string]any
		if err := json.Unmarshal(body, &params); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": map[string]string{"message": err.Error(), "type": "parse_error"},
			})
			return
		}

		isStream, _ := params["stream"].(bool)
		toolCount := 0
		if tools, ok := params["tools"]; ok {
			if t, ok := tools.([]any); ok {
				toolCount = len(t)
			}
		}
		model, _ := params["model"].(string)
		model = stripDisplayPrefix(model)
		params["model"] = model
		log.Printf("  client: stream=%v tools=%d model=%s", isStream, toolCount, model)

		// ClinePass 订阅池: cline-pass/ 前缀模型使用独立 key 池,
		// 不依赖 Cline 账号,须在账号池守卫之前分流。
		if strings.HasPrefix(strings.TrimSpace(model), "cline-pass/") {
			setRouteHeader(w, "cline-pass", model, "")
			handleClinePassChat(w, r, params, isStream)
			return
		}

		// 通用 provider: "provider:model" 前缀直选, 不依赖 Cline 账号
		if name, _, ok := parseProviderModel(model); ok {
			setRouteHeader(w, name, model, "")
			handleProviderChat(w, r, params, name)
			return
		}

		// 候选链: 路由别名(如 free-best)展开成有序候选, 逐站 failover。
		// 必须排在下面 routeModel 分支之前 —— 别名本身不是任何一个上游的模型,
		// 交给单个上游只会得到 400。
		if chain, matched, errMsg := resolveRouteChain(model); matched {
			if errMsg != "" {
				writeJSON(w, http.StatusBadRequest, map[string]any{
					"error": map[string]string{"message": errMsg, "type": "invalid_request_error"},
				})
				return
			}
			setRouteHeader(w, "chain", model, "")
			handleChainedChat(w, r, params, chain, model)
			return
		}

		// zen 免费模型路由: zen 上游匿名可用,不依赖 Cline 账号,
		// 同样须在账号池守卫之前分流。
		if route := routeModel(model); route == "zen" {
			setRouteHeader(w, "zen", model, "")
			applyOverride(params)
			handleZenChat(w, r, params)
			return
		} else if route == "reject" {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": map[string]string{"message": zenRejectMessage(model), "type": "invalid_request_error"},
			})
			return
		}

		// 无账户守卫: active 数必须**实时**计算(P3-38) —— 启动期捕获的
		// activeCount 快照永不刷新, 拿它判断会让"运行中账号新增/失效"永远
		// 看不到, 守卫分支长期基于过期数据。poolSnapshot 是持 poolMu 的
		// 深拷贝快照, 锁外读取无竞争。
		snap := poolSnapshot()
		if activeAccountCountOf(snap) == 0 && len(snap.Accounts) == 0 {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{
				"error": map[string]string{
					"message": "No Cline accounts in pool. Add one in the admin panel (/admin/), or use *-free / cline-pass/* models.",
					"type":    "no_accounts_available",
				},
			})
			return
		}

		// zen 免费模型落到 cline 池 = zen 熔断期间的路由降级
		if _, isZen := resolveZenFreeModel(model); isZen {
			setRouteHeader(w, "cline", model, "zen-degraded")
		} else {
			setRouteHeader(w, "cline", model, "")
		}

		// Override system prompt from override.md for OpenAI format
		applyOverride(params)

		// cline 池路由也要记账: 面板的「全部 token」包含这一路,
		// 否则只看得到 opencode 的消耗。
		tracker := newZenStatsTrackerCtx(r.Context(), zenStatsRecord{
			TS:           time.Now().UnixMilli(),
			Upstream:     upstreamCline,
			Model:        model,
			Stream:       isStream,
			PromptTokens: estimateJSON(params),
		})
		status := http.StatusOK
		defer func() { tracker.finish(status < 400, status) }()

		upstreamStream := isStream
		if !isStream {
			model := getDefaultModel()
			if m, ok := params["model"].(string); ok && m != "" {
				model = normalizeRequestModel(m)
			}
			if modelNeedsStream(model) {
				upstreamStream = true
				log.Printf("  model %s requires stream: forcing upstream stream, will aggregate", model)
			}
		}

		resp, acc, err := callClineAPIFailover(r.Context(), params, upstreamStream)
		if err != nil {
			log.Printf("  api error: %v", err)
			// 上游 4xx 原样透传, 其余(网络错误 / 5xx)统一 502。
			// 此前一律回 500, 客户端因此看不出"是模型不存在"还是"被限流"。
			status = upstreamErrorStatus(err)
			if tr := traceFrom(r.Context()); tr != nil {
				tr.SetError(errClassForStatus(status), err.Error())
			}
			writeJSON(w, status, map[string]any{
				"error": map[string]string{"message": err.Error(), "type": "api_error"},
			})
			return
		}
		defer resp.Body.Close()
		status = resp.StatusCode

		baseUsage := accountUsageFn(acc, params)
		usageFn := func(u map[string]any) {
			baseUsage(u)
			tracker.observeUsage(u)
		}

		if isStream {
			// 终审 P2: 交付状态(空流守卫 502 等)回传, defer 才能按真实结果记账。
			status = handleStreamResponseWithUsage(w, resp, usageFn)
			return
		}

		if upstreamStream {
			out, err := collectStreamResponse(resp)
			if err != nil {
				// 内部 500 必须同步 status(P3-37): 否则下方 defer 的
				// tracker.finish(status < 400, status) 仍按 200 记账,
				// 失败请求被 request-log/统计记成成功, 成功率指标失真。
				status = http.StatusInternalServerError
				writeJSON(w, http.StatusInternalServerError, map[string]any{
					"error": map[string]string{"message": err.Error(), "type": "parse_error"},
				})
				return
			}
			if u, ok := out["usage"].(map[string]any); ok && len(u) > 0 {
				usageFn(u)
			}
			out = normalizeOpenAIResponse(out)
			// 终审 P2: 直连非流式聚合路径此前无空内容守卫 —— 上游 200-空流时
			// 客户端拿到 200 + content:"" 的"成功空回合", 静默中断不重试。
			// 判据与流式守卫同源: 正文/工具调用/reasoning + 输出侧 usage + 合法空终止态。
			msg, _ := getNested(out, "choices", 0, "message").(map[string]any)
			var delivered bool
			if msg != nil {
				switch c := msg["content"].(type) {
				case string:
					delivered = strings.TrimSpace(c) != ""
				case []any:
					delivered = len(c) > 0
				}
				if !delivered {
					if tc, ok := msg["tool_calls"].([]any); ok && len(tc) > 0 {
						delivered = true
					}
				}
				if !delivered {
					delivered = hasAnyReasoningSignal(msg)
				}
			}
			u, _ := out["usage"].(map[string]any)
			if streamDeliveryEmpty(delivered, hasOutputUsageTokens(u), legitEmptyTerminalReason(out)) {
				log.Printf("  nonstream (aggregated): 上游交付空回合, 回 empty_content 错误而非空 200")
				status = http.StatusBadGateway
				writeJSON(w, http.StatusBadGateway, map[string]any{
					"error": map[string]string{
						"message": "上游未返回任何内容(聚合结果无有效正文)。这通常是出口节点或上游 worker 异常所致, 请重试; 若持续出现请更换出口节点。",
						"type":    "empty_content",
					},
				})
				return
			}
			log.Printf("  nonstream (aggregated): model=%v content_len=%d finish=%v",
				out["model"], len(getNested(out, "choices", 0, "message", "content").(string)), getNested(out, "choices", 0, "finish_reason"))
			writeJSON(w, http.StatusOK, out)
			return
		}

		status = handleNonStreamResponseWithUsage(w, resp, usageFn)
	})
	mux.HandleFunc("/v1/chat/completions", chatHandler)
	mux.HandleFunc("/chat/completions", chatHandler)

	// Anthropic Messages API support
	anthropicHandler := apiKeyHandler(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		handleAnthropicMessages(w, r)
	})
	mux.HandleFunc("/v1/messages", anthropicHandler)
	mux.HandleFunc("/messages", anthropicHandler)

	// OpenAI Responses API
	responsesHandler := apiKeyHandler(handleResponses)
	mux.HandleFunc("/v1/responses", responsesHandler)
	mux.HandleFunc("/responses", responsesHandler)

	addr := fmt.Sprintf("%s:%d", host, port)
	proxyListenAddress = addr
	server := &http.Server{
		Addr:    addr,
		Handler: limitInboundBody(requestLogMiddleware(mux)),
		// 以前一个超时都没设。ReadHeaderTimeout 是必须的: 不设的话一个
		// 只发半个请求头的连接就能一直占着 goroutine 不放, 几百条就能把
		// 进程拖垮(Slowloris), 而默认零鉴权让任何人都能发。IdleTimeout
		// 回收空闲 keep-alive 连接。
		//
		// WriteTimeout 仍然不设 —— 它是整条响应的绝对上限, 会把 LLM 的
		// 长流式响应直接切断, 这属于刻意的取舍。
		ReadHeaderTimeout: 20 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	appServer = server

	// 优雅退出: rootCtx 被取消(收到 SIGINT/SIGTERM 或显式 Shutdown())后, 先停服
	// 给在途请求一个收尾窗口, 再刷请求日志、关 sing-box 节点、关诊断日志句柄。
	// 收口逻辑用 sync.Once 保证只跑一次。
	go func() {
		<-appRootCtx.Done()
		doGracefulShutdown()
	}()

	token := loadOrCreateAdminToken()
	panelURL := wrapAdminTokenURL(fmt.Sprintf("http://127.0.0.1:%d/admin/", port))
	fmt.Println("")
	fmt.Println(strings.Repeat("=", 58))
	fmt.Println("  Free Router v1.0 - No CLI Required")
	fmt.Println(strings.Repeat("=", 58))
	fmt.Printf("  http://%s\n", addr)
	fmt.Printf("  http://%s/v1\n", addr)
	fmt.Println("  API Key: any value")
	fmt.Printf("  Model:   %s (auto-detected)\n", getDefaultModel())
	fmt.Printf("  Accounts: %d total, %d active\n", len(poolSnapshot().Accounts), activeCount)
	fmt.Println(strings.Repeat("-", 58))
	fmt.Println("  管理后台(链接已含访问令牌, 直接打开即可):")
	fmt.Printf("    %s\n", panelURL)
	fmt.Println("  管理接口需要令牌, 可用 X-Admin-Token 头或 admin_token Cookie:")
	fmt.Printf("    token: %s\n", token)
	fmt.Println("  令牌落盘在 data/admin-token, 重启后不变。")
	fmt.Println(strings.Repeat("=", 58))
	// windowsgui 构建下没有控制台, 上面这些输出用户看不到, 必须同时进日志文件。
	// 但日志里绝不能带令牌: panelURL 自带 ?token=, 而 data/ 常被云同步盘和
	// 一键备份整目录收走, 一次落盘就等于长期访问权限外泄。GUI 下面板从托盘
	// 就能打开, 不需要靠日志。
	log.Printf("admin panel: http://%s/admin/ (访问令牌已省略, 落盘在 data/admin-token)", addr)

	// ListenAndServe 阻塞直到 server.Shutdown 被调用(优雅退出)或发生致命错误。
	// 优雅退出时 Shutdown 会让 ListenAndServe 返回 http.ErrServerClosed, 视为正常。
	err := server.ListenAndServe()
	if err == http.ErrServerClosed {
		err = nil
	}
	return err
}

// activeAccountCountOf 统计快照里的 active 账号数(调用方传 poolSnapshot() 的
// 深拷贝快照, 锁外读取无竞争)。健康接口与请求守卫都必须用它实时计算 ——
// 启动期捕获的 activeCount 快照永不刷新, 会把过期并发数长期上报给运维(P3-38)。
func activeAccountCountOf(snap poolSnapshotData) int {
	n := 0
	for _, a := range snap.Accounts {
		if a.Status == "active" {
			n++
		}
	}
	return n
}

// healthInfo 把网关自身健康指标并入 /health 响应。除了"有几个账号可用",
// 还要能回答"网关自己还好吗": 出口节点池、出口实际可达数、能否优雅退出、
// 订阅刷新时间、日志体积与丢弃计数都在这一并暴露。
//
// status 不再是硬编码的 "ok": 此前出口池几乎全死(实测 24/3958 可达)时它仍回 ok,
// 于是"托盘亮着、面板打得开、所有上游请求都在失败"成了最典型的静默故障。
// 现在只要订阅里确实有节点、也探测过, 但可达数为 0, 就报 degraded。
//
// activeAccounts 同样不再吃启动期快照(P3-38): 每次请求实时经 poolSnapshot
// (持 poolMu 深拷贝)统计, 运行中账号新增/失效能立刻反映, 锁外读取无竞争。
func healthInfo() map[string]any {
	info := map[string]any{
		"version":        buildVersion,
		"activeAccounts": activeAccountCountOf(poolSnapshot()),
	}

	// nodePool: 当前出口节点隧道数(出口池未初始化时为 0)。nodePorts 由 nodeMu 保护,
	// 这里短暂加锁读长度, 避免与 syncNodeBox 并发写产生数据竞争。
	nodeMu.Lock()
	info["nodePool"] = len(nodePorts)
	nodeMu.Unlock()

	// exitReachable / exitProbed: 最近一次健康检测里判定可达的出口数 / 已探测数。
	// 这两个数字以前只写进日志(「增强检测完成(48 并发), 24/3958 个出口可达」),
	// 导致运维拿不到"出口还有几个活着"的机器可读信号。
	nodeHealthMu.Lock()
	reachable, probed := 0, len(nodeHealth)
	for _, st := range nodeHealth {
		if st.Ok {
			reachable++
		}
	}
	nodeHealthMu.Unlock()
	info["exitReachable"] = reachable
	info["exitProbed"] = probed

	// serverRegistered: HTTP server 是否已注册(此前叫 exitReady, 但它只表示
	// appServer != nil, 与"能否优雅退出"无关, 名字会误导排障)。
	info["serverRegistered"] = appServer != nil

	// lastSubFetch: 订阅缓存文件最近一次写入时间(订阅刷新成功即落盘)。
	if st, err := os.Stat(kit.ResolveDataPath("subs_cache.json")); err == nil {
		info["lastSubFetch"] = st.ModTime().UnixMilli()
	} else {
		info["lastSubFetch"] = int64(0)
	}

	// subNodes: 订阅展开后的节点数。
	subNodes := len(subNodeKeysSnapshot())
	info["subNodes"] = subNodes

	// status: 综合判定。只有"订阅里确实有节点、且已经探测过、但一个都不通"才算
	// degraded; 没配订阅或还没探测完仍是 ok(unknown ≠ 不可用)。
	switch {
	case subNodes > 0 && probed > 0 && reachable == 0:
		info["status"] = "degraded"
	default:
		info["status"] = "ok"
	}

	// logBytes: 主日志文件当前大小(诊断磁盘占用)。
	if st, err := os.Stat(kit.ResolveDataPath("free-router.log")); err == nil {
		info["logBytes"] = st.Size()
	} else {
		info["logBytes"] = int64(0)
	}

	// dropped: 请求日志因缓冲满被丢弃的条数(运维可见性, 此前只 atomic.Add 从不暴露)。
	info["dropped"] = atomic.LoadInt64(&reqLogDropped)

	return info
}

// maxInboundBodyBytes 单个入站请求体的上限(64 MiB)。
//
// 取这个量级是为了不误伤正常用法 —— 多模态请求会把图片以 base64 塞进 body,
// 几十 MB 属于合理范围。此前一个上限都没有, 而 /v1/* 在未配置 API Key 时
// 是敞开的, 任何人 POST 一个几 GB 的 body 就能把进程撑爆。出站响应一侧
// 早就统一用了 io.LimitReader, 入站这一侧一直是空白。
const maxInboundBodyBytes = 64 << 20

// limitInboundBody 给所有入站请求体加上上限。
//
// 放在最外层而不是逐个 handler 里: 请求日志中间件也会 io.ReadAll(r.Body),
// 而它对每个请求都跑, 逐个 handler 补一定会漏。
func limitInboundBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, maxInboundBodyBytes)
		}
		next.ServeHTTP(w, r)
	})
}
