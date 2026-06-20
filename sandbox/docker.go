package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
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
	// 预拉取镜像（非致命，拉取失败时仍继续，容器创建时可能自动拉取）
	go func() {
		cmd := exec.Command("docker", "pull", cfg.Image)
		if err := cmd.Run(); err != nil {
			logger.Debugw("docker image pull failed (non-fatal)",
				"image", cfg.Image, "err", err)
		}
	}()
	return &dockerSandbox{cfg: cfg, logger: logger}, nil
}

func (d *dockerSandbox) Backend() string { return "docker" }

func (d *dockerSandbox) Create(id string) (Workspace, error) {
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
	args = append(args, "-w", workDir, d.cfg.Image, "sleep", "infinity")

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
	if req.Command == "" {
		return nil, errs.New("sandbox/docker: command is empty")
	}

	timeout := req.Timeout
	if timeout == 0 {
		timeout = w.cfg.Timeout
	}
	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// 选择工作目录
	targetDir := w.workDir
	if req.WorkDir != "" {
		// 校验路径安全性
		validated, err := validatePath(w.workDir, req.WorkDir)
		if err != nil {
			return nil, err
		}
		targetDir = validated
	}

	cmd := exec.CommandContext(execCtx, "docker",
		"exec", "-w", targetDir, w.container, "sh", "-c", req.Command)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()

	// 判断截断
	maxOut := w.cfg.MaxOutput
	result := &ExecResult{
		Stdout: stdout.String(),
		Stderr: stderr.String(),
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

	// 获取退出码
	if execCtx.Err() == context.DeadlineExceeded {
		result.ExitCode = -1
		result.Stderr = fmt.Sprintf("command timed out after %s\n%s", timeout, result.Stderr)
		return result, nil
	}

	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			result.ExitCode = exitErr.ExitCode()
		} else {
			return nil, errs.Wrapf(err, "sandbox/docker: exec in container %q", w.container)
		}
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
