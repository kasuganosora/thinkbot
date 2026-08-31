#!/bin/sh
# thinkbot 容器入口：完成 DooD 所需的 docker.sock 组归属设置后，降权到非 root 用户运行主程序。
set -e

# 配置文件防呆：compose 用 bind mount 挂 ./.env:/app/.env，而宿主若不存在该文件，
# Docker 会替你【创建一个空目录】并挂进来。此时 config 层 LoadEnvFile 读目录失败，
# 但只打一条 WARN 就继续用内置默认值（sandbox.backend=auto、
# sandbox.image=alpine:latest），结果是 bot 沙箱没有浏览器、LLM Key 缺失，
# 表现为「能启动但功能残缺」——比直接启动失败更难排查。故在此 fail fast。
if [ -d /app/.env ]; then
	echo "FATAL: /app/.env 是目录而非文件。" >&2
	echo "       通常是宿主项目根缺少 .env，导致 bind mount 挂出了空目录。" >&2
	echo "       请在项目根执行：cp .env.example .env 并填入 LLM API Key，然后重新启动。" >&2
	exit 1
fi

# DooD（Docker-outside-of-Docker）：主程序经挂载的 /var/run/docker.sock 指挥宿主 daemon。
# 宿主 docker 组 gid 不固定，启动期按 socket 实际 gid 创建/复用同名组，把运行用户加入其中；
# 若仍无法访问（权限位不足），兜底放宽 socket 权限位。
# 注意：下面的 chmod 改的是【宿主】socket 文件本身（bind mount 并非副本），是跨容器的
# 宿主副作用，不是「仅本容器内生效」。
if [ -S /var/run/docker.sock ]; then
	sock_gid=$(stat -c '%g' /var/run/docker.sock)
	if ! getent group "$sock_gid" >/dev/null 2>&1; then
		groupadd -g "$sock_gid" dockersock 2>/dev/null || true
	fi
	if command -v usermod >/dev/null 2>&1; then
		usermod -aG "$sock_gid" thinkbot 2>/dev/null || true
	fi
	# 绑定挂载的数据目录默认属主可能为 root，统一归属运行用户。
	# 注意这是 chown -R：部署时应把该目录挂到独立子目录（见 docker-compose.yml
	# 的 ./data/container 与 ./logs/container），直接挂宿主数据根会连带改掉宿主
	# 上其他文件（含裸跑实例在用数据库）的属主。
	if [ -d /app/data ] && [ "$(stat -c '%u' /app/data)" != "1000" ]; then
		chown -R thinkbot:thinkbot /app/data 2>/dev/null || true
	fi
	# 日志目录归属运行用户（镜像内已预建，此处兜底确保可写）。
	if [ -d /app/logs ] && [ "$(stat -c '%u' /app/logs)" != "1000" ]; then
		chown -R thinkbot:thinkbot /app/logs 2>/dev/null || true
	fi
	# 兜底放宽 socket 权限位。仅在上一步建组+usermod 仍未拿到访问权限时才执行，
	# 且只对「属主 root:root 且权限位不足」的情形真正有效（放宽 other 位）。
	#
	# 已知局限与安全代价，部署前应知晓：
	#   1. 改的是宿主 socket 文件本身（bind mount 非副本），宿主上所有用户此后
	#      都能访问 docker daemon —— 等同于向宿主机所有用户开放宿主 root 能力。
	#   2. 若 socket 属主为 root:docker 660 而容器建组 gid 与之不匹配，chmod 660
	#      等于没有变化，此行无效（此时应让宿主 docker 组 gid 与容器内建组一致，
	#      而不是放宽权限位）。
	# 因此这行只作为「跑起来优先」的兜底保留；追求最小权限的部署可将其删除，
	# 代价是上述 root:root 场景下容器无法访问 docker.sock、沙箱功能不可用。
	if [ ! -r /var/run/docker.sock ] || [ ! -w /var/run/docker.sock ]; then
		chmod 660 /var/run/docker.sock 2>/dev/null || true
	fi
fi

# 降权运行：setpriv 来自 util-linux（debian slim 自带），--init-groups 加载目标用户的附加组
# （含上面加入的 docker.sock 组），从而获得 socket 访问权。
exec setpriv --reuid=1000 --regid=1000 --init-groups /app/thinkbot "$@"
