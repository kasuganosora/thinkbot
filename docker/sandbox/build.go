// Package botsandbox 提供 thinkbot 内置浏览器沙箱镜像的按需自构建能力。
//
// 背景：per-bot 持久容器的镜像（含 chromium + patchright 的浏览器沙箱）原本依赖
// 部署者手动 `docker build`，且 thinkbot 更新后容易残留旧镜像。改为由 thinkbot 在
// 启动 bot 时按需构建：构建上下文（Dockerfile + 脚本）通过 go:embed 打进二进制，
// 因此二进制更新即自动携带最新上下文，镜像始终与当前 thinkbot 版本一致。
//
// 镜像以构建上下文的内容哈希作为 tag（thinkbot-bot:<md5>），仅当本地不存在时才
// 构建；thinkbot 更新导致任一上下文文件变化 → 哈希变化 → 新 tag → 自动重建。
package botsandbox

import (
	"bytes"
	"context"
	"crypto/md5"
	"embed"
	"encoding/hex"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"go.uber.org/zap"
	"golang.org/x/sync/singleflight"

	"github.com/kasuganosora/thinkbot/util/errs"
)

//go:embed Dockerfile browser-mcp.js browser-launch.sh
var buildContext embed.FS

const (
	// BuiltinImageSentinel 是 sandbox.image 的特殊值：使用 thinkbot 内置浏览器沙箱
	// 镜像，由 thinkbot 在启动 bot 时按需构建（免去手动 docker build）。
	// 也兼容任意已有镜像名（如 registry/thinkbot-bot:latest）作为预构建镜像直接使用。
	BuiltinImageSentinel = "builtin"

	// imageBuildTimeout 单次镜像构建的硬上限。chromium 安装较慢，给足余量。
	imageBuildTimeout = 30 * time.Minute
)

// buildFiles 是构建上下文的全部文件（内容哈希决定镜像 tag）。
var buildFiles = []string{"Dockerfile", "browser-mcp.js", "browser-launch.sh"}

// buildGroup 去重并发构建：多个 bot 同时启动只构建一个镜像。
var buildGroup singleflight.Group

// builtinTag 由构建上下文内容计算确定性镜像 tag。
// 哈希覆盖全部烤进镜像的文件（Dockerfile + 被 COPY 的 browser-mcp.js / browser-launch.sh），
// 任一处变化都会改变 tag，确保「内容不变 → 不重复构建；内容变了 → 自动重建」。
func builtinTag() (string, error) {
	h := md5.New()
	for _, name := range buildFiles {
		f, err := buildContext.Open(name)
		if err != nil {
			return "", errs.Wrapf(err, "botsandbox: open embedded %q", name)
		}
		if _, err := io.Copy(h, f); err != nil {
			_ = f.Close()
			return "", errs.Wrapf(err, "botsandbox: hash embedded %q", name)
		}
		_ = f.Close()
	}
	return "thinkbot-bot:" + hex.EncodeToString(h.Sum(nil)), nil
}

// EnsureImage 确保内置浏览器镜像已构建并返回其引用。
// 仅当本地不存在该哈希 tag 时才执行 docker build；并发调用经 singleflight 去重。
func EnsureImage(ctx context.Context, logger *zap.SugaredLogger) (string, error) {
	tag, err := builtinTag()
	if err != nil {
		return "", err
	}
	v, err, _ := buildGroup.Do(tag, func() (any, error) {
		return ensureImageOnce(ctx, logger, tag)
	})
	if err != nil {
		return "", err
	}
	s, _ := v.(string)
	return s, nil
}

// EnsureTag 返回内置镜像的确定性 tag（构建上下文内容哈希），不触发构建。
// 供测试、日志与预检使用。
func EnsureTag() (string, error) {
	return builtinTag()
}

// ensureImageOnce 单次构建尝试（由 singleflight 保证同 tag 只跑一次）。
func ensureImageOnce(ctx context.Context, logger *zap.SugaredLogger, tag string) (string, error) {
	if logger == nil {
		logger = zap.NewNop().Sugar()
	}
	if imageExists(tag) {
		logger.Infow("builtin bot image present, skip build", "image", tag)
		return tag, nil
	}

	logger.Infow("building builtin bot image (one-time; installs chromium, may take several minutes)",
		"image", tag)

	dir, err := os.MkdirTemp("", "thinkbot-bot-build-")
	if err != nil {
		return "", errs.Wrap(err, "botsandbox: create build temp dir")
	}
	defer func() { _ = os.RemoveAll(dir) }()

	for _, name := range buildFiles {
		data, err := buildContext.ReadFile(name)
		if err != nil {
			return "", errs.Wrapf(err, "botsandbox: read embedded %q", name)
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			return "", errs.Wrapf(err, "botsandbox: write build context %q", name)
		}
	}

	buildCtx, cancel := context.WithTimeout(ctx, imageBuildTimeout)
	defer cancel()

	cmd := exec.CommandContext(buildCtx, "docker", "build", "-t", tag, dir)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		tail := out.String()
		if len(tail) > 4096 {
			tail = "..." + tail[len(tail)-4096:]
		}
		return "", errs.Wrapf(err, "botsandbox: docker build %q failed:\n%s", tag, tail)
	}

	logger.Infow("builtin bot image built", "image", tag)
	return tag, nil
}

// imageExists 判断本地是否已存在该镜像 tag。
func imageExists(tag string) bool {
	cmd := exec.Command("docker", "image", "inspect", "--format", "{{.Id}}", tag)
	return cmd.Run() == nil
}
