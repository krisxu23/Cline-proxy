# Cline Go Proxy — 综合网关

[![构建](https://github.com/krisxu23/Cline-proxy/actions/workflows/build.yml/badge.svg)](https://github.com/krisxu23/Cline-proxy/actions/workflows/build.yml)
[![Release](https://img.shields.io/github/v/release/krisxu23/Cline-proxy)](https://github.com/krisxu23/Cline-proxy/releases)
[![Go 版本](https://img.shields.io/github/go-mod/go-version/krisxu23/Cline-proxy)](https://go.dev/)
[![Stars](https://img.shields.io/github/stars/krisxu23/Cline-proxy)](https://github.com/krisxu23/Cline-proxy/stargazers)

四合一上游网关：**Cline 账号池 + opencode zen 免费模型 + ClinePass 订阅池 + 任意 OpenAI 兼容 Provider**。一个二进制，按 model 自动分流到四类上游，对外同时提供 OpenAI、Anthropic、OpenAI Responses 三种协议接口，内置中文管理后台。

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
- **系统托盘**：右下角托盘图标，右键可「打开管理界面 / 退出」
- **重复双击**：端口被占时自动并入已在运行的实例，直接弹出管理窗口
- **启动失败**：弹窗提示原因（如端口占用，可换端口）

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
./cline-proxy.exe                      # 默认监听所有网卡，局域网可访问
./cline-proxy.exe -host 127.0.0.1      # 仅允许本机访问
./cline-proxy.exe -port 3457           # 指定端口

# 局域网访问地址：http://<本机局域网IP>:3457/admin/
```

监听所有网卡会开放管理后台给同网设备，建议仅在可信局域网使用，并在系统防火墙中限制 3457 端口。

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
- **上下文压缩**：按 opencode 官方算法做摘要压缩（尾部预算 → 锚定摘要模板 → 重组会话）

出口（后台 **🌐 出口代理与节点**）：

- 支持 http/https/socks5 代理池轮询（`round_robin` / `random` / `fill`）
- 支持 vmess / vless / trojan / ss / hy2 / tuic / hysteria / anytls / ssh / shadowtls / snell 节点链接直接粘贴——内嵌 sing-box 把每个节点转成本地出口，轮询与冷却机制与普通代理一致
- **连通检测参与选路**：检测判定为不可达的节点不再被轮询，网络错误会让该出口短暂冷却，避免重试反复撞上同一个坏节点
- **地区受限模型**：部分模型（如 `muse-spark-1.3-contributor-free`）只对特定出口地区开放。这类模型会单独校验每个节点，节点列表中通过的带 🌍 标记；请求该模型时只走通过校验的节点
- **订阅链接**：后台填订阅地址，保存即抓取、每 6 小时自动刷新，支持 sing-box JSON / Clash YAML / base64 节点列表三种格式，节点并入代理池统一轮询，缓存落盘重启即用
- 节点列表实时展示协议 / 名称 / 来源 / 就绪状态与连通检测结果

### 3. ClinePass 订阅池

在管理后台或通过 REST API 添加订阅 key：

```bash
# 添加
curl -X POST http://127.0.0.1:3457/admin/api/clinepass/keys \
  -H 'Content-Type: application/json' -d '{"key":"sk-..."}'

# 查看（key 脱敏显示）
curl http://127.0.0.1:3457/admin/api/clinepass/keys

# 删除（传脱敏后的 key）
curl -X POST http://127.0.0.1:3457/admin/api/clinepass/keys/delete \
  -H 'Content-Type: application/json' -d '{"key":"sk-1****abcd"}'

# 可用模型
curl http://127.0.0.1:3457/admin/api/clinepass/models
```

多 key 自动轮询；key 命中 429 自动冷却 5 分钟，到期自动恢复。`reasoning` / `reasoning_content` 字段原样透传，CherryStudio 等客户端可正常显示思考过程。

### 4. 通用 Provider（任意 OpenAI 兼容上游）

把任意 OpenAI 兼容站点挂进网关，用 `provider:model` 直选。在后台 **🔌 通用 Provider（OpenAI 兼容上游）** 里填写，或直接改 `.zen-config.json` 的 `providers` 段。

免费判定有三种模式，按上游目录的特点选：

| 模式 | 配置 | 适用 |
|---|---|---|
| 按目录价格 | `catalog: true` + `pricing: true` | 目录里带价格的上游（如 OpenRouter），价格为 0 即免费 |
| 白名单 | 只填 `freeModels` | 目录里没有价格信息的上游（如 B.AI） |
| 白名单 + 目录校验 | `catalog: true` + `freeModels` | 白名单为主，但希望上游下架模型后自动剔除 |

面板上的操作：**保存 Provider**、**🔍 连通测试**（对指定模型发一次最小请求）、**🔄 刷新目录**；列表里显示每个 provider 的 `目录 N · 免费 N · 可聊 N` 与最近一次错误。目录在启动时立即拉取、之后每 15 分钟刷新一次；目录为空时请求路径会按 1 分钟退避自行重试。

配置示例：

```json
{
  "providers": {
    "openrouter": {
      "baseUrl": "https://openrouter.ai/api/v1",
      "apiKey": "sk-or-...",
      "catalog": true,
      "pricing": true
    },
    "gemini": {
      "baseUrl": "https://generativelanguage.googleapis.com/v1beta/openai",
      "apiKey": "AIza...",
      "catalog": true,
      "pricing": false,
      "modelsUrl": "https://generativelanguage.googleapis.com/v1beta/models",
      "modelsKeyHeader": "x-goog-api-key",
      "freeModels": ["gemini-3.8-flash", "gemini-3.7-flash"]
    },
    "bai": {
      "baseUrl": "https://api.b.ai/v1",
      "apiKey": "...",
      "freeModels": ["glm-5.3-flash", "deepseek-v4-flash", "mimo-v2.5"]
    }
  }
}
```

Gemini 需要额外两个字段：`modelsUrl` 指向原生目录（`/v1beta/models`），`modelsKeyHeader` 填 `x-goog-api-key`（该端点不使用 `Authorization: Bearer`）。Gemini 的 thought-signature 由网关自动处理：历史工具调用的签名缺失时回填跳过哨兵，上游拒绝已缓存签名时自动用哨兵重放一次。

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

- **API Key 鉴权**：后台 **🔑 API 密钥管理** 生成/删除；未配置任何 Key 时允许无鉴权访问
- **System Prompt 覆盖**：项目目录放 `override.md`，自动替换所有客户端的系统提示词
- **自定义请求头**：后台 **📨 请求头配置（模拟 Cline CLI 发出）**，编辑转发给上游的头
- **路由决策头**：每个响应带 `X-Proxy-Route`，标注 `upstream` / `model` / `failover`，排查路由一目了然
- **token 统计**：每请求 JSONL 落盘（`data/zen-stats.jsonl`），按账号/上游/模型聚合，后台实时展示
- **请求日志**：`data/requests.jsonl` 记录每次请求；运行日志写在 `data/cline-proxy.log`（追加模式，桌面形态同样落盘）
- **出口治理**：后台 **🌐 出口代理与节点** —— 代理策略、代理列表、订阅、节点列表与连通检测、限流防御、上下文压缩都在此页
- **thinking 透传**：Anthropic 协议下上游 `reasoning_content` 自动转为 `thinking` 内容块（流式 + 非流式）
- **SSE 稳健性**：上游流无任何 choices 时自动补一个空 chunk 收尾，避免客户端报 "Provider returned no completion choices"
- **熔断与自愈**：上游级熔断（连续 5xx/408/429 触发，窗口过期后半开探测，探测失败立即重跳闸；4xx 客户端错误不计入）→ key/账号级冷却（ClinePass key 与 Cline 账号命中 429 独立冷却，成功自动清零）→ 模型级路由（前缀显式分流，付费/未知模型明确拒绝）
- **多平台 CI/CD**：GitHub Actions 自动构建多平台二进制；Release 按语义版本递增

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
