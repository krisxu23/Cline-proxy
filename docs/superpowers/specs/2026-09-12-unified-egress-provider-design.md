# 统一出口 + 统一 Provider 设计（2026-09-12）

> 目标：代理必走代理、直连必走直连；供应商统一成一套，内置 opencode + cline 反代置顶；对外一个地址 + 一个 key。
> 与 `2026-09-12-autogate-merge-spec.md` 第 6 节兼容：模型 ID 别名不变、地区探测降为 provider 可选开关、rescueDirect 语义保留。

## 1. 架构

```
客户端 -> Go HTTP 入口（本机监听，不进 sing-box）
  -> 统一路由（别名展开：zen/xxx、provider:model、cline-pass/xxx -> provider/model）
  -> 统一 Provider 执行（key 轮询 + 健康 + 冷却 + 换站）
  -> 唯一拨号口 zenDialContext -> sing-box（节点入站 in-i / catch-all）
  -> 上游（opencode.ai / cline / 自建）
```

sing-box 仍是进程内 `box.New + Start`（`nodes.go`），只做翻译+出境，不做选路决策。

## 2. 统一 Provider（抄 ai-gateway）

```go
Provider{id, name, baseUrl, apiType(openai|anthropic), apiKeys[{key,enabled}], models[{id,enabled}], enabled}
```

- 内置 `order:[opencode, cline]` 置顶，不可删，只能改 key/启用；其余自由增删。
- `opencode`：匿名 + 镜像故障转移；`cline`：账号池；自建：Bearer 轮询。
- 删除 `catalog/pricing/freeModels/modelsUrl` 免费判定，`enabled` 即为准。
- `/v1/models` 按置顶顺序聚合 `provider/model`。
- 兼容别名：`zen/mimo-v2.5-free` -> `opencode/mimo-v2.5-free`，`openrouter:xxx` -> `openrouter/xxx`，`cline-pass/xxx` -> `clinepass/xxx`。

## 3. 统一出口（推倒代理部分）

- 启动链：网关起 -> sing-box 起 -> 解析订阅+手动节点 -> 每节点 `outbound + 127.0.0.1:mixed` 入站 + 常驻 `in-catchall` -> 输出 socks5 表。
- 健康：拿 socks5 表打 opencode 最小 chat，通=健康，定时复检；未探测按可用（启动即用）。
- 请求：只从健康表 `round_robin/random/fill` 轮询，失败冷却换下一站，池空按 `rescueDirect` 决定报错或 catch-all（默认 true 保持现状，strict 模式设 false 即 fail-closed）。
- 删除 `model_region.go` 全套（种子/探测/🌍/pickZenProxyForModel），地区降为 provider 开关（默认仅 opencode 开），其余 provider 不跑。
- 所有出口（含订阅抓取/模型同步/DNS 解析器选择）走 `zenDialContext`；日志用 `reqExitKey(ctx)` 回读真实出口，禁全局轮询位置冒充。
- 指定出口的新路径（深检/竞速）各自新建 transport，不走共享 `zenHTTPClient` h2 池（§8.2 陷阱）。

## 4. Key 健康（抄 ai-gateway proxy.ts）

多 key 洗牌；`401/403/5xx`/网络错记失败，5 次冷却 5 分钟到期试用；`429`跳过不记；成功清零；单 key 跳过检查直接用。

## 5. 错误处理

- 模型格式错/供应商不存在/未启用/模型未配置/禁用：400/404/403 直接返回。
- key 全灭：502 `key_exhausted` 带最后一次错误摘要。
- 上游 400/404 直接返回；`rescueDirect=false` 且池空：502 `没有可用节点`，不直连。

## 6. 测试

- 单元：别名解析、两级启用过滤、key 健康计数/冷却/试用/恢复、池空 fail-closed/open 两态、h2 池不串出口（单测 transport 隔离）。
- 冒烟：拷贝 `data` 起实例，proxy 模式拔掉节点 -> 期望报错非直连；接回 -> `muse-spark` 与 `mimo` 同一路轮询；`/v1/models` 顺序置顶。
