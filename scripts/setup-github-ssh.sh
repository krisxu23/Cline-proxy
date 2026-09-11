#!/usr/bin/env bash
# ---------------------------------------------------------------------------
# 让 git 在大陆无代理环境下稳定访问 GitHub
#
# 背景（2026-09 实测）:
#   github.com 的 HTTPS(443) 直连被重置, 而 GitHub 官方在 ssh.github.com 的
#   443 端口提供 SSH 服务 —— 443 就是 HTTPS 端口, 几乎不会被封。因此把 git
#   流量整体切到 SSH over 443 即可, 不需要任何代理软件。
#
# 这个脚本是幂等的, 重复执行不会产生副作用。适用 Windows(Git Bash)/macOS/Linux。
#
# 用法:
#   bash setup-github-ssh.sh            # 应用配置并自检
#   bash setup-github-ssh.sh --check    # 只自检, 不改任何东西
# ---------------------------------------------------------------------------
set -u

SSH_HOST="ssh.github.com"
SSH_PORT="443"
SSH_USER="git"
KEY="$HOME/.ssh/id_ed25519"
SSH_CONFIG="$HOME/.ssh/config"
CONFIG_MARK="# >>> github-ssh-443 (managed) >>>"
CHECK_ONLY=0
[ "${1:-}" = "--check" ] && CHECK_ONLY=1

say()  { printf '  %s\n' "$*"; }
head_() { printf '\n=== %s ===\n' "$*"; }
die()  { printf '\nERROR: %s\n' "$*" >&2; exit 1; }

command -v git >/dev/null 2>&1 || die "git 不在 PATH 里"
command -v ssh >/dev/null 2>&1 || die "ssh 不在 PATH 里"

head_ "1/5 准备 SSH key"
mkdir -p "$HOME/.ssh" && chmod 700 "$HOME/.ssh" 2>/dev/null || true
if [ -f "$KEY" ]; then
  say "已存在: $KEY"
else
  [ "$CHECK_ONLY" = "1" ] && die "缺少 $KEY（--check 模式不会创建）"
  say "未找到, 正在生成 ed25519 key（无口令）"
  ssh-keygen -t ed25519 -N "" -C "$(git config --global user.email 2>/dev/null || echo github)" -f "$KEY" >/dev/null || die "ssh-keygen 失败"
  say "已生成: $KEY"
fi
say "公钥指纹: $(ssh-keygen -lf "$KEY.pub" 2>/dev/null | awk '{print $2}')"

head_ "2/5 配置 ~/.ssh/config"
# 让 git@github.com:owner/repo.git 这种写法自动走 443 端口。
if [ "$CHECK_ONLY" = "0" ]; then
  if grep -qF "$CONFIG_MARK" "$SSH_CONFIG" 2>/dev/null; then
    say "已存在受管配置块, 跳过"
  else
    {
      printf '\n%s\n' "$CONFIG_MARK"
      printf 'Host github.com\n'
      printf '  HostName %s\n' "$SSH_HOST"
      printf '  Port %s\n' "$SSH_PORT"
      printf '  User %s\n' "$SSH_USER"
      printf '# <<< github-ssh-443 (managed) <<<\n'
    } >> "$SSH_CONFIG"
    chmod 600 "$SSH_CONFIG" 2>/dev/null || true
    say "已写入受管配置块到 $SSH_CONFIG"
  fi
else
  say "（--check 跳过）"
fi

head_ "3/5 配置全局 URL 重写"
# https://github.com/... -> ssh://git@ssh.github.com:443/...
# 这样已有的 HTTPS remote 不用逐个改。
if [ "$CHECK_ONLY" = "0" ]; then
  git config --global url."ssh://$SSH_USER@$SSH_HOST:$SSH_PORT/".insteadOf "https://github.com/"
  say "已设置 insteadOf 重写"
else
  say "（--check 跳过）"
fi

head_ "4/5 自检: SSH 连通性"
# 注意: ssh -T 在认证成功时也会返回 1（GitHub 不提供 shell）, 所以看输出而不是退出码。
OUT="$(ssh -o BatchMode=yes -o StrictHostKeyChecking=accept-new -o ConnectTimeout=15 \
        -p "$SSH_PORT" -T "$SSH_USER@$SSH_HOST" 2>&1)"
case "$OUT" in
  *"successfully authenticated"*)
    say "OK  $OUT"
    AUTH_OK=1 ;;
  *"Permission denied"*)
    say "FAIL 认证被拒 —— 这个 key 还没加到 GitHub"
    AUTH_OK=0 ;;
  *)
    say "FAIL 无法连接: $(printf '%s' "$OUT" | head -2 | tr '\n' ' ')"
    AUTH_OK=0 ;;
esac

head_ "5/5 自检: git 读写"
if [ "$AUTH_OK" = "1" ]; then
  if git ls-remote "ssh://$SSH_USER@$SSH_HOST:$SSH_PORT/octocat/Hello-World.git" HEAD >/dev/null 2>&1; then
    say "OK  公开仓库读取正常"
  else
    say "WARN 公开仓库读取失败（可能只是网络抖动）"
  fi
fi

head_ "结果"
if [ "$AUTH_OK" = "1" ]; then
  say "配置完成。仓库改用 SSH 即可, 记得 fetch 与 push 都要改:"
  say "  git remote set-url        origin ssh://$SSH_USER@$SSH_HOST:$SSH_PORT/<user>/<repo>.git"
  say "  git remote set-url --push origin ssh://$SSH_USER@$SSH_HOST:$SSH_PORT/<user>/<repo>.git"
  say "  git remote -v   # 确认 fetch 与 push 两行都不是 https://github.com/"
else
  printf '\n需要手工完成一步: 把这个公钥加到 GitHub → Settings → SSH and GPG keys\n\n'
  cat "$KEY.pub"
  printf '\n加完后重新运行本脚本即可。\n'
fi

# 提醒一个本机已存在的安全隐患（不自动改, 由使用者决定）
if [ "$(git config --global --get http.sslverify 2>/dev/null)" = "false" ]; then
  printf '\n[安全提醒] 本机 http.sslverify=false, 所有 HTTPS git 操作都不校验证书。\n'
  printf '切到 SSH 之后这个设置已无必要, 建议恢复:\n'
  printf '  git config --global --unset http.sslverify\n'
fi
