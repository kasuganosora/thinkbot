// Package singleinst 提供进程启动时的单实例版本协商。
//
// 问题背景：守护进程重启时，旧二进制实例若未被及时终止，会与新实例并存，
// 两个 bot engine 同时消费消息导致重复回复——这是本项目反复踩的坑。
//
// 本包在进程启动早期（HTTP server 监听前）探测已在运行的同服务实例，通过
// 其公开的 /health 拿到构建时间（buildTimeUnix，作为版本号）与 PID：
//
//   - 对方版本更新或相同 → 本实例让位（由调用方安全退出，已在运行的实例继续服务）
//   - 对方版本更旧       → 请对方优雅退出（SIGTERM），等待其释放端口后本实例接管
//
// 这样「重新构建并重启」无需外部脚本精确杀进程，binary 自身即可保证单实例。
// 版本号复用 buildinfo.Get().BuildTimeUnix：ldflags 注入构建时间，未注入时回退
// 二进制 mtime，两者都随重新编译更新，足以区分新旧。
package singleinst

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"syscall"
	"time"

	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/internal/buildinfo"
)

// ErrYield 表示本实例已让位给运行中实例。selfExit 通常已终止进程，此值仅作保险。
var ErrYield = errors.New("yield to running instance")

// healthResp 对应 /health 的响应。所有 API 统一包装为 {code,message,data}，
// 版本号与 PID 位于 data 内层。
type healthResp struct {
	Data struct {
		BuildTimeUnix int64 `json:"buildTimeUnix"`
		Pid           int   `json:"pid"`
	} `json:"data"`
}

// Acquire 执行单实例版本协商。
// addr 为本进程的 API 监听地址（如 ":8080"、"127.0.0.1:8080"、"0.0.0.0:8080"）。
// logger 用于记录协商决策；selfExit 在判定本实例应让位时被调用（建议 os.Exit(0)），
// 调用后本函数不再返回。返回 nil 表示本实例应继续启动；返回 ErrYield 为保险路径。
func Acquire(ctx context.Context, addr string, logger *zap.SugaredLogger, selfExit func()) error {
	probe, listenAddr, err := normalizeAddr(addr)
	if err != nil {
		// 地址无法解析则跳过协商，不阻断启动。
		logger.Warnw("singleinst: cannot parse listen addr, skip negotiation", "addr", addr, "err", err)
		return nil
	}
	me := buildinfo.Get().BuildTimeUnix

	client := &http.Client{Timeout: 600 * time.Millisecond}
	resp, err := client.Get("http://" + probe + "/health")
	if err != nil {
		// 连不上 = 无运行中实例，本实例正常接管。
		return nil
	}
	defer resp.Body.Close()

	var hr healthResp
	if err := json.NewDecoder(resp.Body).Decode(&hr); err != nil {
		logger.Warnw("singleinst: health decode failed, skip negotiation", "err", err)
		return nil
	}
	peerPID := hr.Data.Pid
	peerBuild := hr.Data.BuildTimeUnix
	// 响应缺 data 或关键字段（pid/buildTimeUnix 为 0）属格式异常，保守跳过，
	// 绝不据此误杀运行中实例。
	if peerPID == 0 || peerBuild == 0 {
		logger.Warnw("singleinst: health response missing pid/buildTimeUnix, skip negotiation",
			"pid", peerPID, "build", peerBuild)
		return nil
	}
	if peerPID == os.Getpid() {
		return nil
	}

	switch {
	case peerBuild >= me:
		// 对方版本更新或相同：已在运行的实例优先（同版本已占用端口），本实例让位。
		logger.Infow("singleinst: yielding to running instance",
			"peer_pid", peerPID, "peer_build", peerBuild, "mine_build", me)
		selfExit()
		return ErrYield
	default:
		// 对方更旧：请其优雅退出，等待端口释放后本实例接管。
		logger.Infow("singleinst: terminating older instance",
			"peer_pid", peerPID, "peer_build", peerBuild, "mine_build", me)
		if e := syscall.Kill(peerPID, syscall.SIGTERM); e != nil {
			logger.Warnw("singleinst: failed to signal older instance", "peer_pid", peerPID, "err", e)
		}
		if e := waitPortFree(ctx, listenAddr); e != nil {
			return e
		}
		return nil
	}
}

// normalizeAddr 把监听地址拆成「探测用地址」与「端口占用探测地址」。
// 探测用地址统一改为 127.0.0.1（localhost 回环），端口占用探测用原始地址。
func normalizeAddr(addr string) (probe, listen string, err error) {
	host, port, e := net.SplitHostPort(addr)
	if e != nil {
		if p, pe := strconv.Atoi(addr); pe == nil {
			return "127.0.0.1:" + strconv.Itoa(p), addr, nil
		}
		return "", "", e
	}
	listen = addr
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		probe = "127.0.0.1:" + port
	default:
		probe = addr
	}
	return probe, listen, nil
}

// waitPortFree 轮询直到 listen 地址可成功绑定（旧实例已释放端口），或超时/ctx 取消。
func waitPortFree(ctx context.Context, listen string) error {
	deadline := time.Now().Add(10 * time.Second)
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		l, e := net.Listen("tcp", listen)
		if e == nil {
			_ = l.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("singleinst: timeout waiting for %s free after terminating older instance", listen)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
