# Cline-proxy 迭代后全量审查与修复记录

- 审查对象：`krisxu23/Cline-proxy` @ `e43d067`（审查时的 HEAD）
- 规模：25,316 行 Go / 82 个测试文件 / 105 个被跟踪文件
- 环境：Windows + Git Bash，Go 1.25.5（`go.mod` 声明 `go 1.25.5`，CI 用 1.26）
- 构建/测试标签：`with_quic,with_grpc,with_utls`（硬要求，见 `AGENTS.md`）

## 客观基线

| 项 | 审查前 | 修复后 |
|---|---|---|
| `go build ./...` | 通过 | 通过 |
| `go vet ./...` | **3 处 `unreachable code`**（`model_region.go:126/135/194`） | 干净 |
| `go test ./internal/...` | 全部通过 | 全部通过（新增回归测试） |
| `go test -race` | **本机无法运行**（无 gcc，`-race requires cgo`） | 同左，已加 CI `-race` job 兜底 |
| `gofmt -l` | 25 个文件报未格式化 | 同左，但确认为 CRLF 噪音，已加 `.gitattributes` |

测试全绿并不代表没问题 —— 下面 4 个 P0 里有 2 个（P0-1、P0-4）是**测试全绿状态下的功能性缺陷**，因为它们要么只在真实交互路径上触发，要么被上层重试掩盖。

---

## 一、三个根因

所有问题都可以归到三条根因上，单独打补丁会反复复发。

### 根因 A：配置对象没有单一写入口

`zenConfigData` 有 23 个字段，被 4 个以上 handler 各自「重建 / 原地改写」；而 `getZenConfig()` / `getProxyConfig()` 是「锁内返回裸指针、锁外被读」。

这必然导出两类 bug：**某个 handler 不知道某个字段存在 → 静默清空**；以及**读写并发访问同一张 map → 进程直接退出**。

### 根因 B：管理面被当成「本地可信 UI」设计

默认监听 `0.0.0.0`、`/admin/api/*` 零鉴权、所有管理接口无条件返回 `Access-Control-Allow-Origin: *`。三者叠加是一条完整可利用的攻击链，而「本地」这个前提在浏览器面前不成立。

### 根因 C：迁移只做了一半、没有收口

统一出口迁移把地区探测的**产出端** `return` 掉，**消费端**却全部保留；Dockerfile 的构建标签没跟 CI 同步；`gofmt` 检查因为行尾问题长期不可信。

---

## 二、P0（4 项，已全部修复）

### P0-1 保存 zen 设置会静默清空 7 个字段

`internal/app/admin_zen.go:96` 用字段字面量重建配置对象：

```go
next := &zenConfigData{
    Enabled: cur.Enabled, Key: cur.Key, BaseURL: cur.BaseURL,
    BaseURLs: cur.BaseURLs, Proxies: cur.Proxies, Subs: cur.Subs,
    ExitMode: cur.ExitMode, SubsRefreshMins: cur.SubsRefreshMins,
    ProxyStrategy: cur.ProxyStrategy, MaxConcurrency: cur.MaxConcurrency,
    Retries: cur.Retries, Failover: cur.Failover, FailoverCount: cur.FailoverCount,
    FailoverMinutes: cur.FailoverMinutes, Compaction: cur.Compaction,
    Providers: cur.Providers,   // ← 16/23 个字段
}
...
setZenConfig(next)              // 整份落盘
```

漏掉 7 个：`Routes`（候选链）、`Router`（自动路由）、`Usage`（每日配额账本）、`CooldownMs`、`DNSMode`、`DNSCustomDNS`、`RescueDirect`。

- 触发条件极低：在 zen 设置页改**任意一项**（例如重试次数 3 → 5）。
- 其中 `Routes` / `Router` 由 `admin_router.go:352-353` 写入，`Usage` 由 `usage.go:60` 读 —— 都是独立页面配置的，用户完全无法把「改重试次数」和「候选链消失」联系起来。
- `RescueDirect` 归 `nil` 会被「缺省视为 true」解释成**重新打开节点全挂时的直连兜底**，直接违背统一出口的既定方向。

**修复**：`next := cur.clone()`，只覆盖请求里显式出现的字段。新增 `internal/app/config_clone.go` 提供全套深拷贝。

**回归测试**：`internal/app/admin_zen_test.go::TestZenConfigUpdatePreservesUntouchedFields`。已验证该测试对旧实现**必然失败**（临时植入旧行为后 7 个字段全部报错）。

### P0-2 管理面零鉴权 × 默认 0.0.0.0 × CORS `*`

- `main.go:17` 默认 `0.0.0.0`；`admin.go:49-100` 全部 `/admin/api/*` 只包 `corsHandler`；`proxy.go:437-441` 无条件 `Access-Control-Allow-Origin: *`。
- `admin.go:1140-1158` `handleAccountsExport` 明文导出全部 `refreshToken`；`admin_zen.go:22` 回 `cfg.Key`；`admin_providers.go:49` 回 `cfg.APIKey`。

攻击链：用户浏览器打开任意网页 → 该页 `fetch('http://127.0.0.1:3457/admin/api/accounts/export')` → 因为 ACAO 是 `*`，响应体可读 → 账号完全接管。**防火墙拦不住，请求确实发自本机。**

**修复**：
- 新增 `internal/app/admin_auth.go`：访问令牌（首次启动生成、落盘 `data/admin-token` 0600、常量时间比较），支持 `X-Admin-Token` / `Authorization: Bearer` / `admin_token` Cookie。
- 管理接口改用 `adminAuth`，**不再返回任何 CORS 头**；另加 `Origin` 校验，公网来源直接 403。跨站页面即使猜到令牌也读不到响应体（同源策略）。
- 面板零改动：用带 `?token=` 的地址打开一次，服务端校验后种 HttpOnly Cookie，之后同源 fetch 自动携带。
- `main.go` / `StartProxy` 默认 host 改 `127.0.0.1`；启动横幅与托盘自动打开带令牌的地址，令牌同时写入 `data/cline-proxy.log`（GUI 构建下这是唯一可见通道）。

**自己写测试时抓到的顺序错误**：`Origin` 预检最初被放在「令牌校验失败的兜底分支」里，也就是令牌正确就直接放行、从不检查来源。这样令牌一旦外泄（带 `?token=` 的地址被分享、日志被翻），任何网页都能拿它直接读取管理接口。`TestAdminAuthRejectsPublicOrigin` 把它暴露出来，已把 Origin 检查前置到令牌校验之前 —— 它挡的是「浏览器里的第三方页面」，与令牌是否正确无关。

> 说明：令牌到位后，管理接口向已鉴权的同源面板返回 key 明文属于正常行为，**未做脱敏**（面板需要显示与复制）。这是有意识接受的取舍，不是遗漏。

### P0-3 配置读写竞态 → `fatal error` 直接崩进程

`getZenConfig()` / `getProxyConfig()` 锁内返回裸指针，锁随即释放；`mutateProvidersConfig` 原地改同一对象；`routing_chain.go:139` 无锁 `range cfg.Routes`、`proxy.go:629-632` `clineHeaders` **每个 cline 请求**都无锁遍历 `cfg.Headers`。

Go 运行时对「并发 map 读 + 写」抛的是 **fatal error，`recover` 无效**。windowsgui 构建下没有控制台，用户只会看到窗口凭空消失。

**修复**：getter 一律返回深拷贝；新增 `mutateProvidersConfig` / `mutateProxyConfig` 作为唯一写入口（锁内克隆 → 回调改克隆 → 整体替换 → 落盘）。
`headers_sync.go` 由「就地改克隆」改为走 `mutateProxyConfig` —— 就地改克隆不会影响全局，落盘会把改动丢掉，这是改 getter 语义后必须同步调整的隐性依赖。

**回归测试**：`TestGetZenConfigReturnsDetachedCopy`（逐层验证 map/slice/指针已脱钩）、`TestConcurrentConfigReadWrite`。

### P0-4 401 刷新 token 后复用已耗尽的请求体，补救请求必然失败

`proxy.go:701/716/729`：首次 `Do(req)` 之后 `bytes.Reader` 已读到 EOF 并被关闭；401 分支刷新 token 后**复用同一个 `req`** 再 Do 一次，transport 报 `http: ContentLength=N with Body length 0`。即 **token 刷新成功之后的补救请求 100% 失败**：单账号池直接 500，多账号池被外层换账号掩盖。同库 `providers_chat.go:204` 已为同一个坑留了注释。

**修复**：抽出 `newClineRequest(tok)` 闭包，每次发送重建请求；同时改用 `http.NewRequestWithContext` 并给 `callClineAPIFailover` 补 ctx。

---

## 三、P1（16 项，已全部修复）

| # | 问题 | 位置 | 修复 |
|---|---|---|---|
| 1 | 候选链流式成功分支不 Close 上游 body | `routing_dispatch.go:133-149` | 三个 handler 读完即补 `Close`（对比 87/154 两条失败路径本来就 Close 了） |
| 2 | 配置落盘非原子 + 吞掉 marshal 错误 | `pool.go:85` `zen.go:471` `sub.go:62` `usage.go` | 新增 `kit.WriteFileAtomicDefault`（tmp+fsync+rename）；marshal 失败**直接 return 不落盘**，避免写空文件清空账号池 |
| 3 | `freePort` 无条件强杀任意占用端口的进程 | `proxy.go:2275` | 取占用者进程名，**只清理与自己同名的旧实例**；陌生进程记录日志后放手；非 Windows 直接跳过（原先会执行不存在的 powershell 然后白等 5 秒） |
| 4 | 请求日志每请求起一个 goroutine 写同一文件 | `logs.go:45-64` | 改单写协程 + 带缓冲 channel；调用方永不阻塞（满则丢弃并计数）；轮转移入 writer 内消除截断竞争 |
| 5 | 入站请求体无上限 | 全部 `io.ReadAll(r.Body)` | `limitInboundBody` 包在最外层（`maxInboundBodyBytes` = 64 MiB），覆盖日志中间件对每个请求的读取 |
| 6 | `http.Server` 无任何超时 | `proxy.go:393` | 加 `ReadHeaderTimeout` 20s（Slowloris）、`IdleTimeout` 120s、`MaxHeaderBytes` 1 MiB。**WriteTimeout 仍不设** —— 会切断 LLM 长流，属刻意取舍 |
| 7 | 地区探测半迁移：产出端 stub、消费端全活 | `model_region.go:124/133/192` | 恢复启用探测链（实现本身完整，`return` 只是迁移期的临时开关）。修复 `go vet` 3 处 unreachable，并让 `zen.go:595` 的加宽重试与 `regionNodeSupport` 面板列恢复意义 |
| 8 | `esc()` 不转义引号却用于属性上下文 | `admin_html.go:800` + 20 余处 | 拆出 `escAttr`（属性值）与 `escJs`（内联事件里的 JS 字符串）；`a.status` 原先是连 `esc` 都没用的裸拼 |
| 9 | 订阅 / provider 地址无内网与 scheme 限制（SSRF） | `admin_zen.go` `providers_config.go` | 新增 `outbound_url.go`：强制 http/https，拒绝链路本地（含 `169.254.169.254` 云元数据）、未指定地址、CGNAT 段。**刻意不禁回环与私网** —— 本机/局域网跑 Ollama、LM Studio 是常见用法 |
| 10 | CI 只构建不测试不出静态检查 | `.github/workflows/build.yml` | 新增 `test` job（`go vet` + `go test` + ubuntu `-race`），`build` 改为 `needs: test` |
| 11 | Dockerfile 绕过硬性构建标签 | `Dockerfile` | 补 `-tags with_quic,with_grpc,with_utls -buildvcs=false`；去掉 `2>/dev/null \|\| true`；`CMD` 显式 `-host 0.0.0.0`（host 默认值改为 127.0.0.1 后容器否则不可达） |
| 12 | cline 上游无 ctx，客户端断开无法取消 | `proxy.go` `callClineAPI` | 签名补 `ctx`，6 个调用点同步；重试循环前检查 `ctx.Err()` |
| 13 | cline 错误无类型 → 候选分类错误、状态码被压成 502 | `proxy.go` | 非 200 返回 `*upstreamError`（`chainErrorStatusBody` 靠类型取状态）；chatHandler 改用 `upstreamErrorStatus(err)` 透传 4xx |
| 14 | `defaultModel` 被两把锁 + 无锁访问 | `pool.go:56/70` `models.go:214` | 统一由 `modelsMu` 保护；`admin.go` 残留的直接读改写为 `getDefaultModel()` |
| 15 | `ensureAccountToken` 无锁读账号字段 | `pool.go:200` | 在 `poolMu` 下取快照后再判断，避免与 `refreshAccountToken` 竞争 |
| 16 | Google Base URL 为 `/v1/...` 时拼出畸形端点；`isGoogleProvider` 用整条 URL 子串判断 | `providers_config.go:97/102` | 主机名精确匹配；任意 path 一律收敛到 `/v1beta/openai`（已是 `/openai` 的原样尊重） |

---

## 四、P2（已修复）

| 问题 | 位置 | 修复 |
|---|---|---|
| `pickZenProxyWhere` 整池不可用时返回冷却中的出口，与注释语义相反 | `proxy_pool.go:162` | 循环内记录 `found`；未找到即返回 `("", -1)` 交给上层。extra 分支本来就有正确写法，冷却与健康度两条漏了 |
| `clearExpiredCandidateCooldowns` 是死代码 → `candidateCools` 只增不减 | `cooldown.go:160` | 新增 `startCooldownJanitor`（10 分钟）并在 `StartProxy` 启动 |
| `generateSummary` 用 `context.Background()` 且无超时 | `compact.go:227` | ctx 贯穿 3 个 `maybeCompact` 调用点 + 90s 超时 |
| `responses.go` zen 分支不拆 `{data:{...}}`、流式 usage 丢失 | `responses.go:507-522` | 与 cline 分支对齐：拆包装 + 接 `zenStatsTracker` |
| `rebuildZenTransport` 不关旧 transport 空闲连接 | `proxy_pool.go:91` | `CloseIdleConnections()` 后再替换（`setZenConfig` 每次保存都会走到这里） |
| `providers/update` 整体替换 → 只传 `baseUrl` 就清空 apiKeys/models | `admin_providers.go` | `mergeProviderConfigPatch`：以现有配置为底，只覆盖请求里**出现过的键**（走 struct→map→覆盖→struct，天然与字段集合同步，避免再次漏字段） |
| 配置解析失败被静默部分采用 | `loadProxyConfig` / `loadZenConfig` | 解析失败整体退回默认值，并把坏文件改名为 `.bad-<时间戳>` 留证 |
| `stats.go` 文件打开失败后永久降级（`sync.Once` 已消费） | `stats.go:112` | 改可重试初始化，失败原因打日志 |
| `main.go` 只写 `-port` 时掉进 CLI 分支（windowsgui 下无窗口无输出） | `main.go:65` | Windows 默认桌面模式；新增 `-cli` 显式留在控制台 |
| `subsRefreshInterval` 注释说「夹回区间」实际回落到默认；`loadSubCache` 锁外读 `subNodes` | `sub.go` | 行为与注释对齐；长度在锁内取 |
| 缺少 `.gitattributes` → CRLF 让 `gofmt -l` 长期误报 25 个文件 | 仓库根 | 新增 `.gitattributes` 统一 LF |
| `admin.go` `log.Fatalf` 后跟 `os.Exit(1)` 死代码 | `main.go:71` | 删除 |

---

## 五、有意识不修的部分

| 项 | 原因 |
|---|---|
| 管理接口向已鉴权面板返回 key 明文 | 面板需要显示与复制。已由令牌 + 无 CORS 双重门禁；若改为脱敏需同步做「显示/复制」的独立接口 |
| `http.Server` 不设 `WriteTimeout` | 会切断 LLM 长流，属刻意取舍。改为设 `ReadHeaderTimeout` + `IdleTimeout` 已覆盖 Slowloris |
| 禁止回环/私网作为上游 | 本机或局域网跑 Ollama / LM Studio / vLLM 是常见用法，一刀切会误伤。只封链路本地与云元数据 |
| 部分 admin handler 无 Method 校验（如 `handleAdminGetKeys`） | 只读接口，误用 POST 无危害；且已在令牌之后 |
| 351 处被忽略的错误返回值 | 多数是 `f.Close()` / 日志写入一类的合理忽略；逐个改信噪比太低，本次只处理了会导致数据损坏的那些 |

---

## 六、验证方式

```bash
export PATH="/c/Go/bin:$PATH" GOROOT="C:\Go"     # 本机 Go 不在 PATH

go build -tags "with_quic,with_grpc,with_utls" ./...        # 通过
go vet   -tags "with_quic,with_grpc,with_utls" ./...        # 干净（此前 3 处 unreachable）
go test  -count=1 -tags "with_quic,with_grpc,with_utls" ./internal/...   # 全部通过
```

新增回归测试：

| 测试 | 锁住的行为 |
|---|---|
| `TestZenConfigUpdatePreservesUntouchedFields` | 保存 zen 设置不得清空 7 个未提交字段（已验证对旧实现必然失败） |
| `TestGetZenConfigReturnsDetachedCopy` | getter 返回的克隆体与全局逐层脱钩 |
| `TestConcurrentConfigReadWrite` | 并发读写配置不产生 data race |
| `TestPickZenProxyRefusesCooledDownExit` | 整池冷却时不得返回冷却中的出口 |
| 各 worker 补充 | 日志并发写入无坏行、`stats` 失败可恢复、原子写往返、`defaultModel` 并发、`subsRefreshInterval` 上下界 |

**本机无法验证**：`go test -race` 需要 cgo，本机没有 gcc（`-race requires cgo`）。所有竞态结论来自人工走查；已在 CI 增加 ubuntu `-race` job 作为运行时兜底。这一步是必须的，不能省。

**一处测试暴露出的历史包袱**：`TestExitModeDirectBypassesProxyPool` 单独跑通过、全量跑失败。原因是前序用例残留了按索引记录的出口冷却，而旧实现会照旧返回冷却中的出口，所以该用例一直是「靠缺陷碰巧通过」。已在用例内显式清理冷却表，并保留修复后的行为断言。
