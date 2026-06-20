package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/kasuganosora/thinkbot/llm"
	httputil "github.com/kasuganosora/thinkbot/util/http"
	"github.com/kasuganosora/thinkbot/util/log"
	"github.com/kasuganosora/thinkbot/util/retry"
)

// ============================================================================
// CreateMessage — 同步（非流式）
// ============================================================================

// CreateMessage 发送同步 Messages 请求并返回完整响应。
func (c *Client) CreateMessage(ctx context.Context, req MessageRequest) (*MessageResponse, error) {
	if err := validateMessageRequest(&req); err != nil {
		return nil, err
	}

	req.Stream = false

	resp, err := c.newRequest("POST", "/v1/messages").
		SetContext(ctx).
		SetJSONBody(req).
		Do()
	if err != nil {
		return nil, parseAPIError(resp, err)
	}

	var result MessageResponse
	if err := resp.JSON(&result); err != nil {
		return nil, err
	}
	return &result, nil
}

// ============================================================================
// StreamMessage — 流式（SSE）
// ============================================================================

// StreamMessage 发送流式 Messages 请求，通过回调函数处理每个事件。
// 阻塞直到流结束、出错或 context 取消。
//
// 返回值：
//   - nil: 流正常结束
//   - context.Canceled: 用户取消
//   - *httputil.WatchdogTimeoutError: 看门狗超时
//   - 其他 error: API 错误、网络错误
func (c *Client) StreamMessage(
	ctx context.Context,
	req MessageRequest,
	onEvent func(event StreamEvent) error,
) error {
	return c.StreamMessageWithConfig(ctx, req, StreamConfig{}, onEvent)
}

// StreamConfig 流式请求的额外配置。
type StreamConfig struct {
	// WatchdogTimeout 看门狗超时：如果在此时间内没有收到任何事件，
	// 则自动中断连接。0 = 不启用看门狗。
	WatchdogTimeout time.Duration

	// RetryConfig 看门狗超时时的重试配置。
	RetryConfig *retry.Config
}

// StreamMessageWithConfig 与 StreamMessage 相同，但支持看门狗和重试配置。
func (c *Client) StreamMessageWithConfig(
	ctx context.Context,
	req MessageRequest,
	cfg StreamConfig,
	onEvent func(event StreamEvent) error,
) error {
	if err := validateMessageRequest(&req); err != nil {
		return err
	}

	req.Stream = true

	sseCfg := httputil.SSEConfig{
		OnEvent: func(event httputil.SSEEvent) error {
			se, err := parseStreamEvent(event)
			if err != nil {
				return err
			}
			return onEvent(se)
		},
		OnError: func(err error) {
			log.Logger.Debugw("anthropic stream error", "err", err)
		},
	}

	if cfg.WatchdogTimeout > 0 {
		sseCfg.WatchdogTimeout = cfg.WatchdogTimeout
	}
	if cfg.RetryConfig != nil {
		sseCfg.RetryConfig = cfg.RetryConfig
	}

	r := c.newRequest("POST", "/v1/messages").
		SetContext(ctx).
		SetJSONBody(req)

	if err := r.DoSSE(sseCfg); err != nil {
		return parseStreamAPIError(err)
	}
	return nil
}

// StreamMessageChannel 发送流式请求，通过 channel 输出事件。
// 返回的 channel 在流结束后关闭。
// 最终错误通过 error channel 返回（nil = 正常结束）。
func (c *Client) StreamMessageChannel(
	ctx context.Context,
	req MessageRequest,
	cfg StreamConfig,
) (<-chan StreamEvent, <-chan error) {
	ch := make(chan StreamEvent, 64)
	errCh := make(chan error, 1)

	go func() {
		defer close(ch)
		defer close(errCh)

		var finalErr error
		err := c.StreamMessageWithConfig(ctx, req, cfg, func(event StreamEvent) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case ch <- event:
				return nil
			}
		})
		if err != nil {
			finalErr = err
		}
		errCh <- finalErr
	}()

	return ch, errCh
}

// ============================================================================
// CountTokens
// ============================================================================

// CountTokens 统计消息的 token 数量。
func (c *Client) CountTokens(ctx context.Context, req CountTokensRequest) (*CountTokensResponse, error) {
	if err := validateCountTokensRequest(&req); err != nil {
		return nil, err
	}

	resp, err := c.newRequest("POST", "/v1/messages/count_tokens").
		SetContext(ctx).
		SetJSONBody(req).
		Do()
	if err != nil {
		return nil, parseAPIError(resp, err)
	}

	var result CountTokensResponse
	if err := resp.JSON(&result); err != nil {
		return nil, err
	}
	return &result, nil
}

// ============================================================================
// 辅助：流式累积器
// ============================================================================

// StreamAccumulator 是一个流式消息累积器，将流式事件组装成完整响应。
//
// 用法：
//
//	acc := NewStreamAccumulator()
//	err := client.StreamMessage(ctx, req, acc.OnEvent)
//	if err == nil {
//	    resp := acc.Result()
//	}
type StreamAccumulator struct {
	id           string
	model        string
	role         string
	stopReason   StopReason
	stopSequence string
	usage        Usage

	// content blocks 按 index 聚合
	blocks         map[int]*ContentBlock
	order          []int // 已出现的 index 顺序
	textParts      map[int]string
	toolInputs     map[int]string
	thinkingParts  map[int]string
	signatureParts map[int]string
}

// NewStreamAccumulator 创建累积器。
func NewStreamAccumulator() *StreamAccumulator {
	return &StreamAccumulator{
		role:           RoleAssistant,
		blocks:         make(map[int]*ContentBlock),
		textParts:      make(map[int]string),
		toolInputs:     make(map[int]string),
		thinkingParts:  make(map[int]string),
		signatureParts: make(map[int]string),
	}
}

// OnEvent 处理流式事件（可直接作为 StreamMessage 的回调）。
func (a *StreamAccumulator) OnEvent(event StreamEvent) error {
	switch event.Type {
	case EventMessageStart:
		if event.Message != nil {
			a.id = event.Message.ID
			a.model = event.Message.Model
			a.role = event.Message.Role
			a.usage = event.Message.Usage
		}

	case EventContentBlockStart:
		if event.Index != nil && event.ContentBlock != nil {
			idx := *event.Index
			a.blocks[idx] = event.ContentBlock
			a.order = append(a.order, idx)
		}

	case EventContentBlockDelta:
		if event.Index != nil && event.Delta != nil {
			idx := *event.Index
			switch event.Delta.Type {
			case "text_delta":
				a.textParts[idx] += event.Delta.Text
			case "input_json_delta":
				a.toolInputs[idx] += event.Delta.PartialJSON
			case "thinking_delta":
				a.thinkingParts[idx] += event.Delta.Thinking
			case "signature_delta":
				a.signatureParts[idx] += event.Delta.Signature
			}
		}

	case EventMessageDelta:
		if event.Delta != nil {
			if event.Delta.StopReason != "" {
				a.stopReason = event.Delta.StopReason
			}
			if event.Delta.StopSequence != "" {
				a.stopSequence = event.Delta.StopSequence
			}
		}
		if event.Usage != nil {
			a.usage.OutputTokens = event.Usage.OutputTokens
		}

	case EventMessageStop:
		// 流结束，无需处理

	case EventError:
		if event.Error != nil {
			return event.Error
		}
	}

	return nil
}

// Result 返回组装好的完整 MessageResponse。
func (a *StreamAccumulator) Result() *MessageResponse {
	content := make([]ContentBlock, 0, len(a.order))
	for _, idx := range a.order {
		block := a.blocks[idx]
		if block == nil {
			continue
		}
		// 值拷贝，避免修改内部状态导致多次调用 Result() 内容翻倍
		b := *block
		// 填入累积的增量内容
		if b.Type == ContentTypeText {
			if text, ok := a.textParts[idx]; ok {
				b.Text += text
			}
		}
		if b.Type == ContentTypeToolUse {
			if jsonStr, ok := a.toolInputs[idx]; ok && jsonStr != "" {
				b.Input = json.RawMessage(jsonStr)
			}
		}
		if b.Type == ContentTypeThinking {
			if text, ok := a.thinkingParts[idx]; ok {
				b.Thinking += text
			}
			if sig, ok := a.signatureParts[idx]; ok {
				b.Signature += sig
			}
		}
		content = append(content, b)
	}

	return &MessageResponse{
		ID:           a.id,
		Type:         "message",
		Role:         a.role,
		Content:      content,
		Model:        a.model,
		StopReason:   a.stopReason,
		StopSequence: a.stopSequence,
		Usage:        a.usage,
	}
}

// ============================================================================
// 内部工具
// ============================================================================

// parseStreamEvent 将 SSEEvent 解析为 StreamEvent。
func parseStreamEvent(event httputil.SSEEvent) (StreamEvent, error) {
	var se StreamEvent
	if err := json.Unmarshal([]byte(event.Data), &se); err != nil {
		return se, err
	}
	return se, nil
}

// parseAPIError 尝试从 HTTP 响应中提取 Anthropic API 错误，并包装为统一的 llm.LLMError。
func parseAPIError(resp *httputil.Response, httpErr error) error {
	if resp != nil && resp.StatusCode >= 400 {
		var errResp ErrorResponse
		if json.Unmarshal(resp.Body, &errResp) == nil && errResp.Error.Message != "" {
			return anthropicErrorToLLMError(&errResp.Error, resp.StatusCode, resp.Headers)
		}
		// Body wasn't parseable as an Anthropic error — fall through with status code.
		if resp.StatusCode >= 400 {
			return llm.NewLLMError(
				httpStatusToReason(resp.StatusCode),
				"anthropic",
				fmt.Sprintf("HTTP %d: %s", resp.StatusCode, truncateBody(resp.Body)),
				llm.WithCause(httpErr),
			)
		}
	}
	// Network / transport error (no response or < 400 status).
	if httpErr != nil {
		return llm.NewLLMError(llm.ErrorReasonTransport, "anthropic", httpErr.Error(), llm.WithCause(httpErr))
	}
	return httpErr
}

// parseStreamAPIError 尝试从流式 HTTP 错误中提取 Anthropic API 错误，并包装为 llm.LLMError。
func parseStreamAPIError(err error) error {
	var streamErr *httputil.StreamHTTPError
	if errors.As(err, &streamErr) && streamErr.StatusCode >= 400 {
		var errResp ErrorResponse
		if jsonErr := json.Unmarshal(streamErr.Body, &errResp); jsonErr == nil && errResp.Error.Message != "" {
			return anthropicErrorToLLMError(&errResp.Error, streamErr.StatusCode, streamErr.Headers)
		}
		return llm.NewLLMError(
			httpStatusToReason(streamErr.StatusCode),
			"anthropic",
			fmt.Sprintf("HTTP %d: %s", streamErr.StatusCode, truncateBody(streamErr.Body)),
			llm.WithCause(err),
		)
	}
	return err
}

// anthropicErrorToLLMError converts an Anthropic APIError into a unified llm.LLMError.
func anthropicErrorToLLMError(apiErr *APIError, statusCode int, headers http.Header) *llm.LLMError {
	reason := anthropicErrorTypeToReason(apiErr.Type, statusCode)
	opts := []llm.LLMErrorOpt{llm.WithCause(apiErr)}

	if delay := parseRetryAfterHeader(headers); delay > 0 {
		opts = append(opts, llm.WithRetryAfter(delay))
	}

	return llm.NewLLMError(reason, "anthropic", apiErr.Message, opts...)
}

// anthropicErrorTypeToReason maps an Anthropic error type + HTTP status to an llm.ErrorReason.
func anthropicErrorTypeToReason(errType string, statusCode int) llm.ErrorReason {
	switch errType {
	case "invalid_request_error":
		return llm.ErrorReasonInvalidRequest
	case "authentication_error":
		return llm.ErrorReasonAuthentication
	case "permission_error":
		return llm.ErrorReasonAuthentication
	case "rate_limit_error":
		return llm.ErrorReasonRateLimit
	case "not_found_error":
		return llm.ErrorReasonNoRoute
	case "request_too_large":
		return llm.ErrorReasonInvalidRequest
	case "overloaded_error":
		return llm.ErrorReasonProviderInternal
	case "api_error":
		return llm.ErrorReasonProviderInternal
	case "content_policy_violation":
		return llm.ErrorReasonContentPolicy
	case "billing_error":
		return llm.ErrorReasonQuotaExceeded
	}
	return httpStatusToReason(statusCode)
}

func httpStatusToReason(statusCode int) llm.ErrorReason {
	switch {
	case statusCode == 429:
		return llm.ErrorReasonRateLimit
	case statusCode == 401 || statusCode == 403:
		return llm.ErrorReasonAuthentication
	case statusCode == 402:
		return llm.ErrorReasonQuotaExceeded
	case statusCode >= 500:
		return llm.ErrorReasonProviderInternal
	case statusCode >= 400:
		return llm.ErrorReasonInvalidRequest
	default:
		return llm.ErrorReasonTransport
	}
}

// parseRetryAfterHeader extracts the Retry-After delay from HTTP headers.
func parseRetryAfterHeader(headers http.Header) time.Duration {
	if headers == nil {
		return 0
	}
	if msStr := headers.Get("Retry-After-MS"); msStr != "" {
		if ms, err := strconv.ParseFloat(strings.TrimSpace(msStr), 64); err == nil {
			return time.Duration(ms * float64(time.Millisecond))
		}
	}
	if raStr := strings.TrimSpace(headers.Get("Retry-After")); raStr != "" {
		if secs, err := strconv.Atoi(raStr); err == nil {
			return time.Duration(secs) * time.Second
		}
	}
	return 0
}

func truncateBody(body []byte) string {
	const max = 500
	if len(body) <= max {
		return string(body)
	}
	return string(body[:max]) + "..."
}

// validateMessageRequest 校验 MessageRequest 必要字段。
func validateMessageRequest(req *MessageRequest) error {
	if req.Model == "" {
		return errors.New("anthropic: model is required")
	}
	if len(req.Messages) == 0 {
		return errors.New("anthropic: messages must not be empty")
	}
	if req.MaxTokens <= 0 {
		return errors.New("anthropic: max_tokens must be > 0")
	}
	return nil
}

// validateCountTokensRequest 校验 CountTokensRequest 必要字段。
func validateCountTokensRequest(req *CountTokensRequest) error {
	if req.Model == "" {
		return errors.New("anthropic: model is required")
	}
	if len(req.Messages) == 0 {
		return errors.New("anthropic: messages must not be empty")
	}
	return nil
}
