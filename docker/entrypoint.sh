#!/bin/sh
# thinkbot 容器入口：完成 DooD 所需的 docker.sock 组归属设置后，降权到非 root 用户运行主程序。
set -e

# DooD（Docker-outside-of-Docker）：主程序经挂载的 /var/run/docker.sock 指挥宿主 daemon。
# 宿主 docker 组 gid 不固定，启动期按 socket 实际 gid 创建/复用同名组，把运行用户加入其中；
# 若仍无法访问（权限位不足），兜底放宽 socket 权限位（部署侧可控，仅本容器内生效）。
if [ -S /var/run/docker.sock ]; then
	sock_gid=$(stat -c '%g' /var/run/docker.sock)
	if ! getent group "$sock_gid" >/dev/null 2>&1; then
		groupadd -g "$sock_gid" dockersock 2>/dev/null || true
	fi
	if command -v usermod >/dev/null 2>&1; then
		usermod -aG "$sock_gid" thinkbot 2>/dev/null || true
	fi
	# 绑定挂载的 ./data:/app/data 默认属主可能为 root，统一归属运行用户。
	if [ -d /app/data ] && [ "$(stat -c '%u' /app/data)" != "1000" ]; then
		chown -R thinkbot:thinkbot /app/data 2>/dev/null || true
	fi
	# 日志目录归属运行用户（镜像内已预建，此处兜底确保可写）。
	if [ -d /app/logs ] && [ "$(stat -c '%u' /app/logs)" != "1000" ]; then
		chown -R thinkbot:thinkbot /app/logs 2>/dev/null || true
	fi
	if [ ! -r /var/run/docker.sock ] || [ ! -w /var/run/docker.sock ]; then
		chmod 660 /var/run/docker.sock 2>/dev/null || true
	fi
fi

# 降权运行：setpriv 来自 util-linux（debian slim 自带），--init-groups 加载目标用户的附加组
# （含上面加入的 docker.sock 组），从而获得 socket 访问权。
exec setpriv --reuid=1000 --regid=1000 --init-groups /app/thinkbot "$@"
