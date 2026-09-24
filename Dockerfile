# 多平台说明: 本 Dockerfile 仅构建 linux/amd64。本机交付目标是 Windows x86-64,
# 而 CI/镜像场景通常也是 x86-64 服务器; 若需 arm64 请在 `docker buildx build
# --platform linux/arm64` 时确认 with_quic/with_utls 等标签在该平台可用, 并单独
# 维护一份构建矩阵, 此处刻意不做多平台以免引入未验证的架构组合。
FROM golang:1.26-alpine AS builder

# 构建版本号(P3-29): 与 .github/workflows/build.yml 用同一注入点
# -X free-router/internal/app.buildVersion, 未传时回退 dev ——
# 否则容器 /health 恒报硬编码默认值 go-1.1, 无法区分线上构建版本。
ARG VERSION=dev

WORKDIR /build
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -tags with_quic,with_grpc,with_utls -buildvcs=false -ldflags="-s -w -X free-router/internal/app.buildVersion=${VERSION}" -o free-router ./cmd/free-router

FROM alpine:3.21

RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app
COPY --from=builder /build/free-router .

# 以非 root 用户运行(安全加固): 固定 uid/gid=10001, 并把 /app(含数据目录)
# 的所有权交还给该用户。容器仍监听 0.0.0.0(见下方 CMD), 与 USER 切换无关。
RUN addgroup -S -g 10001 appuser \
 && adduser -S -u 10001 -G appuser appuser \
 && mkdir -p /app/data \
 && chown -R appuser:appuser /app
USER appuser

EXPOSE 3457

# 健康检查: 直接打进程内 /health(无需令牌, 只回状态)。容器编排据此判定就绪/存活。
#
# 端口从**进程实际参数**里取(2026-09-24 审查): 此前写死 3457, 一旦用
# `docker run ... -port 8080` 覆盖 CMD, 健康检查会永远打不通 → 容器永久 unhealthy。
# /proc/1/cmdline 是 NUL 分隔的 argv, 转成行后取 -port 的下一行; 取不到回落 3457。
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD sh -c 'port=$(tr "\0" "\n" < /proc/1/cmdline 2>/dev/null | grep -A1 "^-port$" | tail -1); wget -qO- "http://127.0.0.1:${port:-3457}/health" || exit 1'

VOLUME ["/app/data"]

# 这里刻意不设 PORT 环境变量(P3-30): 程序只认 -port flag, 全仓无任何
# Getenv("PORT"), 设了也是假旋钮。端口由下方 CMD 的 -port(及 HEALTHCHECK)决定。

ENTRYPOINT ["/app/free-router"]
# 容器内必须显式 0.0.0.0, 否则端口映射不可达; 这与「镜像内绑定 0.0.0.0」是两回事 ——
# 真正对宿主机暴露哪些端口由 `docker run -p` / compose 的 ports 字段决定(例如
# `ports: "3457:3457"` 会把容器 3457 映射到宿主机 3457, 即绑宿主 0.0.0.0)。无论怎么
# 暴露, 管理后台都强制要求 ADMIN token, 不会因端口可达而裸奔。
CMD ["-host", "0.0.0.0", "-port", "3457"]
