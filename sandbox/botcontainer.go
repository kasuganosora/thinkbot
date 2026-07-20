package sandbox

// ============================================================================
// botContainer — per-bot 长期持久化 Docker 容器
//
// 隔离目标：一个 bot 对应一个长期运行的容器（thinkbot-bot-<botID>）+ 一个 named
// volume（thinkbot-bot-<botID>）挂载到容器内 /workspace。bot 的所有文件读写与命令
// 执行都通过 docker exec 在容器内完成，宿主机磁盘不落任何 bot 文件，真正隔离。
//
// 生命周期：
//   - ensure()：惰性创建/启动容器（若不存在则 docker run -d，已停止则 docker start）
//   - Exec/ReadFile/WriteFile/ListDir/Mkdir/Remove：通过 docker exec 操作
//   - destroy()：docker rm -f（可选连同 volume 一起删除）
// ============================================================================

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/util/errs"
)

// 容器内固定工作目录（agent 面向的虚拟根 /data）。
const containerWorkDir = VirtualRoot

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
func (c *botContainer) ensure(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

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

// create 创建并启动长期容器。
func (c *botContainer) create(ctx context.Context) error {
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

	if mem := c.effectiveMemoryLocked(); mem != "" {
		args = append(args, "--memory", mem)
	}
	if c.cfg.CPULimit != "" {
		args = append(args, "--cpus", c.cfg.CPULimit)
	}
	if c.cfg.NetworkDisabled {
		args = append(args, "--network", "none")
	}

	args = append(args, c.cfg.Image, "sleep", "infinity")

	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return errs.Wrapf(err, "bot_container: create container %q: %s",
			c.container, strings.TrimSpace(out.String()))
	}

	// 确保工作目录存在（named volume 首次挂载可能为空但目录已由 -w 隐含创建）。
	_ = exec.CommandContext(ctx, "docker", "exec", c.container,
		"sh", "-c", "mkdir -p "+containerWorkDir).Run()

	if err := c.ensureNetwork(ctx); err != nil {
		return err
	}

	c.ready = true
	c.logger.Infow("bot container created",
		"container", c.container, "volume", c.volume, "image", c.cfg.Image)
	return nil
}

// destroy 销毁容器。removeVolume 为 true 时连同持久化 volume 一起删除。
func (c *botContainer) destroy(removeVolume bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ready = false

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

	args := []string{"exec", "-w", wd}
	if stdin != nil {
		args = append(args, "-i")
	}
	args = append(args, c.container, "sh", "-c", command)

	cmd := exec.CommandContext(ctx, "docker", args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()

	result := &ExecResult{Stdout: stdout.String(), Stderr: stderr.String()}
	if maxOut := c.cfg.MaxOutput; maxOut > 0 {
		if s, trunc := truncateString(result.Stdout, maxOut); trunc {
			result.Stdout = s
			result.Truncated = true
		}
		if s, trunc := truncateString(result.Stderr, maxOut); trunc {
			result.Stderr = s
			result.Truncated = true
		}
	}

	if ctx.Err() == context.DeadlineExceeded {
		result.ExitCode = -1
		return result, nil
	}
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			result.ExitCode = exitErr.ExitCode()
		} else {
			return nil, errs.Wrapf(err, "bot_container: exec in %q", c.container)
		}
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
	timeout := req.Timeout
	if timeout == 0 {
		timeout = c.cfg.Timeout
	}
	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

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

	cmd := exec.CommandContext(execCtx, "docker", "exec", "-w", wd, c.container, "sh", "-c", req.Command)
	result, err := runCommandWithStreaming(execCtx, cmd, c.cfg.MaxOutput, func(stream, chunk string) {
		if onChunk != nil {
			onChunk(ExecChunk{Stream: stream, Data: chunk})
		}
	})
	if err != nil {
		return nil, errs.Wrapf(err, "bot_container: exec in %q", c.container)
	}
	if result.ExitCode == -1 && execCtx.Err() == context.DeadlineExceeded {
		result.Stderr = fmt.Sprintf("command timed out after %s\n%s", timeout, result.Stderr)
	}
	return result, nil
}

// ReadFile 读取容器内文件。
func (c *botContainer) ReadFile(ctx context.Context, path string) ([]byte, error) {
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

// ListDir 列出容器内目录。
func (c *botContainer) ListDir(ctx context.Context, path string) ([]FileEntry, error) {
	validated, err := validatePath(containerWorkDir, path)
	if err != nil {
		return nil, err
	}
	if err := c.ensure(ctx); err != nil {
		return nil, err
	}
	formatCmd := fmt.Sprintf(
		`for f in %s/*; do [ -e "$f" ] || continue; `+
			`if [ -d "$f" ]; then printf "%%s\td\t0\t%%s\n" "$(basename "$f")" "$(stat -c%%Y "$f" 2>/dev/null || echo 0)"; `+
			`else printf "%%s\tf\t%%s\t%%s\n" "$(basename "$f")" "$(stat -c%%s "$f" 2>/dev/null || echo 0)" "$(stat -c%%Y "$f" 2>/dev/null || echo 0)"; fi; done`,
		validated)
	cmd := exec.CommandContext(ctx, "docker", "exec", c.container, "sh", "-c", formatCmd)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, errs.Wrapf(err, "bot_container: list dir %q: %s",
			path, strings.TrimSpace(stderr.String()))
	}
	return parseListOutputWithMtime(stdout.Bytes()), nil
}

// Mkdir 在容器内创建目录。
func (c *botContainer) Mkdir(ctx context.Context, path string) error {
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
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, "docker", "commit",
		"--message", "thinkbot snapshot "+tag,
		c.container, ref)
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return "", errs.Wrapf(err, "bot_container: commit snapshot %q: %s", ref, strings.TrimSpace(out.String()))
	}
	id := strings.TrimSpace(out.String())
	if i := strings.Index(id, ":"); i >= 0 {
		id = id[i+1:]
	}
	if len(id) > 12 {
		id = id[:12]
	}
	return id, nil
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
