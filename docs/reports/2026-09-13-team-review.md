# Cline-proxy 专家团队七角色评审报告（2026-09-13）

- 评审对象：`krisxu23/Cline-proxy`（`internal/app` 单包约 26,200 行 + `admin_html.go` 2,342 行裸字符串前端）
- 评审方式：7 个只读角色并行（产品经理 / UI-UX / 首席架构 / 前端 / 后端 / 测试 / 运维），lead 交叉验证、推翻 1 处误报后合并去重
- **§5 是行动清单**，§7 是执行记录，§8 是第二轮全量修复记录
- 前情：`2026-09-13-iterative-review.md`（第一轮）、`2026-09-13-second-pass-review.md`（二轮）

> **文件损坏声明（必须读）**：本文件在一次 `cat >>` 追加过程中头部被截断，
> **原 §0 / §1 / §2.1 / §2.2 前 6 行已丢失**（文件无 git 历史，无法恢复原文）。
> 下文 §0–§2.1 是**据会话记录重建的概要**，标记为「重建」，不是原文；
> §2.2 表格**只剩原第 7、8 两行**，1–6 行未重建（原文丢失，不编造）。
> 底层 P0/P1 细节请对照 `2026-09-13-second-pass-review.md`，那份文件完好。

## 0.（重建）本轮评审自己推翻的两处过度声明

这一节原本是用来给自己打脸的 —— 评审者先声明「测试全绿」「渲染测试覆盖了渲染」，
被 lead 交叉验证发现是假的。**先认这两条，否则后面所有结论都不可信。**

### 0.1「测试全绿」只在带构建标签时成立

裸 `go test ./internal/...` 实际是 **2 个 FAIL**（`nodes_tls_sanitize_test.go:124,152`）：
无 `with_utls` 标签时 sing-box 的 `validateOutboundEntry` 会先把 vless/anytls 节点拒绝掉，
sanitizer 根本轮不到跑，测试就红了。上一轮汇报「全绿」时用了带标签的命令，没说明这一点。
→ 已由 §5 第 3 项修掉：把需要 sing-box 的那半截拆到 `//go:build with_utls` 单独文件。

### 0.2 渲染测试 67 条断言里约 8 组是空转假阳性

`scripts/admin-render-test.js` 的 DOM stub 里 `closest()` 硬返回 `null`、
`querySelectorAll()` 硬返回 `[]`、`setTimeout()` 硬返回 `0`，导致 `filterModelIndex` /
`filterCardModels` 的分支整个短路 —— 断言照样 PASS。而且 `esc` stub 比生产**更严**（多转 `"`），
会把「title 属性双引号闭合注入」这类 XSS 直接掩盖：测试全绿，生产是漏洞。
→ 已由 §5 第 7 项修掉（67 → 98 条断言，见 §7）。

## 1.（重建）P0（阻断）

| # | 位置 | 问题 | 处置 |
|---|---|---|---|
| P0-1 | `proxy.go` `initLogFile` | `cline-proxy.log` **无限追加、无轮转**。GUI 构建（`-H=windowsgui`）没有控制台窗口，这个文件是**唯一的诊断通道**；长期运行会撑爆磁盘，翻日志也越来越难 | 已修：`maxLogBytes = 10 << 20`，超限时 `Truncate(0)` + `Seek(0)` 归零（Windows 的 `Truncate` 不会自动把写偏移归零，O_APPEND 下不 Seek 会在文件中间留空字节） |
| P0-2 | `nodes_tls_sanitize_test.go:124,152` | 裸 `go test` 两个红测试（见 §0.1） | 已修：拆分构建标签，见 §5 第 3 项 |

## 2.（重建）P1（应修）

> 原报告本节共 27 项，按 2.1–2.7 分小节。**2.1 与 2.2 的原文在前 6 行已丢失**，
> 下面 2.1 是据会话记录重建的概要；2.2 表格只剩原第 7、8 两行。

### 2.1（重建）我这轮交付代码里的真问题（已复核确认）

| # | 位置 | 问题 | 处置 |
|---|---|---|---|
| 1 | `admin_html.go` 状态徽章 `title` 属性 | 用了 `esc`（只转 `& < >`），双引号未转义 → 上游返回文本含 `"` 即可闭合属性注入。`escAttr` 一直存在但没用在这里 | 已修：`esc` → `escAttr` |
| 2 | `models.go:252` | 8 处 `modelsCache` 读点里唯一一处**裸读** `len(modelsCache)`（其余 7 处都持有 `modelsMu`） | 已修：只取一次快照 |
| 3 | `admin_providers.go` cline `syncedAt` | 零值时间未判空就格式化 → 前端 `new Date(...)` 显示 `Invalid Date` | 已修：零值返回空串 |
| 4 | `admin_html.go` 空态行 | `colspan="5"` 硬编码，而 cline=5 列 / opencode=4 列 / 通用=3 列 → 空态行错位 | 已修：按表头 `<th>` 实际列数动态生成 |
| 5 | `proxy.go` / `sub.go` | 启动时把 **admin 访问令牌**和**完整订阅 URL**（通常带 token）打进日志 | 已修：令牌只写「已省略 + 落盘路径」；订阅 URL 走 `maskURLForLog`（掩掉 User / RawQuery / RawFragment） |

### 2.2 安全与凭据（原文丢失，仅存第 7、8 两行）

> 原表第 1–6 行在本文件截断中丢失，未重建。第 7 项已由 §5 第 2 项修掉，
> 第 8 项列入 §8 本轮修复清单。

| # | 位置 | 问题 | 建议 |
|---|---|---|---|
| 7 | `sub.go:157,163` | `log.Printf("订阅 %s ...", u)` 把**完整订阅 URL** 打进日志 —— 订阅 URL 通常带 token | 复用 `maskProxyURL`，或只打 host + 查询参数名；日志文件权限降到 0600 |
| 8 | `admin_html.go:1684,1718,1725,1730,1747-1753` | 内联 onclick 里 `escJs(n)` 的安全性**完全依赖后端** `providerIDRe`（`^[a-z][a-z0-9_-]*$`），JS 侧零校验 | 前端 `saveProvider` 入口加同名正则校验并 toast 拒绝 |

### 2.3 架构与并发

| # | 位置 | 问题 | 建议 |
|---|---|---|---|
| 9 | `nodes.go:114-232` | `syncNodeBox` **全程持 `nodeMu`** 完成 build+start（`freeLocalPort`/`box.New`/`instance.Start` 可达秒级） | 锁内只做 map 快照+原子替换；box 构建挪锁外并发化，失败丢弃 |
| 10 | `nodes.go:132` | `syncNodeBox` 持 `nodeMu` 时取 `subMu`，锁序未声明 | 显式声明 `nodeMu→subMu` 唯一序；`resolveSubscriptions` 先快照释放 `subMu` |
| 11 | `proxy.go:749` | `isRetryableUpstreamError` 靠**错误字符串匹配**；上游改文案就静默降级，调用方无差别 502 | `callClineAPI` 返回带 Status 的 `upstreamError`，与 `providerError` 对齐 |
| 12 | `proxy.go:95,106`、`models.go:280`、`zen.go:920`、`providers_catalog.go:704`、`cooldown.go:176`、`nodes.go:414` | 6 个后台循环 + `refreshSubsLoop` + `syncNodeBox` 内 sleep goroutine 全部 `for range ticker.C` 无 ctx | `signal.NotifyContext` 生成 rootCtx，全部 `select ctx.Done` |
| 13 | `proxy.go:439` | `ListenAndServe` 无 graceful shutdown；Ctrl-C 后 sing-box / http.Client / 日志句柄残留 | `server.Shutdown(ctx)` |
| 14 | `logs.go:160-177` | `flushReqLogs`/`closeReqLogs` **只有测试调用**；点托盘"退出"直接退出，channel 里排队未落盘的请求日志全丢，`nodeBox` 也不 Close | 退出路径调 `closeReqLogs()` + `nodeBox.Close()`，3-5s 超时兜底 |

### 2.4 前端状态与竞态

| # | 位置 | 问题 | 建议 |
|---|---|---|---|
| 15 | `admin_html.go:1597` + `1338-1339,1804-1805,1815-1816,1850,1918-1923` | `loadModelIndex` 无 seq 保护，同时存在多个 `setTimeout(loadModelIndex, 3000/12000)` —— 慢响应覆盖快响应 | 模块级 `let seq=0`，`const s=++seq; ... if(s!==seq) return` |
| 16 | `admin_html.go:1696-1702,1749-1852` | `toggleProviderModel` 乐观更新 `pvData` 后整表重建；搜索强制展开与 `pvOpenSet` 语义冲突 | 重绘前快照搜索词与勾选态，或只 patch 受影响行 |
| 17 | `admin_html.go:905` | `!data.success && data.error` 才抛错；`success:false` 且 `error:''` 时按成功路径渲染 | 改 `if(!data.success) throw new Error(data.error \|\| '请求失败')` |

### 2.5 可访问性

| # | 位置 | 问题 | 建议 |
|---|---|---|---|
| 18 | `admin_html.go:129` | 全局 `outline:none` 且**无 `:focus-visible` 兜底** —— 所有元素键盘聚焦无视觉反馈 | 删 `outline:none`，加 `:focus-visible{outline:2px solid var(--accent);outline-offset:2px}` |
| 19 | `admin_html.go:1684` | `.ps` 卡片头仅 `onclick`，缺 `role="button"` / `tabindex="0"` / `aria-expanded` / `onkeydown`（参考实现 `pages.ts:376` 全有） | 全补，加 Enter/Space 处理 |
| 20 | `admin_html.go:1693` | `.pd` 展开区无 `id`，`aria-controls` 无引用目标 | `.pd` 加 `id="dt-<n>"` |
| 21 | `admin_html.go:1718,1725,1730` | `.copy-icon` 是 span + onclick，无 `tabindex`/`role`/`aria-label` | 改 `<button type="button" aria-label="复制 <id>">` |

### 2.6 测试有效性

| # | 位置 | 问题 | 建议 |
|---|---|---|---|
| 22 | `nodes.go:109-233` | `syncNodeBox` 回滚保护（failKeepOld）**零测试覆盖**；`exit_unified_test.go` 4 个用例全被 `requireNodeBox` 跳过 | 在 `startNodeInstance` 注入错误，验证 failKeepOld 后 `nodeBox`/`nodePorts`/`catchAllPort` 不变 |
| 23 | `nodes.go:341-360` | `healthRunAgain` 重入补跑零测试 | 并发 goroutine 调 `checkAllNodeHealth`，第一轮注入新节点，验证第二轮跑了 |
| 24 | `scripts/admin-render-test.js` | DOM stub 空转（见 §0.2）+ esc 比生产严 | stub 实现 `closest` 走 parentElement 链、`querySelectorAll` 返回真实元素列表；esc 语义对齐生产 |
| 25 | `admin_providers.go:76-128` | `builtinProviderEntries` 无"cline 不可用（`getFreeModels` 空）"测试 | `TestBuiltinEntriesClineUnavailable` |
| 26 | `admin_html.go:1608-1628` | `providerModels` 三来源归一化零测试 | 构造三种数据断言归一化 |
| 27 | `admin_auth.go` | 鉴权无并发测试 | 10 goroutine 并发请求 |

### 2.7 运维可见性

| # | 位置 | 问题 | 建议 |
|---|---|---|---|
| 28 | `stats.go:145,226` | `zen-stats.jsonl` 无上限不轮转，启动时 `os.ReadFile` **全量读**重建聚合 | 只重建"今天+昨日"，冷数据按日期分文件 |
| 29 | `tray_windows.go:141-143` | 托盘只有"打开管理界面"/"退出"，**没有打开日志目录** | 加"打开数据目录" + "导出诊断包"（data/ 快照 + 日志尾部 + /health + 节点健康 + 端口占用打 zip） |
| 30 | `proxy.go:121,129` / `admin.go:1012` | 版本字符串硬编码 `"go-1.1"`，不来自 `-ldflags -X` | `-ldflags "-s -w -X app.buildVersion=$(git describe)"` |
| 31 | `proxy.go:118-132` | `/health` 只回 status/version/activeAccounts，回答不了"网关自身健康吗" | 加 `nodePool`/`exitReady`/`lastSubFetch`/`subNodes`/`logBytes`/`dropped` |

## 3. P2（建议，摘录）

- `routing_dispatch.go:50` + `proxy.go:567,1674` + `admin_router.go:31`：三上游类型在 5-7 处 switch 分发，**加一个新上游要改 7 个文件**。抽 `Upstream{ Dispatch(ctx,params,stream)(*http.Response,error); ListModels()[]Model }` 接口可降到 2 处。
- `proxy.go:137-172`：`apiKeyHandler` 每请求 `loadPool()` + 线性匹配 Keys，上千 key 时是热点。
- `admin_html.go`（2335 行）：HTML+JS 单文件裸字符串，无 lint、无前端类型检查；改前端必须重编译 Go。短期拆多块 const + CI 抽 JS 跑 eslint，中期改 `//go:embed`。
- `admin_html.go:184`：`.pbadge` 有 `nowrap` 无 `max-width`，60 字符错误信息溢出被 `.pi{overflow:hidden}` 截断。
- `admin_html.go:1788-1797`：`filterCardModels` 全隐藏时无空态提示（用户搜错词看到空表）。
- `admin_html.go:83,446-449`：`.section-title` 无 `flex-wrap`，≤560px 窄屏搜索框溢出。
- `nodes.go:998-1005`：`normalizeSSMethod` 只映射 chacha20 三种别名，`AES-128-CFB` 等大写写法原样透传。
- `admin.go:909`：`saveProxyConfig` 用 `os.WriteFile`（先截断再写），是全仓唯一非原子写；改 `kit.WriteFileAtomicDefault` 一行搞定。
- `nodes.go:457,469`：每节点 1 个常驻本地 mixed 监听 → N 节点 = N+1 个常驻端口，重建时瞬时 ~2N。
- `kit/http.go:24-26`：`kit.HTTPClient` 无 Timeout，调用方忘设 context 会永久挂住。
- `logs.go:53,78`：请求日志缓冲满即丢弃，计数只 `atomic.Add` **永不暴露**。
- `admin_html.go:936,1356,1392,1445,2320`：5 处 `catch(e){/*ignore*/}` 静默失败。
- `admin_html.go:1185 vs 1320`：`copyText` 定义两次，后者覆盖前者；`.pbadge.pb-info` 死 CSS。
- `admin_html.go:1830-1839`：`delete existing.X` 黑名单硬编码 8 个字段，新增字段易漏。
- `providers_config.go:52` 的 `DisplayName` 配置侧叫 `name`、响应侧叫 `display`，同一东西两套名字。
- cline / opencode 的 `models` schema 完全不统一（`status/cost/requiresStream/syncedAt` vs `context/output`）。
- Dockerfile：无 HEALTHCHECK、`ports: "3457:3457"` 宿主默认绑 0.0.0.0、无多平台。
- 模型列表页的启用勾选与自动路由页的勾选是**两条独立状态**，改一处另一处不重绘。
- 目录型 provider（openrouter 300+ 模型）无虚拟化/分页，一次性渲染所有 `<tr>`。
- 搜索无 `<mark>` 高亮。

## 4. 已复核确认「没问题」的点

| 结论 | 依据 |
|---|---|
| `syncNodeBox` 回滚保护逻辑正确：先建新实例、成功才 Close 旧实例；三条失败路径均不修改全局状态，无半替换窗口，`catchAllPort` 失败时保留旧值 | `nodes.go:154-226`，架构与后端两位独立确认 |
| `healthRunAgain` 脏标记读写均在 `nodeHealthRunMu` 内，无竞争；defer 先置 `nodeHealthRunning=false` 再读标记，不存在"永远补不上"或"无限循环" | `nodes.go:342-356` |
| 管理后台只绑 `127.0.0.1`（`proxy.go:27`），鉴权不可绕过：Origin 预检排在令牌校验之前、`subtle.ConstantTimeCompare`、Cookie HttpOnly+SameSite=Strict、`originAllowed` 正确拒绝公网域名 | `admin_auth.go:90-169`，运维与后端两位独立确认 |
| `modelsCache` 其余 7 处读点全部正确持有 `modelsMu`（`admin.go:1086`、`proxy.go:1209`、`zen.go:148,180`、`pool.go:137`、`models.go:242`） | lead 逐处复核 |
| `modelsSyncStamp()` 锁保护正确，消除的正是上一轮同类"裸时间戳读出锁外"竞态 | `models.go:89-96`，测试 `builtin_provider_entries_test.go:84-104` |
| 内置供应商（cline / opencode）无勾选/编辑/删除/连通测试按钮，分支隔离干净 | `admin_html.go:1749` |
| 复制前缀三口径（`cline/`、`zen/`、`name:`）在 `1709/1721/1727` 全部正确；`escJs` 替换顺序正确（`&` 最先、`\` 早于 `'`） | 前端确认 |
| 死代码零残留：`loadModels`/`loadOcModels`/`loadProviders`/`renderProviderList`/`togglePvModels`/`renderPvModelBlock`/`ocModelsList`/`modelsList`/`modelsProbeInfo` 全仓零命中 | 前端 grep 交叉验证 |
| 状态色语义正确：红=异常、绿=就绪、黄=需订阅、灰=下架 —— 这是**系统健康度语义，不是行情涨跌**，不需要按"涨红跌绿"改 | UX 明确判定 |
| 暗色模式全 CSS 变量驱动（`:root` + `[data-theme="light"]` 双套），无写死浅色背景或黑字 | UX |
| 配置崩溃恢复：`WriteFileAtomic` 写临时文件 + fsync + rename，坏配置改名 `.bad-时间戳` 留证而非删除 | 运维 |
| 端口冲突处理克制：只杀与自身同名的旧进程、绝不自杀、无关进程不动 | `proxy.go:2457-2501` |
| **PM 报的 P1「`providerModels()` meta 键永远对不上」是误报**：三家 `catalogModels[].id` 全是**裸名**（`admin_providers.go:87,108`、`providers_catalog.go:636`），meta key 也取 `m.model` 裸名（`admin_html.go:1611`），裸名比裸名能命中。混淆了 `models[].id`（带前缀）与 `models[].model`（裸名） | lead 逐处复核推翻 |

## 5. 优先级行动清单

| 序 | 事项 | 类型 |
|---|---|---|
| 1 | `admin_html.go:1682-1683` 两行 `esc` → `escAttr` | 1 行，堵 XSS |
| 2 | `cline-proxy.log` 加轮转 + 权限 0600，顺手修掉 admin token 与订阅 URL 两处入日志 | 解 P0 + 2 个 P1 |
| 3 | 拆 `nodes_tls_sanitize_test.go` 两个红测试 | 恢复 CI 绿灯 |
| 4 | `models.go:252` 改 `len(getFreeModels())` | 1 行 |
| 5 | `admin_providers.go:106` 零值时间返回空串 | 前端不再显示 Invalid Date |
| 6 | `admin_html.go:1757` colspan 按表头列数动态生成 | 空态行不再错位 |
| 7 | 修渲染测试 stub（`closest`/`querySelectorAll`/esc 对齐生产）+ 补**渲染值断言** | 消掉假阳性 |
| 8 | 补 `syncNodeBox` 回滚 + `healthRunAgain` 重入并发测试 | 覆盖最高风险路径 |
| 9 | `syncNodeBox` 锁外构建 + `models`/`subNodes`/`candidateCools` 改 accessor 快照 | 架构级 |
| 10 | 托盘加"导出诊断包" + 版本注入 + `/health` 补网关自身状态 | GUI 场景可运维 |
| 11 | 进程级 rootCtx 收网（6 个后台循环 + `server.Shutdown`） | 消 goroutine 泄漏 |
| 12 | 补键盘可达性（`.ps` role/tabindex/aria-expanded + `:focus-visible`） | 无障碍 |

## 6. 本轮评审没覆盖到、不能替它下结论的部分

**诚实声明，以下三项我没有独立验证，不在本报告里给结论：**

1. **产品经理第二轮的两个方向因网络中断未交付**（`502 getaddrinfo ENOTFOUND token.sensenova.cn`）：
   - ① `admin-render-test.js` 65 条断言的"结构 / 行为 / 值"分布与空白区清单；
   - ② **用户手工填的、不在上游目录里的模型（自建中转、私有模型名）会不会直接从卡片上消失** —— `providerModels()` 里 `catalogModels` 优先于 `modelEntries`，而 `catalogModels` 只来自 `p.catalogModels()`（`providers_catalog.go:633` 有 `isChatModel` 过滤）。这条如果成立，比"下架模型无占位"严重得多：用户在模型列表页勾了、路由页也配了，卡片上却看不到。
   - lead 只验证了 §4 里那条 meta 键匹配，**没有**验证手工模型路径。
2. **没有真实浏览器环境**，UX 全部结论来自静态阅读 CSS/JS，未实际渲染验证。
3. **没有负载/长跑验证**：数千节点 × 自适应并发 48 的真实连接数、`syncNodeBox` 重建时的端口/句柄峰值，均未经实测。`logs.go:53,78` 的"缓冲满即丢弃"在真实压力下丢了多少也无数据。


## 7. 第 7 项实施记录（渲染测试补盲区）—— 顺带挖出一个真 bug

### 7.1 做了什么

`scripts/admin-render-test.js` 从 67 条断言扩到 **98 条**，新增 5 组：

| 组 | 内容 | 堵的盲区 |
|---|---|---|
| `[13]` | `providerModels` join 不变量：裸名 ↔ 裸名、`on` 传递、附加字段不丢 | PM 假阳性那类「meta 键永远匹配不上」 |
| `[14]` | **渲染值断言**：费用「免费」/ 状态「可用」/ 同步时间 / 上下文 1,000,000 / 输出 65,536 | 「渲染成空壳但断言全绿」 |
| `[15]` | `title` 属性上下文 XSS + `esc` 与 `escAttr` 语义自检 | `[12]` 只测 body 上下文，抓不到属性注入 |
| `[16]` | `filterCardModels` 显隐逻辑单测（受控替身，4 个分支） | 靠 DOM 遍历的处理器此前完全没跑 |
| `[17]` | 显式盲区清单 + `El.closest` 自身可用性断言 | 不让 `ALL PASS` 被误读成全覆盖 |

同时修掉测试自身的两个失真点：
- **`esc` stub 比生产更严**：原来多转了 `"`。生产 `esc` 只转 `& < >`，stub 转了引号会让「title 属性双引号闭合注入」被掩盖 —— 测试全绿而生产是漏洞。已对齐。
- **`El.closest` 硬编码返回 `null`**：现沿 `parentElement` 真实向上找。

### 7.2 反向验证（证明新断言真的会咬人）

故意把修复撤掉再跑，确认断言失败而非空转：

| 撤销的修复 | 测试结果 |
|---|---|
| `providerModels` join 去掉 `requiresStream` | `FAIL cline 流式标记存在` |
| 徽章 `title` 的 `escAttr` 改回 `esc` | `FAIL title 属性值里没有裸双引号` → `"上游返回 \" + document.cookie + \""` |

恢复后重新全绿。这两条是**有证据的**覆盖率，不是「我加了断言」的自我声明。

### 7.3 顺带挖出的真 bug（P1，本轮已修）

`providerModels()` 的 join 漏了 `requiresStream`：

```js
return { id, on, context, output, status, cost, syncedAt }   // ← 没有 requiresStream
```

而 cline 行渲染靠 `m.requiresStream` 画「流式」标记 → **这个标记从来没渲染出来过**。
它不是装饰：`models.go:30` / `proxy.go:1233` 的同名字段是网关**真实决定走不走流式**的依据。
后台发（`admin_providers.go:112`）、代理用、UI 显示 —— 三处口径不一致，用户在管理页
看不到哪些模型需要流式，排障时会对不上。

修：join 补 `requiresStream: m.requiresStream`，并加注释说明与 `proxy.go` 口径必须一致。
`deepseek/deepseek-v4-flash`、`stepfun/step-3.7-flash` 两个模型现在会正确显示「流式」。

### 7.4 本轮实测（真实进程，不是测试替身）

起了编译产物跑通一遍再杀掉：

- `GET /admin/?token=<真令牌>` → **200，121,039 字节**，响应体里两处修复都在。
- 日志凭据扫描：无 `api key` / `Bearer` / 明文 `token=` / `?token=` 链接。
  启动行现在是 `admin panel: http://127.0.0.1:3457/admin/ (访问令牌已省略, 落盘在 data/admin-token)`。
- 构建：`CGO_ENABLED=0` + `with_quic,with_grpc,with_utls` + `-H=windowsgui` →
  `dist/cline-proxy.exe`，41,608,192 字节，PE32+ GUI x86-64。
- `gofmt` 干净、`go vet` 干净、无标签 / 带标签两组 `go test ./internal/...` 全 `ok`。

### 7.5 实测发现的新问题（本轮未修，记录待办）

1. **`0600` 在 Windows 上不生效** —— 实测 `cline-proxy.log` 仍是 `644`。Go 的 Windows
   后端忽略 `os.OpenFile` 的 mode 位。代码里的 0600 是无害但无效的安慰，**真正生效的
   是不把令牌写进日志**（本轮已做）。注释已同步更正，不再声称权限位能挡住什么。
2. **`cline-proxy-stream.log` 没有轮转上限**（`proxy.go:1981`，每次 anthropic 流式请求
   OpenFile 追加）。内容只有出站 SSE 事件（模型输出文本），**不含请求头与 API key，
   不是凭据泄露**，但会无限增长，且没有 10 MiB 截断。建议并入下一条轮转逻辑。
3. **`/admin/` 不带 token 返回 200 + 完整 HTML shell（121 KB）**。这是 SPA 模式，
   API 端点仍强制校验令牌、未被绕过；但静态壳（含全部前端 JS 逻辑）对同机任意进程可见。
   属既有设计取舍，非本轮引入 —— 若同机多账号共享，可考虑对无令牌请求返回 401。

### 7.6 仍未覆盖（沿用 §6 的诚实声明）

§6 的第 2 点（PM 提到的「用户手工填、不在上游目录的模型会不会从卡片消失」）**仍然未验证**。
本轮加的 `[13]` 只覆盖了「catalogModels 存在」的路径；`catalogModels` 为空时走 `modelEntries`
兜底、再为空才回退 `Object.keys(meta)`，这条链路的真实行为我没有实测。
`[17]` 也把 `filterModelIndex` / `toggleProviderModel` / 全部 `save*` / `api` / `toast`
显式列为未覆盖 —— 要看真浏览器或引 jsdom，本轮没做。

## 8. 第二轮全量修复记录（2026-09-13 20:00–20:50）

按 §5 行动清单把专家团队找出的问题**全量落地**。按文件所有权切成三路并行
（fe-fix / be-fix / ops-fix），避免并发写同一文件互相覆盖。

### 8.1 过程里发生的事（必须记录）

- **两个工人在执行中被 429 频率限制打死**（be-fix 21:13、ops-fix 21:46 重置点），
  来不及回报。它们的改动**已经落在磁盘上**，只是没有自报清单。
- 我没有替它们编造报告，改为**亲自验收磁盘上的代码**：跑 gofmt/vet/三套测试，
  再逐条比对它们的任务清单看哪些条目落地、哪些缺项。
- **be-fix 死于半截并留下编译错误**：`nodes.go:138` 写了
  `if ... && startNodeInstanceFn == startNodeInstance` —— Go 里 func 只能和 nil
  比较，包编译不过。改成 bool 注入标记位（`startNodeInstanceInjected`）。
- **同一个半成品还留了第二个坑**：`sub_test.go` 仍调旧函数名
  `saveSubCacheLocked()`，而实现已重命名为 `saveSubCache(nodes []any)`
  （传快照、内部不再取锁）。已改测试并顺手加一条断言把「持锁时调用不死锁」钉死。
- 报告文件头部损坏（§0–§2.1 丢失）已在开工前重建，见文首「文件损坏声明」。

### 8.2 落地清单

#### 前端（fe-fix，已完整回报）
- `saveProvider` 入口加 `providerIDRe` 校验，不再只靠后端兜底（§2.2-8）
- `mIdxSeq` 序号保护 `loadModelIndex`，所有 `setTimeout(loadModelIndex,…)` 收口（§2.4-15）
- `toggleProviderModel` 改**只 patch 受影响卡片**（新增 `renderOneCard`）（§2.4-16）
- `!data.success` 无条件抛错（§2.4-17）
- 删 `outline:none` + 加 `:focus-visible`；`.ps` 补 role/tabindex/aria-expanded/
  aria-controls/onkeydown；`.pd` 加 id；`.copy-icon` span→`<button>`（§2.5-18~21）
- 渲染测试 98 → **104 条断言**，新增 `[18]` 组覆盖 `providerModels` 三来源归一化（§2.6-26）
- §3 P2 前端条目：`.pbadge` 截断、空态占位、`flex-wrap`、5 处静默 catch、
  `copyText` 去重（删掉会回显明文到 toast 的那个版本）、8 字段删除黑名单改白名单、
  搜索 `<mark>` 高亮（先 `esc` 再插标签）

#### 后端（be-fix 死于 429，lead 验收 + 补齐）
- **§2.3-9 锁外构建（最重要）**：`syncNodeBox` 不再全程持 `nodeMu`。锁内只做
  `prevBox/prevPorts/prevCatchAll` 快照 + 原子替换；`freeLocalPort` / `buildNodeParts` /
  `box.New` / `Start` 全在锁外。三条失败路径都不改全局状态，旧实例继续服务，
  `catchAllPort` 失败时保留旧值 —— §4 确认过的 failKeepOld 性质全部保住。
- **§2.3-10 锁序声明**：`nodeMu → subMu` 是唯一合法顺序，`sub.go` 与 `nodes.go`
  两处都加了注释；`resolveSubscriptions` 改为「持锁快照 → 解锁 → 再落盘」，
  `saveSubCache` 重构为接收快照且内部不再取锁。
- **§2.6-22 回滚测试**（be-fix 完成）：`nodes_sync_rollback_test.go`，用
  `startNodeInstanceFn` 注入错误，断言 `nodeBox`/`nodePorts`/`catchAllPort` 三个值
  在 failKeepOld 后与旧快照**完全相等**。能在 `CLINE_PROXY_SKIP_NODEBOX=1` 下跑
  （走注入错误路径，不需要真起 sing-box）。
- **§2.6-23 重入补跑测试**（be-fix 未做，**lead 补写**）：
  `nodes_health_reentry_test.go`，注入 `testNodeComprehensiveFn` 替身 + channel 门闩卡住
  第一轮，中途重入，断言探测被调用**恰好 2 次**（第一轮 1 次 + 补跑 1 次），
  并断言结束后 `nodeHealthRunning`/`healthRunAgain` 都归零。
  另加空池边界用例。
- **§3 normalizeSSMethod**：核实发现**早已大小写不敏感**（`strings.ToLower`）且已有
  测试，报告这条已过期。但报告点名的 `AES-128-CFB` 大写场景**无测试覆盖**，已补：
  全大写/混合/带前后空格/表外大写不改写/空串/纯空格。
- **§5-9 后半 + §2.1 models 快照**：新增 `modelsCacheSnapshot()` accessor，
  `getFreeModels()` 走它；核查全部 6 处 `modelsCache[id]` 点查找
  （admin.go/pool.go/proxy.go/zen.go×2/models.go）**全在 `modelsMu` 内**，
  `subNodes` 2 处裸读也**全在 `subMu` 内**，`candidateCools` 全部访问点在
  `candidateCoolMu` 内 —— **be-fix 承诺要交的跨文件待办清单为空**。

#### 运维与生命周期（ops-fix 死于 429，lead 验收）
- **§2.3-12 进程级收网**：`signal.NotifyContext` 建 `appRootCtx`，新增
  `Shutdown()` 统一收口。
- **§2.3-13 graceful shutdown**：`doGracefulShutdown` 用 `shutdownOnce` 保护，
  `appServer.Shutdown` 5s 超时兜底。
- **§2.3-14 退出刷盘**：`closeReqLogsTimed(3s)` + `closeNodeBoxTimed(3s)` +
  `closeStreamLog()`，超时尽力而为不阻塞退出。
- **§2.3-11 上游错误口径**：`callClineAPI` 返回 `*upstreamError` 带 `Status`，
  `isRetryableUpstreamError` 改按状态码判定，原字符串判断保留作兜底。
- **§2.7-28 zen-stats 轮转**：`maxZenStatsBytes` 上限 + 启动只读尾部
  `statsReadTailBytes`。
- **§2.7-29 托盘**：新增「打开数据目录」+「导出诊断包」
  （`exportDiagnostics` → `data/diag-<时间戳>.zip`）。
- **§2.7-30 版本注入**：`buildVersion` 默认 `"go-1.1"`，支持
  `-ldflags "-X cline-go-proxy/internal/app.buildVersion=…"`。
- **§2.7-31 /health 扩充**：`nodePool`/`exitReady`/`lastSubFetch`/`subNodes`/
  `logBytes`/`dropped`。
- **§7.5-2 stream log 轮转**：复用 `maxLogBytes`，含 Windows 下 `Truncate` 后
  显式 `Seek` 回文件头的处理。
- **§7.5-3 无令牌不返回全壳**：`/admin/` 无 token 返回 1,208 字节提示页
  （原来 121 KB 完整 SPA 壳）。
- **§3 其余**：`admin.go` 改 `WriteFileAtomicDefault`；`kit.HTTPClient` 加
  30s `Timeout`；请求日志丢弃计数暴露到 `/health` 的 `dropped`；Dockerfile 加
  `HEALTHCHECK`、容器内显式 `-host 0.0.0.0`、单平台注释。

### 8.3 两个新挖出的 bug

1. **通用 Provider 开了 catalog 时，用户手工填的私有模型从卡片上彻底消失**
   （fe-fix 验证 §6 缺口时发现，**已修**）。
   `providerModels` 直接用 `catalogModels` 当 base，只存在于 `p.models` 的私有模型
   被丢掉 —— 用户在模型列表页勾了、路由页配了，管理页看不到。
   修法：非内置 Provider 才把 `meta` 里不在 base 的键补回（`on = enabled!==false`）；
   cline/opencode 不并入以免整表炸开。新 `[18]` 断言锁定该行为。

2. **cline 的「流式」标记从来没渲染出来过**（§7 已记录，`providerModels` join
   漏 `requiresStream`，已修）。

### 8.4 反向验证（有证据的覆盖率，不是自我声明）

| 故意制造的回归 | 测试反应 |
|---|---|
| `providerModels` join 去掉 `requiresStream` | `FAIL cline 流式标记存在` |
| 徽章 title 的 `escAttr` 改回 `esc` | `FAIL title 属性值里没有裸双引号` |
| 撤掉「私有模型补回」修复 | `FAIL 私有手工模型出现在卡片` → `[{"id":"up-a","on":true}]` |
| 禁用 `healthRunAgain` 补跑 | `FAIL 探测被调用 1 次, 期望 2` |

（第一次尝试删除整段补跑代码会让 `again` 变成未使用变量而**编译失败**，
改用 `if again && false` 模拟「补跑被关闭」才拿到真正的断言级失败。）

### 8.5 全量校验（真实输出）

| 检查 | 结果 |
|---|---|
| `gofmt -l` 本轮改动文件 | 干净 |
| `go vet`（无标签 / 带标签） | 均干净 |
| `go test ./internal/...`（无标签） | `ok` 2.483s |
| `go test -tags with_quic,with_grpc,with_utls` | `ok` 2.769s |
| `go test -race -count=1 -tags …` | `ok` 4.771s（0 处竞争） |
| 渲染测试 | `ALL PASS`，**104 条断言** |

> `gofmt -l` 会列出 9 个文件（`admin_zen_test.go` / `config_clone.go` /
> `providers_config.go` / `routing_dispatch.go` / `auth.go` / `openai_anthropic.go` /
> `streaming.go` / `doc.go` / `provider.go` / `router.go`）。核查确认这 9 个是
> **HEAD 本身就不干净**（制表符/空格类的注释噪声），与本轮改动无关，按约定不碰，
> 避免在安全修复的 diff 里混入无关重排。

### 8.6 真实进程冒烟（不是替身）

`CGO_ENABLED=0` + 三构建标签 + `-H=windowsgui` + 版本注入 →
`dist/cline-proxy.exe`，41,688,576 字节，PE32+ GUI x86-64。

| 探测 | 结果 |
|---|---|
| `GET /health` | 200 / 158 字节，`version = v-team-review-09132041`（**注入生效**），`nodePool`/`exitReady`/`lastSubFetch`/`subNodes`/`logBytes`/`dropped` **全部存在** |
| `GET /admin/`（无 token） | 200 / **1,208 字节**提示页（原 121 KB），提到「令牌」5 次，**前端 JS 泄露 0 处** |
| `GET /admin/?token=<真令牌>` | 200 / **127,701 字节**完整页面，两处修复标记都在响应体 |
| `GET /admin/api/config`（无 token） | **401** |
| `GET /admin/api/config?token=<真令牌>` | **200**（鉴权不可绕过） |
| 日志凭据扫描 | 命中 **0** 行；启动行 `admin panel: http://127.0.0.1:34579/admin/ (访问令牌已省略, 落盘在 data/admin-token)` |

### 8.7 刻意不改的一条（记录取舍）

`providers_config.go:52` 的 `DisplayName` 在磁盘配置里叫 `name`、在 API 响应里叫
`display`（报告 §3 提出统一）。**决定不改**：`name` 只存在于磁盘配置序列化、
`display` 只存在于只读响应，两者从不在同一序列化边界出现，前端也不回写
`display`，所以这纯粹是命名口味而非缺陷。直接改 JSON 键名会让老配置静默把展示名
读成空串 —— 报告本身也标了这个风险。要真统一，代价是自定义 `UnmarshalJSON`
做双读兼容，为一个纯展示字段的命名一致性引入真实代码风险，不值得。

### 8.8 仍未覆盖（诚实声明）

1. **没在真实浏览器里跑过**。渲染测试是 DOM stub，`[17]` 已显式列出未覆盖项
   （`filterModelIndex` / `toggleProviderModel` / 全部 `save*` / `api` / `toast`）。
   要看真浏览器或引 jsdom。
2. **诊断包导出（`exportDiagnostics`）只验证了编译进二进制**，没有点击托盘菜单
   实测生成的 zip 内容 —— 那是 GUI 交互，headless 环境点不到。
3. **`appRootCtx` 收网只覆盖了 `proxy.go` / `main.go` 手里的循环**。报告 §2.3-12
   点名的其余后台循环（`models.go` / `zen.go` / `providers_catalog.go` /
   `cooldown.go` / `nodes.go` 里的 `for range ticker.C`）本轮**没有逐个改成
   `select + ctx.Done()`** —— 它们靠 `Shutdown()` 取消 rootCtx 后进程退出间接结束。
   若要严格收口，需要把 `appRootCtx` 传进那 4 个文件，属于跨文件改动，留待下一轮。
   `go test -race` 没有报出这些循环的竞争，但它们确实仍然无 ctx。
4. **`0600` 在 Windows 上仍不生效**（§7.5-1 已记录，Go 的 Windows 后端忽略
   `os.OpenFile` 的 mode 位，实测日志仍是 644）。真正生效的是「不把令牌写进日志」，
   本轮已验证日志凭据命中 0 行。

