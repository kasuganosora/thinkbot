import os, sys

BIN = "/Users/sion/Documents/thinkbot/thinkbot"
CWD = "/Users/sion/Documents/thinkbot"
LOG = "/tmp/thinkbot.log"
ARGS = [BIN, "--docker-sandbox"]

# sandbox-c 会在运行时把本地 HTTP 代理地址注入 HTTP_PROXY/HTTPS_PROXY 环境变量，
# 但那只该给沙箱容器用；主 thinkbot 进程的 LLM 调用（连 GLM）若走这个代理会
# proxyconnect ... connection refused 失败。启动前显式 unset，避免继承到被污染的变量。
for k in ("HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy",
          "ALL_PROXY", "all_proxy", "NO_PROXY", "no_proxy"):
    os.environ.pop(k, None)

# First fork: parent exits so child is orphaned (reparented to init)
if os.fork() > 0:
    sys.exit(0)

# New session: detach from the tool's controlling terminal / process session
os.setsid()

# Second fork: ensure we are not a session leader (cannot re-acquire a tty)
if os.fork() > 0:
    sys.exit(0)

# Detach filesystem / stdio
os.chdir(CWD)
devnull = os.open(os.devnull, os.O_RDONLY)
os.dup2(devnull, 0)
with open(LOG, "ab", buffering=0) as f:
    os.dup2(f.fileno(), 1)
    os.dup2(f.fileno(), 2)

os.execv(BIN, ARGS)
