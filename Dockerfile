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
# util-linux：提供 setpriv（非 root 降权运行）
# wget：健康检查 / 调试
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates docker.io util-linux wget \
    && rm -rf /var/lib/apt/lists/*

# 非 root 运行用户（uid/gid 1000）。DooD 仍需经 docker.sock 控制宿主 daemon，
# 由 entrypoint 在启动时把该用户加入 docker.sock 所属组，再降权运行主程序。
RUN groupadd -r -g 1000 thinkbot && useradd -r -u 1000 -g thinkbot thinkbot

WORKDIR /app
COPY --from=builder /out/thinkbot /app/thinkbot
COPY docker/entrypoint.sh /app/entrypoint.sh
RUN chmod +x /app/entrypoint.sh
# 日志目录（主程序以 ./logs 相对路径写入），预先建好并归属运行用户，
# 否则非 root 运行的主程序在 MkdirAll 时因 /app 属 root 而失败 panic（报告 5870 关联）。
RUN mkdir -p /app/logs && chown -R thinkbot:thinkbot /app/logs

# 默认配置（DooD 友好：sandbox.backend=docker）。可用挂载的 .env 覆盖。
COPY .env.example /app/.env

EXPOSE 8080
# 以 root 启动 entrypoint 完成 docker.sock 组归属设置，再降权到 thinkbot 运行主程序。
ENTRYPOINT ["/app/entrypoint.sh"]
