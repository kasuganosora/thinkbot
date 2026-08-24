# syntax=docker/dockerfile:1
#
# thinkbot 沙箱浏览器镜像（per-bot 容器内运行）
# 提供：headful Chromium + xvfb 伪装 + Patchright 反检测驱动 + 中文字体
# 用法：build 后把 config sandbox.image 指向本镜像，per-bot 容器按此镜像起，
#       浏览器经 `docker exec -i` 起的 MCP server 运行在容器内部（见设计文档 §4）。

FROM debian:bookworm-slim

ENV DEBIAN_FRONTEND=noninteractive

# ---------- 系统基础 + 浏览器 + 中文字体 + xvfb(伪装 headful) ----------
RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates curl wget git unzip jq \
        chromium fonts-noto-cjk fonts-noto-color-emoji \
        xvfb xauth \
        python3 python3-pip \
        locales \
    && sed -i '/zh_CN.UTF-8/s/^# *//' /etc/locale.gen \
    && locale-gen zh_CN.UTF-8 \
    && rm -rf /var/lib/apt/lists/*

# ---------- Node 22 LTS + Patchright（生产驱动库，API 与 Playwright 99% drop-in）----------
# 仅装 node 包即可；浏览器用系统 apt chromium（executablePath 指定），不触发 patchright 自下载。
RUN curl -fsSL https://deb.nodesource.com/setup_22.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && rm -rf /var/lib/apt/lists/* \
    && npm install -g patchright \
    && npm cache clean --force

# ---------- 浏览器运行配置 ----------
# 注意：executablePath 不是环境变量能定的——Playwright/Patchright 官方 env 只有
# PLAYWRIGHT_BROWSERS_PATH / PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD；实际路径在 launch 时传
# executablePath（封装层读 BOT_BROWSER_EXECUTABLE）。CHROMIUM_FLAGS 同理由封装层消费。
# --disable-dev-shm-usage 作为兜底（当容器 create 未给 --shm-size 时）；
#   优先由 botcontainer.go 的 --shm-size=512m 提供共享内存，二者不冲突，前者仅在无 shm 时生效。
# --force-webrtc-ip-handling-policy 防 WebRTC 绕过代理泄露真实 IP（走代理部署时关键）。
ENV PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD=1 \
    BOT_BROWSER_EXECUTABLE=/usr/bin/chromium \
    BOT_BROWSER_LAUNCH_ARGS="--no-sandbox --disable-dev-shm-usage --disable-gpu --disable-blink-features=AutomationControlled --force-webrtc-ip-handling-policy=disable_non_proxied_udp" \
    LANG=zh_CN.UTF-8 \
    LC_ALL=zh_CN.UTF-8 \
    NODE_PATH=/usr/lib/node_modules

# 容器工作目录 = sandbox.VirtualRoot (/data)，named volume 挂载点（见 sandbox/sandbox.go）
WORKDIR /data

# 预创建浏览器 profile 目录（封装层 launchPersistentContext 指向此路径，指纹/缓存持久化）。
# 此处仅建目录（默认 755、root 属主），最终属主与权限在下方非 root 加固阶段统一设置，
# 不再使用 777 这种全局可写权限。
RUN mkdir -p /data/.browser-profile

# ---------- 浏览器 MCP server（运行在容器内，thinkbot 经 `docker exec -i` 走 stdio 调用）----------
# 手写 JSON-RPC（不依赖 MCP SDK），驱动 patchright + 系统 chromium。
COPY browser-mcp.js /usr/local/bin/thinkbot-browser-mcp
RUN chmod +x /usr/local/bin/thinkbot-browser-mcp

# ---------- 非 root 加固（仅浏览器 MCP 降权；agent 沙箱与 volume 仍 root 兼容）----------
# 容器默认用户仍是 root（保持 agent 代码执行沙箱与 named volume 权限兼容），
# 但浏览器 MCP 经 launch 脚本以非 root `bot` 用户运行，缩小 --no-sandbox + root 的提权面。
# 浏览器相关目录统一归属 `bot`，且不再开放 777：profile 仅 bot 可读写（含登录态/指纹，
# 属敏感数据），screenshots 仅 owner 访问（root 仍可越权读，供 API 取图）。
RUN apt-get update && apt-get install -y --no-install-recommends util-linux \
    && rm -rf /var/lib/apt/lists/* \
    && useradd -m -s /bin/bash bot

# 浏览器状态/缓存/截图目录：预创建并归属 bot。named volume 首次初始化时会复制这些
# 文件（含 owner），新 bot 自动生效；既有 volume 需在部署时对现有容器做一次 chown。
RUN mkdir -p /data/.browser-profile /data/browser-screenshots \
    && touch /data/.browser-state.json \
    && chown -R bot:bot /home/bot /data/.browser-profile /data/browser-screenshots /data/.browser-state.json \
    && chmod 700 /data/.browser-profile /data/browser-screenshots \
    && chmod 664 /data/.browser-state.json

# 浏览器 MCP 启动器：以非 root 用户运行 xvfb + node wrapper（保留环境，透传 BOT_BROWSER_PROXY 等）。
COPY browser-launch.sh /usr/local/bin/thinkbot-browser-launch
RUN chmod +x /usr/local/bin/thinkbot-browser-launch
