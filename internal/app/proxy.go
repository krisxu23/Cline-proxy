package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"cline-go-proxy/internal/cline"
	"cline-go-proxy/internal/kit"
	"cline-go-proxy/internal/protocol"
	"cline-go-proxy/internal/providers"
)

var defaultModel = "deepseek/deepseek-v4-flash"

var proxyListenAddress = "127.0.0.1:3457"

const (
	defaultMaxTokens       = 128000
	defaultReasoningEffort = "high"
)

// buildVersion 由构建时注入: `-ldflags "-X cline-go-proxy/internal/app.buildVersion=$(git describe)"`。
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
		writeJSON(w, http.StatusOK, healthInfo(activeCount))
	}))
	mux.HandleFunc("/health", corsHandler(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, healthInfo(activeCount))
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

		if activeCount == 0 && len(poolSnapshot().Accounts) == 0 {
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
			handleStreamResponseWithUsage(w, resp, usageFn)
			return
		}

		if upstreamStream {
			out, err := collectStreamResponse(resp)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{
					"error": map[string]string{"message": err.Error(), "type": "parse_error"},
				})
				return
			}
			if u, ok := out["usage"].(map[string]any); ok && len(u) > 0 {
				usageFn(u)
			}
			out = normalizeOpenAIResponse(out)
			log.Printf("  nonstream (aggregated): model=%v content_len=%d finish=%v",
				out["model"], len(getNested(out, "choices", 0, "message", "content").(string)), getNested(out, "choices", 0, "finish_reason"))
			writeJSON(w, http.StatusOK, out)
			return
		}

		handleNonStreamResponseWithUsage(w, resp, usageFn)
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
	fmt.Println("  Cline Go Proxy v1.0 - No CLI Required")
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

// healthInfo 把网关自身健康指标并入 /health 响应。除了"有几个账号可用",
// 还要能回答"网关自己还好吗": 出口节点池、出口实际可达数、能否优雅退出、
// 订阅刷新时间、日志体积与丢弃计数都在这一并暴露。
//
// status 不再是硬编码的 "ok": 此前出口池几乎全死(实测 24/3958 可达)时它仍回 ok,
// 于是"托盘亮着、面板打得开、所有上游请求都在失败"成了最典型的静默故障。
// 现在只要订阅里确实有节点、也探测过, 但可达数为 0, 就报 degraded。
func healthInfo(activeCount int) map[string]any {
	info := map[string]any{
		"version":        buildVersion,
		"activeAccounts": activeCount,
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
	if st, err := os.Stat(kit.ResolveDataPath("cline-proxy.log")); err == nil {
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

// maxLogBytes cline-proxy.log 的上限(10 MiB)。
//
// GUI 构建下没有控制台, 这个文件是唯一的诊断通道; 无限追加会在长期运行后
// 撑爆磁盘, 也让翻日志越来越难。超限时截断成空而不是滚动保留历史 ——
// 诊断日志要的是最近一段, 不值得为历史轮转引入额外复杂度。
const maxLogBytes = 10 << 20

// initLogFile 将日志同时输出到控制台与 cline-proxy.log（追加模式），
// 控制台窗口滚动内容有限，文件可完整保留最近 maxLogBytes 的日志。
func initLogFile() {
	path := kit.ResolveDataPath("cline-proxy.log")
	// 0600 是「不写敏感信息」之外的第二道防线: 日志会记录订阅节点、上游返回文本等
	// 可能带凭据的内容。注意 Windows 不实现数字权限位(实测文件仍是 644), 真正生效的
	// 是把令牌本身挡在日志外 —— 见下方「访问令牌已省略」那条 Printf。
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		log.Printf("  open log file failed: %v", err)
		return
	}
	truncated := false
	initialSize := int64(0)
	if st, serr := f.Stat(); serr == nil {
		initialSize = st.Size()
		if initialSize > maxLogBytes {
			// 必须用 os.Truncate(path) 而不是 f.Truncate(0): Windows 下 O_APPEND
			// 句柄没有 GENERIC_WRITE, 句柄级 Truncate 必然 Access denied ——
			// 这正是"启动期截断从来没生效过"的根因。
			if terr := os.Truncate(path, 0); terr == nil {
				truncated = true
				initialSize = 0
			}
		}
	}
	// 用指针而不是值: logFanout 现在带 mutex, 值传递会复制锁(go vet 也会报)。
	// file/path/size 交给它之后, 运行期的轮转就在写入路径上自动完成。
	log.SetOutput(&logFanout{
		dsts: []io.Writer{f, os.Stderr},
		file: f,
		path: path,
		size: initialSize,
	})
	if truncated {
		log.Printf("log file 超过 %d MiB, 已截断(只保留最近内容)", maxLogBytes>>20)
	}
	log.Printf("========== proxy started, log file: %s ==========", path)
}

// logFanout 逐目标分发日志, 单个目标写入失败不影响其它目标。
// 桌面模式(GUI 子系统, 双击启动)下 os.Stderr 句柄无效, io.MultiWriter
// 会在首个 writer 出错时短路, 导致文件日志一并丢失, 故不走 MultiWriter。
//
// 除分发外, 它还在**写入路径上**维护主日志文件的大小上限: 累计写入超过
// maxLogBytes 就把文件截断成空。这一点是关键 —— 早期实现只在启动时检查
// 一次, 于是"长期不重启"等于"日志无上限"; 而订阅源返回异常内容时, 一次
// 刷新就能吐出几千行(实测两轮刷新各 ~4600 行), 足以把磁盘写满。
// 现在与 writeStreamLog 的语义对齐(后者本来就在每次写入时检查)。
type logFanout struct {
	mu   sync.Mutex
	dsts []io.Writer
	file *os.File // 需要做大小检查与截断的目标(主日志文件), 可为 nil
	path string   // 主日志文件路径, 供 os.Truncate 使用(见 truncateIfNeeded)
	size int64    // 当前文件已写入字节数(含本次启动前已有内容)
}

func (w *logFanout) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, dst := range w.dsts {
		dst.Write(p)
	}
	w.size += int64(len(p))
	w.truncateIfNeeded()
	return len(p), nil
}

// truncateIfNeeded 在持锁状态下调用: 超过上限则清空并写入一条醒目标记。
// 注意这里**绝不能走 log.Printf** —— 那会重入 Write 造成死锁, 所以直接写文件。
//
// 为什么用 os.Truncate(path) 而不是 w.file.Truncate(0):
// Windows 下以 os.O_APPEND 打开的句柄只获得 FILE_APPEND_DATA 权限, 句柄级
// Truncate(SetEndOfFile) 会直接返回 "Access is denied"。而本仓库所有日志截断点
// (主日志、流日志、stats、请求日志)都开在 O_APPEND 句柄上并带 `if err == nil`
// 兜底 —— 于是"轮转"在 Windows 上**一直是静默失效**的。os.Truncate 自己开一个
// 不带 O_APPEND 的句柄, 两个平台都可靠。
// 也正因为句柄是 O_APPEND, 截断后无需 Seek: 每次写都会自动落到文件末尾(=0)。
func (w *logFanout) truncateIfNeeded() {
	if w.file == nil || w.size <= maxLogBytes {
		return
	}
	if err := os.Truncate(w.path, 0); err != nil {
		return
	}
	marker := fmt.Sprintf("log file 超过 %d MiB, 已截断(只保留最近内容)\n", maxLogBytes>>20)
	if _, err := w.file.WriteString(marker); err != nil {
		w.size = 0
		return
	}
	w.size = int64(len(marker))
}

// ============================================================================
// 生命周期与优雅退出
// ============================================================================

// Shutdown 触发进程级优雅退出: 取消 rootCtx → 后台循环停止 → server 关闭 →
// 刷盘 → 关 sing-box 节点。托盘"退出"与未来可能的 RPC 控制面都走这里。
func Shutdown() {
	if appRootCancel != nil {
		appRootCancel()
	}
}

// GracefulExit 同步执行完整优雅退出并终止进程, 供托盘"退出"菜单调用:
// 停服 → 刷请求日志(超时兜底) → 关闭 sing-box 节点 → 关闭诊断日志 → os.Exit。
func GracefulExit() {
	Shutdown()
	doGracefulShutdown()
	os.Exit(0)
}

// doGracefulShutdown 实际执行收口工作, 用 sync.Once 保证只跑一次(信号与显式
// 退出可能同时触发)。
func doGracefulShutdown() {
	shutdownOnce.Do(func() {
		// 1) 停服: server.Shutdown 会停止接收新连接并等待在途请求完成, 自带 5s 超时兜底。
		if appServer != nil {
			sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := appServer.Shutdown(sctx); err != nil {
				log.Printf("shutdown: server.Shutdown 超时: %v", err)
			}
		}
		// 2) 请求日志: 把 channel 里已入队的尽量刷出去; 超时也不阻塞退出
		//    (超时意味着写协程卡死, 强行退出比无限等待更可取)。
		closeReqLogsTimed(3 * time.Second)
		// 3) 账号池与用量账本: 这两份数据平时靠后台 ticker / flush channel 批量落盘,
		//    退出时若还没轮到, 就会丢掉最后 ≤30s 的 token 计数与用量 —— 而它们正是
		//    用户此刻在面板上看着的数字。两者内部各自加锁, 可直接调用。
		flushPoolLocked()
		saveUsageLedger()
		// 4) sing-box 节点: 释放端口与句柄。nodeBox 为 nil(如跳过节点盒的测试)时直接跳过。
		closeNodeBoxTimed(3 * time.Second)
		// 5) 流式诊断日志句柄。
		closeStreamLog()
	})
}

// closeReqLogsTimed 在超时内等待请求日志写协程刷盘并关闭句柄; 超时则尽力而为。
func closeReqLogsTimed(timeout time.Duration) {
	done := make(chan struct{})
	go func() {
		closeReqLogs()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		log.Printf("shutdown: 请求日志关闭超时, 部分已排队日志可能未落盘")
	}
}

// closeNodeBoxTimed 在超时内关闭 sing-box 节点盒; 超时则放弃等待。
//
// 必须先持 nodeMu 把句柄"摘下来"再锁外 Close:
//   - 退出可能与订阅刷新触发的 syncNodeBox 交错(那条 ticker 在收到退出信号前
//     仍会跑), 而 syncNodeBox 是在 nodeMu 下读写 nodeBox 的, 无锁读属于数据竞争;
//   - 摘下并置 nil 之后, 并发的 syncNodeBox 在替换阶段会看到 nodeBox != prevBox,
//     从而走"丢弃本次新实例"的分支 —— 正是退出时想要的语义, 也避免了同一个
//     Box 被 Close 两次。
func closeNodeBoxTimed(timeout time.Duration) {
	nodeMu.Lock()
	box := nodeBox
	nodeBox = nil
	nodeMu.Unlock()
	if box == nil {
		return
	}
	done := make(chan struct{})
	go func() {
		box.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		log.Printf("shutdown: 节点盒关闭超时, 端口可能需手动回收")
	}
}

// ============================================================================
// 流式诊断日志(cline-proxy-stream.log)的共享写句柄
//
// 每次 anthropic 流式请求都会把出站 SSE 事件追加进该文件(内容只有模型输出文本,
// 不含请求头与 API key, 不是凭据泄露), 此前无轮转上限会无限增长。复用
// cline-proxy.log 的 10MiB 截断思路: 多并发请求共用同一句柄并加锁, 超过上限则
// 截断成空, 只保留最近内容。
// ============================================================================

var (
	streamLogMu   sync.Mutex
	streamLogFile *os.File
)

// streamLogFileName 流式诊断日志文件名。截断需要按路径重新开句柄(见 writeStreamLog),
// 所以抽成常量而不是内联字面量, 避免两处写法漂移。
const streamLogFileName = "cline-proxy-stream.log"

func writeStreamLog(line string) {
	streamLogMu.Lock()
	defer streamLogMu.Unlock()
	if streamLogFile == nil {
		f, err := os.OpenFile(kit.ResolveDataPath(streamLogFileName), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			return
		}
		streamLogFile = f
	}
	// 超过上限则截断成空, 只保留最近内容(与 cline-proxy.log 的轮转思路一致)。
	if st, serr := streamLogFile.Stat(); serr == nil && st.Size() > maxLogBytes {
		// 必须用 os.Truncate 而不是句柄级 Truncate: O_APPEND 句柄在 Windows 上
		// 拿不到 GENERIC_WRITE, 句柄级截断会 "Access is denied" —— 旧实现因此
		// 一直静默失效(详见 logFanout.truncateIfNeeded 的注释)。
		if terr := os.Truncate(kit.ResolveDataPath(streamLogFileName), 0); terr != nil {
			log.Printf("streamlog: truncate 失败: %v", terr)
		}
	}
	streamLogFile.WriteString(line)
}

func closeStreamLog() {
	streamLogMu.Lock()
	defer streamLogMu.Unlock()
	if streamLogFile != nil {
		streamLogFile.Close()
		streamLogFile = nil
	}
}

// CORS 策略常量: 未来收紧时改这一处就够。之前散落在 7 个 handler 里手写 "*"
// 是历史遗留 —— corsHandler 里是唯一的完整策略源, 但 handleStreamResponseWithUsage /
// handleAnthropicStreamWithUsage / handleResponses / handleClinePass /
// handleChainedChatAs(shapeResponses) 这几个流式/子协议 handler 内部又各自 Set
// 了一次 Origin, 收紧策略时这些点会漏改, 形成同一入口下策略不一致。抽成
// applyCORS / setCORSOrigin 之后, 改一个点就影响所有 handler。
const (
	corsAllowOrigin  = "*"
	corsAllowMethods = "GET, POST, OPTIONS"
	corsAllowHeaders = "Content-Type, Authorization, x-api-key, anthropic-version, anthropic-beta"
)

// applyCORS 在响应上写完整 CORS 头。用于 corsHandler 入口, 以及需要在子 handler
// 里补一遍的流式/子协议路径。
func applyCORS(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", corsAllowOrigin)
	w.Header().Set("Access-Control-Allow-Methods", corsAllowMethods)
	w.Header().Set("Access-Control-Allow-Headers", corsAllowHeaders)
}

// setCORSOrigin 只补 Origin 头, 用于那些父级 handler 已设过 Methods/Headers、
// 但流式路径自己在 W.WriteHeader 前又刷一遍 Origin 的场景。
func setCORSOrigin(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", corsAllowOrigin)
}

func corsHandler(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		applyCORS(w)

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		h(w, r)
	}
}

// setRouteHeader 在响应头中标注本次请求的路由决策, 便于客户端侧排查
// (灵感来自 OmniRoute 的 X-OmniRoute-Decision)。
func setRouteHeader(w http.ResponseWriter, upstream, model, failover string) {
	v := "upstream=" + upstream + "; model=" + model
	if failover != "" {
		v += "; failover=" + failover
	}
	w.Header().Set("X-Proxy-Route", v)
}

// controlSanitizingReader / sanitizeJSONControlChars 说明(方案参照 OmniRoute
// 的 SSE 数据清洗思路, MIT; Go 侧实现):
//
// JSON 对裸控制字符的宽容度是**分位置**的 —— 字符串外部: \t \n \r 是合法
// 空白; 字符串内部: 所有 < 0x20 的字符(含 TAB)都非法。实测上游(C2PA 图片
// 元数据)会在字符串里塞裸控制字符, 既让解析失败, 又会被行切分当成换行把一条
// JSON 劈成多行, 后半段没有 "data:" 前缀遂走"原样透传"直达客户端, 客户端报
// "Bad control character in string literal"。因此在**行切分之前**按位置清洗。
// 必须跟踪字符串状态, 否则会把字符串内的 TAB 漏掉(实测回归)。

// controlSanitizingReader 在读取层做位置感知的控制字符清洗。
// 状态跨 Read 调用保持(中继为单读者串行使用, 无需加锁)。
type controlSanitizingReader struct {
	src      io.Reader
	inString bool
	escaped  bool
}

func (r *controlSanitizingReader) Read(p []byte) (int, error) {
	n, err := r.src.Read(p)
	for i := 0; i < n; i++ {
		c := p[i]
		if r.inString {
			if r.escaped {
				r.escaped = false
				continue
			}
			switch c {
			case '\\':
				r.escaped = true
			case '"':
				r.inString = false
			default:
				if c < 0x20 {
					p[i] = ' '
				}
			}
			continue
		}
		if c == '"' {
			r.inString = true
			continue
		}
		if c < 0x20 && c != '\t' && c != '\n' && c != '\r' {
			p[i] = ' '
		}
	}
	return n, err
}

// sanitizeJSONControlChars 同上, 作用于已切好的一段文本(兜底路径)。
func sanitizeJSONControlChars(b []byte) ([]byte, bool) {
	inStr, esc, dirty := false, false, false
	for _, c := range b {
		if inStr {
			if esc {
				esc = false
				continue
			}
			switch c {
			case '\\':
				esc = true
			case '"':
				inStr = false
			default:
				if c < 0x20 {
					dirty = true
				}
			}
			continue
		}
		if c == '"' {
			inStr = true
			continue
		}
		if c < 0x20 && c != '\t' && c != '\n' && c != '\r' {
			dirty = true
		}
	}
	if !dirty {
		return b, false
	}
	out := make([]byte, len(b))
	inStr, esc = false, false
	for i, c := range b {
		if inStr {
			if esc {
				out[i] = c
				esc = false
				continue
			}
			switch c {
			case '\\':
				out[i] = c
				esc = true
			case '"':
				out[i] = c
				inStr = false
			default:
				if c < 0x20 {
					out[i] = ' '
				} else {
					out[i] = c
				}
			}
			continue
		}
		if c == '"' {
			out[i] = c
			inStr = true
			continue
		}
		if c < 0x20 && c != '\t' && c != '\n' && c != '\r' {
			out[i] = ' '
			continue
		}
		out[i] = c
	}
	return out, true
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

// applyOverride 用 override.md 替换系统提示词(不存在则跳过)
func applyOverride(params map[string]any) {
	override := loadOverrideContent()
	if override == "" {
		return
	}
	if msgs, ok := params["messages"].([]any); ok {
		found := false
		for _, m := range msgs {
			if mm, ok := m.(map[string]any); ok {
				if mm["role"] == "system" {
					mm["content"] = override
					found = true
					break
				}
			}
		}
		if !found {
			params["messages"] = append([]any{map[string]any{"role": "system", "content": override}}, msgs...)
		}
	}
}

// handleZenChat opencode zen 免费模型分支: 压缩 -> 上游 -> 透传,并记录统计
func handleZenChat(w http.ResponseWriter, r *http.Request, params map[string]any) {
	cfg := getZenConfig()
	if !cfg.Enabled {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": map[string]string{"message": "zen upstream disabled in /admin/ settings", "type": "api_error"},
		})
		return
	}
	model, _ := params["model"].(string)
	zm, ok := resolveZenFreeModel(model)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]string{"message": fmt.Sprintf("model %q is not a free zen model", model), "type": "invalid_request_error"},
		})
		return
	}
	isStream, _ := params["stream"].(bool)
	tracker := newZenStatsTrackerCtx(r.Context(), zenStatsRecord{
		TS:           time.Now().UnixMilli(),
		Upstream:     upstreamZen,
		Model:        zm.ID,
		Stream:       isStream,
		PromptTokens: estimateJSON(params),
	})

	sid := requestSessionID(params, r.Header)
	out := maybeCompact(r.Context(), params, zm, sid)
	tracker.rec.Compacted = out.changed
	tracker.rec.CompactionTokens = out.compactTokens
	if out.changed {
		log.Printf("  zen: %s", out.note)
	}

	resp, rateLimited, err := callZenAPI(r.Context(), params, isStream)
	if err != nil {
		log.Printf("  zen api error: %v", err)
		tracker.rec.RateLimited = rateLimited
		status := zenErrorStatus(err)
		// 请求轨迹: 直连路径的失败也要落"错误类别 + 消息", 否则面板只能看到裸状态码。
		if tr := traceFrom(r.Context()); tr != nil {
			tr.SetError(errClassForStatus(status), err.Error())
		}
		writeJSON(w, status, map[string]any{
			"error": map[string]string{"message": err.Error(), "type": "api_error"},
		})
		tracker.finish(false, status)
		return
	}
	tracker.rec.RateLimited = rateLimited
	defer resp.Body.Close()
	tracker.rec.Status = resp.StatusCode

	usageFn := func(u map[string]any) {
		// 镜像进请求轨迹(多协议字段名归一; 与 stats 记账互不影响)。
		tracker.trace.ObserveUsage(u)
		if pt, ok := u["prompt_tokens"].(float64); ok {
			tracker.rec.CompletionTokens += int(pt) - tracker.rec.PromptTokens
			if tracker.rec.CompletionTokens < 0 {
				tracker.rec.CompletionTokens = 0
			}
		}
		if ct, ok := u["completion_tokens"].(float64); ok {
			tracker.rec.CompletionTokens = int(ct)
		}
	}

	if isStream {
		handleStreamResponseWithUsage(w, resp, usageFn)
		tracker.finish(true, resp.StatusCode)
		return
	}
	handleNonStreamResponseWithUsage(w, resp, usageFn)
	tracker.finish(true, resp.StatusCode)
}

func cleanMessages(messages []any) []any {
	cleaned := make([]any, 0, len(messages))
	for _, m := range messages {
		msg, ok := m.(map[string]any)
		if !ok {
			cleaned = append(cleaned, m)
			continue
		}
		cleaned = append(cleaned, msg)
	}
	return cleaned
}

func buildUpstreamBody(params map[string]any, stream bool) map[string]any {
	sessionID := fmt.Sprintf("sess_%d", time.Now().UnixMilli())

	maxTokens := defaultMaxTokens
	if mt, ok := params["max_tokens"].(float64); ok {
		maxTokens = int(mt)
	} else if mt, ok := params["max_completion_tokens"].(float64); ok {
		maxTokens = int(mt)
	}

	model := getDefaultModel()
	if m, ok := params["model"].(string); ok && m != "" {
		model = normalizeRequestModel(m)
	}

	body := map[string]any{
		"model":            model,
		"max_tokens":       maxTokens,
		"session_id":       sessionID,
		"reasoning_effort": defaultReasoningEffort,
	}

	if msgsRaw, ok := params["messages"]; ok {
		if msgsArr, ok := msgsRaw.([]any); ok {
			body["messages"] = cleanMessages(msgsArr)
		} else {
			body["messages"] = msgsRaw
		}
	}

	if stream {
		body["stream"] = true
	}

	if re, ok := params["reasoning_effort"].(string); ok && re != "" {
		body["reasoning_effort"] = re
	} else if re, ok := params["reasoningEffort"].(string); ok && re != "" {
		body["reasoning_effort"] = re
	}

	for _, key := range passThroughKeys {
		if val, ok := params[key]; ok {
			body[key] = val
		}
	}

	return body
}

func clineHeaders(token, sessionID string) http.Header {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+token)
	h.Set("Content-Type", "application/json")
	h.Set("X-Task-ID", sessionID)

	cfg := getProxyConfig()
	for k, v := range cfg.Headers {
		h.Set(k, v)
	}

	return h
}

// callClineAPIFailover wraps callClineAPI with cross-account failover
// (borrowed from okhsunrog/claude-proxy-rs's retry-and-rotate idea):
// retryable failures (429 / network / token refresh) re-pick the next
// account — pickAccount() already excludes cooled-down and expired
// accounts, so each retry naturally rotates — while non-retryable 4xx
// errors return immediately. Attempts are bounded by the pool size.
func callClineAPIFailover(ctx context.Context, params map[string]any, stream bool) (*http.Response, *Account, error) {
	total := len(poolSnapshot().Accounts)
	if total < 1 {
		total = 1
	}
	var (
		resp *http.Response
		acc  *Account
		err  error
	)
	for attempt := 0; attempt < total; attempt++ {
		// 客户端断开(或上层超时)后立即收手, 不再继续换账号重试。
		// 之前这里既没有 ctx 也没有取消检查, 客户端早就走了, 网关还在
		// 逐个账号把请求打完。
		if cerr := ctx.Err(); cerr != nil {
			return nil, acc, cerr
		}
		resp, acc, err = callClineAPI(ctx, params, stream)
		if err == nil {
			return resp, acc, nil
		}
		if !isRetryableUpstreamError(err) {
			return nil, acc, err
		}
		log.Printf("  failover: attempt %d/%d failed (%v), rotating account", attempt+1, total, err)
	}
	return nil, acc, err
}

// isRetryableUpstreamError 判断 callClineAPI 返回的错误是否值得换账号重试。
//
// 优先级: 先看结构化错误里的 HTTP 状态码 —— callClineAPI 对一切非 200 响应都会
// 包装成 *upstreamError 并带上 Status。按状态码判定最稳: 429 限流与 5xx 重试,
// 其余(包括所有 4xx, 如 400 模型不存在 / 401 鉴权 / 403 地域限制)一律不重试,
// 否则这些"必然失败"的请求会被白白打满整个账号池的 failover 轮次。
//
// 字符串兜底不能删: 网络层错误(token 刷新失败、拨号失败)和早期的调用点返回的是
// 没有 Status 的裸 fmt.Errorf, 只能靠既有文案关键词识别。一刀切删掉会让网络抖动
// 被当成"不可重试"而直接 502。
func isRetryableUpstreamError(err error) bool {
	if err == nil {
		return false
	}
	var ue *upstreamError
	if errors.As(err, &ue) && ue.Status != 0 {
		switch ue.Status {
		case http.StatusTooManyRequests, // 429 限流
			http.StatusInternalServerError, // 500
			http.StatusBadGateway,          // 502
			http.StatusServiceUnavailable,  // 503
			http.StatusGatewayTimeout:      // 504
			return true
		}
		// 其余(含全部 4xx)不重试。
		return false
	}
	// 兜底: 无状态码的错误按文案关键词判断(网络错误 / token 刷新失败等)。
	s := err.Error()
	for _, mark := range []string{"429", "token failed", "token expired", "refresh failed", "network error", "upstream request", "upstream retry"} {
		if strings.Contains(s, mark) {
			return true
		}
	}
	return false
}

func callClineAPI(ctx context.Context, params map[string]any, stream bool) (*http.Response, *Account, error) {
	acc := pickAccount()
	if acc == nil {
		return nil, nil, fmt.Errorf("no active accounts available: %s", describePoolStatus())
	}

	token, err := ensureAccountToken(acc)
	if err != nil {
		// Try other accounts
		return nil, nil, fmt.Errorf("account %s token failed: %w", acc.Email, err)
	}

	body := buildUpstreamBody(params, stream)
	sessionID, _ := body["session_id"].(string)

	bodyJSON, err := json.Marshal(body)
	if err != nil {
		return nil, acc, fmt.Errorf("marshal body: %w", err)
	}

	// newClineRequest 每次发送都重建请求对象。
	//
	// http.Request 的 Body 是一次性的: 首次 Do 之后 bytes.Reader 已经读到 EOF
	// 并被关闭。此前 401 分支刷新 token 后直接复用同一个 req 再 Do 一次, 于是
	// transport 报 "http: ContentLength=N with Body length 0" —— 也就是说
	// token 刷新成功之后的补救请求必然失败, 单账号池上直接表现成 500, 多账号池
	// 则被外层换账号掩盖过去。同库 providers_chat.go 踩过同一个坑并留了注释。
	//
	// 顺手带上 ctx: 客户端断开后请求要能被取消, 而不是继续把上游打完。
	newClineRequest := func(tok string) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, "POST", cline.ClineAPIBase+"/chat/completions", bytes.NewReader(bodyJSON))
		if err != nil {
			return nil, err
		}
		req.Header = clineHeaders(tok, sessionID)
		return req, nil
	}

	req, err := newClineRequest(token)
	if err != nil {
		return nil, acc, fmt.Errorf("create request: %w", err)
	}

	toolCount := 0
	if tools, ok := params["tools"]; ok {
		if t, ok := tools.([]any); ok {
			toolCount = len(t)
		}
	}
	log.Printf("  upstream: account=%s stream=%v tools=%d msgs=%d max_tokens=%v effort=%v",
		truncateEmail(acc.Email), stream, toolCount, getMsgCount(params), body["max_tokens"], body["reasoning_effort"])

	resp, err := getZenHTTPClient().Do(req)
	if err != nil {
		// 网络错误：临时短冷却 5 分钟
		markAccountCooldown(acc, "network error: "+err.Error(), 5*time.Minute)
		return nil, acc, fmt.Errorf("upstream request: %w", err)
	}

	if resp.StatusCode == 401 {
		resp.Body.Close()
		// Refresh token and retry
		if err := refreshAccountToken(acc); err == nil {
			// acc.AccessToken 由 refreshAccountToken 在 poolMu 内写入, 读取也走同一把锁。
			poolMu.Lock()
			token = acc.AccessToken
			poolMu.Unlock()
			req, err = newClineRequest(token)
			if err != nil {
				return nil, acc, fmt.Errorf("rebuild request: %w", err)
			}
			resp, err = getZenHTTPClient().Do(req)
			if err != nil {
				return nil, acc, fmt.Errorf("upstream retry: %w", err)
			}
			if resp.StatusCode == 401 {
				resp.Body.Close()
				poolMu.Lock()
				acc.Status = "expired"
				savePoolLocked()
				poolMu.Unlock()
				return nil, acc, fmt.Errorf("account %s token expired permanently", acc.Email)
			}
		} else {
			poolMu.Lock()
			acc.Status = "expired"
			savePoolLocked()
			poolMu.Unlock()
			return nil, acc, fmt.Errorf("account %s refresh failed: %w", acc.Email, err)
		}
	}

	if resp.StatusCode != 200 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		// Mark account on cooldown on rate limits
		if resp.StatusCode == 429 {
			reason := kit.Truncate(string(bodyBytes), 500)
			duration := parseInferenceCapDuration(string(bodyBytes))
			if duration <= 0 {
				duration = parseRetryAfter(resp.Header.Get("Retry-After"))
			}
			markAccountCooldown(acc, "429: "+reason, duration)
			log.Printf("  account %s cooldown %v (reason: %s)", truncateEmail(acc.Email), duration, reason)
		}
		// 返回带类型的上游错误, 而不是裸 fmt.Errorf。
		//
		// 候选链是靠**错误类型**取状态码的(chainErrorStatusBody 只认
		// upstreamError / zenUpstreamError / providerError)。此前这里返回裸
		// error, 于是 cline 的 429/403/404 一律拿不到状态码 → 被
		// classifyCandidateFailure 当成"网络超时"只做 5 分钟短冷却, 并且
		// 回给客户端的响应码被压成 502, 真实原因(限流/下架/无权限)全部丢失。
		// 同目录 providers_chat.go 早就用 providerError 这么做了, 这里补上。
		return nil, acc, &upstreamError{
			Upstream: upstreamCline,
			Status:   resp.StatusCode,
			Body:     kit.Truncate(string(bodyBytes), 500),
		}
	}

	bumpUsage(acc)
	return resp, acc, nil
}

// accountUsageFn 构造账号 token 记账回调：从上游 usage 提取
// prompt_tokens + completion_tokens，计入该账号今日/累计消耗。
func accountUsageFn(acc *Account, params map[string]any) func(map[string]any) {
	return func(u map[string]any) {
		var pt, ct float64
		if v, ok := u["prompt_tokens"].(float64); ok {
			pt = v
		}
		if v, ok := u["completion_tokens"].(float64); ok {
			ct = v
		}
		tokens := int64(pt + ct)
		if tokens <= 0 && params != nil {
			// 上游未返回 usage 时用入站请求估算兜底（与 zen 统计一致）
			tokens = int64(estimateJSON(params))
		}
		recordAccountTokens(acc, tokens)
	}
}

func truncateEmail(email string) string {
	if len(email) <= 12 {
		return email
	}
	parts := splitEmail(email)
	if len(parts) == 2 && len(parts[0]) > 3 {
		return parts[0][:3] + "***@" + parts[1]
	}
	if len(email) > 12 {
		return email[:8] + "..."
	}
	return email
}

func splitEmail(email string) []string {
	for i := 0; i < len(email); i++ {
		if email[i] == '@' {
			return []string{email[:i], email[i+1:]}
		}
	}
	return []string{email}
}

func getMsgCount(params map[string]any) int {
	if msgs, ok := params["messages"].([]any); ok {
		return len(msgs)
	}
	return 0
}

func handleStreamResponseWithUsage(w http.ResponseWriter, upstream *http.Response, onUsage func(map[string]any)) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	setCORSOrigin(w)
	w.WriteHeader(http.StatusOK)

	flusher, ok := w.(http.Flusher)
	if !ok {
		log.Printf("  streaming not supported for client")
		return
	}

	// 流式保活(P1-11): 上游静默超过间隔时注入空 delta 帧, 防止客户端把
	// "上游排队/推理中"当成挂死。行边界注入, 协议合法, 客户端无需感知。
	// 上游流空闲保护(P2, 参照 OmniRoute 的流式 idle 机制): 正文阶段挂起时
	// 主动断开, 由收尾逻辑合成 finish/[DONE], 避免客户端无限等待。
	idleRC := newIdleAbortReader(upstream.Body, streamIdleTimeout())
	defer idleRC.Close()

	src := io.Reader(idleRC)
	if iv := streamHeartbeatInterval(); iv > 0 {
		src = newHeartbeatReader(idleRC, iv, func() []byte { return openAIHeartbeatFrame })
	}
	// 控制字符清洗放在行切分之前(见 controlSanitizingReader 注释)。
	src = &controlSanitizingReader{src: src}

	reader := bufio.NewReader(src)

	// 形态判定(P2, 参照 OmniRoute open-sse/utils/jsonToSse.ts):
	// 上游可能忽略 stream:true 直接回完整 JSON, 或按 NDJSON 逐行回 JSON。
	// 这两类都不是 SSE —— 原样透传会让客户端报 "JSON parsing failed"。
	// 首行探测: 非 data:/event:/注释 且以 { [ 开头 → 走合成路径。
	if firstLine, ferr := reader.ReadString('\n'); looksLikeJSONBody(firstLine) {
		if json.Valid([]byte(strings.TrimSpace(firstLine))) {
			// NDJSON 模式: 逐行转 data: 帧
			log.Printf("%s", sseSynthesisLog("NDJSON", len(firstLine)))
			if frame, ok := ndjsonLineToSSE(firstLine); ok {
				w.Write(frame)
			}
			for {
				line, lerr := reader.ReadString('\n')
				if t := strings.TrimSpace(line); t != "" {
					if frame, ok := ndjsonLineToSSE(t); ok {
						w.Write(frame)
					} else if t == "[DONE]" {
						w.Write([]byte("data: [DONE]\n\n"))
					}
				}
				if lerr != nil {
					break
				}
			}
			if b, mErr := json.Marshal(protocol.EmptyOpenAIChunk("").Payload); mErr == nil {
				w.Write([]byte("data: " + string(b) + "\n\n"))
			}
			w.Write([]byte("data: [DONE]\n\n"))
			flusher.Flush()
			return
		}
		// 多行 JSON body: 缓冲全部后解析合成
		rest, _ := io.ReadAll(io.LimitReader(reader, jsonBodyMaxBytes))
		body := append([]byte(firstLine), rest...)
		if sse, ok := synthesizeOpenAISSEFromJSON(body); ok {
			log.Printf("%s", sseSynthesisLog("完整 JSON body", len(body)))
			w.Write(sse)
			flusher.Flush()
			if onUsage != nil {
				var parsed map[string]any
				if json.Unmarshal(body, &parsed) == nil {
					if u, ok := parsed["usage"].(map[string]any); ok && len(u) > 0 {
						onUsage(u)
					}
				}
			}
			return
		}
		// 合不成: 退回原逻辑(把首行放回处理)
		if _, ok := ndjsonLineToSSE(firstLine); !ok {
			log.Printf("  stream: 上游返回无法识别的非 SSE 数据(%d 字节首行), 按原样透传", len(firstLine))
		}
	} else if ferr == nil {
		// 首行是正常 SSE: 先处理它, 再进入主循环
		if frame := firstLine; strings.TrimSpace(frame) != "" {
			// 复用主循环逻辑: 写入 reader 前部不可行, 这里直接解析一次
			line := strings.TrimRight(frame, "\r\n")
			if strings.HasPrefix(line, "data:") {
				payload := strings.TrimSpace(line[5:])
				if payload != "" && payload != "[DONE]" {
					var obj map[string]any
					if json.Unmarshal([]byte(payload), &obj) == nil {
						w.Write([]byte("data: " + payload + "\n\n"))
						flusher.Flush()
					}
				} else if payload == "[DONE]" {
					w.Write([]byte("data: [DONE]\n\n"))
					flusher.Flush()
				}
			}
		}
	}

	sawFinish := false // 上游是否已发过 finish_reason
	sawDone := false   // 上游是否已发过 [DONE]
	lastModel := ""    // 用于兜底 chunk 的 model 字段
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				if line != "" {
					w.Write([]byte(line + "\n"))
				}
			}
			break
		}

		line = strings.TrimRight(line, "\r\n")

		if strings.HasPrefix(line, "data:") {
			payload := strings.TrimSpace(line[5:])
			if payload == "" {
				w.Write([]byte(line + "\n\n"))
				flusher.Flush()
				continue
			}
			if payload == "[DONE]" {
				// 上游结束但从未给出 finish_reason: 补一个终止 chunk
				if !sawFinish {
					if b, mErr := json.Marshal(protocol.EmptyOpenAIChunk(lastModel).Payload); mErr == nil {
						w.Write([]byte("data: " + string(b) + "\n\n"))
					}
				}
				sawDone = true
				w.Write([]byte(line + "\n\n"))
				flusher.Flush()
				continue
			}

			// Try to normalize the response
			var obj map[string]any
			if err := json.Unmarshal([]byte(payload), &obj); err != nil {
				// 坏行门卫(P2 修复): 上游会送来两类脏 JSON ——
				//   1) 被截断/交错的行(实测 {"choices"0}],...);
				//   2) 字符串里夹裸控制字符的行(实测 _manifest/C2PA 图片元数据,
				//      报 "Bad control character in string literal")。
				// 先尝试清洗控制字符后重试(能救回来就不丢内容), 仍失败才丢弃。
				if fixed, ok := sanitizeJSONControlChars([]byte(payload)); ok {
					if err := json.Unmarshal(fixed, &obj); err == nil {
						log.Printf("  stream: 上游 data 行含非法控制字符, 已清洗救回(%d 字节)", len(payload))
					} else {
						log.Printf("  stream: 丢弃无法解析的上游 data 行(%d 字节): %q", len(payload), kit.Truncate(payload, 120))
						continue
					}
				} else {
					log.Printf("  stream: 丢弃无法解析的上游 data 行(%d 字节): %q", len(payload), kit.Truncate(payload, 120))
					continue
				}
			}
			{
				if onUsage != nil {
					if u, ok := obj["usage"].(map[string]any); ok && len(u) > 0 {
						onUsage(u)
					}
				}
				// Some Cline responses wrap in {data: {...}}
				if data, ok := obj["data"]; ok {
					if d, ok := data.(map[string]any); ok {
						if _, hasChoices := d["choices"]; hasChoices {
							obj = d
						}
						if _, hasID := d["id"]; hasID {
							obj = d
						}
					}
				}
				if m, ok := obj["model"].(string); ok && m != "" {
					lastModel = m
				}
				normalized := normalizeOpenAIResponse(obj)
				if !sawFinish && protocol.HasStopSignal(normalized) { // 跨协议终止判定(OmniRoute checkIfStopSignal 等价)
					sawFinish = true
				}
				if normBytes, err := json.Marshal(normalized); err == nil {
					w.Write([]byte("data: " + string(normBytes) + "\n\n"))
					flusher.Flush()
					continue
				}
			}
		}

		w.Write([]byte(line + "\n"))
		flusher.Flush()
	}

	// 上游断流未发 [DONE](或发 [DONE] 前无 finish_reason): 合成收尾,
	// 避免客户端报 "Stream ended without finish_reason" 或挂起等待。
	if !sawFinish {
		if b, mErr := json.Marshal(protocol.EmptyOpenAIChunk(lastModel).Payload); mErr == nil {
			w.Write([]byte("data: " + string(b) + "\n\n"))
			flusher.Flush()
		}
	}
	if !sawDone {
		w.Write([]byte("data: [DONE]\n\n"))
		flusher.Flush()
	}
}

func handleNonStreamResponseWithUsage(w http.ResponseWriter, upstream *http.Response, onUsage func(map[string]any)) {
	var raw map[string]any
	if err := json.NewDecoder(upstream.Body).Decode(&raw); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": map[string]string{"message": err.Error(), "type": "parse_error"},
		})
		return
	}

	if onUsage != nil {
		if u, ok := raw["usage"].(map[string]any); ok && len(u) > 0 {
			onUsage(u)
		}
	}

	// Some Cline responses wrap in {data: {...}}
	out := raw
	if data, ok := raw["data"]; ok {
		if d, ok := data.(map[string]any); ok {
			out = d
		}
	}

	out = normalizeOpenAIResponse(out)

	if msg, ok := getNested(out, "choices", 0, "message").(map[string]any); ok {
		tc, _ := msg["tool_calls"].([]any)
		content, _ := msg["content"].(string)
		log.Printf("  nonstream finish=%v tool_calls=%d content_len=%d",
			getNested(out, "choices", 0, "finish_reason"),
			len(tc), len(content))
	}

	writeJSON(w, http.StatusOK, out)
}

func collectStreamResponse(upstream *http.Response) (map[string]any, error) {
	var (
		model        string
		content      strings.Builder
		finishReason string
		usage        map[string]any
		toolCalls    []any
		toolCallIdx  = -1
		curToolCall  map[string]any
		curArgs      strings.Builder
	)

	reader := bufio.NewReader(upstream.Body)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if err != io.EOF && err != bufio.ErrBufferFull {
				break
			}
		}
		line = strings.TrimRight(line, "\r\n")

		if strings.HasPrefix(line, "data:") {
			payload := strings.TrimSpace(line[5:])
			if payload == "" || payload == "[DONE]" {
				if err == io.EOF {
					break
				}
				continue
			}
			var obj map[string]any
			if json.Unmarshal([]byte(payload), &obj) != nil {
				continue
			}
			if data, ok := obj["data"]; ok {
				if d, ok := data.(map[string]any); ok {
					obj = d
				}
			}
			if m, ok := obj["model"].(string); ok && m != "" {
				model = m
			}
			if u, ok := obj["usage"].(map[string]any); ok && len(u) > 0 {
				usage = u
			}
			choices, _ := getNested(obj, "choices").([]any)
			if len(choices) == 0 {
				continue
			}
			choice, _ := choices[0].(map[string]any)
			if choice == nil {
				continue
			}
			delta, _ := choice["delta"].(map[string]any)
			if delta == nil {
				delta = choice
			}
			if fr, ok := choice["finish_reason"].(string); ok && fr != "" {
				finishReason = fr
			}
			if c, ok := delta["content"].(string); ok && c != "" {
				content.WriteString(c)
			}
			if tcRaw, ok := delta["tool_calls"].([]any); ok {
				for _, tc := range tcRaw {
					tcMap, _ := tc.(map[string]any)
					if tcMap == nil {
						continue
					}
					idx := 0
					if i, ok := tcMap["index"].(float64); ok {
						idx = int(i)
					}
					if idx != toolCallIdx {
						if curToolCall != nil {
							curToolCall["function"].(map[string]any)["arguments"] = curArgs.String()
							toolCalls = append(toolCalls, curToolCall)
						}
						curToolCall = map[string]any{
							"id":       tcMap["id"],
							"type":     "function",
							"function": map[string]any{"name": "", "arguments": ""},
						}
						curArgs.Reset()
						toolCallIdx = idx
					}
					if fn, ok := tcMap["function"].(map[string]any); ok {
						if n, ok := fn["name"].(string); ok && n != "" {
							curToolCall["function"].(map[string]any)["name"] = n
						}
						if a, ok := fn["arguments"].(string); ok && a != "" {
							curArgs.WriteString(a)
						}
					}
				}
			}
		}
		if err == io.EOF {
			break
		}
	}
	if curToolCall != nil {
		curToolCall["function"].(map[string]any)["arguments"] = curArgs.String()
		toolCalls = append(toolCalls, curToolCall)
	}

	message := map[string]any{
		"role":    "assistant",
		"content": content.String(),
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}
	choice := map[string]any{
		"index":         0,
		"message":       message,
		"finish_reason": finishReason,
	}
	out := map[string]any{
		"id":      "chatcmpl_" + fmt.Sprintf("%x", time.Now().UnixMilli()),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{choice},
	}
	if usage != nil {
		out["usage"] = usage
	}
	return out, nil
}

func modelNeedsStream(modelID string) bool {
	initModelsCache()
	modelsMu.Lock()
	defer modelsMu.Unlock()
	if m, ok := modelsCache[modelID]; ok && m.RequiresStream {
		return true
	}
	return false
}

// Anthropic Messages API support
type anthropicMsg struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type toolAccumulator struct {
	index   int
	id      string
	name    string
	args    string
	emitted bool
}

type anthropicReq struct {
	Model       string          `json:"model"`
	MaxTokens   int             `json:"max_tokens"`
	Messages    []anthropicMsg  `json:"messages"`
	System      json.RawMessage `json:"system,omitempty"`
	Stream      bool            `json:"stream,omitempty"`
	Temperature float64         `json:"temperature,omitempty"`
	TopP        float64         `json:"top_p,omitempty"`
	TopK        int             `json:"top_k,omitempty"`
	Stop        json.RawMessage `json:"stop_sequences,omitempty"`
	Tools       json.RawMessage `json:"tools,omitempty"`
	ToolChoice  json.RawMessage `json:"tool_choice,omitempty"`
	Metadata    json.RawMessage `json:"metadata,omitempty"`
	Extra       map[string]any  `json:"-"`
}

func loadOverrideContent() string {
	data, err := os.ReadFile("override.md")
	if err != nil {
		// override.md 是可选功能，文件不存在时静默使用客户端自带提示词
		return ""
	}
	content := strings.TrimSpace(string(data))
	if content != "" {
		log.Printf("  using override.md as system prompt (%d bytes)", len(content))
	} else {
		log.Printf("  override.md is empty, using client system prompt")
	}
	return content
}

func extractStringContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	// Try string first
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	// Try array of content blocks
	var blocks []map[string]any
	if err := json.Unmarshal(raw, &blocks); err == nil {
		parts := []string{}
		for _, b := range blocks {
			if b["type"] == "text" {
				if t, ok := b["text"].(string); ok {
					parts = append(parts, t)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func anthropicToolsToOpenAI(tools []any) []any {
	out := make([]any, 0, len(tools))
	for _, t := range tools {
		if tMap, ok := t.(map[string]any); ok {
			// Already in OpenAI format
			if tMap["type"] == "function" {
				out = append(out, t)
				continue
			}
			// Convert Anthropic format to OpenAI
			oai := map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        tMap["name"],
					"description": tMap["description"],
					"parameters":  tMap["input_schema"],
				},
			}
			out = append(out, oai)
		}
	}
	return out
}

func anthropicToOpenAI(req anthropicReq) map[string]any {
	openAI := map[string]any{
		"model":      req.Model,
		"max_tokens": req.MaxTokens,
		"stream":     req.Stream,
		"messages":   []any{},
	}
	if req.Temperature != 0 {
		openAI["temperature"] = req.Temperature
	}
	if req.TopP != 0 {
		openAI["top_p"] = req.TopP
	}
	// Convert Anthropic tools to OpenAI format
	if req.Tools != nil {
		var toolsArr []any
		if err := json.Unmarshal(req.Tools, &toolsArr); err == nil {
			openAI["tools"] = anthropicToolsToOpenAI(toolsArr)
		}
	}
	if req.ToolChoice != nil {
		openAI["tool_choice"] = req.ToolChoice
	}

	msgs := []any{}

	// System prompt: use override.md if it exists, otherwise use Anthropic's system field
	sysContent := loadOverrideContent()
	if sysContent == "" && req.System != nil {
		sysContent = extractStringContent(req.System)
	}
	if sysContent != "" {
		log.Printf("  system prompt: %d bytes (from override.md)", len(sysContent))
		msgs = append(msgs, map[string]any{"role": "system", "content": sysContent})
	}

	for _, m := range req.Messages {
		switch c := m.Content.(type) {
		case string:
			msgs = append(msgs, map[string]any{"role": m.Role, "content": c})
		case []any:
			textParts := []string{}
			var toolCalls []any
			var toolResults []map[string]any

			for _, block := range c {
				if b, ok := block.(map[string]any); ok {
					switch b["type"] {
					case "text":
						if t, ok := b["text"].(string); ok {
							textParts = append(textParts, t)
						}
					case "image":
						// skip images
					case "tool_use":
						argsStr := "{}"
						if input, ok := b["input"]; ok && input != nil {
							if s, ok := input.(string); ok {
								argsStr = s
							} else if bts, err := json.Marshal(input); err == nil {
								argsStr = string(bts)
							}
						}
						tc := map[string]any{
							"id":   b["id"],
							"type": "function",
							"function": map[string]any{
								"name":      b["name"],
								"arguments": argsStr,
							},
						}
						toolCalls = append(toolCalls, tc)
					case "tool_result":
						toolCallID, _ := b["tool_use_id"].(string)
						if toolCallID == "" {
							continue
						}
						toolResults = append(toolResults, map[string]any{
							"role":         "tool",
							"content":      anthropicContentToString(b["content"]),
							"tool_call_id": toolCallID,
						})
					}
				}
			}

			if m.Role == "assistant" && len(toolCalls) > 0 {
				msg := map[string]any{
					"role":       "assistant",
					"content":    strings.Join(textParts, "\n"),
					"tool_calls": toolCalls,
				}
				msgs = append(msgs, msg)
				log.Printf("  anthropic req: assistant tool_calls=%d", len(toolCalls))
			} else if m.Role == "user" && len(toolResults) > 0 {
				for _, tr := range toolResults {
					msgs = append(msgs, tr)
					content, _ := tr["content"].(string)
					id, _ := tr["tool_call_id"].(string)
					log.Printf("  anthropic req: tool_result id=%s content_len=%d prefix=%s", id, len(content), kit.Truncate(content, 400))
				}
			} else {
				content := strings.Join(textParts, "\n")
				msgs = append(msgs, map[string]any{"role": m.Role, "content": content})
			}
		}
	}

	openAI["messages"] = msgs
	return openAI
}

// parseToolArgs 解析工具调用参数 JSON，带容错修复
func parseToolArgs(raw string) (any, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return map[string]any{}, nil
	}
	// 清理杂散引号前缀：上游流式输出偶发 "" 前缀（如 ""{"file_path":...}）
	for strings.HasPrefix(raw, `""`) {
		raw = strings.TrimPrefix(raw, `""`)
	}
	raw = strings.TrimSpace(raw)
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err == nil {
		if v == nil {
			return map[string]any{}, nil
		}
		return v, nil
	}
	// 整体被 JSON 字符串包裹（"{\"file_path\": ...}"）时，解包字符串后再解析
	if strings.HasPrefix(raw, `"`) && strings.HasSuffix(raw, `"`) && len(raw) >= 2 {
		var s string
		if json.Unmarshal([]byte(raw), &s) == nil {
			var v2 any
			if json.Unmarshal([]byte(s), &v2) == nil && v2 != nil {
				return v2, nil
			}
		}
	}
	fixed := raw
	if strings.HasPrefix(fixed, "{") && !strings.HasSuffix(fixed, "}") {
		fixed += "}"
	} else if strings.HasPrefix(fixed, "[") && !strings.HasSuffix(fixed, "]") {
		fixed += "]"
	}
	if strings.HasSuffix(fixed, ",") {
		fixed = strings.TrimRight(fixed, ",") + "}"
	}
	if err := json.Unmarshal([]byte(fixed), &v); err == nil && v != nil {
		return v, nil
	}
	// 最终兜底：从杂散内容中提取首个 JSON 对象/数组
	if i := strings.IndexAny(raw, "{["); i >= 0 {
		openCh := raw[i]
		closeCh := byte('}')
		if openCh == '[' {
			closeCh = ']'
		}
		if j := strings.LastIndex(raw, string(closeCh)); j > i {
			sub := raw[i : j+1]
			if json.Unmarshal([]byte(sub), &v) == nil && v != nil {
				return v, nil
			}
		}
	}
	return nil, fmt.Errorf("invalid json: %s", kit.Truncate(raw, 120))
}

// extractToolSchemas 从 Anthropic 请求的 tools 定义中解析每个工具的 input_schema 属性集合，
// 用于转发 tool_use 时裁剪 input，避免多余字段触发客户端校验失败。
func extractToolSchemas(tools json.RawMessage) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	if len(tools) == 0 {
		return out
	}
	var arr []map[string]any
	if err := json.Unmarshal(tools, &arr); err != nil {
		return out
	}
	for _, t := range arr {
		name, _ := t["name"].(string)
		if name == "" {
			continue
		}
		schema, _ := t["input_schema"].(map[string]any)
		props, _ := schema["properties"].(map[string]any)
		var propNames []string
		for k := range props {
			propNames = append(propNames, k)
		}
		var required []string
		if req, ok := schema["required"].([]any); ok {
			for _, r := range req {
				if s, ok := r.(string); ok {
					required = append(required, s)
				}
			}
		}
		log.Printf("  tool schema: name=%s properties=%v required=%v", name, propNames, required)
		if len(props) == 0 {
			continue
		}
		allowed := map[string]bool{}
		for k := range props {
			allowed[k] = true
		}
		out[name] = allowed
	}
	return out
}

// filterToolInput 将工具参数裁剪到客户端 schema 允许的字段内；
// 找不到 schema 或过滤后为空时保留原参数，避免丢参数。
func filterToolInput(name string, input map[string]any, schemas map[string]map[string]bool) map[string]any {
	allowed, ok := schemas[name]
	if !ok || len(allowed) == 0 {
		return input
	}
	out := map[string]any{}
	for k, v := range input {
		if allowed[k] {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return input
	}
	return out
}

// anthropicContentToString 将 Anthropic content（字符串或块数组）转为纯文本
func anthropicContentToString(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	if arr, ok := v.([]any); ok {
		parts := []string{}
		for _, it := range arr {
			if b, ok := it.(map[string]any); ok {
				if t, ok := b["text"].(string); ok {
					parts = append(parts, t)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func openAIToAnthropic(openAI map[string]any) map[string]any {
	out := map[string]any{
		"id":    "msg_" + fmt.Sprintf("%x", time.Now().UnixMilli()),
		"type":  "message",
		"role":  "assistant",
		"model": getNested(openAI, "model"),
	}

	choices := getNested(openAI, "choices")
	if choices == nil {
		out["content"] = []any{map[string]any{"type": "text", "text": ""}}
		out["stop_reason"] = "end_turn"
		out["usage"] = map[string]any{"input_tokens": 0, "output_tokens": 0}
		return out
	}

	text := ""
	choice0, ok := getNested(openAI, "choices", 0).(map[string]any)
	if !ok {
		out["content"] = []any{map[string]any{"type": "text", "text": text}}
		out["stop_reason"] = "end_turn"
		out["usage"] = map[string]any{"input_tokens": 0, "output_tokens": 0}
		return out
	}
	msg, _ := choice0["message"].(map[string]any)
	if msg == nil {
		msg, _ = choice0["delta"].(map[string]any)
	}
	if msg != nil {
		if c, ok := msg["content"].(string); ok {
			text = sanitizeContent(c)
		}
	}

	contentBlocks := []any{map[string]any{"type": "text", "text": text}}

	// reasoning_content -> thinking block (非流式路径, 与流式 thinking 透传一致)
	if msg != nil {
		if rc, ok := msg["reasoning_content"].(string); ok && rc != "" {
			contentBlocks = append([]any{map[string]any{"type": "thinking", "thinking": rc}}, contentBlocks...)
		}
	}

	// Convert tool_calls to Anthropic tool_use blocks
	if msg != nil {
		if tc, ok := msg["tool_calls"].([]any); ok && len(tc) > 0 {
			contentBlocks = []any{}
			if text != "" {
				contentBlocks = append(contentBlocks, map[string]any{"type": "text", "text": text})
			}
			for _, tcItem := range tc {
				if tcMap, ok := tcItem.(map[string]any); ok {
					funcData, _ := tcMap["function"].(map[string]any)
					if funcData == nil {
						continue
					}
					input := funcData["arguments"]
					// OpenAI arguments is a JSON string; Anthropic expects an object
					if argsStr, ok := input.(string); ok {
						var argsObj any
						if json.Unmarshal([]byte(argsStr), &argsObj) == nil {
							input = argsObj
						}
					}
					if input == nil {
						input = map[string]any{}
					}
					id, _ := tcMap["id"].(string)
					if id == "" {
						id = fmt.Sprintf("toolu_%x_%d", time.Now().UnixMilli(), len(contentBlocks))
					}
					name, _ := funcData["name"].(string)
					if name == "" {
						continue
					}
					block := map[string]any{
						"type":  "tool_use",
						"id":    id,
						"name":  name,
						"input": input,
					}
					contentBlocks = append(contentBlocks, block)
				}
			}
		}
	}

	out["content"] = contentBlocks

	switch getNested(openAI, "choices", 0, "finish_reason") {
	case "stop":
		out["stop_reason"] = "end_turn"
	case "length":
		out["stop_reason"] = "max_tokens"
	case "tool_calls":
		out["stop_reason"] = "tool_use"
	default:
		out["stop_reason"] = "end_turn"
	}

	usage := map[string]any{}
	if u := getNested(openAI, "usage"); u != nil {
		if um, ok := u.(map[string]any); ok {
			usage["input_tokens"] = um["prompt_tokens"]
			usage["output_tokens"] = um["completion_tokens"]
		}
	}
	out["usage"] = usage

	return out
}

func handleAnthropicMessages(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]string{"message": err.Error(), "type": "parse_error"},
		})
		return
	}

	var req anthropicReq
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]string{"message": err.Error(), "type": "parse_error"},
		})
		return
	}

	if len(req.Messages) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]string{"message": "messages is required", "type": "parse_error"},
		})
		return
	}

	toolSchemas := extractToolSchemas(req.Tools)
	if len(toolSchemas) > 0 {
		log.Printf("  anthropic tools: %d schemas", len(toolSchemas))
	}

	if req.MaxTokens == 0 {
		req.MaxTokens = defaultMaxTokens
	}

	openAIReq := anthropicToOpenAI(req)
	req.Model = stripDisplayPrefix(req.Model)
	openAIReq["model"] = req.Model

	log.Printf("  anthropic: model=%s stream=%v msgs=%d", req.Model, req.Stream, len(req.Messages))

	// 通用 Provider 直选: "provider:model"。
	//
	// 必须与 OpenAI 入口(/v1/chat/completions)保持一致: /v1/models 会把这些模型
	// 公开出去, 但此前只有 OpenAI 入口认这个前缀, 于是在 Anthropic 端点上
	// bai:glm-5.3 会落到 routeModel 被当成 cline 池的模型名送出去并必然失败 ——
	// 同一个模型名在三个协议端点上行为不一致。
	//
	// 直接复用候选链的调度器(单候选): 三种响应形状的转换只维护一份, 不为
	// 这一条路径再写一个新的 shape 转换分支。
	if name, sub, ok := parseProviderModel(req.Model); ok {
		setRouteHeader(w, name, req.Model, "")
		handleChainedChatAs(w, r, openAIReq,
			[]routeCandidate{{Upstream: name, Model: sub}}, req.Model,
			chainTarget{Shape: shapeAnthropic, ToolSchemas: toolSchemas})
		return
	}

	// 候选链: 路由别名(如 free-best)展开成有序候选, 逐站 failover。
	// openAIReq 已是转换后的 OpenAI 形状, 胜出那一站的响应再转回 Anthropic。
	if chain, matched, errMsg := resolveRouteChain(req.Model); matched {
		if errMsg != "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": map[string]string{"message": errMsg, "type": "invalid_request_error"},
			})
			return
		}
		setRouteHeader(w, "chain", req.Model, "")
		handleChainedChatAs(w, r, openAIReq, chain, req.Model, chainTarget{Shape: shapeAnthropic, ToolSchemas: toolSchemas})
		return
	}

	// zen / clinepass 免费模型路由
	if route := routeModel(req.Model); route == "zen" {
		setRouteHeader(w, "zen", req.Model, "")
		handleZenAnthropic(w, r, req, openAIReq, toolSchemas)
		return
	} else if route == "reject" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]string{"message": zenRejectMessage(req.Model), "type": "invalid_request_error"},
		})
		return
	}

	// ClinePass 订阅池: cline-pass/ 前缀模型
	if strings.HasPrefix(strings.TrimSpace(req.Model), "cline-pass/") {
		setRouteHeader(w, "cline-pass", req.Model, "")
		handleClinePassAnthropic(w, r, req, openAIReq, toolSchemas)
		return
	}

	activeCount := 0
	// 请求热路径: 必须走 poolSnapshot, 否则与 pickAccount 持 poolMu 改 a.Status 并发。
	p := poolSnapshot()
	for _, a := range p.Accounts {
		if a.Status == "active" {
			activeCount++
		}
	}

	// zen 免费模型落到 cline 池 = zen 熔断期间的路由降级
	if _, isZen := resolveZenFreeModel(req.Model); isZen {
		setRouteHeader(w, "cline", req.Model, "zen-degraded")
	} else {
		setRouteHeader(w, "cline", req.Model, "")
	}

	if activeCount == 0 && len(p.Accounts) == 0 {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": map[string]string{
				"message": "No Cline accounts in pool. Add one in the admin panel (/admin/), or use *-free / cline-pass/* models.",
				"type":    "no_accounts_available",
			},
		})
		return
	}

	upstreamStream := req.Stream
	if !req.Stream && modelNeedsStream(normalizeRequestModel(req.Model)) {
		upstreamStream = true
		log.Printf("  anthropic model %s requires stream: forcing upstream stream, will aggregate", req.Model)
	}

	resp, acc, err := callClineAPIFailover(r.Context(), openAIReq, upstreamStream)
	if err != nil {
		log.Printf("  anthropic api error: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": map[string]string{"message": err.Error(), "type": "api_error"},
		})
		return
	}
	defer resp.Body.Close()

	usageFn := accountUsageFn(acc, openAIReq)

	if req.Stream {
		handleAnthropicStreamWithUsage(w, resp, normalizeRequestModel(req.Model), toolSchemas, usageFn)
		return
	}

	if upstreamStream {
		out, err := collectStreamResponse(resp)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{
				"error": map[string]string{"message": err.Error(), "type": "parse_error"},
			})
			return
		}
		if u, ok := out["usage"].(map[string]any); ok && len(u) > 0 {
			usageFn(u)
		}
		out = normalizeOpenAIResponse(out)
		anthropicResp := openAIToAnthropic(out)
		if tc, ok := getNested(out, "choices", 0, "message", "tool_calls").([]any); ok && len(tc) > 0 {
			anthropicResp["stop_reason"] = "tool_use"
		}
		writeJSON(w, http.StatusOK, anthropicResp)
		return
	}

	var raw map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": map[string]string{"message": err.Error(), "type": "parse_error"},
		})
		return
	}
	if u, ok := raw["usage"].(map[string]any); ok && len(u) > 0 {
		usageFn(u)
	}
	out := raw
	if data, ok := raw["data"]; ok {
		if d, ok := data.(map[string]any); ok {
			out = d
		}
	}
	out = normalizeOpenAIResponse(out)
	anthropicResp := openAIToAnthropic(out)

	if tc, ok := getNested(out, "choices", 0, "message", "tool_calls").([]any); ok && len(tc) > 0 {
		anthropicResp["stop_reason"] = "tool_use"
	}

	writeJSON(w, http.StatusOK, anthropicResp)
}

// handleZenAnthropic Anthropic Messages 请求路由到 zen 免费模型上游
func handleZenAnthropic(w http.ResponseWriter, r *http.Request, req anthropicReq, openAIReq map[string]any, toolSchemas map[string]map[string]bool) {
	cfg := getZenConfig()
	if !cfg.Enabled {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": map[string]string{"message": "zen upstream disabled in /admin/ settings", "type": "api_error"},
		})
		return
	}
	zm, ok := resolveZenFreeModel(req.Model)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]string{"message": fmt.Sprintf("model %q is not a free zen model", req.Model), "type": "invalid_request_error"},
		})
		return
	}
	isStream := req.Stream
	tracker := newZenStatsTrackerCtx(r.Context(), zenStatsRecord{
		TS:           time.Now().UnixMilli(),
		Upstream:     upstreamZen,
		Model:        zm.ID,
		Stream:       isStream,
		PromptTokens: estimateJSON(openAIReq),
	})

	sid := requestSessionID(openAIReq, r.Header)
	out := maybeCompact(r.Context(), openAIReq, zm, sid)
	tracker.rec.Compacted = out.changed
	tracker.rec.CompactionTokens = out.compactTokens
	if out.changed {
		log.Printf("  anthropic zen: %s", out.note)
	}

	resp, rateLimited, err := callZenAPI(r.Context(), openAIReq, isStream)
	if err != nil {
		log.Printf("  anthropic zen api error: %v", err)
		tracker.rec.RateLimited = rateLimited
		status := zenErrorStatus(err)
		writeJSON(w, status, map[string]any{
			"error": map[string]string{"message": err.Error(), "type": "api_error"},
		})
		tracker.finish(false, status)
		return
	}
	tracker.rec.RateLimited = rateLimited
	defer resp.Body.Close()
	tracker.rec.Status = resp.StatusCode

	usageFn := func(u map[string]any) {
		if ct, ok := u["completion_tokens"].(float64); ok {
			tracker.rec.CompletionTokens = int(ct)
		}
	}

	if isStream {
		handleAnthropicStreamWithUsage(w, resp, zm.ID, toolSchemas, usageFn)
		tracker.finish(true, resp.StatusCode)
		return
	}

	var raw map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": map[string]string{"message": err.Error(), "type": "parse_error"},
		})
		tracker.finish(false, http.StatusInternalServerError)
		return
	}
	if u, ok := raw["usage"].(map[string]any); ok && len(u) > 0 {
		usageFn(u)
	}
	chatOut := raw
	if data, ok := raw["data"]; ok {
		if d, ok := data.(map[string]any); ok {
			chatOut = d
		}
	}
	chatOut = normalizeOpenAIResponse(chatOut)
	anthropicResp := openAIToAnthropic(chatOut)
	if tc, ok := getNested(chatOut, "choices", 0, "message", "tool_calls").([]any); ok && len(tc) > 0 {
		anthropicResp["stop_reason"] = "tool_use"
	}
	writeJSON(w, http.StatusOK, anthropicResp)
	tracker.finish(true, resp.StatusCode)
}

func handleAnthropicStreamWithUsage(w http.ResponseWriter, upstream *http.Response, modelName string, toolSchemas map[string]map[string]bool, onUsage func(map[string]any)) {
	log.Printf("  anthropic stream: starting real-time forward")
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	setCORSOrigin(w)
	w.WriteHeader(http.StatusOK)
	flusher, ok := w.(http.Flusher)
	if !ok {
		return
	}

	// 流式诊断日志改走共享、带轮转上限的 writeStreamLog(见其定义), 不再每请求独占句柄。
	emit := func(event string, data any) {
		d, _ := json.Marshal(data)
		line := fmt.Sprintf("event: %s\ndata: %s\n\n", event, string(d))
		w.Write([]byte(line))
		writeStreamLog(line)
		flusher.Flush()
	}

	msgID := "msg_" + fmt.Sprintf("%x", time.Now().UnixMilli())
	stopReason := "end_turn"
	emit("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":      msgID,
			"type":    "message",
			"role":    "assistant",
			"content": []any{},
			"model":   modelName,
			"usage": map[string]any{
				"input_tokens":  0,
				"output_tokens": 0,
			},
			"stop_reason": nil,
		},
	})

	textIndex := new(int)
	*textIndex = -1
	hasText := false
	thinkingIndex := new(int)
	*thinkingIndex = -1
	hasThinking := false
	pendingTools := map[int]*toolAccumulator{}
	emitIndex := 0
	nextIndex := func() int {
		i := emitIndex
		emitIndex++
		return i
	}

	emitToolBlock := func(acc *toolAccumulator) {
		acc.emitted = true
		if acc.name == "" {
			log.Printf("  tool_use missing name, skipping (id=%s)", acc.id)
			return
		}
		idx := nextIndex()
		id := acc.id
		if id == "" {
			id = fmt.Sprintf("toolu_%x_%d", time.Now().UnixMilli(), idx)
			log.Printf("  tool_use missing id, generated %s", id)
		}
		argsObj, err := parseToolArgs(acc.args)
		if err != nil {
			log.Printf("  tool args parse failed for %s: %v (raw: %s)", acc.name, err, kit.Truncate(acc.args, 300))
			argsObj = map[string]any{}
		}
		if inputMap, ok := argsObj.(map[string]any); ok {
			argsObj = filterToolInput(acc.name, inputMap, toolSchemas)
		}
		parsed, _ := json.Marshal(argsObj)
		log.Printf("  tool_use emit: name=%s id=%s input=%s", acc.name, id, string(parsed))
		emit("content_block_start", map[string]any{
			"type":  "content_block_start",
			"index": idx,
			"content_block": map[string]any{
				"type":  "tool_use",
				"id":    id,
				"name":  acc.name,
				"input": map[string]any{},
			},
		})
		emit("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": idx,
			"delta": map[string]any{
				"type":         "input_json_delta",
				"partial_json": string(parsed),
			},
		})
		emit("content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": idx,
		})
	}

	processSSELine := func(line string) {
		line = strings.TrimRight(line, "\r\n")
		if !strings.HasPrefix(line, "data:") {
			return
		}
		payload := strings.TrimSpace(line[5:])
		if payload == "" || payload == "[DONE]" {
			return
		}

		var obj map[string]any
		if err := json.Unmarshal([]byte(payload), &obj); err != nil {
			return
		}
		if onUsage != nil {
			if u, ok := obj["usage"].(map[string]any); ok && len(u) > 0 {
				onUsage(u)
			}
		}
		if data, ok := obj["data"]; ok {
			if d, ok := data.(map[string]any); ok {
				obj = d
			}
		}

		if errPayload, ok := obj["error"]; ok {
			errBody, _ := json.Marshal(errPayload)
			log.Printf("  upstream SSE error: %s", string(errBody))
			emit("error", map[string]any{"type": "error", "error": errPayload})
			return
		}

		choices, _ := getNested(obj, "choices").([]any)
		if len(choices) == 0 {
			return
		}
		choice, _ := choices[0].(map[string]any)
		if choice == nil {
			return
		}

		delta, ok := choice["delta"].(map[string]any)
		if !ok {
			delta = choice
		}

		if c, ok := delta["content"].(string); ok && c != "" {
			if !hasText {
				hasText = true
				*textIndex = nextIndex()
				emit("content_block_start", map[string]any{
					"type":  "content_block_start",
					"index": *textIndex,
					"content_block": map[string]any{
						"type": "text",
						"text": "",
					},
				})
			}
			emit("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": *textIndex,
				"delta": map[string]any{
					"type": "text_delta",
					"text": sanitizeContent(c),
				},
			})
		}

		// reasoning_content -> thinking block (reasoning 透传,
		// borrowed from hayou2002/clinepass-proxy: CherryStudio 等客户端
		// 依赖 thinking 块显示思考过程).
		if rc, ok := delta["reasoning_content"].(string); ok && rc != "" {
			if !hasThinking {
				hasThinking = true
				*thinkingIndex = nextIndex()
				emit("content_block_start", map[string]any{
					"type":  "content_block_start",
					"index": *thinkingIndex,
					"content_block": map[string]any{
						"type":     "thinking",
						"thinking": "",
					},
				})
			}
			emit("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": *thinkingIndex,
				"delta": map[string]any{
					"type":     "thinking_delta",
					"thinking": rc,
				},
			})
		}

		if tcRaw, ok := delta["tool_calls"].([]any); ok {
			for _, tc := range tcRaw {
				tcMap, _ := tc.(map[string]any)
				if tcMap == nil {
					continue
				}
				idx := 0
				if i, ok := tcMap["index"].(float64); ok {
					idx = int(i)
				}
				acc, exists := pendingTools[idx]
				if !exists {
					acc = &toolAccumulator{index: idx}
					pendingTools[idx] = acc
				}
				if id, ok := tcMap["id"].(string); ok && id != "" {
					acc.id = id
				}
				if fn, ok := tcMap["function"].(map[string]any); ok {
					if name, ok := fn["name"].(string); ok && name != "" {
						acc.name = name
					}
					if args, ok := fn["arguments"].(string); ok && args != "" {
						acc.args += args
					} else if argsRaw, ok := fn["arguments"]; ok && argsRaw != nil {
						if bts, err := json.Marshal(argsRaw); err == nil {
							acc.args = string(bts)
						}
					}
				}
			}
		}

		if fr, ok := choice["finish_reason"].(string); ok && fr != "" {
			switch fr {
			case "length":
				stopReason = "max_tokens"
			case "tool_calls":
				stopReason = "tool_use"
			}
		}
	}

	reader := bufio.NewReader(upstream.Body)

	for {
		line, err := reader.ReadString('\n')
		if line != "" {
			processSSELine(line)
		}
		if err != nil {
			break
		}
	}

	// Stop text block if active
	if hasText {
		emit("content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": *textIndex,
		})
	}

	// Stop thinking block if active
	if hasThinking {
		emit("content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": *thinkingIndex,
		})
	}

	// Emit any remaining un-emitted tool blocks
	for _, acc := range pendingTools {
		if !acc.emitted {
			emitToolBlock(acc)
		}
	}

	emit("message_delta", map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason":   stopReason,
			"stop_sequence": nil,
		},
		"usage": map[string]any{
			"output_tokens": 0,
		},
	})

	emit("message_stop", map[string]any{"type": "message_stop"})
	log.Printf("  anthropic stream done: hasText=%v tools=%d reason=%s", hasText, len(pendingTools), stopReason)
}

func normalizeOpenAIResponse(obj map[string]any) map[string]any {
	out := make(map[string]any)
	for k, v := range obj {
		if k == "provider_metadata" || k == "proxy_metadata" {
			continue
		}
		out[k] = v
	}

	if choices, ok := out["choices"].([]any); ok {
		normalized := make([]any, 0, len(choices))
		for _, ch := range choices {
			if c, ok := ch.(map[string]any); ok {
				nc := make(map[string]any)
				for k, v := range c {
					if k == "provider_metadata" || k == "proxy_metadata" {
						continue
					}
					nc[k] = v
				}
				if msg, ok := nc["message"].(map[string]any); ok {
					nc["message"] = normalizeMessage(msg)
				}
				if delta, ok := nc["delta"].(map[string]any); ok {
					nd := make(map[string]any)
					for k, v := range delta {
						if k == "provider_metadata" || k == "proxy_metadata" {
							continue
						}
						nd[k] = v
					}
					if tc, ok := nd["tool_calls"].([]any); ok && len(tc) > 0 {
						repairToolCallDeltas(tc)
						if nd["content"] == nil {
							nd["content"] = ""
						}
					}
					nc["delta"] = nd
				}
				normalized = append(normalized, nc)
			} else {
				normalized = append(normalized, ch)
			}
		}
		out["choices"] = normalized
	}

	return out
}

func sanitizeContent(s string) string {
	return s
}

func normalizeMessage(msg map[string]any) map[string]any {
	out := make(map[string]any)
	for k, v := range msg {
		if k == "provider_metadata" || k == "proxy_metadata" {
			continue
		}
		out[k] = v
	}
	if tc, ok := out["tool_calls"].([]any); ok && len(tc) > 0 {
		// 部分开源/免费模型返回的 tool_calls 缺 id, 客户端会整体报错
		// "tool_calls without a complete id and function name", 在出口处修好
		repaired, _ := repairToolCalls(tc)
		out["tool_calls"] = repaired
		if out["content"] == nil {
			out["content"] = ""
		}
	}
	if c, ok := out["content"].(string); ok {
		out["content"] = sanitizeContent(c)
	}
	return out
}

// genToolCallID 生成 OpenAI 风格的 tool_call id。
func genToolCallID() string { return "call_" + kit.RandHex(12) }

// repairToolCalls 修补非流式响应里的 tool_calls, 返回修补后的条目与剔除数。
//
// 客户端(Cline 类 agent 工具)对完整性有硬校验: id 与 function.name 缺一即
// 整体报 "Model provider returned tool_calls without a complete id and
// function name"。实测部分 free 模型只回 function.name 不回 id —— 补一个
// 随机 id 就是完全合法的调用; 连 name 都没有的条目客户端无法执行, 只能剔除。
func repairToolCalls(tcs []any) ([]any, int) {
	out := make([]any, 0, len(tcs))
	dropped := 0
	for _, raw := range tcs {
		tc, ok := raw.(map[string]any)
		if !ok {
			dropped++
			continue
		}
		fn, _ := tc["function"].(map[string]any)
		name := ""
		if fn != nil {
			name, _ = fn["name"].(string)
		}
		if strings.TrimSpace(name) == "" {
			dropped++ // 没有 name 的调用无法执行
			continue
		}
		if id, _ := tc["id"].(string); strings.TrimSpace(id) == "" {
			tc["id"] = genToolCallID()
		}
		if t, _ := tc["type"].(string); t == "" {
			tc["type"] = "function"
		}
		if fn["arguments"] == nil || fn["arguments"] == "" {
			fn["arguments"] = "{}"
		}
		out = append(out, tc)
	}
	return out, dropped
}

// repairToolCallDeltas 流式 delta 的保守修补: 起始块(function.name 非空)缺 id
// 时补一个并确保 type; 续流块(name 为空、只带 arguments 分片)保持原样 ——
// 客户端按 index 累积分片, 乱动续流块会把流弄坏。
func repairToolCallDeltas(tcs []any) {
	for _, raw := range tcs {
		tc, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		fn, _ := tc["function"].(map[string]any)
		if fn == nil {
			continue
		}
		name, _ := fn["name"].(string)
		if strings.TrimSpace(name) == "" {
			continue
		}
		if id, _ := tc["id"].(string); strings.TrimSpace(id) == "" {
			tc["id"] = genToolCallID()
		}
		if t, _ := tc["type"].(string); t == "" {
			tc["type"] = "function"
		}
	}
}

func getNested(obj map[string]any, keys ...any) any {
	current := any(obj)
	for _, key := range keys {
		switch k := key.(type) {
		case string:
			if m, ok := current.(map[string]any); ok {
				current = m[k]
			} else {
				return nil
			}
		case int:
			if arr, ok := current.([]any); ok && k < len(arr) {
				current = arr[k]
			} else {
				return nil
			}
		default:
			return nil
		}
	}
	return current
}

// freePort 启动前清理占着目标端口的「旧实例」。
//
// 此前这里是无条件的 Stop-Process: 只要端口被占, 就把占用者的进程强杀, 既没有
// 任何身份校验也没有提示 —— 用户机器上恰好用该端口的无关程序会被静默干掉。
// 现在只清理与当前可执行文件同名的进程(即另一个 cline-proxy), 遇到陌生进程
// 如实记录后放手, 让 ListenAndServe 用 "address already in use" 明确报错。
func freePort(port int) {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	if !portInUse(addr) {
		return // port is free
	} else if runtime.GOOS != "windows" {
		// 非 Windows 没有可靠的「按端口找进程」手段。旧实现在这里会去执行
		// 根本不存在的 powershell, 失败后仍进入 5 秒重试循环, 而每次都能
		// 拨通旧进程 —— 等于白等 5 秒才失败。直接交给监听报错更清楚。
		return
	}
	if !killOwnProcessOnPort(port) {
		return
	}
	// 杀进程后确认端口确实释放，避免旧进程尚未退出时立刻竞争监听。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !portInUse(addr) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	log.Printf("  端口 %d 仍被占用, 监听可能失败", port)
}

// portInUse 目标地址当前是否有进程在监听。
func portInUse(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// selfProcessName 当前可执行文件名(去扩展名), 用于识别「自己人」。
func selfProcessName() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return strings.TrimSuffix(filepath.Base(exe), filepath.Ext(exe))
}

// killOwnProcessOnPort 只终止与自身同名的进程占用的端口, 返回是否杀掉过进程。
func killOwnProcessOnPort(port int) bool {
	self := selfProcessName()
	if self == "" {
		return false
	}
	// 只列 Listen 态连接, 并带上进程名, 供下面按名字过滤。
	script := fmt.Sprintf(
		`$p=Get-NetTCPConnection -LocalPort %d -State Listen -ErrorAction SilentlyContinue; `+
			`if($p){ $p.OwningProcess | Sort-Object -Unique | ForEach-Object { `+
			`$proc=Get-Process -Id $_ -ErrorAction SilentlyContinue; `+
			`if($proc){ Write-Output "$($proc.Id) $($proc.ProcessName)" } } }`, port)
	out, err := kit.ExecCommand("powershell", "-NoProfile", "-Command", script).Output()
	if err != nil && len(bytes.TrimSpace(out)) == 0 {
		log.Printf("  端口 %d 被占用, 但无法枚举占用进程: %v", port, err)
		return false
	}

	selfPID := strconv.Itoa(os.Getpid())
	killed := false
	var foreign []string
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		pid, name := fields[0], fields[1]
		if !strings.EqualFold(name, self) {
			foreign = append(foreign, name+"(pid "+pid+")")
			continue
		}
		if pid == selfPID {
			continue // 绝不自杀
		}
		stop := kit.ExecCommand("powershell", "-NoProfile", "-Command",
			fmt.Sprintf("Stop-Process -Id %s -Force -ErrorAction SilentlyContinue", pid))
		if err := stop.Run(); err == nil {
			killed = true
			log.Printf("  已终止占用端口 %d 的旧实例 %s(pid %s)", port, name, pid)
		}
	}
	if len(foreign) > 0 {
		log.Printf("  端口 %d 被无关进程占用, 不会强杀: %s", port, strings.Join(foreign, ", "))
	}
	return killed
}

// parseInferenceCapDuration 从 Cline 429 错误体中解析 "Try again in 17h 59m" 形式的等待时长。
// 支持 "17h 59m"、"17h"、"59m"、"30s"、"1d 2h 30m" 等组合。
func parseInferenceCapDuration(body string) time.Duration {
	// 在错误体中查找 "Try again in ..." 子串
	idx := strings.Index(body, "Try again in")
	if idx < 0 {
		return 0
	}
	rest := body[idx+len("Try again in"):]
	// 截取到下一个引号或换行
	end := len(rest)
	if i := strings.IndexAny(rest, "\"\n\r}"); i >= 0 {
		end = i
	}
	segment := strings.TrimSpace(rest[:end])
	return parseHumanDuration(segment)
}

// parseHumanDuration 解析 "17h 59m" / "2h" / "59m" / "30s" / "1d 2h" 之类的时长。
func parseHumanDuration(s string) time.Duration {
	if s == "" {
		return 0
	}
	var total time.Duration
	num := 0
	valid := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
			num = num*10 + int(c-'0')
			valid = true
		case c == 'd':
			total += time.Duration(num) * 24 * time.Hour
			num, valid = 0, false
		case c == 'h':
			total += time.Duration(num) * time.Hour
			num, valid = 0, false
		case c == 'm' && i+1 < len(s) && s[i+1] == 's':
			total += time.Duration(num) * time.Millisecond
			num, valid = 0, false
			i++
		case c == 'm':
			total += time.Duration(num) * time.Minute
			num, valid = 0, false
		case c == 's':
			total += time.Duration(num) * time.Second
			num, valid = 0, false
		case c == ' ':
			// 分隔符
		default:
			// 未知字符，重置
			num, valid = 0, false
		}
	}
	if total <= 0 {
		return 0
	}
	_ = valid
	return total
}

// parseRetryAfter 解析 HTTP Retry-After 头（秒数或 HTTP 日期）。
func parseRetryAfter(header string) time.Duration {
	header = strings.TrimSpace(header)
	if header == "" {
		return 0
	}
	// 尝试秒数
	if secs, err := parseIntSafe(header); err == nil {
		return time.Duration(secs) * time.Second
	}
	// 尝试 HTTP 日期
	if t, err := http.ParseTime(header); err == nil {
		d := time.Until(t)
		if d > 0 {
			return d
		}
	}
	return 0
}

func parseIntSafe(s string) (int, error) {
	var n int
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, fmt.Errorf("not a number")
		}
		n = n*10 + int(s[i]-'0')
	}
	return n, nil
}
