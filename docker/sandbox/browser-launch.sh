#!/bin/sh
# thinkbot 浏览器 MCP 启动器
# 以非 root 用户 `bot` 运行 xvfb + node wrapper，缩小 `--no-sandbox` + root 的提权面。
# agent 代码执行沙箱与 named volume 仍由容器默认 root 用户运行，兼容性不变。
# 环境默认保留（runuser 不重置环境），以透传 BOT_BROWSER_PROXY 等变量给 node wrapper。
set -e

# 浏览器目录/状态文件须对 bot 用户可写：thinkbot 经 WriteFile 以 root 写入状态文件
# （cat > 保留 inode owner=root），降权后的 wrapper（bot）若无可写权限则 saveState 静默失败，
# 会话结束收不回 cookie。本脚本以容器默认 root 运行，在降权前 chown 一次即可（幂等、廉价）。
if id bot >/dev/null 2>&1; then
  mkdir -p /data/.browser-profile /data/browser-screenshots
  touch /data/.browser-state.json 2>/dev/null || true
  chown -R bot:bot /data/.browser-profile /data/browser-screenshots /data/.browser-state.json 2>/dev/null || true
  chmod 777 /data/.browser-profile /data/browser-screenshots 2>/dev/null || true
  chmod 664 /data/.browser-state.json 2>/dev/null || true
fi

if command -v runuser >/dev/null 2>&1; then
  exec runuser -u bot -- env HOME=/home/bot xvfb-run -a -s "-screen 0 1920x1080x24" node /usr/local/bin/thinkbot-browser-mcp
elif command -v su >/dev/null 2>&1; then
  exec su -m bot -c 'HOME=/home/bot xvfb-run -a -s "-screen 0 1920x1080x24" node /usr/local/bin/thinkbot-browser-mcp'
else
  # 无降权工具则退回 root（仅丢失加固，保持原行为不中断服务）
  exec env HOME=/root xvfb-run -a -s "-screen 0 1920x1080x24" node /usr/local/bin/thinkbot-browser-mcp
fi
