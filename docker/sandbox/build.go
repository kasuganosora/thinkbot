// Package botsandbox 提供 thinkbot 内置浏览器沙箱镜像的按需自构建能力。
//
// 背景：per-bot 持久容器的镜像（含 chromium + patchright 的浏览器沙箱）原本依赖
// 部署者手动 `docker build`，且 thinkbot 更新后容易残留旧镜像。改为由 thinkbot 在
// 启动 bot 时按需构建：构建上下文（Dockerfile + 脚本）通过 go:embed 打进二进制，
// 因此二进制更新即自动携带最新上下文，镜像始终与当前 thinkbot 版本一致。
//
// 镜像以构建上下文的内容哈希作为 tag（thinkbot-bot:<md5>），仅当本地不存在时才
// 构建；thinkbot 更新导致任一上下文文件或 thinkbot-channel 变化 → 哈希变化 →
// 新 tag → 自动重建。生产 slim 镜像用 -tags embedchannel 把 channel 二进制嵌进来；
// 开发树没有嵌入时，用源码摘要（cmd/thinkbot-channel 与 sandbox/channel）代替。
package botsandbox

import (
	"context"
	"crypto/md5"
	"embed"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/util/errs"
)

//go:embed Dockerfile browser-mcp.js browser-launch.sh desktop-stdio.js desktop-session.sh tint2rc
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
var buildFiles = []string{"Dockerfile", "browser-mcp.js", "browser-launch.sh", "desktop-stdio.js", "desktop-session.sh", "tint2rc"}

// ErrImageBuilding 表示内置镜像正在后台构建，此刻还不可用。
//
// 首次部署时 docker build（要装 chromium）需要数分钟。若让它在 fx 的启动阶段同步
// 等待，会耗尽 fx 的启动超时（15s）并让整个进程以 exit 1 中止 —— 表现是「服务起不来」，
// 而真因只是镜像还没建好。改为后台构建并立即返回本错误：调用方据此先跳过容器创建，
// 镜像就绪后下一次调用自然拿到镜像并建容器。
//
// 调用方必须用 errors.Is 判断，不可按字符串匹配。
var ErrImageBuilding = errs.New("botsandbox: builtin bot image is still building")

// imageBuild 是一次后台构建的句柄。
type imageBuild struct {
	tag  string
	done chan struct{}

	mu     sync.Mutex
	err    error
	tail   []string // 构建输出尾部，失败时拼进错误
	sawTag bool     // stdout 里出现过成功标记
}

// builds 记录进行中/已结束的后台构建，按 tag 去重。
var (
	buildsMu sync.Mutex
	builds   = map[string]*imageBuild{}
)

// BrowserMCPScript 返回内置镜像里 /usr/local/bin/thinkbot-browser-mcp 的当前版本
// （嵌入二进制的 browser-mcp.js）。已存在的 bot 容器不会因镜像 tag 变化而重建，
// 启动浏览器前据此把新版脚本同步进容器（见 agent/bot/browser_sync.go）。
func BrowserMCPScript() ([]byte, error) {
	return buildContext.ReadFile("browser-mcp.js")
}

// BrowserLaunchScript is the container entrypoint that starts the shared X display.
func BrowserLaunchScript() ([]byte, error) {
	return buildContext.ReadFile("browser-launch.sh")
}

// DesktopScript is the RFB helper that reads the bot's X display on stdio.
func DesktopScript() ([]byte, error) {
	return buildContext.ReadFile("desktop-stdio.js")
}

// DesktopSessionScript starts the light window manager on the shared display.
func DesktopSessionScript() ([]byte, error) {
	return buildContext.ReadFile("desktop-session.sh")
}

// Tint2Config is the panel config baked into the builtin image.
func Tint2Config() ([]byte, error) {
	return buildContext.ReadFile("tint2rc")
}

// BuiltinImagePrefix 是内置镜像 tag 的前缀（thinkbot-bot:<内容哈希>）。
const BuiltinImagePrefix = "thinkbot-bot:"

// builtinTag 由构建上下文内容计算确定性镜像 tag。
// 哈希覆盖 Dockerfile、被 COPY 的脚本，以及 thinkbot-channel（嵌入字节或源码摘要）。
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
	if err := hashChannel(h); err != nil {
		return "", err
	}
	return BuiltinImagePrefix + hex.EncodeToString(h.Sum(nil)), nil
}

// hashChannel mixes the channel daemon into the image tag. Embedded bytes win
// so a slim binary (no source tree) still changes tag when the daemon changes.
func hashChannel(h io.Writer) error {
	if b := embeddedChannelBinary(); len(b) > 0 {
		_, _ = io.WriteString(h, "embedded-channel")
		_, _ = h.Write(b)
		return nil
	}
	sum, err := channelSourceDigest()
	if err != nil {
		return err
	}
	_, _ = io.WriteString(h, "channel-src")
	_, _ = h.Write(sum)
	return nil
}

// EnsureImage 确保内置浏览器镜像已构建并返回其引用。
//
// 镜像已存在时直接返回（快路径）。不存在时在后台启动 docker build 并立即返回
// ErrImageBuilding —— 构建要装 chromium、需数分钟，若同步等待会拖垮 fx 的启动超时。
// 构建完成后下一次调用即可拿到镜像；失败则后续调用返回构建错误。
// 同一 tag 的并发调用共享一次构建。
func EnsureImage(ctx context.Context, logger *zap.SugaredLogger) (string, error) {
	tag, err := builtinTag()
	if err != nil {
		return "", err
	}
	return ensureImageTag(tag, logger)
}

// ensureImageTag 是 EnsureImage 按 tag 参数化的入口（便于测试注入不同的 tag）。
func ensureImageTag(tag string, logger *zap.SugaredLogger) (string, error) {
	if imageExists(tag) {
		return tag, nil
	}

	buildsMu.Lock()
	b := builds[tag]
	if b == nil {
		b = &imageBuild{tag: tag, done: make(chan struct{})}
		builds[tag] = b
		if logger == nil {
			logger = zap.NewNop().Sugar()
		}
		go b.run(logger)
	}
	buildsMu.Unlock()

	select {
	case <-b.done:
		if err := b.result(); err != nil {
			return "", err
		}
		return tag, nil
	default:
		return "", fmt.Errorf("%w: %s", ErrImageBuilding, tag)
	}
}

// result 返回构建结果（已完成时）。
func (b *imageBuild) result() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.err
}

// buildHook 允许测试替换真实构建，避免单测真的跑 docker build。
var buildHook func(*imageBuild, *zap.SugaredLogger) error

// run 在后台执行构建，结束（成功或失败）时关闭 done。
func (b *imageBuild) run(logger *zap.SugaredLogger) {
	defer close(b.done)
	err := b.doBuild(logger)
	b.mu.Lock()
	b.err = err
	b.mu.Unlock()
	if err != nil {
		logger.Errorw("builtin bot image build failed", "image", b.tag, "err", err)
		return
	}
	logger.Infow("builtin bot image built", "image", b.tag)
}

// EnsureTag 返回内置镜像的确定性 tag（构建上下文内容哈希），不触发构建。
// 供测试、日志与预检使用。
func EnsureTag() (string, error) {
	return builtinTag()
}

// buildLineWriter 逐行消费 docker build 的输出。构建一次要几分钟，输出是唯一的
// 进度来源：逐行喂给回调，用于识别成功标记、留存错误上下文、打实时日志。
type buildLineWriter struct {
	onLine  func(string)
	pending strings.Builder
}

func (w *buildLineWriter) Write(p []byte) (int, error) {
	n := len(p)
	w.pending.Write(p)
	for {
		s := w.pending.String()
		i := strings.IndexByte(s, '\n')
		if i < 0 {
			return n, nil
		}
		w.onLine(strings.TrimRight(s[:i], "\r"))
		w.pending.Reset()
		w.pending.WriteString(s[i+1:])
	}
}

// flush 处理最后一行（构建输出未必以换行结尾）。
func (w *buildLineWriter) flush() {
	if s := w.pending.String(); s != "" {
		w.onLine(strings.TrimRight(s, "\r"))
		w.pending.Reset()
	}
}

// buildTailLines 是失败时附在错误里的输出行数上限。
const buildTailLines = 40

func (b *imageBuild) doBuild(logger *zap.SugaredLogger) error {
	if buildHook != nil {
		return buildHook(b, logger)
	}
	return b.build(logger)
}

// build 执行一次 docker build。它在独立 goroutine 里跑，不占用调用方的 ctx。
func (b *imageBuild) build(logger *zap.SugaredLogger) error {
	// 后台构建没有调用方在等，这条 Info 是「镜像在构建中」的唯一可见线索，
	// 不能省：否则构建挂起或反复失败时，日志里只剩调用方的 not ready。
	logger.Infow("building builtin bot image in background (one-time; installs chromium, may take several minutes)",
		"image", b.tag)

	dir, err := os.MkdirTemp("", "thinkbot-bot-build-")
	if err != nil {
		return errs.Wrap(err, "botsandbox: create build temp dir")
	}
	defer func() { _ = os.RemoveAll(dir) }()

	for _, name := range buildFiles {
		data, err := buildContext.ReadFile(name)
		if err != nil {
			return errs.Wrapf(err, "botsandbox: read embedded %q", name)
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			return errs.Wrapf(err, "botsandbox: write build context %q", name)
		}
	}
	if err := writeChannelBinary(dir); err != nil {
		return err
	}

	// 故意不用调用方 ctx：它通常是 fx 的启动 ctx，启动阶段一结束就 cancel，
	// 会把 docker build 连同装了一半的 chromium 一起杀掉。
	buildCtx, cancel := context.WithTimeout(context.Background(), imageBuildTimeout)
	defer cancel()

	cmd := exec.CommandContext(buildCtx, "docker", "build", "-t", b.tag, dir)

	tail := make([]string, 0, buildTailLines)
	sawSuccess := false
	out := &buildLineWriter{onLine: func(line string) {
		if line == "" {
			return
		}
		// 成功标记：经典 builder 会打印这两行之一（BuildKit 可能不打印，
		// 所以只作辅助判据，最终以镜像是否真的存在为准）。
		if strings.HasPrefix(line, "Successfully tagged") || strings.HasPrefix(line, "Successfully built") {
			sawSuccess = true
		}
		if len(tail) >= buildTailLines {
			tail = append(tail[:0], tail[1:]...)
		}
		tail = append(tail, line)
		logger.Debugw("docker build", "image", b.tag, "line", line)
	}}
	cmd.Stdout = out
	cmd.Stderr = out

	runErr := cmd.Run()
	out.flush()

	b.mu.Lock()
	b.sawTag = sawSuccess
	b.tail = append([]string(nil), tail...)
	b.mu.Unlock()

	joined := strings.Join(tail, "\n")
	if runErr != nil {
		return errs.Wrapf(runErr, "botsandbox: docker build %q failed (output tail):\n%s", b.tag, joined)
	}
	// 退出码为 0 也可能没产出镜像，以镜像是否真实存在为准。
	if !imageExists(b.tag) {
		return errs.Newf("botsandbox: docker build %q exited 0 but image is absent (output tail):\n%s",
			b.tag, joined)
	}
	if !sawSuccess {
		logger.Warnw("docker build produced the image without a success marker",
			"image", b.tag, "output_tail", joined)
	}
	return nil
}

// imageExists 判断本地是否已存在该镜像 tag。
func imageExists(tag string) bool {
	cmd := exec.Command("docker", "image", "inspect", "--format", "{{.Id}}", tag)
	return cmd.Run() == nil
}

// 以下为测试辅助：重置后台构建表，仅供单测调用。
func resetBuildsForTest() {
	buildsMu.Lock()
	defer buildsMu.Unlock()
	builds = map[string]*imageBuild{}
}
