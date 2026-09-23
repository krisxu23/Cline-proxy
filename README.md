# Free-Router

把 Cline 账号池、opencode zen 免费模型、ClinePass 订阅与任意 OpenAI 兼容站点合流为一个本地网关：三协议对外、按 model 前缀直选上游；单二进制，Windows 桌面形态开箱即用。

## 目录

- [特性](#特性)
- [快速开始](#快速开始)
- [架构](#架构)
- [路由规则](#路由规则)
- [上游配置](#上游配置)
- [配置客户端](#配置客户端)
- [管理与观测](#管理与观测)
- [环境变量与数据目录](#环境变量与数据目录)
- [开发与构建](#开发与构建)
- [在国内无代理推送](#在国内无代理推送)
- [致谢](#致谢)

## 特性

- **四类上游、一个入口** —— Cline 账号池、opencode zen 免费模型、ClinePass 订阅池，以及自配的任意 OpenAI 兼容站点（Gemini / OpenRouter / TokenRouter / B.AI …）；`cline-pass/*`、`zen/*`、`provider:model` 三种前缀直选上游
- **三协议对外** —— OpenAI `/v1/chat/completions`、Anthropic `/v1/messages`、OpenAI Responses `/v1/responses`，同一份上游能力给任意客户端；`GET /v1/models` 实时聚合全部可用模型
- **opencode 伪装层** —— 出站 zen 请求与官方 opencode CLI 完全同形：官方式身份头 + 合法的 `prj_/ses_/usr_` ID，免费层强制 agent 形态（stream + 五件套工具），详见 [opencode 伪装层](#opencode-伪装层)
- **出口治理** —— 内嵌 sing-box，节点链接直贴即成本地出口；代理池轮询 + 冷却，连通检测参与选路，地区受限模型只走通过地区校验的出口
- **三层自愈 + 账号池自动化** —— 上游熔断 → key/账号冷却 → 模型级路由，`X-Proxy-Route` 头标注每次决策；OAuth / 手动 Token / 批量导入入池，429 到期自动恢复，请求失败自动换号
- **Gemini 协议兼容** —— 原生目录方言归一化、thought-signature 回填与自动重放、免费配额语义区分
- **上下文压缩** —— 移植 opencode 官方摘要算法（尾部预算 → 锚定摘要模板 → 重组会话），超长会话不撞上限
- **单二进制 + 桌面形态** —— 无控制台黑窗，托盘 + 独立管理窗口；也支持服务器模式与 Docker；token 统计与请求日志落盘

## 快速开始

### 桌面应用（Windows）

从 [Release](https://github.com/krisxu23/Free-Router/releases) 下载 `free-router-windows-amd64.exe`，双击即用：

- **无控制台黑窗口**：GUI 子系统构建，启动即进桌面形态
- **应用窗口**：独立应用窗口渲染 Web 后台（无地址栏/书签，观感即桌面应用）；仅首次运行自动弹出（标记 `data/.panel-opened`，删除该文件可恢复每次弹出），之后纯后台启动，需要时用托盘「打开管理界面」打开
- **系统托盘**：右键菜单含 4 项——「打开管理界面」「打开数据目录」「导出诊断包」（打包日志与运行状态到 `data/diag-<时间戳>.zip`）「退出」
- **重复双击**：端口被占时自动并入已在运行的实例
- **启动失败**：弹窗提示原因（如端口占用，可换端口）
- **完整性校验**：发布包未签名，Windows SmartScreen 可能拦截。下载后比对 Release 页的 `free-router-windows-amd64.exe.sha256`：

  ```powershell
  certutil -hashfile free-router-windows-amd64.exe SHA256
  ```

  输出与 `.sha256` 文件内容一致再运行。

命令行功能照常可用（终端直跑会回挂控制台）：

```powershell
.\free-router.exe -list                 # 查看账号池
.\free-router.exe -add-account          # OAuth 添加账号
.\free-router.exe -desktop -port 3457   # 显式桌面模式
```

带任意参数从终端启动则进入常规服务器模式，日志实时打印，Ctrl+C 停止。

### 源码编译

```bash
# Windows GUI 构建（无控制台窗口）
go build -tags "with_quic,with_grpc,with_utls" -ldflags "-s -w -H=windowsgui" -o free-router.exe ./cmd/free-router

# 控制台构建（调试用，或 Linux/macOS）
go build -tags "with_quic,with_grpc,with_utls" -o free-router.exe ./cmd/free-router

# 构建 + 启动 + 打开浏览器
go run -tags "with_quic,with_grpc,with_utls" ./cmd/free-router -start
```

> 构建标签 `with_quic,with_grpc,with_utls` **不可省略**：缺少时 reality/uTLS 与 QUIC（hysteria2/tuic）类节点会被 sing-box 判为无效出站并从出口池剔除，可用节点数会大幅减少。

```bash
./free-router.exe                      # 默认只监听 127.0.0.1，仅本机可访问
./free-router.exe -host 0.0.0.0        # 允许局域网访问（自行承担同网段风险）
./free-router.exe -port 3457           # 指定端口
./free-router.exe -cli                 # Windows: 留在前台控制台模式（调试用），不进托盘

# 局域网访问地址：http://<本机局域网IP>:3457/admin/
```

启动时会打印**管理后台地址（已内嵌访问令牌）**和令牌本身：

```
  管理后台(链接已含访问令牌, 直接打开即可):
    http://127.0.0.1:3457/admin/?token=<token>
  管理接口需要令牌, 可用 X-Admin-Token 头或 admin_token Cookie:
    token: <token>
  令牌落盘在 data/admin-token, 重启后不变。
```

### 管理后台访问控制

`/admin/api/*` 全部需要访问令牌。令牌在首次启动时自动生成并落盘到 `data/admin-token`，重启不变。三种传递方式：

> Windows 不实现数字权限位，令牌文件落盘后仍是默认权限。请勿把 `data/` 目录放在共享或可被其他用户读取的位置。

| 方式 | 用法 |
|---|---|
| 打开面板 | 直接用带 `?token=` 的地址打开，服务端校验后会种下 HttpOnly Cookie，之后正常使用 |
| 请求头 | `X-Admin-Token: <token>` 或 `Authorization: Bearer <token>` |
| Cookie | `admin_token=<token>` |

管理接口不返回 CORS 头，并拒绝带公网 `Origin` 的请求——否则用户浏览器里打开的任意网页都能 `fetch` 本机管理接口，读走全部账号 refreshToken 与上游 API Key。

> 需要局域网访问时请显式 `-host 0.0.0.0`，并确保 `data/admin-token` 不随镜像或日志外泄。

### Docker 部署

```bash
docker compose up -d
docker compose logs -f
docker compose down
```

数据持久化在 `./data/` 目录下，`override.md` 会自动从项目根目录挂载到容器内。

## 架构

```
客户端 (Cline / Claude Code / Cursor / CherryStudio ...)
   │  OpenAI / Anthropic / Responses 任一协议
   ▼
┌────────────────────────────────────────────────────────────┐
│  internal/app   HTTP 路由 + 协议转换 + 管理后台              │
│      │                                                     │
│      ▼  providers.Router (按 model 分流)                    │
│  ┌────────┬──────────┬───────────┬───────────────────────┐ │
│  │ cline  │   zen    │ clinepass │  generic provider      │ │
│  │ 账号池  │ 免费模型  │ 订阅 key 池│  自配 OpenAI 兼容站点  │ │
│  └────────┴──────────┴───────────┴───────────────────────┘ │
└────────────────────────────────────────────────────────────┘
   │            │              │                │
   ▼            ▼              ▼                ▼
api.cline.bot  opencode.ai/zen  api.cline.bot   你配置的任意上游
(账号轮询)      (免费模型,        (Bearer key)    (Gemini / OpenRouter
                可经节点出口)                     / TokenRouter / B.AI …)
```

## 路由规则

| model 形态 | 上游 | 鉴权 |
|---|---|---|
| `cline-pass/*` | ClinePass 订阅池 | 独立 key 池（后台配置） |
| `zen/*`（如 `zen/mimo-v2.5-free`）或裸 `*-free` | opencode zen | 匿名/自配 key |
| `provider:model`（如 `openrouter:z-ai/glm-5.3:free`、`gemini:gemini-3.8-flash`） | 通用 Provider | 该 provider 自己的 API key（后台配置） |
| 其余（如 `deepseek/deepseek-v4-flash`） | Cline 账号池 | OAuth 账号轮询 |

几条边界行为：

- `zen/` 前缀只接受免费 zen 模型，付费或未知模型明确 400 拒绝，不会误入 Cline 池；裸 `*-free` 名称继续兼容。
- `provider:` 前缀要求该 provider 已声明且模型命中它的**免费集合**，否则 400 拒绝——避免经网关误用付费模型。
- zen 连续失败时自动故障转移到 Cline 账号池（**前提是池内有可用账号**，否则继续尝试 zen）；Cline 账号 429/掉线时自动换号重试。

## 上游配置

### 1. Cline 账号池

管理后台 **账号管理** 页（OAuth 登录、手动 Token、批量导入都在此页的下半部分）：

- **OAuth 浏览器登录**：点击按钮弹出 WorkOS 登录窗口，完成后自动填入
- **手动输入 Token**：输入已有账号的 Access Token
- **批量文件导入**：上传包含账号数据的 JSON 文件

账号操作：**⚡ 测试**（真实探测，失败按上游返回时长冷却）、**↻ 重置**（仅清今日调用）、**✕ 删除**。

命中 429 `INFERENCE_CAP_ERROR` 时自动解析 "Try again in 17h 59m" 并冷却，到期自动恢复；请求失败自动换下一个可用账号。

### 2. opencode zen 免费模型

无需配置，启动即启用（匿名 key `public`）。

- **多端点**：官方源 + 3 个 CDN 镜像共 4 个端点（官方在前），重试与模型同步自动跨端点轮换
- **模型同步**：每 10 分钟同步官方模型列表；付费 zen 模型显式 400 拒绝
- **上游错误**：4xx 原样返回（如 `403 RegionError`），网络错误与 5xx 归一为 502
- **Responses 专用模型**：`muse-*` 等免费模型上游只提供 OpenAI Responses 接口（直连 chat 会被崩成 500）。网关自动改走上游 `/v1/responses`，请求与响应（含流式、tool_calls、usage）双向翻译回 chat 格式，对客户端透明。翻译时 `max_tokens` 补足上游下限（<16 被拒），推理强度默认 `low`（可用 `reasoning_effort` 覆盖）；不压低时推理容易吃光预算导致正文为空，建议 `max_tokens` ≥ 512
- **上下文压缩**：按 opencode 官方算法做摘要压缩（尾部预算 → 锚定摘要模板 → 重组会话）

#### opencode 伪装层

出站的每个 zen 请求都与官方 opencode CLI 同形，由两层构成：

- **身份头**：`User-Agent: opencode/<版本>`、`x-opencode-client: cli`，以及结构合法的 `prj_` / `ses_` / `usr_` ID。project 与 user 全进程稳定；session 由请求体指纹（模型 / system / 首条用户消息 / 工具集）确定性派生——同一会话恒得同一 ID，命中上游 prompt cache。客户端自带的 `x-opencode-*` 原样转发，非 CLI 形态的 UA 替换为官方形态。
- **免费层形态整形**：免费模型只认 agent 形态——强制 `stream: true` 并补齐 `bash / edit / glob / grep / read` 五件套（chat / responses / messages 三形状按端点生成）；客户端要非流式时，被强制出的 SSE 由网关汇总回 JSON，调用方无感。付费模型不整形。

实测边界（`docs/opencode-zen-facts.md`）：身份头**不能造成也修不好** `403 FreeTierError`（2026-09-18 A/B/C 三向对照），保留它是为了与官方客户端完全同形；免费层真正的硬门禁是请求体形态——缺 `stream` 或缺 agent 工具一律 403，补齐后同 IP 同 key 即 200（2026-09-22 双向实测）。

出口（后台 **🌐 出口代理与节点**）：

- http/https/socks5 代理池轮询（`round_robin` / `random` / `fill`）
- vmess / vless / trojan / ss / hy2 / tuic / hysteria / anytls / ssh / shadowtls / snell 节点链接直接粘贴——内嵌 sing-box 把每个节点转成本地出口，轮询与冷却和普通代理一致
- **连通检测参与选路**：不可达节点不再被轮询，网络错误让该出口短暂冷却，避免反复撞上同一个坏节点
- **按地区选择出口**：出口按实测国家归到 **美国 / 日本 / 台湾 / 香港 / 新加坡 / 欧洲 / 其他** 7 组。勾选一个或多个地区后，整个网关（zen / cline 池 / 通用 Provider / 订阅抓取）的出站只走所选地区的 IP，不勾选 = 不限制；检测结果落盘（`data/node-regions.json`），重启后仍可用，未检测或无法判定的出口（含手填代理）归入「其他」。所选地区无可用出口时临时回退全部出口并记日志告警
- **地区受限模型**：部分模型（如 `muse-spark-1.3-contributor-free`）只对特定出口地区开放，请求时只走通过地区校验的节点
- **订阅链接**：保存即抓取、默认**每 30 分钟**自动刷新（间隔可在面板调整，1 分钟–30 天，改完即生效），支持 sing-box JSON / Clash YAML / base64 三种格式，并入代理池统一轮询，缓存落盘重启即用；订阅列表默认收起，点击展开才显示完整地址与删除按钮

### 3. ClinePass 订阅池

在管理后台或通过 REST API 添加订阅 key：

```bash
# 管理接口都需要访问令牌, 见「管理后台访问控制」; 令牌可从 data/admin-token 读取
TOKEN=$(cat data/admin-token)

# 添加
curl -X POST http://127.0.0.1:3457/admin/api/clinepass/keys \
  -H "X-Admin-Token: $TOKEN" \
  -H 'Content-Type: application/json' -d '{"key":"sk-..."}'

# 查看（key 脱敏显示）
curl -H "X-Admin-Token: $TOKEN" http://127.0.0.1:3457/admin/api/clinepass/keys

# 删除（传脱敏后的 key）
curl -X POST http://127.0.0.1:3457/admin/api/clinepass/keys/delete \
  -H "X-Admin-Token: $TOKEN" \
  -H 'Content-Type: application/json' -d '{"key":"sk-1****abcd"}'

# 可用模型
curl -H "X-Admin-Token: $TOKEN" http://127.0.0.1:3457/admin/api/clinepass/models
```

多 key 自动轮询；key 命中 429 自动冷却 5 分钟，到期自动恢复。`reasoning` / `reasoning_content` 字段原样透传，CherryStudio 等客户端可正常显示思考过程。

### 4. 通用 Provider（任意 OpenAI 兼容上游）

把任意 OpenAI 兼容站点挂进网关，用 `provider:model` 直选。在后台 **🔌 通用 Provider（OpenAI 兼容上游）** 里填写，或直接改 `.zen-config.json` 的 `providers` 段。

免费模型由显式开关决定，按上游是否提供模型目录选择配置方式：

- 提供目录的上游（`catalog: true`；Google 上游自动强制开启）：目录拉取后在面板逐个勾选免费模型
- 不提供目录的上游（如 B.AI）：把模型名写进 `freeModels` 白名单
- 两者可叠加：白名单优先，上游已下架的模型自动剔除

> `catalog` 用于拉取模型清单与连通性，**不据目录价格自动判定免费**；目录带价格字段时会解析但不以此判定。

面板操作：**保存 Provider**、**🔍 连通测试**（对指定模型发一次最小请求）、**🔄 刷新目录**；目录启动时立即拉取、之后每 15 分钟刷新，目录为空时请求路径按 1 分钟退避重试。

配置示例：

```json
{
  "providers": {
    "openrouter": {
      "baseUrl": "https://openrouter.ai/api/v1",
      "apiKey": "sk-or-...",
      "catalog": true
    },
    "gemini": {
      "baseUrl": "https://generativelanguage.googleapis.com/v1beta/openai",
      "apiKey": "AIza..."
    },
    "bai": {
      "baseUrl": "https://api.b.ai/v1",
      "apiKey": "...",
      "freeModels": ["glm-5.3-flash", "deepseek-v4-flash", "mimo-v2.5"]
    }
  }
}
```

Google（Gemini）上游只需填 Base URL + API Key：目录地址与鉴权方言按 hostname 自动推导（OpenAI 兼容路径发 `Authorization: Bearer`，Google 原生目录路径发 `x-goog-api-key`）。历史工具调用的 thought-signature 缺失时自动回填，上游拒绝已缓存签名时自动重放一次。

## 配置客户端

**OpenAI 格式（/v1/chat/completions）：**
```
Base URL: http://<本机局域网IP>:3457/v1
API Key:  <在管理后台生成的 Key>
Model:    deepseek/deepseek-v4-flash              # Cline 账号池
          zen/mimo-v2.5-free                      # opencode zen 免费模型
          cline-pass/deepseek-v4-flash            # ClinePass 订阅
          openrouter:z-ai/glm-5.3:free            # 通用 Provider
```

**Anthropic 格式（/v1/messages）：**
```
Base URL: http://<本机局域网IP>:3457/v1
API Key:  <在管理后台生成的 Key>
Model:    同上，四种前缀任选
```

**Responses API（/v1/responses，Cursor 等）：**
```
Base URL: http://<本机局域网IP>:3457/v1
Model:    同上
```

`GET /v1/models` 实时聚合四类上游的全部可用模型。具体有哪些以该接口为准——上游清单变动频繁，不在文档里维护一份会过期的表。

## 管理与观测

- **API Key 鉴权**：后台 **🔑 API 密钥管理** 生成/删除，这是**调用方**访问 `/v1/*` 用的 key，与管理后台访问令牌（`data/admin-token`）是两回事。未配置任何 Key 时 `/v1/*` 允许无鉴权访问——因此默认只监听 127.0.0.1；若显式 `-host 0.0.0.0` 且未配置 Key，等于对局域网开放了一个会消耗你账号额度的开放代理，**请务必先生成 Key**
- **System Prompt 覆盖**：项目目录放 `override.md`，自动替换所有客户端的系统提示词
- **自定义请求头**：后台 **📨 请求头配置**，编辑转发给上游的头
- **路由决策头**：每个响应带 `X-Proxy-Route`，标注 `upstream` / `model` / `failover`
- **token 统计**：每请求 JSONL 落盘（`data/zen-stats.jsonl`），按账号/上游/模型聚合，后台实时展示
- **请求日志**：`data/requests.jsonl`；运行日志在 `data/free-router.log`；四类文件均按大小自动轮转（主日志 10 MiB）
- **出口治理**：后台 **🌐 出口代理与节点** —— 代理策略、代理列表、订阅、节点列表与连通检测、限流防御、上下文压缩都在此页
- **thinking 透传**：Anthropic 协议下上游 `reasoning_content` 自动转为 `thinking` 内容块（流式 + 非流式）
- **SSE 稳健性**：上游流无任何 choices 时自动补一个空 chunk 收尾，避免客户端报 "Provider returned no completion choices"
- **熔断与自愈**：上游级熔断（连续 5xx/408/429 触发，窗口过期半开探测；4xx 不计入）→ key/账号级冷却（命中 429 独立冷却，成功清零）→ 模型级路由（付费/未知模型明确拒绝）
- **多平台 CI/CD**：GitHub Actions 自动构建多平台二进制；Release 按语义版本递增，附 `sha256` 校验值

### 健康检查 `GET /health`

无需鉴权（供探针/编排使用）：

```bash
curl -s http://127.0.0.1:3457/health
```

| 字段 | 含义 |
|---|---|
| `status` | `ok` / `degraded`。配置了订阅、已探测、且可达出口为 0 时报 `degraded`；未配置订阅或尚未探测完报 `ok` |
| `version` | 构建时注入的版本号 |
| `activeAccounts` | 账号池中 `active` 状态的账号数 |
| `nodePool` | 当前出口隧道数 |
| `exitReachable` / `exitProbed` | 最近一次连通检测中可达 / 已探测的出口数 |
| `subNodes` | 订阅展开后的节点总数 |
| `lastSubFetch` | 订阅缓存最近一次成功写入时间（Unix 毫秒） |
| `serverRegistered` | HTTP server 是否已注册 |
| `logBytes` | 主日志当前字节数 |
| `dropped` | 请求日志因缓冲满被丢弃的条数 |

## 环境变量与数据目录

| 变量 | 作用 |
|---|---|
| `FREE_ROUTER_ALLOW_PRIVATE_UPSTREAM=1` | 允许把**私网/回环地址**配为上游（本机或内网跑 Ollama / LM Studio / vLLM 等）。默认拦截以防 SSRF；链路本地与云元数据地址（`169.254.169.254` 等）即使设了该变量也仍然拦截 |
| `FREE_ROUTER_SKIP_NODEBOX=1` | 测试专用：不实例化真实 sing-box（避开其内部竞态） |

运行时数据在 `data/`（首次使用时生成）：

| 文件 | 内容 |
|---|---|
| `.cline-accounts.json` | Cline 账号池 |
| `.zen-config.json` | zen 与通用 Provider 配置 |
| `.clinepass-keys.json` | ClinePass 订阅 key |
| `subs_cache.json` | 订阅解析缓存 |
| `requests.jsonl` | 请求日志 |
| `zen-stats.jsonl` | token 统计 |
| `free-router.log` | 运行日志 |
| `override.md` | 可选的系统提示词覆盖 |

## 开发与构建

```bash
go build -tags "with_quic,with_grpc,with_utls" ./...   # 编译（标签见上）
go vet -tags "with_quic,with_grpc,with_utls" ./...     # 静态检查
go test -tags "with_quic,with_grpc,with_utls" ./...    # 单元测试
```

覆盖协议转换、provider 目录与免费判定、Gemini 签名与配额、路由与冷却逻辑；不带构建标签时涉及节点出站的用例会失败（缺协议栈，属预期）。

## 致谢

- [diegosouzapw/OmniRoute](https://github.com/diegosouzapw/OmniRoute)（MIT）—— 本项目的路由/日志/错误规则/协议翻译/SSE 数据处理多处机制以其为参照并做了 Go 侧移植；MIT 版权声明见仓库根目录 `NOTICE`

出口选路、节点池与订阅处理的设计参考了以下优秀项目（思路借鉴，代码均为原创实现）：

- [jasonwong1991/easy_proxies](https://github.com/jasonwong1991/easy_proxies) —— sing-box 节点池管理（健康检查/黑名单/GeoIP 路由/订阅热重载）
- [Resinat/Resin](https://github.com/Resinat/Resin) —— 代理池网关的粘性会话（Sticky Session）
- [MetaCubeX/mihomo](https://github.com/MetaCubeX/mihomo) —— url-test 选路 + tolerance 防抖
- [nadoo/glider](https://github.com/nadoo/glider) —— 多策略转发与健康检查
- [sub-store-org/Sub-Store](https://github.com/sub-store-org/Sub-Store) —— 订阅处理管线

- [YuJunZhiXue/Cline-proxy](https://github.com/YuJunZhiXue/Cline-proxy) — 项目基座
- [defyma/cline-proxy](https://github.com/defyma/cline-proxy) — SSE 空 choices 兜底思路
- [hayou2002/clinepass-proxy](https://github.com/hayou2002/clinepass-proxy) — ClinePass key 池轮询/冷却与 reasoning 透传
- [okhsunrog/claude-proxy-rs](https://github.com/okhsunrog/claude-proxy-rs) — 跨账号故障转移思路
- [www222fff/free-router](https://github.com/www222fff/free-router) — 通用 Provider 的免费模型判定与 Gemini 兼容思路
- [LINUX DO](https://linux.do) 社区
