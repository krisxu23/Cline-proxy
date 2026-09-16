# Cline-proxy ↔ OmniRoute 功能模块映射表

> 用途：逐字逐句照抄工作的地基。每个我方文件都必须在本表中找到定位——
> 要么指向 OmniRoute 的参考实现（照抄），要么明确标注「OmniRoute 无对应」（自研）。
>
> 我方基线：`internal/` + `cmd/`，112 个功能文件 / 31,466 行
> 参考基线：`D:\OmniRoute\resources\app\open-sse\`（核心逻辑）+ `src\sse\`（请求编排）

---

## 一、映射总览

### 1.1 对话处理与工具调用（最高优先级 — 直接决定 AI 准确度）

| 我方文件 | 行数 | OmniRoute 参考 | 状态 |
|---|---|---|---|
| `internal/app/anthropic.go` | 1030 | `open-sse/translator/*` + `handlers/chatCore.ts` | ⬜ 待对照 |
| `internal/app/proxy_stream.go` | 764 | `utils/stream.ts` + `utils/streamHelpers.ts` + `utils/streamEmptyChoices.ts` | 🟡 部分照抄（hasValuableContent 已抄） |
| `internal/app/responses.go` | 615 | `handlers/responsesHandler.ts` + `transformer/responsesTransformer.ts` | ⬜ 待对照 |
| `internal/app/zen_responses_convert.go` | 590 | 同上 | ⬜ 待对照 |
| `internal/app/compact.go` | 657 | `services/contextManager.ts` + `services/contextHandoff.ts` + `utils/flattenToolHistory.ts` | ⬜ 待对照 |
| `internal/app/protocol_normalize.go` | 157 | `services/roleNormalizer.ts` + `utils/toolCallArguments.ts` | ✅ 已照抄（T18 归一 + 参数增量拼接） |
| `internal/app/textual_tool_call.go` | 268 | `utils/textualToolCall.ts`（112 行，4 导出） | ✅ 已照抄（新增） |
| `internal/app/textual_tool_call_collect.go` | 300 | `utils/stream.ts:261-339, 2506-2529` | ✅ 已照抄并接入（新增） |
| `internal/app/tool_schema_sanitizer.go` | 300 | `services/toolSchemaSanitizer.ts` | ✅ 已照抄（新增） |
| `internal/app/thought_signature.go` | 281 | `services/geminiThoughtSignatureStore.ts` + `services/signatureCache.ts` | ⬜ 待对照 |
| `internal/app/zen_reasoning.go` | 437 | `utils/reasoningPlaceholder.ts` + `reasoningFields.ts` + `reasoningContentInjector.ts` + `services/opencodeReasoningSanitizer.ts` | ✅ 已照抄 |
| `internal/app/zen_responses_quirks.go` | 188 | `services/muse-spark-web` 相关 quirk | ⬜ 待对照 |
| `internal/app/adapters.go` | 298 | `handlers/chatCore.ts` 的 executor 分派 | ⬜ 待对照 |

**关键参考模块（OmniRoute 侧，对话域）**：
- `utils/streamHelpers.ts` — `hasValuableContent`（权威「有价值 chunk」判定）
- `utils/toolCallArguments.ts` — 工具参数增量拼接
- `services/toolSchemaSanitizer.ts` — 工具 schema 清洗
- `services/responsesInputSanitizer.ts` — Responses 输入清洗
- `services/responsesItemId.ts` — Responses item id 生成
- `handlers/chatCore/openAICompatibleTools.ts` — OpenAI 兼容工具转换
- `handlers/chatCore/claudeToolDefaults.ts` — Claude 工具默认值
- `handlers/chatCore/toolCallingRequiredCheck.ts` — 工具调用必需性检查
- `utils/textualToolCall.ts` — 文本形态工具调用解析
- `utils/composerToolCalls.ts` — 组合工具调用
- `utils/flattenToolHistory.ts` — 历史工具调用扁平化
- `services/claudeCodeToolRemapper.ts` — 工具名重映射
- `services/responsesToolHandoff.ts` — 工具交接

---

### 1.2 请求处理与协议转换

| 我方文件 | 行数 | OmniRoute 参考 | 状态 |
|---|---|---|---|
| `internal/protocol/openai_anthropic.go` | 366 | `translator/` + `utils/anthropicHost.ts` | ⬜ 待对照 |
| `internal/protocol/responses_chat.go` | 241 | `transformer/responsesTransformer.ts` | ⬜ 待对照 |
| `internal/protocol/normalize.go` | 135 | `services/roleNormalizer.ts` | ⬜ 待对照 |
| `internal/protocol/streaming.go` | 173 | `utils/stream.ts` 的 ScanSSE 等价物 | ⬜ 待对照 |
| `internal/protocol/util.go` | — | `utils/number.ts` + `utils/estimateSize.ts` | ⬜ 待对照 |
| `internal/protocol/time.go` | — | — | ✅ 自研（无对应） |
| `internal/translate/translate.go` | 342 | `translator/registry.ts` + `translator/formats.ts` | ⬜ 待对照 |
| `internal/translate/claude_to_openai.go` | 257 | `translator/` Claude↔OpenAI | ⬜ 待对照 |
| `internal/translate/gemini.go` | 388 | `executors/vertex.ts` + Gemini 转换 | ⬜ 待对照 |
| `internal/app/translate_registry/translate.go` | 161 | `translator/registry.ts` | ⬜ 待对照 |
| `internal/app/json_to_sse.go` | 131 | `utils/jsonToSse.ts` + `handlers/chatCore/jsonBodyToSse.ts` | ⬜ 待对照 |

**关键参考模块（请求域）**：
- `handlers/chatCore/requestFormat.ts` / `targetFormat.ts` — 格式判定
- `handlers/chatCore/unsupportedParamsStrip.ts` — 不支持参数剥离
- `handlers/chatCore/passthroughHelpers.ts` / `passthroughToolNames.ts` — 透传
- `handlers/chatCore/requestSetup.ts` — 请求装配
- `services/systemTransforms.ts` / `systemPrompt.ts` — 系统提示处理
- `services/targetRequestSanitizer.ts` — 目标请求清洗
- `services/payloadRules.ts` — payload 规则
- `utils/responsesInputNormalization.ts` — Responses 输入归一

---

### 1.3 模型与供应商管理（用户点名的重点域）

| 我方文件 | 行数 | OmniRoute 参考 | 状态 |
|---|---|---|---|
| `internal/app/providers_catalog.go` | 754 | `services/model.ts` + `executors/registry.ts` + `services/modelCapabilities.ts` | ⬜ 待对照 |
| `internal/app/providers_config.go` | 506 | `config/providerRegistry.ts` + `services/provider.ts` | ⬜ 待对照 |
| `internal/app/providers_chat.go` | 432 | `handlers/chatCore.ts` 的 provider 分派 | ⬜ 待对照 |
| `internal/app/models.go` | 318 | `services/model.ts` + `services/modelLifecycle.ts` | ⬜ 待对照 |
| `internal/app/model_region.go` | 348 | `services/specificityDetector.ts` + 地区策略 | ⬜ 待对照 |
| `internal/app/gemini_quota.go` | 204 | `services/geminiRateLimitTracker.ts` | ⬜ 待对照 |
| `internal/app/zen_models.go` | 173 | `executors/opencode.ts` 模型列表 | ⬜ 待对照 |
| `internal/app/zen_models_cache.go` | — | 同上 | ⬜ 待对照 |
| `internal/app/zen_model_health.go` | — | `services/modelLifecycle.ts` | ⬜ 待对照 |
| `internal/providers/provider.go` | — | `executors/base.ts` 接口 | ⬜ 待对照 |
| `internal/providers/router.go` | — | `executors/registry.ts` | ⬜ 待对照 |
| `internal/providers/clinepass.go` | 363 | `executors/clinepassModels.ts` + `utils/clinepassEnvelope.ts` | ⬜ 待对照 |
| `internal/app/clinepass.go` | 303 | 同上 | ⬜ 待对照 |

**关键参考模块（模型/供应商域）**：
- `config/providerRegistry.ts` — 供应商注册表（**唯一真源**）
- `services/modelCapabilities.ts` — 模型能力表
- `services/modelStrip.ts` — 模型名剥离
- `services/modelFamilyFallback.ts` — 模型族回退
- `services/modelEndpointPolicy.ts` — 端点策略
- `services/modelDeprecation.ts` / `modelLifecycle.ts` — 生命周期
- `services/providerCooldownTracker.ts` — 供应商冷却
- `services/providerRequestDefaults.ts` / `providerDefaultRateLimit.ts` — 默认值
- `services/defaultReasoningEffort.ts` / `thinkingBudget.ts` — 推理档位
- `services/qwenThinking.ts` / `mimoThinking.ts` / `cloudCodeThinking.ts` — 各家族 thinking
- `services/tierResolver.ts` / `tierConfig.ts` — 分层

---

### 1.4 流式处理、错误恢复与故障转移（本轮已知差距集中区）

| 我方文件 | 行数 | OmniRoute 参考 | 状态 |
|---|---|---|---|
| `internal/app/proxy_stream.go` | 764 | `utils/stream.ts` + `streamHelpers.ts` + `streamEmptyChoices.ts` + `streamFailureFinalization.ts` | 🟡 部分照抄 |
| `internal/app/stream_early_eof.go` | ~80 | `utils/streamReadiness.ts` `ensureStreamReadiness` | 🔴 **需重写**（判定强度不足） |
| `internal/app/stream_idle.go` | 106 | `utils/streamTiming.ts` + `directResponseStartTimeout.ts` | ⬜ 待对照 |
| `internal/app/stream_keepalive.go` | 169 | `utils/earlyStreamKeepalive.ts` + `earlyKeepaliveByteBuffer.ts` + `keepaliveThreshold.ts` + `sseHeartbeat.ts` | ⬜ 待对照 |
| `internal/app/cooldown.go` | 401 | `services/providerCooldownTracker.ts` + `handlers/chatCore/cooldownClassification.ts` | ⬜ 待对照 |
| `internal/app/error_rules.go` | — | `services/errorClassifier.ts` + `utils/streamErrorFormat.ts` + `upstreamErrorPassthrough.ts` | ⬜ 待对照 |
| `internal/app/exit_select.go` | 185 | `services/proxyAutoSelector.ts` | ⬜ 待对照 |
| `internal/app/exit_fold.go` | — | `services/proxyFamily.ts` + `proxyFamilyResolve.ts` | ⬜ 待对照 |
| `internal/app/exit_region.go` | 364 | 地区过滤（OmniRoute 无直接对应） | 🟡 部分自研 |

**关键参考模块（流式/恢复域）**：
- `utils/streamReadiness.ts` — **提交前预检（本轮核心）**
- `utils/streamReadinessPolicy.ts` — 预检超时策略
- `utils/streamEmptyChoices.ts` — 空 choices 拦截
- `utils/streamFailureFinalization.ts` — 失败收尾
- `utils/streamRecovery.ts` — HoldbackBuffer 透明重试
- `utils/throughputWatchdog.ts` — 吞吐看门狗
- `utils/streamContentWatcher`（在 streamReadiness.ts 内）— **放行后零交付观测（我方缺）**
- `services/errorClassifier.ts` — 错误分类（含 LEGIT_EMPTY_* 常量）
- `utils/responseSanitizer.ts` / `handlers/responseSanitizer.ts`
- `utils/passthroughTailProcessor.ts` — 尾部处理

---

### 1.5 节点池与出口选择

| 我方文件 | 行数 | OmniRoute 参考 | 状态 |
|---|---|---|---|
| `internal/app/pool.go` | 662 | `services/sessionPool/` + `accountSelector.ts` + `accountSemaphore.ts` | ⬜ 待对照 |
| `internal/app/proxy_pool.go` | 610 | `services/proxyAutoSelector.ts` + `utils/proxyDispatcher.ts` | ⬜ 待对照 |
| `internal/app/node_parse.go` | 616 | — | ✅ 自研（协议解析，OmniRoute 无对应） |
| `internal/app/node_probe.go` | 532 | `services/webSessionPoolHealth.ts` | ⬜ 待对照 |
| `internal/app/node_build.go` | 243 | — | ✅ 自研（sing-box 配置生成） |
| `internal/app/node_classify.go` | 251 | — | ✅ 自研 |
| `internal/app/node_health.go` | 163 | `services/webSessionPoolHealth.ts` | ⬜ 待对照 |
| `internal/app/node_upstream.go` | 237 | `services/nodeUpstream`（我方特有） | 🟡 自研 |
| `internal/app/nodes.go` | 383 | — | ✅ 自研 |
| `internal/app/nodes_dns.go` | 127 | `utils/socksConnectorWithFamily.ts` | ⬜ 待对照 |
| `internal/app/socks5_dial.go` | 234 | `utils/socksConnectorWithFamily.ts` + `utils/proxyFamily.ts` | ⬜ 待对照 |
| `internal/app/node_ports_traffic.go` | 373 | — | ✅ 自研 |
| `internal/app/node_view.go` | 203 | — | ✅ 自研 |
| `internal/app/sub.go` | 700 | — | ✅ 自研（订阅） |
| `internal/app/outbound_url.go` | 125 | `utils/urlSanitize.ts` | ⬜ 待对照 |

---

### 1.6 鉴权、请求头指纹与用量计量

| 我方文件 | 行数 | OmniRoute 参考 | 状态 |
|---|---|---|---|
| `internal/cline/auth.go` | 372 | `services/tokenRefresh.ts` + `services/tokenRefresh/` | ⬜ 待对照 |
| `internal/app/headers_sync.go` | 260 | `handlers/chatCore/executorClientHeaders.ts` + `upstreamExecuteHeaders.ts` | ⬜ 待对照 |
| `internal/app/usage.go` | 325 | `utils/usageTracking.ts` + `services/usage.ts` | ⬜ 待对照 |
| `internal/app/stats.go` | 451 | `utils/usageTracking.ts` | ⬜ 待对照 |
| `internal/app/req_trace.go` | 464 | `handlers/chatCore/stageTrace.ts` | ⬜ 待对照 |
| `internal/app/logs.go` | 505 | `utils/requestLogger.ts` + `providerRequestLogging.ts` | ⬜ 待对照 |
| `internal/app/zen_config_store.go` | 160 | `services/rotationConfig.ts` | ⬜ 待对照 |
| `internal/app/admin_auth.go` | 202 | `services/keyGroupAuth.ts` + `ipFilter.ts` | ⬜ 待对照 |

**关键参考模块（鉴权/指纹域）**：
- `services/claudeCodeFingerprint.ts` — 指纹（**请求头自动同步的权威参考**）
- `services/claudeCodeObfuscation.ts` — 混淆
- `services/claudeCodeCCH.ts` / `claudeCodeExtraRemap.ts`
- `services/ccBridgeTransforms.ts`
- `services/rollingRpmGate.ts` / `slidingWindowLimiter.ts` — 限流
- `services/quotaPreflight.ts` / `quotaMonitor.ts` — 配额预检
- `services/tokenLimitCounter.ts` — token 计数

---

### 1.7 AI 响应非流式路径（**已修 ①**）

| 我方文件 | 行数 | OmniRoute 参考 | 状态 |
|---|---|---|---|
| `internal/app/proxy_stream.go`（合法空终止态） | — | `services/errorClassifier.ts:14-15` + `utils/streamReadiness.ts:166` | ✅ **已照抄** |

**已修复的 Bug ①：合法空终止态白名单缺失**

OmniRoute 权威定义（`services/errorClassifier.ts:14-15`）：

```ts
const LEGIT_EMPTY_CLAUDE_STOP = new Set(["max_tokens", "tool_use"]);
const LEGIT_EMPTY_OPENAI_FINISH = new Set(["length", "tool_calls", "content_filter"]);
```

原注释点明用途：被 token 上限截断、或纯工具调用回合的空内容，是**合法的成功完成**，
不是静默假成功 —— 「Used to avoid rewriting a valid HTTP 200 (e.g. a Claude Code
`max_tokens: 1` connectivity ping) into a synthetic 502」。

`utils/streamReadiness.ts:166` 把它合并成 5 元素白名单：

```ts
const LEGIT_EMPTY_TERMINAL_REASONS = new Set([
  "length", "tool_calls", "content_filter", "max_tokens", "tool_use",
]);
```

**我方原有缺陷**：`streamDeliveredValue` 只有两个条件（`forwardedValuableChunk ||
hasValidUsage`），缺第三个 `legitEmpty`。后果是**纯工具调用回合**（agent 场景下
每轮调工具本来就只有 `tool_calls` 而无正文）与**被 token 上限截断的回合**会被误判
为空流、改写成合成的 502。

**已实现的修复**（`internal/app/proxy_stream.go`）：
- 新增 `legitEmptyTerminalReasons` map（5 个值，逐字对齐）
- 新增 `legitEmptyTerminalReason(obj)` —— 认 `choices[0].finish_reason`（OpenAI）
  与顶层 `stop_reason`（Claude 形态）
- 新增状态 `sawLegitEmptyTerminal`，在三处判定点（NDJSON 路径 / 完整 JSON body 路径 /
  主循环收尾）统一接入
- `streamDeliveredValue` 签名扩为三参数

**负向验证证据**（关键区分，务必保留）：
- 带 `delta.tool_calls` 的帧 → `hasValuableContent` 已判真，白名单**冗余**
- 带 `choices[0].finish_reason` 的帧 → `hasValuableContent` 已判真，白名单**冗余**
- **只有顶层 `stop_reason` 的 Claude 形态帧 → `hasValuableContent` 不认
  （它只看 `choices[0].delta.*` 与 `firstChoice.finish_reason`），白名单是唯一防线**
- 实测：把 `legitEmptyTerminalReason(normalized)` 改成 `&& false` 后，
  `Test空流_仅顶层stop_reason必须通过` 由「通过」变「被判空流」；
  而带 tool_calls / finish_reason 的用例不受影响 —— 证明白名单非装饰。

**新增测试 6 组**：`TestLegitEmptyTerminalReason`（8 子例）、
`Test空流_仅顶层stop_reason必须通过`（2 子例）、`Test空流_顶层stop_reason非白名单值仍失败`、
`Test空流_纯工具调用回合必须通过`、`Test空流_被token上限截断必须通过`、
`Test空流_完整JSON纯工具调用必须通过`、`Test空流_finish_reason帧按参考实现算有价值`、
`Test空流_空choices数组才是真空流`。

**同时澄清的语义**（防止后人"顺手加强"而漂移）：参考实现定义的「空流」只有一种 ——
**`choices` 为空数组**。OmniRoute 自己的参考用例
（`tests/unit/stream-empty-choices-interceptor.test.ts`）构造空流只用
`emptyChoicesChunk`（`choices: []`），从不使用「`finish_reason=stop` + 空 delta」。

**待办 ②**：`nonStreamingResponseParse.ts` / `nonStreamingSse.ts` /
`nonStreamingJsonResponse.ts` / `nonStreamingResponseBody.ts` 四个专用模块尚未对照，
需检查我方**非流式**路径是否遗漏同类判定。

---

## 二、优先级排序（照抄执行顺序）

| 序 | 域 | 任务 | 理由 |
|---|---|---|---|
| 1 | 对话/工具调用 | #34 | 用户点名最高优先级，直接影响 AI 准确度与工具调用 |
| 2 | 流式/错误恢复 | #37 | 本轮报错根因所在，已有明确差距清单 |
| 3 | 模型/供应商 | #36 | 用户点名重点域 |
| 4 | 请求/协议转换 | #35 | 数据正确性基础 |
| 5 | 鉴权/指纹/计量 | #39 | 请求头对齐官方 Cline CLI |
| 6 | 节点池/出口 | #38 | 1% 可达率根因 |

---

## 三、照抄纪律（每处改动必须满足）

1. **逐字对照**：先在 OmniRoute 找到对应实现，读完整函数（含注释），再写 Go。
2. **注释写明出处**：`// 照抄 OmniRoute open-sse/utils/xxx.ts:NNN funcName`。
3. **负向验证**：改坏 → 测试必须变红 → 还原 → md5 比对确认字节一致。
4. **全绿门槛**：gofmt 干净 / vet 通过 / build 通过 / test 全绿 / -race 0 竞争。
5. **交付验证**：`grep -a '<唯一字符串>' exe` 确认修复真编进二进制。

---

## 四、排查进度

| 序 | 域 | 任务 | 状态 |
|---|---|---|---|
| 0 | 映射表 | #33 | ✅ 完成 |
| 1 | 对话/工具调用 | #34 | 🟡 进行中（已抄 4 项：T18 归一 / 参数增量拼接 / 工具 schema 清洗 / 文本型工具调用解析） |
| 2 | 流式/错误恢复 | #37 | 🟡 进行中（已修「合法空终止态」） |
| 3 | 模型/供应商 | #36 | ⬜ 待开始 |
| 4 | 请求/协议转换 | #35 | ⬜ 待开始 |
| 5 | 鉴权/指纹/计量 | #39 | ⬜ 待开始 |
| 6 | 节点池/出口 | #38 | ⬜ 待开始 |
| 7 | 全量验证交付 | #40 | ⬜ 待开始 |

### 已完成的照抄项

| 项 | 出处 | 文件 | 验证 |
|---|---|---|---|
| 合法空终止态白名单 | `services/errorClassifier.ts:14-15` + `utils/streamReadiness.ts:166` | `proxy_stream.go` | ✅ 负向验证通过，22 测试绿，race 0 |
| 有价值 chunk 判定 | `utils/streamHelpers.ts:379` | `proxy_stream.go` | ✅ 前一轮 |
| 空流拒绝 | `utils/streamEmptyChoices.ts:70` | `proxy_stream.go` | ✅ 前一轮 |
| reasoning 四模块 | `reasoningPlaceholder/Fields/ContentInjector` + `opencodeReasoningSanitizer` | `zen_reasoning.go` | ✅ 前一轮 |
| T18 `finish_reason` 归一 | `utils/stream.ts:1953` + `handlers/responseSanitizer.ts:302` + `handlers/chatCore/passthroughToolNames.ts:93` | `protocol_normalize.go` | ✅ 负向验证通过（2 红 3 绿），md5 `b98741aa` |
| 工具参数增量拼接 | `utils/toolCallArguments.ts:41` | `protocol_normalize.go` | ✅ 10 子例 + 对象形态回归 |
| 工具 schema 清洗 | `services/toolSchemaSanitizer.ts`（enum 剔 null / required 过滤 / 元组降级 / 根补 type） | `tool_schema_sanitizer.go` | ✅ 负向验证通过（10 红），md5 `321937bd` |
| 文本型工具调用解析 | `utils/textualToolCall.ts`（4 导出） | `textual_tool_call.go` | ✅ 负向验证通过（5 红），md5 `19a2b136` |
| 文本型工具调用收集与接入 | `utils/stream.ts:261-339` + `:2506-2529` | `textual_tool_call_collect.go`（接入 `proxy_stream.go` / `proxy_cline.go`） | ✅ 双负向验证通过（2 红 / 4 红），md5 `7d7d2e46` |
| 工具历史扁平化 | `utils/flattenToolHistory.ts`（117 行，4 分支）+ `translator/helpers/geminiHelper.ts:284-294` 的 `extractTextContent` | `flatten_tool_history.go` | ✅ 双负向验证通过（2 红 / 3 红），md5 `b8e751fd` |
| 上游不支持参数剥离 | `translator/paramSupport.ts:32-96`（`STRIP_RULES` 11 条）+ `:108-135`（`applyMaxOutputClamp`）+ `:141-166` | `param_support.go`（接入 `providers_chat.go:chatWithKey`） | ✅ 负向验证通过（11 红），md5 `37a86170`；已补接入级用例 |
| Claude 工具 type 默认值 | `handlers/chatCore/claudeToolDefaults.ts`（27 行） | `claude_tool_defaults.go` | ✅ 负向验证通过（1 红），md5 `2f47fb69`；**未接入，理由见下** |
| OpenAI 兼容工具归一 | `handlers/chatCore/openAICompatibleTools.ts`（46 行） | `openai_compatible_tools.go` | ✅ 负向验证通过（2 红），md5 `24137817`；**未接入，理由见下** |
| 工具调用必需性护栏 | `handlers/chatCore/toolCallingRequiredCheck.ts`（33 行） | `tool_calling_required_check.go` | ✅ 负向验证通过（2 红），md5 `5b88426e`；**未接入，理由见下** |
| Responses item id 守卫 | `services/responsesItemId.ts`（7 行） | `responses_item_id.go` | ✅ 负向验证通过（1 红），md5 `c3e94911`；**未接入，理由见下** |
| 严格 provider 的 system 消息提升 | `translator/helpers/strictSystemHoist.ts`（66 行）+ `src/lib/memory/injection.ts:84`/`:90-118` | `strict_system_hoist.go`（接入 `providers_chat.go:chatWithKey`） | ✅ 负向验证通过（11 红），md5 `654380b4`；已补接入级用例 |
| max_tokens 上下文调整 | `translator/helpers/maxTokensHelper.ts`（21 行）+ `config/constants.ts:151`/`:154` | `max_tokens_helper.go` | ✅ 负向验证通过（3 红），md5 `0d998ee4`；**未接入，理由见下** |
| Claude 工具 schema 净化 | `translator/helpers/schemaCoercion.ts:187-223`（`stripUnsupportedRegexPatterns`）+ `:500-623`（`coerceNumericString` / `coerceIndexedObjectToArray` / `stripInvalidSchemaConstructs`）+ `:625-639`（`sanitizeClaudeToolSchema(s)`） | `schema_coercion.go`（**已接入** `anthropic.go:anthropicToolsToOpenAI`） | ✅ 双负向验证通过（纯函数 4 红 / 接入 1 红），md5 `303e5bd8`；接入点对位 `executors/base.ts:970` + `cliproxyapi.ts:347` |
| 工具配对修复（四遍扫描） | `services/contextManager.ts:717-819`（`fixToolPairs`） | `compact.go:fixToolPairs`（**重写为照抄版**） | ✅ 负向验证通过（3 红），md5 `60ad2116`。**修正 4 处实质差异，见下** |
| 工具结果相邻性 + 尾部孤儿守卫 | `services/contextManager.ts:829-1000`（`fixToolAdjacency` / `stripTrailingAssistantOrphanToolUse` / `stripTrailingAssistantForProvider`） | `tool_adjacency.go`（**已接入** `providers_chat.go:chatWithKey`, Claude-only） | ✅ 负向验证通过（3 红）+ 接入级 4 用例 + 源码层接线断言，md5 `89dbd78a`。调用链逐字对位 `executors/base.ts:1320-1339` |
| 空 reasoning_content 回放（含接线 gate） | `translator/index.ts:610-619` + `provider.ts:471-473` + `services/reasoningCache.ts:83-121` + `schemaCoercion.ts:455-487` | `reasoning_replay.go`（**已接入** `providers_chat.go:chatWithKey` 的非 anthropic 分支） | ✅ 双负向验证通过（NEG-Q 2 红 / NEG-R 4 红）+ 源码层接线断言，md5 `73d532b9`。★ 接线 gate 与 inject 内部判定**故意不对称**（`allowLegacyFallback=false` + 真实 `hasThinkingConfig`），480 组枚举里 256 组不同 |
| Claude 工具顺序归一（Pass 1.5） | `translator/helpers/claudeHelper.ts:160-284`（`fixToolUseOrdering` 三步） | `claude_tool_ordering.go`（**已接入** `providers_chat.go:chatWithKey` 的 anthropic 分支，`splitMisplacedToolResults` 之后） | ✅ 负向验证通过（NEG-TOJ 3 红：形态守卫 + 两条既有接入测试），md5 `aa749c15`。★ 含**形态守卫**（见下） |
| Claude prompt-cache 断点重锚 + `output_config` 剥离 | `translator/helpers/claudeHelper.ts:286-293`/`:295-302`/`:307-328`/`:342-347`/`:387-396`/`:408-413`/`:499-513`/`:527-544`/`:727-740` | `claude_cache_control.go`（**已接入** `providers_chat.go:chatWithKey` 的 anthropic 分支） | ✅ 双负向验证通过（NEG-CACHE-A 1 红 / NEG-CACHE-B 1 红）+ 23 用例，md5 `bdffa2c8` |
| 工具调用参数清洗 shim（Read / submit_pr_review） | `translator/helpers/toolCallShim.ts`（129 行，含 `coerceToArray` / `isValidPdfPagesArg` / `sanitizeReadArgs` / `TOOL_SHIMS` / `resolveToolCallShim` / `applyToolCallShimToBuffer`） | `tool_call_shim.go`（**已接入** `anthropic.go:emitToolBlock`，即组装完成后、发 `input_json_delta` 之前） | ✅ 双负向验证通过（NEG-SHIM-A 实现级 1 红 / NEG-SHIM-B 作用域级 2 红），md5 `63c37be2` |
| Claude thinking 块归一（Pass 2） | `translator/helpers/claudeHelper.ts:546-719` + `config/defaultThinkingSignature.ts:2-3` + `utils/reasoningPlaceholder.ts:6` | `claude_thinking_blocks.go`（**已接入** `providers_chat.go:chatWithKey` 的 anthropic 分支，`reanchorClaudePromptCache` 之后） | ✅ 双负向验证通过（NEG-THINK-A 接线 1 红 / NEG-THINK-B 实现 6 红）+ 15 用例 + 接线锁，md5 `063a59ee` |
| 第三方工具名伪装 + 双向还原 | `services/claudeCodeToolRemapper.ts`（483 行）+ `services/claudeCodeExtraRemap.ts`（18 行）+ `translator/helpers/toolCallHelper.ts:100-157`（`caseInsensitiveToolNameLookup` / `restoreOpenAIToolNames`） | `claude_tool_remap.go`（**已接入 5 处**：请求侧 `providers_chat.go:chatWithKey` anthropic 分支；响应侧 `anthropic.go:emitToolBlock` 流式 + `openAIToAnthropicWithMap` 非流式 + `proxy_stream.go` 两个 OpenAI 形态还原点；映射经 `routing_dispatch.go` 的 `hopParams` 透传） | ✅ 双负向验证通过（NEG-REMAP-A 实现 1 红 `R6l` / NEG-REMAP-B 接线 1 红「出现 2 次」）+ R1–R6 共 30 用例 + 接线锁 5 例，md5 `2f90c3ef` |

### ★ 工具名伪装的任务价值（`claudeCodeToolRemapper`）

Anthropic 在**第一方 Messages API**（原生 Claude OAuth）上用**工具名指纹**识别第三方
agent harness：真 Claude Code 用 `Bash` / `Read` 大驼峰，而 Codex / OpenCode / Cline
发来的历史普遍是 snake_case（`read_file` / `run_command` / `list_directory`）。被识别后
上游**拒绝服务，且错误伪装成 `400 out of extra usage`** —— 看着像计费问题，实为 SSE
流被拒。在 agent 客户端里就表现为**任务无声中断**，与本轮用户报的现象同源。

两种失败模式（参考实现原注释逐字）：

> 1. Specific blacklisted names (e.g. `mixture_of_agents`) are refused even in isolation.
> 2. A large enough SET of recognizable snake_case agent tool names is refused
>    collectively, even though each name passes on its own.

因此分两步、**不可合并**：`remapToolNamesInRequest` 只归一**固定清单**，`cloakThirdPartyToolNames`
把**任何**看起来不像真 Claude Code 工具的名字确定性改名（有 canonical 用 canonical，
否则 PascalCase），并记入 per-request 的 `_toolNameMap` 供回程还原。

### ★ `_toolNameMap` 的"不该上行"必须用**显式 delete** 表达（Go 侧无法照搬 `enumerable: false`）

参考实现把映射用

```ts
Object.defineProperty(transformed, "_toolNameMap", {
  value: toolNameMap, enumerable: false, configurable: true, writable: true,
});
```

挂在**同一个** body 对象上：`JSON.stringify` 自然忽略它，而 `chatCore` 仍能从同一对象读到。
Go 的 `json.Marshal` 没有 "non-enumerable" 概念 —— 要么删掉（读不到）、要么留着（会发上线，
哪怕值是 `{}`，**键名本身非法**，Anthropic 回 400 `Extra inputs are not permitted`）。

Go 侧的等价做法是**双键**：

| 键 | 作用 | 线序化可见性 |
|---|---|---|
| `_toolNameMap`（`toolNameMapKey`） | 对位参考实现的同名键，仅在函数内部短暂存在 | 出站前被 `detachToolNameMap` 摘除 |
| `__goToolNameMap`（`toolNameMapSideChannelKey`） | Go 专有的进程内旁路键 | 每次 `json.Marshal(params)` **之前** delete、之后回填，**永远不出现在 payload 里** |

这样既满足"同对象读写"的语义，又保证映射不上行。摘下来的映射由调用方
（`routing_dispatch.go` 的 `hopParams` / `providers_chat.go` 的 `params`）用
`takeToolNameMap` 读走，交给响应侧还原。

### ★ 响应侧还原必须**按客户端形态**各接一处（漏一处就漏一种客户端）

请求侧 cloak 只由**上游形状**决定（`claude` / `anthropic-compatible-*`），与客户端形态无关。
因此回程必须在**每种客户端形态**上都还原，否则该形态的客户端会收到自己从未声明过的工具名：

| 客户端形态 | 还原点 | 参考实现出处 |
|---|---|---|
| Claude 流式 | `anthropic.go:emitToolBlock`（`restoreClaudeToolName(acc.name, toolNameMap)`） | `utils/stream.ts:restoreClaudePassthroughToolUseName` |
| Claude 非流式 | `anthropic.go:openAIToAnthropicWithMap`（`restoreClaudeToolName(name, nameMap)`） | `responseTranslator.ts:740` |
| OpenAI 流式 | `proxy_stream.go`（`restoreOpenAIToolNames(normalized, toolNameMap)`） | `responseTranslator.ts:165` + `toolCallHelper.ts:131-157` |
| OpenAI 非流式 | `proxy_stream.go`（`restoreOpenAIToolNames(out, toolNameMap)`） | `responseTranslator.ts:173` + `toolCallHelper.ts:131-157` |

**只在 `emitToolBlock` 里改 `content_block.name`，绝不能改 `acc.name` 本身**：上方的
`filterToolInput(acc.name, ...)` 与 `hasToolCallShim(acc.name)` 都以**上游回显的别名**
（如 `Read`）为键 —— 那两张表按 Claude 权威名建索引，用别名查才对。只有**发给客户端**
的那个名字要还原。

### `fixToolUseOrdering` 的形态守卫（★ 本轮最重要的作用域修正）

参考实现里 `fixToolUseOrdering` 由 `prepareClaudeRequest` 调用，而后者只在
`targetFormat === FORMATS.CLAUDE` 时执行（`translator/index.ts:567`），且是
`translateRequest` 的**最后一步** —— 也就是说它见到的 messages **必然已是
Claude 形态**（`openaiToClaudeRequest` 已把 `tool_calls` 转成 `content[].tool_use`，
见 `translator/request/openai-to-claude.ts:660-675`，转换后不再保留 `tool_calls` 字段）。

我方出站咽喉拿到的是**客户端原始 body**，可能仍是 OpenAI 形态。曾有一版不加区分
直接接，结果：

- Pass 2 把 `content: null` 包成 `[{type:"text",text:null}]`；
- 本函数**不认识** `tool_calls`，那条 assistant 的工具调用信息在块里凭空消失；
- 下游 `stripTrailingAssistantOrphanToolUse` 靠 `tool_calls` 字段判定"尾部孤儿调用"，
  此时看不到该字段 → 漏判。

两条既有接入测试当场变红（`TestChatWithKey接入_trailing调用被清理` 与
`..._Anthropic走相邻性_OpenAI不走`）。修正办法是**把参考实现的隐含前置条件显式补齐**：

| 函数 | 职责 | 是否加守卫 |
|---|---|---|
| `fixToolUseOrdering(messages)` | 接入级包装：检测到**任何**消息带 `tool_calls` 字段即原样返回 | ✅ 有（形态守卫） |
| `fixToolUseOrderingClaudeShape(messages)` | 纯变换，与参考实现逐条对应 | ❌ 无（纯函数） |

> **为什么是"保守跳过"而不是"顺便转换"**：转换是 `openaiToClaudeRequest` 的职责
> （含 `CLAUDE_OAUTH_TOOL_PREFIX` 前缀、`tryParseJSON` 参数解析等本函数不该复刻的
> 细节）。越界去猜只会引入第二套转换语义。

> **`fixToolUseOrdering` 相对旧链的增益**：旧链（`fixToolAdjacency`）只会把
> tool_result 够不着的 tool_use 剥掉、再把孤儿 tool_result 清掉，于是整个工具往返
> 消失（模型再也看不到工具输出）。新链把它**归一成合法形状** —— 合并相邻同 role 回合、
> tool_result 提前，工具输出得以保留。这一差异由 `.negbak/probe_chain6.mjs` 的
> C1（六步链）vs C3（旧五步链）两栏对照实测锁定。

### ★ `modelTargetsClaude` 必须作为参数传入，不能写死（本轮第二次「作用域」教训）

`claudeHelper.ts:383-385`：

```ts
const modelTargetsClaude =
  !!provider && !!model && getModelTargetFormat(provider, model) === "claude";
const supportsRedactedThinking = !isKimiCoding && (supportsPromptCaching || modelTargetsClaude);
```

`supportsRedactedThinking` 决定 thinking 块走 **`redacted_thinking{data:<签名 blob>}`** 还是
**`thinking{thinking:<文本>}`**。我第一版实现把 `modelTargetsClaude` 写死为 `true`，理由是
"进入本函数的路径本身就是 anthropic 形态出站，语义上等价于 `targetFormat === "claude"`"。

**这个推理是错的**，而且错得很隐蔽：

- `targetFormat === "claude"` 说的是**出站协议形态**（用 Messages API 的 body 结构）；
- `modelTargetsClaude` 说的是**上游是不是真 Anthropic 端点**（能不能校验签名 blob）。

二者只在 `claude` / `anthropic-compatible-*` 上重合。对 `glmt` / `zai` 这类
"说 Claude 协议但不是 Anthropic"的中转，出站形态确实是 `claude`，但它们**无法校验签名
blob** —— 参考实现的注释点名了这种情形会 400
`Invalid signature in thinking block`，而这正是它引入 `supportsRedactedThinking` 的原因。

**抓出方式**：探针首版同样写死 `true`，于是 T11–T13 全部产出 `redacted_thinking`；
把 `modelTargetsClaude` 改为入参、对 `glmt` 传 `false` 后，这三栏立刻变成
`thinking{thinking:"(prior reasoning summary unavailable)"}` —— 差异肉眼可见。
据此把 Go 实现的签名改成接收 `modelTargetsClaude bool`，调用方传
`supportsPromptCachingForProvider(upstreamProvider)`（保守：只认 claude /
anthropic-compatible-*），并在接线锁里加了一条"**不得写死 true**"的断言
（NEG-THINK-A 实测抓出 1 红）。

> **教训**：照抄一个从别处 import 的判定值时，先问清它**语义上在判什么**。
> "当前上下文里恒为真"常常只是巧合 —— 参考实现写 `getModelTargetFormat(...)` 而不是
> 写 `true`，恰恰因为它需要区分"说 Claude 协议"和"是 Anthropic"。

### ★ 源码层接线锁必须断言「次数 == 1」（本轮第一次「弱断言」教训）

`toolCallShim` 的接线锁初版只断言两条：

1. `hasToolCallShim` / `applyToolCallShimToBuffer` 出现在 `emitToolBlock` 区间内；
2. `applyToolCallShimToBuffer` 全文件出现 1 次。

把调用**同时**放进 `emitToolBlock` 和 `processSSELine` 的分片累加路径后，第 2 条立刻变红
（`应恰好出现 1 次, 实际 2 次`），而如果只写"第一次出现的位置落在区间内"就会漏掉 ——
第一次出现的位置**没变**，错误在于**多了一处**。

第 1 条还额外锁了"shim 不得出现在分片累加路径上"：参考实现要求清洗发生在**拼装完成之后**，
若在分片路径上跑，会把还没拼完的半截 JSON 当完整 JSON 解析 → 全部退化成 `{}`。
这是一条**只有靠作用域断言才能守住**的语义（函数级测试永远看不到）。NEG-SHIM-B 实测 2 红。

### `fixToolUseOrdering` 的有意偏离（探针实测，非猜测）

| 行为 | 参考实现 | 我方 | 理由 |
|---|---|---|---|
| `messages` 里出现非对象条目（如 `null`） | Pass 2 读 `msg.role` 抛 `TypeError: Cannot read properties of null` | `continue` **跳过**（不崩溃、不造假回合） | 长驻服务里一个 panic 会打掉整个进程，比一次 400 严重得多。参考实现的上游已有 `Array.isArray(body.messages)` 与逐条 `msg.content` 的隐式前提，本网关不成立 |

> 同类的偏离还有 `markMessageCacheControl` 对非对象块（参考实现抛
> `Cannot set properties of null`）。两处都在代码注释里显式登记为"有意偏离"。

### `fixToolPairs` 逐字对照发现的 4 处实质差异（本轮修正）

原实现是我此前按"两步策略"自写的，与参考实现有四处语义差距。每一处都用 Node 实跑参考实现确认过，不是读代码推断：

| # | 参考实现 | 原实现 | 后果 |
|---|---|---|---|
| 1 | Pass 1 同时收集 Anthropic 形态 `user.content[].tool_result.tool_use_id`（`:721-730`） | 只认 `role=="tool"` 的 `tool_call_id` | **Anthropic 形态的 tool_result 完全不被识别**：配对完好的会被当孤儿删掉，真孤儿的反而原样上行 → 400 |
| 2 | `!isLastMessage(idx)` 例外：**最后一条 assistant 的 tool_calls 永不修剪**（`:735-737`） | 无此例外 | **agent 刚发起调用的瞬时状态被删** → 客户端表现为「没有任何错误、没有任何提示、任务直接中断」。这是用户报的故障的机制级原因之一 |
| 3 | Pass 2 同时修剪 `content` 数组里的 `tool_use` 块（`:751-760`） | 只修剪 `tool_calls` | Anthropic 形态 content 未处理 |
| 4 | 保留条件 `!tc.id \|\| toolResultIds.has(tc.id)` —— **无 id 的 tool_call 保留**（`:743`） | `if answered[id]`，空串查表 | 无 id 的工具调用被误删 |

另有一处细节：参考实现修剪后是把 `tool_calls` **置成空数组**（`:746`），不是 `delete`；Pass 4 才把"既无内容也无调用"的 assistant 整条剔除（`:805-814`）——顺序不能颠倒，否则会绕开第 2 条的例外。

**`schemaCoercion` 的关键差异（已核实，非推测）**：参考实现的 `sanitizeClaudeToolSchema` **只调 `stripInvalidSchemaConstructs`**，刻意**不组合** `stripUnsupportedRegexPatterns`。原注释：「`stripInvalidSchemaConstructs` now also coerces numeric-string constraints, so it is the single pass for the Claude path. We deliberately do NOT compose `coerceSchemaNumericFields`: it strips the valid `default` keyword (Fix #1782) which on the native / passthrough surface would silently alter tool schemas that were previously forwarded verbatim.」 我方接法与之逐字一致（只净化、不剥正则），接入级测试里专门锁了「lookaround 正则**不被**这条路径剥离」。

### 四个"已照抄但未接入"模块的真实原因（诚实登记）

照抄纪律要求接线，但**接线错误比不接线更坏** —— 接在拿不到正确入参的位置等于埋一处永不触发的死代码，还会让"测试绿"掩盖"线上不生效"。逐个说明为什么暂不接、以及接入的前置条件：

| 模块 | 参考实现接入点 | 我方为何暂不接 | 接入前置条件 |
|---|---|---|---|
| `defaultClaudeToolType` | `chatCore.ts:2586`：`targetFormat === FORMATS.CLAUDE` 时给**出站 Claude 形态**的工具补 `type` | 我方生产 Anthropic 路径（`anthropic.go:508` → `anthropicToOpenAI`）是**入站 Anthropic → 出站 OpenAI** 方向，构造的是 OpenAI 形态工具（`anthropic.go:86-105`）。而 `internal/translate` 包虽含 `OpenAIChatToClaudeRequest`（构造 Claude 形态），但**生产调用点为零**（该包文件头自述的 R2 审计 F4 结论）。在 translate 里接 = 死代码 | 当 `internal/translate` 接进生产链路（出站 Claude 形态真实存在）时，在 `OpenAIChatToClaudeRequest` 的 `out["tools"] = claudeTools` 处接 |
| `normalizeOpenAICompatibleTools` | `chatCore.ts:2382`：`provider?.startsWith("openai-compatible-")` 且**非** Responses 透传时归一工具 | 该条件依赖"供应商 id 前缀 `openai-compatible-`"这套参考实现的注册表命名法；我方 generic provider 是用户自配的任意 id（`providerIDRe = ^[a-z][a-z0-9_-]*$`），没有这个前缀约定。且此归一与已接入的 `sanitizeOpenAITools`（`proxy_cline.go:163`）**职责重叠**——后者已在出站咽喉处理工具定义合规 | 需要先定义"哪些 provider 算 openai 兼容形态"的判定口径（我方目前按 `providerConfig.APIType` 区分，语义与参考实现的前缀判定不等价） |
| `checkToolCallingRequiredButUnsupported` | `chatCore.ts:2740`：`unsupported` 来自 `getUnsupportedParams(provider, model)` 的**模型能力表** | 我方**没有模型能力表**（`getUnsupportedParams` 无对应实现）。该护栏的入参 `unsupported` 无处可得 —— 空数组传入会让函数恒返回"不拦截"，接了等于没接 | 需要先建立模型能力/不支持参数表。在此之前，该模块作为**已就绪的护栏实现**待命 |
| `isValidResponsesItemId` | `responsesInputSanitizer.ts` + `reasoningInputPolicy.ts`：回放 Responses `input[]` 前剥掉非法 `id` | 我方 Responses（zen）路径目前不做 `input[]` 回放式重放（`zen_responses_convert.go` 是单次转换）。该守卫的消费方尚不存在 | 当 zen 路径引入"会话历史回放"时，在剥离处接 |
| `adjustMaxTokens` | `translator/request/{claude,gemini,antigravity}-to-openai.ts` + `openai-to-claude.ts`：格式转换时把 `max_tokens` 调成上下文无关的安全值 | 参考实现的四个调用点**全部在 `internal/translate` 包对应位置**（该包生产调用点为零）；且我方 `buildUpstreamBody` 已有自己的 `defaultMaxTokens`（128000）逻辑，替换会改变线上行为 | 需要先统一"max_tokens 取值口径"（我方 128000 vs 参考 64000/32000）。在口径未对齐前接入会互相打架 |

> **这四项的价值不因未接入而作废**：它们是"照抄完成度"的诚实快照 —— 实现与规格都已固化（含逐条 Node 实测的行为锁），触发条件一旦出现即可接入，且不会有"重新理解参考实现"的返工。反过来，如果在没有正确入参的情况下硬接，就会得到一处骗过测试、线上永不生效的代码，那才是真正的技术债。

> **`paramSupport.ts` 的 Phase 2 差异（已声明，非遗漏）**：参考实现的 `stripUnsupportedParams` 末尾调用 `applyConfigFilters(provider, model, rec, snapshot)`，规则来自 DB 表 `ProviderParamFilter` / `ModelParamFilter`（操作台可配）。我方无该表，故只实现 Phase 1 硬编码规则。**Phase 1 是参考实现里真正被测试覆盖的部分**（`tests/unit/executors-strip-unsupported-params.test.ts` 203 行 + `azure-param-rules.test.ts` 96 行全部只测 Phase 1）。
>
> **`clampToModelMaxOutput` 差异（已声明）**：该分支需要 model catalog（`getProviderModel(provider, model).maxOutputTokens`）提供候选上限。我方无 catalog，该分支贡献不了候选值 → `applyMaxOutputClamp` 在只有 `clampToModelMaxOutput` 而没有 `maxOutputCap` 时直接返回。受影响的规则只有 `glm-4.6v`（`:110`）一条。
>
> **接入点差异（已声明）**：参考实现的接入咽喉是 `handlers/chatCore/upstreamBody.ts:227` 的 `sanitizeRequestForResolvedTarget`（它内部 `:80` 调 `stripUnsupportedParams`）。它的语义前提是：**此处已解析出具体的 target provider + target model**（`provider` 来自路由决策，`model = payloadRuleModel = bodyToSend.model`），因为规则表里 11 条规则有 6 条带 `provider` 限定。
>
> 我方有两个候选构造点，**选点由"能否提供真实 provider+model"决定**：
>
> | 构造点 | 能拿到的 model | 能拿到的 provider | 结论 |
> |---|---|---|---|
> | `proxy_cline.go:buildUpstreamBody` | `normalizeRequestModel(...)` → 非法 id 会**兜底成默认模型** `deepseek/deepseek-v4-flash` | 无（Cline 是固定上游） | ❌ 不接。model 已被改写，规则会静默失配（实测 `modelMatchesAnyStripRule("", normalizeRequestModel("claude-opus-4-20250514"))` = `false`） |
> | `providers_chat.go:chatWithKey` | `params["model"]`，**原始真实 id** | `p.name`，**真实 provider id** | ✅ 接此处 |
>
> 该选择与参考实现同构：`providers_chat.go:chatWithKey` 正是 generic provider（Google Gemini / B.AI / Hermes / OpenRouter / TokenRouter…）发出上游请求的唯一位置，等价于参考实现的 `upstreamBody.ts`（"translated body 已在手、target 已解析、即将 fetch"）。
>
> **`proxy_cline.go` 侧的接入已回退**：`buildUpstreamBody` 的 model 是归一后值，在那里做 model 级匹配等于"用被改写的名字去查表"，既不生效又掩盖问题。宁可明说不接，也不留一处永远不触发的死代码。

### 照抄时被测试抓出的真实缺陷（非猜测，均由用例定位）

| 缺陷 | 表现 | 定位方式 | 修正 |
|---|---|---|---|
| `collectTextualToolCalls` 白名单分支嵌套错误 | "解析得出但工具名不在白名单"时既不收集也不清空，畸形标记原样漏给 agent | `TestCollect_白名单外不收集但被判定畸形` 变红 + Node 实跑参考实现确认终态为 `content=""` | 把白名单判断改为 `else if`，落到 `containsMalformedTextualToolCall` 分支（对齐 `stream.ts:2521-2529`） |
| `fixToolPairs` 漏掉"最后一条 assistant 例外" | 历史被切片后，客户端刚发起、尚未收到结果的那次调用被整条删除 → 表现为「无错误、无提示、任务中断」 | 用 Node 实跑参考实现四场景（`.negbak/probe_fixtoolpairs.mjs`）与 Go 实现逐场景对比，场景 A 我方 output len=1 vs 参考 len=2 | 照抄 `!isLastMessage(idx)` 例外（`contextManager.ts:735-737`） |
| `fixToolPairs` 不识别 Anthropic 形态 `tool_result` | 孤儿 tool_result 原样上行 → 上游 400；同时配对完好的也可能被误删 | Node 实跑场景 B/D + Go 侧孤儿用例对比 | Pass 1 补收 `user.content[].tool_result.tool_use_id` |
| **接入作用域过宽**（自引入，非参考实现缺陷） | 把 Claude-only 的五步链无条件挂到所有 provider，`gemini` 路径里合法的「以 assistant(tool_calls) 结尾」历史被删；两条既有测试当场变红 | `TestProviderChatInjectsThoughtSignature` / `TestProviderChatReplaysOnRejectedSignature` 变红 | 用缩进回溯定位参考实现的外层包裹 `if (((provider === "claude" && ...) \|\| usesClaudeCodeProtocol) && ...)`（`executors/base.ts:955-960`），把接线收进 `if cfg.APIType == "anthropic"` |

> 第三条的教训：**照抄必须连作用域一起抄**。只抄函数体、把调用放宽到所有路径，比不抄更坏 —— 它会在参考实现从不敢碰的路径上改变行为。

> 这条缺陷的价值: 它证明"照抄 + 逐条对应用例"能抓到**光靠读代码看不出来**的分支顺序问题 —— 我读 `stream.ts:319-339` 时把 `return null` 理解成"放弃处理"，而实际语义是"交给下一个判定器处理"。

### 照抄过程中确认的「反直觉参考行为」（禁止"顺手修正"）

以下行为经 Node 实跑 OmniRoute 原逻辑确认，属参考实现的既有效果。照抄时若"看着像 bug 就改掉"，会造成偏离，故逐条登记：

| 行为 | 验证 | 说明 |
|---|---|---|
| `isValidToolCallHeaderPrefix("[Tool call: ter[foo]")` = `true` | Node 实测 | 名字段里的 `[` 检查只在**无 `]`** 分支（`:24-27`）执行；有 `]` 时走 `:29-30`，只查换行与 trim 空 |
| `parseTextualToolCallCandidate` 对紧凑畸形 `"[Tool call: n{k:v}]"` 返回 **partial** 而非 null | Node 实测 | `]` 后为空 → 头部前缀判真；但头部正则要求 `]\nArguments:`，失配 → partial。真正拦截发生在调用方 `stream.ts:2527-2528` |
| 参数被整体引号包裹时 `args` 类型是 **string** | Node 实测 | decoder 1（恒等）即成功，`JSON.parse` 返回字符串并立即 return，decoder 2 永不执行 |
| `properties` 内层的 `null` 条目保留为 `{}` | Node 实测 | 外层 `:51-52` 的 null 守卫管不到内层循环 `:56-66`；`typeof null === "object"` 但非 isPlainObject → 落 else → `{}` |
