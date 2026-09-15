
## 致谢

- [diegosouzapw/OmniRoute](https://github.com/diegosouzapw/OmniRoute)（MIT）—— 本项目的路由/日志/错误规则/协议翻译/SSE 数据处理多处机制以其为参照并做了 Go 侧移植；MIT 版权声明见仓库根目录 `NOTICE`


出口选路、节点池与订阅处理的设计参考了以下优秀项目（思路借鉴，代码均为原创实现）：

- [jasonwong1991/easy_proxies](https://github.com/jasonwong1991/easy_proxies) —— sing-box 节点池管理（健康检查/黑名单/GeoIP 路由/订阅热重载）
- [Resinat/Resin](https://github.com/Resinat/Resin) —— 代理池网关的粘性会话（Sticky Session）
- [MetaCubeX/mihomo](https://github.com/MetaCubeX/mihomo) —— url-test 选路 + tolerance 防抖
- [nadoo/glider](https://github.com/nadoo/glider) —— 多策略转发与健康检查
- [sub-store-org/Sub-Store](https://github.com/sub-store-org/Sub-Store) —— 订阅处理管线
自动分流到四类上游，对外同时提供 OpenAI、Anthropic、OpenAI Responses 三种协议接口，内置中文管理后台。

## 目录

- [特性](#特性)
- [快速开始](#快速开始)
- [架构](#架构)
- [路由规则](#路由规则)
- [上游配置](#上游配置)
- [配置客户端](#配置客户端)
- [管理与观测](#管理与观测)
- [项目结构](#项目结构)
- [开发与构建](#开发与构建)
- [致谢](#致谢)

## 特性

- **四类上游，一个入口** —— Cline 账号池、opencode zen 免费模型、ClinePass 订阅池，以及你自己挂的任意 OpenAI 兼容站点（Gemini、OpenRouter、TokenRouter、B.AI…），按 model 自动分流
- **三种协议对外** —— OpenAI `/v1/chat/completions`、Anthropic `/v1/messages`、OpenAI Responses `/v1/responses`，同一份上游能力给任意客户端
- **前缀直选 + 模型聚合** —— `cline-pass/*`、`zen/*`、`provider:model` 三种写法精确指定上游；`GET /v1/models` 实时聚合四类上游的全部可用模型
- **出口治理** —— 内嵌 sing-box 把节点链接直接转成本地出口，代理池轮询 + 冷却；**连通检测结果参与选路**（判定不可达的节点不再被轮询）；**地区受限模型只在通过地区校验的节点上路由**
- **三层自愈** —— 上游熔断 → key/账号级冷却 → 模型级路由；每个响应带 `X-Proxy-Route` 头标注本次决策，排查路由一目了然
- **账号池自动化** —— OAuth 浏览器登录 / 手动 Token / 批量导入；命中 429 自动解析冷却时长并到期恢复，请求失败自动换下一个可用账号
- **Gemini 协议兼容** —— 原生目录方言归一化、thought-signature 回填与自动重放、免费配额语义区分（"没有免费层"与"当日额度用尽"走不同处置）
- **上下文压缩** —— 移植 opencode 官方摘要算法（尾部预算 → 锚定摘要模板 → 重组会话），超长会话不撞上限
- **单二进制 + 桌面形态** —— GUI 子系统构建无控制台窗口，托盘图标 + 独立管理窗口；也支持服务器模式与 Docker；token 统计与请求日志落盘

## 快速开始

### 桌面应用（Windows）

从 [Release](https://github.com/krisxu23/Cline-proxy/releases) 下载 `cline-proxy-windows-amd64.exe`，双击即用：

- **无控制台黑窗口**：GUI 子系统构建，启动即进桌面形态
- **管理窗口**：自动弹出独立应用窗口渲染 Web 后台（无地址栏、独立任务栏图标）
- **系统托盘**：右键菜单含 4 项——「打开管理界面」「打开数据目录」（资源管理器打开 `data/`）「导出诊断包」（打包日志与运行状态到 `data/diag-<时间戳>.zip`）「退出」
- **重复双击**：端口被占时自动并入已在运行的实例，直接弹出管理窗口
- **启动失败**：弹窗提示原因（如端口占用，可换端口）
- **完整性校验**：发布包未签名，Windows SmartScreen 可能拦截。下载后比对 Release 页的 `cline-proxy-windows-amd64.exe.sha256`：

  ```powershell
  certutil -hashfile cline-proxy-windows-amd64.exe SHA256
  ```

  输出与 `.sha256` 文件内容一致再运行。

命令行功能照常可用（终端直跑会回挂控制台）：

```powershell
.\cline-proxy.exe -list                 # 查看账号池
.\cline-proxy.exe -add-account          # OAuth 添加账号
.\cline-proxy.exe -desktop -port 3457   # 显式桌面模式
```

带任意参数从终端启动则进入常规服务器模式，日志实时打印，Ctrl+C 停止。

### 源码编译

```bash
# Windows GUI 构建（无控制台窗口）
go build -tags "with_quic,with_grpc,with_utls" -ldflags "-s -w -H=windowsgui" -o cline-proxy.exe .

# 控制台构建（调试用，或 Linux/macOS）
go build -tags "with_quic,with_grpc,with_utls" -o cline-proxy.exe .

# 构建 + 启动 + 打开浏览器
go run -tags "with_quic,with_grpc,with_utls" . -start
```

> 构建标签 `with_quic,with_grpc,with_utls` **不可省略**：缺少时 reality/uTLS 与 QUIC（hysteria2/tuic）类节点会被 sing-box 判为无效出站并从出口池剔除，可用节点数会大幅减少。

```bash
./cline-proxy.exe                      # 默认只监听 127.0.0.1，仅本机可访问
./cline-proxy.exe -host 0.0.0.0        # 允许局域网访问（自行承担同网段风险）
./cline-proxy.exe -port 3457           # 指定端口
./cline-proxy.exe -cli                 # Windows: 留在前台控制台模式（调试用），不进托盘

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

- `internal/protocol/` — 纯函数协议层：SSE 解析与归一化、Anthropic ↔ OpenAI、Responses ↔ Chat。无 I/O、无状态，单元测试覆盖。
- `internal/providers/` — 上游 Provider 抽象 + 路由注册表 + ClinePass key 池。cline / zen 两个适配器留在 `internal/app/adapters.go`，复用账号池与限流状态机。
- 通用 Provider 一族（`internal/app/providers_*.go`）与 `internal/providers/` 是两回事：前者是本项目新增的"任意 OpenAI 兼容上游"，后者是既有的 ClinePass 包。

## 路由规则

| model 形态 | 上游 | 鉴权 |
|---|---|---|
| `cline-pass/*` | ClinePass 订阅池 | 独立 key 池（后台配置） |
| `zen/*`（如 `zen/mimo-v2.5-free`）或裸 `*-free` | opencode zen | 匿名/自配 key |
| `provider:model`（如 `openrouter:z-ai/glm-5.3:free`、`gemini:gemini-3.8-flash`） | 通用 Provider | 该 provider 自己的 API key（后台配置） |
| 其余（如 `deepseek/deepseek-v4-flash`） | Cline 账号池 | OAuth 账号轮询 |

几条边界行为：

- `zen/` 前缀只接受免费 zen 模型，付费或未知模型明确 400 拒绝，不会误入 Cline 池；裸 `*-free` 名称继续兼容。
- `provider:` 前缀要求该 provider 已在配置中声明，且模型命中它的**免费集合**，否则 400 拒绝——避免经网关误用付费模型。
- zen 连续失败时自动故障转移到 Cline 账号池，**前提是池内存在可用账号**；否则保持 zen 继续尝试。
- Cline 账号 429/掉线时自动换号重试（受池内可用账号数限制）。

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

- **多端点**：官方源 + 3 个 CDN 镜像共 4 个 API 端点（官方在前），重试与模型同步自动跨端点轮换
- **模型同步**：每 10 分钟自动同步官方模型列表；付费 zen 模型显式 400 拒绝
- **上游错误**：4xx 状态码原样返回（例如 `403 RegionError` 对客户端可见），网络错误与 5xx 归一为 502
- **Responses 专用模型**：`muse-*` 家族的免费模型（`muse-spark-1.2/1.3-contributor-free` 等）在上游只提供 OpenAI Responses 接口——直接请求 `chat/completions` 会被上游后端崩成 500。网关对这类模型自动改走上游 `/v1/responses` 端点，请求体与响应（含流式）双向翻译回 chat 格式，对客户端完全透明；tool_calls 工具调用链路与 usage 统计不受影响。翻译时 `max_tokens` 会补足上游下限（<16 会被拒），推理强度默认降到 `low`（客户端可用 `reasoning_effort` 覆盖为 minimal/low/medium/high/xhigh）——这是推理型模型，effort 不压低时容易把预算全花在思考上导致正文为空，建议 `max_tokens` 给到 512 以上
- **上下文压缩**：按 opencode 官方算法做摘要压缩（尾部预算 → 锚定摘要模板 → 重组会话）

出口（后台 **🌐 出口代理与节点**）：

- 支持 http/https/socks5 代理池轮询（`round_robin` / `random` / `fill`）
- 支持 vmess / vless / trojan / ss / hy2 / tuic / hysteria / anytls / ssh / shadowtls / snell 节点链接直接粘贴——内嵌 sing-box 把每个节点转成本地出口，轮询与冷却机制与普通代理一致
- **连通检测参与选路**：检测判定为不可达的节点不再被轮询，网络错误会让该出口短暂冷却，避免重试反复撞上同一个坏节点
- **按地区选择出口**：后台不再逐个罗列节点，而是把出口按实测出口国家归到 **美国 / 日本 / 台湾 / 香港 / 新加坡 / 欧洲 / 其他地区** 7 组，每组显示「可用 / 共」数量并带勾选框。勾选一个或多个地区后，**整个网关（zen / cline 池 / 通用 Provider / 订阅抓取）的出站只走所选地区的 IP**；不勾选 = 不限制。地区来自连通检测实测的出口国家，检测结果会落盘（`data/node-regions.json`），重启后仍可用；未检测或无法判定国家的出口（含手填代理）归入「其他地区」。兜底：所选地区一个出口都没有时不会让网关失去出口，而是临时回退全部出口并记日志告警
- **地区受限模型**：部分模型（如 `muse-spark-1.3-contributor-free`）只对特定出口地区开放。这类模型会单独校验每个节点，请求该模型时只走通过校验的节点
- **订阅链接**：后台填订阅地址，保存即抓取、默认**每 30 分钟**自动刷新（间隔可在面板调整，范围 1 分钟–30 天，改完即生效无需重启），支持 sing-box JSON / Clash YAML / base64 节点列表三种格式，节点并入代理池统一轮询，缓存落盘重启即用；订阅列表默认收起为「主机名 + 抓取状态 + 节点数」，点击展开才显示完整地址与删除按钮

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

- 提供目录的上游（`catalog: true`；Google 上游自动强制开启）：目录拉取后在面板「🔌 通用 Provider」页逐个勾选免费模型；
- 不提供目录的上游（如 B.AI）：把模型名写进 `freeModels` 白名单；
- 两者可叠加（`catalog: true` + `freeModels`）：白名单优先，上游已下架的模型自动剔除。

> `catalog` 用于拉取模型清单与连通性，不据目录价格自动判定免费；上游目录若带价格字段，网关会解析但不以此判定免费。

面板上的操作：**保存 Provider**、**🔍 连通测试**（对指定模型发一次最小请求）、**🔄 刷新目录**；列表里显示每个 provider 的 `目录 N · 免费 N · 可聊 N` 与最近一次错误。目录在启动时立即拉取、之后每 15 分钟刷新一次；目录为空时请求路径会按 1 分钟退避自行重试。

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

Google（Gemini）上游只需填 Base URL + API Key：目录地址与鉴权方言由网关按 hostname 自动推导——OpenAI 兼容路径发 `Authorization: Bearer`，Google 原生目录路径（`/v1beta/models`）发 `x-goog-api-key`。历史工具调用的 thought-signature 缺失时自动回填跳过哨兵，上游拒绝已缓存签名时自动用哨兵重放一次。

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

`GET /v1/models` 实时聚合四类上游的全部可用模型：Cline 模型、zen 免费模型、ClinePass 订阅模型，以及通用 Provider 的免费模型（id 形如 `provider:model`）。具体有哪些以该接口为准——上游的模型清单变动频繁，没必要在文档里维护一份会过期的表。

## 管理与观测

- **API Key 鉴权**：后台 **🔑 API 密钥管理** 生成/删除。这是**调用方**访问 `/v1/*` 用的 key，与管理后台的访问令牌（`data/admin-token`）是两回事。未配置任何 Key 时 `/v1/*` 允许无鉴权访问 —— 因此默认只监听 127.0.0.1；若显式 `-host 0.0.0.0` 且未配置 Key，等于对局域网开放了一个会消耗你账号额度的开放代理，请务必先生成 Key。
- **System Prompt 覆盖**：项目目录放 `override.md`，自动替换所有客户端的系统提示词
- **自定义请求头**：后台 **📨 请求头配置（模拟 Cline CLI 发出）**，编辑转发给上游的头
- **路由决策头**：每个响应带 `X-Proxy-Route`，标注 `upstream` / `model` / `failover`，排查路由一目了然
- **token 统计**：每请求 JSONL 落盘（`data/zen-stats.jsonl`），按账号/上游/模型聚合，后台实时展示
- **请求日志**：`data/requests.jsonl` 记录每次请求；运行日志写在 `data/cline-proxy.log`（追加模式，桌面形态同样落盘）
- **日志自轮转**：`cline-proxy.log` / `cline-proxy-stream.log` / `zen-stats.jsonl` / `requests.jsonl` 按大小自动轮转（主日志 10 MiB，超限截断只保留最近内容）。
- **出口治理**：后台 **🌐 出口代理与节点** —— 代理策略、代理列表、订阅、节点列表与连通检测、限流防御、上下文压缩都在此页
- **thinking 透传**：Anthropic 协议下上游 `reasoning_content` 自动转为 `thinking` 内容块（流式 + 非流式）
- **SSE 稳健性**：上游流无任何 choices 时自动补一个空 chunk 收尾，避免客户端报 "Provider returned no completion choices"
- **熔断与自愈**：上游级熔断（连续 5xx/408/429 触发，窗口过期后半开探测，探测失败立即重跳闸；4xx 客户端错误不计入）→ key/账号级冷却（ClinePass key 与 Cline 账号命中 429 独立冷却，成功自动清零）→ 模型级路由（前缀显式分流，付费/未知模型明确拒绝）
- **多平台 CI/CD**：GitHub Actions 自动构建多平台二进制；Release 按语义版本递增，并附带 `sha256` 校验值

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

## 项目结构

```
├── main.go                    入口与 CLI 参数
├── internal/
│   ├── app/
│   │   ├── proxy.go           HTTP 路由、协议转换、账号池调用与故障转移
│   │   ├── adapters.go        cline/zen Provider 适配器 + 网关组装
│   │   ├── clinepass.go       ClinePass 三协议 handler + admin API
│   │   ├── zen.go             zen 上游、三态路由、限流状态机
│   │   ├── proxy_pool.go      出口代理选择、冷却、按模型选路
│   │   ├── nodes.go           vmess/vless/trojan/ss/hy2/tuic 节点出口（内嵌 sing-box）
│   │   ├── model_region.go    地区受限模型的节点校验与标记
│   │   ├── sub.go             订阅抓取、解析、缓存
│   │   ├── providers_config.go  通用 Provider 配置与运行时注册表
│   │   ├── providers_catalog.go 目录拉取与免费模型判定
│   │   ├── providers_chat.go    通用 Provider 请求转发与聊天入口
│   │   ├── thought_signature.go Gemini thought-signature 缓存与回填
│   │   ├── gemini_quota.go      Gemini 配额语义与永久拒绝判定
│   │   ├── compact.go         opencode 官方摘要压缩机制移植
│   │   ├── responses.go       /v1/responses 转换
│   │   ├── models.go          Cline 官方免费模型同步
│   │   ├── pool.go            账号池管理与持久化
│   │   ├── logs.go            请求日志与过滤
│   │   ├── stats.go           token 统计与 JSONL 日志
│   │   ├── admin.go           管理后台 REST API
│   │   ├── admin_zen.go       zen 管理页面与 API
│   │   ├── admin_providers.go 通用 Provider 管理 API
│   │   ├── admin_html.go      管理后台页面
│   │   ├── tray_windows.go    系统托盘（Windows）
│   │   ├── tray_other.go      托盘占位（其他平台）
│   │   └── types.go           数据结构
│   ├── providers/
│   │   ├── provider.go        Provider 接口与类型
│   │   ├── router.go          按注入的分类函数路由分发
│   │   └── clinepass.go       ClinePass key 池（轮询/冷却/恢复）
│   ├── protocol/
│   │   ├── streaming.go       SSE 扫描、空 choices 兜底
│   │   ├── normalize.go       OpenAI chunk 归一化
│   │   ├── openai_anthropic.go Anthropic ↔ OpenAI 转换
│   │   └── responses_chat.go  Responses ↔ Chat 转换
│   ├── cline/                 WorkOS OAuth、token 刷新
│   └── kit/                   HTTP 客户端、路径解析、身份轮换
├── Dockerfile
├── docker-compose.yml
├── go.mod
└── override.md                可选的系统提示词覆盖
```

运行时数据在 `data/`（首次使用时生成）：

| 文件 | 内容 |
|---|---|
| `.cline-accounts.json` | Cline 账号池 |
| `.zen-config.json` | zen 与通用 Provider 配置 |
| `.clinepass-keys.json` | ClinePass 订阅 key |
| `subs_cache.json` | 订阅解析缓存 |
| `requests.jsonl` | 请求日志 |
| `zen-stats.jsonl` | token 统计 |
| `cline-proxy.log` | 运行日志 |

## 开发与构建

```bash
go build -tags "with_quic,with_grpc,with_utls" ./...   # 编译（标签见上）
go vet -tags "with_quic,with_grpc,with_utls" ./...     # 静态检查
go test -tags "with_quic,with_grpc,with_utls" ./...    # 单元测试
```

测试覆盖协议转换、provider 目录与免费判定、Gemini 签名与配额解析、路由与冷却逻辑。不带构建标签运行时，涉及节点出站的用例会因缺协议栈而失败——这是标签要求的一部分，不是测试本身的问题。

## 在国内无代理推送

GitHub 的 HTTPS 端口在大陆经常不可达，但 **SSH 端口是通的** —— GitHub 官方还在
443 端口提供 SSH 服务，而这个端口就是 HTTPS 端口，几乎不会被封。配上 SSH key 后，
拉取与推送都不再依赖代理软件。

本仓库的 `origin` 已指向 SSH over 443：

```bash
git remote -v
# origin  ssh://git@ssh.github.com:443/<user>/<repo>.git (fetch)
# origin  ssh://git@ssh.github.com:443/<user>/<repo>.git (push)
```

一台新机器上的完整设置：

```bash
# 1. 生成 key（已有可跳过）
ssh-keygen -t ed25519 -C "your@email"

# 2. 公钥粘贴到 GitHub → Settings → SSH and GPG keys → New SSH key
cat ~/.ssh/id_ed25519.pub

# 3. 验证（应回显 Hi <用户名>! You've successfully authenticated）
ssh -T -p 443 git@ssh.github.com

# 4. 切换 remote（fetch 与 push 都要改）
git remote set-url        origin ssh://git@ssh.github.com:443/<user>/<repo>.git
git remote set-url --push origin ssh://git@ssh.github.com:443/<user>/<repo>.git
```

想一次覆盖本机所有仓库，加一条全局重写即可：

```bash
git config --global url."ssh://git@ssh.github.com:443/".insteadOf "https://github.com/"
```

### 两个容易踩的坑

- `git remote set-url` **只改 fetch**。若这个 remote 之前单独设过 push URL，
  必须再执行一次 `set-url --push`；否则 `git remote -v` 里 push 一行仍是
  `https://github.com/...`，推送照样要代理。排查时先看这一行。
- 拉取慢时可以读加速镜像（本仓库保留了一个只读的 `mirror` remote）：

  ```bash
  git fetch mirror     # 读走镜像，写仍走 SSH
  ```

  这类镜像（如 `github.boki.moe`）只代理只读流量的居多，推送以 SSH 为准。

## 致谢

- [YuJunZhiXue/Cline-proxy](https://github.com/YuJunZhiXue/Cline-proxy) — 项目基座
- [defyma/cline-proxy](https://github.com/defyma/cline-proxy) — SSE 空 choices 兜底思路
- [hayou2002/clinepass-proxy](https://github.com/hayou2002/clinepass-proxy) — ClinePass key 池轮询/冷却与 reasoning 透传
- [okhsunrog/claude-proxy-rs](https://github.com/okhsunrog/claude-proxy-rs) — 跨账号故障转移思路
- [www222fff/free-router](https://github.com/www222fff/free-router) — 通用 Provider 的免费模型判定与 Gemini 兼容思路
- [LINUX DO](https://linux.do) 社区
