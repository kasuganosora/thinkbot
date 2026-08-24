#!/usr/bin/env bash
# thinkbot 日志观察脚本：检查存活 + 扫描最近日志中的错误/恐慌
# 输出结构化状态，供人工或自动任务判断是否需要修复/重部署。
# 用法：bash scripts/watch.sh
set -u
REPO=/Users/sion/Documents/thinkbot
cd "$REPO" || exit 1
LOG=logs/thinkbot.log
OUT=logs/watch_8h.log

TS=$(date '+%Y-%m-%dT%H:%M:%S%z')

# ---- 存活 ----
if curl -sf http://localhost:8080/health >/dev/null 2>&1; then
  ALIVE=yes
else
  ALIVE=no
fi

# ---- 扫描最近 ~4000 行 ----
if [ -f "$LOG" ]; then
  TAIL=$(tail -n 4000 "$LOG")
else
  TAIL=""
fi
PANIC=$(printf '%s\n' "$TAIL" | grep -c '"level":"PANIC"\|"level":"FATAL"' || true)
ERROR=$(printf '%s\n' "$TAIL" | grep -c '"level":"ERROR"' || true)
# 重启循环迹象（短时间内 repeated listen/start）
RESTART=$(printf '%s\n' "$TAIL" | grep -c 'listen tcp.*address already in use\|panic:\|runtime error' || true)

# 最近的错误样本（最多 8 行）
SAMPLE=$(printf '%s\n' "$TAIL" | grep '"level":"ERROR"\|"level":"PANIC"\|"level":"FATAL"' | tail -n 8 || true)

{
  echo "=== watch $TS ==="
  echo "ALIVE=$ALIVE PANIC=$PANIC ERROR=$ERROR RESTART_SIGNS=$RESTART"
  if [ -n "$SAMPLE" ]; then
    echo "--- recent error/panic sample ---"
    echo "$SAMPLE"
  fi
} | tee -a "$OUT"
