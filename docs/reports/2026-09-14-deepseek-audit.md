# Cline-proxy 独立盲审报告（DeepSeek V4.1 Flash · MVP 七角色）

- **审计日期**：2026-09-14
- **审计模型**：DeepSeek V4.1 Flash（与上一轮 §0–§9 报告的执行模型不同）
- **代码基线**：`914c1a9`（工作树全程保持干净，审计结束时 `git status --porcelain` 为空）
- **代码规模**：非测试 23,198 行 / 测试 6,798 行 / 46 个 `*_test.go` / 225 个 `Test` 函数 / 前端渲染断言 104 条
- **审计方式**：七个角色**独立盲审**（互不通信，各自只向 lead 汇报），lead 逐条复核关键结论

## 0. 本次审计与上一轮的区别

上一轮（§0–§9，另一模型执行）是**已知问题的修复追踪**。本轮是**独立盲审**：不给七位专家"要找什么问题"的暗示，只给角色视角与代码范围，看他们各自独立能挖出什么。因此本轮的价值不在"发现了更多问题"，而在于：

1. **哪些结论经得起复核**——lead 亲手复现了 7 条关键结论（见 §2 台账）；
2. **哪些结论是模型自己脑补的**——本报告明确标注了 5 处需要修正的细节（见 §2.2）；
3. **哪些问题被多个角色从不同视角独立撞车**——这是可信度最高的部分（见 §5）。

---

## 1. 执行摘要

**这个项目不是"能不能用"的问题，是"能不能安全地改"的问题。**

七位角色给出的独立总评高度一致地产出了同一个结论：

| 角色 | 独立总评 |
|---|---|
| 严过关（QA） | 协议转换层是纸糊的，"不敢在只改 `proxy.go` 协议逻辑的情况下靠测试兜底" |
| 卜宕机（运维） | 目前交付形态"**只能自己用**" |
| 高见远（架构） | 局部工程纪律及格，但结构上"**是在还债**" |
| 贝洛奇（后端） | 正确性主干扎实（B-，78/100），但有一类真实数据竞争未被覆盖 |
| 贾思敏（前端） | "能用，且不是随时会炸"——风险在状态管理不在 XSS |
| 颜好看（UX） | 能用但信息摊得太开；令牌页失败路径是死胡同 |
| 许清楚（产品） | 功能密度已超参考项目，**唯一方向性缺口是自动路由没有排序依据** |

**七个角色中，有五个独立指向了同一个结构性事实**：这个代码库的**功能跑得通，但"改动它的安全性"远低于"它的完成度给人的印象"**。QA 用实验证明了这一点（改坏协议转换，全套测试零反应），运维用真实日志证明了这一点（7011 行日志跨 25+ 次重启、`已截断` 一次都没出现），架构用依赖分析证明了这一点（配置层↔出口层已经成环）。

---

## 2. 复核台账（lead 亲手验证）

### 2.1 复核为真的结论

| # | 结论 | 提出者 | 我的取证方式 | 结果 |
|---|---|---|---|---|
| V1 | **协议转换层零测试防护** | 严过关 | 亲自把 `responses.go:23` 的 model 透传改成 `m + "___LEAD_VERIFY_BROKEN___"`，跑 `CLINE_PROXY_SKIP_NODEBOX=1 go test ./... -count=1` | **成立**：`ok internal/app 2.311s` / `ok internal/protocol 0.059s` / `ok internal/providers 0.083s`，**EXIT=0**。改坏了 Responses→Chat 的 model 透传，全部 225 个用例 + 104 条前端断言沉默。（事后已 `git checkout` 还原） |
| V2 | **主日志无运行时轮转** | 卜宕机 | 读 `proxy.go:551-576`（`initLogFile`）与 `proxy.go:583-588`（`logFanout.Write`），对比 `writeStreamLog`（`proxy.go:688`） | **成立**：截断逻辑只在 `initLogFile` 里、即**启动时执行一次**；`logFanout.Write` 每次写入**不做任何大小检查**；而 `writeStreamLog` 在**每次写**时都查 `st.Size() > maxLogBytes`。最该有位阶的日志反而没有。 |
| V3 | **节点凭据（含 UUID）进日志** | 卜宕机 | grep `subEntryKey` 全部调用点 + 读 `sub.go:108-119` 定义 | **结论成立，坐标需修正**：实际位置是 `nodes.go:515` 与 `nodes.go:527`（**不是报告里的 454/466**）。`subEntryKey()` 对字符串条目直接 `return nodeLocalKey(v)`——注释自陈"链接原文(去名称)"。同文件已有脱敏版 `nodeDisplayName()`（`nodes.go:691`）未被使用。 |
| V4 | **`loadPool()` 泄漏可变共享指针** | 贝洛奇 | 读 `pool.go:90-96` + grep 全部裸读点 | **成立**：函数持 `poolMu` 进入、`defer` 解锁、**把裸 `pool` 指针直接返回**。裸读点共 5 处：`admin.go:186`（`.CurrentIdx`）、`admin.go:226`、`proxy.go:312`、`proxy.go:456`、`proxy.go:941`（`len(.Accounts)`），全部与持 `poolMu` 的写方（`pickAccount` / `addAccount` / `refreshAccountToken`）并发。 |
| V5 | **`closeNodeBoxTimed` 无锁读 `nodeBox`** | 高见远 | 读 `proxy.go:646-660` | **成立**：`if nodeBox == nil`（:648）与 `nodeBox.Close()`（:653）均**不持 `nodeMu`**，而 `syncNodeBox` 在 `nodeMu` 下写该字段。且可达——`doGracefulShutdown` 未先停后台循环。 |
| V6 | README 指示的 `"pricing": true` 开关不生效 | 许清楚 | 读 `providerConfig` 结构体定义（`providers_config.go:39-58`） | **成立**：该结构体**没有 `Pricing` 字段**，JSON 未知键被静默丢弃。（`providers_catalog.go:142` 的 `Pricing` 是上游目录响应的嵌套字段，与此无关。） |
| V7 | README 指示的 `modelsUrl` / `modelsKeyHeader` 不存在 | 许清楚 | grep 全仓 Go 代码 | **成立**：两个键**在任何 Go 文件里都不存在**（仅有一个无关的 `recommendedModelsURL` 常量）。用户照文档配置会得到两个被忽略的键。 |
| V8 | 订阅刷新默认 30 分钟而非文档说的 6 小时 | 许清楚 | grep `defaultSubsRefreshMins` | **成立**：`sub.go:28` 为 `30`（分钟）。文档错 12 倍。 |

### 2.2 需要修正的报告细节（结论对、坐标或前提错）

| # | 提出者 | 报告原文 | 实际 |
|---|---|---|---|
| C1 | 卜宕机 | 节点凭据泄漏在 `nodes.go:454/466` | 实际在 `nodes.go:515/527`（偏差 60 行） |
| C2 | 高见远 | "44 个测试文件" | 实际 **46** 个（`ls internal/app/*test*.go` 计数） |
| C3 | 贾思敏 | 裸 `setInterval` 在 `admin_html.go:431-433` | 提出者已自我更正为 **2431-2433** |
| C4 | 贝洛奇 | "环境无 Go 工具链（`go vet`/`go test -race` 不可用）" | **不成立**：Go 在本机可用且 lead 全程在用。该角色未设 `PATH`/`GOROOT` 即下此判断，导致其报告**全部为静态核对、零运行时证据**。结论质量高，但"可复现性"环节缺失。 |
| C5 | 贝洛奇 | 把 `doGracefulShutdown` 列入"已验证良好" | 与高见远（V5）**直接冲突**：同一函数内的 `closeNodeBoxTimed` 存在无锁读。见 §5.2 |

---

## 3. P0 阻断级发现

### P0-1 协议转换层零测试防护（严过关，lead 已复现 V1）

这是本轮**最严重的单点发现**，且是唯一由实验而非阅读得出的结论。

| 未覆盖路径 | 行数 | 风险 |
|---|---|---|
| `responses.go` 全文 | 589 | Responses 请求→OpenAI 上游→回吐 Responses SSE，任何映射错都静默发错包 |
| `proxy.go` 协议段落（1529–1918） | ~390 | `anthropicToOpenAI` / `openAIToAnthropic` / `anthropicToolsToOpenAI` / `filterToolInput` |
| `compact.go` 全文 | 483 | 上下文压缩选错/截错 = 答非所问或丢历史 |
| `zen.go` / `proxy_pool.go` | 882 / 457 | 各仅 1 个用例 |

**关键证据**：把 `responsesToChat` 与 `anthropicToOpenAI` 两个核心转换函数各改坏一处，`go test ./...` 仍 `ok exit=0`。这三个协议正是网关存在的理由，却几乎没有断言。

对照组（证明防护力是"分层不均衡"而非"整体失效"）：改坏 `proxy.go:2574` 的 tool_calls 修补条件 → **立刻三连红**（`TestRepairToolCalls` / `TestChainBodyOnlyBrokenToolCalls` / `TestChainFailsOverOnBrokenToolCalls`）。

### P0-2 主日志无运行时轮转（卜宕机，已验证 V2）

`maxLogBytes = 10 MiB` 的限制**只在启动时检查一次**，运行期日志无上限增长。

- 旁证：交付目录真实日志 `D:\cline-proxy-windows-amd64\data\cline-proxy.log` 跨 25+ 次重启、7,011 行，`已截断` 一次都没出现。
- 放大器：`09/13 16:18` 两轮订阅刷新各吐 ~4,600 行 `node NNNN: 解析失败已跳过: unsupported scheme "https"`——日志能被外部输入轻易刷爆。
- 修复落点：把截断搬进 `logFanout.Write`（加 mutex，超限则 `Truncate(0) + Seek(0,0)`），与 `writeStreamLog` 对齐。

### P0-3 节点凭据写入日志，且会被打进诊断包（卜宕机，坐标已修正 V3）

`nodes.go:515/527` 打印 `subEntryKey(v)` = **节点链接原文**，含 `vless://<UUID>@<IP>:<PORT>?...`。而 `tray_windows.go` 的"导出诊断包"会把日志尾部打进 zip，其注释却写着"避免把敏感文件带出去"。

- 上一轮只脱敏了**订阅 URL**（`sub.go:163`），**漏了节点链接**。
- 同文件已有现成的脱敏函数 `nodeDisplayName()`（`nodes.go:691`，输出 `host:port`）。

---

## 4. P1 / P2 / P3 汇总

### P1

| 编号 | 发现 | 位置 | 提出者 | 复核 |
|---|---|---|---|---|
| P1-1 | **自动路由没有排序依据**——候选链按 provider 名字典序展开，与模型能力无关（部署态 5 家 provider / 上千模型，首个候选可能是 2B 小模型） | `routing_chain.go:198`；`providerNames()` `providers_config.go:402` | 许清楚 | 未复核 |
| P1-2 | **免费判定靠目录不靠实测**——目录无价格的上游（Gemini/B.AI）只能靠手工白名单 | `providers_catalog.go:173-184` | 许清楚 | 未复核 |
| P1-3 | **Account 字段锁外裸读**（并发）- 见 V4 | `pool.go:90` | 贝洛奇 | ✅ |
| P1-4 | **`closeNodeBoxTimed` 无锁读**（并发）- 见 V5 | `proxy.go:648,653` | 高见远 | ✅ |
| P1-5 | **包内双向依赖环**：配置层(zen.go) ↔ 出口层(proxy_pool.go)，根因是 `getZenConfig()`(36 处/15 文件) 与 `getZenHTTPClient()`(10 处/9 文件) 实为全局服务定位器 | `zen.go:660,785,850` ↔ `proxy_pool.go:112,124,210` | 高见远 | 未复核 |
| P1-6 | **文档误导配置**（3 条，见 V6/V7/V8） | `README.md:224,246,186` | 许清楚 | ✅ |
| P1-7 | **`buildVersion` 三处构建入口都未注入** → 所有发布版 `/health` 恒回 `go-1.1`，两个版本无法区分 | `build.yml:120` / `AGENTS.md:85` / `main.go:133` | 卜宕机 | 未复核 |
| P1-8 | **启动失败在 1.2s 后被静默吞掉**（GUI 无控制台，用户看不到任何原因） | `main.go:86-101` | 卜宕机 | 未复核 |
| P1-9 | **令牌引导页失败路径是死胡同**——令牌粘错后回同一页、零反馈，用户会再次得出"程序坏了" | `admin.go:125,161-169` | 颜好看 | 未复核 |
| P1-10 | **加载失败静默 + 可覆盖真实配置**（表单留空后点保存 = 用默认值清掉真实代理/订阅列表） | `admin_html.go:1392,1445` | 颜好看 / 贾思敏 | 未复核 |
| P1-11 | **数据目录不可写 → 零诊断且不弹窗**（`MkdirAll` 错误被丢弃；用户把 exe 放进 `Program Files` 即触发） | `kit/data.go:39-44` | 卜宕机 | 未复核 |

### P2（择要）

- **`/health` 的 `status` 恒为 `"ok"`**，出口池几乎全死时（实测 24/3958 可达）仍报 ok；`exitReady` 名不副实（语义是 `appServer != nil`）。（卜宕机）
- **`poolMu` 在落盘全程被持有**（marshal+fsync+rename），热路径 `pickAccount` 排在磁盘 IO 后 → p99 尖刺。（贝洛奇）
- **11 个后台 ticker 循环只有 1 个接了 `appRootCtx`**（仅 `stats.go:372`）。（高见远 / 贝洛奇 独立撞车）
- **退出不落盘 pool 计数器与 usage 账本**（面板上用户正看着的数据）。（高见远）
- **全局搜索清空后 `<mark>` 高亮残留**（真功能缺陷，`filterModelIndex` `admin_html.go:1823-1855`；对照 `filterCardModels` 写对了）。（贾思敏）
- **`renderOneCard` 用 `escAttr` 拼 CSS 选择器且位于保存 try 内** → 异常被误报"保存失败"并回滚**已成功的保存**。（贾思敏）
- **前端测试不进 CI**：`scripts/admin-render-test.js`（104 断言）不在 `build.yml` 中 → 前端改坏照样合并。（高见远 / 严过关 独立撞车）
- **日志无 level、无请求关联键**，一次请求散在 8 行，无法聚合。（卜宕机）
- **`model sync` 失败每分钟无限重复刷屏**。（卜宕机）

### P3（择要）

- Windows 上 `0600` 无效且无 ACL 替代（`admin-token` / `.cline-accounts.json` / `.zen-config.json` / `subs_cache.json` 明文）。
- Release 无校验值/签名（41.7MB 未签名 exe + SmartScreen）。
- Docker：`docker-compose.yml:11` 端口暴露全网段、`Dockerfile:11` 无非 root `USER`。
- `internal/cline/auth.go:113`、`internal/providers/clinepass.go:84` 非原子写。
- `time.Sleep` 余量过小（`zen_model_health_test.go:54` hold=50ms 睡 60ms，余量仅 10ms）→ flaky 风险。
- 测试全局状态靠手写 reset，任何 `t.Parallel()` 都会炸。
- 测试全白盒（46/46 `package app`），测试钩子（`startNodeInstanceFn` 等 3 个）长在生产代码里。

---

## 5. 跨角色撞车与分歧

### 5.1 独立撞车（不同视角各自发现同一问题 → 可信度最高）

| 问题 | 角色 A | 角色 B |
|---|---|---|
| 后台循环未接 `appRootCtx` | 高见远（从 11 个 ticker 的 ctx 覆盖统计） | 贝洛奇（从退出路径的循环存活） |
| 前端测试不进 CI | 高见远（从 CI 步骤清单） | 严过关（从 CI 与本地一致性比对） |
| 配置加载失败 → 静默覆盖 | 颜好看（从用户操作路径） | 贾思敏（从 `console.warn` 代码点） |
| 日志会带出敏感内容 | 卜宕机（从日志写入点 grep） | 高见远（从 `0600` 在 Windows 无效的结构性观察） |

### 5.2 唯一实质分歧：`doGracefulShutdown`

- **高见远**：`closeNodeBoxTimed` 无锁读 `nodeBox`，是退出期的真竞态（P2）。
- **贝洛奇**：把 `doGracefulShutdown` 列入"已验证良好"（`sync.Once` + 超时兜底，不会挂住退出）。

**lead 裁定**：高见远正确（见 V5）。两者说的不是同一件事——贝洛奇评的是"**退出会不会卡死**"（确实不会，`sync.Once` + 3s/5s 超时兜底是对的），高见远评的是"**退出过程会不会与订阅刷新竞争同一份数据**"（会）。贝洛奇在这个函数上**漏审了并发面**。这恰好也解释了 C4：贝洛奇没有运行时环境，静态阅读时漏掉了跨 goroutine 的可达性分析。

---

## 6. 与上一轮评审（另一模型）的差异

这是本次审计最值得记录的部分——同一份代码，不同模型得出的结论**不重叠度很高**：

| 维度 | 上一轮（§0–§9） | 本轮（DeepSeek V4.1 Flash） |
|---|---|---|
| **性质** | 已知问题的修复追踪 | 独立盲审 |
| **最强发现** | 具体的并发/生命周期缺陷（`syncNodeBox` 锁外构建、`modelsCacheSnapshot`、`healthRunAgain` 补偿） | **协议层零测试防护**——一个"改错了测试会送你进生产"的实验级证据；以及 **`loadPool()` 共享指针竞态** |
| **方法特征** | 修复 → 反向验证（制造回归证明断言有效） | 角色分工 → 交叉复核（7 条关键结论被 lead 亲手复现） |
| **独有发现** | `requiresStream` join、私有模型卡片消失、cookie 回退 | 协议层零防护、`loadPool` 竞态、包内依赖环、README 三处配置误导、节点凭据入日志 |
| **重复发现** | — | 后台 ticker 未接 ctx（上一轮 §8.8-3 已记录为遗留项） |

**两轮都提到、但两轮都没解决的**：后台循环的 `appRootCtx` 覆盖（上一轮列为遗留，本轮两个角色独立再次报出）。

---

## 7. 建议行动（按"投入产出比"排序）

### 立刻做（当天可完成，收益最高）

1. **给协议层补表驱动测试**：`responses.go` 的 `responsesToChat` / `chatToResponses`，`proxy.go` 的 `anthropicToOpenAI` / `openAIToAnthropic`（含 tools 的 `input_schema`→`function.parameters` 映射），流式聚合用 `httptest` 造 SSE。这是唯一"不做就等于没测试"的地方。
2. **修 P0-2 / P0-3 两条日志问题**：截断搬进 `logFanout.Write`；`nodes.go:515/527` 改用现成的 `nodeDisplayName()`。
3. **修 `loadPool()` 竞态**：新增 `poolSnapshot()` 返回深拷贝（照抄 `modelsCacheSnapshot()` 的既有模式），5 个裸读点全部改走它。
4. **CI 加一行 `node scripts/admin-render-test.js`**（纯 Node、零依赖）。
5. **修 3 条 README 配置落差**（`pricing` / `modelsUrl` / `modelsKeyHeader` / 6 小时→30 分钟）。

### 随后做（1–3 天）

6. `closeNodeBoxTimed` 改为"先持 `nodeMu` 摘 box 并置 nil，再锁外 Close"。
7. 令牌引导页区分"未带令牌"与"令牌不匹配"，回显「令牌无效」；进入后 `replaceState` 抹掉地址栏 token。
8. 配置加载失败时置错误态并禁用保存按钮（防覆盖真实配置）。
9. `doGracefulShutdown` 补 `flushPool()` + `flushUsage()`。
10. 把 `buildVersion` 注入 LDFLAGS；`/health` 的 `status` 改为综合判定（`exitReachable==0 && subNodes>0` → `degraded`）。

### 需要设计决策（不要顺手改）

11. **自动路由排序依据**（许清楚的产品缺口）：把 frp 的 `evaluateModel` 那套打分（10 道确定性题 + 元数据 + 冷延迟 + 真实流量成功率校正）移植过来，纯内部逻辑、零新依赖。这是本轮唯一"产品方向上值得做的事"。
12. **`admin_html.go` 是否用 `//go:embed` 分离成真 `.html` 文件**（高见远的建议）：论据是全文只有 2 个反引号、0 个 `${`——前端被 Go 的字面量分隔符反向约束，**永久不能用模板字符串**。`//go:embed` 不引入任何构建步骤、产物与今天完全一致。
13. **依赖收口**（拆环）：给 `getZenConfig` / `getZenHTTPClient` / `effectiveProxyList` 建显式依赖入口。注意：**测试耦合（46 个白盒测试）不要动**——收益低风险高，只需加一条"新增对外行为一律用 `package app_test`"的规矩。

---

## 8. 审计过程本身的记录

- **工作树完整性**：审计全程 HEAD `914c1a9`，`git status --porcelain` 为空。lead 在复现 V1 后已 `git checkout` 还原并复核。
- **一次流程风险已被拦截**：某角色在交卷时把一条前端缺陷直接移交给修复工去改代码（审计阶段不应写代码），lead 已下达 HOLD 指令叫停，并确认工作树未被污染——否则另一个角色"改坏代码→验证→还原→证明 git 干净"的取证链条会当场失效。
- **未能实测的部分**：贝洛奇（并发）因未配置 Go 环境，其全部结论为静态核对；卜宕机/高见远/贾思敏/颜好看/许清楚的部分条目为阅读所得，未经 lead 逐条复核（本报告已用"复核"列标注）。

---

## 9. 修复落地记录（2026-09-14，commit `2fca091`）

本报告的全部 P0/P1 已落地，P2 大部分落地，P3 择要落地。落地过程中**又发现并修掉了两个本报告未覆盖的更深问题**，一并记录在此。

### 9.1 修复实施期新发现（不在原报告内）

| 新发现 | 严重度 | 说明 |
|---|---|---|
| **全仓 4 处日志轮转在 Windows 上一直是静默失效的** | **比原报告更严重** | 原报告只说"主日志没有运行时轮转"。实际是 `proxy.go`(主日志/流日志)、`stats.go`、`logs.go` 四处都在 `os.O_APPEND` 句柄上调 `file.Truncate(0)`，而 **Windows 下 O_APPEND 句柄拿不到 `GENERIC_WRITE`，截断返回 `Access is denied`**，每处的 `if err == nil` 兜底把错误吞了。已在真实 Windows 环境实测复现（探针输出 `truncate ...: Access is denied.`），统一改为 `os.Truncate(path, 0)`。 |
| **`poolSnapshot()` 曾被留在"故意写坏的负向验证版本"上** | 阻断 | 实施 `poolSnapshot` 的写手在做负向验证时被限流打断，盘上留下的是**锁外裸读**的版本——代码编译得过、测试也绿，**但竞态根本没修**，只是从 `loadPool()` 挪进了 `poolSnapshot()`。只有逐行读才抓得住。 |

### 9.2 落地清单

- **P0-1 协议层测试**：新增 `responses_test.go`(500 行) / `protocol_convert_test.go`(374 行) / `compact_test.go`(218 行)。
- **P0-2 日志轮转**：`logFanout` 改为带锁结构并在写入路径维护上限；4 处截断点统一改 `os.Truncate`；新增 `log_rotate_test.go`（3 条断言 + 负向验证）。
- **P0-3 节点凭据入日志**：`nodes.go` 两处改用 `nodeDisplayName()`。
- **P1-3 账号池竞态**：新增 `poolSnapshot()` 持锁深拷贝，9 处只读点改走它（`admin.go` / `zen.go` / `proxy.go`），抽出 `loadPoolLocked()`；新增 `pool_race_test.go`（负向验证稳定报 `WARNING: DATA RACE`）。
- **P1-4**：`closeNodeBoxTimed` 改为先持 `nodeMu` 摘句柄置 nil 再锁外 Close。
- **P1-7**：CI 在构建前算版本并 `-X` 注入 `buildVersion`（实测 `/health` 已从 `go-1.1` 变为注入值）。
- **P1-8**：`main.go` 增加常驻 `errCh` 监听，启动失败不再被静默吞掉。
- **P1-9**：令牌引导页区分"未带令牌"与"令牌无效"，后者回显提示并回填输入（真机冒烟已验证两种分支）。
- **P1-11**：`kit` 增加 `os.UserConfigDir()` 回退与 `DataPathError()` 供上层感知。
- **P2**：`/health` 的 `status` 改为综合判定 + 新增 `exitReachable`/`exitProbed` + `exitReady`→`serverRegistered`；退出补 `flushPoolLocked()`+`saveUsageLedger()`；11 个后台 ticker 接入 `appRootCtx`；前端测试进 CI；`<mark>` 残留；`renderOneCard` 的 `CSS.escape`；导出账号改走统一 `apiResponse` 信封；模型同步失败刷屏抑制。
- **P3**：Docker 非 root 用户；compose 端口只绑 `127.0.0.1`；Release 生成 sha256；网关密钥改 `crypto/rand`。

### 9.3 已知未修（明确记录，不静默跳过）

| 项 | 原因 |
|---|---|
| `internal/providers/clinepass.go` 的 `recoveryLoop` 未接退出信号 | `package providers` 跨包访问不到 `appRootCtx`；为"整洁"引入跨包依赖是负收益。Go 中泄漏的 ticker 协程不阻止进程退出（`GracefulExit` 后即 `os.Exit(0)`），故列为已知项。 |
| 日志无 level、无请求关联键 | 改动面大（需要中间件往 context 注入请求 ID 并贯穿各转发路径），收益低于成本，本轮不做。 |
| `admin_html.go` 的部分 UX 细化（label 关联、窄屏 toast、删除进度、文案统一等） | 低危项，本轮优先保证了正确性类修复。 |
| `kit.LastDataPathError` / `proxyListenAddress` 等启动期写、运行期读的全局量未加锁 | 写入只发生在启动或极端失败路径，读发生在启动后；未构成运行期竞态。 |

