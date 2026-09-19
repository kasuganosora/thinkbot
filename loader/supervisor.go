package loader

import (
	"context"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"go.uber.org/zap"
)

// Supervisor 监管 thinkbot 子进程的生命周期：启动、等待、崩溃重启（指数退避）、
// 信号转发，并在检测到“崩溃环”时回调上层执行二进制回退。它通过一组带缓冲的
// 控制通道（replaceChan/restartChan/stopChan）接收外部指令，保证整个循环是
// 单 goroutine、无竞态：新二进制替换、重启、关停都在 select 迭代内原子完成。
type Supervisor struct {
	cfg    Config
	log    *zap.SugaredLogger
	bin    string
	env    []string
	dir    string
	health *HealthChecker

	mu           sync.Mutex
	cmd          *exec.Cmd
	childPID     int
	restarts     int
	backoff      time.Duration
	crashTimes   []time.Time
	shuttingDown bool

	replaceChan chan string
	restartChan chan struct{}
	stopChan    chan struct{}
	swapDone    chan error

	crashLoopHandler func() error
}

const (
	minBackoff = 100 * time.Millisecond
	maxBackoff = 10 * time.Second
	stopWait   = 30 * time.Second // 关停子进程等待上限，超时则 SIGKILL
)

// NewSupervisor 构造监管器。
func NewSupervisor(cfg Config, log *zap.SugaredLogger) *Supervisor {
	return &Supervisor{
		cfg:         cfg,
		log:         log,
		bin:         cfg.ChildBin,
		env:         PassthroughEnv(),
		dir:         cfg.RuntimeDir,
		health:      NewHealthChecker(cfg.HealthAddr),
		replaceChan: make(chan string, 1),
		restartChan: make(chan struct{}, 1),
		stopChan:    make(chan struct{}, 1),
		swapDone:    make(chan error, 1),
		backoff:     minBackoff,
	}
}

// SetCrashLoopHandler 注册崩溃环触发时的回退回调（由 Deployer 注入，用于恢复上一良版本）。
func (s *Supervisor) SetCrashLoopHandler(fn func() error) {
	s.crashLoopHandler = fn
}

func (s *Supervisor) spawn() *exec.Cmd {
	cmd := exec.Command(s.bin)
	cmd.Env = s.env
	cmd.Dir = s.dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd
}

// Run 进入监管主循环，直到 RequestShutdown 触发或 ctx 取消。
func (s *Supervisor) Run(ctx context.Context) error {
	for {
		if s.isShuttingDown() {
			return nil
		}
		cmd := s.spawn()
		s.setCmd(cmd)
		if err := cmd.Start(); err != nil {
			s.log.Errorf("子进程启动失败: %v", err)
			if s.sleepBackoff() {
				return nil
			}
			continue
		}
		pid := cmd.Process.Pid
		s.setPID(pid)
		s.log.Infow("子进程已启动", "pid", pid, "bin", s.bin)

		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()

		select {
		case err := <-done:
			s.setCmd(nil)
			if s.isShuttingDown() {
				return nil
			}
			s.handleCrash(err)
			if s.sleepBackoff() {
				return nil
			}
		case newBin := <-s.replaceChan:
			s.log.Infow("执行二进制替换", "new", newBin)
			s.stopChild(cmd)
			s.waitExit(done)
			s.setCmd(nil)
			rerr := os.Rename(newBin, s.bin)
			if rerr != nil {
				// 替换失败：旧二进制原样保留，循环将继续拉起旧版本，不丢服务。
				s.log.Errorf("替换二进制失败（旧版本将继续运行）: %v", rerr)
			} else {
				s.resetBackoff()
			}
			s.swapDone <- rerr
			// 循环继续 → 拉起新二进制
		case <-s.restartChan:
			s.log.Infow("收到重启指令")
			s.stopChild(cmd)
			s.waitExit(done)
			s.setCmd(nil)
			s.resetBackoff()
			// 循环继续 → 拉起同一二进制
		case <-s.stopChan:
			s.log.Infow("收到关停指令")
			s.stopChild(cmd)
			s.waitExit(done)
			s.setCmd(nil)
			return nil
		}
	}
}

// handleCrash 处理子进程异常退出：记录崩溃时间，若落入崩溃环则触发回退并重置。
func (s *Supervisor) handleCrash(err error) {
	s.mu.Lock()
	s.restarts++
	s.mu.Unlock()
	s.log.Warnw("子进程退出", "pid", s.currentPID(), "error", err)

	s.recordCrash(time.Now())
	if s.crashLoopExceeded() {
		s.log.Errorw("检测到崩溃环，回退到上一良版本", "crashes", s.recentCrashes())
		if s.crashLoopHandler != nil {
			if rerr := s.crashLoopHandler(); rerr != nil {
				s.log.Errorf("回退失败: %v", rerr)
			}
		}
		s.resetCrashes()
		s.resetBackoff()
	}
}

func (s *Supervisor) stopChild(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
}

// waitExit 等待子进程退出；stopWait 内未退出则 SIGKILL。
func (s *Supervisor) waitExit(done chan error) {
	t := time.NewTimer(stopWait)
	defer t.Stop()
	select {
	case <-done:
	case <-t.C:
		s.mu.Lock()
		cmd := s.cmd
		s.mu.Unlock()
		if cmd != nil && cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		<-done
	}
}

// ReplaceBin 停止当前子进程、将 newBin 原子重命名为受监管二进制，随后循环拉起新版本。
// 用于自部署上线。返回 rename 的错误（nil 表示成功）。
func (s *Supervisor) ReplaceBin(newBin string) error {
	s.replaceChan <- newBin
	return <-s.swapDone
}

// Restart 仅重启子进程（不改动二进制）。
func (s *Supervisor) Restart() {
	select {
	case s.restartChan <- struct{}{}:
	default:
	}
}

// RequestShutdown 请求优雅关停：置标志并下发 stop 信号。
func (s *Supervisor) RequestShutdown() {
	s.mu.Lock()
	s.shuttingDown = true
	s.mu.Unlock()
	select {
	case s.stopChan <- struct{}{}:
	default:
	}
}

// ChildStatus 探活当前子进程，返回 pid 与健康状态（供运维接口 /loader/health、/status 使用）。
func (s *Supervisor) ChildStatus() (int, HealthStatus) {
	pid := s.childPID
	st := s.health.Check(context.Background())
	return pid, st
}

func (s *Supervisor) isShuttingDown() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.shuttingDown
}

func (s *Supervisor) setCmd(cmd *exec.Cmd) {
	s.mu.Lock()
	s.cmd = cmd
	s.mu.Unlock()
}

func (s *Supervisor) setPID(pid int) {
	s.mu.Lock()
	s.childPID = pid
	s.mu.Unlock()
}

func (s *Supervisor) currentPID() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.childPID
}

func (s *Supervisor) recordCrash(t time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.crashTimes = append(s.crashTimes, t)
	cut := t.Add(-s.cfg.CrashLoopWindow)
	kept := s.crashTimes[:0]
	for _, c := range s.crashTimes {
		if c.After(cut) {
			kept = append(kept, c)
		}
	}
	s.crashTimes = kept
}

func (s *Supervisor) crashLoopExceeded() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.crashTimes) >= s.cfg.CrashLoopMax
}

func (s *Supervisor) recentCrashes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.crashTimes)
}

func (s *Supervisor) resetCrashes() {
	s.mu.Lock()
	s.crashTimes = nil
	s.mu.Unlock()
}

func (s *Supervisor) resetBackoff() {
	s.mu.Lock()
	s.backoff = minBackoff
	s.mu.Unlock()
}

// sleepBackoff 退避后休眠；返回 true 表示 ctx 已取消（应退出循环）。
func (s *Supervisor) sleepBackoff() bool {
	s.mu.Lock()
	b := s.backoff
	if s.backoff < maxBackoff {
		s.backoff *= 2
		if s.backoff > maxBackoff {
			s.backoff = maxBackoff
		}
	}
	s.mu.Unlock()
	t := time.NewTimer(b)
	defer t.Stop()
	select {
	case <-t.C:
		return false
	}
}
