# 多平台说明: 本 Dockerfile 仅构建 linux/amd64。本机交付目标是 Windows x86-64,
# 而 CI/镜像场景通常也是 x86-64 服务器; 若需 arm64 请在 `docker buildx build
# --platform linux/arm64` 时确认 with_quic/with_utls 等标签在该平台可用, 并单独
# 维护一份构建矩阵, 此处刻意不做多平台以免引入未验证的架构组合。
FROM golang:1.26-alpine AS builder

WORKDIR /build
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -tags with_quic,with_grpc,with_utls -buildvcs=false -ldflags="-s -w" -o cline-proxy .

FROM alpine:3.21

RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app
COPY --from=builder /build/cline-proxy .

EXPOSE 3457

# 健康检查: 直接打进程内 /health(无需令牌, 只回状态)。容器编排据此判定就绪/存活。
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD wget -qO- http://127.0.0.1:3457/health || exit 1

VOLUME ["/app/data"]

ENV PORT=3457

ENTRYPOINT ["/app/cline-proxy"]
# 容器内必须显式 0.0.0.0, 否则端口映射不可达; 这与「镜像内绑定 0.0.0.0」是两回事 ——
# 真正对宿主机暴露哪些端口由 `docker run -p` / compose 的 ports 字段决定(例如
# `ports: "3457:3457"` 会把容器 3457 映射到宿主机 3457, 即绑宿主 0.0.0.0)。无论怎么
# 暴露, 管理后台都强制要求 ADMIN token, 不会因端口可达而裸奔。
CMD ["-host", "0.0.0.0", "-port", "3457"]
