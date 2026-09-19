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
FROM golang:1.27-bookworm AS builder
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

# 自举 loader 二进制（/thinkbot-loader）：默认不进 slim 最终镜像，仅自托管阶段使用。
# 复用同一组 buildinfo 注入，使其 /loader/health 也能回报版本。
RUN CGO_ENABLED=1 GOOS=linux go build \
    -ldflags="-s -w \
      -X github.com/kasuganosora/thinkbot/internal/buildinfo.BuildTime=${BUILD_TIME} \
      -X github.com/kasuganosora/thinkbot/internal/buildinfo.GitRevision=${GIT_REVISION} \
      -X github.com/kasuganosora/thinkbot/internal/buildinfo.Version=${VERSION}" \
    -o /out/thinkbot-loader ./cmd/loader

# ---- 运行阶段（默认，最小镜像，不含 loader/源码）----
FROM debian:bookworm-slim AS thinkbot-slim

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

# ============================================================================
# 自托管运行阶段（target: selfhost）
#
# 启用方式：docker compose 的 build.target 设为 selfhost（见 docker-compose.yml）。
# 该镜像在 slim 基础上额外包含「自举」所需的一切：
#   - 开发工具：git / go 工具链（golang 基础镜像）/ node22（前端 build）/ gcc+libc6-dev（cgo）
#   - 全量源码仓（含 .git，落在 /app/src），使容器内可执行 git pull 自部署
#   - thinkbot-loader 二进制与入口（entrypoint 在 loader 存在时改 exec loader）
# 配合宿主 .env 的 loader.enabled=true + loader.token，thinkbot 获得「监管 + 自部署 +
# 健康门控回滚」能力。镜像体积显著大于 slim（含 go/node 工具链），仅自托管场景使用。
#
# 注意：本阶段基础镜像即 golang:1.27-bookworm（含 gcc/cgo），故无需再装构建链；
# node 经 NodeSource 安装 v22（与 frontend 阶段一致），保证 vite 构建可用。
# ============================================================================
FROM golang:1.27-bookworm AS selfhost

# 系统依赖：git（自部署拉代码）、docker.io（DooD 同宿主 daemon）、util-linux（setpriv 降权）、
# wget（健康检查）、ca-certificates（HTTPS 出站）；gcc/libc6-dev 已随 golang 基础镜像具备，
# 此处再显式确保 cgo 链路完整。
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        git ca-certificates docker.io util-linux wget \
    && rm -rf /var/lib/apt/lists/*

# Node.js 22（前端构建）。经 NodeSource 仓库安装，与 frontend 阶段版本对齐。
RUN curl -fsSL https://deb.nodesource.com/setup_22.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && rm -rf /var/lib/apt/lists/*

# 非 root 运行用户（uid/gid 1000），与 slim 阶段一致。
RUN groupadd -r -g 1000 thinkbot && useradd -r -u 1000 -g thinkbot thinkbot

WORKDIR /app
# 全量源码仓（含 .git，由 .dockerignore 的 !.git 否定规则放行进上下文）。
COPY . /app/src
# 主程序与 loader 由 builder 阶段产出（带 buildinfo）。
COPY --from=builder /out/thinkbot /app/thinkbot
COPY --from=builder /out/thinkbot-loader /app/thinkbot-loader
COPY docker/entrypoint.sh /app/entrypoint.sh
RUN sed -i 's/\r$//' /app/entrypoint.sh && chmod +x /app/entrypoint.sh

# 初始前端构建（产物落 /app/src/static）；运行时 /app/static 软链指向它，
# 使后续自部署的 npm run build 直接落到同一路径，主程序 CWD=/app 即可读取。
RUN cd /app/src/web && npm ci && npm run build
RUN ln -s /app/src/static /app/static

# HOME / DOCKER_CONFIG 指到运行用户可写目录（消除 docker CLI 的 config 权限 WARNING）。
ENV HOME=/home/thinkbot \
    DOCKER_CONFIG=/home/thinkbot/.docker
RUN mkdir -p /home/thinkbot/.docker && chown -R thinkbot:thinkbot /home/thinkbot

# 数据与日志目录预先建好并归属运行用户（非 root 运行时 MkdirAll 不会因 /app 属 root panic）。
# 整棵 /app 归属 thinkbot：loader 需写 /app 做二进制 mv/回滚，git pull 需写 /app/src。
RUN mkdir -p /app/data /app/logs && chown -R thinkbot:thinkbot /app

# 默认配置（DooD 友好）。可用挂载的 .env 覆盖；自部署需在宿主 .env 设 loader.enabled=true。
COPY .env.example /app/.env

EXPOSE 8080
# entrypoint 在 /app/thinkbot-loader 存在时改 exec loader（否则仍直接 exec thinkbot）。
ENTRYPOINT ["/app/entrypoint.sh"]
