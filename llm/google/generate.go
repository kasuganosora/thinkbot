package google

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/kasuganosora/thinkbot/llm"
	httputil "github.com/kasuganosora/thinkbot/util/http"
	"github.com/kasuganosora/thinkbot/util/retry"
	"github.com/kasuganosora/thinkbot/util/traceid"
)

// ============================================================================
// GenerateContent — 同步（非流式）
// ============================================================================

// GenerateContent 发送同步 generateContent 请求并返回完整响应。
func (c *Client) GenerateContent(ctx context.Context, model string, req GenerateContentRequest) (*GenerateContentResponse, error) {
	if err := validateGenerateRequest(model, &req); err != nil {
		return nil, err
	}

	resp, err := c.newRequest("POST", modelPath(model, "generateContent")).
		SetContext(ctx).
		SetJSONBody(req).
		Do()
	if err != nil {
		return nil, parseAPIError(resp, err)
	}

	var result GenerateContentResponse
	if err := resp.JSON(&result); err != nil {
		return nil, err
	}
	return &result, nil
}

// ============================================================================
// StreamGenerateContent — 流式（SSE）
// ============================================================================

// StreamGenerateContent 发送流式 streamGenerateContent 请求，通过回调函数处理每个 chunk。
// 阻塞直到流结束、出错或 context 取消。
//
// 返回值：
//   - nil: 流正常结束
//   - context.Canceled: 用户取消
//   - *httputil.WatchdogTimeoutError: 看门狗超时
//   - 其他 error: API 错误、网络错误
func (c *Client) StreamGenerateContent(
	ctx context.Context,
	model string,
	req GenerateContentRequest,
	onChunk func(resp GenerateContentResponse) error,
) error {
	return c.StreamGenerateContentWithConfig(ctx, model, req, StreamConfig{}, onChunk)
}

// StreamConfig 流式请求的额外配置。
type StreamConfig struct {
	// WatchdogTimeout 看门狗超时：如果在此时间内没有收到任何事件，
	// 则自动中断连接。0 = 不启用看门狗。
	WatchdogTimeout time.Duration

	// RetryConfig 看门狗超时时的重试配置。
	RetryConfig *retry.Config
}

// StreamGenerateContentWithConfig 与 StreamGenerateContent 相同，但支持看门狗和重试配置。
func (c *Client) StreamGenerateContentWithConfig(
	ctx context.Context,
	model string,
	req GenerateContentRequest,
	cfg StreamConfig,
	onChunk func(resp GenerateContentResponse) error,
) error {
	if err := validateGenerateRequest(model, &req); err != nil {
		return err
	}

	sseCfg := httputil.SSEConfig{
		OnEvent: func(event httputil.SSEEvent) error {
			chunk, err := parseStreamChunk(event)
			if err != nil {
				return err
			}
			return onChunk(chunk)
		},
		OnError: func(err error) {
			traceid.L(ctx).Debugw("gemini stream error", "err", err)
		},
	}

	if cfg.WatchdogTimeout > 0 {
		sseCfg.WatchdogTimeout = cfg.WatchdogTimeout
	}
	if cfg.RetryConfig != nil {
		sseCfg.RetryConfig = cfg.RetryConfig
	}

	// Gemini 流式端点需要 alt=sse 参数
	r := c.newRequest("POST", modelPath(model, "streamGenerateContent")).
		SetContext(ctx).
		SetQuery("alt", "sse").
		SetJSONBody(req)

	if err := r.DoSSE(sseCfg); err != nil {
		return parseStreamAPIError(err)
	}
	return nil
}

// StreamGenerateContentChannel 发送流式请求，通过 channel 输出响应。
// 返回的 channel 在流结束后关闭。
// 最终错误通过 error channel 返回（nil = 正常结束）。
func (c *Client) StreamGenerateContentChannel(
	ctx context.Context,
	model string,
	req GenerateContentRequest,
	cfg StreamConfig,
) (<-chan GenerateContentResponse, <-chan error) {
	ch := make(chan GenerateContentResponse, 64)
	errCh := make(chan error, 1)

	go func() {
		defer close(ch)
		defer close(errCh)

		var finalErr error
		err := c.StreamGenerateContentWithConfig(ctx, model, req, cfg, func(resp GenerateContentResponse) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case ch <- resp:
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
// StreamAccumulator — 流式累积器
// ============================================================================

// StreamAccumulator 将流式 chunk 组装成完整的 GenerateContentResponse。
//
// 用法：
//
//	acc := NewStreamAccumulator()
//	err := client.StreamGenerateContent(ctx, model, req, acc.OnChunk)
//	if err == nil {
//	    resp := acc.Result()
//	}
type StreamAccumulator struct {
	modelVersion string
	finishReason FinishReason
	usage        *UsageMetadata

	// parts 累积的 parts
	parts         []Part
	textParts     map[int]string // 按候选 index 聚合文本
	textSignature string         // 非函数调用部分的思考签名（流式时可能出现在空文本部分中）
}

// NewStreamAccumulator 创建累积器。
func NewStreamAccumulator() *StreamAccumulator {
	return &StreamAccumulator{
		textParts: make(map[int]string),
	}
}

// OnChunk 处理流式 chunk（可直接作为 StreamGenerateContent 的回调）。
func (a *StreamAccumulator) OnChunk(resp GenerateContentResponse) error {
	if resp.ModelVersion != "" {
		a.modelVersion = resp.ModelVersion
	}
	if resp.UsageMetadata != nil {
		a.usage = resp.UsageMetadata
	}

	if len(resp.Candidates) == 0 {
		return nil
	}

	cand := resp.Candidates[0]
	if cand.FinishReason != "" {
		a.finishReason = cand.FinishReason
	}

	// 合并 parts
	for _, part := range cand.Content.Parts {
		if part.Text != "" && !part.Thought {
			// 文本部分：累加
			a.textParts[0] += part.Text
			// 保留文本部分携带的思考签名
			if part.ThoughtSignature != "" {
				a.textSignature = part.ThoughtSignature
			}
		} else if part.Text == "" && part.ThoughtSignature != "" &&
			part.FunctionCall == nil && part.FunctionResponse == nil &&
			part.InlineData == nil && part.FileData == nil &&
			part.ExecutableCode == nil && part.CodeExecutionResult == nil && !part.Thought {
			// 仅含思考签名的空部分（流式传输中的常见情况）
			a.textSignature = part.ThoughtSignature
		} else if part.Thought || part.FunctionCall != nil || part.FunctionResponse != nil ||
			part.InlineData != nil || part.FileData != nil || part.ExecutableCode != nil ||
			part.CodeExecutionResult != nil {
			// 非空的其他部分（thought、functionCall、inlineData 等）：直接追加
			a.parts = append(a.parts, part)
		}
		// 空文本部分（如 finishReason chunk 中的占位）被跳过
	}

	return nil
}

// Result 返回组装好的完整 GenerateContentResponse。
func (a *StreamAccumulator) Result() *GenerateContentResponse {
	var parts []Part

	// 按 parts 出现顺序，文本在前
	if text, ok := a.textParts[0]; ok && text != "" {
		p := Part{Text: text}
		// 将保存的思考签名附加到聚合文本部分
		if a.textSignature != "" {
			p.ThoughtSignature = a.textSignature
		}
		parts = append(parts, p)
	} else if a.textSignature != "" {
		// 无文本但有思考签名（流式中仅含签名的空部分）
		parts = append(parts, Part{ThoughtSignature: a.textSignature})
	}
	parts = append(parts, a.parts...)

	return &GenerateContentResponse{
		Candidates: []Candidate{{
			Content: Content{
				Role:  RoleModel,
				Parts: parts,
			},
			FinishReason: a.finishReason,
		}},
		UsageMetadata: a.usage,
		ModelVersion:  a.modelVersion,
	}
}

// ============================================================================
// 内部工具
// ============================================================================

// parseStreamChunk 将 SSEEvent 解析为 GenerateContentResponse。
func parseStreamChunk(event httputil.SSEEvent) (GenerateContentResponse, error) {
	var resp GenerateContentResponse
	if err := json.Unmarshal([]byte(event.Data), &resp); err != nil {
		return resp, err
	}
	return resp, nil
}

// parseAPIError 尝试从 HTTP 响应中提取 Google API 错误，并包装为 llm.LLMError。
func parseAPIError(resp *httputil.Response, httpErr error) error {
	if resp != nil && resp.StatusCode >= 400 {
		var errResp ErrorResponse
		if json.Unmarshal(resp.Body, &errResp) == nil && errResp.Error != nil && errResp.Error.Message != "" {
			return googleErrorToLLMError(errResp.Error)
		}
		return llm.NewLLMError(
			googleStatusToReason(""),
			"google",
			fmt.Sprintf("HTTP %d", resp.StatusCode),
			llm.WithCause(httpErr),
		)
	}
	if httpErr != nil {
		return llm.NewLLMError(llm.ErrorReasonTransport, "google", httpErr.Error(), llm.WithCause(httpErr))
	}
	return httpErr
}

// parseStreamAPIError 尝试从流式 HTTP 错误中提取 Google API 错误，并包装为 llm.LLMError。
func parseStreamAPIError(err error) error {
	var streamErr *httputil.StreamHTTPError
	if errors.As(err, &streamErr) && streamErr.StatusCode >= 400 {
		var errResp ErrorResponse
		if jsonErr := json.Unmarshal(streamErr.Body, &errResp); jsonErr == nil && errResp.Error != nil && errResp.Error.Message != "" {
			return googleErrorToLLMError(errResp.Error)
		}
	}
	return err
}

func googleErrorToLLMError(apiErr *APIError) *llm.LLMError {
	return llm.NewLLMError(
		googleStatusToReason(apiErr.Status),
		"google",
		apiErr.Message,
		llm.WithCause(apiErr),
	)
}

func googleStatusToReason(status string) llm.ErrorReason {
	switch status {
	case "INVALID_ARGUMENT":
		return llm.ErrorReasonInvalidRequest
	case "PERMISSION_DENIED", "UNAUTHENTICATED":
		return llm.ErrorReasonAuthentication
	case "RESOURCE_EXHAUSTED":
		return llm.ErrorReasonRateLimit
	case "FAILED_PRECONDITION":
		return llm.ErrorReasonQuotaExceeded
	case "INTERNAL", "UNAVAILABLE", "DEADLINE_EXCEEDED":
		return llm.ErrorReasonProviderInternal
	case "NOT_FOUND":
		return llm.ErrorReasonNoRoute
	default:
		return llm.ErrorReasonProviderInternal
	}
}

// validateGenerateRequest 校验 GenerateContentRequest 必要字段。
func validateGenerateRequest(model string, req *GenerateContentRequest) error {
	if model == "" {
		return errors.New("google: model is required")
	}
	if len(req.Contents) == 0 {
		return errors.New("google: contents must not be empty")
	}
	return nil
}
