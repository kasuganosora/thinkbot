package sandbox

import (
	"context"

	"go.uber.org/zap"

	botsandbox "github.com/kasuganosora/thinkbot/docker/sandbox"
)

// isBuiltinImage 判断配置中的镜像名是否表示「使用 thinkbot 内置镜像」。
// 空值也视为内置（与 DefaultConfig 的 builtin 默认值一致）。
func isBuiltinImage(image string) bool {
	return image == "" || image == botsandbox.BuiltinImageSentinel
}

// resolveBotImage 解析 bot 容器实际使用的镜像引用。
//
// 规则：
//   - image 为空或等于 builtin 哨兵 → 确保内置浏览器镜像已按需构建，返回其哈希 tag；
//   - 其他值 → 原样返回（兼容部署者预构建镜像，如 alpine:latest 或私有仓库镜像）。
//
// 内置镜像由 thinkbot 在启动 bot 时构建，免去手动 docker build，且随二进制更新自动
// 保持一致（构建上下文已 go:embed 进二进制，tag 取上下文内容哈希）。
func resolveBotImage(ctx context.Context, image string, logger *zap.SugaredLogger) (string, error) {
	if isBuiltinImage(image) {
		return botsandbox.EnsureImage(ctx, logger)
	}
	return image, nil
}
