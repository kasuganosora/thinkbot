import os, sys

# 路径不再硬编码作者个人绝对路径：默认取脚本所在目录，
# 可用环境变量覆盖（部署时按需指向实际安装位置）。
SCRIPT_DIR = os.path.dirname(os.path.realpath(__file__))
BIN = os.environ.get("THINKBOT_BIN", os.path.join(SCRIPT_DIR, "thinkbot"))
CWD = os.environ.get("THINKBOT_CWD", SCRIPT_DIR)
# 日志默认写到脚本目录下的 thinkbot.log（不再写 /tmp）；可用环境变量改到别处。
LOG = os.environ.get("THINKBOT_LOG", os.path.join(SCRIPT_DIR, "thinkbot.log"))
ARGS = [BIN]

# sandbox-c 会在运行时把本地 HTTP 代理地址注入 HTTP_PROXY/HTTPS_PROXY 环境变量，
# 但那只该给沙箱容器用；主 thinkbot 进程的 LLM 调用（连 GLM）若走这个代理会
# proxyconnect ... connection refused 失败。启动前显式 unset，避免继承到被污染的变量。
for k in ("HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy",
          "ALL_PROXY", "all_proxy", "NO_PROXY", "no_proxy"):
    os.environ.pop(k, None)

# 使用项目根目录的真实数据库 thinkbot.db。不设置则 Go 默认落到 data/thinkbot.db
# 空壳（0 用户），启动后登录直接 403 bootstrap disabled。真实库始终在项目根。
os.environ["DB_PATH"] = os.path.join(CWD, "thinkbot.db")

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
