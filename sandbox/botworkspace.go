package sandbox

// ============================================================================
// BotWorkspaceManager — 持久化的 per-bot 工作空间管理器
//
// 设计理念：
//   - 每个 Bot 拥有一个持久化目录（如 data/workspaces/{botID}/）
//   - 文件操作（ReadFile/WriteFile/ListDir）直接在宿主文件系统执行
//   - 命令执行通过 Docker 临时容器（volume mount）或本地进程
//   - 文件始终持久化，不因会话结束或容器销毁而丢失
//   - SoulLoader 可从 {baseDir}/{botID}/SOUL.md 加载人格定义
//
// 与 SandboxManager 的区别：
//   - SandboxManager: per-session（BotID:Channel:UserID），临时，自动清理
//   - BotWorkspaceManager: per-bot（BotID only），持久化，不自动清理
//
// Docker 执行模式：
//   docker run --rm -v {hostDir}:/workspace -w /workspace {image} sh -c {cmd}
//   每条命令一个临时容器，文件通过 volume mount 持久化在宿主机
// ============================================================================

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"

	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/util/errs"
)

// BotWorkspaceManager 管理持久化的 per-bot 工作空间。
type BotWorkspaceManager struct {
	baseDir string // 宿主文件系统根目录（如 "data/workspaces"）
	cfg     Config
	backend string // "docker" 或 "local"
	logger  *zap.SugaredLogger

	mu         sync.RWMutex
	workspaces map[string]*botWorkspace
}

// NewBotWorkspaceManager 创建持久化工作空间管理器。
//
// baseDir 是 bot 工作空间的根目录（如 "data/workspaces"），为空则使用 "data/workspaces"。
// cfg.Backend 决定命令执行的隔离方式：
//   - "auto"（默认）：Docker 可用则用 Docker，否则降级到 local
//   - "docker"：强制 Docker，不可用则报错
//   - "local"：强制本地执行
func NewBotWorkspaceManager(baseDir string, cfg Config, logger *zap.SugaredLogger) (*BotWorkspaceManager, error) {
	if logger == nil {
		logger = zap.NewNop().Sugar()
	}
	logger = logger.With("component", "bot_workspace")

	if baseDir == "" {
		baseDir = "data/workspaces"
	}

	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		return nil, errs.Wrapf(err, "bot_workspace: create base dir %q", baseDir)
	}

	backend := cfg.Backend
	if backend == "" {
		backend = "auto"
	}

	var b string
	switch backend {
	case "docker":
		if !dockerAvailable() {
			return nil, errs.New("bot_workspace: Docker backend requested but Docker is not available")
		}
		b = "docker"
		logger.Info("bot_workspace: command execution via docker (ephemeral containers)")
	case "auto":
		if dockerAvailable() {
			b = "docker"
			logger.Info("bot_workspace: command execution via docker (auto-detect)")
		} else {
			if cfg.RequireDocker {
				return nil, errs.New("bot_workspace: RequireDocker is set but Docker is not available")
			}
			b = "local"
			logger.Warn("bot_workspace: command execution via local process (no Docker isolation)")
		}
	case "local":
		b = "local"
		logger.Info("bot_workspace: command execution via local process (forced)")
	default:
		return nil, errs.Newf("bot_workspace: unknown backend %q", backend)
	}

	// Docker 可用时预拉取镜像
	if b == "docker" {
		go func() {
			cmd := exec.Command("docker", "pull", cfg.Image)
			if err := cmd.Run(); err != nil {
				logger.Debugw("docker image pull failed (non-fatal)",
					"image", cfg.Image, "err", err)
			}
		}()
	}

	// docker 后端默认启用「一 bot 一长期容器」持久化隔离模式。
	if b == "docker" && !cfg.PersistentContainer {
		cfg.PersistentContainer = true
		logger.Info("bot_workspace: persistent per-bot container mode enabled (isolated)")
	}

	return &BotWorkspaceManager{
		baseDir:    baseDir,
		cfg:        cfg,
		backend:    b,
		logger:     logger,
		workspaces: make(map[string]*botWorkspace),
	}, nil
}

// Backend 返回命令执行的后端类型。
func (m *BotWorkspaceManager) Backend() string {
	return m.backend
}

// BaseDir 返回工作空间根目录。
func (m *BotWorkspaceManager) BaseDir() string {
	return m.baseDir
}

// GetOrCreate 返回指定 bot 的持久化工作空间。
// 目录不存在时自动创建。
func (m *BotWorkspaceManager) GetOrCreate(botID string) (Workspace, error) {
	if botID == "" {
		return nil, errs.New("bot_workspace: botID is required")
	}

	// 快速路径
	m.mu.RLock()
	if ws, ok := m.workspaces[botID]; ok {
		m.mu.RUnlock()
		return ws, nil
	}
	m.mu.RUnlock()

	// 慢路径
	m.mu.Lock()
	defer m.mu.Unlock()

	if ws, ok := m.workspaces[botID]; ok {
		return ws, nil
	}

	dir := filepath.Join(m.baseDir, botID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, errs.Wrapf(err, "bot_workspace: create dir %q", dir)
	}

	ws := &botWorkspace{
		botID:   botID,
		root:    dir,
		cfg:     m.cfg,
		backend: m.backend,
		logger:  m.logger,
	}
	// docker 持久容器模式：为该 bot 绑定一个长期容器（惰性创建）。
	if m.backend == "docker" && m.cfg.PersistentContainer {
		ws.container = newBotContainer(botID, m.cfg, m.logger)
	}
	m.workspaces[botID] = ws

	m.logger.Debugw("bot workspace ready", "botID", botID, "dir", dir)

	return ws, nil
}

// BotDir 返回指定 bot 的工作空间目录路径（不存在则创建）。
// 用于 SoulLoader 等外部模块获取 bot 数据目录。
func (m *BotWorkspaceManager) BotDir(botID string) (string, error) {
	ws, err := m.GetOrCreate(botID)
	if err != nil {
		return "", err
	}
	return ws.WorkDir(), nil
}

// CloseAll 清除内存中的工作空间引用（不删除文件）。
func (m *BotWorkspaceManager) CloseAll() {
	m.mu.Lock()
	m.workspaces = make(map[string]*botWorkspace)
	m.mu.Unlock()
}

// DestroyBot 彻底销毁指定 bot 的运行时资源。
// docker 持久容器模式下：删除容器；removeData 为 true 时连同持久化 volume 一起删除。
// local 模式下：removeData 为 true 时删除宿主工作目录。
// 用于 bot 被删除时清理，确保不残留容器/卷/文件。
func (m *BotWorkspaceManager) DestroyBot(botID string, removeData bool) error {
	m.mu.Lock()
	ws := m.workspaces[botID]
	delete(m.workspaces, botID)
	m.mu.Unlock()

	// 容器模式：即使内存中无引用，也按命名规则销毁容器/卷（重启后仍可清理）。
	if m.backend == "docker" && m.cfg.PersistentContainer {
		c := newBotContainer(botID, m.cfg, m.logger)
		return c.destroy(removeData)
	}

	// local 模式：可选删除宿主目录。
	if removeData {
		dir := filepath.Join(m.baseDir, botID)
		if err := os.RemoveAll(dir); err != nil {
			return errs.Wrapf(err, "bot_workspace: remove dir %q", dir)
		}
	}
	_ = ws
	return nil
}

// StopBot 停止指定 bot 的容器（保留容器与数据，下次使用时自动重启）。
// 仅对 docker 持久容器模式有效；其他模式为 no-op。
func (m *BotWorkspaceManager) StopBot(botID string) error {
	if m.backend != "docker" || !m.cfg.PersistentContainer {
		return nil
	}
	c := newBotContainer(botID, m.cfg, m.logger)
	return c.stop()
}

// Close 释放管理器（不删除 bot 数据文件）。
func (m *BotWorkspaceManager) Close() error {
	m.CloseAll()
	return nil
}

// HealthCheckAll 检查所有活跃 bot 工作空间的健康状态。
// 返回 botID → HealthStatus 的映射。
func (m *BotWorkspaceManager) HealthCheckAll(ctx context.Context) map[string]HealthStatus {
	m.mu.RLock()
	ids := make([]string, 0, len(m.workspaces))
	for id := range m.workspaces {
		ids = append(ids, id)
	}
	m.mu.RUnlock()

	result := make(map[string]HealthStatus, len(ids))
	for _, id := range ids {
		m.mu.RLock()
		ws, ok := m.workspaces[id]
		m.mu.RUnlock()
		if !ok {
			result[id] = HealthStatus{
				Healthy: false, Backend: m.backend, Status: "evicted",
				Message: "workspace was removed",
			}
			continue
		}
		result[id] = ws.HealthCheck(ctx)
	}
	return result
}

// HealthCheck 检查指定 bot 工作空间的健康状态。
// 如果 botID 对应的工作空间尚未创建，返回 not-found 状态。
func (m *BotWorkspaceManager) HealthCheck(ctx context.Context, botID string) HealthStatus {
	m.mu.RLock()
	ws, ok := m.workspaces[botID]
	m.mu.RUnlock()
	if !ok {
		return HealthStatus{
			Healthy: false,
			Backend: m.backend,
			Status:  "not-created",
			Message: fmt.Sprintf("bot workspace %q has not been created yet", botID),
		}
	}
	return ws.HealthCheck(ctx)
}

// ContainerInfo 描述 bot 工作空间底层容器/后端的真实信息（供 API 概览展示）。
type ContainerInfo struct {
	Backend       string `json:"backend"`       // "docker" | "local"
	Persistent    bool   `json:"persistent"`    // 是否长期容器模式
	ContainerName string `json:"containerName"` // docker 容器名（persistent 模式）
	ContainerID   string `json:"containerId"`   // docker 容器短 ID（persistent 且存在时）
	Volume        string `json:"volume"`        // named volume 名（persistent 模式）
	Image         string `json:"image"`         // 镜像
	State         string `json:"state"`         // 真实容器状态：running/exited/""(未创建)；local 为 "local"
	WorkDir       string `json:"workDir"`       // 工作目录（容器内 /workspace 或宿主目录）
	Created       bool   `json:"created"`       // 工作空间是否已实例化
}

// ContainerInfo 返回指定 bot 的真实底层容器/后端信息。
// docker 持久容器模式下会通过 docker inspect 读取真实状态；local 模式返回本地信息。
func (m *BotWorkspaceManager) ContainerInfo(ctx context.Context, botID string) ContainerInfo {
	info := ContainerInfo{
		Backend:    m.backend,
		Persistent: m.backend == "docker" && m.cfg.PersistentContainer,
		Image:      m.cfg.Image,
	}
	m.mu.RLock()
	ws, ok := m.workspaces[botID]
	m.mu.RUnlock()
	if !ok {
		info.State = ""
		info.Created = false
		return info
	}
	info.Created = true
	info.WorkDir = ws.WorkDir()
	if ws.container != nil {
		info.ContainerName = ws.container.container
		info.Volume = ws.container.volume
		if dockerAvailable() {
			info.State = ws.container.containerState(ctx)
			if info.State != "" {
				info.ContainerID = ws.container.containerID(ctx)
			}
		} else {
			info.State = "docker-unavailable"
		}
	} else {
		info.State = "local"
	}
	return info
}

// StartBot 启动（或创建后启动）指定 bot 的持久容器。
// 仅对 docker 持久容器模式有效；其他模式为 no-op。
func (m *BotWorkspaceManager) StartBot(ctx context.Context, botID string) error {
	if m.backend != "docker" || !m.cfg.PersistentContainer {
		return nil
	}
	ws, err := m.GetOrCreate(botID)
	if err != nil {
		return err
	}
	bw, ok := ws.(*botWorkspace)
	if !ok || bw.container == nil {
		return nil
	}
	return bw.container.ensure(ctx)
}

// SnapshotBot 用 docker commit 将当前容器状态保存为镜像快照，返回镜像短 ID。
// 仅 docker 持久容器模式有效。
func (m *BotWorkspaceManager) SnapshotBot(ctx context.Context, botID, tag string) (string, error) {
	if m.backend != "docker" || !m.cfg.PersistentContainer {
		return "", errs.New("bot_workspace: snapshot only supported in docker persistent mode")
	}
	ws, err := m.GetOrCreate(botID)
	if err != nil {
		return "", err
	}
	bw, ok := ws.(*botWorkspace)
	if !ok || bw.container == nil {
		return "", errs.New("bot_workspace: no container for bot")
	}
	return bw.container.snapshot(ctx, tag)
}

// ListBotSnapshots 列出该 bot 的所有快照镜像。
func (m *BotWorkspaceManager) ListBotSnapshots(ctx context.Context, botID string) ([]SnapshotInfo, error) {
	if m.backend != "docker" || !m.cfg.PersistentContainer {
		return nil, nil
	}
	c := newBotContainer(botID, m.cfg, m.logger)
	return c.listSnapshots(ctx)
}

// ============================================================================
// botWorkspace — 持久化 per-bot 工作空间
// ============================================================================

// botWorkspace 实现 Workspace 接口。
//
// 文件操作直接在宿主文件系统执行（快速、可靠）。
// 命令执行通过 Docker 临时容器（隔离）或本地进程（降级）。
type botWorkspace struct {
	botID   string
	root    string // 宿主文件系统绝对路径
	cfg     Config
	backend string // "docker" 或 "local"
	logger  *zap.SugaredLogger

	// container 非 nil 时（docker 持久容器模式），所有文件/命令操作走容器内，
	// 宿主机磁盘不落 bot 文件（隔离）。为 nil 时走原有逻辑（宿主目录 + 临时容器/local）。
	container *botContainer
}

func (w *botWorkspace) ID() string      { return w.botID }
func (w *botWorkspace) WorkDir() string { return w.root }

func (w *botWorkspace) HealthCheck(ctx context.Context) HealthStatus {
	// docker 持久容器模式：检查容器是否可运行。
	if w.container != nil {
		if !dockerAvailable() {
			return HealthStatus{
				Healthy: false, Backend: "docker", Status: "docker-unavailable",
				Message: "Docker daemon is not available",
			}
		}
		state := w.container.containerState(ctx)
		switch state {
		case "running":
			return HealthStatus{
				Healthy: true, Backend: "docker", Status: "running",
				Message: fmt.Sprintf("container %q running (isolated)", w.container.container),
			}
		case "":
			return HealthStatus{
				Healthy: true, Backend: "docker", Status: "not-created",
				Message: fmt.Sprintf("container %q will be created on first use", w.container.container),
			}
		default:
			return HealthStatus{
				Healthy: true, Backend: "docker", Status: state,
				Message: fmt.Sprintf("container %q is %s (will start on demand)", w.container.container, state),
			}
		}
	}

	// 检查持久化目录是否存在
	info, err := os.Stat(w.root)
	if err != nil {
		return HealthStatus{
			Healthy: false,
			Backend: w.backend,
			Status:  "not-found",
			Message: fmt.Sprintf("workspace dir %q does not exist", w.root),
		}
	}
	if !info.IsDir() {
		return HealthStatus{
			Healthy: false,
			Backend: w.backend,
			Status:  "error",
			Message: fmt.Sprintf("workspace path %q is not a directory", w.root),
		}
	}

	// Docker 后端额外检查 daemon 是否可用
	if w.backend == "docker" {
		if !dockerAvailable() {
			return HealthStatus{
				Healthy: false,
				Backend: "docker",
				Status:  "docker-unavailable",
				Message: "Docker daemon is not available; commands will fail",
			}
		}
	}

	return HealthStatus{
		Healthy: true,
		Backend: w.backend,
		Status:  "ok",
		Message: fmt.Sprintf("workspace dir %q accessible (backend: %s)", w.root, w.backend),
	}
}

// --- 文件操作（直接宿主文件系统） ---

func (w *botWorkspace) ReadFile(ctx context.Context, path string) ([]byte, error) {
	if w.container != nil {
		return w.container.ReadFile(ctx, path)
	}
	validated, err := validatePath(w.root, path)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(validated)
	if err != nil {
		return nil, errs.Wrapf(err, "bot_workspace: read file %q", path)
	}
	return data, nil
}

func (w *botWorkspace) WriteFile(ctx context.Context, path string, data []byte) error {
	if w.container != nil {
		return w.container.WriteFile(ctx, path, data)
	}
	if w.cfg.MaxFileWrite > 0 && len(data) > w.cfg.MaxFileWrite {
		return errs.Newf("bot_workspace: file size %d exceeds max write %d",
			len(data), w.cfg.MaxFileWrite)
	}
	validated, err := validatePath(w.root, path)
	if err != nil {
		return err
	}
	dir := filepath.Dir(validated)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return errs.Wrapf(err, "bot_workspace: mkdir parent dir for %q", path)
	}
	if err := os.WriteFile(validated, data, 0o644); err != nil {
		return errs.Wrapf(err, "bot_workspace: write file %q", path)
	}
	return nil
}

func (w *botWorkspace) ListDir(ctx context.Context, path string) ([]FileEntry, error) {
	if w.container != nil {
		return w.container.ListDir(ctx, path)
	}
	validated, err := validatePath(w.root, path)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(validated)
	if err != nil {
		return nil, errs.Wrapf(err, "bot_workspace: list dir %q", path)
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

// --- 命令执行 ---

func (w *botWorkspace) Exec(ctx context.Context, req ExecRequest) (*ExecResult, error) {
	if req.Command == "" {
		return nil, errs.New("bot_workspace: command is empty")
	}

	if w.container != nil {
		return w.container.Exec(ctx, req)
	}

	timeout := req.Timeout
	if timeout == 0 {
		timeout = w.cfg.Timeout
	}
	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var cmd *exec.Cmd

	if w.backend == "docker" {
		// Docker 临时容器：docker run --rm -v {root}:/workspace -w /workspace {image} sh -c {cmd}
		args := []string{"run", "--rm"}

		// Volume mount（宿主目录 → 容器 /workspace）
		mountPath, err := filepath.Abs(w.root)
		if err != nil {
			mountPath = w.root
		}
		// Docker 在所有平台上都接受正斜杠
		mountPath = filepath.ToSlash(mountPath)
		args = append(args, "-v", mountPath+":/workspace")

		// 时区环境变量
		tz := w.cfg.Timezone
		if tz == "" {
			tz = "UTC"
		}
		args = append(args, "-e", "TZ="+tz)

		// 工作目录
		containerWorkDir := "/workspace"
		if req.WorkDir != "" {
			validated, err := validatePath("/workspace", req.WorkDir)
			if err != nil {
				return nil, err
			}
			containerWorkDir = validated
		}
		args = append(args, "-w", containerWorkDir)

		// 资源限制
		if w.cfg.MemoryLimit != "" {
			args = append(args, "--memory", w.cfg.MemoryLimit)
		}
		if w.cfg.CPULimit != "" {
			args = append(args, "--cpus", w.cfg.CPULimit)
		}
		if w.cfg.NetworkDisabled {
			args = append(args, "--network", "none")
		}

		args = append(args, w.cfg.Image, "sh", "-c", req.Command)
		cmd = exec.CommandContext(execCtx, "docker", args...)
	} else {
		// 本地执行
		targetDir := w.root
		if req.WorkDir != "" {
			validated, err := validatePath(w.root, req.WorkDir)
			if err != nil {
				return nil, err
			}
			targetDir = validated
			_ = os.MkdirAll(targetDir, 0o755)
		}

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
			return nil, errs.Wrap(err, "bot_workspace: exec command")
		}
	}

	return result, nil
}

// Close 是 no-op——持久化工作空间的文件不删除。
func (w *botWorkspace) Close() error {
	return nil
}
