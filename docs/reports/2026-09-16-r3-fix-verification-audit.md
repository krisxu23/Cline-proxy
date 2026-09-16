# Cline-proxy 第三轮审计报告（R2 修复闭环验证 + 心跳事故修复复核）

- **审计对象**：`C:\Users\Administrator\WorkBuddy\2026-09-13-12-31-02\Cline-proxy`
- **审计日期**：2026-09-16
- **基线**：分支 `main` @ `aed184e`（ahead of origin/main 56）；工作树干净
- **方式**：**纯只读，零代码改动**。`git show` 增量复核 + 必需标签下 `go vet` / 全量 `go test` / `gofmt -l` / `admin-render-test.js`。未改任何业务代码，未引入依赖，未上传外部服务
- **增量范围**：R2 审计后 2 个提交——`aed184e`（F1–F7 全量修复）+ `4dcd86e`（心跳移到输出侧 + 非流式清洗）
- **前置报告**：`docs/reports/2026-09-15-r2-incremental-audit.md`（F1–F7 定义出处）

## 0. 执行摘要

结论一句话：**R2 审计开出的 F1–F7 已全部闭环，且 `4dcd86e` 用真实生产日志（11 次连续丢行）定位并根治了一个 R2 审计时未发现的真正的线上事故——旧 `heartbeatReader` 把心跳帧注入上游输入字节流，巨帧分片以 `\n` 结尾时被截断，黏合成非法行后被坏行门卫丢弃，客户端报你此前反复遇到的 `Bad control character / JSON parsing failed`。**

修复质量高：每次改动都带注释、回归用例与提交信息论证；全量测试、vet、渲染冒烟全绿。但是"改得多"本身带来一个新风险：`proxy.go` 流中继主路径两天内被重写两遍（F2 统一 + 4dcd86e 输出侧心跳），现在约 3200+ 行，建议冻结该路径一段观察期，只修 bug 不加功能。

本轮另发现 2 个新的 P2/P3 项（见 §3），无 P0/P1。

## 1. 验证台账（基线 aed184e 全部复现）

| # | 检查项 | 命令 | 结果 |
|---|---|---|---|
| V1 | 静态检查 | `go vet -tags "with_quic,with_grpc,with_utls" ./...` | 通过，退出码 0 |
| V2 | 全量测试 | `go test -count=1 -tags ... ./internal/...` | `app` / `app/translate` / `protocol` / `providers` / `translate` 全部 ok |
| V3 | 前端冒烟 | `node scripts/admin-render-test.js` | ALL PASS |
| V4 | 格式化 | `gofmt -l internal main.go` | 4 个文件未格式化（与 R2 相同：`cline/auth.go`、`providers/{doc,provider,router}.go`——**本次两个修复提交均未顺手修，仍挂着**） |
| V5 | 工作树 | `git status --short` | 空输出——审计全程零改动，可复查 |

## 2. R2 的 F1–F7 闭环逐项判定

| 项 | R2 定义 | 本次判定 | 证据 |
|---|---|---|---|
| F1 | `node_view.go:130` 无锁读 `exitFoldDupOf`（P1 竞争） | ✅ 闭环 | `exit_fold.go` 新增 `exitFoldDupTarget` 加锁访问器，面板路径改经访问器。顺带消除的是同一竞争模式 |
| F2 | 首行 SSE 分支与主循环双实现漂移 | ✅ 闭环，且超预期 | 收敛为单一 `handleLine` 闭包；额外修了 Cline 包裹形态 usage 先查外层导致漏记的**既有顺序 bug**；新增 `stream_firstline_test.go` |
| F3 | 清洗只覆盖流式 | ✅ 闭环 | `4dcd86e` 在 `handleNonStreamResponseWithUsage` 补 `sanitizeJSONControlChars`（R2 建议的"只差调用点"正是这么落的） |
| F4 | translate 新包零接线 + 双包同名 | ✅ 文档闭环，代码未动（符合预期） | 新包注释写明"批次⑤预铺未接线"；旧包分工注释 + 注册表三处（`HasRequest/TranslateRequest/RegisteredDirections`）加 `reqMu` 锁。前者 R2 本就建议"给时间点或删掉"——给了时间点，可接受 |
| F5 | 死代码 + StreamIdleSecs 无入口 | ✅ 闭环 | `exitFoldCount`、`HasChoices` 已删；`streamIdleSecs` 补 zen config 读+写口（0~1800s 校验，0=默认90）。面板 HTML 是否有对应输入框未验证（见 N1） |
| F6 | early-EOF 注释与行为不符 | ✅ 闭环 | 注释已改为"空闲超时→换站" |
| F7 | 测速用例防护弱 | ✅ 闭环 | sleep 前置到每块写入之前 |

## 3. 本轮新发现（2 项，非阻塞）

### N1（P2）StreamIdleSecs 的面板 UI 入口未确认

`admin_zen.go` 已补 `streamIdleSecs` 读写口，但 R2 审计的 grep 显示面板 HTML 无对应输入框，本次 `grep streamIdleSecs internal/app/web/` 无命中（且 `web/` 目录可能就不存在）。若面板确实没入口，该字段只能手改 JSON——R2 的 F5 算"半闭环"。建议：要么在面板出口配置区加一个数字输入框，要么在文档注明手改路径。**未改代码，待你确认。**

### N2（P3）心跳重写的残留引用与测试口径

- `heartbeatReader` 旧类型已删除，新 `sseHeartbeat` 落地；但 `error_rules_test.go:1`、`proxy.go:2`、`stream_keepalive.go:2` 仍有 `heartbeatReader` 字样（注释或改写后的用例名残留）。无功能影响，仅卫生项；
- `TestSSEHeartbeatNeverCorruptsRealFrames`（半帧中途静默必须零字节）是事故的反向回归断言——写法正确，点名表扬。建议后续任何再动心跳的人先跑它。

## 4. 对 4dcd86e（心跳事故修复）的专项评价

这是本轮最值得细读的一个提交，几个判断：

1. **根因定位可信**：提交信息给出 22:06–22:10 连续 11 次丢行的生产日志实证，黏合行样本（`{"id":"chatdata: {...`）与"心跳插进巨帧中间"的机制解释自洽，且对齐了 OmniRoute `earlyStreamKeepalive.ts` 的"心跳只去客户端"设计。这不是拍脑袋重构，是证据驱动的修复；
2. **修法彻底**：输出侧 `sseHeartbeat` 只在 SSE 事件边界（输出以空行结尾）注入，半帧中途静默宁可不发——把"截帧"从概率事件变成不可能事件；同时堵死了非 data 行原样透传的最后一个口子（裸 JSON 分片/黏合帧一律先 NDJSON 抢救）；
3. **风险提示**：流中继主路径两天两轮大改（F2 统一 + 本次心跳重写），`handleStreamResponseWithUsage` 现在是多层包装（idle → 心跳输出侧 → 清洗 → 形态判定 → 统一行处理）。逻辑都对，但**复杂度已到建议冻结线**：未来两周该函数只接受 bugfix，不接受新形态分支。

## 5. 与你此前 ZCode 报错的关系（更新）

R2 报告写的是"网关侧已修、ZCode 直连仍会遇到"。本次 `4dcd86e` 把结论推进了一步：**你那两次 `bai:qwen3.8-flash` 的 "Bad control character / C2PA _manifest" 报错，恰好就是旧 `heartbeatReader` 截帧 + 坏行门卫丢弃的完整事故链**——如果当时你的请求是经过这个网关转发的，那么用新基线重新构建 exe 后，这类报错应该消失（脏行要么被清洗救回，要么被合成 SSE，要么被丢弃并记日志，不再原样递给客户端）。如果仍出现，请把 `data/cline-proxy.log` 里同时间段的 `stream:` 日志发我，那是下一步定位的直接证据。

## 6. 遗留与下一步

| 事项 | 状态 |
|---|---|
| gofmt 4 文件 | 未动，建议下次顺手 `gofmt -w`（改动小，可单独一提交） |
| proxy.go 拆分（R2 P2） | 未动；现约 3200+ 行，优先级继续上升，但建议先过两周观察期再拆 |
| sing-box 日志开关 / 启动检测中间态 | 仍未动，属已知 backlog |
| translate 批次⑤接线 | 作者已承诺时间点，本轮不追 |

> 一句话：R2 的七项已全关，新事故修得漂亮；现在最该做的是**重新构建交付 exe**（修复只在代码里，不在你机器上跑的那个旧二进制里），然后给流中继两周观察期。
