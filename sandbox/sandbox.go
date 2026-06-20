// Package sandbox 为 Bot 提供隔离的工作空间，使 LLM 能在其中执行命令、读写文件。
//
// 设计理念（参照 Memoh 的 containerd 沙箱机制）：
//   - 平台无关的 Workspace 接口，上层无需关心底层实现
//   - Linux + Docker：容器隔离执行（真正的安全边界）
//   - Windows / 无 Docker：本地临时目录降级（进程级，无容器隔离）
//   - Factory 模式自动检测后端，接口抹平平台差异
//
// 使用方式：
//
//	sb, _ := sandbox.NewSandbox(sandbox.DefaultConfig(), logger)
//	ws, _ := sb.Create("session-1")
//	defer ws.Close()
//	result, _ := ws.Exec(ctx, sandbox.ExecRequest{Command: "echo hello"})
//	_ = ws.WriteFile(ctx, "output.txt", []byte(result.Stdout))
package sandbox

import (
	"context"
	"os/exec"
	"slices"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/util/errs"
)

// ============================================================================
// 核心接口 — 平台无关
// ============================================================================

// HealthStatus 描述工作空间或后端的健康状态。
type HealthStatus struct {
	// Healthy 是否健康可用。
	Healthy bool `json:"healthy"`
	// Backend 后端类型标识："docker" 或 "local"。
	Backend string `json:"backend"`
	// Status 状态摘要："running"、"ok"、"not-found"、"stopped"、"error" 等。
	Status string `json:"status"`
	// Message 人类可读的详细信息。
	Message string `json:"message"`
}

// Workspace 是一个沙箱工作空间，提供命令执行和文件操作能力。
// 所有路径参数均为相对于工作空间根目录的相对路径或工作空间内的绝对路径。
type Workspace interface {
	// ID 返回工作空间的唯一标识。
	ID() string

	// WorkDir 返回工作空间的工作目录（用于提示 LLM）。
	WorkDir() string

	// Exec 在工作空间中执行一条命令。
	Exec(ctx context.Context, req ExecRequest) (*ExecResult, error)

	// ReadFile 读取工作空间中的文件内容。
	ReadFile(ctx context.Context, path string) ([]byte, error)

	// WriteFile 向工作空间写入文件。
	// 如果父目录不存在会自动创建。
	WriteFile(ctx context.Context, path string, data []byte) error

	// ListDir 列出工作空间中指定目录的内容。
	ListDir(ctx context.Context, path string) ([]FileEntry, error)

	// HealthCheck 检查工作空间的健康状态（容器是否存活、目录是否存在等）。
	HealthCheck(ctx context.Context) HealthStatus

	// Close 销毁工作空间，释放所有资源。
	Close() error
}

// Sandbox 是工作空间的后端工厂。
// 不同实现（Docker / Local）实现相同的 Create 接口。
type Sandbox interface {
	// Create 创建一个新的工作空间。
	Create(id string) (Workspace, error)

	// Close 关闭后端，释放所有底层资源。
	Close() error

	// Backend 返回后端类型标识："docker" 或 "local"。
	Backend() string
}

// ============================================================================
// 类型定义
// ============================================================================

// ExecRequest 是命令执行请求。
type ExecRequest struct {
	// Command 要执行的命令（完整 shell 命令字符串）。
	Command string

	// WorkDir 命令的工作目录（相对于工作空间根）。空表示使用默认工作目录。
	WorkDir string

	// Timeout 执行超时。零值使用 Config.Timeout。
	Timeout time.Duration
}

// ExecResult 是命令执行结果。
type ExecResult struct {
	ExitCode int    `json:"exitCode"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	// Truncated 输出是否被截断（超过 MaxOutput 限制）。
	Truncated bool `json:"truncated"`
}

// FileEntry 是目录中的一个条目。
type FileEntry struct {
	Name  string `json:"name"`
	IsDir bool   `json:"isDir"`
	Size  int64  `json:"size"`
}

// ============================================================================
// 配置
// ============================================================================

// Config 是沙箱配置。
type Config struct {
	// Backend 后端选择："auto"（默认）|"docker"|"local"。
	Backend string

	// Image Docker 镜像（仅 Docker 后端使用）。
	Image string

	// BaseDir Local 后端的根目录。空则使用系统临时目录。
	BaseDir string

	// MemoryLimit Docker 容器内存限制。
	MemoryLimit string

	// CPULimit Docker 容器 CPU 限制。
	CPULimit string

	// NetworkDisabled Docker 容器是否禁用网络。
	NetworkDisabled bool

	// Timeout 命令执行的默认超时。
	Timeout time.Duration

	// MaxOutput stdout/stderr 各自的最大字节数。
	MaxOutput int

	// MaxFileWrite 单次文件写入的最大字节数。
	MaxFileWrite int

	// RequireDocker 为 true 时，auto 模式下 Docker 不可用直接返回错误，
	// 不降级到 local。适用于生产环境等不可接受无隔离的场景。
	RequireDocker bool

	// Timezone 时区标识符（IANA 格式，如 "Asia/Shanghai"）。
	// 为空时使用 "UTC" 作为容器默认时区（不影响本地进程，本地进程继承宿主时区）。
	// 影响：Docker 容器的 TZ 环境变量、本地执行进程的 TZ 环境变量。
	Timezone string
}

// DefaultConfig 返回默认配置。
func DefaultConfig() Config {
	return Config{
		Backend:         "auto",
		Image:           "alpine:latest",
		BaseDir:         "",
		MemoryLimit:     "512m",
		CPULimit:        "1.0",
		NetworkDisabled: true,
		Timeout:         30 * time.Second,
		MaxOutput:       1 << 20,  // 1 MB
		MaxFileWrite:    10 << 20, // 10 MB
		Timezone:        "UTC",
	}
}

// ============================================================================
// Factory — 后端选择
// ============================================================================

// NewSandbox 根据配置和运行环境创建合适的沙箱后端。
//
// 选择逻辑：
//  1. Backend == "docker" → 强制使用 Docker 后端（如果 Docker 不可用则返回错误）
//  2. Backend == "local" → 强制使用 Local 后端
//  3. Backend == "auto"（默认）→ Docker 可用则用 Docker，否则 Local
//     如果 RequireDocker == true 且 Docker 不可用，返回错误而非降级
func NewSandbox(cfg Config, logger *zap.SugaredLogger) (Sandbox, error) {
	if logger == nil {
		logger = zap.NewNop().Sugar()
	}
	logger = logger.With("component", "sandbox")

	backend := cfg.Backend
	if backend == "" {
		backend = "auto"
	}

	switch backend {
	case "docker":
		if !dockerAvailable() {
			return nil, errs.New("sandbox: Docker backend requested but Docker is not available")
		}
		logger.Info("sandbox backend: docker (forced)")
		return newDockerSandbox(cfg, logger)

	case "local":
		logger.Info("sandbox backend: local (forced)")
		return newLocalSandbox(cfg, logger)

	case "auto":
		if dockerAvailable() {
			logger.Info("sandbox backend: docker (auto-detect)")
			return newDockerSandbox(cfg, logger)
		}
		// Docker 不可用 → 降级或报错
		if cfg.RequireDocker {
			return nil, errs.New("sandbox: RequireDocker is set but Docker is not available")
		}
		logger.Warn("sandbox backend: local (Docker not available, fallback) — " +
			"WARNING: local mode has NO container isolation, LLM commands run directly on host")
		return newLocalSandbox(cfg, logger)

	default:
		return nil, errs.Newf("sandbox: unknown backend %q", backend)
	}
}

// ============================================================================
// 辅助函数
// ============================================================================

// dockerAvailable 探测当前环境中 Docker 是否可用。
func dockerAvailable() bool {
	_, err := exec.LookPath("docker")
	if err != nil {
		return false
	}
	// 快速探测 Docker daemon 是否运行（带 3s 超时，防止 daemon 无响应时无限阻塞）
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "version", "--format", "{{.Server.Version}}")
	if err := cmd.Run(); err != nil {
		return false
	}
	return true
}

// validatePath 校验路径安全性，防止路径逃逸（../../etc/passwd 之类）。
//
// root 是工作空间根目录的绝对路径。
// path 是用户提供的相对路径或绝对路径。
// 返回经过清理的、保证在 root 内的绝对路径。
func validatePath(root, path string) (string, error) {
	if path == "" {
		path = "."
	}

	// 统一替换为正斜杠（兼容 Windows 反斜杠）
	cleaned := strings.ReplaceAll(path, "\\", "/")

	// 拒绝包含 .. 的路径（防止目录遍历攻击）
	parts := strings.Split(cleaned, "/")
	if slices.Contains(parts, "..") {
		return "", errs.Newf("sandbox: path %q contains '..' (directory traversal not allowed)", path)
	}

	// 去掉前导 /
	cleaned = strings.TrimLeft(cleaned, "/")

	// 在 root 基础上构建绝对路径
	var full string
	if cleaned == "" || cleaned == "." {
		full = root
	} else {
		full = root + "/" + cleaned
	}

	// 清理多余的 /
	for strings.Contains(full, "//") {
		full = strings.ReplaceAll(full, "//", "/")
	}

	return full, nil
}
