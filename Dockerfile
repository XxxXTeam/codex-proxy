# 阶段一：构建
FROM golang:1.26-alpine AS builder

RUN apk add --no-cache git ca-certificates nodejs npm

WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download
COPY . .

RUN --mount=type=cache,target=/root/.npm \
    npm ci --prefix web && npm run build --prefix web

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -ldflags="-s -w" -o /codex-proxy .

# 阶段二：运行
FROM alpine:3.21

RUN apk add --no-cache ca-certificates tzdata
ENV TZ=Asia/Shanghai

WORKDIR /app
COPY --from=builder /codex-proxy /app/codex-proxy
COPY config.example.yaml /app/config.example.yaml

RUN mkdir -p /app/auths

EXPOSE 8080

ENTRYPOINT ["/app/codex-proxy"]
CMD ["-config", "/app/config.yaml"]
