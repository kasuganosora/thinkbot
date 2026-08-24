package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/kasuganosora/thinkbot/util/errs"
	utilhttp "github.com/kasuganosora/thinkbot/util/http"
)

// ============================================================================
// Transport — MCP 通信传输层抽象
// ============================================================================

// transport 是 MCP 客户端使用的底层传输接口。
// 一次 RoundTrip 完成完整的请求-响应交换。
type transport interface {
	// RoundTrip 发送一条 JSON-RPC 请求并返回原始响应字节。
	RoundTrip(ctx context.Context, data []byte) ([]byte, error)
	// Send 仅写入一条 JSON-RPC 消息（用于通知，不需要响应）。
	Send(ctx context.Context, data []byte) error
	// Close 关闭传输层。
	Close() error
	// Healthy 返回传输层是否仍存活（用于断线检测 / 自动重连）。
	Healthy() bool
}

// ============================================================================
// stdioTransport — 通过子进程 stdin/stdout 通信
// ============================================================================

type stdioTransport struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	mu     sync.Mutex // 串行化 stdin/stdout 访问
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func newStdioTransport(ctx context.Context, command string, args, env []string, stderr io.Writer) (*stdioTransport, error) {
	cmdCtx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(cmdCtx, command, args...)
	if len(env) > 0 {
		// 追加而非替换，避免丢掉 PATH 等基础环境导致命令（如 docker）找不到。
		cmd.Env = append(os.Environ(), env...)
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, errs.Wrap(err, "mcp: create stdin pipe")
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, errs.Wrap(err, "mcp: create stdout pipe")
	}
	// stderr 为 nil 时丢弃（默认）；非 nil 时回写到调用方提供的 io.Writer（如 thinkbot 日志）。
	cmd.Stderr = stderr

	if err := cmd.Start(); err != nil {
		cancel()
		return nil, errs.Wrapf(err, "mcp: start command %q", command)
	}

	t := &stdioTransport{
		cmd:    cmd,
		stdin:  stdin,
		stdout: bufio.NewReader(stdout),
		cancel: cancel,
	}
	t.wg.Add(1)
	go func() {
		defer t.wg.Done()
		_ = cmd.Wait()
	}()
	return t, nil
}

func (t *stdioTransport) RoundTrip(ctx context.Context, data []byte) ([]byte, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if _, err := t.stdin.Write(append(data, '\n')); err != nil {
		return nil, errs.Wrap(err, "mcp: write to stdin")
	}

	// 读取响应：跳过服务器提前发来的通知（如启动 log 通知，含 method 但无 id），
	// 直到拿到真正的响应行，避免把通知误当响应解析（此前报
	// "parse initialize result: unexpected end of JSON input"）。
	for {
		line, err := t.readLine(ctx)
		if err != nil {
			return nil, err
		}
		if isNotificationLine(line) {
			continue
		}
		return line, nil
	}
}

// readLine 从 stdout 读取一行（去除首尾空白）。
func (t *stdioTransport) readLine(ctx context.Context) ([]byte, error) {
	type result struct {
		line []byte
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		line, err := t.stdout.ReadBytes('\n')
		ch <- result{line, err}
	}()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r := <-ch:
		if r.err != nil {
			return nil, errs.Wrap(r.err, "mcp: read from stdout")
		}
		return bytes.TrimSpace(r.line), nil
	}
}

// isNotificationLine 判断一行 JSON-RPC 消息是否为通知（有 method 但无 id）。
// 空行或无法解析的噪声行也视为可跳过。
func isNotificationLine(line []byte) bool {
	if len(bytes.TrimSpace(line)) == 0 {
		return true
	}
	var probe struct {
		Method string           `json:"method"`
		ID     *json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal(line, &probe); err != nil {
		return true
	}
	return probe.Method != "" && probe.ID == nil
}

// Send 仅写入一条消息（用于通知），不等待响应。
func (t *stdioTransport) Send(ctx context.Context, data []byte) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, err := t.stdin.Write(append(data, '\n')); err != nil {
		return errs.Wrap(err, "mcp: write to stdin")
	}
	return nil
}

func (t *stdioTransport) Close() error {
	if t.cmd != nil && t.cmd.Process != nil {
		// 先优雅发送 SIGTERM，给子进程机会做退出清理（如发送 exit 通知）。
		_ = t.cmd.Process.Signal(syscall.SIGTERM)
	}

	done := make(chan struct{})
	go func() {
		t.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// 子进程已在宽限期内自行退出
	case <-time.After(3 * time.Second):
		// 优雅退出超时，强制取消（SIGKILL）并等待进程真正退出。
		t.cancel()
		<-done
	}

	_ = t.stdin.Close()
	return nil
}

// Healthy 通过向子进程发送信号 0 探测其是否仍存活（进程退出后 Signal(0) 报错）。
func (t *stdioTransport) Healthy() bool {
	if t.cmd == nil || t.cmd.Process == nil {
		return false
	}
	return t.cmd.Process.Signal(syscall.Signal(0)) == nil
}

// ============================================================================
// httpTransport — Streamable HTTP 传输
// ============================================================================

type httpTransport struct {
	url       string
	headers   map[string]string
	client    *utilhttp.Client
	mu        sync.Mutex // 保护 sessionID
	sessionID string     // 服务器返回的 Mcp-Session-Id
}

func newHTTPTransport(url string, headers map[string]string) *httpTransport {
	return &httpTransport{
		url:     url,
		headers: headers,
		client: utilhttp.New(
			utilhttp.WithTimeout(120*time.Second),
			utilhttp.WithHeaders(headers),
			utilhttp.WithMaxBodySize(10*1024*1024), // 10MB
		),
	}
}

func (t *httpTransport) RoundTrip(ctx context.Context, data []byte) ([]byte, error) {
	req := t.client.Post(t.url).
		SetContext(ctx).
		SetHeader("Content-Type", "application/json").
		SetHeader("Accept", "application/json, text/event-stream").
		SetBody(bytes.NewReader(data))

	// 转发 session ID（如果服务器已分配）
	t.mu.Lock()
	if t.sessionID != "" {
		req.SetHeader("Mcp-Session-Id", t.sessionID)
	}
	t.mu.Unlock()

	resp, err := req.Do()

	// 捕获服务器分配的 session ID（通常在 initialize 响应中返回）
	if resp != nil {
		if sid := resp.Headers.Get("Mcp-Session-Id"); sid != "" {
			t.mu.Lock()
			t.sessionID = sid
			t.mu.Unlock()
		}
	}

	if err != nil {
		// 非 2xx 时 resp 可能为 nil（网络错误）或非 nil（HTTP 错误码）
		if resp != nil && resp.StatusCode >= 400 {
			return nil, fmt.Errorf("mcp: http error %d: %s", resp.StatusCode, string(resp.Body))
		}
		return nil, errs.Wrap(err, "mcp: http request")
	}

	contentType := resp.Headers.Get("Content-Type")
	if strings.Contains(contentType, "text/event-stream") {
		return parseSSEResponse(bytes.NewReader(resp.Body))
	}
	return resp.Body, nil
}

func (t *httpTransport) Close() error { return nil }

// Send 仅发送一条消息（用于通知），不等待响应（fire-and-forget POST）。
func (t *httpTransport) Send(ctx context.Context, data []byte) error {
	req := t.client.Post(t.url).
		SetContext(ctx).
		SetHeader("Content-Type", "application/json").
		SetHeader("Accept", "application/json, text/event-stream").
		SetBody(bytes.NewReader(data))
	_, err := req.Do()
	return err
}

// Healthy HTTP 传输始终返回 true；连接错误由调用方在请求失败时驱动重连。
func (t *httpTransport) Healthy() bool { return true }

// parseSSEResponse 从 SSE 流中提取所有 data: 行并拼接为完整的 JSON。
// SSE 规范允许多行 data: 组成单个事件体。
func parseSSEResponse(r io.Reader) ([]byte, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024)
	var dataLines []string
	for scanner.Scan() {
		line := scanner.Text()
		if rest, ok := strings.CutPrefix(line, "data:"); ok {
			rest = strings.TrimSpace(rest)
			if rest != "" {
				dataLines = append(dataLines, rest)
			}
		}
	}
	if len(dataLines) == 0 {
		return nil, fmt.Errorf("mcp: no data in SSE response")
	}
	// 如果只有一行，直接返回
	if len(dataLines) == 1 {
		return []byte(dataLines[0]), nil
	}
	// 多行：拼接（SSE 规范中多行 data 用换行连接）
	return []byte(strings.Join(dataLines, "\n")), nil
}
