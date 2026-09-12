# sing-box 集成评估 + Freebuff-2API 移植评估

日期：2026-09-12 · 范围：只做评估，未改动任何代码
方法：读本仓库 `internal/app/{nodes,sub,proxy_pool,providers_*}.go`、`go.mod`；
拉取 SagerNet/sing-box 与 lza6/Freebuff-2API 的真实源码核对（参考代码存于 `D:\fb2api`）。

---

## 一、结论速览

| 你的问题 | 结论 |
|---|---|
| sing-box 集成是否"完美完善"？ | **主干是对的，且设计比常见做法更省资源**：单实例 + 多出站 + 每节点一个本地入站，1:1 路由。缺 3 种链接格式（wireguard / naive / tor）与 SSR 支持（后者是 sing-box 核心的硬限制）。 |
| 所有活动是否都走 sing-box？ | **业务上游流量（zen / cline 池 / 通用 Provider / 订阅抓取 / 模型目录同步）已全部走**。两个**故意**的例外：① 节点地区探测逐节点直拨（这是它的工作原理）；② direct 模式绕过 sing-box（等价直连，不是走 sing-box 的 direct 出站）。 |
| Freebuff-2API 是"多供应商轮询"吗？ | **不是**。它是 **codebuff.com 单一上游的反代 + 多账号轮询**（Rust，53 星）。"多供应商"这一层网关反而更强。它真正强的是**熔断/重试/会话/用量/上下文压缩**这几块工程细节。 |
| 值得移植吗？ | 值得，但要**按模块挑**，不是整体照搬。优先级见第四节。 |

---

## 二、sing-box 集成现状（逐项核对到代码）

### 2.1 架构

```
客户端 → 网关 :3457 → 出口决策(direct/proxy)
                        ├─ proxy：嵌入式 sing-box 单实例（节点出站 × N，每节点一个本地入站端口）
                        │          → 节点服务器 → 上游（域名在节点侧解析）
                        └─ direct：Go 直连，跳过 sing-box → 上游
```

- **单实例**：`syncNodeBox()` 一次性构建 `inbounds[] + outbounds[] + rules[]`，交给 `box.New()`（`internal/app/nodes.go`）。不是"一个节点一个进程"，80 个节点仍然只有一个 sing-box 实例 —— 内存与启动开销都可控。
- **每节点一个入站**：`freeLocalPort()` 为每个节点分配本地端口，路由规则把该入站定向到该节点的出站；`dialNodeProxy()` 经 `dialHTTPProxy` 拨这个本地端口。
- **依赖**：`github.com/sagernet/sing-box v1.14.0` + `sing v0.9.0-beta.4`，Go 1.25.5。构建标签 `with_quic,with_grpc,with_utls` 决定 QUIC/gRPC/uTLS 是否编入。
- **DNS**：sing-box 内使用 `default_domain_resolver`（8.8.8.8 UDP），目标域名交给出站远端解析 —— 避免本地 DNS 污染，这是对的。
- **健康检查**：`checkNodeHealth() / startNodeHealthLoop()` 周期性探测，另有 `validateOutboundEntry()` 逐节点校验配置（单个坏节点不影响其他节点）。

### 2.2 链接格式覆盖

| 格式 | 网关 | sing-box 官方 | 说明 |
|---|---|---|---|
| vmess / vless / trojan / ss | ✅ | ✅ | 含 reality、ws/grpc/httpupgrade、shadowtls 组合 |
| hysteria / hysteria2 | ✅ | ✅ | 需 `with_quic` |
| tuic | ✅ | ✅ | 需 `with_quic` |
| anytls / ssh / snell / shadowtls | ✅ | ✅ | |
| socks / http | ✅ | ✅ | 作为上游节点 |
| **wireguard** | ❌ | ✅ | 缺链接解析（wg 无统一 URL scheme，通常给配置文件） |
| **naive** | ❌ | ✅ | 缺解析 |
| **tor / openvpn / openconnect / tailscale** | ❌ | ✅ | 网关未接入（对 LLM 网关属低频需求） |
| **ssr (ShadowsocksR)** | ❌ | ❌ | **sing-box 核心不支持**，不是网关的锅，无法通过移植解决 |

### 2.3 订阅格式覆盖

- sing-box JSON（`parseSingBoxSub`）—— **出站配置整块透传**，因此 sing-box 支持而网关"没有专门解析器"的类型（如 wireguard JSON、naive JSON），只要订阅里给的是 sing-box 格式，实际**已经能用**（`validateOutboundEntry` 会校验）。
- Clash YAML（`parseClashSub` + `clashToOutbound`）。
- base64 分享链接列表（`isNodeOrProxyLine` + `nodeOutbound`）。

**所以"支持所有节点格式"的真实达成度高于链接解析表所显示的**：链接层面缺 wireguard/naive/tor，但 JSON 订阅层面已经通了。

### 2.4 已做对的地方（不建议改动）

1. 单实例多出站 —— 比"每节点起一个进程"省得多。
2. 出口决策作用于全部上游（zen / cline 池 / Provider / 订阅 / 目录同步 / 模型同步），这是我们前几天刚统一好的。
3. 四层冷却：账号层 / 出口层（节点）/ 熔断层（zen）/ 候选层（`upstream:model`）—— 比 sing-box 自己带的 `urltest` 更贴合"按错误类型差异化冷却"的需求。
4. 域名在节点侧解析，本地无 DNS 泄漏。
5. 节点健康检查 + 地区能力探测（`probeNodeModel`）分离：前者看"通不通"，后者看"这个地区能不能用这个模型"（Google 类上游必需）。
6. `validateOutboundEntry` 的逐节点校验：坏节点被剔除而不是拖垮整个实例。

### 2.5 缺口清单（按优先级）

| 优先级 | 缺口 | 影响 | 建议 |
|---|---|---|---|
| P0 | 链接格式缺 `wireguard` | 用户订阅里 wg 节点被静默丢弃 | 补一个 `parseWireguard`（接受 `wg://` 或粘贴 JSON） |
| P1 | 链接格式缺 `naive` | 同上 | 补 `parseNaive`（sing-box 原生支持，成本低） |
| P1 | 未使用 sing-box `urltest` / `selector` | 无"自动选最快节点"能力 | 可选：加一个虚拟节点走 urltest 出站；但注意与自研冷却的职责重叠 |
| P2 | 每节点一个入站端口 | 节点很多时本地监听数量线性增长 | 当前可接受；如节点数破百可改"单入站 + selector 切换" |
| P2 | 链接格式缺 `tor` | 低频需求 | 暂不做 |
| — | `ssr` | sing-box 核心不支持 | 无法解决，需换内核 |

### 2.6 关于"所有活动都走 sing-box"的准确表述

- 已覆盖：LLM 上游请求、订阅抓取、目录/模型同步、候选链全部站点。
- 故意例外（不建议改）：`probeNodeModel` 逐节点直拨 —— 它本身就是"用每个节点各拨一次"来判断地区可用性，若走统一出口就失去意义。
- direct 模式：走 Go 原生 dialer，**不经过 sing-box**。行为等价于"直连"，但不是 sing-box 的 `direct` 出站。若你希望连 direct 模式也统一由 sing-box 承载（例如为了后续加分流规则），可以改成"sing-box 始终运行、direct 模式把路由 final 指向 direct 出站"，改动量中等。

---

## 三、Freebuff-2API 真实定位

**它是什么**：`codebuff.com` 的**逆向反代**，把 CLI/Web 的协议还原成 OpenAI/Anthropic 兼容接口，重点是**多账号 token 轮询 + 高并发**，可 Docker / 桌面端部署。Rust 实现，模块划分清晰。

**它不是什么**：不是多供应商网关。它的 `upstream_base_url` 只有一个（`https://www.codebuff.com`），"多"体现在 `auth_tokens[]`（多个账号），配置里 `fallback_models` 只是模型名回退，不涉及供应商间切换。

### 3.1 模块对照

| Freebuff 模块 | 职责 | 网关对应物 | 对比 |
|---|---|---|---|
| `pool.rs` | 账号池 + **三态熔断器** | `zen.go` clinePoolReady / 账号层冷却 | **Freebuff 更精细** |
| `retry.rs` | **FailureKind 分类 + 可重试判定 + 抖动退避** | `cooldown.go` 分类 + 自研重试 | **Freebuff 更成体系** |
| `router.rs` | 模型路由 + **工具结果压缩 / 按 token 截断** | 无对应 | **Freebuff 独有** |
| `usage.rs` | **SQLite 用量库 + 每日聚合** | `usage.go` JSON 账本 | 各有优劣 |
| `session.rs` | **会话保活 + 账号亲和** | 无对应 | **Freebuff 独有** |
| `upstream.rs` / `protocol/*` | SSE 双向转换 | `responses.go` / `anthropic` 转换 | 平手 |
| `telemetry.rs` / `logbus.rs` | 遥测与结构化日志 | 有请求日志 | Freebuff 更结构化 |

### 3.2 它明显更强的四点

1. **三态熔断器**：`Closed / Open / HalfOpen`，`FAILURE_THRESHOLD=4` 次连续失败跳闸，`BASE_COOLDOWN=60s → MAX_COOLDOWN=600s` 指数退避，半开态**只放行一个探测请求**（`probing` 闸门），连续 `HALF_OPEN_SUCCESS_TO_CLOSE=2` 次成功才恢复。比网关现在的"按类别定死时长"更能自适应。
2. **失败分类成体系**：`RateLimit / Auth / Forbidden / Server / Network / Timeout / Other`，并且区分**可重试 / 不可重试**（401/403 判定为"换号而不是原地重试"），退避带抖动（`JITTER_RATIO`），限流有独立倍率（`RATE_LIMIT_MULTIPLIER`）。
3. **会话保活与账号亲和**（`session_keepalive_sec=45`）：同一会话粘在同一账号上，避免多账号轮询打断上下文 —— 对 Cline/zen 这类有会话概念的上游很有价值。
4. **上下文压缩**（`compress_tool_result` / `truncate_to_tokens`）：直接省 token 成本，网关目前没有。

### 3.3 网关更强的地方（不要被"功能强大"误导）

- **多上游候选链 + 跨供应商故障转移**（Freebuff 完全没有）。
- **出口/节点体系**（sing-box 集成、地区能力探测、出口冷却）。
- **自动路由别名 + discovery 自动发现 + 配额账本**。
- 结论：两者是**不同方向**的项目，Freebuff 强在"单上游把账号榨干"，网关强在"多上游择优"。合并才是正解。

---

## 四、移植建议（按价值 / 成本排序）

| # | 项目 | 来源 | 价值 | 成本 | 风险 | 建议 |
|---|---|---|---|---|---|---|
| 1 | **三态熔断器**（账号层 + 候选层） | `pool.rs` | 高：自适应冷却，减少无效重试 | 中：`cooldown.go` 加状态机，配置项扩展 | 低 | **推荐先做** |
| 2 | **FailureKind + 抖动退避 + 可重试判定** | `retry.rs` | 高：401/403 不再原地重试，退避不撞车 | 小-中 | 低 | **推荐先做** |
| 3 | **遥测字段 `error_kind`** | `telemetry.rs` | 中：排障与面板统计更准 | 小 | 低 | 顺手做 |
| 4 | **会话保活 / 账号亲和** | `session.rs` | 中-高：对 cline 池、zen 会话有用 | 中 | 中（需确认上游会话语义） | 二期 |
| 5 | **工具结果压缩 / token 截断** | `router.rs` | 中：直接省钱 | 小-中 | 中（可能影响结果完整性，需可开关） | 二期 |
| 6 | **SQLite 用量库替代 JSON/JSONL** | `usage.rs` | 中：并发写入更稳、查询更快 | 中（引入依赖，与"单 exe 无依赖"取向一致其实可行） | 中 | 可选 |
| — | 单上游假设 / ads / 登录窗口 / skills / 记忆 | — | 与网关目标无关 | — | — | **不移植** |

> 说明：项目现有约定是"不引入新的第三方依赖，保持单 exe 分发"（见 `AGENTS.md`）。SQLite 需要驱动依赖（如 `modernc.org/sqlite`，纯 Go 可实现），是否引入需你拍板；前三项都不需要新依赖。

---

## 五、如果要让 sing-box 集成"完美完善"

1. **补 wireguard 链接解析**（P0）—— 让 wg 节点不再被静默丢弃。
2. **补 naive 解析**（P1）—— sing-box 原生支持，成本低。
3. **文档化支持矩阵** —— 在 README 明确列出：链接格式、订阅格式、DNS 行为、direct 语义、"JSON 订阅可透传任意 sing-box 出站"这一点（用户最容易低估）。
4. **可选：单一虚拟节点走 `urltest`** —— 提供"自动选最快节点"，与现有按错误冷却并存（urltest 负责延迟择优，自研冷却负责故障隔离）。
5. **可选：direct 模式也由 sing-box 承载** —— 为将来加分流规则（域名白名单走直连、其余走节点）留出结构。

---

## 六、建议的下一步（等你选）

- **方案 A（小）**：补 wireguard + naive 解析 + README 支持矩阵。
- **方案 B（中，推荐）**：A + 三态熔断器 + FailureKind/抖动退避 + 遥测字段。
- **方案 C（大）**：B + 会话保活 + 上下文压缩 + （可选）SQLite 用量。

参考代码已下载到 `D:\fb2api`（pool/retry/router/usage/upstream/session + README + 配置示例），便于对照实现。
