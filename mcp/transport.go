package mcp

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
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
	// Close 关闭传输层。
	Close() error
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

func newStdioTransport(ctx context.Context, command string, args []string, env []string) (*stdioTransport, error) {
	cmdCtx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(cmdCtx, command, args...)
	if len(env) > 0 {
		cmd.Env = env
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
	cmd.Stderr = nil

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

func (t *stdioTransport) Close() error {
	t.cancel()
	_ = t.stdin.Close()
	t.wg.Wait()
	return nil
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
