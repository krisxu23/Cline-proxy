# opencode Zen 事实表

> 用途：**写 zen 相关代码前的唯一事实来源。** 本表把「官方明确写的」「实测验证过的」
> 「尚未确认的」严格分开 —— 遇到标 `[未知]` 的条目，**不要猜，先做实验**，实验完把结论回填本表。
>
> 上次核对：2026-09-17。官方文档会变，改动前请重新核对
> `https://opencode.ai/docs/zh-cn/zen`。

## 来源标注约定

| 标记 | 含义 | 可信度 |
|---|---|---|
| `[官方]` | opencode 官方文档明确写的 | 高，但可能过时 |
| `[实测]` | 本项目或前序 agent 实测验证，有记录 | 高，但可能只在当时的条件下成立 |
| `[推断]` | 由前两者推导，未直接验证 | 中，**落地前需验证** |
| `[未知]` | 尚未确认 | **禁止据此写代码** |

---

## 1. 端点矩阵 `[官方]`

Zen 是**多端点**网关，不同模型走**不同的 API 形态**。这不是可选项，是硬约束。

Base URL：`https://opencode.ai/zen/v1`（**已含 `/v1`**）

| API 形态 | 完整路径 | 覆盖模型 |
|---|---|---|
| Chat Completions | `https://opencode.ai/zen/v1/chat/completions` | DeepSeek V4 系列 / MiniMax M2.5-M3 / GLM 5.x / Kimi K2.5-K3 / **Big Pickle** / **MiMo-V2.5 Free** / **Ling 3.0 Flash Fin Free** / **Nemotron 3 Ultra Free** / **Nemotron 3.5 Lightning Free** |
| Responses | `https://opencode.ai/zen/v1/responses` | 全部 GPT 系列 / Grok 系列 / **Muse Spark 1.3 Contributor Free** |
| Anthropic Messages | `https://opencode.ai/zen/v1/messages` | 全部 Claude 系列 / Qwen 3.x 系列 / **Union Alpha Free** |
| Google 原生 | `https://opencode.ai/zen/v1/models/<model-id>` | 全部 Gemini 系列（路径**带模型 ID**，不是 `/chat/completions`） |

### ★ 拼接 URL 的坑

`/messages` 在官方文档里写的是**完整路径** `https://opencode.ai/zen/v1/messages`。
Base URL 已经含 `/v1`，所以：

```
正确： base + "/messages"        → https://opencode.ai/zen/v1/messages
错误： base + "/v1/messages"     → https://opencode.ai/zen/v1/v1/messages
```

Gemini 同理：`base + "/models/" + modelID`。

---

## 2. 免费模型清单 `[官方]`

官方定价表里标注 Free 的共 **7 个**，**跨 3 个不同端点**：

| 模型 ID | 端点 | 隐私条款 |
|---|---|---|
| `big-pickle` | `/chat/completions` | 免费期间**收集的数据可能用于改进模型** |
| `mimo-v2.5-free` | `/chat/completions` | 免费期间**收集的数据可能用于改进模型** |
| `ling-3.0-flash-fin-free` | `/chat/completions` | 免费期间**收集的数据可能用于改进模型** |
| `nemotron-3-ultra-free` | `/chat/completions` | 「**仅供试用 — 请勿提交个人或机密数据**」 |
| `nemotron-3.5-lightning-free` | `/chat/completions` | 「**仅供试用 — 请勿提交个人或机密数据**」 |
| `union-alpha` | **`/messages`** | 提供商遵循**零保留策略，不会用于训练** |
| `muse-spark-1.3-contributor-free` | **`/responses`** | 以**允许用你的提示词和补全内容训练未来 Meta 模型**为条件 |

### ★ 两条必须记住的推论

1. **`union-alpha` 只走 `/messages`**，`muse-spark-1.3-contributor-free` 只走 `/responses`。
   把它们发到 `/chat/completions` 必然失败 —— 这是**端点问题，不是模型问题**。
2. **免费是「限时」的**。官方对每个免费模型都写「限时免费提供，团队正在利用这段时间
   收集反馈」。→ 清单会变，**必须靠目录同步而不是硬编码**。

### 隐私风险（写代码时应考虑在面板标注）

- 三个模型明确说数据可能用于改进模型
- 两个 Nemotron 明确说「请勿提交个人或机密数据」
- Muse Spark Contributor 明确说用你的提示词和补全训练 Meta 模型
- **只有 `union-alpha` 承诺零保留**

---

## 3. 认证与计费 `[官方]`

- 需登录 `https://opencode.ai/auth`，**添加账单信息**，复制 API Key
- **按请求付费**，可充值；余额低于 $5 自动充值 $20
- 支持工作区级 / 成员级**月度使用限额**
- 支持**自带密钥**（用你自己的 OpenAI / Anthropic key，此时由原提供商计费）

| 端点 | 鉴权头 | 来源 |
|---|---|---|
| `/chat/completions` | `Authorization: Bearer <key>` | `[实测]` 本项目在用；2026-09-18 复测：伪造 key → `401 AuthError: Invalid API key.`，**说明该端点确实校验 Bearer** |
| `/responses` | `Authorization: Bearer <key>` | `[实测]` 本项目在用 |
| `/messages` | `Authorization: Bearer <key>`（与全体一致） | `[实测]` 2026-09-18：`/messages` **不暴露鉴权差异** —— 传合法 key / 伪造 key / **完全不传**，对同一 body 返回完全相同的响应。原因是它**先校验 model、后（或不）校验鉴权**：model 合法但上游不可用时直接回 `400 Model is unavailable`，根本走不到鉴权层。所以只能确认「zen 全站用 Bearer」，无法用该端点单独验证鉴权 |
| `/models/<id>` | `[实测]` **该路径不是 JSON API** | 2026-09-18：`GET https://opencode.ai/zen/v1/models/<id>` 返回的是官网 SPA 的 HTML（不是 JSON）。官方端点矩阵里的 `/models/<model-id>` 指的应是**另一个 base/路径**，不能按 `zen/v1/models/<id>` 拼 |

### 2026-09-18 端点探针的附带发现 `[实测]`
- `union-alpha` 当前**上游不可用**：`POST /zen/v1/messages {"model":"union-alpha"}` → `400 Model is unavailable`。
  加上 `opencode/` 前缀反而变成 `401 ModelError: Model opencode/union-alpha is not supported`
  → 该端点上模型名**不带** `opencode/` 前缀（前缀只在客户端配置层用）。
  → 即：union-alpha 现在无论怎么路由都拿不到结果，属上游侧状态，不是本项目的问题。
- 同一次探针里 `/chat/completions` + 免费模型 + 合法 key 仍回 `403 FreeTierError`（与本机直连 IP 相关，见第 4 节）。

---

## 4. 额度与风控 —— 官方说法 vs 实测

### 官方明确说的 `[官方]`
- 付费模型**按量计费**，有月度限额
- 免费模型**限时免费**，官方**没有**给出任何「额度」数字或计数口径

### 实测验证的 `[实测]`
| 现象 | 结论 | 证据 |
|---|---|---|
| 403 `FreeTierError`（"can only be used from within OpenCode"） | **按出口 IP 判定**，与请求头无关 | codex 2026-09-17 实测：同一 `mimo-v2.5-free` 走香港/大陆中转节点 200、走美/法节点与本机直连一律 403；带参考实现的完整 CLI 身份头同样被拒 |
| 429 | **换一个 IP 就能继续用**（用户实测） | 用户口述，未留存日志 |
| ~~CLI 身份头形态~~ **【2026-09-18 已证伪，见下】** | — | 原记载：「`d0a7bae` 直连实测：伪造结构合法 ID + UA 1.18.31 + cli 即 200，去头即 403」 |
| **CLI 身份头对 `FreeTierError` 没有影响** | 同一 IP 下，**带什么身份头结果都一样** | 2026-09-18 A/B/C 三向对照（同机直连、同一 body、同一 key，只换身份头）：<br>**A** `d0a7bae` 之前的旧形态（OmniRoute 式：`UA=opencode` + `client=desktop` + `project=global` + session=16hex + request=UUID）→ `403 FreeTierError`<br>**B** **当前代码形态**（官方式：`UA=opencode/1.18.31` + `client=cli` + `project=prj_…` + `session=ses_…` + `request=usr_…`）→ **同样 `403 FreeTierError`**<br>**C** 完全不带身份头 → **同样 `403 FreeTierError`**<br>→ 三者**逐字相同**。原记载「伪造 ID 即 200、去头即 403」**在 2026-09-18 无法复现**：形态 B 正是原记载里"能拿到 200"的那套，现在同样 403；而不带头也是 403（原记载说"去头即 403"，这条对上了）。<br>→ 最可能解释：原记载里那次 200 的**自变量是出口 IP**（当次请求走了别的出口），不是请求头；或门禁此后又收紧 |

> ★ 这条更正很重要：它意味着**不要再去调身份头了**。`FreeTierError` 自始至终
> 只看出口 IP（与本表第一行一致）。本项目 `internal/app/opencode_headers.go`
> 里那套 CLI 身份构造**既不能造成、也不能修好**这个 403 —— 保留它的唯一理由是
> 对齐官方客户端形态（可能影响 Cloudflare 层的限流），**不是**解决 FreeTier。
> 若将来又看到"改头之后好了"，先确认出口 IP 有没有变。
| **额度重置窗口** = **按日，边界 00:00 UTC（= 08:00 北京）** | 由两个独立采样反推：上游 429 回的 `Retry-After` 头①`18h53m35s`(13:06:25 采样) ②`17h47m25s`(14:12:36 采样)，两者相加**都精确落在次日 08:00:00/08:00:01** | 部署实例 `data/free-router.log` 2026-09-17 两行 `zen rate limited (429), retry 1/6 after ...` |
| **429 的重置时刻可直接读 `Retry-After` 头** | 不需要解析响应体 —— 上游主动给了精确时刻，且本项目已在读它 | 同上（`parseRetryAfter(resp.Header.Get("Retry-After"))`） |

> ★ 「日重置 / 00:00 UTC」这条直接决定了冷却时长：冷却**不能**短于 Retry-After，
> 否则会在额度恢复前反复打同一个已耗尽的出口。实现见
> `proxy_pool.go` 的 `zenQuotaRetryAfterCap`（此前被无条件截成 6h，2026-09-18 修正）。

### 尚未确认的 `[未知]` —— 禁止据此写代码

| 问题 | 状态 | 说明 |
|---|---|---|
| **额度粒度**：一个 IP 的额度是全局共享（所有模型共用）还是 per-model？ | `[未知]` **实验未能完成** | 2026-09-18 用户批准后尝试：钉住单一出口、交替打两个免费模型（每请求 `max_tokens:1`）。**前提不成立**：① 历史 910 次 zen 请求**从未出现 429**（详见下节），说明在我们真实用量下额度根本不是瓶颈；② 要判定粒度必须先让**某个出口的额度真的耗尽**，而候选出口当前要么连不上（HTTP 000）、要么直接 `403 FreeTierError`（IP 不被接受），拿不到"已耗尽"这个状态。**结论：暂不可测，且很可能长期不重要** |
| **多 key 是否降低风控率** | `[未知]` **被硬阻塞** | 当前配置里**只有 1 个 zen key**（`.zen-config.json` 只有 `key` 字段，无 `keys` 数组）。没有第二把 key 就构不成对照实验。**需用户提供 ≥2 把 zen key 后才能做** |

#### 支撑上表的实测统计（2026-09-18，来自 `data/requests-*.jsonl` 全量）
| 日期 | zen 状态分布 |
|---|---|
| 09-15 | 200 × 42，403 × 2 |
| 09-16 | 200 × 427，502 × 13，403 × 7，400 × 4 |
| 09-17 | 200 × 339，502 × 30，403 × 30，400 × 16 |
| **合计** | **200 × 808 / 910（89%）**；**429 × 0** |

- 失败全部是：`502`（出口线路/socks5 失败、context canceled）、`403`（**RegionError** "This model is not available in your country"）、`400`（请求体）。
- 日志里那些 `retry ... after 18h53m` 的 **429 全部来自 `provider/gemini`**（另一个上游），
  **不是 zen**。此前把两者混为一谈是误读。
- 单出口最高连打 **276 次 200 / 0 失败**（`sub-1-___Singapore`），`176.108.246.58:9881` 244/0
  → 若存在"按 IP 的额度"，其阈值远高于本次观测到的用量。

> 已从 `[未知]` 移除的条目（结论见上表）：额度重置窗口、429 是否含配额信息、
> `/messages` 与 `/models/<id>` 的鉴权方式。**前两条已可落地使用**，第三条
> 落地为「/models/<id> 不可按该路径调用」，故 Gemini 原生端点一行保持不做。

---

## 5. 本项目现状对照

> 2026-09-17 更新：**已补齐 `/messages` 端点**。下表为当前状态。

| 能力 | 位置 | 状态 |
|---|---|---|
| 端点选择 | `zen_endpoints.go`（`zenEndpointKind` / `zenEndpointFor` / `zenStaticEndpoint`） | ✅ 已建，覆盖 4 种形态 |
| `/chat/completions` 出站 | `zen_call.go`（`zenEndpoint.pathFor`） | ✅ 正常 |
| `/responses` 出站 | `zen_call.go` + `zen_responses_convert.go` | ✅ 正常 |
| `/messages` 出站 | `zen_call.go` + `zen_messages_convert.go` | ✅ **已补齐**（请求/响应双向转换 + Anthropic 鉴权） |
| `/models/<model-id>` 出站 | — | ❌ **仍缺失**（原因见下） |
| 端点自适应学习 | `zen_responses.go`（responses）+ `zen_endpoints.go`（messages） | ✅ 已扩到 chat → responses → messages 依次回退 |
| `internal/translate` 接线 | `zen_messages_convert.go` 是它的**第一个生产调用点** | ✅ 已接线 |
| 模型目录同步 | `zen_models.go` / `zen_models_cache.go` | ✅ 有 |
| 模型 ID 前缀解析（`opencode/`、`zen/`） | `zen.go:resolveZenModel` | ✅ 有 |
| `union-alpha` 路由 | `zenStaticEndpoint` → messages | ✅ **已支持** |

### `/models/<model-id>`（Gemini 原生）为什么没做

免费模型里**没有任何 Gemini**（见第 2 节清单），而 Google 原生形态的请求体与
OpenAI 形态差异很大（`contents` / `parts` 而非 `messages`），需要一整套新转换层。
在免费档用不到它的情况下，投入产出比不成立。**若将来要支持，先确认有免费 Gemini 可用。**

### 仍待验证的边界（`[未知]`，见第 4 节）

- `/messages` 的鉴权方式：现按 Anthropic 惯例发 `x-api-key` + `anthropic-version`
  （`zenEndpointKind.usesAnthropicAuth`）。**若实测发现 zen 也收 Bearer，改成同时发两种头即可。**
- `union-alpha` 是否**只能**走 `/messages`：官方文档端点矩阵如此列出，但未实测。
  若它其实也能走 chat，不影响（走 messages 同样能通）。

### 端点回退的顺序与理由

`zenEndpointFallbacks`：chat → responses → messages。只在首选为 chat 时触发
（首选非 chat 说明静态表或学习已定向，回退只会重复同一个失败）。
负向结论**只进程内记，不落盘** —— 与 `zen_responses.go` 同一纪律。

---

## 6. 其他可用资源 `[官方]`

- **模型元数据端点**：`https://opencode.ai/zen/v1/models`
  「你可以从以下地址获取可用模型及其元数据的完整列表」
- **配置中的模型 ID 格式**：`opencode/<model-id>`（例：`opencode/gpt-5.5`）
- **已弃用模型表**：官方列了各模型的弃用日期，可作目录同步的参考

---

## 7. 改 zen 相关代码的检查清单

1. **先查本表。** 表里有的不要猜，表里标 `[未知]` 的先做实验。
2. **新增端点支持时**，同步更新第 5 节「现状对照」。
3. **实验完把结论回填**第 4 节的 `[未知]` 表格，并把标记改成 `[实测]`（附日期）。
4. **改 CLI 身份头前**重读 `opencode_headers.go` 的注释 —— 那里记录了实测证据
   （`d0a7bae`：结构化 ID + `cli` 才 200，UUID 形态与 `desktop`/`global` 已失效）。
5. **不要照抄其他开源项目**的 zen 处理 —— 已核对 `Tlrenhb/opencode-free-gateway`：
   它用**随机 UUID** 做 session/request、`project="default"`，正是被实测判定为 403 的形态，
   且该项目 2026-08-20 后停更（早于 9/17 门禁收紧）。
