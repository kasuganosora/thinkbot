# syntax=docker/dockerfile:1

# ============================================================================
# ThinkBot DooD 构建
#
# 部署形态：主程序运行在本容器内，通过挂载的 /var/run/docker.sock 指挥宿主
# Docker daemon，为每个 bot 创建独立的「兄弟容器」（sandbox）。因此本镜像需要
# docker CLI（裸调 docker 命令，与 sandbox 代码一致），但不需要运行 dockerd
# （容器本身不以 privileged 运行，daemon 来自宿主）。
# ============================================================================

# ---- 前端构建阶段 ----
# api/router.go 在运行期从相对路径 static 提供 SPA，目录不存在则不注册静态路由，
# 管理界面直接 404。因此前端产物必须进镜像，不能只打包二进制。
FROM node:22-bookworm-slim AS frontend
WORKDIR /web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
# vite 的 outDir 相对 root（默认 = cwd，即 npm run 所在的 /web）解析：
# ../static → /static，故运行阶段从 /static 拷。此解析已实测确认（在同等目录结构的
# 临时目录实跑 build，产物落在 web 的兄弟目录，而非再上一级）。
RUN npm run build

# ---- 构建阶段 ----
FROM golang:1.25-bookworm AS builder
WORKDIR /src

# 必须 CGO：本项目 db/db.go 用 gorm.io/driver/sqlite，其底层驱动是
# github.com/mattn/go-sqlite3（C 实现），而非纯 Go 的 modernc.org/sqlite。
# gcc + libc6-dev 供 cgo 编译该驱动。
RUN apt-get update \
    && apt-get install -y --no-install-recommends gcc libc6-dev \
    && rm -rf /var/lib/apt/lists/*

COPY go.mod go.sum ./
RUN go mod download

COPY . .
# 版本信息注入（可选）。internal/singleinst 用 buildinfo.BuildTimeUnix 作为版本号
# 做单实例协商；未注入时回退到「二进制 mtime」，容器内该 mtime 取自 COPY 时间，
# 可用但不精确。compose 里已通过 build.args 传入，见 docker-compose.yml。
ARG BUILD_TIME=unknown
ARG GIT_REVISION=unknown
ARG VERSION=dev

# ⚠️ CGO_ENABLED 必须为 1，不可改成 0。
# go-sqlite3 在 CGO_ENABLED=0 下仍能【编译通过】，但会被替换成 stub，运行期首次
# 打开数据库即返回 "Binary was compiled with 'CGO_ENABLED=0', go-sqlite3 requires
# cgo to work. This is a stub"，主程序无法启动（已实测复现）。
# 代价：二进制动态链接 glibc，故运行阶段必须同为 bookworm（下方即是），
# 不能换成 alpine（musl）或 scratch。
RUN CGO_ENABLED=1 GOOS=linux go build \
    -ldflags="-s -w \
      -X github.com/kasuganosora/thinkbot/internal/buildinfo.BuildTime=${BUILD_TIME} \
      -X github.com/kasuganosora/thinkbot/internal/buildinfo.GitRevision=${GIT_REVISION} \
      -X github.com/kasuganosora/thinkbot/internal/buildinfo.Version=${VERSION}" \
    -o /out/thinkbot ./cmd

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

# entrypoint 用 setpriv 降权，而 setpriv【不会改写环境变量】——HOME 仍是 root 的
# /root。docker CLI 据 HOME 找 ~/.docker/config.json，读不到就往 stderr 打
# "WARNING: Error loading config file: /root/.docker/config.json: permission denied"。
# 这不只是噪音：sandbox/botcontainer.go 有把 stdout+stderr 合并进同一 buffer 后
# 直接解析输出的调用点（如 docker commit 取镜像 ID），WARNING 会污染解析结果。
# 故显式把 HOME 与 DOCKER_CONFIG 指到运行用户自己的可写目录（已实测消除该 WARNING）。
ENV HOME=/home/thinkbot \
    DOCKER_CONFIG=/home/thinkbot/.docker
RUN mkdir -p /home/thinkbot/.docker && chown -R thinkbot:thinkbot /home/thinkbot

WORKDIR /app
COPY --from=builder /out/thinkbot /app/thinkbot
COPY docker/entrypoint.sh /app/entrypoint.sh
# Windows checkout 可能把 .sh 检成 CRLF；去 \r 避免 shebang 变成 #!/bin/sh\r。
RUN sed -i 's/\r$//' /app/entrypoint.sh && chmod +x /app/entrypoint.sh
# 数据与日志目录（主程序以 ./data、./logs 相对路径写入），预先建好并归属运行用户。
# 否则非 root 运行时 MkdirAll 会因 /app 属 root 而失败 panic（报告 5870 关联）。
# compose 会把宿主目录挂到这两处；无挂载时镜像内目录也能直接落库。
RUN mkdir -p /app/data /app/logs && chown -R thinkbot:thinkbot /app/data /app/logs

# 前端构建产物（SPA，由 frontend 阶段产出）
COPY --from=frontend /static /app/static

# 默认配置（DooD 友好：sandbox.backend=docker）。可用挂载的 .env 覆盖。
COPY .env.example /app/.env

EXPOSE 8080
# 以 root 启动 entrypoint 完成 docker.sock 组归属设置，再降权到 thinkbot 运行主程序。
ENTRYPOINT ["/app/entrypoint.sh"]
