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

VOLUME ["/app/data"]

ENV PORT=3457

ENTRYPOINT ["/app/cline-proxy"]
# 容器内必须显式 0.0.0.0，否则端口映射不可达；管理后台的访问控制由 ADMIN token 负责。
CMD ["-host", "0.0.0.0", "-port", "3457"]
