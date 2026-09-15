# LEARNINGS.md — 项目级经验沉淀

## [LRN-20260915-HB] 心跳/合成帧绝不可注入上游输入字节流

- **日期**: 2026-09-15
- **优先级**: HIGH
- **领域**: stream / SSE 代理
- **来源提交**: 4dcd86e（v0.0.69，CI 34987749935 success）

### 背景
`bai:qwen3.8-flash` 反复报 `Bad control character in string literal` / `JSON parsing failed`。
网关日志取证（22:06–22:10，11 条"丢弃无法解析的上游 data 行"）发现被丢弃的行是
`{"id":"chatdata: {...` 这种**胶水帧**——旧 `heartbeatReader` 把心跳帧直接写进上游
输入字节流，当大 JSON 帧（C2PA `_manifest`，数十 KB）被 TCP 分片且分片末尾恰好是
`\n` 时，边界判断误判为 true，心跳插在帧中间，帧被截断成两半后无法重组。

### 教训
1. **合成帧（心跳、keepalive）只能注入客户端输出侧**，永远不要注入上游输入字节流。
   参照 OmniRoute `open-sse/utils/earlyStreamKeepalive.ts`：心跳直达 client，不碰 upstream 流。
2. 输出侧注入还必须**只在 SSE 事件边界**（`\n\n` 之后）插入，需跨 write 检测
   `\n\n`（保留 `prev` 字节），否则一样会截自己的帧。
3. Go 实验证明 `controlSanitizingReader` 不吃 `\n\n`——排查同类问题时先做最小
   复现实验，不要先怀疑清洗器。
4. 归因要分案：18:58 那单确实是上游脏数据；22:06–22:10 这单是我们自己的代码。
   同样的报错文案，根因可能完全不同，看日志里被丢弃行的**形状**（胶水 `data:` vs
   裸控制字符）即可区分。
5. 非流式路径曾是清洗盲区：`json.NewDecoder` 直解不过 `sanitizeJSONControlChars`，
   已在 `handleNonStreamResponseWithUsage` 补上（ReadAll + 失败重试清洗）。

### 回归防线
`TestSSEHeartbeatNeverCorruptsRealFrames`：帧半截期间静默 120ms，断言心跳不得
插入两半之间。改流式输出路径时必须保留此测试。
