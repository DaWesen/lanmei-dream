# ── 多阶段构建 ──

FROM golang:1.25-alpine AS builder

RUN apk add --no-cache git

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /lanmei ./cmd/lanmei

# ── 运行阶段 ──

FROM alpine:3.21

RUN apk add --no-cache ca-certificates tzdata

WORKDIR /app

COPY --from=builder /lanmei /app/lanmei

# 环境变量通过 docker-compose / .env 注入，不硬编码
ENV TZ=Asia/Shanghai

ENTRYPOINT ["/app/lanmei"]
