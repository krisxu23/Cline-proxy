# 多供应商免费模型路由设计（free-router 机制移植）

日期：2026-09-11
状态：已确认（用户批准设计后撰写）
参考项目：https://github.com/www222fff/free-router（Node.js 20，零 npm 依赖）

## 1. 背景与目标

Cline-proxy 目前的上游是三类硬编码实现：opencode zen（含节点池出口）、cline 账号池、ClinePass；
`routeModel` 提供两级故障转移（zen → cline 池，带 3 次/5 分钟熔断）。

free-router 提供一组互补机制：插件式 OpenAI 兼容 provider 注册表、`free-best` 有序候选链、
按错误类别分级的冷却、每日配额账本、免费模型自动发现（discovery + 评估试跑）。

目标：把 free-router 的机制完整移植为 Go 原生子系统，纳入现有网关，使四家 provider
（gemini / openrouter / tokenrouter / bai）的免费模型与 zen / cline 上游在同一条候选链中
统一路由、统一冷却、统一计量。保持单 exe 发行与现有行为兼容。

用户确认的范围决定：
- 四家 provider 全接入（含 Gemini 特殊处理全套）
- `free-best` 与现有 zen→cline failover 整合为统一候选链（现有行为成为隐式默认链）
- discovery 完整移植（含评估试跑），手动配置候选链永远兜底

## 2. 范围

**纳入**：上游抽象、统一候选链、分级冷却、每日配额账本、discovery、Gemini 三件套
（方言归一化 / thought-signature / 配额解析）、admin UI 扩展、测试。

**不纳入**：free-router 的 .env 机制（沿用配置文件 + admin UI）、`socksFirstHosts`
（节点池机制覆盖）、Docker / start.sh（单 exe 项目）。

## 3. 架构总览

```
客户端请求 (OpenAI/Anthropic 形状)
  → 模型名解析
      ├─ "provider:model" 前缀 → 单候选直选
      ├─ 路由别名 (free-best 等) → 展开有序候选链
      └─ 其他 → 现有默认链 (zen → cline 池), 行为不变
  → 候选遍历: 跳过 [冷却中 | 配额尽 | 未配置]
  → 上游分发
      ├─ zen      → 节点池出口 + 地区受限模型标记 (现有)
      ├─ cline    → 账号轮询 + 账号冷却 (现有)
      └─ provider → genericProvider Chat (新增)
  → 错误分类 → 候选层冷却 + 配额记账 → 下一站
  → 全链失败: 返回最后一站错误 (状态码透传)
```

冷却分四层，互不干扰：

| 层 | key | 现状 |
|---|---|---|
| 账号层 | cline 账号 | 现有（429 解析 Retry-After，默认 18h） |
| 出口层 | 节点/代理 | 现有（节点冷却 2min、连通可达性过滤） |
| 熔断层 | zen 全局 | 现有（3 次/5min） |
| **候选层** | `upstream:model` | **本次新增** |

## 4. 模块设计

### 4.1 上游抽象 — `internal/app/providers.go`（新，约 500 行）

```go
type Upstream interface {
    Name() string
    Configured() bool                  // 无 key 不参与候选
    IsFree(modelID string) bool        // 免费判定
    RefreshCatalog(ctx context.Context) error
    Chat(ctx context.Context, body map[string]any, stream bool) (*http.Response, error)
}
```

- `genericProvider` 实现：`baseUrl / apiKey / headers / chatPath / modelsUrl / modelsKeyHeader`
  可配置；免费判定三模式对应 free-router 的 `catalog`（按价格 `isZeroCost` + `isChatModel`）、
  `static`（`freeModels` 白名单）、`static+catalog`（白名单 + catalog 校验未下架）。
- catalog 拉取：分页跟完（最多 10 页），OpenAI 形状直接用；Google native 方言
  （`models[]`、`supportedGenerationMethods`、无价格）归一化为统一形状，
  `chatCapable = generateContent`，`x-goog-api-key` 头携带 key（不走 Bearer，避免 401）。
- 模型 slug 归一化：`normalizeModelSlug`（剥 `:free` 后缀、取最后一段路径），用于跨
  provider 同名模型合并与 `offeringsForSlug` 查找。
- **Gemini thought-signature**（thought-signature.mjs 移植）：LRU 签名缓存（5000 条）；
  非流式响应与 SSE 流（按行解析 `data:` 负载）中提取 `tool_calls[].extra_content.google.thought_signature`
  并按 `call.id` 记忆；发送前注入历史 assistant 消息缺失的签名，回填顺序为
  缓存查找 → `skip_thought_signature_validator` 哨兵。仅对 baseUrl 匹配
  `generativelanguage.googleapis.com` 的 provider 生效。
- **Gemini 配额解析**（quota.mjs 移植）：429 时把响应 message 的
  `Quota exceeded for metric: X, limit: N` 与 `details[].QuotaFailure.violations[].quotaId`
  按指标配对，区分：全部免费层 limit=0 → 该模型无免费层（永久剔除）；存在
  `PerDay` 且 limit>0 的耗尽 → 冷却到重置点（Google 免费层配额在太平洋时间
  `America/Los_Angeles` 午夜重置，与 §4.4 账本日界的本地时区是两个独立时区）；
  `RetryInfo` 解析 retryDelay。
- **probeFreeTier**：无定价 catalog 的 provider 可选开启——对白名单模型直接发一次
  最小请求探测是否免费层可用（仅对未绑定计费的 key 安全，opt-in，默认关）；
  探测返回 429 且解析为"无免费层"→ 永久剔除。
- **永久拒绝缓存**：404（"no longer available" / catalog 在但此处不提供）、
  400（"only supports Interactions API"）→ 记入永久剔除集合，不再消耗试跑与候选位。
- 配置：`.zen-config.json` 扩展 `providers` 段（见 §5.1），key 经 admin UI 保存，
  沿用现有配置持久化与脱敏（日志中 key 打码）。

### 4.2 统一候选链 — 改造 `zen.go` 的 `routeModel`（约 250 行）

- 候选结构 `{Upstream string, Model string}`，`Upstream ∈ {"zen","cline"} ∪ provider 名`。
- 解析优先级：
  1. `provider:model` 前缀且 provider 存在 → 单候选直选；
  2. 模型名命中路由别名（`routes` 段，如 `free-best`）→ 展开候选链：
     手动链在前（顺序不动），discovery 收录模型按用量权重排序追加尾部。
     `routes` 未配置该别名时的默认链：按 provider 配置声明顺序收录其全部免费
     模型（定价 catalog 者按 catalog、白名单者按白名单顺序），之后追加发现模型；
     无任何已配置 provider 时请求 `free-best` 返回 400 与明确错误说明；
  3. 其余 → 现有默认链：zen 模型走 zen，失败且 `clinePoolReady()` 切 cline 池
     （现行为原样保留，成为隐式默认项）。
- 遍历：候选满足任一跳过条件（候选层冷却中 / 当日配额尽 / provider 未配置 /
  永久拒绝）则跳过；失败按 §4.3 分类记冷却并前进；全部失败返回最后一站错误，
  沿用 `zenUpstreamError`/`zenErrorStatus` 泛化后的状态码透传。
- zen 候选经 `callZenAPI`（ctx 已带 `ctxKeyZenModel`，地区受限模型节点标记继续生效）；
  cline 候选经 `callClineAPI`；provider 候选经 genericProvider。
- 流式：候选链中每一站均支持 SSE 直通；某站失败发生在首字节前才允许 failover，
  已开始输出则终止并返回该站错误（与现有语义一致）。

### 4.3 候选层分级冷却 — `internal/app/cooldown.go`（新，约 150 行）

| 类别 | 触发 | 默认时长 |
|---|---|---|
| rateLimit | 429（含 Retry-After 解析） | 10 min |
| timeout | 超时/网络中断 | 5 min |
| serverError | 5xx | 2 min |
| empty | 200 但无内容 | 5 min |
| notFound | 404 | 1 h |
| forbidden | 403 | 1 h |
| quotaDay | Gemini 当日免费额度耗尽 | 至当日重置点（provider 时区） |
| permanent | 无免费层 / 已下架 / 非.chat 模型 | 永久（进程内 + 落盘） |

- key = `upstream:model`；时长可在 `cooldownMs` 段覆盖（`quotaDay`/`permanent` 除外）。
- 现有三层冷却（账号/出口/熔断）保持不动。

### 4.4 每日配额账本 — `internal/app/usage.go`（新，约 200 行）

- 维度 `upstream:model` → 当日请求数 / 成功 / 失败；内存 + 落盘
  `data/usage-ledger.json`；7 天滚动清理；日界按时区计算（默认 `Asia/Shanghai`，可配）。
- `dailyLimits` 命中 → 该候选当日跳过（等价冷却到日界）。
- Gemini 429 解析出的 `dailyRequestLimit` 可自动补全限额（优先级低于手动配置）。
- 读写锁保护；落盘节流（脏标记 + 定时 flush + 退出 flush）。

### 4.5 Discovery — `internal/app/discovery.go`（新，约 300 行）

- 定时（默认 48h，可配）：拉取 discovery provider（默认 openrouter）catalog →
  `isZeroCost + isChatModel` → 排除规则（模型名 pattern + 描述文本 pattern，
  自 free-router config 移植）→ 与已收录集合差集 = 待评估新模型。
- 评估试跑：每轮最多 8 个、单请求 maxTokens 4000；成功（HTTP 200）且响应含
  至少一个 choice 的非空 `content` 或 `tool_calls` → 收录；
  `permanentRejection` → 剔除并缓存；其余错误 → 本轮跳过，下轮再试。
- 收录持久化 `data/discovered-free-models.json`（模型、provider、收录时间、最近试跑结果）。
- 手动配置兜底：候选链展开时手动条目永远在前且不被删除/插队。

### 4.6 Admin UI — `admin_html.go` 扩展（约 150 行）

- 上游配置页新增 Provider 区块：每 provider 的 baseUrl / key / 白名单编辑、
  catalog 状态（模型数 / 免费数 / 最后刷新 / 错误）、连通测试按钮。
- 用量面板：今日各 `upstream:model` 用量 vs dailyLimits。
- 路由展示：`free-best` 当前实际顺序（手动段 + 发现段标注）。

### 4.7 错误处理与状态码

- 上游错误统一为 `upstreamError{Upstream, Status, Body}`（由 `zenUpstreamError` 泛化）；
  `upstreamErrorStatus(err)`：上游 4xx 原样透传，网络错误/5xx → 502。
- 候选链失败：返回最后一站的错误；请求日志记录实际命中的站与跳过原因（冷却/配额）。

### 4.8 兼容性

- 现有 `/models` 列表继续以 zen 模型为主体；provider 免费模型以
  `provider:model` 形式并入列表（`owned_by` = provider 名）。
- 不带前缀且不在路由表的模型名 → 现行为（zen 优先）。
- `routeModel` 现有调用点（/chat/completions 与 anthropic 两个入口）改为调用新链。

## 5. 数据格式

### 5.1 `.zen-config.json` 扩展段

```json
{
  "providers": {
    "gemini":     { "baseUrl": "https://generativelanguage.googleapis.com/v1beta/openai", "apiKey": "<key>",
                    "catalog": true, "pricing": false, "probeFreeTier": true,
                    "modelsUrl": "https://generativelanguage.googleapis.com/v1beta/models",
                    "modelsKeyHeader": "x-goog-api-key",
                    "freeModels": ["gemini-3.8-flash", "gemini-3.7-flash", "gemini-3.5-flash-lite", "gemini-3.1-flash-lite"] },
    "openrouter": { "baseUrl": "https://openrouter.ai/api/v1", "apiKey": "<key>", "catalog": true, "pricing": true,
                    "headers": { "HTTP-Referer": "${origin}", "X-Title": "Cline Proxy" } },
    "tokenrouter":{ "baseUrl": "https://api.tokenrouter.com/v1", "apiKey": "<key>", "catalog": true, "pricing": false,
                    "freeModels": ["z-ai/glm-5.3-free"] },
    "bai":        { "baseUrl": "https://api.b.ai/v1", "apiKey": "<key>",
                    "freeModels": ["glm-5.3-flash", "deepseek-v4-flash", "qwen3.8-flash", "hy3", "mimo-v2.5"] }
  },
  "routes": { "free-best": ["gemini:gemini-3.8-flash", "zen:mimo-v2.5-free", "cline:*"] },
  "cooldownMs": { "rateLimit": 600000, "timeout": 300000, "serverError": 120000,
                  "empty": 300000, "notFound": 3600000, "forbidden": 3600000 },
  "usage": { "retentionDays": 7, "timezone": "Asia/Shanghai",
             "dailyLimits": { "gemini:gemini-3.8-flash": 20 } },
  "discovery": { "enabled": true, "provider": "openrouter", "intervalMs": 172800000,
                 "maxPerRun": 8, "evalMaxTokens": 4000, "usageWeight": 12,
                 "exclude": {
                   "modelPatterns": ["[-_]tts(?:$|[-_])", "[-_]image(?:$|[-_])", "(?:^|[:/])nano-banana",
                                     "(?:^|[:/])lyria", "[-_]transcribe(?:$|[-_])", "robotics",
                                     "computer-use", "deep-research", "(?:^|[:/])antigravity",
                                     "[-_]latest$"],
                   "textPatterns": ["\\b(finance|financial|investment|medicine|medical|healthcare|health|clinical|biomedical|pharmaceutical|legal|accounting|tax)[\\s-]*(focused|specific|specialized|specialised|domain)\\b",
                                    "\\bdomain-(specific|specialized|specialised)\\b"]
                 } }
}
```

`<key>` 由 admin UI 填写后写入配置文件（日志脱敏）。`headers` 值支持 `${origin}`
占位（运行时展开为本地网关地址）。

`cline:*` 表示"cline 池当前可用模型"占位（沿用池内轮询）。

### 5.2 运行时持久化

- `data/usage-ledger.json`：`{"days": {"2026-09-11": {"gemini:gemini-3.8-flash":
  {"req": 12, "ok": 10, "fail": 2}}}}`
- `data/discovered-free-models.json`：`{"models": [{"provider": "openrouter",
  "model": "x/y:free", "addedAt": "2026-09-11T00:00:00+08:00",
  "lastProbe": "2026-09-11T00:00:00+08:00", "ok": true}]}`
- 永久拒绝缓存随 discovered 文件同存（`rejected` 数组，含原因）。

## 6. 测试

- `providers_test.go`：Google 方言归一化、isFree 三模式、catalog 分页、slug 合并、
  thought-signature 提取/注入/哨兵、Gemini 配额解析（无免费层 / 当日耗尽 / retryDelay）、
  permanentRejection 判定。
- `cooldown_test.go`：分类、过期、时长覆盖、quotaDay 到日界。
- `usage_test.go`：时区日界、7 天留存清理、dailyLimits 判定、Gemini 自动限额回填。
- 路由集成测试：前缀直选 / free-best 展开 / 冷却跳过 / 配额跳过 / 永久剔除 /
  全链失败透传最后一站错误 / 流式首字节前 failover。
- 现有测试（nodes_test.go 等）全部保持通过；构建保持
  `-tags "with_quic,with_grpc,with_utls"`。

## 7. 分期交付

每期独立可编译、可用、单独 commit 推送：

1. **第 1 期**：providers.go（含 Gemini 三件套）+ 配置扩展 + admin UI Provider 区块 +
   `provider:model` 前缀直选。交付即可手动挂四家 key 并直选模型。
2. **第 2 期**：统一候选链 + `free-best` 别名 + 候选层分级冷却 + upstreamError 泛化。
3. **第 3 期**：配额账本 + discovery（含评估试跑）+ UI 用量面板。

---

## 8. 交付状态

三期均已交付。

| 期 | 落地位置 | 状态 |
|---|---|---|
| 1 | `providers_config.go` / `providers_catalog.go` / `providers_chat.go` / `admin_providers.go` | 已交付 |
| 2 | `routing_chain.go`（候选解析与 `free-best`）、`routing_dispatch.go`（逐站 failover）、`cooldown.go`（候选层分级冷却） | 已交付 |
| 3 | `usage.go`（每日配额账本）、`discovery.go`（自动发现与评估试跑）、`admin_routing.go` + 面板「🔀 路由链与用量」 | 已交付 |

实现说明（与本文档的差异，均为落地时的取舍）：

- **时区**：单 exe 分发无法假设系统装有 zoneinfo，故内嵌 `time/tzdata`
  （见 `tzdata.go`）。Gemini 重置点用 `America/Los_Angeles`，账本日界用配置时区，
  两者仍是相互独立的时区。
- **默认链顺序**：`providers` 与 `freeModels` 在配置里分别是 map 与切片。
  provider 名取字典序、同 provider 内模型取模型名字典序展开，保证同一请求
  每次展开结果一致；白名单的书写顺序在上游已被 `freeModelIDs` 按名排序覆盖。
- **failover 时机**：候选链在拿到 `*http.Response` 并判定状态码（非流式还会
  校验响应确实带 content）之后才写响应。因此客户端只会看到胜出那一站的输出，
  不存在"已输出一部分再换站"的破损响应，流式也天然满足"首字节前 failover"。
- **`providerError` 归类**：通用 Provider 的 `Chat` 把非 200 直接返回为错误而非
  响应，故候选链必须从错误对象里取状态码，否则限流/下架会被误判为超时。
- **`usageWeight`**：解释为发现模型的排名窗口（天）——窗口内请求数越多越靠前。
- **`probeFreeTier`**：字段已落地，但试跑逻辑由 discovery 承担（第 3 期），
  未额外实现"对白名单模型逐个探测"的独立路径。

