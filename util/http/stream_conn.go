package http

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/kasuganosora/thinkbot/util/errs"
	"github.com/kasuganosora/thinkbot/util/log"
	"github.com/kasuganosora/thinkbot/util/watchdog"
)

// streamConnResult 封装流式连接建立后的结果。
type streamConnResult struct {
	resp    *http.Response
	reqURL  string
	wd      *watchdog.Watchdog
	wdOwned bool
	origCtx context.Context
	start   time.Time
}

// streamConnect 是 SSE 和 Stream 共用的连接建立逻辑。
//
// 职责：
//   - 看门狗创建/管理
//   - 构建 HTTP 请求
//   - 发送请求（使用零超时客户端）
//   - 连接错误分类（watchdog timeout / user cancel / network error）
//   - HTTP 状态码检查
//
// 参数：
//   - kind: "SSE" 或 "Stream"，用于日志
//   - wd: 外部看门狗（可为 nil）
//   - wdTimeout: 看门狗超时（wd 为 nil 且此值 > 0 时自动创建）
//   - requireOK: 是否要求状态码严格为 200（SSE=true, Stream=false 则允许 2xx）
//   - onError: 连接失败时的回调（可为 nil）
//
// 调用方负责在读取完成后调用 resp.Body.Close() 和（如果 wdOwned）wd.Stop(true)。
func (r *Request) streamConnect(
	kind string,
	wd *watchdog.Watchdog,
	wdTimeout time.Duration,
	requireOK bool,
	onError func(error),
) (*streamConnResult, error) {
	origCtx := r.ctx

	// --- 看门狗管理 ---
	var wdOwned bool
	if wd == nil && wdTimeout > 0 {
		wd = watchdog.NewWithName(origCtx, wdTimeout, kind+"-watchdog")
		wdOwned = true
	}

	// 关键：使用看门狗的 context 作为 HTTP 请求的 context。
	if wd != nil {
		r.ctx = wd.Context()
	}

	// --- 构建请求 ---
	req, err := r.buildHTTPRequest()
	if err != nil {
		r.ctx = origCtx
		if wdOwned {
			wd.Stop(true)
		}
		return nil, errs.Wrapf(err, "failed to build %s request", kind)
	}

	reqURL := req.URL.String()

	// --- 发送请求（使用零超时客户端）---
	start := time.Now()
	streamClient := r.client.newStreamClient()

	log.Logger.Debugw(kind+" connecting", "method", r.method, "url", reqURL)

	resp, err := streamClient.Do(req)
	if err != nil {
		r.ctx = origCtx
		if wdOwned {
			wd.Stop(true)
		}

		// 看门狗可能在连接建立阶段就超时了
		if wd != nil && wd.TimedOut() {
			log.Logger.Warnw(kind+" ended (watchdog timeout during connection)",
				"url", reqURL, "elapsed", time.Since(start))
			return nil, &WatchdogTimeoutError{
				URL:          reqURL,
				Elapsed:      time.Since(start),
				WatchdogName: wd.Name(),
			}
		}
		// 用户主动取消
		if origCtx.Err() != nil {
			log.Logger.Debugw(kind+" ended (user context canceled during connection)",
				"url", reqURL)
			return nil, context.Canceled
		}
		log.Logger.Warnw(kind+" connection failed",
			"method", r.method, "url", reqURL, "err", err, "elapsed", time.Since(start))
		if onError != nil {
			onError(err)
		}
		return nil, errs.Wrapf(err, "%s connection failed", strings.ToLower(kind))
	}

	// --- 状态码检查 ---
	if requireOK {
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			r.ctx = origCtx
			if wdOwned {
				wd.Stop(true)
			}
			log.Logger.Warnw(kind+" unexpected status",
				"method", r.method, "url", reqURL,
				"status", resp.StatusCode, "elapsed", time.Since(start))
			return nil, errs.HTTPErrorf(resp.StatusCode,
				"%s connection to %s returned status %d", kind, reqURL, resp.StatusCode)
		}
	} else {
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			resp.Body.Close()
			r.ctx = origCtx
			if wdOwned {
				wd.Stop(true)
			}
			log.Logger.Warnw(kind+" unexpected status",
				"method", r.method, "url", reqURL,
				"status", resp.StatusCode, "elapsed", time.Since(start))
			return nil, errs.HTTPErrorf(resp.StatusCode,
				"%s connection to %s returned status %d", kind, reqURL, resp.StatusCode)
		}
	}

	log.Logger.Debugw(kind+" connected",
		"method", r.method, "url", reqURL,
		"status", resp.StatusCode, "elapsed", time.Since(start))

	return &streamConnResult{
		resp:    resp,
		reqURL:  reqURL,
		wd:      wd,
		wdOwned: wdOwned,
		origCtx: origCtx,
		start:   start,
	}, nil
}

// newStreamClient 创建一个零超时的 HTTP 客户端副本（用于流式长连接）。
func (c *Client) newStreamClient() *http.Client {
	sc := &http.Client{}
	*sc = *c.httpClient
	sc.Timeout = 0
	return sc
}

// classifyStreamError 在流读取遇到错误时，判断具体原因。
//
// 返回值：
//   - nil: 正常结束（EOF）
//   - context.Canceled: 用户主动取消
//   - *WatchdogTimeoutError: 看门狗超时
//   - 其他 error: 读取错误
func classifyStreamError(
	err error,
	origCtx context.Context,
	wd *watchdog.Watchdog,
	reqURL string,
	start time.Time,
	itemsReceived, bytesReceived int,
	kind string,
) error {
	// EOF → 正常结束
	if err == nil {
		log.Logger.Debugw(kind+" stream ended normally",
			"url", reqURL, "items", itemsReceived, "bytes", bytesReceived,
			"elapsed", time.Since(start))
		return nil
	}

	// 原始 context 被取消 → 用户主动取消
	if origCtx.Err() != nil {
		log.Logger.Debugw(kind+" stream ended (user context canceled)",
			"url", reqURL, "items", itemsReceived, "bytes", bytesReceived)
		return context.Canceled
	}

	// 看门狗超时
	if wd != nil && wd.TimedOut() {
		log.Logger.Warnw(kind+" stream ended (watchdog timeout)",
			"url", reqURL, "items", itemsReceived,
			"bytes", bytesReceived, "elapsed", time.Since(start))
		return &WatchdogTimeoutError{
			URL:           reqURL,
			ItemsReceived: itemsReceived,
			BytesReceived: bytesReceived,
			Elapsed:       time.Since(start),
			WatchdogName:  wd.Name(),
		}
	}

	// 其他读取错误
	log.Logger.Warnw(kind+" stream read error",
		"url", reqURL, "err", err, "items", itemsReceived, "bytes", bytesReceived)
	return errs.Wrap(err, kind+" stream read error")
}
