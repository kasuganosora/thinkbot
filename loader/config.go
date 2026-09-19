// Package loader 实现 thinkbot 的自举（bootstrap）外壳：
// 一个独立于主程序的监管 + 自部署进程。它可作为容器 PID1，
// 负责拉起/守护 thinkbot 子进程、暴露运维 HTTP 接口、并在 .env 开启
// loader.enabled 时支持从 git 自行拉代码重新编译二进制（健康门控回滚）。
//
// 设计要点：
//   - loader 与 thinkbot 是两个进程，子进程崩溃/升级不影响监管者“永生”。
//   - loader.enabled=false 时 loader 退化为透明启动器（syscall.Exec thinkbot），
//     行为与未引入 loader 前完全一致，现有部署零影响。
//   - 自部署切换采用“隔离端口 smoke test”：新二进制在独立临时目录（独立 .env/api.addr
//   - 独立 DB_PATH）以 :18080 启动验证健康，验证不过则旧服务零中断、立即回退。
//     原因：thinkbot 配置优先级为 overrides>db>.env文件>os环境变量，.env 文件会压过
//     API_ADDR 进程环境变量，无法用环境变量改端口，故用临时 .env 隔离。
package loader

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/kasuganosora/thinkbot/config"
)

// Config 持有 loader 的全部运行参数，主要来自 .env 的 loader.* 键。
type Config struct {
	// 从 .env 解析的开关与运维参数
	Enabled         bool
	OpsAddr         string
	Token           string
	HealthAddr      string // 子进程 /health 完整 URL；为空时由 APIAddr 推导
	APIAddr         string // 来自 .env api.addr，用于推导 HealthAddr
	HealthTimeout   time.Duration
	SmokePort       string // 隔离 smoke test 端口，如 ":18080"
	BuildTimeout    time.Duration
	GitDir          string // 自部署源码目录
	CrashLoopMax    int
	CrashLoopWindow time.Duration

	// 运行期路径（由 LoadConfig 按 runtimeDir 解析）
	RuntimeDir string // thinkbot 运行工作目录（CWD，默认 /app）
	EnvFile    string // 实际加载的 .env 路径
	ChildBin   string // /app/thinkbot
	PrevBin    string // /app/thinkbot.prev（上一良版本快照）
	NewBin     string // /app/thinkbot.new（构建产物）
}

// LoadConfig 从 path 指定的 .env 读取 loader.* 配置，并按 runtimeDir 解析二进制路径。
func LoadConfig(path, runtimeDir string) (Config, error) {
	cfg := Config{
		Enabled:         false,
		OpsAddr:         "127.0.0.1:8090",
		Token:           "",
		HealthTimeout:   30 * time.Second,
		SmokePort:       ":18080",
		BuildTimeout:    600 * time.Second,
		GitDir:          "/app/src",
		CrashLoopMax:    5,
		CrashLoopWindow: 60 * time.Second,
		RuntimeDir:      runtimeDir,
		EnvFile:         path,
	}
	vars, err := config.LoadEnvFile(path)
	if err != nil {
		return cfg, fmt.Errorf("loader: 加载 .env %q 失败: %w", path, err)
	}
	if v, ok := vars["loader.enabled"]; ok {
		cfg.Enabled = parseBool(v)
	}
	if v, ok := vars["loader.ops_addr"]; ok && v != "" {
		cfg.OpsAddr = v
	}
	if v, ok := vars["loader.token"]; ok {
		cfg.Token = v
	}
	if v, ok := vars["loader.health_addr"]; ok && v != "" {
		cfg.HealthAddr = v
	}
	if v, ok := vars["loader.health_timeout"]; ok && v != "" {
		if d, e := time.ParseDuration(v); e == nil {
			cfg.HealthTimeout = d
		}
	}
	if v, ok := vars["loader.smoke_port"]; ok && v != "" {
		cfg.SmokePort = v
	}
	if v, ok := vars["loader.build_timeout"]; ok && v != "" {
		if d, e := time.ParseDuration(v); e == nil {
			cfg.BuildTimeout = d
		}
	}
	if v, ok := vars["loader.git_dir"]; ok && v != "" {
		cfg.GitDir = v
	}
	if v, ok := vars["loader.crashloop_max"]; ok && v != "" {
		if n, e := strconv.Atoi(v); e == nil && n > 0 {
			cfg.CrashLoopMax = n
		}
	}
	if v, ok := vars["loader.crashloop_window"]; ok && v != "" {
		if d, e := time.ParseDuration(v); e == nil {
			cfg.CrashLoopWindow = d
		}
	}
	if v, ok := vars["api.addr"]; ok && v != "" {
		cfg.APIAddr = v
	}
	if cfg.HealthAddr == "" {
		cfg.HealthAddr = deriveHealthURL(cfg.APIAddr)
	}

	// 二进制路径（runtimeDir 需为绝对路径，便于 supervisor 与 deployer 使用）
	absDir, err := filepath.Abs(runtimeDir)
	if err != nil {
		absDir = runtimeDir
	}
	cfg.RuntimeDir = absDir
	cfg.ChildBin = filepath.Join(absDir, "thinkbot")
	cfg.PrevBin = filepath.Join(absDir, "thinkbot.prev")
	cfg.NewBin = filepath.Join(absDir, "thinkbot.new")
	return cfg, nil
}

func parseBool(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "1", "yes", "on", "enable", "enabled":
		return true
	default:
		return false
	}
}

// deriveHealthURL 由 api.addr（形如 ":8080" 或 "127.0.0.1:8080"）推导 /health URL。
func deriveHealthURL(apiAddr string) string {
	const fallback = "http://127.0.0.1:8080/health"
	if apiAddr == "" {
		return fallback
	}
	host := "127.0.0.1"
	port := apiAddr
	if i := strings.LastIndex(apiAddr, ":"); i >= 0 {
		host = apiAddr[:i]
		port = apiAddr[i+1:]
		if host == "" {
			host = "127.0.0.1"
		}
	}
	if port == "" {
		return fallback
	}
	return "http://" + host + ":" + port + "/health"
}

// PassthroughEnv 复制当前进程环境变量，但剔除 sandbox-c 注入的 HTTP 代理
// （主进程 LLM 调用若走该代理会 proxyconnect 失败）。与 daemon_launch.py 同款处理。
// 导出供 cmd/loader 透传模式使用。
func PassthroughEnv() []string {
	drop := map[string]bool{
		"HTTP_PROXY":  true,
		"HTTPS_PROXY": true,
		"http_proxy":  true,
		"https_proxy": true,
		"ALL_PROXY":   true,
		"all_proxy":   true,
		"NO_PROXY":    true,
		"no_proxy":    true,
	}
	out := make([]string, 0, len(os.Environ()))
	for _, e := range os.Environ() {
		if i := strings.IndexByte(e, '='); i > 0 {
			if drop[e[:i]] {
				continue
			}
		}
		out = append(out, e)
	}
	return out
}
