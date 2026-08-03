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
	"os"
	"os/exec"
	"path/filepath"
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

	// Timeout 执行超时（硬上限兜底）。零值使用 Config.Timeout。
	// 注意：单条命令不再用固定超时一刀切地杀掉——真正决定是否终止的是
	// 卡死看门狗（见 StuckTimeout）。本字段作为「硬上限」：即便命令一直在输出，
	// 超过它也会被强制终止，防止无限挂起。bot 也可显式传此值覆盖。
	Timeout time.Duration

	// StuckTimeout 卡死看门狗阈值（可选覆盖）。零值使用 Config.StuckTimeout。
	// 命令连续无 stdout/stderr 输出超过该时长，且已超过启动宽限期、进程仍存活，
	// 则判定为「卡死（无进展）」并终止。只要命令持续产生输出（哪怕缓慢），就不会被杀；
	// 只有真正卡住无进展时才终止。这正是区分「编译慢」与「死锁卡死」的关键。
	StuckTimeout time.Duration
}

// ExecResult 是命令执行结果。
type ExecResult struct {
	ExitCode int    `json:"exitCode"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	// Truncated 输出是否被截断（超过 MaxOutput 限制）。仅表示「输出超长被截断，
	// 命令已完整执行」，不表示「命令没跑完」。
	Truncated bool `json:"truncated"`

	// —— 完整性 / 可信度信号（shell 工具结果加固，见 docs/shell_reliable_result_design.md）——
	// Reliable 命令是否正常且完整地执行（默认 true；命中 OOM / 信号杀 / 超时 / 异常文本则为 false）。
	Reliable bool `json:"reliable"`
	// Aborted 命令是否中途失败（OOM / 被信号杀死 / 超时）。
	Aborted bool `json:"aborted"`
	// OOMKilled 执行期间 cgroup oom_kill 计数是否增加（docker/local 后端在支持时填充）。
	OOMKilled bool `json:"oomKilled"`
	// Warnings 人类可读的不可信原因，回传给 LLM。
	Warnings []string `json:"warnings,omitempty"`
}

// ExecChunk 是命令执行中的流式输出片段。
type ExecChunk struct {
	// Stream 为输出流："stdout" | "stderr" 为真实输出；"heartbeat" 为保活心跳
	// （不携带数据，仅用于向前端证明命令仍在运行，避免前端卡死看门狗误报）。
	Stream string `json:"stream"`
	Data   string `json:"data"`
}

// StreamWorkspace 是可选接口：支持命令流式输出。
// 不实现该接口的 Workspace 仍可通过 Exec 正常工作。
type StreamWorkspace interface {
	ExecStream(ctx context.Context, req ExecRequest, onChunk func(ExecChunk)) (*ExecResult, error)
}

// FileEntry 是目录中的一个条目。
type FileEntry struct {
	Name  string `json:"name"`
	IsDir bool   `json:"isDir"`
	Size  int64  `json:"size"`
	// ModTime 最后修改时间（可能为零值，取决于后端是否提供）。
	ModTime time.Time `json:"modTime,omitempty"`
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

	// Timeout 单条命令的硬上限（兜底）。默认 0 表示自动 = StuckTimeout × 3（见
	// resolveExecTimeouts 与 hardTimeoutFactor），可由 sandbox.timeout 配置显式覆盖。
	// 与 StuckTimeout 的区别：本值是「无论如何都不能超过」的总时长；StuckTimeout 是
	// 「无输出多久算卡死」的看门狗阈值。正常运行的慢命令（持续输出）靠 StuckTimeout 放行，
	// 只有本值兜底防止无限挂起。本值不写死，始终随卡死阈值联动。
	Timeout time.Duration

	// StuckTimeout 卡死看门狗阈值。默认 5 分钟；可由 sandbox.stuck_timeout 配置覆盖。
	// 命令连续无输出超过该时长即判定卡死并终止（需已过启动宽限期且进程存活）。
	StuckTimeout time.Duration

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

	// PersistentContainer 为 true 时，per-bot Docker 后端使用「一个 bot 一个长期容器」
	// 模式：容器在首次使用时创建（docker run -d ... sleep infinity），挂载一个 named
	// volume 到容器内 /workspace，bot 的所有文件读写与命令执行都通过 docker exec 在
	// 容器内完成，宿主机磁盘不落任何 bot 文件（真正隔离）。
	// 为 false 时（旧行为）：每条命令起一个临时容器（docker run --rm），文件走宿主机 bind mount。
	// 仅影响 BotWorkspaceManager 的 Docker 后端；local 后端不受影响。
	PersistentContainer bool
}

// DefaultConfig 返回默认配置。
func DefaultConfig() Config {
	return Config{
		Backend:         "auto",
		Image:           "alpine:latest",
		BaseDir:         "",
		MemoryLimit:     "2g",
		CPULimit:        "1.0",
		NetworkDisabled: false,
		Timeout:         0,               // 硬上限兜底：0 表示自动 = StuckTimeout × 3（见 resolveExecTimeouts / hardTimeoutFactor），可由 sandbox.timeout 配置显式覆盖
		StuckTimeout:    5 * time.Minute, // 卡死看门狗阈值：连续 5 分钟无输出即判定卡死，可由 sandbox.stuck_timeout 配置覆盖
		MaxOutput:       1 << 20,         // 1 MB
		MaxFileWrite:    10 << 20,        // 10 MB
		Timezone:        "UTC",
	}
}

// ============================================================================
// 卡死看门狗（stuck watchdog）参数
// ============================================================================
//
// 设计理念：单条命令不再用「固定超时一刀切杀掉」——那样会把「编译慢」和「死锁卡死」
// 同等对待。取而代之的是看门狗：只要命令持续产生输出（哪怕缓慢），就认为它活着，
// 不杀；只有当命令「连续长时间无任何输出且进程仍存活」时才判定为卡死并终止。
// 硬上限（Config.Timeout）仅作为最终兜底，防止无限挂起。

const (
	// defaultStuckTimeout 卡死看门狗默认阈值：连续 5 分钟无输出即判定卡死。
	defaultStuckTimeout = 5 * time.Minute
	// hardTimeoutFactor 硬上限兜底 = 卡死阈值 × 该系数（默认 3 倍）。
	// 硬上限不再写死为固定时长，而是随卡死阈值联动：StuckTimeout 越大，硬上限越长。
	hardTimeoutFactor = 3
	// watchdogTick 看门狗轮询间隔。
	watchdogTick = 5 * time.Second
	// heartbeatInterval 前端保活心跳间隔：命令「安静」（距上次真实输出已超过该时长）
	// 时，向前端发一次「活着」信号。远小于前端卡死看门狗阈值（默认 3 分钟），
	// 保证前端不会把「编译慢 / 长时间无输出但仍在跑」误报为「连接已中断」。
	heartbeatInterval = 15 * time.Second
	// maxStartupGrace 启动宽限期上限：命令运行不足该时长时不判卡死，
	// 避免启动加载阶段（前若干秒无输出）被误杀。实际宽限期取 stuckTimeout/2，
	// 并受此上限约束（见 runCommandWithStreaming）。
	maxStartupGrace = 30 * time.Second
)

// resolveExecTimeouts 从请求 / 配置 / 默认中解析卡死阈值与硬上限。
//   - stuck：卡死看门狗阈值，优先 req.StuckTimeout → cfg.StuckTimeout → 默认 5 分钟
//   - hard：硬上限兜底，优先 req.Timeout → cfg.Timeout → 默认 = StuckTimeout × 3
//     （见 hardTimeoutFactor）。硬上限始终随卡死阈值联动，不写死固定时长。
func resolveExecTimeouts(req ExecRequest, cfg Config) (stuck, hard time.Duration) {
	stuck = req.StuckTimeout
	if stuck == 0 {
		stuck = cfg.StuckTimeout
	}
	if stuck == 0 {
		stuck = defaultStuckTimeout
	}
	hard = req.Timeout
	if hard == 0 {
		hard = cfg.Timeout
	}
	if hard == 0 {
		// 硬上限兜底不写死：取卡死阈值的 hardTimeoutFactor 倍（默认 3 倍）。
		// StuckTimeout 调整时硬上限自动联动。
		hard = stuck * hardTimeoutFactor
	}
	return stuck, hard
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
		if ok, reason := dockerAvailability(); !ok {
			return nil, errs.Newf("sandbox: Docker backend requested but Docker is not available: %s", reason)
		}
		logger.Info("sandbox backend: docker (forced)")
		return newDockerSandbox(cfg, logger)

	case "local":
		logger.Info("sandbox backend: local (forced)")
		return newLocalSandbox(cfg, logger)

	case "auto":
		ok, reason := dockerAvailability()
		if ok {
			logger.Info("sandbox backend: docker (auto-detect)")
			return newDockerSandbox(cfg, logger)
		}
		// Docker 不可用 → 降级或报错
		if cfg.RequireDocker {
			return nil, errs.Newf("sandbox: RequireDocker is set but Docker is not available: %s", reason)
		}
		logger.Warnw("sandbox backend: local (Docker not available, fallback) — "+
			"WARNING: local mode has NO container isolation, LLM commands run directly on host",
			"reason", reason)
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
	ok, _ := dockerAvailability()
	return ok
}

// dockerAvailability 探测 Docker 可用性，并在不可用时返回可读原因。
//
// 返回原因是为了让「静默降级到 local」变得可诊断：曾发生过 launchd 启动时 PATH 被裁剪
// （不含 /opt/homebrew/bin）导致 LookPath 失败、沙箱悄悄退化为宿主直跑的事故。
func dockerAvailability() (bool, string) {
	// 先尝试自愈 PATH：服务管理器（launchd/systemd）常把 PATH 裁剪成最小集。
	ensureDockerPath()

	if _, err := exec.LookPath("docker"); err != nil {
		return false, "docker executable not found in PATH (PATH=" + os.Getenv("PATH") +
			"); if docker is installed elsewhere, set " + EnvDockerBinDir
	}
	// 快速探测 Docker daemon 是否运行（带 3s 超时，防止 daemon 无响应时无限阻塞）
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "version", "--format", "{{.Server.Version}}")
	out, err := cmd.CombinedOutput()
	if err != nil {
		reason := "docker daemon not responding: " + err.Error()
		if s := strings.TrimSpace(string(out)); s != "" {
			reason += " (" + s + ")"
		}
		return false, reason
	}
	return true, ""
}

// VirtualRoot 是 agent（bot）面向的统一工作目录虚拟根。
// docker 模式下容器内物理挂载点即为该路径；local 模式下它是宿主真实
// 工作目录的逻辑别名，路径入参会被映射回宿主目录。
const VirtualRoot = "/data"

// validatePath 校验路径安全性，防止路径逃逸（../../etc/passwd 之类）。
//
// root 是工作空间根目录的绝对路径。
// path 是用户提供的相对路径或绝对路径。
// 若 path 以虚拟根 /data 开头，会先剥离该前缀（统一 docker/local 语义）。
// 返回经过清理的、保证在 root 内的绝对路径。
func validatePath(root, path string) (string, error) {
	if path == "" {
		path = "."
	}

	// 统一替换为正斜杠（兼容 Windows 反斜杠）
	cleaned := strings.ReplaceAll(path, "\\", "/")

	// 剥离虚拟根前缀 /data（agent 以 /data 为工作根，需映射回真实 root）。
	if cleaned == VirtualRoot {
		cleaned = "."
	} else if strings.HasPrefix(cleaned, VirtualRoot+"/") {
		cleaned = cleaned[len(VirtualRoot)+1:]
	}

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

	// 解析 symlink，防止通过 symlink 逃逸到工作空间外
	// 仅在路径或其父目录存在时检查（新文件写入场景跳过）
	if resolved, err := filepath.EvalSymlinks(full); err == nil {
		// 路径存在，验证解析后的路径仍在 root 内
		resolvedRoot, _ := filepath.EvalSymlinks(root)
		if resolvedRoot == "" {
			resolvedRoot = root
		}
		if !isWithinPath(resolvedRoot, resolved) {
			return "", errs.Newf("sandbox: path %q resolves outside workspace root (symlink escape)", path)
		}
		return resolved, nil
	}

	return full, nil
}

// isWithinPath 检查 target 路径是否在 root 目录内（含 root 自身）。
func isWithinPath(root, target string) bool {
	root = filepath.Clean(root)
	target = filepath.Clean(target)
	if target == root {
		return true
	}
	return strings.HasPrefix(target, root+string(filepath.Separator))
}
