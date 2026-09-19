// Command thinkbot-loader 是 thinkbot 的自举外壳入口。
//
// 两种模式（由 .env 的 loader.enabled 控制）：
//   - false（默认）：退化为透明启动器，syscall.Exec 直接接管 thinkbot 进程，
//     行为与未引入 loader 前完全一致，现有部署零影响。
//   - true：作为容器 PID1 监管 thinkbot 子进程，暴露运维 HTTP 接口，并支持
//     从 git 自行重新编译二进制（健康门控回滚）。
//
// 入口链：entrypoint.sh → exec thinkbot-loader →（passthrough | supervisor）。
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/kasuganosora/thinkbot/loader"
)

func main() {
	log := loader.NewLogger()

	// 运行工作目录：entrypoint 以 WORKDIR /app 启动 loader，故 CWD 即 /app。
	runtimeDir, err := os.Getwd()
	if err != nil {
		runtimeDir = "."
	}

	envPath := ".env"
	if p := os.Getenv("LOADER_ENV"); p != "" {
		envPath = p
	}
	cfg, err := loader.LoadConfig(envPath, runtimeDir)
	if err != nil {
		log.Errorf("加载 loader 配置失败: %v", err)
		os.Exit(1)
	}

	// 模式一：未启用 → 透明 exec thinkbot（loader 进程被 thinkbot 取代，信号原生可达）。
	if !cfg.Enabled {
		log.Infow("loader.enabled=false，透传执行 thinkbot", "bin", cfg.ChildBin)
		env := loader.PassthroughEnv()
		if err := syscall.Exec(cfg.ChildBin, []string{cfg.ChildBin}, env); err != nil {
			log.Errorf("exec thinkbot 失败: %v", err)
			os.Exit(1)
		}
	}

	// 启用但 token 为空：fail-safe，拒绝启动运维接口（避免无鉴权的危险操作暴露）。
	if cfg.Token == "" {
		log.Fatal("loader.enabled=true 但 loader.token 为空，拒绝启动（fail-safe）。请在 .env 配置 loader.token 后重试。")
	}

	sup := loader.NewSupervisor(cfg, log)
	dep := loader.NewDeployer(cfg, log, sup)
	// 崩溃环触发时，由部署器恢复上一良版本快照。
	sup.SetCrashLoopHandler(func() error {
		return dep.RestorePrev()
	})

	// 运维 HTTP 接口（异步）
	ops := loader.NewOpsServer(cfg, log, sup, dep)
	go func() {
		if err := ops.Start(); err != nil && err.Error() != "http: Server closed" {
			log.Errorf("运维接口异常: %v", err)
		}
	}()

	// 信号转发：收到 SIGTERM/SIGINT → 请求监管器优雅关停子进程。
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sig
		log.Infow("收到退出信号，请求关停子进程")
		sup.RequestShutdown()
	}()

	// 进入监管主循环（阻塞，直到关停）
	if err := sup.Run(context.Background()); err != nil {
		log.Errorf("监管循环退出: %v", err)
	}
	_ = ops.Shutdown(context.Background())
}
