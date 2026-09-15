# Cline-proxy 第二轮全面审计报告（增量复核 + ponytail 视角）

- **审计对象**：`C:\Users\Administrator\WorkBuddy\2026-09-13-12-31-02\Cline-proxy`
- **审计日期**：2026-09-15（第二轮，替代同日 18:29 的 `2026-09-15-full-audit.md` 中已过时的结论）
- **基线**：分支 `main` @ `a7eb3d6`（ahead of origin/main 54）；工作树干净，仅本文件未跟踪
- **审计方式**：**纯只读**。git diff 增量复核 + 必需标签下 `go vet` / 全量 `go test` / `gofmt -l` / 前端渲染冒烟。未修改任何业务代码，未引入依赖，未向外部服务上传代码（CodeRabbit CLI 未安装）
- **审计框架**：ponytail-audit（复杂度/可删减）+ superpowers 工作纪律（验证先于结论、报告与推测分开）
- **增量范围**：`5403cbd..a7eb3d6`，16 个提交，+2492 / -46 行，28 个文件

## 0. 执行摘要

上一轮审计后的 16 个增量提交整体质量**高**：每个功能都带注释、出处（OmniRoute/MIT 声明）和配套测试；网关全量测试、vet、前端冒烟全部通过。上一轮定位的检测 bug（`node_probe.go` 提前 `cancel()`）已被修复并有回归用例。

本轮新发现 1 个 P1 数据竞争（`exit_fold` 面板读取无锁）、若干 P2/P3 一致性与死代码问题；ponytail 视角下最大的结构性风险是**新 `internal/translate` 包（约 1217 行）尚未接入任何生产调用点**，与旧 `internal/app/translate` 同名并存，目前是"预铺的基础设施"。

另外，本轮审计确认：**用户在 ZCode 会话里反复遇到的 `bai:qwen3.8-flash` "Bad control character / _manifest C2PA" 报错，与仓库 `json_to_sse.go` 注释里点名的"实测 B.AI 图片响应内含巨型 C2PA _manifest"是同一根因**，网关侧的修复（位置感知控制字符清洗 + 坏行门卫 + 非 SSE 合成）已在 `5bb1686` 落地。只要运行中的 exe 是用该基线重新构建的，经网关的这类响应就不会再把脏 JSON 递给客户端。

## 1. 验证台账（全部在基线 a7eb3d6 复现）

| # | 检查项 | 命令 | 结果 |
|---|---|---|---|
| V1 | 静态检查 | `go vet -tags "with_quic,with_grpc,with_utls" ./...` | 通过 |
| V2 | 全量测试 | `go test -count=1 -tags "with_quic,with_grpc,with_utls" ./internal/...` | `app` / `protocol` / `providers` / `translate` 全部 ok |
| V3 | 前端冒烟 | `node scripts/admin-render-test.js` | ALL PASS，退出码 0 |
| V4 | 格式化 | `gofmt -l internal main.go` | 4 个文件未格式化：`internal/cline/auth.go`、`internal/providers/{doc,provider,router}.go`（上轮为 7 个，协议层 3 个已被增量提交顺手修掉） |
| V5 | 工作树 | `git status` | 干净（仅本报告未跟踪），审计全程零代码改动 |
| V6 | 旧 P0 修复复核 | `git diff 5403cbd..HEAD -- node_probe.go` | 三处 `cancel()` 均已移到响应体 Close 之后，含注释与回归用例 `TestProbeNodeSpeedReadsBodyBeforeCancel` |
| V7 | 增量测试覆盖 | `node_filter_test.go` 新增用例清单 | 覆盖控制字符清洗、SSE 合成、idle 中断、early-EOF 探测、gemini 双向翻译——增量功能有测试，符合仓库纪律 |

## 2. 增量提交逐块结论

### 2.1 流式四件套（5bb1686 / 85708b4 / 47c219c / c988d99 / 373b968）✅ 整体高质量

- `controlSanitizingReader`：位置感知（字符串内 `<0x20` 全替换、字符串外放行 \t\n\r），状态跨 Read 保持，实现正确；
- `json_to_sse.go`：NDJSON 逐行合成 + 完整 JSON body 缓冲合成（64MB 上限），失败回退原样透传并打日志；
- `probeStreamFirstEvent`：early-EOF 用"已读前缀 + 续读句柄"合回 `prefixedBody`，无字节丢失；
- `idleAbortReader`：每读一 goroutine + scratch 私有缓冲 + 缓冲 channel，373b968 修掉了 -race 实抓的在途读竞态——修法正确；
- `HasStopSignal`：四协议终止形态判定，带测试。

**问题见 §3（F1/F2/F6/F7）。**

### 2.2 出口级去重（0b87fc8 的 exit_fold）⚠️ 方向对，有一处 P1

折叠算法（按实测 ExitIP 分组、延迟最低为主力、每轮检测后重算）合理，`exit_fold_test.go` 两个用例通过。但面板读取路径无锁，见 **F1**。

### 2.3 translate 新包（d920c07 / 3c7ae0c / c15cb1e / 88e212a）⚠️ WIP 未接线

注册表 + claude 双向 + gemini 双向，NOTICE 的 MIT 逐文件移植声明规范。**但 `GetRequestTranslator` / `GetResponseTranslator` 在生产代码里零调用点**（grep 证实），即约 1217 行目前只被自己的测试消费。见 **F4**。

### 2.4 provider key 级健康（b37f891 / cc0451f）✅

降级 key 排队尾、成功清零、与 `pickHealthyKeys` 共用常量避免语义分叉，注释交代了动机；`X-Proxy-Route` 头补齐 provider 路径日志可见性。无问题。

## 3. 本轮新发现清单

### F1（P1）node_view.go:130 无锁读 `exitFoldDupOf` —— 数据竞争

`withHealthResult` 直接 `exitFoldDupOf[key]` 读 map，而 `recomputeExitFold` 在 `exitFoldMu` 保护下整体替换该 map 变量。变量级读写并发即 Go 内存模型下的数据竞争：检测轮结束（订阅刷新触发）与面板拉取节点列表并发时，-race 理论上可报、实践中最坏读到半新半旧状态。当前测试没有同时踩这两条路径所以 CI 绿——属于**潜伏竞争**，不是已爆故障。修复方式极简单：在 `node_view.go` 处套 `exitFoldMu.RLock()`（或提供一个加锁访问器）。**本次审计未改动，留给你决定。**

### F2（P2）流中继"首行 SSE"分支与主循环行为不一致

`proxy.go` 约 1503-1520 行：首行是正常 `data:` 事件时直接解析回写，但——
1. 不走 `normalizeOpenAIResponse`（Cline `{data:{...}}` 包裹形态的首行会原样漏给客户端）；
2. 不调 `onUsage`（首行带 usage 的响应少记一笔账）；
3. 不更新 `sawFinish/sawDone/lastModel`（首行即 `[DONE]` 的极短流，收尾时会**再写一个 `[DONE]`，客户端收到重复终止帧**）；
4. 首行解析失败时静默吞掉（主循环有坏行门卫日志，这里没有）。

影响面小（首行通常是 role 帧），但属于"同一逻辑两份实现开始漂移"的典型苗头。

### F3（P2）控制字符清洗只覆盖流式路径

`controlSanitizingReader` 只挂在 `handleStreamResponseWithUsage`。若上游以 **非流式** 200 JSON 返回带裸控制字符的 C2PA 响应：`handleNonStreamResponseWithUsage` 的 `json.NewDecoder(...).Decode` 会失败 → 网关回 500 parse_error（比透传脏 JSON 好，但仍不可用）。建议评估把 `sanitizeJSONControlChars` 也接到非流式解码前（兜底路径其实已经写好了，只差调用点）。

### F4（P2）translate 新包未接线 + 双包同名

- 新 `internal/translate`（OpenAI↔Claude↔Gemini 双向）生产零调用 → 现在删掉不影响任何功能（ponytail `yagni:`），但 NOTICE 声明"分批移植中"，说明作者意图是批次⑤接线；
- 旧 `internal/app/translate`（注册表 + 出站形态不变量校验）与被 import 的包**同名不同路径**，且旧包 `[no test files]`——它守的恰是历史上出过"重复转换致 input 为空"事故的那条线。两包职责不同却同名，时间一长必然混淆；旧包无测试也与其"防事故"定位不匹配。

### F5（P3）死代码与配置无入口

| 项 | 位置 | 说明 |
|---|---|---|
| `exitFoldCount` | exit_fold.go:84 | 注释称"面板信号"，全仓无调用点（ponytail `delete:` 或接线到 /health） |
| `HasChoices` | protocol/normalize.go:80 | 增量后全仓零调用（含测试）（`delete:`） |
| `StreamIdleSecs` | zen.go:270 | 有字段有消费，但 admin 面板/`/zen/config/update` 无写入口，只能手改 JSON（`yagni:` 或补配置面） |

### F6（P3）probeStreamFirstEvent 注释与行为不符

头注释写"读到/超时（上游还在但首 token 慢）→ 提交"，实际实现是：空闲 90s 内没有任何有效事件 → 判空流**换站**（`err != nil → return true`）。行为本身可辩护（90 秒零字节基本等于死了），但注释承诺的语义没实现，排障时会误导。

### F7（P3）测速回归用例的防护强度偏弱

`TestProbeNodeSpeedReadsBodyBeforeCancel` 的 mock 服务器是"写完 64KB 后 sleep 5ms"。旧 bug（Do 返回即 cancel）下，首块数据很可能已进客户端缓冲区，被测出 0 字节断流的概率不高——用例对**新实现**验证充分，对**旧 bug 复现**偏弱。更稳的写法是把 sleep 放在每块写入**之前**（首块也延迟），保证 cancel 时缓冲区为空。

### 遗留项状态（对照上一轮报告）

| 上轮发现 | 当前状态 |
|---|---|
| node_probe 提前 cancel（P0） | ✅ 已修复（V6） |
| 测速失败无差别判断流（P0-2） | ⚠️ 部分：取消时机修了，"测速端点全挂 ≠ 节点断流"的区分仍未做 |
| sing-box 日志 disabled 无开关（P1） | ❌ 未动 |
| 启动检测空窗无中间态提示（P1） | ❌ 未动（面板仍只见"未检测"） |
| gofmt 7 个文件（P1） | 🔶 剩 4 个（V4） |
| proxy.go 拆分（P2） | ❌ 未动，且增量又 +228 行（现约 3185 行），优先级上升 |

## 4. ponytail-audit 输出（只列可删减项，one-shot）

```
delete: protocol.HasChoices —— 增量后全仓零调用。删掉即可。[internal/protocol/normalize.go:80]
delete: exitFoldCount —— 自称"面板信号"但无调用方。删掉，或接进 /health。[internal/app/exit_fold.go:84]
yagni:  internal/translate 整包(注册表+双向 gemini/claude) 生产零接线。保留需给出批次⑤接线时间点，否则 1217 行纯属预铺。[internal/translate/]
yagni:  StreamIdleSecs 配置字段 —— 面板/API 都写不了它。补 UI 或降级为常量。[internal/app/zen.go:270]
shrink: sanitizeJSONControlChars 两遍扫描可在 dirty=true 时单遍完成(预分配复用), 当前实现对干净数据已经零开销, 可不动。[internal/app/proxy.go:925]
net: -2 functions, -1217 lines-or-wired possible.
```

（正确性与安全项不在 ponytail 口径内，见 §3。）

## 5. 安全与隐私复核（增量相关面）

1. 新增日志点（坏行门卫、合成打点）只记录 `kit.Truncate(payload, 120)` 截断样本——上游响应片段入日志，理论上可能含模型输出正文；量级小、只进本地 `data/cline-proxy.log`，可接受，但分享日志求援时注意该文件；
2. `NOTICE` 的 MIT 移植声明规范（逐机制列出、注明 Go 重写非逐行复制、保留上游许可指向）——合规面比多数同类项目认真；
3. 密钥面、admin token、订阅脱敏与上一轮结论一致，增量未引入新的暴露路径。

## 6. 部署一致性提醒

- `go.mod` 仍是 `go 1.25.5`，Dockerfile/CI 用 1.26——保持上轮建议：统一；
- **修复是否生效取决于运行中的 exe 是否重新构建**：`git log` 显示修复在 `5bb1686`/`0b87fc8`，如果用户机器上的 cline-proxy.exe 早于这些提交，C2PA 脏包问题仍会透传。交付构建命令以 `AGENTS.md` §3 为准（必需标签 + `-H=windowsgui`）。

## 7. 结论

> 16 个增量提交方向和纪律都对，测试全绿；本轮审计**未改动任何代码**。最值得马上处理的是 F1（一行锁）；F2/F3 是流式中继的一致性收尾；F4 需要作者明确 translate 包"何时接线"，否则它会在下一个重构轮里被当成死代码删掉。ponytail 口径：现在全树还有 2 个函数、约 1217 行处于"写了但没人用"状态。
