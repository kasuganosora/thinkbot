#!/usr/bin/env python3
# thinkbot 守护启动器（macOS / Linux 通用）
# 与 daemon_launch.py 同机制：双 fork + setsid 脱离控制终端与会话，
# 使主程序在启动它的 shell（含 Bash 工具）退出后仍能常驻。
import os, sys

SCRIPT_DIR = os.path.dirname(os.path.realpath(__file__))
REPO = os.path.dirname(SCRIPT_DIR)
BIN = os.path.join(REPO, "thinkbot")
LOG = os.path.join(REPO, "logs", "launch.out")
# 本机原生部署的真实数据库是仓库根的 thinkbot.db（含 admin 等历史数据），
# 而非 #22 加固后代码默认的 data/thinkbot.db（docker 卷路径）。若未显式指定
# DB_PATH，则回退到仓库根的库，避免重部署后指向空库导致登录报 bootstrap disabled。
os.environ["DB_PATH"] = os.environ.get("DB_PATH") or os.path.join(REPO, "thinkbot.db")
ARGS = [BIN, "--docker-sandbox"]

# sandbox-c 会注入本地代理环境变量，但主程序连 GLM 若走该代理会失败；启动前清除。
for k in ("HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy",
          "ALL_PROXY", "all_proxy", "NO_PROXY", "no_proxy"):
    os.environ.pop(k, None)

# First fork
if os.fork() > 0:
    sys.exit(0)
# New session
os.setsid()
# Second fork
if os.fork() > 0:
    sys.exit(0)

os.chdir(REPO)
devnull = os.open(os.devnull, os.O_RDONLY)
os.dup2(devnull, 0)
with open(LOG, "ab", buffering=0) as f:
    os.dup2(f.fileno(), 1)
    os.dup2(f.fileno(), 2)

os.execv(BIN, ARGS)
