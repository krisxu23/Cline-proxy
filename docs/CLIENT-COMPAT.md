# 客户端兼容矩阵

网关的客户端（Cline / OpenCode / Claude Code / Codex 等）各有怪癖。这里记录
已知的兼容性约定，以及网关侧对应的保障措施。每一条约定都有对应的代码与测试
锁定 —— 修改相关代码时必须同步更新本表。

| 客户端怪癖 | 网关保障 | 锁定测试 |
|---|---|---|
| OpenAI 协议客户端把 `finish_reason:"length"` 当作"被截断"，Claude Code 会直接中止一次已交付完的回答 | muse-spark 家族的假截断归一：完成量 < 预算×0.9 时 length→stop（仅 muse，非流式与流式都生效） | `TestNormalizeMuseSparkFinish`、`TestResponsesToChatBodyMuseFakeLength` |
| muse-spark 会把小预算全部烧在隐藏推理上，正文恒空（客户端以为模型坏了） | Responses 翻译时预算地板 512（仅当调用方给了预算且 <512） | `TestChatBodyToResponsesBody_Basic` |
| 读超时普遍 30~120s，上游排队/推理期的长时间静默会被当成挂死 | 流式心跳：上游静默 >15s（可配 `streamHeartbeatSecs`，0=关）注入合法空 delta 帧；只在行边界注入 | `TestHeartbeatReaderInjectsOnSilence`、`TestHeartbeatReaderNoInjectMidLine` |
| "Stream ended without finish_reason" / 挂等 `[DONE]` | 上游断流时合成 finish chunk + `[DONE]`（含未发 finish 的兜底 chunk） | `handleStreamResponseWithUsage`（既有行为） |
| 上游"200 但无内容"会被客户端当成功 | 候选链在首字节前校验 200 是否真带内容/完整 tool_calls，不合格换下一站 | `chatBodyHasContent` / `chainBodyOnlyBrokenToolCalls`（routing_dispatch.go） |
| Responses 专用模型（muse 家族）走 chat/completions 上游直接崩 500 | 静态规则 + 自适应学习：自动改走 /responses 并双向翻译 | `TestZenStaticResponsesOnlyRule` 等 |
| max_tokens < 16 会被 Responses 端点 400 | 翻译时钳到 16 | `TestChatBodyToResponsesBody_MaxTokensClamp` |
| 压缩/截断会砍断 tool 配对 → 上游 400 | fixToolPairs 三步修复（孤儿 tool 结果丢弃 / 未回复调用修剪 / 纯调用壳丢弃） | `TestFixToolPairs*` |
| Codex 等客户端对响应头阶段有硬超时 | transport 层 TLS 握手 10s / 响应头 120s 分层超时，卡死主动断开换站 | `buildZenTransport`（proxy_pool.go） |
| 上游地区封锁（403 RegionError）被普通鉴权失败掩盖 | 声明式错误规则表：geoBlocked 归类 + 24h 冷却；CF 1010/质询页豁免 | `TestMatchErrorRulesGeoBlock` |

## 新增客户端支持时的检查单

1. 该客户端对 `finish_reason` / 空心跳 / `[DONE]` 的容忍度？
2. 它的默认读超时是多少？心跳间隔是否需要调整？
3. 它发送的请求形状是否被出站体校验覆盖？
4. 在本表添加一行 + 对应锁定测试。
