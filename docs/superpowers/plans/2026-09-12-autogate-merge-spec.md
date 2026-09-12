# autogate 能力合并实施方案（供执行 agent 使用）

> 本文档是自包含的实施规格。执行 agent 无需本次对话上下文，按此文档即可完成移植、测试与交付。
> 目标仓库：`D:\deepseek\Cline-proxy`（远端 `krisxu23/Cline-proxy`）
> 源材料仓库：`krisxu23/opencode-free-autogate`（下称 autogate）
>
> **开工前必读**：仓库根目录的 `AGENTS.md`（GitHub 一律走 SSH 443、构建命令、
> Git Bash 路径陷阱）。本文与其冲突时以本文为准。

---

## 0. 背景与总目标

Cline-proxy 是多上游 LLM 网关（Go，单二进制，内嵌 sing-box）。autogate 是同作者的
另一个 opencode 专注网关，两者重叠约 70%。本次任务：**把 autogate 独有的 6 项能力
移植进 Cline-proxy，分 3 批交付**。不合并代码库（autogate 是 84KB 单体 `gateway.go`
+ 包级全局变量，直接搬文件无法编译），而是逐模块重写适配到 `internal/app` 结构。

移植后行为不变的部分：所有现有模型 ID 格式（`zen/xxx`、`provider:model`、
`cline-pass/xxx`、路由别名）、四层冷却、地区探测、候选链、管理面板既有功能。

---

## 1. 现网关架构速览（执行 agent 的坐标系统）

### 1.1 请求主路径

```
入口(3 个):
  proxy.go       POST /v1/chat/completions  (OpenAI)
  proxy.go       POST /v1/messages          (Anthropic)
  responses.go   POST /v1/responses         (Codex)
    ↓ 路由决策
  zen.go:routeModel(id)          → "zen" / "cline" / "reject"（zenRejectMessage）
  providers_chat.go:parseProviderModel(id) → "provider:model" 直选
  routing_chain.go:resolveRouteChain(id)   → 路由别名(auto-router/free-best)展开候选链
    ↓ 执行
  routing_dispatch.go:handleChainedChatAs(w, r, params, chain, requested, target)
    ├─ candidateSkip(cand)                候选可用性(冷却/配额/未配置/上游矩阵)
    ├─ callChainUpstream(ctx, cand, ...)  逐站调用, 失败 applyCandidateFailure 换下一站
    ├─ chatBodyHasContent(body)           200 空壳判失败
    └─ 胜出后回写: proxy.go 的
       handleNonStreamResponseWithUsage / handleStreamResponseWithUsage /
       handleAnthropicStreamWithUsage
    ↓ 拨号(唯一出口)
  proxy_pool.go:zenDialContext(ctx, network, addr)
    ├─ pickZenProxyForModel(modelID)   选出口(model_region.go, 地区受限模型特殊选路)
    ├─ dialNodeProxy / socks5_dial.go:dialSOCKS5(节点本地 mixed 入站)
    └─ 无节点 → catch-all 入站(nodes.go: catchAllInTag = "in-catchall")
```

### 1.2 关键文件与职责

| 文件 | 职责 |
|---|---|
| `internal/app/zen.go` | ZenModel/种子表/routeModel/handleZenChat(重试+地区处理)/目录同步 |
| `internal/app/zen_models_cache.go` | zen 同步模型持久化(刚加, 勿破坏) |
| `internal/app/proxy_pool.go` | zenDialContext 唯一拨号口/出口轮询/冷却/getZenHTTPClient/zenHTTP2Transport(h2 池!) |
| `internal/app/model_region.go` | 地区受限模型探测与选路(regionRestrictedSeed 含 muse-spark) |
| `internal/app/node_upstream.go` | 出口×上游可达性矩阵(TLS 握手级) |
| `internal/app/nodes.go` | sing-box 实例(buildNodeParts/syncNodeBox)/节点连通检测/TLS 配置修正 |
| `internal/app/socks5_dial.go` | SOCKS5 拨号(节点本地入站) |
| `internal/app/providers_*.go` | 通用 Provider(配置/目录/对话), catalogExitBudget, 免费判定 |
| `internal/app/routing_chain.go` / `routing_dispatch.go` | 候选链解析与调度 |
| `internal/app/cooldown.go` | 候选层分级冷却(8 类错误) |
| `internal/app/proxy.go` | 三个入口之一 + 响应回写 + normalizeMessage(含 repairToolCalls) |
| `internal/app/admin_html.go` | 内嵌管理面板(Go 原始字符串, 修改注意 div 配平) |
| `internal/app/logs.go` | 请求日志(requests.jsonl, X-Proxy-Route 头) |
| `internal/kit/` | `kit.RandHex` / `kit.WithRetryJitter` / `kit.FreshZenIdentity` / `kit.ResolveDataPath` / `kit.Truncate` / `kit.ReadBody` |

### 1.3 可复用的基础设施（移植时直接用，不要重造）

- 出口选择：`pickZenProxy()` / `pickZenProxyWhere(filter)` / `pickZenProxyForModel(modelID)`
- 冷却：`cooldownZenProxy(idx, d)` / `markCandidateCooldown(upstream, model, class, reason)`（8 类：rateLimit/timeout/serverError/empty/notFound/forbidden/quotaDay/permanent）
- 真实出口回读：`reqExitKey(ctx)`（拨号层写入该请求实际使用的出口）
- 会话/指纹：`kit.FreshZenIdentity()` 生成 session/request/UA
- 随机 ID：`kit.RandHex(n)`
- 数据落盘：`kit.ResolveDataPath(name)` → data 目录，写法参考 `zen_models_cache.go`（tmp+rename 原子写）
- 测试辅助：`withTestConfig(t, cfg)`（routing_chain_test.go）、`withZenModelsReset(t)`（zen_models_cache_test.go）

---

## 2. 源材料获取

`raw.githubusercontent.com` 在本机被墙（见 AGENTS.md 可达性表）。获取 autogate 源码用：

```bash
# 单文件(GitHub Contents API, 直连可用):
curl -s --noproxy '*' -H "Accept: application/vnd.github.raw" \
  "https://api.github.com/repos/krisxu23/opencode-free-autogate/contents/<path>" -o <local>

# 或整包(codeload 直连可用):
curl -sL --noproxy '*' -o ag.tar.gz \
  "https://codeload.github.com/krisxu23/opencode-free-autogate/tar.gz/refs/heads/main"
```

需要移植的源文件（括号内为字节数）：

| 源文件 | 大小 | 对应批次 |
|---|---|---|
| `bodyhygiene.go` / `bodyhygiene_test.go` | 9759 | 1 |
| `bodyorder.go` / `bodyorder_test.go` | 3574 | 1 |
| `ssehygiene.go` / `ssehygiene_test.go` | 5142 | 1 |
| `deepprobe.go` / `deepprobe_test.go` | 7772 | 1 |
| `modelhealth.go` | 6934 | 1 |
| `recoveryprobe.go` | 2746 | 1 |
| `absorb.go` / `absorb_test.go` | 8180 | 2 |
| `streamresume.go` / `streamresume_test.go` | 8647 | 2 |
| `holdback.go` / `holdback_test.go` | 5759 | 2 |
| `scoring.go` | 12935 | 2 |
| `iprep.go` / `iprep_test.go` | 23929 | 3 |
| `outage.go` / `outage_test.go` | 5268 | 3 |
| `wakedetect.go` | 1055 | 3 |

**不移植**：`gateway.go`（84KB 单体，只作行为参考）、`singbox.go`/`proxypool.go`/`mirrors.go`
（Cline-proxy 已有更强实现）、`providers.go`/`usage.go`（已有）、`ui_windows.go` 等 Windows
GUI（有网页面板）、`m365/` 整个目录（微软 Copilot 上游，超出本次范围）、`theme_windows.go`。

---

## 3. 第 1 批：请求体卫生 + SSE 保活心跳 + chat 深检

### 3.1 请求体卫生（新文件 `internal/app/body_hygiene.go`）

**目标**：客户端发来的畸形请求在转发前自动修复，消除上游 400 类硬失败。

从 `bodyhygiene.go` + `bodyorder.go` 移植以下行为（以 autogate 实现为准逐条对照）：

1. **孤儿 tool_result 修复**：`messages` 中 `role=tool`（OpenAI）或 `tool_result`
   （Anthropic）块前面没有对应 `tool_use`/`tool_calls` 时 —— 首选注入一个合成的
   assistant tool_use 块（id 匹配 `kit.RandHex`，name 用 tool_result 里的 name 字段，
   没有则 `unknown_tool`），上游明确拒绝注入时降级为删除该孤儿块。
2. **tools 截断保护**：`tools` 数组总序列化尺寸超阈值（建议 512KB 可配置）时，从尾部
   丢弃最久未使用的 tool 定义（保序），并在日志记 `body hygiene: tools truncated`。
3. **载荷超限裁剪**：整请求体超限（建议 8MB 可配置）时，从最老的非 system 消息开始
   成对删除（assistant+user 一组），直到达标；保留 system 与最后一轮。
4. **消息顺序规范**（bodyorder）：确保 system 在最前、连续同角色消息合并、
   assistant 带 tool_calls 后必须紧跟对应 tool 结果。
5. **缺字段补全**：`messages[]` 元素缺 `role` 补 `user`、`content` 为 null 补空串。

**挂载点**：三个入口在路由决策**之前**调用（proxy.go 两处 + responses.go 一处），
对 `params`/`req` 的 messages 做原地清洗。加包级开关：

```go
// config: zenConfigData 增加字段（json tag 对应面板读写）
BodyHygiene bool `json:"bodyHygiene"` // 默认 true（loadZenConfig 迁移补 true）
```

**测试**（`body_hygiene_test.go`）：孤儿 tool_result 的注入/删除两路、tools 截断保序、
超限裁剪保 system+最后一轮、顺序规范、缺字段补全。≥10 个用例。

### 3.2 SSE 保活心跳（新文件 `internal/app/sse_keepalive.go`）

**目标**：上游慢（首字节 > N 秒）时，客户端不断开，网关获得继续等待/换道的权利。

从 `ssehygiene.go` 移植心跳写法：流式请求在等待上游首字节的窗口内，每隔
`keepaliveInterval`（默认 5s，可配置）向客户端写一行 SSE 注释 `: keepalive\n\n`
（Anthropic 入口同样用注释行，客户端会忽略）。上游首字节到达后停止心跳。

**挂载点**：不直接改三处入口 —— 封装为 `newKeepaliveWriter(w, interval)`，在
`handleStreamResponseWithUsage` / `handleAnthropicStreamWithUsage` 进入上游等待前
启动、首字节后 `Stop()`。吸收模式（第 2 批）复用同一 writer。

**注意**：必须在 `http.Flusher` 可用时才启用心跳；写失败（客户端断开）立即停并
取消上游请求 context。

### 3.3 chat 深检并入健康体系（扩展 `node_upstream.go`）

**目标**：现有矩阵只能证明“TLS 能到上游”，不能证明“额度活着、模型真能出话”。
autogate 的两轮体检（`deepprobe.go`/`modelhealth.go`/`recoveryprobe.go`）补上第二轮。

设计：

1. `node_upstream.go` 增加 `probeUpstreamChat(upstream, host, exitKey) bool`：
   经该出口对目标上游发一个最小 chat 请求（`max_tokens: 1`，消息 `[{role:user,content:"hi"}]`，
   openai 形状；对 anthropic 形状上游用 `/v1/messages` 最小体）。判定：
   - HTTP 200 → 健康（额度活着）
   - 401/402/403（非地区类）/404 → 上游侧问题，**与出口无关**，不标记出口，只标记该上游“不可用”状态
   - 429 → 视为“活着但忙”，不标记
   - 5xx/网络错误 → 该出口对该上游标记不可达（与现有 TLS 级矩阵同一张表，加一层）
2. 矩阵数据结构升级：`nodeUpstreamOK` 的 `map[upstream]map[exitKey]bool` 保持，
   新增平行表 `nodeUpstreamChat map[upstream]map[exitKey]bool`（chat 深检结果），
   `nodeSupportsUpstream` 改为两级都通过才算支持（TLS 级通过但 chat 级明确失败的才跳过；
   chat 级无数据不拦截 —— 与现有“未探测按可用”原则一致）。
3. 触发时机：现有 `probeUpstreamMatrixAsync`（连通检测后/定时/Provider 变更）完成
   TLS 级后，对 TLS 级通过的组合抽样做 chat 深检（并发 ≤4，每出口×上游 10 分钟节流），
   避免探测风暴（教训：8 并发深检 144 节点曾撞出上游限流，见第 8 节陷阱 4）。
4. zen 上游的深检请求带上 `kit.FreshZenIdentity()` 三头 + 配置 key，模型用
   `mimo-v2.5-free`（免费种子，不烧额度）。

**面板**：`admin_html.go` 节点列表的 `✓/✕` 标注区分两级（如 `✓tls ✓chat` / `✓tls ✕chat`），
后端 `nodeView` 增加 `chat map[string]bool` 字段（仅返回已探测的）。

**测试**：分类矩阵（200/401/403/429/5xx/网络错 → 各自的标记语义）、两级合成判定、
节流不重入。

### 3.4 第 1 批验收标准

- [ ] 全部现有测试绿：`go test -count=1 -tags "with_quic,with_grpc,with_utls" ./internal/...`
- [ ] 新增测试 ≥14 个全绿
- [ ] 真实进程冒烟（拷贝 `D:\cline-proxy-windows-amd64\data` 到工作区临时目录起实例）：
      1) 发一个带孤儿 tool_result 的请求 → 200（注入或删除生效）
      2) 流式请求人为延迟上游 → 客户端 5 秒收到 `: keepalive`
      3) 点连通检测 → 日志出现 chat 深检抽样记录，无探测风暴
- [ ] 提交推送（SSH 443），交付二进制重建（见第 7 节）

---

## 4. 第 2 批：吸收模式 + 中流续写 + 波次竞速 + 粘性会话

> 本批动请求主路径，是三批中最重的。**必须逐个小步提交**，每步保持测试绿。

### 4.1 吸收模式（新文件 `internal/app/absorb.go` + `stream_resume.go`）

**目标**：客户端只看到“正常但偏慢”的流式响应；上游抖动在网关内消化。

从 `absorb.go`（8.2KB）+ `streamresume.go`（8.6KB）移植，适配我们的候选链：

1. **吸收窗口**：流式请求进入 `handleChainedChatAs` 后，若 `AbsorbEnabled`（默认
   false，配置开关）：
   - 候选站失败时不再需要“首字节前”限制 —— 若**尚未向客户端发过任何业务事件**
     （心跳注释不算），直接换下一站（现有行为）；
   - 若已发过业务事件后上游断流：转入**中流续写** —— 向客户端发注释行
     `: reconnecting`，在网关内对**同一候选**重发请求，带上已收到的部分：
     把已产出的 assistant 文本/工具调用拼成 `assistant` 消息追加到 messages 尾部
     （assistant prefill），再请求剩余部分，继续向客户端转发。最多续写
     `AbsorbMaxRetries`（默认 2）次，失败则按现有错误路径结束。
2. **SSE 事件缓冲与切分**：移植 streamresume 的事件解析 —— 只在事件边界
   （OpenAI: `data: {...}\n\n`；Anthropic: 事件行+data 行）切分转发，半行缓冲；
   续写时丢弃重复的首块（按已转发字节数/事件数对齐）。
3. **非流式吸收**：非流式请求在吸收模式下退化为“网关内多站重试”（现有候选链已支持），
   加上 `chatBodyHasContent`/tool_calls 完整性判定（已有）。

**挂载点**：`routing_dispatch.go` 的流式胜出分支（现直接
`handleStreamResponseWithUsage`/`handleAnthropicStreamWithUsage`）包一层 absorb
转发器；`zen.go:handleZenChat` 与 `providers_chat.go:Chat` 的流式回写同理
（做成 `absorbWriter` 通用类型，避免三处复制）。

**配置**：
```go
AbsorbEnabled   bool `json:"absorbEnabled"`   // 默认 false
AbsorbMaxRetries int `json:"absorbMaxRetries"` // 默认 2
```

### 4.2 波次竞速 + 粘性会话（新文件 `internal/app/hedged.go` + `exit_scoring.go`）

**目标**：降低首字节延迟；同一会话钉住同一出口，让上游提示词缓存生效。

从 `holdback.go`（5.8KB，竞速波次控制）+ `scoring.go`（13KB，出口评分）移植思想，
按我们结构重写：

1. **粘性会话**：请求的会话键 = 客户端 `x-session-id` 头（存在则用），否则取
   messages 前 2KB 的 SHA256 前缀。会话键 → 出口 key（`nodeLocalKey`）映射存内存
   （LRU，1024 条，TTL 30 分钟）。选出口时（`pickZenProxyWhere` 的调用方）优先粘住
   上次胜出且当前可用（未冷却/健康/矩阵通过）的出口。
2. **波次竞速**（仅非流式 + 吸收关闭时的流式首字节阶段；吸收开启时与 4.1 互斥）：
   - 竞速宽度 `RaceWidth`（默认 1 = 关闭；最大 8）。
   - 实现：同一请求经 `RaceWidth` 个不同出口并发发出（复用 `dialNodeProxy`），
     最先返回 200 且通过内容判定的胜出，其余 context 取消。计费类上游
     （provider 配置了非零价目）**默认不参与竞速**（防重复计费），白名单只允许
     zen 免费/已标记 free 的 provider —— 用现有 `freeModelIDs()` 判定。
   - 败者的失败**不记冷却**（它们不是真失败，是输了比赛）。
3. **出口评分**（scoring 简化版）：每次真实请求成功/失败更新出口分数
   （成功 +1，失败按错误类别 −2~−10，衰减因子 0.95/小时，落盘可省略）。
   `pickZenProxyWhere` 的轮询起点改为“分数最高且可用”的出口（竞速关闭时的日常增益）。

**配置**：
```go
RaceWidth        int  `json:"raceWidth"`        // 默认 1(关闭), 最大 8
StickySessions   bool `json:"stickySessions"`   // 默认 true
ExitScoring      bool `json:"exitScoring"`      // 默认 true
```

**挂载点**：`proxy_pool.go` 的 `pickZenProxyWhere`（粘性优先+评分排序）与新的
`hedged.go`（竞速编排，被 `handleChainedChatAs` 与 `handleZenChat` 在非流式分支调用）。

### 4.3 第 2 批验收标准

- [ ] 全量测试绿；新增测试 ≥16 个（吸收续写的 prefill 拼装、事件边界切分、
      粘性 LRU、竞速胜出与败者不冷却、评分衰减）
- [ ] 真实进程冒烟：
      1) 吸收模式开：mock 上游发 3 个事件后断流 → 客户端收到 `: reconnecting`
         后完整收尾，网关日志显示 prefill 续写
      2) 竞速宽 3：日志显示 3 出口并发、1 胜出、败者无冷却记录
      3) 粘性：同会话头连发 3 请求 → `via` 出口一致
- [ ] 面板新增开关（吸收/竞速宽/粘性/评分）读写正常
- [ ] 分批提交推送，交付二进制重建

---

## 5. 第 3 批：IP 信誉 + 停摆熔断 + 睡眠唤醒

### 5.1 IP 信誉（新文件 `internal/app/iprep.go`，从 `iprep.go` 23.9KB 移植精简）

- 对每个出口节点查信誉（autogate 用 iprisk.top 等；若外部 API 不可达则优雅降级为
  “未知”），结果缓存 24h。
- 用途：`pickZenProxyWhere` 同分时偏好信誉高的出口；面板节点列表显示信誉标记。
- **约束**：查询必须走网关统一出口之外的方式（直接出网查询会引入鸡生蛋），允许该
  模块使用 Go 原生直连（与 `probeNodeModel` 同理，属工作原理需要）；外部 API 不可达
  时静默降级，**绝不阻塞请求路径**。

### 5.2 停摆熔断 + 睡眠唤醒（`outage.go` → `internal/app/outage.go`，`wakedetect.go` 并入）

- 停摆检测：滑动窗口内上游失败率超阈值（如 60 秒内 ≥80% 且 ≥10 次）时，判定
  “上游/链路级停摆”，冻结对上游的轮换重试（转指数退避 30s→5min），避免节点池被
  一波打空（冷却层被打穿的教训见第 8 节陷阱 5）。恢复探测由 `recoveryprobe.go`
  思想实现：冻结期每退避周期放行 1 个探测请求，成功即解冻。
- 睡眠唤醒：检测系统时间跳变（`time.Now` 与单调钟差值 > 60s，wakedetect 的手法）
  → 触发一次 `checkAllNodeHealth()` + 清空粘性会话表 + 重置传输池。挂到
  `startNodeHealthLoop`（若存在）或新建 goroutine。

### 5.3 第 3 批验收标准

- [ ] 全量测试绿；新增测试 ≥8 个
- [ ] 冒烟：人为制造连续失败 → 日志出现停摆冻结与恢复探测；修改系统时间模拟唤醒
      （或以单测覆盖时间跳变逻辑）
- [ ] 提交推送，交付二进制重建

---

## 6. 硬性约束（执行 agent 不得违反）

1. **不得删除或弱化**：地区探测体系（`model_region.go`）、四层冷却、候选链、
   免费判定（`freeModelIDs`/catalog）、`zenRejectMessage` 语义。这些是防止
   403 污染节点池与误用付费模型的隔离带（历史教训，非过度设计）。
2. **模型 ID 格式兼容**：`zen/xxx`、`provider:model`、`cline-pass/xxx`、路由别名
   一个都不能变；客户端零改动是验收前提。
3. **rescueDirect 语义保持**：默认 true（节点全挂允许直连兜底）。不要改成 fail-closed
   —— 那是用户明确做过相反决策的点。
4. **每次请求的“真实出口”**：新增任何日志必须用 `reqExitKey(ctx)`（拨号层回写），
   禁止用全局轮询位置冒充（曾有 403 排查被此误导一整轮）。
5. **不移植**：Windows GUI、m365、autogate 的 singbox/proxypool/mirrors/providers/usage。
6. **不提交**任何二进制、缓存文件到 git（`.gitignore` 已覆盖 `*.bak-*`）。
7. 计费保护：竞速与深检只对免费/零价上游全量启用；对配了价目的 provider 竞速默认关闭。

---

## 7. 环境与工作流约定

```bash
# Go 不在 PATH(本机装在 C:\Go):
export PATH="/c/Go/bin:/usr/bin:/bin:$PATH" GOROOT="C:\\Go"

# 构建(全标签是硬要求, 缺标签剔除 reality/quic 类节点):
go build -tags "with_quic,with_grpc,with_utls" ./...
go test  -count=1 -tags "with_quic,with_grpc,with_utls" ./internal/...

# 交付二进制(无黑窗, 41.4MB 级): 输出路径必须用 Windows 风格, Git Bash 会把
# /d/xxx 转成 D:\d\xxx(实测踩坑):
CGO_ENABLED=0 go build -tags with_quic,with_grpc,with_utls \
  -ldflags "-s -w -H=windowsgui" \
  -o "D:/cline-proxy-windows-amd64/cline-proxy-windows-amd64.exe" .

# 推送: SSH over 443 已全局重写, git push origin HEAD 即可; 失败重试, 不要 unset 代理变量。
```

冒烟验证方法（沙箱无法直连用户在跑的网关）：

```python
# 拷贝真实数据起独立实例, 用 Python subprocess + ProxyHandler({}) 探测:
# 参考 docs/reports 与既往做法: shutil.copytree(r'D:\cline-proxy-windows-amd64\data', ws/data)
# proc = subprocess.Popen([exe,'-host','127.0.0.1','-port','13460'], cwd=ws)
# 轮询 /admin/api/config 直到就绪(真实数据含 sing-box 启动, 留足 30s)
# 临时目录放 WorkBuddy 工作区(用户 D 盘新建 exe 曾被杀软清除)
```

每批完成：commit（约定 `type: subject`，正文说明动机与实测）→ push →
重建交付二进制 → 在工作日志
`C:\Users\Administrator\WorkBuddy\2026-09-11-10-28-16\.workbuddy\memory\<date>.md`
追加一段结论。

---

## 8. 已知陷阱清单（前人踩过，勿重蹈）

1. **`box.New()` 只构建不启动**，必须再调 `instance.Start()`（nodes.go 现有写法）。
2. **h2 传输自带连接池会缓存出口**：`zenHTTP2Transport` 的 `DialTLSContext` 只在
   池未命中时调用。任何“按模型选出口”的新路径如果走共享 zen 客户端会被绕过 ——
   地区受限模型的处理方式是每次请求新建 transport（`zen.go` 有现成写法）。新加的
   竞速/深检如需“指定出口”必须各自新建 transport 或走 `dialNodeProxy`。
3. **admin_html.go 是 Go 原始字符串**：改 HTML 时先做 div 配平检查（开闭计数 +
   各面板 section 层级一致），一个缺失的 `<div>` 会让后面所有内容“逃出面板”，
   症状出现在完全无关的页面。全局 CSS 把所有 `input` 拉满 100% 宽，新增原生
   checkbox 需显式覆盖样式。
4. **并发探测会撞上游限流**：深检/矩阵探测一律带节流（10 分钟）与低并发（≤4），
   判定用“是否被地区拒绝”而不是“是否 200”（429 是活着的证据）。
5. **候选层冷却预算与池大小**：目录/对话轮换预算 = 池内健康且未冷却出口数，
   有 16 次硬上限与 30s 总时长上限 —— 新增轮换路径沿用 `catalogExitBudget()` 模式，
   不要无上限轮换。
6. **换出口重试不睡眠**：节点级失败立即轮换（退避只留给 429）；客户端断开
   （ctx 取消）立即终止重试。
7. **探测的“未探测按可用”原则**：不能用“没数据”当“不可用”，否则刚启动就无节点可用。
8. **沙箱环境**：`HTTP_PROXY=127.0.0.1:54256` 会拦 localhost（用
   `--noproxy '*'` / `ProxyHandler({})`）；后台进程活不过一次工具调用（启动+探测+kill
   写进同一命令或同一 Python 脚本）；`/tmp` 不可靠（用工作区目录）。
9. **报错文案必须与真实原因一致**：曾出现“paid zen model”实为目录缺失（已修，
   `zenRejectMessage`）。新增错误路径时先想清楚用户拿这条信息能不能定位。

---

## 9. 总验收清单（三批全部完成后执行 agent 自评）

| # | 项 | 判据 |
|---|---|---|
| 1 | 构建 | 全标签 `go build` 零错误 |
| 2 | 测试 | `go test -count=1` 全绿，新增 ≥38 用例 |
| 3 | 兼容 | 现有模型 ID/入口/面板功能零回归（用旧冒烟脚本复验） |
| 4 | 体卫生 | 孤儿 tool_result 请求 200；超限裁剪保 system+末轮 |
| 5 | 心跳 | 上游延迟 >5s 时客户端收到 keepalive 且不断开 |
| 6 | 深检 | 日志可见两级矩阵（tls/chat），chat 级失败不误伤出口 |
| 7 | 吸收 | 断流续写完整收尾，客户端仅见 `: reconnecting` 注释 |
| 8 | 竞速 | 多出口并发、败者无冷却、计费上游默认不参与 |
| 9 | 粘性 | 同会话同出口（可用时），提示缓存命中率可从上游响应观察 |
| 10 | 信誉/停摆/唤醒 | 各自冒烟通过，外部 API 失败时优雅降级 |
| 11 | 交付 | 每批独立 commit 已推送（SSH 443），交付 exe 已重建到运行目录 |
| 12 | 文档 | 工作日志已追加；README 若行为有变已更新 |

自评表请连同每批的 commit hash 一起写进工作日志，供复核。

---

## 10. 批次间依赖与建议顺序

- 第 1 批独立，可立即开始；其中 3.2 的 `newKeepaliveWriter` 是第 2 批吸收模式的地基，
  接口按第 4.1 节预留。
- 第 2 批依赖第 1 批的心跳 writer；竞速依赖 `pickZenProxyWhere` 的过滤参数（已在）。
- 第 3 批独立，可与第 2 批并行（不同文件）。
- 每批内部按小节顺序小步提交；`git push` 失败属临时网络抖动，等几分钟重试，
  不要改代理配置。
