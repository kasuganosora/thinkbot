package openai

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	httputil "github.com/kasuganosora/thinkbot/util/http"
	"github.com/kasuganosora/thinkbot/util/retry"
	"github.com/kasuganosora/thinkbot/util/traceid"
)

// ============================================================================
// StreamResponse — 流式（SSE）
// ============================================================================

// StreamConfig 流式请求的额外配置。
type StreamConfig struct {
	// WatchdogTimeout 看门狗超时。
	WatchdogTimeout time.Duration
	// RetryConfig 看门狗超时时的重试配置。
	RetryConfig *retry.Config
}

// StreamResponse 发送流式 Responses API 请求，通过回调处理每个事件。
//
// 阻塞直到流结束、出错或 context 取消。
func (c *Client) StreamResponse(
	ctx context.Context,
	model string,
	input any,
	onEvent func(StreamEvent) error,
	opts ...RequestOption,
) error {
	return c.StreamResponseWithConfig(ctx, model, input, StreamConfig{}, onEvent, opts...)
}

// StreamResponseWithConfig 与 StreamResponse 相同，但支持看门狗和重试。
func (c *Client) StreamResponseWithConfig(
	ctx context.Context,
	model string,
	input any,
	cfg StreamConfig,
	onEvent func(StreamEvent) error,
	opts ...RequestOption,
) error {
	req := CreateResponseRequest{
		Model: model,
	}
	if err := setInput(&req, input); err != nil {
		return err
	}
	for _, opt := range opts {
		opt(&req)
	}
	return c.DoStreamResponse(ctx, req, cfg, onEvent)
}

// DoStreamResponse 发送完整的流式 CreateResponseRequest。
func (c *Client) DoStreamResponse(
	ctx context.Context,
	req CreateResponseRequest,
	cfg StreamConfig,
	onEvent func(StreamEvent) error,
) error {
	if err := validateRequest(&req); err != nil {
		return err
	}

	req.Stream = true

	sseCfg := httputil.SSEConfig{
		OnEvent: func(event httputil.SSEEvent) error {
			// 跳过 [DONE]
			if strings.TrimSpace(event.Data) == "[DONE]" {
				return nil
			}
			var se StreamEvent
			if err := json.Unmarshal([]byte(event.Data), &se); err != nil {
				return err
			}
			return onEvent(se)
		},
		OnError: func(err error) {
			traceid.L(ctx).Debugw("openai stream error", "err", err)
		},
	}

	if cfg.WatchdogTimeout > 0 {
		sseCfg.WatchdogTimeout = cfg.WatchdogTimeout
	}
	if cfg.RetryConfig != nil {
		sseCfg.RetryConfig = cfg.RetryConfig
	}

	r := c.newRequest("POST", "/v1/responses").
		SetContext(ctx).
		SetJSONBody(req)

	return r.DoSSE(sseCfg)
}

// StreamResponseChannel 发送流式请求，通过 channel 输出事件。
func (c *Client) StreamResponseChannel(
	ctx context.Context,
	model string,
	input any,
	cfg StreamConfig,
	opts ...RequestOption,
) (<-chan StreamEvent, <-chan error) {
	ch := make(chan StreamEvent, 64)
	errCh := make(chan error, 1)

	go func() {
		defer close(ch)
		defer close(errCh)

		err := c.StreamResponseWithConfig(ctx, model, input, cfg, func(event StreamEvent) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case ch <- event:
				return nil
			}
		}, opts...)
		errCh <- err
	}()

	return ch, errCh
}

// ============================================================================
// 流式累积器
// ============================================================================

// StreamAccumulator 将流式事件组装成完整响应。
//
// 用法：
//
//	acc := NewStreamAccumulator()
//	err := client.StreamResponse(ctx, model, input, acc.OnEvent)
//	if err == nil {
//	    resp := acc.Result()
//	    text := acc.Text()
//	}
type StreamAccumulator struct {
	response      *Response
	text          strings.Builder
	items         map[int]*OutputItem
	itemOrder     []int
	usage         *ResponseUsage
	status        string
	id            string
	model         string
	createdAt     int64
	functionCalls []FunctionCallOutput
}

// NewStreamAccumulator 创建累积器。
func NewStreamAccumulator() *StreamAccumulator {
	return &StreamAccumulator{
		items: make(map[int]*OutputItem),
	}
}

// OnEvent 处理流式事件。
func (a *StreamAccumulator) OnEvent(event StreamEvent) error {
	switch event.Type {
	case EventResponseCreated, EventResponseInProgress:
		if event.Response != nil {
			a.response = event.Response
			a.id = event.Response.ID
			a.model = event.Response.Model
			a.status = event.Response.Status
			a.createdAt = event.Response.CreatedAt
		}

	case EventResponseOutputTextDelta:
		a.text.WriteString(event.Delta)

	case EventResponseOutputItemAdded:
		if event.Item != nil {
			idx := event.OutputIndex
			if _, ok := a.items[idx]; !ok {
				a.itemOrder = append(a.itemOrder, idx)
			}
			a.items[idx] = event.Item
			if event.Item.Type == TypeFunctionCall {
				a.functionCalls = append(a.functionCalls, FunctionCallOutput{
					ID:        event.Item.ID,
					CallID:    event.Item.CallID,
					Name:      event.Item.Name,
					Arguments: event.Item.Arguments,
					Status:    event.Item.Status,
				})
			}
		}

	case EventResponseOutputItemDone:
		if event.Item != nil {
			idx := event.OutputIndex
			a.items[idx] = event.Item
		}

	case EventResponseCompleted:
		if event.Response != nil {
			a.response = event.Response
			a.usage = event.Response.Usage
			a.status = event.Response.Status
		}

	case EventResponseFailed:
		if event.Response != nil {
			a.response = event.Response
			a.status = event.Response.Status
		}

	case EventResponseIncomplete:
		if event.Response != nil {
			a.response = event.Response
			a.status = event.Response.Status
		}
	}
	return nil
}

// Text 返回累积的输出文本。
func (a *StreamAccumulator) Text() string {
	return a.text.String()
}

// Result 返回组装好的完整 Response。
func (a *StreamAccumulator) Result() *Response {
	if a.response != nil {
		return a.response
	}

	output := make([]OutputItem, 0, len(a.itemOrder))
	for _, idx := range a.itemOrder {
		if item, ok := a.items[idx]; ok {
			output = append(output, *item)
		}
	}

	return &Response{
		ID:        a.id,
		Object:    "response",
		CreatedAt: a.createdAt,
		Status:    a.status,
		Model:     a.model,
		Output:    output,
		Usage:     a.usage,
	}
}

// FunctionCalls 返回累积的函数调用列表。
func (a *StreamAccumulator) FunctionCalls() []FunctionCallOutput {
	return a.functionCalls
}

// ============================================================================
// 便捷流式：仅文本
// ============================================================================

// StreamText 流式获取纯文本输出，通过回调处理每个文本增量。
//
// 这是 StreamResponse 的简化版本，只回调文本增量片段。
func (c *Client) StreamText(
	ctx context.Context,
	model string,
	input any,
	onDelta func(text string) error,
	opts ...RequestOption,
) error {
	acc := NewStreamAccumulator()
	return c.StreamResponse(ctx, model, input, func(event StreamEvent) error {
		if err := acc.OnEvent(event); err != nil {
			return err
		}
		if event.Type == EventResponseOutputTextDelta {
			return onDelta(event.Delta)
		}
		return nil
	}, opts...)
}

// StreamTextChannel 流式获取纯文本输出，通过 channel 输出文本增量。
func (c *Client) StreamTextChannel(
	ctx context.Context,
	model string,
	input any,
	opts ...RequestOption,
) (<-chan string, <-chan string, <-chan error) {
	deltaCh := make(chan string, 64)
	finalCh := make(chan string, 1)
	errCh := make(chan error, 1)

	go func() {
		defer close(deltaCh)
		defer close(finalCh)
		defer close(errCh)

		acc := NewStreamAccumulator()
		err := c.StreamResponse(ctx, model, input, func(event StreamEvent) error {
			if err := acc.OnEvent(event); err != nil {
				return err
			}
			if event.Type == EventResponseOutputTextDelta {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case deltaCh <- event.Delta:
					return nil
				}
			}
			return nil
		}, opts...)
		finalCh <- acc.Text()
		errCh <- err
	}()

	return deltaCh, finalCh, errCh
}
