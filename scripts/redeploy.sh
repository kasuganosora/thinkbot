#!/usr/bin/env bash
# thinkbot 重部署脚本：停止旧实例 -> 构建 -> 启动 -> 健康检查
# 用法：bash scripts/redeploy.sh
set -u
REPO=/Users/sion/Documents/thinkbot
cd "$REPO" || exit 1

log() { echo "[$(date '+%Y-%m-%dT%H:%M:%S%z')] $*"; }

# ---- 1. 停止旧实例 ----
stop_thinkbot() {
  local pids
  pids=$(pgrep -f "thinkbot --docker-sandbox" 2>/dev/null || true)
  if [ -z "$pids" ]; then
    pids=$(lsof -tiTCP:8080 -sTCP:LISTEN 2>/dev/null || true)
  fi
  if [ -n "$pids" ]; then
    log "stopping thinkbot pids: $pids"
    kill $pids 2>/dev/null || true
    sleep 2
    local still
    still=$(pgrep -f "thinkbot --docker-sandbox" 2>/dev/null || true)
    if [ -n "$still" ]; then
      log "force kill: $still"
      kill -9 $still 2>/dev/null || true
      sleep 1
    fi
  else
    log "no running thinkbot found"
  fi
}
stop_thinkbot

# ---- 2. 构建（注入构建信息，供 /health 探针识别运行版本）----
log "building thinkbot (with all uncommitted fixes)..."
BUILD_LDFLAGS="-s -w"
BUILD_LDFLAGS="$BUILD_LDFLAGS -X github.com/kasuganosora/thinkbot/internal/buildinfo.GitRevision=$(git rev-parse HEAD 2>/dev/null || echo unknown)"
BUILD_LDFLAGS="$BUILD_LDFLAGS -X github.com/kasuganosora/thinkbot/internal/buildinfo.BuildTime=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
BUILD_LDFLAGS="$BUILD_LDFLAGS -X github.com/kasuganosora/thinkbot/internal/buildinfo.Version=$(git describe --tags --always 2>/dev/null || echo dev)"
if ! go build -ldflags="$BUILD_LDFLAGS" -o thinkbot ./cmd; then
  log "BUILD FAILED — aborting redeploy, old binary untouched"
  exit 2
fi
log "build OK (git=$(git rev-parse --short HEAD 2>/dev/null), buildTime=$(date -u +%Y-%m-%dT%H:%M:%SZ))"

# ---- 3. 启动 ----
# macOS 无 setsid；用 Python 双 fork + os.setsid 守护化，使进程脱离 Bash 工具会话常驻。
log "launching ./thinkbot --docker-sandbox (detached via run_thinkbot.py)"
python3 scripts/run_thinkbot.py
sleep 4

# ---- 4. 健康检查 ----
if curl -sf http://localhost:8080/health >/dev/null 2>&1; then
  log "HEALTH OK — thinkbot listening on :8080"
else
  log "HEALTH CHECK FAILED — check logs/launch.out and logs/thinkbot.log"
fi
