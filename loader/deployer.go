package loader

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/kasuganosora/thinkbot/config"
	"go.uber.org/zap"
)

// DeployStatus 是单次自部署任务的状态机取值。
type DeployStatus string

const (
	DeployPending   DeployStatus = "pending"
	DeployBuilding  DeployStatus = "building"
	DeploySmoke     DeployStatus = "smoke"
	DeployPromoting DeployStatus = "promoting"
	DeployOK        DeployStatus = "ok"
	DeployFailed    DeployStatus = "failed"
)

// DeployResult 记录一次自部署的全过程，供 /loader/deploy/:id 轮询。
type DeployResult struct {
	ID         string       `json:"id"`
	Ref        string       `json:"ref"`
	Status     DeployStatus `json:"status"`
	StartedAt  time.Time    `json:"startedAt"`
	FinishedAt time.Time    `json:"finishedAt"`
	Error      string       `json:"error"`
	Log        []string     `json:"log"`
	FromRev    string       `json:"fromRev"`
	ToRev      string       `json:"toRev"`
}

// Deployer 负责从 git 拉代码、重新编译二进制，并以健康门控方式切换/回滚。
// 全程单飞（同一时刻只允许一个部署），避免并发 rebuild 互相覆盖。
type Deployer struct {
	cfg Config
	log *zap.SugaredLogger
	sup *Supervisor

	mu      sync.Mutex
	running bool
	results map[string]*DeployResult
}

// NewDeployer 构造部署器。
func NewDeployer(cfg Config, log *zap.SugaredLogger, sup *Supervisor) *Deployer {
	return &Deployer{cfg: cfg, log: log, sup: sup, results: map[string]*DeployResult{}}
}

// Capability 报告自部署所依赖的工具/目录是否就绪。
func (d *Deployer) Capability() map[string]bool {
	_, err := os.Stat(d.cfg.GitDir)
	return map[string]bool{
		"gitDir": err == nil,
		"go":     which("go") != "",
		"node":   which("node") != "",
	}
}

// Deploy 触发一次自部署（异步）。ref 为空表示 pull 当前分支；否则 fetch+checkout 该 ref。
func (d *Deployer) Deploy(ctx context.Context, ref string) (*DeployResult, error) {
	d.mu.Lock()
	if d.running {
		d.mu.Unlock()
		return nil, fmt.Errorf("已有部署进行中")
	}
	d.running = true
	d.mu.Unlock()

	id := fmt.Sprintf("deploy-%d", time.Now().UnixNano())
	res := &DeployResult{ID: id, Ref: ref, Status: DeployPending, StartedAt: time.Now()}
	d.mu.Lock()
	d.results[id] = res
	d.mu.Unlock()

	go func() {
		defer func() {
			d.mu.Lock()
			d.running = false
			d.mu.Unlock()
		}()
		d.doDeploy(ctx, ref, res)
	}()
	return res, nil
}

// Result 按 id 取部署结果。
func (d *Deployer) Result(id string) (*DeployResult, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	r, ok := d.results[id]
	return r, ok
}

// LastResult 返回最近一次部署结果（按开始时间）。
func (d *Deployer) LastResult() (*DeployResult, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var last *DeployResult
	for _, r := range d.results {
		if last == nil || r.StartedAt.After(last.StartedAt) {
			last = r
		}
	}
	if last == nil {
		return nil, false
	}
	return last, true
}

// RestorePrev 回退到上一良版本快照（thinkbot.prev）。
func (d *Deployer) RestorePrev() error {
	if _, err := os.Stat(d.cfg.PrevBin); err != nil {
		return fmt.Errorf("无上一良版本快照 %s", d.cfg.PrevBin)
	}
	return d.sup.ReplaceBin(d.cfg.PrevBin)
}

// doDeploy 执行完整部署流程。
func (d *Deployer) doDeploy(ctx context.Context, ref string, res *DeployResult) {
	logf := func(format string, a ...any) {
		d.mu.Lock()
		res.Log = append(res.Log, fmt.Sprintf(format, a...))
		d.mu.Unlock()
	}
	fail := func(err error) {
		d.mu.Lock()
		res.Status = DeployFailed
		res.Error = err.Error()
		res.FinishedAt = time.Now()
		d.mu.Unlock()
		d.log.Errorw("部署失败", "id", res.ID, "error", err)
	}

	ctx, cancel := context.WithTimeout(ctx, d.cfg.BuildTimeout)
	defer cancel()

	// 1) 快照当前（已知良）二进制 → prev
	if err := copyFile(d.cfg.ChildBin, d.cfg.PrevBin); err != nil {
		fail(fmt.Errorf("快照当前二进制失败: %w", err))
		return
	}
	logf("已快照当前二进制 → %s", d.cfg.PrevBin)

	// 2) 拉取代码
	res.Status = DeployBuilding
	fromRev := gitRev(d.cfg.GitDir)
	res.FromRev = fromRev
	logf("当前版本 %s", fromRev)

	if ref == "" || ref == "HEAD" {
		if err := runCmd(ctx, d.cfg.GitDir, logf, "git", "pull", "--ff-only"); err != nil {
			fail(fmt.Errorf("git pull 失败: %w", err))
			return
		}
	} else {
		if err := runCmd(ctx, d.cfg.GitDir, logf, "git", "fetch", "origin", ref); err != nil {
			fail(fmt.Errorf("git fetch 失败: %w", err))
			return
		}
		if err := runCmd(ctx, d.cfg.GitDir, logf, "git", "checkout", ref); err != nil {
			fail(fmt.Errorf("git checkout 失败: %w", err))
			return
		}
	}

	// 3) 前端构建（产物落入源码树 static，部署后由运行时 /app/static 软链指向）
	if _, err := os.Stat(filepath.Join(d.cfg.GitDir, "web", "package.json")); err == nil {
		if err := runCmd(ctx, filepath.Join(d.cfg.GitDir, "web"), logf, "npm", "ci"); err != nil {
			logf("npm ci 警告: %v", err)
		}
		if err := runCmd(ctx, filepath.Join(d.cfg.GitDir, "web"), logf, "npm", "run", "build"); err != nil {
			fail(fmt.Errorf("前端构建失败: %w", err))
			return
		}
	}

	// 4) 编译主二进制（带 buildinfo ldflags，使用完整 import 路径，避免静默失效）
	rev := gitRev(d.cfg.GitDir)
	res.ToRev = rev
	now := time.Now().UTC().Format(time.RFC3339)
	ver := gitDescribe(d.cfg.GitDir)
	ldflags := fmt.Sprintf(
		"-s -w -X github.com/kasuganosora/thinkbot/internal/buildinfo.GitRevision=%s "+
			"-X github.com/kasuganosora/thinkbot/internal/buildinfo.BuildTime=%s "+
			"-X github.com/kasuganosora/thinkbot/internal/buildinfo.Version=%s",
		rev, now, ver,
	)
	buildEnv := append(os.Environ(), "CGO_ENABLED=1", "GOOS=linux")
	if err := runCmdEnv(ctx, d.cfg.GitDir, buildEnv, logf, "go", "build",
		"-ldflags", ldflags, "-o", d.cfg.NewBin, "./cmd"); err != nil {
		fail(fmt.Errorf("go build 失败: %w", err))
		return
	}
	logf("编译完成 → %s (%s)", d.cfg.NewBin, rev)

	// 5) 隔离 smoke test：在临时目录用改写 api.addr 的 .env + 独立 DB_PATH 启动新二进制
	res.Status = DeploySmoke
	tmp, err := os.MkdirTemp("", "tb-smoke-")
	if err != nil {
		fail(fmt.Errorf("创建 smoke 临时目录失败: %w", err))
		return
	}
	defer os.RemoveAll(tmp)
	if err := writeSmokeEnv(d.cfg, tmp); err != nil {
		fail(fmt.Errorf("生成 smoke .env 失败: %w", err))
		return
	}
	smokeEnv := PassthroughEnv()
	smokeEnv = append(smokeEnv, "DB_PATH="+filepath.Join(tmp, "db", "thinkbot.db"))
	smokeCmd := exec.Command(d.cfg.NewBin)
	smokeCmd.Dir = tmp
	smokeCmd.Env = smokeEnv
	smokeCmd.Stdout = os.Stderr
	smokeCmd.Stderr = os.Stderr
	if err := smokeCmd.Start(); err != nil {
		fail(fmt.Errorf("启动 smoke 进程失败: %w", err))
		return
	}
	smokeURL := "http://127.0.0.1" + ensureColon(d.cfg.SmokePort) + "/health"
	checker := NewHealthChecker(smokeURL)
	ctx2, canc2 := context.WithTimeout(ctx, d.cfg.HealthTimeout)
	st := checker.WaitUntilHealthy(ctx2)
	canc2()
	if !st.OK {
		_ = smokeCmd.Process.Kill()
		_, _ = smokeCmd.Process.Wait()
		fail(fmt.Errorf("smoke test 未通过: %v", st.Err))
		return
	}
	logf("smoke test 通过 (%s)", rev)
	_ = smokeCmd.Process.Kill()
	_, _ = smokeCmd.Process.Wait()

	// 6) 健康门控切换：停旧 → 原子替换 → 监管循环拉起新版本
	res.Status = DeployPromoting
	if err := d.sup.ReplaceBin(d.cfg.NewBin); err != nil {
		fail(fmt.Errorf("切换二进制失败: %w", err))
		return
	}
	d.mu.Lock()
	res.Status = DeployOK
	res.FinishedAt = time.Now()
	d.mu.Unlock()
	d.log.Infow("部署成功", "id", res.ID, "rev", rev)
}

// ---------------------------------------------------------------------------
// 工具函数
// ---------------------------------------------------------------------------

func ensureColon(s string) string {
	if strings.HasPrefix(s, ":") {
		return s
	}
	return ":" + s
}

func which(name string) string {
	p, err := exec.LookPath(name)
	if err != nil {
		return ""
	}
	return p
}

func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o755)
}

func gitRev(dir string) string {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(out))
}

func gitDescribe(dir string) string {
	out, err := exec.Command("git", "-C", dir, "describe", "--tags", "--always").Output()
	if err != nil {
		return "dev"
	}
	return strings.TrimSpace(string(out))
}

// writeSmokeEnv 把真实 .env 复制到临时目录，并将 api.addr 改写为隔离 smoke 端口，
// 使新二进制在独立端口启动、不与运行中的 v1 争抢 :8080 与 SQLite。
func writeSmokeEnv(cfg Config, tmp string) error {
	vars, err := config.LoadEnvFile(cfg.EnvFile)
	if err != nil {
		return err
	}
	vars["api.addr"] = ensureColon(cfg.SmokePort)
	var b strings.Builder
	for k, v := range vars {
		b.WriteString(k)
		b.WriteString("=")
		b.WriteString(v)
		b.WriteString("\n")
	}
	return os.WriteFile(filepath.Join(tmp, ".env"), []byte(b.String()), 0o644)
}

// runCmd 执行命令并把 stdout/stderr 逐行写入 logf，非零退出返回 error。
func runCmd(ctx context.Context, dir string, logf func(string, ...any), name string, args ...string) error {
	return runCmdEnv(ctx, dir, nil, logf, name, args...)
}

func runCmdEnv(ctx context.Context, dir string, env []string, logf func(string, ...any), name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	if env != nil {
		cmd.Env = env
	}
	w := &lineWriter{fn: logf}
	cmd.Stdout = w
	cmd.Stderr = w
	err := cmd.Run()
	w.flush()
	return err
}

// lineWriter 把写入的字节按行拆分后转发给 logf（适配 exec 的流式输出）。
type lineWriter struct {
	fn  func(string, ...any)
	buf []byte
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		idx := bytes.IndexByte(w.buf, '\n')
		if idx < 0 {
			break
		}
		line := string(w.buf[:idx])
		w.buf = w.buf[idx+1:]
		w.fn("%s", line)
	}
	return len(p), nil
}

func (w *lineWriter) flush() {
	if len(w.buf) > 0 {
		w.fn("%s", string(w.buf))
		w.buf = nil
	}
}
