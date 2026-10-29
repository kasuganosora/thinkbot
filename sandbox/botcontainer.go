package sandbox

// ============================================================================
// botContainer — per-bot 长期持久化 Docker 容器
//
// 隔离目标：一个 bot 对应一个长期运行的容器（thinkbot-bot-<botID>）+ 一个 named
// volume（thinkbot-bot-<botID>）挂载到容器内 /data。bot 的所有文件读写与命令
// 执行都通过 docker exec 在容器内完成，宿主机磁盘不落任何 bot 文件，真正隔离。
// 内置镜像的 PID 1 是 thinkbot-channel（镜像 ENTRYPOINT），不再用 sleep infinity。
//
// 生命周期：
//   - ensure()：惰性创建/启动容器（若不存在则 docker run -d，已停止则 docker start）
//   - Exec/ReadFile/WriteFile/ListDir/Mkdir/Remove：通过 docker exec 操作
//   - destroy()：docker rm -f（可选连同 volume 一起删除）
// ============================================================================

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	botsandbox "github.com/kasuganosora/thinkbot/docker/sandbox"
	"github.com/kasuganosora/thinkbot/sandbox/channel"
	"github.com/kasuganosora/thinkbot/sandbox/channelclient"
	"github.com/kasuganosora/thinkbot/util/errs"
)

// 容器内固定工作目录（agent 面向的虚拟根 /data）。
const containerWorkDir = VirtualRoot

// channel 重建退避。探针失败后的重建是破坏性动作（docker rm -f + docker run），
// 当故障是环境级的（宿主不支持容器 unix socket、镜像与守护进程协议不匹配等）时
// 重建永远修不好，若无冷却就会随调用方频率无限销毁重建容器。
const (
	channelRecreateBaseBackoff = 30 * time.Second
	channelRecreateMaxBackoff  = 10 * time.Minute
	// channelRecreateErrRunes 是缓存的最近失败原因字符数上限，避免长错误串常驻内存。
	channelRecreateErrRunes = 200
)

// channelRecreateBackoff 返回第 n 次连续失败后的冷却时长（指数退避，封顶）。
func channelRecreateBackoff(failures int) time.Duration {
	if failures <= 0 {
		return 0
	}
	d := channelRecreateBaseBackoff
	for i := 1; i < failures; i++ {
		if d >= channelRecreateMaxBackoff/2 {
			return channelRecreateMaxBackoff
		}
		d *= 2
	}
	return d
}

// botContainer 管理单个 bot 的长期容器。
type botContainer struct {
	botID     string
	container string // 容器名 thinkbot-bot-<botID>
	volume    string // named volume 名 thinkbot-bot-<botID>
	cfg       Config
	logger    *zap.SugaredLogger

	mu      sync.Mutex
	ready   bool // 容器是否已确认就绪（避免每次 exec 都探测）
	stopped bool // 用户显式停止后置位；ensure() 不会再自动 docker start

	// memoryOverride 非空时覆盖 cfg.MemoryLimit（per-bot 配置）。
	// "" 表示使用 cfg.MemoryLimit；"0" 或 "-" 表示不限制。
	memoryOverride string

	// oomElevatedMB 记录因 OOM 重试已临时提升到的内存上限（MB），0 表示尚未提升。
	// 用于在反复 OOM 时按 2 倍逐级放大（6144 → 12288 → …）直到封顶，
	// 避免每次 OOM 都只回到固定的 6144m 而陷入「OOM→重建→再 OOM」死循环。
	oomElevatedMB atomic.Int64

	// channelAddr 是 TCP 传输下宿主 127.0.0.1 的拨号地址（Docker 动态分配的端口）。
	// 只放在进程内运行时状态，不是密钥，也不写入容器环境变量。unix 传输时为空。
	channelAddr string

	// channelRecreate* 是「重建后探针仍失败」的冷却状态，受 mu 保护。
	// 冷却只抑制破坏性的「删容器 + 重建」，不影响只读探针：容器仍可能自行恢复。
	channelRecreateFails int       // 连续失败次数，决定退避档位
	channelRecreateUntil time.Time // 冷却截止时刻
	channelRecreateErr   string    // 最近一次失败原因（截断），仅用于日志
}

func newBotContainer(botID string, cfg Config, logger *zap.SugaredLogger) *botContainer {
	safe := sanitizeName(botID)
	return &botContainer{
		botID:     botID,
		container: "thinkbot-bot-" + safe,
		volume:    "thinkbot-bot-" + safe,
		cfg:       cfg,
		logger:    logger,
	}
}

// sanitizeName 把 botID 转成合法的 docker 名称片段（[a-zA-Z0-9_.-]）。
func sanitizeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '_', r == '.', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := b.String()
	if out == "" {
		out = "unknown"
	}
	return out
}

// containerState 返回容器状态："running" / "exited" / "" (不存在)。
func (c *botContainer) containerState(ctx context.Context) string {
	cmd := exec.CommandContext(ctx, "docker", "inspect",
		"--format", "{{.State.Status}}", c.container)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return "" // 不存在或 inspect 失败
	}
	return strings.TrimSpace(out.String())
}

// containerID 返回容器短 ID（12 位）。不存在返回 ""。
func (c *botContainer) containerID(ctx context.Context) string {
	cmd := exec.CommandContext(ctx, "docker", "inspect",
		"--format", "{{.Id}}", c.container)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return ""
	}
	id := strings.TrimSpace(out.String())
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// inspectNetwork 返回容器网络模式与已连接网络数量。
func (c *botContainer) inspectNetwork(ctx context.Context) (string, int, error) {
	cmd := exec.CommandContext(ctx, "docker", "inspect",
		"--format", "{{.HostConfig.NetworkMode}} {{len .NetworkSettings.Networks}}", c.container)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return "", 0, errs.Wrapf(err, "bot_container: inspect network %q: %s",
			c.container, strings.TrimSpace(errb.String()))
	}
	raw := strings.TrimSpace(out.String())
	if raw == "" {
		return "", 0, nil
	}
	mode := ""
	networks := 0
	_, _ = fmt.Sscanf(raw, "%s %d", &mode, &networks)
	return mode, networks, nil
}

// ensureNetwork 在网络启用模式下确保容器可联网。
// 兼容历史遗留：若容器以 --network none 创建，Docker 不允许直接 connect 其他网络，
// 需重建容器（复用原 named volume）。
func (c *botContainer) ensureNetwork(ctx context.Context) error {
	if c.cfg.NetworkDisabled {
		return nil
	}
	mode, networks, err := c.inspectNetwork(ctx)
	if err != nil {
		return err
	}
	if mode == "none" {
		return c.recreateForNetwork(ctx, mode, networks)
	}
	if networks > 0 {
		return nil
	}

	// 非 bridge/default 的网络模式（如 host/container）不主动改写。
	if mode != "" && mode != "default" && mode != "bridge" {
		c.logger.Warnw("skip bridge attach for non-bridge network mode",
			"container", c.container,
			"mode", mode,
			"networks", networks)
		return nil
	}

	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, "docker", "network", "connect", "bridge", c.container)
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		msg := strings.ToLower(strings.TrimSpace(out.String()))
		if strings.Contains(msg, "already exists") || strings.Contains(msg, "already connected") {
			return nil
		}
		return errs.Wrapf(err, "bot_container: connect bridge network for %q: %s",
			c.container, strings.TrimSpace(out.String()))
	}

	c.logger.Infow("bot container bridge network attached",
		"container", c.container,
		"previous_mode", mode,
		"previous_networks", networks)
	return nil
}

func (c *botContainer) recreateForNetwork(ctx context.Context, mode string, networks int) error {
	var out bytes.Buffer
	rm := exec.CommandContext(ctx, "docker", "rm", "-f", c.container)
	rm.Stdout = &out
	rm.Stderr = &out
	if err := rm.Run(); err != nil {
		return errs.Wrapf(err, "bot_container: remove legacy none-network container %q: %s",
			c.container, strings.TrimSpace(out.String()))
	}

	c.ready = false
	c.logger.Warnw("bot container recreated to recover networking",
		"container", c.container,
		"previous_mode", mode,
		"previous_networks", networks)
	return c.create(ctx)
}

// ensure 保证容器已创建并处于 running 状态（惰性、幂等）。
// 内置 channel 镜像还会探针 channel（Linux 默认 unix socket，Windows 或
// sandbox.channel.transport=http 时为宿主 127.0.0.1 上的 TCP）；失败则删容器
// 重建一次（保留数据 volume）。
func (c *botContainer) ensure(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.ensureRunningLocked(ctx); err != nil {
		return err
	}
	if err := c.ensureChannelLocked(ctx); err != nil {
		c.ready = false
		return err
	}
	return nil
}

// ensureRunningLocked 拉起容器。调用方必须已持有 c.mu。
func (c *botContainer) ensureRunningLocked(ctx context.Context) error {
	// 用户已显式停止：不再自动拉起，避免“停止后又被 agent exec 重启”的死循环。
	if c.stopped {
		return errs.New("bot_container: container is stopped by user; refusing to auto-start")
	}

	if c.ready {
		// 快速复核：仍在运行则直接返回（并补齐网络配置）。
		if c.containerState(ctx) == "running" {
			if err := c.ensureNetwork(ctx); err != nil {
				return err
			}
			return nil
		}
		c.ready = false
	}

	state := c.containerState(ctx)
	switch state {
	case "running":
		if err := c.ensureNetwork(ctx); err != nil {
			return err
		}
		c.ready = true
		return nil
	case "exited", "created", "paused":
		// 已存在但未运行 → 启动
		if err := exec.CommandContext(ctx, "docker", "start", c.container).Run(); err != nil {
			return errs.Wrapf(err, "bot_container: start existing container %q", c.container)
		}
		if err := c.ensureNetwork(ctx); err != nil {
			return err
		}
		c.ready = true
		c.logger.Infow("bot container started (existing)", "container", c.container)
		return nil
	default:
		// 不存在 → 创建
		return c.create(ctx)
	}
}

// usesChannel 为真时，这个 bot 的镜像入口是 thinkbot-channel，ensure 必须探针成功。
// 自定义镜像仍走 sleep，不在这里失败（它们没有 channel 可探针）。
func (c *botContainer) usesChannel() bool {
	return isBuiltinImage(c.cfg.Image) || imageUsesChannelPID1(c.cfg.Image)
}

func (c *botContainer) channelHostDir() string {
	root := c.cfg.ChannelSockRoot
	if root == "" {
		root = channelclient.DefaultSockRoot
	}
	return root + "/thinkbot-channel-" + sanitizeName(c.botID)
}

func (c *botContainer) channelSockFile() string {
	return c.channelHostDir() + "/channel.sock"
}

// writeChannelToken puts the derived token on Volume B as a 0600 file and
// does not pass it through the container environment. The daemon reads it
// once at startup and deletes the file.
func (c *botContainer) writeChannelToken(token string) error {
	if err := os.MkdirAll(c.channelHostDir(), 0o750); err != nil {
		return errs.Wrapf(err, "bot_container: channel socket dir %q", c.channelHostDir())
	}
	path := c.channelHostDir() + "/" + channel.TokenFileName
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(token), 0o600); err != nil {
		return errs.Wrap(err, "bot_container: write channel token")
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return errs.Wrap(err, "bot_container: commit channel token")
	}
	return nil
}

// ensureChannelLocked 探针 Health + Hello。失败则删除容器并重建一次，数据 volume 保留。
// 调用方必须已持有 c.mu。没有第二种执行路径。
func (c *botContainer) ensureChannelLocked(ctx context.Context) error {
	if !c.usesChannel() {
		return nil
	}
	transport, err := c.channelTransport()
	if err != nil {
		return err
	}
	token, err := channelclient.DeriveToken(c.cfg.ChannelSecret, c.botID)
	if err != nil {
		return errs.Wrap(err, "bot_container: channel token")
	}
	probe := func(ctx context.Context) error {
		ep, err := c.channelEndpointLocked(ctx, transport)
		if err != nil {
			return err
		}
		return channelclient.ProbeEndpoint(ctx, ep, token, c.botID)
	}
	// attempted 表示已经越过冷却闸、开始破坏性重建。窗口内被跳过的失败
	// 不能记入 noteChannelRecreateFailure：否则每次 ensure 都会 fails+1，
	// 并把 channelRecreateUntil 刷新到 now+backoff，连续探测下窗口永远到不了期。
	attempted := false
	recreate := func(ctx context.Context) error {
		// 冷却窗口内不再销毁重建。重建修不好的故障在窗口内每轮都会重演，
		// 无冷却时调用方每几秒就会销毁并重建一次容器（日志刷屏、拖垮启动）。
		if d := time.Until(c.channelRecreateUntil); d > 0 {
			c.logger.Warnw("channel recreate skipped: cooling down",
				"container", c.container, "retry_in", d.Round(time.Second),
				"consecutive_failures", c.channelRecreateFails,
				"last_error", c.channelRecreateErr)
			return errs.Newf("bot_container: channel recreate cooling down (retry in %s, %d consecutive failures): %s",
				d.Round(time.Second), c.channelRecreateFails, c.channelRecreateErr)
		}
		attempted = true
		c.logger.Warnw("channel probe failed; recreating bot container",
			"container", c.container, "data_volume", c.volume)
		if err := c.removeContainerKeepData(); err != nil {
			return err
		}
		c.ready = false
		return c.create(ctx)
	}
	if err := channelclient.Recover(ctx, probe, recreate); err != nil {
		if attempted {
			c.noteChannelRecreateFailure(err)
		}
		return errs.Wrapf(err, "bot_container: %s", c.container)
	}
	c.clearChannelRecreateBackoff()
	return nil
}

// noteChannelRecreateFailure 记录一次「破坏性重建已经尝试且仍然失败」，并抬升冷却档位。
// 调用方必须已持有 c.mu。冷却窗口内被跳过的重建不得调用本函数。
func (c *botContainer) noteChannelRecreateFailure(err error) {
	c.channelRecreateFails++
	backoff := channelRecreateBackoff(c.channelRecreateFails)
	// 只延长、不缩短。现存窗口更长时（例如外部已设更长窗口）保留原来那个。
	if next := time.Now().Add(backoff); next.After(c.channelRecreateUntil) {
		c.channelRecreateUntil = next
	}
	if err != nil {
		// 按 rune 截断，避免把多字节字符切一半留下半个乱码。
		runes := []rune(err.Error())
		if len(runes) > channelRecreateErrRunes {
			runes = runes[:channelRecreateErrRunes]
		}
		c.channelRecreateErr = string(runes)
	}
	c.logger.Warnw("channel recreate backoff armed",
		"container", c.container, "consecutive_failures", c.channelRecreateFails,
		"backoff", backoff.String())
}

// clearChannelRecreateBackoff 在探针通过后复位冷却状态。调用方必须已持有 c.mu。
func (c *botContainer) clearChannelRecreateBackoff() {
	if c.channelRecreateFails == 0 && c.channelRecreateUntil.IsZero() {
		return
	}
	c.logger.Infow("channel recovered; recreate cooldown cleared",
		"container", c.container, "previous_failures", c.channelRecreateFails)
	c.channelRecreateFails = 0
	c.channelRecreateUntil = time.Time{}
	c.channelRecreateErr = ""
}

func (c *botContainer) removeContainerKeepData() error {
	out, err := exec.Command("docker", "rm", "-f", c.container).CombinedOutput()
	if err != nil && !strings.Contains(string(out), "No such container") {
		return errs.Wrapf(err, "bot_container: remove %q for channel recreate: %s",
			c.container, strings.TrimSpace(string(out)))
	}
	c.channelAddr = ""
	c.logger.Infow("bot container removed for channel recreate",
		"container", c.container, "data_volume", c.volume)
	return nil
}

// DialChannel ensures the container and returns an authenticated session.
// Later call sites can switch file and exec traffic onto this client.
func (c *botContainer) DialChannel(ctx context.Context) (*channelclient.Client, error) {
	if !c.usesChannel() {
		return nil, errs.New("bot_container: image has no thinkbot-channel entrypoint")
	}
	if err := c.ensure(ctx); err != nil {
		return nil, err
	}
	token, err := channelclient.DeriveToken(c.cfg.ChannelSecret, c.botID)
	if err != nil {
		return nil, errs.Wrap(err, "bot_container: channel token")
	}
	c.mu.Lock()
	transport, err := c.channelTransport()
	if err != nil {
		c.mu.Unlock()
		return nil, err
	}
	ep, err := c.channelEndpointLocked(ctx, transport)
	c.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return channelclient.DialEndpoint(ctx, ep, token, c.botID)
}

// dockerRunArgs 是 `docker run -d` 的参数（不含 docker 本身）。
// 内置镜像的 ENTRYPOINT 已是 thinkbot-channel，不能再追加 sleep，否则那两个词会变成
// 守护进程的 argv。自定义镜像没有这个入口，仍用 sleep infinity 保持容器不退出。
//
// Linux 默认只挂 Volume B 上的 unix socket。TCP（Windows，或 sandbox.channel.transport=http）
// 额外把容器端口发布到宿主 127.0.0.1 的动态端口；Hello token 仍只来自 Volume B 上的一次性文件。
func (c *botContainer) dockerRunArgs(image string) ([]string, error) {
	args := []string{
		"run", "-d",
		"--name", c.container,
		"--label", "thinkbot.bot=" + c.botID,
		"--label", "thinkbot.managed=true",
		"-v", c.volume + ":" + containerWorkDir,
		"-w", containerWorkDir,
	}

	tz := c.cfg.Timezone
	if tz == "" {
		tz = "UTC"
	}
	args = append(args, "-e", "TZ="+tz)

	// 全局出口代理：注入到容器环境变量，使容器内请求统一走部署侧代理。
	if c.cfg.Proxy != "" {
		args = append(args, "-e", "HTTP_PROXY="+c.cfg.Proxy, "-e", "HTTPS_PROXY="+c.cfg.Proxy)
	}

	if mem := c.effectiveMemoryLocked(); mem != "" {
		args = append(args, "--memory", mem)
	}
	// 浏览器场景需要较大的 /dev/shm，否则 Chromium 渲染大页面必崩。
	// 见 docs/sandbox-browser-image-design.md §5（--shm-size 优先于 --disable-dev-shm-usage）。
	args = append(args, "--shm-size", "512m")
	if c.cfg.CPULimit != "" {
		args = append(args, "--cpus", c.cfg.CPULimit)
	}
	if c.cfg.NetworkDisabled {
		args = append(args, "--network", "none")
	}

	if imageUsesChannelPID1(image) {
		transport, err := channel.SelectTransport(c.cfg.ChannelTransport, runtime.GOOS)
		if err != nil {
			return nil, errs.Wrap(err, "bot_container: channel transport")
		}
		if transport == channel.TransportTCP && c.cfg.NetworkDisabled {
			return nil, errs.New("bot_container: tcp channel transport publishes on 127.0.0.1 and cannot use --network none")
		}
		args = append(args, "-v", c.channelHostDir()+":"+channelclient.ContainerSockDir)
		args = append(args, "-e", channel.EnvBotID+"="+c.botID)
		// The token is a 0600 file on Volume B, not a container environment variable.
		// THINKBOT_CHANNEL_HTTP is only the listen address. Docker forwards it to
		// a host port bound on 127.0.0.1; the client dials that loopback port.
		if transport == channel.TransportTCP {
			args = append(args,
				"-p", channel.PublishSpec(),
				"-e", channel.EnvHTTP+"="+channel.ContainerListenAddr(),
			)
		} else if uid := os.Getuid(); uid > 0 {
			args = append(args,
				"-e", channel.EnvSockUID+"="+strconv.Itoa(uid),
				"-e", channel.EnvSockGID+"="+strconv.Itoa(os.Getgid()),
			)
		}
		args = append(args,
			"--label", "thinkbot.channel.version="+channel.Version,
			"--label", "thinkbot.channel.protocol="+channel.Protocol,
			"--label", "thinkbot.channel.transport="+transport,
			image,
		)
		return args, nil
	}
	args = append(args, image, "sleep", "infinity")
	return args, nil
}

// imageUsesChannelPID1 为真时，镜像 ENTRYPOINT 是 thinkbot-channel，容器命令必须留空。
func imageUsesChannelPID1(image string) bool {
	return strings.HasPrefix(image, botsandbox.BuiltinImagePrefix)
}

// create 创建并启动长期容器。
func (c *botContainer) create(ctx context.Context) error {
	// 解析实际镜像：builtin/空 → thinkbot 按需自构建；否则原样使用（兼容预构建镜像）。
	image, err := resolveBotImage(ctx, c.cfg.Image, c.logger)
	if err != nil {
		return errs.Wrapf(err, "bot_container: resolve image for %q", c.container)
	}
	if imageUsesChannelPID1(image) {
		c.channelAddr = ""
		if _, err := c.channelTransport(); err != nil {
			return err
		}
		token, err := channelclient.DeriveToken(c.cfg.ChannelSecret, c.botID)
		if err != nil {
			return errs.Wrap(err, "bot_container: channel token")
		}
		if err := c.writeChannelToken(token); err != nil {
			return err
		}
	}

	args, err := c.dockerRunArgs(image)
	if err != nil {
		return err
	}

	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		// Offline API calls may own distinct managers for the same bot. Both can
		// inspect an absent container before either creates it; the loser should
		// attach to the winner, not fail an otherwise valid terminal/file request.
		if ctx.Err() == nil && strings.Contains(out.String(), "already in use") {
			if c.awaitConcurrentContainer(ctx, image) {
				if networkErr := c.ensureNetwork(ctx); networkErr != nil {
					return networkErr
				}
				c.ready = true
				return nil
			}
		}
		return errs.Wrapf(err, "bot_container: create container %q: %s",
			c.container, strings.TrimSpace(out.String()))
	}

	// 确保工作目录存在（named volume 首次挂载可能为空但目录已由 -w 隐含创建）。
	_ = exec.CommandContext(ctx, "docker", "exec", c.container,
		"sh", "-c", "mkdir -p "+containerWorkDir).Run()

	// 引导安装 ripgrep：sandbox_search_content 优先使用 rg（比 BusyBox grep 强大得多，
	// 支持真正的正则、PCRE2、更快）。幂等：已存在则跳过；失败（非 alpine 镜像 / 无网络）
	// 静默忽略，搜索工具会自动回退到容器内的 grep。
	_ = exec.CommandContext(ctx, "docker", "exec", c.container,
		"sh", "-c", "command -v rg >/dev/null 2>&1 || (apk add --no-cache ripgrep 2>/dev/null || apt-get install -y --no-install-recommends ripgrep 2>/dev/null) || true").Run()

	if err := c.ensureNetwork(ctx); err != nil {
		return err
	}

	c.ready = true
	c.logger.Infow("bot container created",
		"container", c.container, "volume", c.volume, "image", image)
	return nil
}

// awaitConcurrentContainer only reuses a concurrent creator's exact managed
// bot/image/volume combination. A same-name foreign container is never started
// or attached. Docker can reserve a name before inspect reports it as running.
func (c *botContainer) awaitConcurrentContainer(ctx context.Context, image string) bool {
	for attempt := 0; attempt < 20; attempt++ {
		data, err := exec.CommandContext(ctx, "docker", "inspect", "--format", "{{json .}}", c.container).Output()
		if err == nil {
			var detail struct {
				Config struct {
					Image  string
					Labels map[string]string
				}
				State  struct{ Status string }
				Mounts []struct{ Type, Name, Destination string }
			}
			if json.Unmarshal(data, &detail) != nil || detail.Config.Image != image || detail.Config.Labels["thinkbot.managed"] != "true" || detail.Config.Labels["thinkbot.bot"] != c.botID {
				return false
			}
			ownVolume := false
			for _, mount := range detail.Mounts {
				if mount.Type == "volume" && mount.Name == c.volume && mount.Destination == containerWorkDir {
					ownVolume = true
				}
			}
			if !ownVolume {
				return false
			}
			if detail.State.Status == "running" {
				return true
			}
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(100 * time.Millisecond):
		}
	}
	return false
}

// destroy 销毁容器。removeVolume 为 true 时连同持久化 volume 一起删除。
func (c *botContainer) destroy(removeVolume bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ready = false
	c.channelAddr = ""

	_ = exec.Command("docker", "rm", "-f", c.container).Run()
	c.logger.Infow("bot container destroyed", "container", c.container)

	if removeVolume {
		if err := exec.Command("docker", "volume", "rm", "-f", c.volume).Run(); err != nil {
			c.logger.Warnw("failed to remove bot volume", "volume", c.volume, "err", err)
			return errs.Wrapf(err, "bot_container: remove volume %q", c.volume)
		}
		c.logger.Infow("bot volume removed", "volume", c.volume)
	}
	return nil
}

// stop 停止容器但保留（下次 ensure 会 start）。
func (c *botContainer) stop() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stopped = true
	c.ready = false
	if err := exec.Command("docker", "stop", "-t", "10", c.container).Run(); err != nil {
		return errs.Wrapf(err, "bot_container: stop container %q", c.container)
	}
	return nil
}

// unstop 解除用户停止标记（容器启动前调用），允许 ensure() 再次自动拉起。
func (c *botContainer) unstop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stopped = false
	c.ready = false
}

// SetMemoryOverride 设置内存限制覆盖值。
// limit 为 "" 时清除覆盖（使用 cfg.MemoryLimit）；
// 为 "0" 或 "-" 时不设置内存限制；
// 其他值直接作为 docker --memory 参数。
func (c *botContainer) SetMemoryOverride(limit string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.memoryOverride = limit
}

// effectiveMemory 返回实际使用的 --memory 值。
//   - memoryOverride 为空：使用 cfg.MemoryLimit（系统默认 2G）。
//   - memoryOverride 为 "0" 或 "-"：显式不限制（返回 ""，docker run 不加 --memory）。
//   - 其他：直接使用该值（如 "4096m"）。
//
// effectiveMemoryLocked 内部版本：调用方必须已持有 c.mu。
// 拆出此版本是为了避免在持锁路径（如 ensure→create）中再次请求同一把锁，
// 否则会与自身持有的锁构成 Mutex 自死锁（Go 的 sync.Mutex 不可重入）。
func (c *botContainer) effectiveMemoryLocked() string {
	if c.memoryOverride == "" {
		return c.cfg.MemoryLimit
	}
	if c.memoryOverride == "0" || c.memoryOverride == "-" {
		return "" // 不限制
	}
	return c.memoryOverride
}

// sanitizeMemoryMB 规范化 OOM 提升的目标内存（MB）：低于起步值取起步值，超过封顶值取封顶值。
func sanitizeMemoryMB(mb int) int {
	if int64(mb) < oomRetryElevatedMB {
		return int(oomRetryElevatedMB)
	}
	if int64(mb) > oomRetryMaxMB {
		return int(oomRetryMaxMB)
	}
	return mb
}

// ElevateMemory 在【不销毁容器】的前提下，通过 `docker update --memory` 提升
// 运行中容器的内存上限，随后即可在原容器内立即重试命令。
//
// 相比原先「销毁 + 重建容器」的 OOM 恢复方式，这种做法完整保留了容器文件系统
// （/tmp、构建缓存、已安装的 toolchain 等），从而避免 bot 误判为
// 「文件系统在不同 exec 调用之间不持久化」（容器重建会清空 volume 以外的全部状态）。
//
// 行为：
//   - 先 ensure() 保证容器存活（OOM 通常只杀死容器内进程，容器仍在；极端情况下
//     init 进程也死亡时 ensure 会重建，但 named volume 不删，/data 数据仍在）。
//   - 调 `docker update --memory {mb}m` 就地放大上限；该命令在运行中的容器上生效，无需重建。
//   - 成功后把 memoryOverride / oomElevatedMB 一并更新，使后续重建（如宿主机重启）沿用已提升上限。
//
// 返回 error 表示 docker update 不可用（老旧 docker），调用方应回退到销毁+重建兜底逻辑。
// ElevateMemory 在【不销毁容器】的前提下，通过 `docker update --memory` 提升运行中容器
// 的内存上限，随后即可在原容器内立即重试命令。返回实际采用的目标上限（MB）与可能的错误。
//
// 提升策略：在「调用方建议值」与「容器当前上限的 2 倍」中取较大者（封顶 oomRetryMaxMB），
// 确保每次 OOM 重试都真正放大内存，避免容器当前已等于建议值却原地踏步（thinkbot 重启后
// 内存记忆清零时尤其常见）。若容器当前上限已不低于目标，则不做 docker update（避免降配），
// 直接返回当前值。
//
// 相比原先「销毁 + 重建容器」的 OOM 恢复方式，这种做法完整保留了容器文件系统
// （/tmp、构建缓存、已安装的 toolchain 等），从而避免 bot 误判为
// 「文件系统在不同 exec 调用之间不持久化」（容器重建会清空 volume 以外的全部状态）。
func (c *botContainer) ElevateMemory(ctx context.Context, suggestMB int) (int, error) {
	if err := c.ensure(ctx); err != nil {
		return 0, errs.Wrapf(err, "bot_container: ensure before elevate memory")
	}

	target := sanitizeMemoryMB(suggestMB)
	// 若容器当前上限已知，则取「建议值的 2 倍」与「当前上限的 2 倍」中较大者，确保逐步放大。
	if curBytes, err := c.currentMemoryBytes(ctx); err == nil && curBytes > 0 {
		curMB := int(curBytes / (1024 * 1024))
		if doubled := curMB * 2; doubled > target {
			target = sanitizeMemoryMB(doubled)
		}
		if curBytes >= int64(target)*1024*1024 {
			// 当前已足够高：同步内存记忆，不降配，直接返回当前值。
			c.mu.Lock()
			c.memoryOverride = fmt.Sprintf("%dm", curMB)
			c.oomElevatedMB.Store(int64(curMB))
			c.mu.Unlock()
			c.logger.Infow("bot container memory already >= target, skip in-place update",
				"container", c.container, "target", fmt.Sprintf("%dm", target), "actual", fmt.Sprintf("%dm", curMB))
			return curMB, nil
		}
	}

	limit := fmt.Sprintf("%dm", target)
	cmd := exec.CommandContext(ctx, "docker", "update", "--memory", limit, c.container)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return target, errs.Wrapf(err, "bot_container: docker update --memory %s %q: %s",
			limit, c.container, strings.TrimSpace(out.String()))
	}

	c.mu.Lock()
	c.memoryOverride = limit
	c.oomElevatedMB.Store(int64(target))
	c.mu.Unlock()

	c.logger.Infow("bot container memory elevated in-place (no recreate)",
		"container", c.container, "to", limit)
	return target, nil
}

// currentMemoryBytes 读取容器当前 --memory 限制（字节）；0 表示不限制（unlimited）。
func (c *botContainer) currentMemoryBytes(ctx context.Context) (int64, error) {
	cmd := exec.CommandContext(ctx, "docker", "inspect", "-f", "{{.HostConfig.Memory}}", c.container)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return 0, errs.Wrapf(err, "bot_container: inspect memory %q: %s",
			c.container, strings.TrimSpace(errb.String()))
	}
	var b int64
	_, _ = fmt.Sscanf(strings.TrimSpace(out.String()), "%d", &b)
	return b, nil
}

// execInContainer 在容器内执行一条 shell 命令（底层原语）。
func (c *botContainer) execInContainer(ctx context.Context, workDir, command string, stdin []byte) (*ExecResult, error) {
	if err := c.ensure(ctx); err != nil {
		return nil, err
	}

	wd := containerWorkDir
	if workDir != "" {
		validated, err := validatePath(containerWorkDir, workDir)
		if err != nil {
			return nil, err
		}
		wd = validated
	}

	execCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd, stop, err := dockerExecCommand(execCtx, c.container, wd, command, stdin != nil)
	if err != nil {
		return nil, err
	}
	// 复用统一的执行 + 完整性检测收尾（finalizeExecResult）。
	stuck, hard := resolveExecTimeouts(ExecRequest{}, c.cfg)
	result, err := runCommandWithStreaming(execCtx, cancel, cmd, c.cfg.MaxOutput, stdin, stuck, hard, nil, stop)
	if err != nil {
		return nil, errs.Wrapf(err, "bot_container: exec in %q", c.container)
	}
	return result, nil
}

// Exec 执行用户命令（带超时）。
func (c *botContainer) Exec(ctx context.Context, req ExecRequest) (*ExecResult, error) {
	return c.ExecStream(ctx, req, nil)
}

// ExecStream 执行用户命令并回调增量输出。
func (c *botContainer) ExecStream(ctx context.Context, req ExecRequest, onChunk func(ExecChunk)) (*ExecResult, error) {
	if req.Command == "" {
		return nil, errs.New("bot_container: command is empty")
	}
	if c.usesChannel() {
		return c.execStreamChannel(ctx, req, onChunk)
	}
	execCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stuck, hard := resolveExecTimeouts(req, c.cfg)

	if err := c.ensure(execCtx); err != nil {
		return nil, err
	}

	wd := containerWorkDir
	if req.WorkDir != "" {
		validated, err := validatePath(containerWorkDir, req.WorkDir)
		if err != nil {
			return nil, err
		}
		wd = validated
	}

	cmd, stop, err := dockerExecCommand(execCtx, c.container, wd, req.Command, false)
	if err != nil {
		return nil, err
	}

	// OOM 检测：命令前后对比容器内 cgroup 的 oom_kill 计数。
	// 这是最可靠的判定（不被管道掩盖退出码影响，如 `cmd | tee | head`）。
	snap0, _ := readContainerCgroupOOMKill(execCtx, c.container)

	result, err := runCommandWithStreaming(execCtx, cancel, cmd, c.cfg.MaxOutput, nil, stuck, hard, func(stream, chunk string) {
		if onChunk != nil {
			onChunk(ExecChunk{Stream: stream, Data: chunk})
		}
	}, stop)
	if err != nil {
		return nil, errs.Wrapf(err, "bot_container: exec in %q", c.container)
	}

	// 命令可能已超时导致 execCtx 失效，用独立短超时上下文读取 oom_kill 收尾值。
	snapCtx, snapCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer snapCancel()
	if snap1, ok := readContainerCgroupOOMKill(snapCtx, c.container); ok && snap1 > snap0 {
		result.OOMKilled = true
		result.Aborted = true
		result.Warnings = append(result.Warnings,
			fmt.Sprintf("Process was likely OOM-killed (cgroup oom_kill rose %d→%d); the result is incomplete", snap0, snap1))
	}
	result.Reliable = !result.Aborted && !result.OOMKilled

	return result, nil
}

// ReadFile 读取容器内文件。
func (c *botContainer) ReadFile(ctx context.Context, path string) ([]byte, error) {
	if c.usesChannel() {
		return c.readFileChannel(ctx, path)
	}
	validated, err := validatePath(containerWorkDir, path)
	if err != nil {
		return nil, err
	}
	if err := c.ensure(ctx); err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, "docker", "exec", c.container, "cat", validated)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, errs.Wrapf(err, "bot_container: read file %q: %s",
			path, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// WriteFile 写入容器内文件（自动创建父目录）。
func (c *botContainer) WriteFile(ctx context.Context, path string, data []byte) error {
	if c.usesChannel() {
		if c.cfg.MaxFileWrite > 0 && len(data) > c.cfg.MaxFileWrite {
			return errs.Newf("bot_container: file size %d exceeds max write %d", len(data), c.cfg.MaxFileWrite)
		}
		var limit int64
		if c.cfg.MaxFileWrite > 0 {
			limit = int64(c.cfg.MaxFileWrite)
		}
		return c.writeFileChannel(ctx, path, data, limit)
	}
	if c.cfg.MaxFileWrite > 0 && len(data) > c.cfg.MaxFileWrite {
		return errs.Newf("bot_container: file size %d exceeds max write %d",
			len(data), c.cfg.MaxFileWrite)
	}
	validated, err := validatePath(containerWorkDir, path)
	if err != nil {
		return err
	}
	if err := c.ensure(ctx); err != nil {
		return err
	}
	dir := pathDir(validated)
	quoted := shellQuote(validated)
	var command string
	if dir != "" && dir != containerWorkDir {
		command = fmt.Sprintf("mkdir -p %s && cat > %s", shellQuote(dir), quoted)
	} else {
		command = fmt.Sprintf("cat > %s", quoted)
	}
	res, err := c.execInContainer(ctx, "", command, data)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return errs.Newf("bot_container: write file %q failed: %s", path, res.Stderr)
	}
	return nil
}

// writeFileUnbounded 同 WriteFile，但跳过 MaxFileWrite 限制。
// 仅供框架内部转储（工具输出 offload）：offload 承载的是已经在内存中的输出，
// 受调用方截断上限与 PruneToolOutput 约束。
// 用户 HTTP 上传不走这里，而是 writeUserStream 流式写入。
// 普通工具写文件仍走 WriteFile，继续受 MaxFileWrite 约束。
func (c *botContainer) writeFileUnbounded(ctx context.Context, path string, data []byte) error {
	if c.usesChannel() {
		return c.writeFileChannel(ctx, path, data, 0)
	}
	validated, err := validatePath(containerWorkDir, path)
	if err != nil {
		return err
	}
	if err := c.ensure(ctx); err != nil {
		return err
	}
	dir := pathDir(validated)
	quoted := shellQuote(validated)
	var command string
	if dir != "" && dir != containerWorkDir {
		command = fmt.Sprintf("mkdir -p %s && cat > %s", shellQuote(dir), quoted)
	} else {
		command = fmt.Sprintf("cat > %s", quoted)
	}
	res, err := c.execInContainer(ctx, "", command, data)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return errs.Newf("bot_container: write tool output %q failed: %s", path, res.Stderr)
	}
	return nil
}

// writeUserStream 把 r 直接送进 `docker exec -i … cat > path` 的 stdin。
// 不读进内存，也不经过宿主磁盘（持久容器的 named volume 在主进程里不可见）。
// limit > 0 时超限返回 ErrUserUploadTooLarge，并在容器内删除半截文件。
func (c *botContainer) writeUserStream(ctx context.Context, path string, r io.Reader, limit int64) (int64, error) {
	if c.usesChannel() {
		return c.writeStreamChannel(ctx, path, r, limit)
	}
	validated, err := validatePath(containerWorkDir, path)
	if err != nil {
		return 0, err
	}
	if err := c.ensure(ctx); err != nil {
		return 0, err
	}
	dir := pathDir(validated)
	quoted := shellQuote(validated)
	var command string
	if dir != "" && dir != containerWorkDir {
		command = fmt.Sprintf("mkdir -p %s && cat > %s", shellQuote(dir), quoted)
	} else {
		command = fmt.Sprintf("cat > %s", quoted)
	}
	cmd := exec.CommandContext(ctx, "docker", "exec", "-i", c.container, "sh", "-c", command)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return 0, errs.Wrapf(err, "bot_container: stdin pipe for %q", path)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return 0, errs.Wrapf(err, "bot_container: stream file %q", path)
	}
	n, copyErr := CopyUserUpload(stdin, r, limit)
	closeErr := stdin.Close()
	waitErr := cmd.Wait()
	if errors.Is(copyErr, ErrUserUploadTooLarge) {
		c.removeStreamedFile(validated)
		return n, ErrUserUploadTooLarge
	}
	if copyErr != nil || closeErr != nil || waitErr != nil {
		c.removeStreamedFile(validated)
		switch {
		case copyErr != nil:
			return n, errs.Wrapf(copyErr, "bot_container: stream file %q", path)
		case waitErr != nil:
			return n, errs.Wrapf(waitErr, "bot_container: stream file %q: %s", path, strings.TrimSpace(stderr.String()))
		default:
			return n, errs.Wrapf(closeErr, "bot_container: stream file %q", path)
		}
	}
	return n, nil
}

func (c *botContainer) removeStreamedFile(validated string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "exec", c.container, "rm", "-f", "--", validated)
	_ = cmd.Run()
}

// ListDir 列出容器内目录。
func (c *botContainer) ListDir(ctx context.Context, path string) ([]FileEntry, error) {
	if c.usesChannel() {
		return c.listDirChannel(ctx, path)
	}
	validated, err := validatePath(containerWorkDir, path)
	if err != nil {
		return nil, err
	}
	if err := c.ensure(ctx); err != nil {
		return nil, err
	}
	formatCmd := directoryCheckScript(validated) + fmt.Sprintf(
		`for f in %s/*; do [ -e "$f" ] || continue; `+
			`if [ -d "$f" ]; then printf "%%s\td\t0\t%%s\n" "$(basename "$f")" "$(stat -c%%Y "$f" 2>/dev/null || echo 0)"; `+
			`else printf "%%s\tf\t%%s\t%%s\n" "$(basename "$f")" "$(stat -c%%s "$f" 2>/dev/null || echo 0)" "$(stat -c%%Y "$f" 2>/dev/null || echo 0)"; fi; done`,
		shellQuote(validated))
	cmd := exec.CommandContext(ctx, "docker", "exec", c.container, "sh", "-c", formatCmd)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, errs.Wrapf(directoryExecError(err, path), "bot_container: list dir %q: %s",
			path, strings.TrimSpace(stderr.String()))
	}
	return parseListOutputWithMtime(stdout.Bytes()), nil
}

// Mkdir 在容器内创建目录。
func (c *botContainer) Mkdir(ctx context.Context, path string) error {
	if c.usesChannel() {
		return c.mkdirChannel(ctx, path)
	}
	validated, err := validatePath(containerWorkDir, path)
	if err != nil {
		return err
	}
	res, err := c.execInContainer(ctx, "", fmt.Sprintf("mkdir -p %s", shellQuote(validated)), nil)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return errs.Newf("bot_container: mkdir %q failed: %s", path, res.Stderr)
	}
	return nil
}

// Stat 返回容器内单个路径的信息（用于下载前校验）。返回 exists, isDir, size。
func (c *botContainer) Stat(ctx context.Context, path string) (exists bool, isDir bool, size int64, err error) {
	if c.usesChannel() {
		return c.statChannel(ctx, path)
	}
	validated, verr := validatePath(containerWorkDir, path)
	if verr != nil {
		return false, false, 0, verr
	}
	cmd := fmt.Sprintf(
		`if [ -d %s ]; then echo "d 0"; elif [ -e %s ]; then echo "f $(stat -c%%s %s 2>/dev/null || echo 0)"; else echo "n"; fi`,
		shellQuote(validated), shellQuote(validated), shellQuote(validated))
	res, rerr := c.execInContainer(ctx, "", cmd, nil)
	if rerr != nil {
		return false, false, 0, rerr
	}
	out := strings.TrimSpace(res.Stdout)
	switch {
	case strings.HasPrefix(out, "d"):
		return true, true, 0, nil
	case strings.HasPrefix(out, "f"):
		var sz int64
		_, _ = fmt.Sscanf(out, "f %d", &sz)
		return true, false, sz, nil
	default:
		return false, false, 0, nil
	}
}

// parseListOutputWithMtime 解析 name\ttype\tsize\tmtime(unix) 四段输出。
func parseListOutputWithMtime(data []byte) []FileEntry {
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	entries := make([]FileEntry, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 4)
		if len(parts) < 2 {
			continue
		}
		entry := FileEntry{Name: parts[0]}
		if parts[1] == "d" {
			entry.IsDir = true
		}
		if len(parts) >= 3 {
			_, _ = fmt.Sscanf(parts[2], "%d", &entry.Size)
		}
		if len(parts) >= 4 {
			var unix int64
			if _, e := fmt.Sscanf(parts[3], "%d", &unix); e == nil && unix > 0 {
				entry.ModTime = time.Unix(unix, 0).UTC()
			}
		}
		entries = append(entries, entry)
	}
	return entries
}

// SnapshotInfo 描述一个容器快照（docker commit 生成的镜像）。
type SnapshotInfo struct {
	ID        string    `json:"id"`        // 镜像短 ID
	Repo      string    `json:"repo"`      // 仓库名 thinkbot-snap/<bot>
	Tag       string    `json:"tag"`       // 标签（快照名，sanitize 后）
	CreatedAt time.Time `json:"createdAt"` // 镜像创建时间
	Size      string    `json:"size"`      // 人类可读大小
}

// snapshotRepo 返回该 bot 的快照镜像仓库名。
func (c *botContainer) snapshotRepo() string {
	return "thinkbot-snap/" + sanitizeName(c.botID)
}

// snapshot 用 docker commit 把当前容器状态保存为镜像。
// tag 为快照标签（已 sanitize）；返回镜像短 ID。
func (c *botContainer) snapshot(ctx context.Context, tag string) (string, error) {
	if c.containerState(ctx) == "" {
		return "", errs.Newf("bot_container: container %q does not exist", c.container)
	}
	ref := c.snapshotRepo() + ":" + tag
	// stdout 与 stderr 必须分开：commit 的镜像 ID 从 stdout 解析，而 docker CLI
	// 可能往 stderr 打无关警告（如容器内 HOME 不可写时的 "Error loading config
	// file"），合并后会被当成 ID 前缀解析出垃圾值。
	var out, errb bytes.Buffer
	cmd := exec.CommandContext(ctx, "docker", "commit",
		"--message", "thinkbot snapshot "+tag,
		c.container, ref)
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return "", errs.Wrapf(err, "bot_container: commit snapshot %q: %s", ref, strings.TrimSpace(errb.String()))
	}
	// 取最后一行非空输出：docker commit 正常只输出一行 sha256:<64hex>。
	id := lastNonEmptyLine(out.String())
	if i := strings.Index(id, ":"); i >= 0 {
		id = id[i+1:]
	}
	if len(id) > 12 {
		id = id[:12]
	}
	return id, nil
}

// lastNonEmptyLine 返回文本中最后一行非空内容（已 TrimSpace）。
// 用于从 docker CLI 输出里取「结果行」，避免前置的进度/警告行干扰解析。
func lastNonEmptyLine(s string) string {
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if v := strings.TrimSpace(lines[i]); v != "" {
			return v
		}
	}
	return ""
}

// listSnapshots 列出该 bot 的所有快照镜像。
func (c *botContainer) listSnapshots(ctx context.Context) ([]SnapshotInfo, error) {
	repo := c.snapshotRepo()
	cmd := exec.CommandContext(ctx, "docker", "images",
		"--format", "{{.ID}}\t{{.Repository}}\t{{.Tag}}\t{{.CreatedAt}}\t{{.Size}}",
		repo)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return nil, errs.Wrapf(err, "bot_container: list snapshots: %s", strings.TrimSpace(errb.String()))
	}
	var result []SnapshotInfo
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 5)
		if len(parts) < 3 {
			continue
		}
		si := SnapshotInfo{ID: parts[0], Repo: parts[1], Tag: parts[2]}
		if len(parts) >= 5 {
			si.Size = parts[4]
			// CreatedAt 形如 "2026-07-13 14:35:56 +0800 CST"
			if t, e := time.Parse("2006-01-02 15:04:05 -0700 MST", parts[3]); e == nil {
				si.CreatedAt = t
			}
		}
		result = append(result, si)
	}
	return result, nil
}
