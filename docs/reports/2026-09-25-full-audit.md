# Cline-proxy / Free-Router 全面审计报告（2026-09-25）

- **审计对象**：`D:\oh-my-pi\OMP Desktop\Cline-proxy`（公开仓库 `krisxu23/Free-Router`）
- **审计日期**：2026-09-25
- **代码基线**：`main` @ `abbb06b`（与 origin/main 同步）；工作树不干净：`internal/app/models.go` 有未提交改动（+15 行，apiModelList 硬编码追加一条 `cline/cline-free/gemini-3.8-flash` 别名条目）+ 1 个未跟踪 plan 文档
- **自上轮审计（R3，基线 `aed184e`）以来的增量**：68 个提交，255 文件，+48461/-10233（含"77 缺陷修复"批次、流式空流分岔/懒提交重构、zen 出口级连接隔离、代码重组 main.go→cmd/free-router、admin HTML→internal/webui）
- **方式**：纯只读，零代码改动。5 路并行子系统深审（安全构建/流式核心/admin 面板/协议转换/zen·节点）+ 本机全量基线验证；每条 P1/P2 均经审计者**亲自读码复核**，3 条代理疑点被证伪剔除
- **必需构建标签**：`with_quic,with_grpc,with_utls`
- **报告位置**：`docs/reports/2026-09-25-full-audit.md`

## 0. 执行摘要

一句话：**工程质量高于平均、历史问题闭环良好，但公开仓库里躺着两处疑似真实凭据（P1），外加一条确定性的流式探测协程泄漏和一条 zen 多 key 配对失效（P2）**。

- 没有发现 P0（崩溃/数据损坏/远程利用链）。
- 基线全绿：必需标签下 build / vet / gofmt / 全量测试 / CI 口径 `-race`（0 竞态）/ 面板渲染冒烟全部通过。
- 历史 R2/R3 审计的遗留项绝大多数已闭环（gofmt 进 CI、proxy.go 拆分、admin HTML 迁出、probe 上下文取消修复、translate_registry 接线）；仅 StreamIdleSecs 面板入口、sing-box 日志开关两项仍挂账。
- 本轮真正要动手的是四件事：**清凭据（P1-1/P2-6）、给 release job 加事件门禁（P1-2）、修空流探测泄漏（P2-2）、修 zen key/出口顺序（P2-4）**。
- 覆盖声明：安全/构建/依赖为全覆盖精审；zen/节点为 7 个核心文件全读；流式核心约 10 文件精读；admin/协议为广度审计。`anthropic.go`、`proxy_stream.go`、`proxy_pool.go`、`claude_tool_remap.go`、`providers_chat.go` 等大文件未逐行覆盖，见 §7。

## 1. 验证台账

| # | 检查项 | 命令 | 结果 |
|---|---|---|---|
| V1 | 构建 | `go build -tags "with_quic,with_grpc,with_utls" ./...` | ✅ 通过 |
| V2 | 静态检查 | `go vet -tags ... ./...` | ✅ 通过 |
| V3 | 格式化 | `gofmt -l internal cmd` | ✅ 零差异（历史 4 文件已修） |
| V4 | 全量测试 | `go test -count=1 -tags ... ./internal/...` | ✅ 全绿（app 36.5s） |
| V5 | 竞态（CI 口径） | `FREE_ROUTER_SKIP_NODEBOX=1 go test -race -tags ... ./internal/...` | ✅ 全绿，0 竞态 |
| V6 | 竞态（真实 sing-box 模式） | 同上但不设 SKIP 变量 | ❌ 17 处 DATA RACE——**全部位于第三方 sing-box v1.14.0 内部**（`route.NetworkManager` Start 与 notifyInterfaceUpdate），本项目代码零命中；这正是 SKIP 开关存在的原因，非本项目缺陷 |
| V7 | 渲染冒烟 | `node scripts/admin-render-test.js` | ✅ ALL PASS |
| V8 | 覆盖率 | `go test -cover` | app 64.9% / translate_registry 94.3% / cline 71.2% / translate 65.2% / kit 59.7% / providers 41.6% / **protocol 21.8%** / webui 无 |
| V9 | 仓库公开性 | GitHub API `repos/krisxu23/Free-Router` | `private: false`（P1-1/P2-6 的前提） |
| V10 | 依赖完整性 | `go mod verify` | all modules verified |

## 2. P1（建议立即处理）

### P1-1 公开仓库硬编码真实第三方订阅链接（疑似凭据泄露）

- 位置：`scripts/admin-render-test.js:573`
- 证据：测试夹具写死 `https://misub.bursaonline.eu/920731/rq?clash`，且夹具状态串 `🟢 09-14 19:23 · 98 节点` 明显是从真实面板复制的抓取状态。该文件随 main 分发到公开仓库（已通过 API 拉取验证）。测试本身禁网（`global.fetch` 抛错），不会外呼。
- 影响：订阅链接即凭据，任何访客可拿它消耗第三方订阅额度；违反本项目自身"仓库/日志/报告不得出现真实订阅 URL"的纪律。
- 建议：立即更换该订阅（或确认其为公开测试源）；夹具改 `example.invalid` 假域名。已在 git 历史中，仅删当前行不够。
- 置信度：暴露已验证；"链接仍有效"高度疑似（需换源复测）。

### P1-2 CI release job 无事件门禁：pull_request 也会创建官方 Release

- 位置：`.github/workflows/build.yml:3-8`（`on: pull_request`）+ `build.yml:170-231`（release job）
- 证据：release job 仅 `needs: build`，**全 workflow 0 处 `if:` 条件**（已 grep 验证），job 级 `permissions: contents: write`；所有事件共用 `concurrency: release-main`。
- 影响：① 同仓任意 PR 触发即用 PR 内容创建 tag+Release 并上传产物——未评审代码成为"官方发布物"，语义版本号被 PR 消耗/竞争；② fork PR 的 GITHUB_TOKEN 恒只读 → 外部贡献者 CI 永远红；③ PR 与 main 推送互相排队阻塞。
- 建议：release job（及产物上传）加 `if: github.event_name == 'push' && github.ref == 'refs/heads/main'`；concurrency 按事件分组。

## 3. P2（重要，建议排期修复）

### P2-1 裸跑 `go test ./...`（无标签）直接红灯，失败用例不是 SKIP

- 位置：`internal/app/nodes_test.go:268-292`（`TestAllOutboundTypesRegistered`）
- 证据：该用例的守卫 `requireNodeBox(t)` 只识别 `FREE_ROUTER_SKIP_NODEBOX`，不识别构建标签缺失。干净环境不带标签裸跑 → `uTLS is not included in this build` FAIL；带标签全绿。
- 影响：外部协作者/新环境第一次 `go test ./...` 即红，极易误判"项目坏了"。（CI 两口径——带标签普通跑、SKIP+race——均绿，故从代理初报的 P1 降级；AGENTS.md/README 也只记载带标签跑法。）
- 建议：该用例在 `!with_utls` 时 `t.Skip`，或整体加 `//go:build with_utls`。

### P2-2 空流探测失败路径泄漏 idleAbortReader 泵协程（16KiB/次，永不回收）

- 位置：`internal/app/stream_early_eof.go:100`、`:113`（两条 `return true, nil, ...`）；配 `stream_idle.go`（泵协程结构）、`routing_dispatch.go:253-255`（调用方）
- 证据：成功路径返回 `prefixedBody{closer: idle}`，Close 会关闭 idle → doneCh 关闭 → 泵退出。但 EOF / 总时限 / `buf > 1MiB` 三条失败路径直接丢弃 `idle`、从不 Close；调用方随后关的是**原始** `resp.Body`。泵协程的三个退出点全部依赖 `doneCh` 或 `resCh` 有接收者：总时限/超限路径中泵正卡在 `ackCh` 等待或下一次 `resCh <-` 发送上，无接收者且 doneCh 永不关闭 → 永久阻塞。
- 影响：上游持续回"200 + 心跳但无真实事件"（正是该守卫要防的场景）时，**每次探测泄漏 1 个 goroutine + 16KiB 缓冲 + 1 个 timer**，长跑进程只增不减。
- 建议：两条失败路径在返回前 `idle.Close()`（一行修法），或让泵在 body-EOF 时自行退出。
- 置信度：已验证（审计者亲自读全两文件 + 调用方）。

### P2-3 "提交后内容观测"机制在生产代码中是死代码

- 位置：`internal/app/stream_readiness.go:471-478、556-671`
- 证据：`hasStreamReadinessSignal`、`newStreamContentWatcher`/`SawContent`/`SawLegitEmptyTerminal`/`SawSseFrame`/`SawError`、`hasUsefulStreamContent` 全仓 grep（排除定义文件与测试）**零生产调用点**；注释却声称是"客户端侧内容观测"的照抄实现。
- 影响：文档与实现不符——响应头提交后实际没有任何 watcher 在观察客户端流；后续维护者会以为已有兜底。
- 建议：要么在 `proxy_stream.go` 客户端写出路径接上 watcher，要么删除并改注释。

### P2-4 zen 多 key 的"同一出口永远同一把 key"被"先选 key、后选出口"顺序打破

- 位置：`internal/app/zen_call.go:316`（key 选择）vs `:353-359`（出口选择与写回）
- 证据：`reqKey := zenSelectKeyForModel(cfg, reqExitKey(ctx), ...)` 在循环内先执行；首试时 `reqExitKey(ctx)` 必为空（`setReqExit` 在 L359 才写），于是所有请求首试恒落 `keys[0]`；重试时 key 按上一轮出口哈希选，而本轮出口已因冷却换新。与 `zen_keys.go` 的设计声明（按出口确定性配对）在任意一次尝试上都不成立。
- 影响：多 key 场景 key↔出口映射紊乱（隐蔽性目标落空）；单 key / 匿名 "public" 模式无影响。401 退役逻辑仍正确。
- 建议：把 `zenSelectKeyForModel` 移到 `pickUnifiedExit`/`setReqExit` 之后，用本次真实出口选 key。

### P2-5 zen 负向 memo：文本匹配绕过状态码限制 + 进程级永久

- 位置：`internal/app/zen_responses_quirks.go:143-149`（判定）、`:197-199`（落 memo）、`zen_responses.go:146-156`（memo 存储）
- 证据：注释声明"401/429/408/5xx 不记负向"，但 `zenEndpointUnsupported` 对**任意状态码**只要 body 含 `not found`/`unsupported` 就返回 true——一个 5xx 网关错误页（常见 "404 Not Found" 字样的 HTML）就会误记。`zenChatOnlyMemo` 是进程内 map，无 TTL、无配置重载清理（全仓仅定义+一处写入）。
- 影响：一次瞬时上游故障可能让某模型整个进程生命周期不再走 /responses 回退，静默能力丢失；重启前无法自愈。
- 建议：文本匹配仅限 404 状态码时生效；给 memo 加 TTL 或配置重载时清空。

### P2-6 公开仓库硬编码形似真实 API key 的字面量

- 位置：`internal/app/zen_keys_test.go:119`
- 证据：`sk-` 开头 67 字符字面量（掩码测试夹具），已在公开仓库 main。全仓 `sk-` 类字面量仅此一处。
- 影响：若为真实 zen key 则已泄露；即便失效也会被安全扫描器持续报警。
- 建议：改用确定性假值（`sk-` + 重复 `a`）；并在 provider 侧核对该 key 是否活跃、必要时轮换。

### P2-7 getAccountByID / pickAccount 把共享 `*Account` 裸指针带出锁外

- 位置：`internal/app/pool.go:319-330`（getAccountByID）、`:404-448`（pickAccount）
- 证据：`poolSnapshot()` 的文档明写此前"锁外读 + 持锁写"是真竞态并为此引入快照；但这两个函数仍返回原始指针。字段写方（`refreshAccountToken`/`bumpUsage`/冷却自动解除）都在 `poolMu` 内，而调用点存在锁外读：`admin_accounts.go:605`（`truncateEmail(acc.Email)`）、`admin_accounts.go:530→testAccount(acc)` 等。
- 影响：与 poolSnapshot 想根治的竞态同源；`-race` 下调用点锁外读字段即报（当前 race 全绿只是因为这些路径未在 race 用例中并发触发）。
- 建议：与 `poolSnapshotAccount` 同构返回深拷贝；至少注释"返回指针仅可立即使用，不得缓存"。

### P2-8 缺 `.dockerignore`：`data/` 凭据与 `.git` 进入 Docker 构建上下文

- 位置：`Dockerfile:15`（`COPY . .`）；根目录无 `.dockerignore`（已验证不存在）
- 影响：`docker compose build`（README 部署路径）把 `data/`（admin-token、.cline-accounts.json、.zen-config.json）与 `.git` 整个发给 daemon，进入 builder 层与构建缓存；导出/推送/快照/远程 builder 即泄露。
- 建议：补 `.dockerignore`：`data/`、`.git`、`docs/`、`*.log`、`.learnings/`。

### P2-9 SSRF 残余面：HTTP 重定向与"配置期后 rebinding 到私网"在拨号期不拦

- 位置：`internal/app/outbound_url.go:178-207`（拨号期守卫刻意只拦链路本地/云元数据，注释自认取舍）+ `internal/app/proxy_pool.go:28`（zenHTTPClient 无 `CheckRedirect`，默认跟随 10 跳）
- 影响：控制了"用户已配置的订阅/上游域名"的攻击者可用 302→内网 或 DNS rebinding 探测内网 HTTP 服务并回读（链路本地已封死，云 metadata 安全）。
- 建议：给订阅抓取/Provider/zen 客户端单独设 `CheckRedirect`（拒绝跳私网/回环），与节点拨号路径解耦；节点拨号保持现状。
- 置信度：设计注释证实取舍；重定向路径未逐跳实测。

### P2-10 MIT 许可合规缺口：无 LICENSE 文件 + NOTICE 失真

- 位置：仓库根无 `LICENSE`/`COPYING`（已验证）；`NOTICE:20` 引用工作区 `omniroute-translator-ref/`——该目录不在仓库也不在工作区；`NOTICE:24-27` 的 MIT 许可只有链接无全文。
- 影响：公开分发含 MIT 移植代码的衍生作品，MIT 要求"随副本附上许可文本"，链接不满足；无 LICENSE = 默认保留所有权利。
- 建议：仓库根加 LICENSE（MIT 全文 + 版权行）；修正 NOTICE 死引用。

### P2-11 go.mod 无 `toolchain` 指令，实际两套 Go 工具链

- 位置：`go.mod`（`go 1.26.0`，无 toolchain 行）、`Dockerfile`（1.26）、CI（1.26）、本机交付（1.27.0）
- 影响：Windows 交付 exe 与 CI/镜像由不同编译器+stdlib 构建；无 toolchain 行不可复现。
- 建议：按发布口径钉死（`toolchain go1.27.0` 或全链升 1.27）。

## 4. P3（卫生/加固，择机处理）

### 并发与资源

1. **kitTruncateHead 按字节截断切 UTF-8**（`error_rules.go:188-194`）：`s[:200]` 落在多字节字符中间 → 日志/面板冷却原因乱码（U+FFFD）。建议回退到 rune 边界。
2. **冷却升级整数除法下溢**（`cooldown.go:137-141`）：override > 24h 时 `24h/d` 整除得 0，`d *= 0` → 冷却立即过期，与封顶意图相反。建议 `d = min(d*escalate, 24h)`。
3. **半开探测条目 janitor 永不回收**（`cooldown.go:239-253`）：`!c.probing` 条件使 probing 项永删；候选不再被尝试（模型下架/订阅移除）时条目永驻。有注释声明取舍，建议给 probing 项加绝对 TTL 兜底。
4. **checkAllNodeHealth 用 defer 递归补跑**（`node_health.go:184-193`）：每次重触发嵌套一层栈帧（持有 keys/groups/sem 大对象）；订阅刷新快于一轮检测时深度无界。建议改 `for again { ... }`。
5. **429 短等待持并发槽睡眠**（`zen_call.go:565-582`）：wait≤60s 的睡眠不释放信号量（默认 8 槽），8 个并发 429 可把 zen 并发窗口睡满数分钟。

### 安全面

6. **start 菜单把含完整令牌的面板 URL 打到 stdout**（`cmd/free-router/main.go:269-270`）：与同文件 :132 "日志不能带 adminURL" 的自身约定矛盾；重定向到文件即令牌落盘。改打掩码版或仅打 `http://127.0.0.1:port/admin/`。
7. **网关 API key 用 `==` 明文比较 + 401 无限速**（`proxy.go:190-191`；对照 admin 令牌已用 `subtle.ConstantTimeCompare`）：高熵 key 实际可利用性低，但口径应统一。
8. **admin 无失败限速 + 最敏感操作零审计**：`handleAccountsExport`（批量导出 refreshToken!）、`handleAdminUpdateConfig`、`handleZenConfigUpdate`、`handleAdminGenerateKey` 等写操作均无审计日志（盘点见代理核验）；令牌本身 192-bit + 恒定时间比较，brute-force 不可行，限速为纵深防御。
9. **管理面板响应缺安全头**：全仓无 `X-Content-Type-Options`/`CSP`/`X-Frame-Options`；现有 Cookie+转义已挡已知注入，属纵深防御缺口。

### 逻辑与协议

10. **StreamIdleSecs / StreamHeartbeatSecs 无面板 UI 入口**（R3 报告 N1 遗留，范围扩大到两个流控参数）：读写口与迁移回填齐全，但 webui 无输入框、`saveOcConfig` 不提交，只能手改 JSON。
11. **flattenToolHistory 疑似死代码**（`flatten_tool_history.go`，277 行零生产调用）：CLIENT-COMPAT 若承诺了"工具历史扁平化"保障，则该保障未接线；否则删除。
12. **`protocol.SanitizeContent` 名为清洗实为 no-op**（`internal/protocol/streaming.go:136`）：注释声明是预留钩子（诚实），但命名易造成"已清洗"误读。建议改名 passthrough 或落实清洗。
13. **clinepass 仅透传 5 个 Extra 字段**（`internal/providers/clinepass.go:378-397`）：`temperature/top_p/tools/tool_choice/stop` 之外的请求级字段（`response_format` 等）静默丢弃，走 clinepass 路由的客户端表现随路由漂移。建议扩充 allowlist 并在 CLIENT-COMPAT 登记。

### 文档/构建/CI（漂移清单）

14. **README 横幅描述与实现不符**：README:82-90 称打印"令牌本身"，实际 `proxy.go:531` 打掩码；`proxy.go:522` 无条件提示免 key，即使已配置。
15. **README 日志轮转过时**：README:297/340 称四类文件按大小轮转（requests.jsonl），实际请求日志已改**按天分段 + 保留 7 天**（`log_rotate.go:3-9`，产物 `requests-YYYYMMDD.jsonl`）；`webui/page_settings_logs.go:9` 同样漂移。
16. **README 把已废弃的 freeModels 白名单当主用法**（README:231-232/255 vs `providers_config.go:43-47`"一次性迁移输入"）：迁移完成后用户再往白名单加模型**静默不生效**。
17. **CLIENT-COMPAT 锁定测试名错误**：引用的 `TestHeartbeatReaderInjectsOnSilence` 等不存在，实际为 `TestSSEHeartbeatInjectsOnIdle`/`TestSSEHeartbeatNeverCorruptsRealFrames`。
18. **README 环境变量表不全**：缺 `FREE_ROUTER_MITM_URL/TRACE_URL/LIVENESS_URLS/IP_ECHO_URLS/SPEEDTEST_URLS/PROBE_DEEP/CLAUDE_DISABLE_TOOL_NAME_CLOAK` 等。
19. **docker-compose 三处**：:23 注释称 HEALTHCHECK 写死 3457（实际 Dockerfile:39 已动态取）；:1 `version:` 已废弃；:20 挂 `./override.md` 但文件默认不存在——Docker 会把缺失源**建成 root 目录**，容器内 override 静默不生效。
20. **CI 供应链细节**：actions 全部按可变 tag 钉（第三方 `softprops/action-gh-release@v2` 建议钉 SHA）；全 workflow 无 `timeout-minutes`；checkout 未设 `persist-credentials: false`；Dockerfile 基础镜像/apk 未钉版本、`go mod download` 阶段未先 COPY go.sum。
21. **sing-box 日志仍全关**（`node_build.go:202/248` `"disabled": true`）——9-15 审计 P1-4 原样遗留，排障时看不到 sing-box 侧错误。
22. **心跳默认值依赖一次性迁移链**（`stream_keepalive.go:176-182` 只认显式 >0；15 秒由 v1→v2 迁移回填，`defaultZenConfig()` 不含该字段）：当前行为正确（新装 SchemaVersion=0 → 迁移回填并落盘，已验证），但未来新增 v3 迁移时容易漏；建议 `defaultZenConfig()` 显式置 15。

### 低置信提示（未定谳，复核后再定级）

- `zen_call.go:281/377/600` `baseURLs[attempt%len(baseURLs)]` 无空切片防护（取决于 `zenBaseURLList` 是否可能返回空）；
- `zen_call.go:158` `trace := traceFrom(ctx)` 可能为 nil 直接 `AddAttempt`（取决于 traceFrom 语义）;
- `providers_catalog.go:807-836` catalogStatus 持 p.mu 期间调用配置层函数，锁序与其余代码相反（潜在倒置死锁，需读配置层确认）;
- `zen_state.go:35-44` rebuildZenSem 换新通道窗口内瞬时总并发可超上限（自愈，轻）;
- `zen_state.go:110-114` clearZenProbing 全局无差别清除，可误清他人探测标志（自愈，轻）;
- `providers_catalog.go:862` startProviderRefresher 启动 30s sleep 不响应退出信号（拖慢关停）。

## 5. 证伪与撤销（防止后续重复怀疑）

| 疑点 | 结论 | 证据 |
|---|---|---|
| `p.rejected` 锁外读 map → 条件性 P1 | **安全** | 写侧是 copy-on-write：`providers_chat.go:739-749` 新建 map 整体替换指针，且有注释声明该契约；读者持锁拷 map 头后读旧 map 安全 |
| `classFingerprint` 不在 defaultCooldownMs → 指纹拒绝零冷却 | **不成立** | `error_rules.go` init() 已注册 `defaultCooldownMs[classFingerprint] = 5*60*1000` |
| `fetchFirstCatalogPage` 二次调用 `providerExitClient()` 可能换出口 | **不成立** | `providerExitClient()` = `getZenHTTPClient()`（共享单例），两次调用同值 |
| 64MiB JSON 缓冲无上限 | **不成立** | 调用点有约束：`proxy_stream.go:29-30`（单行上限）与 `:530`（`io.LimitReader`） |
| "clone-status / 未鉴权 zen/stream 端点" | **不存在** | 全仓 grep 零命中；非 adminAuth 路由全量枚举仅剩令牌门控外壳/302 跳转//health/网关四端点 |
| 心跳默认值缺失导致新装静默关闭 | **不成立（行为正确）** | `loadZenConfig` 每次加载跑迁移链，新装 v0→v2 回填 15 并落盘；仅保留 §4-22 的卫生建议 |
| 真实 sing-box 模式下 17 处 DATA RACE | **第三方内部竞态** | 全部帧落在 `github.com/sagernet/sing-box@v1.14.0/route/network.go`，本项目代码零命中；SKIP 开关即为规避此而设 |

## 6. 历史遗留项闭环核对（R2/R3 → 本轮）

| 遗留项 | 现状 |
|---|---|
| gofmt 4 文件未格式化（R3-V4） | ✅ 闭环：零差异，且 CI 已加 gofmt 门禁 |
| proxy.go 上帝文件 ~3200 行（R2/R3 P2） | ✅ 已拆分：proxy.go 678 行 + proxy_pool/proxy_stream/proxy_http/proxy_lifecycle/proxy_util |
| admin HTML 内嵌 Go 字符串（9-15 P2-8） | ✅ 已迁出至 internal/webui/（page_*+script_* 独立文件 + 渲染冒烟测试） |
| node_probe.go Do 后立即 cancel 的三处（9-15 P0-1） | ✅ 已修：统一"读尽再 cancel"（node_probe.go:191-196 有明文注释） |
| translate 批次⑤"预铺未接线"（R3-F4） | ✅ 已接线：`TranslateRequest` 在 providers_api_format.go:76、zen_call.go:183/:628 实际调用 |
| heartbeatReader 残留引用（R3-N2） | ✅ 闭环：现存引用均为解释旧事故的说明性注释 |
| StreamIdleSecs 面板 UI 入口（R3-N1） | ❌ 仍开放：且范围扩大到 StreamHeartbeatSecs（本轮 P3-10） |
| sing-box 日志开关（9-15 P1-4） | ❌ 仍开放：node_build.go 两处仍 `"disabled": true`（本轮 P3-21） |
| 文档关键断言进 CI（9-15 §10.1②） | ❌ 未落实：workflow 无文档校验步骤 |
| Go 版本三方统一（9-15 §10.5） | ✅ 三方均 1.26；但引出新项 P2-11（无 toolchain + 本机 1.27） |

## 7. 已查无问题清单（正向结论）

- **管理面板安全四件套扎实**：192-bit `crypto/rand` 令牌 + 0600 落盘 + `subtle.ConstantTimeCompare` + Cookie `HttpOnly+SameSite=Strict`；API 层不认 `?token=`（一次性 exchange 后 302）；Origin 预检排在令牌校验前；65 处 admin 路由全部包 `adminAuth`（逐条核对）；横幅/日志令牌均已掩码。
- **TLS**：全仓 0 处 `InsecureSkipVerify`；节点解析里的 `insecure` 仅透传用户自己订阅链接的语义，属代理客户端常规。
- **无调试后门**：0 处 TODO/FIXME/HACK/XXX、0 处 pprof/expvar/debug 端点、0 处默认密码。
- **敏感数据不进 git**：`data/` 8 个文件逐个 `git check-ignore` 验证，`git ls-files data/` 为空。
- **请求日志只存元数据**（无 header/prompt），`maskURLForLog`×7、`maskZenKey` 有测试锁定。
- **Docker**：非 root（uid 10001）、compose 仅绑 127.0.0.1、`/health` 只回计数无凭据。
- **状态表锁覆盖**（zen/节点 7 文件全读）：candidateCools/keyHealth/nodeHealth/subStatus 锁覆盖完整，无锁外写；持久化全走 `WriteFileAtomicDefault`，marshal 失败拒绝落盘（不写空）；恢复只补空缺带 7 天 TTL。
- **重试边界无惊群**：429 换出口有独立预算、总次数封顶 `max(retries,8)`，全冷却即交还 429；目录刷新有 inflight 去重 + 退避封顶 6h；401 退役、半开 CAS 所有 return 路径均终局收口。
- **资源管理**（同范围）：订阅抓取 defer 关 body + 8MiB 上限；headerTimer 每 attempt 必 Stop；attempt cancel 绑 body Close；非流式 5 分钟总超时；后台循环全部响应 appRootCtx。
- **协议转换主链路**与 CLIENT-COMPAT 承诺一致：工具 schema 两步 sanitize+normalize、temperature/top_p/stop 指针透传、SSE 心跳（15s/事件边界/仅客户端）、finish_reason 映射（Gemini SAFETY→content_filter）、metadata 剥离、P1-7/8 锚点函数真实存在。
- **依赖合规**：6 个直接依赖各有用途，`go mod verify` 通过；大件（anthropic-sdk-go 等）均为 sing-box 传递依赖，不违反"不引入新依赖"约定。

## 8. 结构与可维护性评价

- **做得好**：防御性注释密度罕见地高（锁顺序、取舍原因、事故复盘都写在代码里）；文件级拆分合理（本轮见证 proxy.go/admin HTML 两个大项落地）；测试 3.05 万行对源码 4.77 万行（64%），关键事故链均有回归钉子；`getZenConfig()` 返回深拷贝并写明理由，是全局配置的正确姿势。
- **结构债**：`internal/app` 仍是一个 300+ 文件的巨型包，包边界未拆；`anthropic.go`(1601)、`proxy_pool.go`(1259)、`proxy_stream.go`(1207)、`claude_tool_remap.go`(1046) 进入"每次改动全量回归"区间；全局包级状态仍多。
- **测试盲区与审计盲区重合**：protocol 21.8%、providers 41.6% 的覆盖率恰是本轮深审较薄的区域——补测试与补审计应一并做。

## 9. 覆盖声明（本轮审计的边界）

| 子系统 | 覆盖度 |
|---|---|
| 安全/构建/CI/依赖/文档 | 全覆盖精审（代理 + 审计者双重复核） |
| zen / 节点（7 文件：zen_call/cooldown/zen_state/zen_keys/node_health/sub/providers_catalog） | 全文精读 |
| 流式核心 | 约 10 文件精读（stream_*、error_rules、pool、json_to_sse、config_migrate、routing_dispatch 片段） |
| admin / webui / cmd / kit / cline auth | 广度审计（认证面全量核对，admin_batch*/dashboard 未逐行） |
| 协议转换 | 中等（claude_*、responses*、providers、protocol 全览；anthropic.go/max_tokens_helper/param_support/types 未逐行） |
| 未逐行覆盖的大文件 | anthropic.go、proxy_stream.go、proxy_pool.go、claude_tool_remap.go、providers_chat.go、zen.go、compact.go、adapters.go |

## 10. 建议处理顺序

1. **当天**：换订阅 + 清 `admin-render-test.js:573` 夹具、核销 `zen_keys_test.go:119` 的 key、release job 加事件门禁（三处都是小 diff）。
2. **本周**：`stream_early_eof.go` 失败路径补 `idle.Close()`（一行）；`zen_call.go` 调整 key/出口选择顺序；补 `.dockerignore`；`TestAllOutboundTypesRegistered` 加标签 skip；LICENSE 落盘。
3. **排期**：P2-5/P2-7（memo TTL、pool 快照化）、P3 并发与资源组、README/CLIENT-COMPAT 漂移集中一次清账、protocol/providers 补表驱动测试。
4. **观察**：工作树里 `models.go` 未提交改动（硬编码模型别名特殊分支）建议随下一提交一并 review——硬编码 ID 最好改为配置项。

> 审计全程零代码改动；本报告为本次新增的唯一文件。
