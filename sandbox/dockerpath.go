package sandbox

// ============================================================================
// Docker 可执行文件路径自愈
//
// 背景（真实事故）：
//   通过 launchd / systemd 等服务管理器启动时，进程继承的 PATH 往往被裁剪成最小集
//   （macOS launchd 默认 "/usr/bin:/bin:/usr/sbin:/sbin"），**不含** Homebrew 的
//   /opt/homebrew/bin。而 docker CLI 通常就装在那里。
//
//   后果非常隐蔽：exec.LookPath("docker") 失败 → dockerAvailable() 返回 false →
//   Backend="auto" 静默降级为 "local"，LLM 命令直接跑在宿主上、且 bot 看到的是
//   宿主 data/workspaces/{botID} 空目录（而非容器 named volume 里的真实工作成果），
//   表现为「沙箱找不到容器、变成直接访问物理目录、文件全丢了」。
//
// 解法：
//   在探测 Docker 之前，主动在若干标准安装位置里找 docker 可执行文件，找到就把该目录
//   补进本进程的 PATH。这样 sandbox 包内 70 余处 exec.Command("docker", ...) 全部受益，
//   无需逐个改写为绝对路径。
//
// 注意：仅在 LookPath 已经失败时才补，正常 shell 启动的进程零影响。
// ============================================================================

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// EnvDockerBinDir 允许运维显式指定 docker 可执行文件所在目录，优先级最高。
// 适用于 docker 装在非标准位置的环境。
const EnvDockerBinDir = "THINKBOT_DOCKER_BIN_DIR"

var (
	dockerPathOnce     sync.Once
	dockerPathResolved string // 被补进 PATH 的目录；"" 表示未补（本来就能找到，或哪都没找到）
)

// dockerSearchDirs 返回按优先级排序的 docker 可执行文件候选目录。
func dockerSearchDirs() []string {
	var dirs []string

	// 1. 显式配置优先
	if d := strings.TrimSpace(os.Getenv(EnvDockerBinDir)); d != "" {
		dirs = append(dirs, d)
	}

	home, _ := os.UserHomeDir()

	switch runtime.GOOS {
	case "darwin":
		dirs = append(dirs,
			"/opt/homebrew/bin", // Homebrew (Apple Silicon)
			"/usr/local/bin",    // Homebrew (Intel) / Docker Desktop 符号链接
			"/Applications/Docker.app/Contents/Resources/bin", // Docker Desktop 自带
		)
		if home != "" {
			dirs = append(dirs,
				filepath.Join(home, ".docker", "bin"),   // Docker Desktop 用户级安装
				filepath.Join(home, ".rd", "bin"),       // Rancher Desktop
				filepath.Join(home, ".colima", "bin"),   // Colima
				filepath.Join(home, ".orbstack", "bin"), // OrbStack
				filepath.Join(home, ".local", "bin"),    // 通用用户级
			)
		}
	default: // linux 及其他
		dirs = append(dirs,
			"/usr/bin",
			"/usr/local/bin",
			"/snap/bin",
			"/opt/homebrew/bin", // Linuxbrew 亦可能存在
		)
		if home != "" {
			dirs = append(dirs,
				filepath.Join(home, ".docker", "bin"),
				filepath.Join(home, ".local", "bin"),
			)
		}
	}

	return dirs
}

// findDockerDir 在候选目录中查找可执行的 docker，返回其所在目录。
// 找不到返回 ""。抽成独立函数以便单测（不依赖 sync.Once 与真实 PATH）。
func findDockerDir(dirs []string) string {
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		cand := filepath.Join(dir, "docker")
		fi, err := os.Stat(cand)
		if err != nil {
			continue
		}
		// 目录不算；必须带任一执行位。
		if fi.IsDir() || fi.Mode().Perm()&0o111 == 0 {
			continue
		}
		return dir
	}
	return ""
}

// ensureDockerPath 保证本进程 PATH 中能找到 docker 可执行文件。
//
// 返回被补进 PATH 的目录（"" 表示没做任何修改）。幂等，且只在首次调用时真正执行。
func ensureDockerPath() string {
	dockerPathOnce.Do(func() {
		// 已经能找到 → 什么都不做，避免污染正常环境的 PATH。
		if _, err := exec.LookPath("docker"); err == nil {
			return
		}
		dir := findDockerDir(dockerSearchDirs())
		if dir == "" {
			return
		}
		cur := os.Getenv("PATH")
		if cur == "" {
			_ = os.Setenv("PATH", dir)
		} else {
			_ = os.Setenv("PATH", cur+string(os.PathListSeparator)+dir)
		}
		dockerPathResolved = dir
	})
	return dockerPathResolved
}
