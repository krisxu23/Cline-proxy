# Cline-proxy 全面审计报告（2026-09-15）

- **审计对象**：`C:\Users\Administrator\WorkBuddy\2026-09-13-12-31-02\Cline-proxy`
- **审计日期**：2026-09-15
- **代码基线**：分支 `main`，相对 `origin/main` 超前 38 个提交
- **基线头提交**：`5403cbd fix(nodes): 端口分配两阶段化 + 顺序唯一分配`
- **工作树状态**：审计中的只读命令未改动业务文件；报告本身是本次新增的唯一文件
- **审计方式**：本地静态审查 + 关键路径代码追踪 + 必需标签下的构建/测试/格式证据；未安装 CodeRabbit CLI，因此没有向外部评审服务上传代码
- **必需构建标签**：`with_quic,with_grpc,with_utls`
- **报告位置**：`docs/reports/2026-09-15-full-audit.md`

## 0. 执行摘要

网关主流程可以正常启动，HTTP 服务、 sing-box 节点池、订阅恢复、健康检测、选路和日志链路在代码层面是连贯的。本次审计没有发现必须立即停服的崩溃路径。

用户报告的“网关起来后 sing-box 部分检测节点跑不起来”最可能不是单一崩溃，而是三层因素叠加后的表现：

1. **启动顺序天然制造一个“检测空窗”**：启动时先同步重建一次节点池，然后异步恢复订阅缓存；订阅节点大量到达之前，首次调度的检测可能面对很少甚至没有节点。
2. **健康检测自身有一个高可疑的请求上下文用法**：活性、出口 IP、测速三个阶段都在 `client.Do` 返回后立刻取消请求上下文，然后继续读响应体；该行为可能让部分本可成功的检测被记为失败或断流。
3. **可观测性不足放大了“好像没检测”的体感**：sing-box 日志被关闭，面板缺少检测进度和失败原因字段，用户只能看到“未检测”。

其余方面，项目整体测试和构建是绿的，近端提交已经收口了多个真实事故链；主要风险集中在协议转换测试覆盖、配置与文档漂移、日志与凭据暴露面、退出路径的并发细节上。

## 1. 本次审计与既有报告的关系

目标仓库已有五份报告：

- `docs/reports/2026-09-12-singbox-and-freebuff-assessment.md`
- `docs/reports/2026-09-13-iterative-review.md`
- `docs/reports/2026-09-13-second-pass-review.md`
- `docs/reports/2026-09-13-team-review.md`
- `docs/reports/2026-09-14-deepseek-audit.md`

本次没有重复撰写“另一套独立盲审”，而是做了三件此前报告没有完整覆盖的事：

1. 针对新 bug 把 `StartProxy → syncNodeBox → loadSubCache → checkAllNodeHealth → testNodeComprehensive` 的整条链路重新走了一遍；
2. 在本机用必需标签实际跑了 `go vet`、核心单测、全量 `internal/...` 测试和全量构建；
3. 明确区分“已用测试和命令验证的结论”与“仍需线上复现确认的推测”。

## 2. 验证台账

以下每条都可以在目标目录复现，命令均显式进入目标仓库执行。

| 编号 | 检查项 | 命令/方法 | 结果 |
|---|---|---|---|
| E1 | 必需标签下静态检查 | `go vet -tags "with_quic,with_grpc,with_utls" ./internal/...` | 通过，无输出 |
| E2 | 节点健康与构建回归单测 | `go test -count=1 -tags ... ./internal/app/ -run 'TestCheckAllNodeHealth\|TestSyncNodeBox\|TestBuildNodeParts\|TestSSBadMethod\|TestNormalizeSSMethod\|TestSandboxRejects' -v` | 全部通过；空池路径明确打印“没有可检测的出口(sing-box 实例未就绪), 本轮检测跳过” |
| E3 | 全量内部测试 | `go test -count=1 -tags "with_quic,with_grpc,with_utls" ./internal/...` | `internal/app`、`internal/protocol`、`internal/providers` 全部通过 |
| E4 | 全量构建 | `go build -tags "with_quic,with_grpc,with_utls" ./...` | 通过 |
| E5 | 格式化检查 | `gofmt -l internal main.go` | 7 个文件未格式化：`internal/cli 出口 IP 检测：
  - `resp, err := client.Do(req)`
  - `cancel()`
  - 然后检查状态码并用 `io.ReadAll` 读响应体。
- 测速：
  - `resp, err := client.Do(req)`
  - `cancel()`
  - 然后循环调用 `resp.Body.Read`。

在 Go 的 HTTP 客户端语义里，请求上下文取消后继续读响应体是不可靠的。即使活性探测只判断状态码、影响较小，出口 IP 和测速阶段也可能因此读到错误或零字节。

测速的失败后果尤其严重：

```go
// 全部端点都失败 = 断流
return 0, true
```

而健康判定是：

```go
ok := r.Alive && !r.MITMRisk && !r.IsStalled
```

也就是说，即使节点本身是通的，只要测速阶段因为上下文取消或网络波动全部失败，该节点就会被记为断流，从而被记为不健康。

这可以解释“节点看起来就绪，但检测通过率极低或全部失败”。

需要强调的是：本结论来自代码语义和直接搜索证据，不是线上抓包结论。最终确认仍需要一次受控复现，例如用本地可控 HTTP 端点替换检测 URL，或在检测路径加入结构化失败原因日志。

### 3.4 第三层：失败时缺少可观测性

`startNodeInstance` 和 `validateOutboundEntry` 都把 sing-box 日志关闭：

```go
"log": map[string]any{"disabled": true},
```

这在正常运行时减少噪声，但当实例构建或启动失败时，用户只能看到网关侧的一行概括日志，看不到 sing-box 侧的具体出站或入站错误。

同时，`checkAllNodeHealth` 只记录“完成、并发数、可达数”，不记录每个节点在哪一关失败。面板的节点视图有延迟、出口国家、测速、MITM 等字段，但缺少“最近一次失败阶段”和“失败原因”字段。

因此即使检测真的跑了并大量失败，用户也很难从面板判断是活性失败、IP 回显失败、测速失败还是 MITM 误判。

### 3.5 其他可能分支

以下分支目前没有直接证据，但排查时不应遗漏：

1. **构建标签缺失**：交付二进制如果没有 `with_quic,with_grpc,with_utls`，部分 reality、uTLS、QUIC 节点可能被剔除。本次源码构建已带标签验证通过，但用户手里的实际运行二进制需要单独确认构建方式。
2. **订阅解析中间态**：空入口守卫只在已有实例时保留旧实例；首次启动且订阅抓取尚未完成时，实例可能只有 catch-all 而没有节点。
3. **端口或实例启动失败**：`syncNodeBox` 在 `Start` 失败后会清除本次稳定端口并保留旧实例；如果旧实例也不存在，节点池就是空的。
4. **坏节点拖垮全量构建**：已有逐节点校验和手动节点回退路径，但 `validateOutboundEntry` 只覆盖 `box.New` 阶段，不能保证 `Start` 阶段一定成功。
5. **检测目标本身不可达**：活性依赖 Google 相关端点，IP 回显依赖外部 IP 情报服务，测速依赖 Cloudflare；如果运行环境到这些目标的网络本身受限，检测通过率会系统性偏低。

## 4. 代码结构与文件组织评价

### 4.1 做得好的地方

- sing-box 相关职责已经拆分：
  - `nodes.go`：生命周期与同步；
  - `node_build.go`：构建与端口；
  - `node_parse.go`：链接解析；
  - `node_probe.go`：检测引擎；
  - `node_health.go`：健康状态与调度；
  - `node_view.go`：展示；
  - `node_upstream.go`：上游矩阵；
  - `nodes_dns.go`：DNS；
  - `node_ports_traffic.go`：端口与流量。
- 并发注释质量明显高于普通业务项目：锁顺序、构建互斥、失败保留旧实例、重入补偿都有文字说明。
- 关键事故链已经被测试钉住，包括构建失败保留旧实例、健康重入补偿、空池跳过、坏 SS 方法剔除。
- 订阅缓存、国家落盘、端口稳定表都有明确的持久化思路，不是纯内存状态。

### 4.2 结构性问题

1. **`internal/app` 仍然过大**：167 个非数据文件里，绝大多数集中在 `internal/app`。节点、订阅、代理池、健康、地区、DNS、面板、协议转换、统计、日志、托盘都在同一包内。虽然文件名拆得不错，但包边界没有拆，编译依赖和全局状态仍然高度集中。
2. **`proxy.go` 仍然是上帝文件**：HTTP 路由、网关主流程、协议转换、日志、生命周期、CORS、流式处理都在里面。将近 3000 行的规模已经不适合继续追加功能。
3. **管理面板 HTML 以 Go 字符串形式内嵌**：`admin_html*.go` 的维护成本很高，前端改动难以做常规的 JS/CSS 检查和格式化。
4. **全局状态偏多**：节点池、健康、订阅、统计、传输客户端、冷却、地区缓存等大量使用包级变量和互斥锁；测试需要频繁保存和恢复全局状态，长期会增加回归成本。
5. **测试替身与真实路径分离**：健康重入、回滚、空池等并发行为有替身测试，但真实 sing-box 实例加真实网络的端到端检测没有受控覆盖。这正是新 bug 难以快速定位的原因之一。

## 5. Bug 清单

### P0：建议尽快修复

1. **检测请求在读响应体前取消上下文**
   - 位置：`internal/app/node_probe.go`
   - 影响：活性、出口 IP、测速三阶段；
   - 建议：把 `cancel()` 改为在响应体完全消费并关闭后再调用，或对每个阶段使用 `defer cancel()` 并确保响应体读取不受提前取消影响；
   - 建议同时为每个检测阶段记录结构化失败原因。

2. **测速全部失败直接记为断流，区分度不足**
   - 位置：`internal/app/node_probe.go` 的 `probeNodeSpeed`
   - 影响：测速服务端波动会被记成节点断流；
   - 建议：区分“节点不可达”“代理链路失败”“测速服务端失败”“真实低速/空闲断流”，至少在日志和面板给出不同原因。

### P1：重要但不立即停服

3. **启动期检测空窗没有明确提示**
   - 位置：`internal/app/proxy.go`、`internal/app/sub.go`、`internal/app/node_health.go`
   - 建议：在健康接口和面板增加“订阅恢复中”“节点构建中”“检测排队中”等中间状态，而不是只有“未检测”。

4. **sing-box 日志完全关闭导致排障困难**
   - 位置：`internal/app/node_build.go`
   - 建议：增加可选的详细日志开关，默认保持安静，排障时可打开；不要长期全关。

5. **空池跳过只有一行日志**
   - 位置：`internal/app/node_health.go`
   - 建议：把“实例是否存在、节点数、订阅数、最近一次构建错误”一起暴露到 `/health` 或节点接口。

6. **部分 `gofmt` 差异未清理**
   - 影响：改动可读性和 reviewer 信任；
   - 建议：跑一次 `gofmt -w` 并确认 diff。

### P2：优化建议

7. **将 `proxy.go` 按路由、协议转换、生命周期拆分**；
8. **将管理面板前端从 Go 字符串迁移为可独立检查的前端资产，或至少加入渲染冒烟测试之外的 JS 语法检查**；
9. **为检测结果增加失败阶段字段**，例如 `liveness / ipEcho / speed / mitm / warp`；
10. **为大规模订阅增加构建进度日志**，避免数分钟的静默构建被误认为卡死；
11. **继续收敛包级全局状态**，优先把节点池、健康、订阅封装成可注入的结构体。

## 6. 安全与隐私审查

本次没有发现新的高危远程利用链，但以下事项需要保持警惕：

1. **管理接口已有令牌保护**，这是正确且重要的防线；不要为了方便而放宽。
2. **订阅 URL、节点链接、账号 refreshToken 都属于敏感数据**：
   - 日志里已有 `maskURLForLog` 等脱敏处理；
   - 但 `data/` 目录本身经常被备份或同步，需要在文档里明确提醒用户不要随意分享该目录；
   - 报告和日志中不得粘贴真实订阅 URL、节点 UUID、refreshToken 或 admin token。
3. **配置导入导出包含敏感文件**，导入路径已有 PAR 校验思路，但仍建议定期复核导入文件的 schema 白名单。
4. **Docker 镜像已使用非 root 用户**，做法正确；宿主机 bind 挂载 `data` 时需要注意文件所有权。
5. **健康接口是否需要令牌**：当前 `/health` 可直接访问，返回的是状态和计数，没有敏感字段；保持现状即可，但不要往该接口追加敏感信息。

## 7. 部署与文档问题

1. **Dockerfile 使用 Go 1.26 构建镜像，而 `go.mod` 声明为 Go 1.25.5**：短期内一般可用，但建议统一版本，避免工具链行为差异。
2. **既有报告已经指出多处 README 漂移**，例如订阅刷新间隔、模型配置键名等；本次审计不再重复展开，但建议把文档校验纳入发布前清单。
3. **Windows 交付构建参数有明确约定**，包括必需标签、`CGO_ENABLED=0`、`-s -w -H=windowsgui`；排查新 bug 时首先要确认问题二进制是否按该约定构建。

## 8. 给用户的直接排障建议

如果用户现在就要定位手头那台机器的问题，建议按以下顺序收集信息，不要直接反复点“连通检测”：

1. 打开管理面板的节点接口，记录节点总数、`Running` 状态和健康状态分布；
2. 查看 `data/cline-proxy.log` 里最近的 `nodes:` 日志，重点找：
   - “订阅缓存: X 个节点已恢复”；
   - “X 个高级节点出口已就绪”；
   - “没有可检测的出口”；
   - “增强检测完成”；
   - “解析失败已跳过”“出站无效已剔除”“启动失败”。
3. 访问 `/health`，记录 `nodePool`、`subNodes`、`exitProbed`、`exitReachable`；
4. 如果 `nodePool` 为 0，问题在构建或启动阶段，不在检测引擎；
5. 如果 `nodePool` 有值但 `exitProbed` 一直为 0，问题在检测调度；
6. 如果 `exitProbed` 有值但 `exitReachable` 为 0，问题在检测执行或检测目标网络；
7. 保留脱敏后的日志片段再做下一步改动；不要在报告或聊天记录里粘贴真实订阅链接和 token。

## 9. 本次审计的边界与未做事项

1. 没有修改业务代码；
2. 没有触碰真实账号、真实订阅内容和真实上游额度；
3. 没有做长时间真实网络复现；
4. 没有引入新依赖；
5. 没有向外部代码评审服务上传代码；
6. `node_probe.go` 的上下文问题是强推测，不是线上抓包定论；修复前建议先加失败原因日志并做受控验证。

## 10. 补充核查：CI、安全、结构、依赖

本节是报告初稿落盘后的补充核查，用于覆盖用户要求的“代码结构、文件格式、项目组织”。

### 10.1 CI 配置

`.github/workflows/build.yml` 的测试任务已经覆盖：

- 必需标签下的 `go vet`；
- 必需标签下的普通 `go test`；
- `CLINE_PROXY_SKIP_NODEBOX=1` 下的 `-race` 测试；
- `scripts/admin-render-test.js` 的前端渲染冒烟测试。

做法是合理的：普通测试保留真实 sing-box 路径，竞态测试跳过第三方 sing-box 后台 goroutine 引入的外部数据竞争。后续建议只补两项：

1. 把 `gofmt --check` 加入 CI，本次发现的 7 个未格式化文件就不该进入主分支；
2. 为文档关键断言加一个轻量检查，例如订阅刷新默认值、构建标签、Windows 交付参数是否与 README 一致。

### 10.2 出站 URL 安全校验

`internal/app/outbound_url.go` 已对服务端主动请求的地址做校验，覆盖：

- 非 http/https 直接拒绝；
- 链路本地 `169.254.0.0/16`、`fe80::/10`；
- `0.0.0.0`、`::`；
- 本机回环地址需要结合上下文判断。

订阅和上游端点更新路径也走了 `filterOutboundURLs`。这部分比很多同类网关完整，结论是保持即可，不建议再放宽。

### 10.3 文件规模与结构风险

按行数排序，风险最高的文件是：

- `internal/app/proxy.go`：2957 行；
- `internal/app/admin_html_part4.go`：1939 行；
- `internal/app/admin.go`：1691 行；
- `internal/app/zen.go`：1041 行；
- `internal/app/sub.go`：700 行；
- `internal/app/node_parse.go`：616 行。

`proxy.go` 的规模已经进入“每次改动都要全量回归”的区间。建议按路由注册、协议转换、生命周期、流式处理拆成至少 4 个文件。管理面板 HTML 建议从 Go 字符串迁出或至少引入独立的 JS 语法检查。

### 10.4 日志与凭据

订阅抓取和订阅恢复路径已使用 `maskURLForLog`，做法正确。但节点键本身仍是“去掉名称后的原始链接”，包含凭据字段；日志里应继续坚持只打脱敏展示名或脱敏 URL，不打原始链接。

### 10.5 依赖面

`go.mod` 声明：

- `go 1.25.5`；
- `github.com/sagernet/sing-box v1.14.0`；
- `github.com/sagernet/sing v0.9.0-beta.4`；
- `github.com/refraction-networking/utls v1.8.2`；
- `golang.org/x/net v0.57.0`；
- `gopkg.in/yaml.v3 v3.0.1`。

`Dockerfile` 和 CI 使用 Go 1.26 构建镜像，存在小版本漂移。短期一般可用，但建议把 `go.mod`、Dockerfile、CI 的 Go 版本统一，避免将来出现难以复现的构建差异。

另外，早前一次附带的 `go list -m all` 因为本机 Go 代理不可达而失败；这不是否定依赖，而是说明该环境不适合做全量模块元数据拉取。依赖版本结论以仓库内 `go.mod`、`go.sum` 为准。

### 10.6 本轮补充验证结果

1. **前端渲染冒烟测试通过**：`node scripts/admin-render-test.js` 返回 `ALL PASS`，退出码 0。管理面板嵌入 HTML/JS 的基础渲染断言仍有效。
2. **工具链漂移确认**：`go.mod` 为 `go 1.25.5`，`Dockerfile` 与 `.github/workflows/build.yml` 均为 Go 1.26。建议统一，避免将来出现难以复现的构建差异。
3. **敏感数据忽略规则正确**：`.gitignore` 第 20 行忽略 `data/`，`git check-ignore` 确认 `data/admin-token` 与 `data/subs_cache.json` 均被忽略，不会随报告或补丁误提交。
4. **检测链路可疑行号已精确到行**：`internal/app/node_probe.go` 第 147-148、170-171、303-304 行均为 `client.Do` 后立刻 `cancel()`，然后继续读响应体；MITM 与 WARP 阶段第 352-353、382-383 行使用 `defer cancel()`，写法正常。修复时优先改前三处。

### 10.7 并行改动的复核（该增量现已被提交进 main）

审计收尾时工作区出现与节点选路直接相关的并行改动（非本次审计产生，现已提交），逐条复核如下：

- 新增 `internal/app/exit_fold.go`（88 行）：按实测出口 IP 对健康节点做去重，同出口只保留延迟最低者，其余标记为折叠副本；
- `node_health.go` 在每轮检测完成后调用 `recomputeExitFold()`；
- `proxy_pool.go` 的 `nodeUsable` 对折叠副本返回 false；
- `node_view.go` 增加 `duplicateOf` 展示字段。

`go vet` 在含该增量的工作区下仍通过。设计方向合理，但有三点风险需要后续处理：

1. ~~`TestExitFoldKeepsFastestPerExitIP` 复跑失败（键口径不一致）~~（2026-09-15 报告落盘后复核：该增量已被提交，测试键已统一为完整链接，`TestExitFoldKeepsFastestPerExitIP` 与 `TestExitFoldReElectionOnDegrade` 均通过，此项关闭）；
2. `withHealthResult` 直接读 `exitFoldDupOf` 的读取路径需要复核是否全部持锁；
3. 该增量会改变“可用节点数”的口径，面板和 `/health` 需要同步解释，否则用户可能把折叠误认为检测失败。

## 11. 结论

可以这样向用户总结：

> 网关本身能跑，构建和测试也是绿的；新 bug 更像“检测条件没凑齐 + 检测自身写法可疑 + 失败看不见”三件事叠在一起。最优先处理 `node_probe.go` 的上下文取消和测速失败判定，同时补上检测进度和失败原因的可观测性；之后再做结构拆分和文档收敛。
