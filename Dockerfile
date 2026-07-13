# syntax=docker/dockerfile:1

# ============================================================================
# ThinkBot DooD 构建
#
# 部署形态：主程序运行在本容器内，通过挂载的 /var/run/docker.sock 指挥宿主
# Docker daemon，为每个 bot 创建独立的「兄弟容器」（sandbox）。因此本镜像需要
# docker CLI（裸调 docker 命令，与 sandbox 代码一致），但不需要运行 dockerd
# （容器本身不以 privileged 运行，daemon 来自宿主）。
# ============================================================================

# ---- 构建阶段 ----
FROM golang:1.25-bookworm AS builder
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
# CGO_ENABLED=0 生成纯静态二进制；若项目使用 CGO 版 SQLite 驱动，请改为 1 并
# 在 builder 阶段安装 gcc/musl-dev。
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/thinkbot ./cmd

# ---- 运行阶段 ----
FROM debian:bookworm-slim

# ca-certificates：HTTPS 出站调用（LLM API、web_fetch 等）
# docker.io：docker CLI（DooD 关键，通过挂载的 docker.sock 连接宿主 daemon）
# wget：健康检查 / 调试
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates docker.io wget \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app
COPY --from=builder /out/thinkbot /app/thinkbot

# 默认配置（DooD 友好：sandbox.backend=docker）。可用挂载的 .env 覆盖。
COPY .env.example /app/.env

EXPOSE 8080
ENTRYPOINT ["/app/thinkbot"]
