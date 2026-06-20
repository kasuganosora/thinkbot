package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/util/errs"
)

// ============================================================================
// LocalSandbox — 本地进程后端（Windows 降级 / 无 Docker 环境）
//
// 每个工作空间对应一个本地临时目录，命令在主机上直接执行。
// 不提供容器级隔离，但通过路径校验确保文件操作不越界。
// ============================================================================

// localSandbox 实现 Sandbox 接口，使用本地文件系统和子进程。
type localSandbox struct {
	cfg    Config
	logger *zap.SugaredLogger
}

func newLocalSandbox(cfg Config, logger *zap.SugaredLogger) (*localSandbox, error) {
	if logger == nil {
		logger = zap.NewNop().Sugar()
	}
	logger = logger.With("component", "sandbox_local")
	// 确保 BaseDir 存在
	if cfg.BaseDir != "" {
		if err := os.MkdirAll(cfg.BaseDir, 0o755); err != nil {
			return nil, errs.Wrapf(err, "sandbox/local: create base dir %q", cfg.BaseDir)
		}
	}
	return &localSandbox{cfg: cfg, logger: logger}, nil
}

func (l *localSandbox) Backend() string { return "local" }

func (l *localSandbox) Create(id string) (Workspace, error) {
	// 创建工作空间目录
	var dir string
	if l.cfg.BaseDir != "" {
		dir = filepath.Join(l.cfg.BaseDir, id)
	} else {
		tmp, err := os.MkdirTemp("", "thinkbot-sandbox-"+id+"-*")
		if err != nil {
			return nil, errs.Wrap(err, "sandbox/local: create temp dir")
		}
		dir = tmp
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, errs.Wrapf(err, "sandbox/local: mkdir %q", dir)
	}

	l.logger.Debugw("local workspace created", "id", id, "dir", dir)

	return &localWorkspace{
		id:     id,
		root:   dir,
		cfg:    l.cfg,
		logger: l.logger,
	}, nil
}

func (l *localSandbox) Close() error {
	// Local 后端的 Close 是无状态的（各 Workspace 自行管理目录）
	return nil
}

// ============================================================================
// localWorkspace — 本地文件系统工作空间
// ============================================================================

// localWorkspace 实现 Workspace 接口，操作本地文件系统。
type localWorkspace struct {
	id     string
	root   string // 绝对路径
	cfg    Config
	logger *zap.SugaredLogger
}

func (w *localWorkspace) ID() string     { return w.id }
func (w *localWorkspace) WorkDir() string { return w.root }

func (w *localWorkspace) HealthCheck(ctx context.Context) HealthStatus {
	info, err := os.Stat(w.root)
	if err != nil {
		return HealthStatus{
			Healthy: false,
			Backend: "local",
			Status:  "not-found",
			Message: fmt.Sprintf("workspace dir %q does not exist", w.root),
		}
	}
	if !info.IsDir() {
		return HealthStatus{
			Healthy: false,
			Backend: "local",
			Status:  "error",
			Message: fmt.Sprintf("workspace path %q is not a directory", w.root),
		}
	}
	return HealthStatus{
		Healthy: true,
		Backend: "local",
		Status:  "ok",
		Message: fmt.Sprintf("workspace dir %q accessible", w.root),
	}
}

func (w *localWorkspace) Exec(ctx context.Context, req ExecRequest) (*ExecResult, error) {
	if req.Command == "" {
		return nil, errs.New("sandbox/local: command is empty")
	}

	timeout := req.Timeout
	if timeout == 0 {
		timeout = w.cfg.Timeout
	}
	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// 选择工作目录
	targetDir := w.root
	if req.WorkDir != "" {
		validated, err := validatePath(w.root, req.WorkDir)
		if err != nil {
			return nil, err
		}
		targetDir = validated
		if err := os.MkdirAll(targetDir, 0o755); err != nil {
			return nil, errs.Wrapf(err, "sandbox/local: mkdir work dir %q", targetDir)
		}
	}

	// 按 OS 选择 shell
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(execCtx, "cmd", "/c", req.Command)
	} else {
		cmd = exec.CommandContext(execCtx, "sh", "-c", req.Command)
	}
	cmd.Dir = targetDir
	// 设置 TZ 环境变量
	if w.cfg.Timezone != "" {
		cmd.Env = append(os.Environ(), "TZ="+w.cfg.Timezone)
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()

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

	if execCtx.Err() == context.DeadlineExceeded {
		result.ExitCode = -1
		result.Stderr = fmt.Sprintf("command timed out after %s\n%s", timeout, result.Stderr)
		return result, nil
	}

	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			result.ExitCode = exitErr.ExitCode()
		} else {
			return nil, errs.Wrap(err, "sandbox/local: exec command")
		}
	}

	return result, nil
}

func (w *localWorkspace) ReadFile(ctx context.Context, path string) ([]byte, error) {
	validated, err := validatePath(w.root, path)
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(validated)
	if err != nil {
		return nil, errs.Wrapf(err, "sandbox/local: read file %q", path)
	}
	return data, nil
}

func (w *localWorkspace) WriteFile(ctx context.Context, path string, data []byte) error {
	if len(data) > w.cfg.MaxFileWrite {
		return errs.Newf("sandbox/local: file size %d exceeds max write %d",
			len(data), w.cfg.MaxFileWrite)
	}

	validated, err := validatePath(w.root, path)
	if err != nil {
		return err
	}

	// 创建父目录
	dir := filepath.Dir(validated)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return errs.Wrapf(err, "sandbox/local: mkdir parent dir for %q", path)
	}

	if err := os.WriteFile(validated, data, 0o644); err != nil {
		return errs.Wrapf(err, "sandbox/local: write file %q", path)
	}

	return nil
}

func (w *localWorkspace) ListDir(ctx context.Context, path string) ([]FileEntry, error) {
	validated, err := validatePath(w.root, path)
	if err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(validated)
	if err != nil {
		return nil, errs.Wrapf(err, "sandbox/local: list dir %q", path)
	}

	result := make([]FileEntry, 0, len(entries))
	for _, entry := range entries {
		info, err := entry.Info()
		size := int64(0)
		if err == nil {
			size = info.Size()
		}
		result = append(result, FileEntry{
			Name:  entry.Name(),
			IsDir: entry.IsDir(),
			Size:  size,
		})
	}

	return result, nil
}

func (w *localWorkspace) Close() error {
	w.logger.Debugw("removing local workspace", "id", w.id, "dir", w.root)
	if err := os.RemoveAll(w.root); err != nil {
		return errs.Wrapf(err, "sandbox/local: remove workspace dir %q", w.root)
	}
	w.logger.Infow("local workspace destroyed", "id", w.id)
	return nil
}
