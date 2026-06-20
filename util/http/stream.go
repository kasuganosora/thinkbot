package http

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/kasuganosora/thinkbot/util/log"
	"github.com/kasuganosora/thinkbot/util/retry"
	"github.com/kasuganosora/thinkbot/util/watchdog"
)

// ============================================================================
// 流式响应配置
// ============================================================================

// StreamConfig 配置流式响应行为。
type StreamConfig struct {
	// Watchdog 流式看门狗：每当从流中读到数据时自动 Feed。
	Watchdog *watchdog.Watchdog

	// WatchdogTimeout 如果设置，且 Watchdog 为 nil，则自动创建一个
	// 以此超时的看门狗（从 Request 的 context 派生）。
	// 流结束后自动 Stop。
	WatchdogTimeout time.Duration

	// RetryConfig 重试配置（可选），复用 retry.Config。
	// 仅在看门狗超时（非用户主动取消）时生效。
	// 如果 ShouldRetry 为 nil，使用 DefaultStreamShouldRetry。
	// 使用外部 Watchdog 时重试不可用。
	RetryConfig *retry.Config

	// BufferSize 读取 buffer 大小，默认 32KB。
	BufferSize int

	// OnChunk 每读到一段数据时的回调（LineMode=false 时使用）。
	OnChunk func(data []byte) error

	// OnLine 按行读取时的回调（LineMode=true 时使用）。
	OnLine func(line string) error

	// LineMode 是否按行读取。
	LineMode bool

	// OnConnect 连接建立后的回调（可选）。
	OnConnect func(resp *http.Response)

	// OnError 流读取遇到错误时的回调（可选）。
	OnError func(err error)
}

// ============================================================================
// 流式执行
// ============================================================================

// DoStream 以流式方式执行请求，持续读取响应体。
//
// 如果设置了 RetryConfig，看门狗超时会自动重试。
// 用户主动取消不会触发重试。
func (r *Request) DoStream(cfg StreamConfig) error {
	// 无重试配置 → 直接执行
	if cfg.RetryConfig == nil || cfg.RetryConfig.MaxRetries == 0 {
		return r.doStreamInternal(cfg)
	}
	// 外部 Watchdog 不支持重试
	if cfg.Watchdog != nil {
		log.Logger.Warnw("stream retry not supported with external watchdog, ignoring retry config",
			"url", r.url)
		return r.doStreamInternal(cfg)
	}
	return r.doStreamWithRetry(cfg)
}

// DoStreamChunks 以流式方式执行请求，通过 channel 输出数据块。
//
// 注意：channel 模式不支持自动重试。如果需要重试，请使用 DoStream + OnChunk 回调。
func (r *Request) DoStreamChunks(cfg StreamConfig) (<-chan []byte, error) {
	ch := make(chan []byte, 64)

	cfg.OnChunk = func(data []byte) error {
		select {
		case <-r.ctx.Done():
			return r.ctx.Err()
		case ch <- data:
			return nil
		}
	}

	go func() {
		defer close(ch)
		// 错误静默丢弃（此变体不返回 error channel）
		_ = r.doStreamInternal(cfg)
	}()

	return ch, nil
}

// DoStreamChunksWithErr 与 DoStreamChunks 相同，但额外返回一个 error channel。
// error channel 在流结束时收到最终错误（nil 表示正常结束），然后关闭。
func (r *Request) DoStreamChunksWithErr(cfg StreamConfig) (<-chan []byte, <-chan error) {
	ch := make(chan []byte, 64)
	errCh := make(chan error, 1)

	cfg.OnChunk = func(data []byte) error {
		select {
		case <-r.ctx.Done():
			return r.ctx.Err()
		case ch <- data:
			return nil
		}
	}

	go func() {
		defer close(ch)
		defer close(errCh)
		err := r.doStreamInternal(cfg)
		errCh <- err
	}()

	return ch, errCh
}

// DoStreamLines 以行流式方式执行请求，通过 channel 输出行。
//
// 注意：channel 模式不支持自动重试。
func (r *Request) DoStreamLines(cfg StreamConfig) (<-chan string, error) {
	ch := make(chan string, 64)

	cfg.LineMode = true
	cfg.OnLine = func(line string) error {
		select {
		case <-r.ctx.Done():
			return r.ctx.Err()
		case ch <- line:
			return nil
		}
	}

	go func() {
		defer close(ch)
		// 错误静默丢弃（此变体不返回 error channel）
		_ = r.doStreamInternal(cfg)
	}()

	return ch, nil
}

// DoStreamLinesWithErr 与 DoStreamLines 相同，但额外返回一个 error channel。
// error channel 在流结束时收到最终错误（nil 表示正常结束），然后关闭。
func (r *Request) DoStreamLinesWithErr(cfg StreamConfig) (<-chan string, <-chan error) {
	ch := make(chan string, 64)
	errCh := make(chan error, 1)

	cfg.LineMode = true
	cfg.OnLine = func(line string) error {
		select {
		case <-r.ctx.Done():
			return r.ctx.Err()
		case ch <- line:
			return nil
		}
	}

	go func() {
		defer close(ch)
		defer close(errCh)
		err := r.doStreamInternal(cfg)
		errCh <- err
	}()

	return ch, errCh
}

// ============================================================================
// 流式重试循环
// ============================================================================

// doStreamWithRetry 带重试执行流式请求。
//
// 每次重试创建全新的看门狗和 HTTP 连接。
// 用户主动取消不会触发重试（retry.Do 会检测 context 取消并立即返回）。
func (r *Request) doStreamWithRetry(cfg StreamConfig) error {
	retryCfg := *cfg.RetryConfig // copy
	origCtx := r.ctx

	// 默认 ShouldRetry：仅对看门狗超时（未收到数据）重试
	if retryCfg.ShouldRetry == nil {
		retryCfg.ShouldRetry = DefaultStreamShouldRetry
	}

	name := fmt.Sprintf("Stream %s %s", r.method, r.url)

	res := retry.Do(origCtx, name, retryCfg, func(ctx context.Context) error {
		r.ctx = origCtx // 每次重试使用原始 context（doStreamInternal 会创建新看门狗）
		return r.doStreamInternal(cfg)
	})

	return res.Err
}

// ============================================================================
// 流式核心读取逻辑
// ============================================================================

// doStreamInternal 单次执行流式请求。
func (r *Request) doStreamInternal(cfg StreamConfig) error {
	// --- 建立连接（共用逻辑）---
	conn, err := r.streamConnect("Stream", cfg.Watchdog, cfg.WatchdogTimeout, false, cfg.OnError)
	if err != nil {
		return err
	}

	resp := conn.resp
	wd := conn.wd
	reqURL := conn.reqURL
	origCtx := conn.origCtx
	start := conn.start

	defer func() { r.ctx = origCtx }()
	if conn.wdOwned {
		defer wd.Stop(true)
	}
	defer func() { _ = resp.Body.Close() }()

	if cfg.OnConnect != nil {
		cfg.OnConnect(resp)
	}

	// --- 读取流 ---
	if cfg.LineMode {
		return r.readStreamLines(resp.Body, cfg, wd, origCtx, reqURL, start)
	}
	return r.readStreamChunks(resp.Body, cfg, wd, origCtx, reqURL, start)
}

// readStreamChunks 按原始 chunk 读取流。
func (r *Request) readStreamChunks(
	body io.Reader,
	cfg StreamConfig,
	wd *watchdog.Watchdog,
	origCtx context.Context,
	reqURL string,
	start time.Time,
) error {
	bufSize := cfg.BufferSize
	if bufSize <= 0 {
		bufSize = 32 * 1024
	}
	buf := make([]byte, bufSize)

	totalBytes := 0
	chunksReceived := 0

	for {
		// 检查 context（非阻塞）
		if err := origCtx.Err(); err != nil {
			return context.Canceled
		}

		n, err := body.Read(buf)
		if n > 0 {
			totalBytes += n
			chunksReceived++
			// copy 避免 buffer 别名：回调方存储 data 切片时不会因下一次 Read 覆盖
			data := make([]byte, n)
			copy(data, buf[:n])

			// 喂狗
			if wd != nil {
				wd.Feed()
			}

			// 回调
			if cfg.OnChunk != nil {
				if cbErr := cfg.OnChunk(data); cbErr != nil {
					log.Logger.Debugw("stream interrupted by callback",
						"url", reqURL, "err", cbErr, "total_bytes", totalBytes)
					return cbErr
				}
			}
		}

		if err != nil {
			if err == io.EOF {
				return classifyStreamError(nil, origCtx, wd, reqURL, start, chunksReceived, totalBytes, "stream")
			}
			return classifyStreamError(err, origCtx, wd, reqURL, start, chunksReceived, totalBytes, "stream")
		}
	}
}

// readStreamLines 按行读取流。
func (r *Request) readStreamLines(
	body io.Reader,
	cfg StreamConfig,
	wd *watchdog.Watchdog,
	origCtx context.Context,
	reqURL string,
	start time.Time,
) error {
	reader := bufio.NewReaderSize(body, 64*1024)
	totalLines := 0
	totalBytes := 0

	for {
		// 检查 context（非阻塞）
		if err := origCtx.Err(); err != nil {
			return context.Canceled
		}

		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			totalLines++
			totalBytes += len(line)
			cleaned := strings.TrimRight(line, "\r\n")

			// 喂狗
			if wd != nil {
				wd.Feed()
			}

			// 回调
			if cfg.OnLine != nil {
				if cbErr := cfg.OnLine(cleaned); cbErr != nil {
					log.Logger.Debugw("stream interrupted by callback",
						"url", reqURL, "err", cbErr, "total_lines", totalLines)
					return cbErr
				}
			}
		}

		if err != nil {
			if err == io.EOF {
				return classifyStreamError(nil, origCtx, wd, reqURL, start, totalLines, totalBytes, "stream")
			}
			return classifyStreamError(err, origCtx, wd, reqURL, start, totalLines, totalBytes, "stream")
		}
	}
}
