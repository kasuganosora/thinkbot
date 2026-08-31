package sandbox

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"

	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/util/errs"
)

// ============================================================================
// DockerSandbox — Docker 容器后端
//
// 通过 Docker CLI 管理容器，不引入 Docker Go SDK 以保持依赖精简。
// 每个工作空间对应一个独立的 Docker 容器。
// ============================================================================

// dockerSandbox 实现 Sandbox 接口，使用 Docker CLI 管理容器。
type dockerSandbox struct {
	cfg    Config
	logger *zap.SugaredLogger
}

func newDockerSandbox(cfg Config, logger *zap.SugaredLogger) (*dockerSandbox, error) {
	if logger == nil {
		logger = zap.NewNop().Sugar()
	}
	// 预拉取镜像（非致命，拉取失败时仍继续，容器创建时可能自动拉取）。
	// builtin 镜像由 thinkbot 本地构建，无需也不能从 registry 拉取。
	if !isBuiltinImage(cfg.Image) {
		go func() {
			cmd := exec.Command("docker", "pull", cfg.Image)
			if err := cmd.Run(); err != nil {
				logger.Debugw("docker image pull failed (non-fatal)",
					"image", cfg.Image, "err", err)
			}
		}()
	}
	return &dockerSandbox{cfg: cfg, logger: logger}, nil
}

func (d *dockerSandbox) Backend() string { return "docker" }

func (d *dockerSandbox) Create(id string) (Workspace, error) {
	// 解析实际镜像：builtin/空 → thinkbot 按需自构建；否则原样使用（兼容预构建镜像）。
	image, err := resolveBotImage(context.Background(), d.cfg.Image, d.logger)
	if err != nil {
		return nil, errs.Wrapf(err, "sandbox/docker: resolve image for %q", id)
	}

	containerName := "thinkbot-sandbox-" + id

	// 构建 docker run 命令
	args := []string{
		"run", "-d",
		"--name", containerName,
	}

	// 时区环境变量
	tz := d.cfg.Timezone
	if tz == "" {
		tz = "UTC"
	}
	args = append(args, "-e", "TZ="+tz)

	// 全局出口代理：注入到容器环境变量，使容器内请求统一走部署侧代理。
	if d.cfg.Proxy != "" {
		args = append(args, "-e", "HTTP_PROXY="+d.cfg.Proxy, "-e", "HTTPS_PROXY="+d.cfg.Proxy)
	}

	// 资源限制
	if d.cfg.MemoryLimit != "" {
		args = append(args, "--memory", d.cfg.MemoryLimit)
	}
	if d.cfg.CPULimit != "" {
		args = append(args, "--cpus", d.cfg.CPULimit)
	}
	if d.cfg.NetworkDisabled {
		args = append(args, "--network", "none")
	}

	// 工作目录
	const workDir = "/workspace"
	args = append(args, "-w", workDir, image, "sleep", "infinity")

	d.logger.Debugw("creating docker container",
		"container", containerName, "image", d.cfg.Image)

	cmd := exec.Command("docker", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, errs.Wrapf(err, "sandbox/docker: create container %q: %s",
			containerName, strings.TrimSpace(string(output)))
	}

	// 创建工作目录
	mkDir := exec.Command("docker", "exec", containerName,
		"mkdir", "-p", workDir)
	if err := mkDir.Run(); err != nil {
		// 容器已创建，清理
		_ = exec.Command("docker", "rm", "-f", containerName).Run()
		return nil, errs.Wrapf(err, "sandbox/docker: mkdir %s in container %q", workDir, containerName)
	}

	d.logger.Infow("docker container created",
		"container", containerName, "id", id)

	return &dockerWorkspace{
		id:        id,
		container: containerName,
		workDir:   workDir,
		cfg:       d.cfg,
		logger:    d.logger,
	}, nil
}

func (d *dockerSandbox) Close() error {
	// Docker 后端的 Close 是无状态的（容器由各 Workspace 自行管理）
	return nil
}

// ============================================================================
// dockerWorkspace — Docker 容器内的工作空间
// ============================================================================

// dockerWorkspace 实现 Workspace 接口，所有操作通过 docker exec 在容器内执行。
type dockerWorkspace struct {
	id        string
	container string
	workDir   string
	cfg       Config
	logger    *zap.SugaredLogger
}

func (w *dockerWorkspace) ID() string      { return w.id }
func (w *dockerWorkspace) WorkDir() string { return w.workDir }

func (w *dockerWorkspace) HealthCheck(ctx context.Context) HealthStatus {
	// docker inspect --format '{{.State.Status}}' {container}
	cmd := exec.CommandContext(ctx, "docker", "inspect",
		"--format", "{{.State.Status}}", w.container)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		errStr := strings.TrimSpace(stderr.String())
		if strings.Contains(errStr, "No such") || strings.Contains(errStr, "not found") {
			return HealthStatus{
				Healthy: false,
				Backend: "docker",
				Status:  "not-found",
				Message: fmt.Sprintf("container %q does not exist", w.container),
			}
		}
		return HealthStatus{
			Healthy: false,
			Backend: "docker",
			Status:  "error",
			Message: fmt.Sprintf("inspect failed: %s", errStr),
		}
	}

	state := strings.TrimSpace(stdout.String())
	healthy := state == "running"
	return HealthStatus{
		Healthy: healthy,
		Backend: "docker",
		Status:  state,
		Message: fmt.Sprintf("container %q is %s", w.container, state),
	}
}

func (w *dockerWorkspace) Exec(ctx context.Context, req ExecRequest) (*ExecResult, error) {
	return w.ExecStream(ctx, req, nil)
}

func (w *dockerWorkspace) ExecStream(ctx context.Context, req ExecRequest, onChunk func(ExecChunk)) (*ExecResult, error) {
	if req.Command == "" {
		return nil, errs.New("sandbox/docker: command is empty")
	}

	execCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stuck, hard := resolveExecTimeouts(req, w.cfg)

	// 选择工作目录
	targetDir := w.workDir
	if req.WorkDir != "" {
		validated, err := validatePath(w.workDir, req.WorkDir)
		if err != nil {
			return nil, err
		}
		targetDir = validated
	}

	cmd := exec.CommandContext(execCtx, "docker",
		"exec", "-w", targetDir, w.container, "sh", "-c", req.Command)

	result, err := runCommandWithStreaming(execCtx, cancel, cmd, w.cfg.MaxOutput, nil, stuck, hard, func(stream, chunk string) {
		if onChunk != nil {
			onChunk(ExecChunk{Stream: stream, Data: chunk})
		}
	})
	if err != nil {
		return nil, errs.Wrapf(err, "sandbox/docker: exec in container %q", w.container)
	}
	return result, nil
}

func (w *dockerWorkspace) ReadFile(ctx context.Context, path string) ([]byte, error) {
	validated, err := validatePath(w.workDir, path)
	if err != nil {
		return nil, err
	}

	cmd := exec.CommandContext(ctx, "docker", "exec", w.container, "cat", validated)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, errs.Wrapf(err, "sandbox/docker: read file %q: %s",
			path, strings.TrimSpace(stderr.String()))
	}

	return stdout.Bytes(), nil
}

func (w *dockerWorkspace) WriteFile(ctx context.Context, path string, data []byte) error {
	if len(data) > w.cfg.MaxFileWrite {
		return errs.Newf("sandbox/docker: file size %d exceeds max write %d",
			len(data), w.cfg.MaxFileWrite)
	}

	validated, err := validatePath(w.workDir, path)
	if err != nil {
		return err
	}

	// 创建父目录
	dir := pathDir(validated)
	if dir != w.workDir && dir != "" {
		mkdirCmd := exec.CommandContext(ctx, "docker", "exec", w.container,
			"mkdir", "-p", dir)
		if err := mkdirCmd.Run(); err != nil {
			return errs.Wrapf(err, "sandbox/docker: mkdir parent dir %q", dir)
		}
	}

	// 通过 stdin 写入文件内容（路径用单引号转义防注入）
	quotedPath := shellQuote(validated)
	cmd := exec.CommandContext(ctx, "docker", "exec", "-i", w.container,
		"sh", "-c", fmt.Sprintf("cat > %s", quotedPath))
	cmd.Stdin = bytes.NewReader(data)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return errs.Wrapf(err, "sandbox/docker: write file %q: %s",
			path, strings.TrimSpace(stderr.String()))
	}

	return nil
}

func (w *dockerWorkspace) ListDir(ctx context.Context, path string) ([]FileEntry, error) {
	validated, err := validatePath(w.workDir, path)
	if err != nil {
		return nil, err
	}

	// 使用 stat 格式输出 name/isDir/size
	// 格式: {name}\t{type}\t{size}
	formatCmd := fmt.Sprintf(
		`for f in %s/*; do [ -e "$f" ] || continue; `+
			`if [ -d "$f" ]; then printf "%%s\td\t0\n" "$(basename "$f")"; `+
			`else printf "%%s\tf\t%%s\n" "$(basename "$f")" "$(stat -c%%s "$f" 2>/dev/null || echo 0)"; fi; done`,
		validated)

	cmd := exec.CommandContext(ctx, "docker", "exec", w.container, "sh", "-c", formatCmd)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, errs.Wrapf(err, "sandbox/docker: list dir %q: %s",
			path, strings.TrimSpace(stderr.String()))
	}

	return parseListOutput(stdout.Bytes()), nil
}

func (w *dockerWorkspace) Close() error {
	w.logger.Debugw("destroying docker container", "container", w.container)

	// 先 stop（10s 宽限期），再 rm
	stopCmd := exec.Command("docker", "stop", "-t", "10", w.container)
	_ = stopCmd.Run()

	rmCmd := exec.Command("docker", "rm", "-f", w.container)
	if err := rmCmd.Run(); err != nil {
		w.logger.Warnw("failed to remove docker container",
			"container", w.container, "err", err)
		return errs.Wrapf(err, "sandbox/docker: remove container %q", w.container)
	}

	w.logger.Infow("docker container destroyed", "container", w.container)
	return nil
}

// ============================================================================
// 共享辅助函数
// ============================================================================

// pathDir 返回路径的父目录（POSIX 风格）。
func pathDir(path string) string {
	idx := strings.LastIndex(path, "/")
	if idx <= 0 {
		return ""
	}
	return path[:idx]
}

// shellQuote 用单引号包裹路径并转义内部单引号，防止 shell 注入。
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// runCommandWithStreaming 执行命令并可选回调 stdout/stderr 增量。
// stdin 非空时作为命令标准输入（用于 write_file 等场景）；onChunk 为 nil 时不回调。
// cancel 由调用方持有（context.WithCancel 的 cancelFunc），卡死看门狗判定命令卡死或
// 超硬上限时调用它终止命令；进程组看门狗（见下）会随之清理整个管道子孙进程。
// stuckTimeout / hardTimeout 分别为卡死看门狗阈值与硬上限兜底。
// 收尾处调用 finalizeExecResult 填充完整性 / 可信度信号（退出码 / 卡死 / 输出文本特征）。
func runCommandWithStreaming(ctx context.Context, cancel context.CancelFunc, cmd *exec.Cmd, maxOut int, stdin []byte, stuckTimeout, hardTimeout time.Duration, onChunk func(stream, chunk string)) (*ExecResult, error) {
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	// 关键：必须在 Start() 之前设置进程组，否则 SysProcAttr 不生效。
	// 进程组看门狗依赖它：ctx 取消时 syscall.Kill(-pid) 才能连带杀掉
	// sh -c "cmd | head" 的全部子孙进程，避免子进程持管道写端导致
	// cmd.Wait() 永久阻塞（即「执行中」永不停的根因）。
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	} else {
		cmd.SysProcAttr.Setpgid = true
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	// 命令启动时刻与最后一次「有输出」时刻（看门狗心跳）。
	startTime := time.Now()
	var lastActivity atomic.Int64
	lastActivity.Store(startTime.UnixNano())
	// 看门狗判定原因："stuck"(卡死) / "hard"(硬上限) / ""(正常结束或外部取消)。
	var stuckReason atomic.Value
	stuckReason.Store("")

	// 进程组看门狗：当 ctx 被取消（卡死看门狗触发 / 客户端断开 / 主动 abort）时，
	// 杀掉整个进程组而非仅直接子进程。配合上方 Setpgid（已在 Start 前设置），
	// kill(-pid) 可连带清理 sh -c "cmd | head" 的全部子孙，避免子进程持管道
	// 写端导致 cmd.Wait() 永久阻塞 —— 这是「执行中」永不停的根因。
	watchDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			if cmd.Process != nil {
				// 先杀进程组，再兜底杀直接子进程。
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				_ = cmd.Process.Kill()
			}
		case <-watchDone:
		}
	}()
	defer close(watchDone)

	// 卡死看门狗：真正判断命令「是否真的卡住」，而非固定超时一刀切。
	//   - 只要命令持续产生输出（哪怕缓慢），lastActivity 不断刷新，就不会被杀；
	//   - 连续 stuckTimeout 无任何输出、且已超过启动宽限期、且进程仍存活
	//     （kill -0 成功）→ 判定卡死，cancel() 终止；
	//   - 超过 hardTimeout（无论是否有输出）→ 强制终止，作为最终兜底。
	// 注意：命令已正常结束的情况，readPipe 会读到 EOF、cmd.Wait 自然返回，
	// 不会触发本看门狗（进程已退出，kill -0 失败）。
	// 启动宽限期：取 stuckTimeout 的一半、上限 maxStartupGrace——避免启动加载阶段
	// （前若干秒无输出）被误杀，同时不影响较短阈值的判定灵敏度。
	startupGrace := stuckTimeout / 2
	if startupGrace > maxStartupGrace {
		startupGrace = maxStartupGrace
	}
	stuckWatchDone := make(chan struct{})
	go func() {
		ticker := time.NewTicker(watchdogTick)
		defer ticker.Stop()
		for {
			select {
			case <-stuckWatchDone:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				now := time.Now()
				elapsed := now.Sub(startTime)
				// 硬上限兜底：单条命令总时长不可超过 hardTimeout。
				if elapsed > hardTimeout {
					stuckReason.Store("hard")
					cancel()
					return
				}
				// 卡死判定：已过启动宽限期，且连续 stuckTimeout 无输出、进程仍存活。
				if elapsed > startupGrace &&
					now.UnixNano()-lastActivity.Load() > int64(stuckTimeout) &&
					cmd.Process != nil {
					if err := cmd.Process.Signal(syscall.Signal(0)); err == nil {
						stuckReason.Store("stuck")
						cancel()
						return
					}
				}
			}
		}
	}()
	defer close(stuckWatchDone)

	// 前端保活心跳：只要命令还在运行，就周期性地向前端发「活着」信号，
	// 避免前端的卡死看门狗（默认 3 分钟无更新即报「连接可能已中断」）把
	// 「编译慢 / 长时间无输出但仍在跑」的命令误杀。仅在命令「安静」（距上次
	// 真实输出已超过一个心跳间隔）时才发，避免与真实输出 chunk 重复刷屏。
	// 心跳走 onChunk("heartbeat", "")：上游工具层转成不携带 chunk 的 progress
	// 事件，前端收到后仅刷新「存活」状态、不污染输出。
	heartbeatDone := make(chan struct{})
	go func() {
		ticker := time.NewTicker(heartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-heartbeatDone:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				last := time.Unix(0, lastActivity.Load())
				if time.Since(last) >= heartbeatInterval {
					if onChunk != nil {
						onChunk("heartbeat", "")
					}
				}
			}
		}
	}()
	defer close(heartbeatDone)

	var stdoutBuf, stderrBuf bytes.Buffer
	var mu sync.Mutex
	readPipe := func(stream string, r io.Reader) {
		reader := bufio.NewReader(r)
		buf := make([]byte, 2048)
		for {
			n, readErr := reader.Read(buf)
			if n > 0 {
				chunk := string(buf[:n])
				// 刷新看门狗心跳：有输出即视为「活着 / 有进展」，不判卡死。
				lastActivity.Store(time.Now().UnixNano())
				mu.Lock()
				if stream == "stderr" {
					stderrBuf.WriteString(chunk)
				} else {
					stdoutBuf.WriteString(chunk)
				}
				mu.Unlock()
				if onChunk != nil {
					onChunk(stream, chunk)
				}
			}
			if readErr != nil {
				if errors.Is(readErr, io.EOF) {
					return
				}
				return
			}
		}
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		readPipe("stdout", stdoutPipe)
	}()
	go func() {
		defer wg.Done()
		readPipe("stderr", stderrPipe)
	}()

	waitErr := cmd.Wait()
	wg.Wait()

	result := &ExecResult{
		Stdout: stdoutBuf.String(),
		Stderr: stderrBuf.String(),
	}
	if maxOut > 0 {
		if s, trunc := truncateString(result.Stdout, maxOut); trunc {
			result.Stdout = s
			result.Truncated = true
		}
		if s, trunc := truncateString(result.Stderr, maxOut); trunc {
			result.Stderr = s
			result.Truncated = true
		}
	}

	// 卡死看门狗 / 硬上限触发：判定后 cancel() 已被调用，进程组看门狗随之清理
	// 整个管道，cmd.Wait() 返回。此时以看门狗原因为准，不再依赖 ctx.Err()。
	if reason := stuckReason.Load().(string); reason != "" {
		result.ExitCode = -1
		var label string
		if reason == "stuck" {
			label = fmt.Sprintf("命令被卡死看门狗终止：连续 %s 无输出且无进展\n", stuckTimeout)
		} else {
			label = fmt.Sprintf("命令超过硬上限 %s 被强制终止\n", hardTimeout)
		}
		result.Stderr = label + result.Stderr
	} else if waitErr != nil {
		if exitErr, ok := waitErr.(*exec.ExitError); ok {
			result.ExitCode = exitErr.ExitCode()
		} else {
			return nil, waitErr
		}
	}

	// 填充完整性 / 可信度信号（退出码 / 卡死 / 输出文本特征）。
	// cgroup OOM 由调用方在 ExecStream 中前后对比后叠加到 OOMKilled。
	finalizeExecResult(result, stuckReason.Load().(string))
	return result, nil
}

// truncateString 安全截断字符串到 maxBytes，确保不截断多字节 UTF-8 字符。
func truncateString(s string, maxBytes int) (string, bool) {
	if maxBytes <= 0 || len(s) <= maxBytes {
		return s, false
	}
	// 回退到最后一个完整 UTF-8 字符边界
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
}

// parseListOutput 解析 ListDir 的 \t 分隔输出。
// 格式: name\ttype\tsize (type: 'd'=dir, 'f'=file)
func parseListOutput(data []byte) []FileEntry {
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	entries := make([]FileEntry, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 3)
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
		entries = append(entries, entry)
	}
	return entries
}
