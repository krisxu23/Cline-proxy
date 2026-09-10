# Cline Go Proxy — 综合网关

Cline API 反向代理 + opencode zen 免费模型 + ClinePass 订阅池三合一网关：一个二进制、三种上游，按 model 自动路由，同时向外提供 OpenAI（`/v1/chat/completions`）、Anthropic（`/v1/messages`）、OpenAI Responses（`/v1/responses`）三种协议接口，内置中文管理后台。

## 架构

```
客户端 (Cline / Claude Code / Cursor / CherryStudio ...)
   │  OpenAI / Anthropic / Responses 任一协议
   ▼
┌─────────────────────────────────────────────┐
│  internal/app   HTTP 路由 + 协议转换 + 管理   │
│      │                                      │
│      ▼  providers.Router (按 model 分流)     │
│  ┌──────────┬──────────────┬─────────────┐  │
│  │  cline   │     zen      │  clinepass  │  │
│  │ 账号池    │ opencode 免费 │ 订阅 key 池  │  │
│  └──────────┴──────────────┴─────────────┘  │
└─────────────────────────────────────────────┘
   │                │                 │
   ▼                ▼                 ▼
api.cline.bot   opencode.ai/zen    api.cline.bot
(账号轮询)        (免费模型)         (Bearer key)
```

- `internal/protocol/` — 纯函数协议层：SSE 解析与归一化、Anthropic ↔ OpenAI、Responses ↔ Chat。无 I/O、无状态，单元测试覆盖。
- `internal/providers/` — 上游 Provider 抽象 + 路由注册表 + ClinePass key 池。cline / zen 两个适配器留在 `internal/app/adapters.go`，复用账号池与限流状态机。

## 路由规则

| model 形态 | 上游 | 鉴权 |
|---|---|---|
| `cline-pass/*` | ClinePass 订阅池 | 独立 key 池（`/admin/` 管理） |
| `*-free`、`opencode/*` | opencode zen | 匿名/自配 key |
| 其余（如 `deepseek/deepseek-v4-flash`） | Cline 账号池 | OAuth 账号轮询 |

zen 连续失败时自动故障转移到 Cline 账号池；Cline 账号 429/掉线时自动换号重试（受池内可用账号数限制）。

## 快速开始

```bash
# 编译并启动（默认监听所有网卡，局域网可访问）
go build -o cline-proxy.exe .
./cline-proxy.exe

# 局域网访问地址：http://<本机局域网IP>:3457/admin/
# 仅允许本机访问时：
./cline-proxy.exe -host 127.0.0.1

# 指定端口
./cline-proxy.exe -port 3457

# 构建 + 启动 + 打开浏览器
go run . -start
```

监听所有网卡会开放管理后台给同网设备，建议仅在可信局域网使用，并在系统防火墙中限制 3457 端口。

### Docker 部署

```bash
docker compose up -d
docker compose logs -f
docker compose down
```

数据持久化在 `./data/` 目录下，`override.md` 会自动从项目根目录挂载到容器内。

## 配置客户端

**OpenAI 格式（/v1/chat/completions）：**
```
Base URL: http://<本机局域网IP>:3457/v1
API Key:  <在管理后台生成的 Key>
Model:    deepseek/deepseek-v4-flash        # Cline 账号池
          deepseek-v4-flash-free            # zen 免费模型
          cline-pass/deepseek-v4-flash      # ClinePass 订阅
```

**Anthropic 格式（/v1/messages）：**
```
Base URL: http://<本机局域网IP>:3457/v1
API Key:  <在管理后台生成的 Key>
Model:    同上，三种前缀任选
```

**Responses API（/v1/responses，Cursor 等）：**
```
Base URL: http://<本机局域网IP>:3457/v1
Model:    同上
```

`GET /v1/models` 实时聚合三种上游的全部可用模型。

## 上游配置

### 1. Cline 账号池

管理后台 **账号管理** → **导入账号**：

- **OAuth 浏览器登录**：点击按钮弹出 WorkOS 登录窗口，完成后自动填入
- **手动输入 Token**：输入已有账号的 Access Token
- **批量文件导入**：上传包含账号数据的 JSON 文件

账号操作：**⚡ 测试**（真实探测，失败按上游返回时长冷却）、**↻ 重置**（仅清今日调用）、**✕ 删除**。

命中 429 `INFERENCE_CAP_ERROR` 时自动解析 "Try again in 17h 59m" 并冷却，到期自动恢复；请求失败自动换下一个可用账号。

### 2. opencode zen 免费模型

无需配置，启动即启用（匿名 key `public`）。官方源 + 3 个 CDN 镜像共 4 个 API 端点（官方在前），重试与模型同步自动跨端点轮换；可在后台「API 端点」里整体替换。每 10 分钟自动同步官方模型列表；付费 zen 模型显式 400 拒绝。超限时按 opencode 官方算法做摘要压缩（尾部预算 → 锚定摘要模板 → 重组会话）。支持 http/https/socks5 代理池轮询出口（round_robin / random / fill）。后台 **opencode 免费模型** 页可调全部参数。

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

## 通用功能

- **API Key 鉴权**：后台 **设置** → **API Keys** 生成/删除；未配置任何 Key 时允许无鉴权访问
- **System Prompt 覆盖**：项目目录放 `override.md` 自动替换所有客户端的系统提示词
- **自定义请求头**：后台 **设置** → **请求头**，编辑转发给上游的头（如 `x-client-type: cline-cli`）
- **thinking 透传**：Anthropic 协议下上游 `reasoning_content` 自动转为 `thinking` 内容块（流式 + 非流式）
- **SSE 稳健性**：上游流无任何 choices 时自动补一个空 chunk 收尾，避免客户端报 "Provider returned no completion choices"
- **token 统计**：每请求 JSONL 落盘，按账号/上游/模型聚合，管理后台实时展示
- **多平台 CI/CD**：GitHub Actions 自动构建 6 平台二进制；Release 从 `v0.0.1` 起按语义版本递增

## 可用模型（实测）

### Cline 账号池

| 模型 ID | 状态 | 说明 |
|---------|:----:|------|
| `deepseek/deepseek-v4-flash` | ✅ 免费 | DeepSeek V4 Flash |
| `deepseek/deepseek-v4-pro` | ✅ 消耗额度 | DeepSeek V4 Pro |
| `openai/gpt-4.1-nano` | ✅ 消耗额度 | GPT-4.1 Nano |
| `qwen/qwen3-235b-a22b` | ✅ 消耗额度 | Qwen3 235B |
| `meta-llama/llama-4-maverick` | ✅ 消耗额度 | Llama 4 Maverick |
| `google/gemini-2.5-flash` | ⚠️ 响应为空 | API 返回 200 但内容为空 |
| `google/gemini-2.5-pro` | ⚠️ 响应为空 | API 返回 200 但内容为空 |

### opencode zen（免费）

| 模型 ID | 上下文 | 说明 |
|---------|:----:|------|
| `deepseek-v4-flash-free` | 200K | 别名 `deepseek-v4-flash` |
| `nemotron-3-ultra-free` | 1M | 免费里最大上下文 |
| `north-mini-code-free` | 256K | |
| `mimo-v2.5-free` / `ling-3.0-flash-free` / `laguna-s-2.1-free` / `longcat-2.0-free` / `big-pickle` | 200K | |

### ClinePass（需订阅）

`cline-pass/deepseek-v4-flash`、`cline-pass/glm-5.2`、`cline-pass/kimi-k2.7-code`、`cline-pass/qwen3.7-max` 等，完整列表见 `GET /admin/api/clinepass/models`。

## 项目结构

```
├── main.go                    入口与 CLI 参数
├── internal/
│   ├── app/
│   │   ├── proxy.go           HTTP 路由、协议转换、账号池调用与故障转移
│   │   ├── adapters.go        cline/zen Provider 适配器 + 网关组装
│   │   ├── clinepass.go       ClinePass 三协议 handler + admin API
│   │   ├── models.go          Cline 官方免费模型同步
│   │   ├── zen.go             zen 上游、三态路由、限流状态机、代理池
│   │   ├── compact.go         opencode 官方摘要压缩机制移植
│   │   ├── responses.go       /v1/responses 转换
│   │   ├── pool.go            账号池管理与持久化
│   │   ├── auth.go            WorkOS OAuth（internal/cline 的调用方）
│   │   ├── stats.go           token 统计与 JSONL 日志
│   │   ├── admin.go           管理后台 REST API
│   │   ├── admin_zen.go       zen 管理页面与 API
│   │   ├── admin_html.go      管理后台页面
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
├── .cline-accounts.json       账号池数据（运行时生成）
├── .zen-config.json           zen 配置（运行时生成）
├── .clinepass-keys.json       ClinePass keys（运行时生成）
└── override.md                可选的系统提示词覆盖
```

## 开发

```bash
go build ./...        # 编译
go vet ./...          # 静态检查
go test ./internal/... # 单元测试（protocol + providers）
```

## 致谢

- [YuJunZhiXue/Cline-proxy](https://github.com/YuJunZhiXue/Cline-proxy) — 项目基座
- [defyma/cline-proxy](https://github.com/defyma/cline-proxy) — SSE 空 choices 兜底思路
- [hayou2002/clinepass-proxy](https://github.com/hayou2002/clinepass-proxy) — ClinePass key 池轮询/冷却与 reasoning 透传
- [okhsunrog/claude-proxy-rs](https://github.com/okhsunrog/claude-proxy-rs) — 跨账号故障转移思路
- [LINUX DO](https://linux.do) 社区
