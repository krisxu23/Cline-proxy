# AGENTS.md — 给自动化 agent 的操作约定

本文件是 agent（WorkBuddy / Claude Code / Cursor / Codex 等）在本仓库工作时的操作约定。
**下面的网络约定是硬性要求，任何 agent 都必须遵守，不得改用其他方式访问 GitHub。**

## 1. GitHub 连通性（硬性要求）

本机在中国大陆，`github.com` 的 **HTTPS(443) 直连被重置**，且常常没有代理软件在跑。
因此：

> **访问 GitHub 一律走 SSH over 443，禁止使用 `https://github.com/` 直连 git 操作。**

### 首选：git 操作（clone / fetch / push / ls-remote）

```bash
git remote set-url        origin ssh://git@ssh.github.com:443/<owner>/<repo>.git
git remote set-url --push origin ssh://git@ssh.github.com:443/<owner>/<repo>.git
```

- **必须两条都执行**。`git remote set-url` 只改 fetch；若该 remote 之前单独设过 push URL，
  少执行 `--push` 那条，推送仍会走 HTTPS 直连并失败。
- 排查入口永远是 `git remote -v`：输出是两行，只要 fetch 或 push 任一行还是
  `https://github.com/`，那次操作就仍然需要代理。

### 一次性配置（新机器 / 其他 agent 工具）

直接执行仓库内脚本，幂等，可重复运行：

```bash
bash scripts/setup-github-ssh.sh          # 应用配置并自检
bash scripts/setup-github-ssh.sh --check  # 只自检, 不改动任何东西
```

它等价于下面三条命令：

```bash
# 1. 全局 URL 重写: 已有的 https://github.com/... remote 自动改走 SSH 443
git config --global url."ssh://git@ssh.github.com:443/".insteadOf "https://github.com/"

# 2. 让 scp 风格的 git@github.com:owner/repo.git 也走 443（写入 ~/.ssh/config）
#    Host github.com / HostName ssh.github.com / Port 443 / User git

# 3. 自检（认证成功时退出码是 1，属正常，看输出里的 "successfully authenticated"）
ssh -T -p 443 git@ssh.github.com
```

### 其他 GitHub 域名的实测结论（2026-09）

| 域名 | 直连 | 说明 |
|---|---|---|
| `ssh.github.com:443` | ✅ 可用 | **git 读写首选** |
| `github.com:22` (SSH) | ✅ 可用 | 同样可行，但 22 端口更容易被单位/酒店网络封 |
| `github.com` (HTTPS) | ❌ 被重置 | 不要用 |
| `api.github.com` | ✅ 200 | REST API 可直连；取文件内容用 `GET /repos/{o}/{r}/contents/{path}` |
| `api.githubcopilot.com/mcp/` | ✅ 401 | GitHub MCP 端点直连可用（401 = 待 token，正常） |
| `codeload.github.com` | ✅ 200 | 整包下载：`/repos...` → `https://codeload.github.com/<o>/<r>/tar.gz/refs/heads/<br>` |
| `raw.githubusercontent.com` | ❌ 000 | 被墙；改用 `api.github.com` 的 contents 接口 |
| `objects.githubusercontent.com` | ❌ 000 | release 附件 / LFS 被墙，需代理兜底 |
| `github.boki.moe` 等加速镜像 | ⚠️ 不可靠 | `info/refs` 探测返回 200，但实际 git POST 与 archive 拿不到数据。**不要依赖** |

结论：**读取与下载同样走 SSH**（`git clone ssh://git@ssh.github.com:443/...`）；
不是 git 仓库的场景才用 `api.github.com` / `codeload.github.com`。

## 2. 提交约定

- commit 信息只描述**最终状态**，不写"修复了 X"式的过程叙述。
- 不引入新的第三方依赖，保持单 exe 分发。

## 3. 构建

```bash
export PATH="/c/Go/bin:$PATH" GOROOT="C:\\Go"     # 本机 Go 不在 PATH
go build -tags "with_quic,with_grpc,with_utls" ./...
go test  -count=1 -tags "with_quic,with_grpc,with_utls" ./internal/...
```

构建标签是硬要求：不带标签时 reality/QUIC 类节点会被剔除，相关用例会失败。
给原生命令传输出路径要用 Windows 风格（`-o "D:/x/app.exe"`），Git Bash 的路径转换
会把 `/d/x/app.exe` 交给 Windows 版 go，实际落到 `D:\d\x\app.exe`。
