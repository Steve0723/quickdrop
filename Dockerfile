# ---------- 构建阶段 ----------
FROM golang:1.22-alpine AS builder

WORKDIR /src

# 无第三方依赖，先放 go.mod 便于层缓存
COPY go.mod ./
RUN go mod download

# 前端静态资源通过 go:embed 打进二进制
COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/quickdrop .

# ---------- 运行阶段 ----------
FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata su-exec \
 && addgroup -g 10001 -S app \
 && adduser -u 10001 -S -G app -h /app app

WORKDIR /app

COPY --from=builder /out/quickdrop /app/quickdrop
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh

RUN chmod +x /usr/local/bin/docker-entrypoint.sh \
 && mkdir -p /app/data \
 && chown -R app:app /app

ENV PORT=6026 \
    HOST=0.0.0.0 \
    QD_DATA_DIR=/app/data \
    TZ=Asia/Shanghai

EXPOSE 6026
VOLUME ["/app/data"]

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD wget -q -O /dev/null http://127.0.0.1:6026/api/me || exit 1

# 以 root 启动，入口脚本修正数据目录属主后降权到 app(10001) 运行
ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
CMD ["/app/quickdrop"]
