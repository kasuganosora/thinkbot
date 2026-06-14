package grok

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	httputil "github.com/kasuganosora/thinkbot/util/http"
	"github.com/kasuganosora/thinkbot/util/log"
	"github.com/kasuganosora/thinkbot/util/retry"
)

// ============================================================================
// 消息构造辅助函数
// ============================================================================

// SystemMessage 创建系统消息。
func SystemMessage(content string) Message {
	return Message{Role: RoleSystem, Content: json.RawMessage(quoteJSONString(content))}
}

// UserMessage 创建用户消息。
func UserMessage(content string) Message {
	return Message{Role: RoleUser, Content: json.RawMessage(quoteJSONString(content))}
}

// AssistantMessage 创建助手消息。
func AssistantMessage(content string) Message {
	return Message{Role: RoleAssistant, Content: json.RawMessage(quoteJSONString(content))}
}

// ToolMessage 创建工具结果消息。
func ToolMessage(toolCallID, content string) Message {
	return Message{Role: RoleTool, ToolCallID: toolCallID, Content: json.RawMessage(quoteJSONString(content))}
}

// UserMessageWithImage 创建包含文本和图片的用户消息。
func UserMessageWithImage(text, imageURL string) Message {
	parts := []ContentPart{
		{Type: ContentTypeImageURL, ImageURL: &ImageURL{URL: imageURL}},
		{Type: ContentTypeText, Text: text},
	}
	data, _ := json.Marshal(parts)
	return Message{Role: RoleUser, Content: data}
}

// UserMessageWithBase64Image 创建包含文本和 base64 图片的用户消息。
func UserMessageWithBase64Image(text, mediaType, base64Data string) Message {
	dataURI := "data:" + mediaType + ";base64," + base64Data
	return UserMessageWithImage(text, dataURI)
}

// quoteJSONString 将字符串安全编码为 JSON 字符串字面量。
func quoteJSONString(s string) string {
	data, _ := json.Marshal(s)
	return string(data)
}

// ============================================================================
// 请求选项
// ============================================================================

// RequestOption 在构建请求时应用额外的修改。
type RequestOption func(*ChatCompletionRequest)

// WithTemperature 设置 temperature。
func WithTemperature(t float64) RequestOption {
	return func(r *ChatCompletionRequest) { r.Temperature = &t }
}

// WithMaxTokens 设置 max_tokens。
func WithMaxTokens(n int) RequestOption {
	return func(r *ChatCompletionRequest) { r.MaxTokens = &n }
}

// WithTopP 设置 top_p。
func WithTopP(p float64) RequestOption {
	return func(r *ChatCompletionRequest) { r.TopP = &p }
}

// WithReasoningEffort 设置推理努力程度。
func WithReasoningEffort(effort ReasoningEffort) RequestOption {
	return func(r *ChatCompletionRequest) { r.ReasoningEffort = effort }
}

// WithResponseFormat 设置响应格式。
func WithResponseFormat(format *ResponseFormat) RequestOption {
	return func(r *ChatCompletionRequest) { r.ResponseFormat = format }
}

// WithTools 设置工具列表。
func WithTools(tools ...Tool) RequestOption {
	return func(r *ChatCompletionRequest) { r.Tools = tools }
}

// WithSeed 设置随机种子。
func WithSeed(seed int) RequestOption {
	return func(r *ChatCompletionRequest) { r.Seed = &seed }
}

// WithFrequencyPenalty 设置频率惩罚。
func WithFrequencyPenalty(p float64) RequestOption {
	return func(r *ChatCompletionRequest) { r.FrequencyPenalty = &p }
}

// WithPresencePenalty 设置存在惩罚。
func WithPresencePenalty(p float64) RequestOption {
	return func(r *ChatCompletionRequest) { r.PresencePenalty = &p }
}

// WithN 设置生成数量。
func WithN(n int) RequestOption {
	return func(r *ChatCompletionRequest) { r.N = &n }
}

// JSONSchemaResponseFormat 创建 JSON Schema 响应格式。
func JSONSchemaResponseFormat(name string, schema json.RawMessage, strict bool) *ResponseFormat {
	return &ResponseFormat{
		Type: ResponseFormatJSONSchema,
		JSONSchema: &JSONSchemaConfig{
			Name:   name,
			Schema: schema,
			Strict: &strict,
		},
	}
}

// JSONObjectResponseFormat 创建 JSON Object 响应格式。
func JSONObjectResponseFormat() *ResponseFormat {
	return &ResponseFormat{Type: ResponseFormatJSONObject}
}

// ============================================================================
// CreateChatCompletion — 同步（非流式）
// ============================================================================

// CreateChatCompletion 发送同步 Chat Completions 请求并返回完整响应。
func (c *Client) CreateChatCompletion(ctx context.Context, model string, messages []Message, opts ...RequestOption) (*ChatCompletionResponse, error) {
	req := ChatCompletionRequest{
		Model:    model,
		Messages: messages,
	}
	for _, opt := range opts {
		opt(&req)
	}
	return c.DoChatCompletion(ctx, req)
}

// DoChatCompletion 发送完整的 ChatCompletionRequest。
func (c *Client) DoChatCompletion(ctx context.Context, req ChatCompletionRequest) (*ChatCompletionResponse, error) {
	if err := validateRequest(&req); err != nil {
		return nil, err
	}

	req.Stream = false

	resp, err := c.newRequest("POST", "/v1/chat/completions").
		SetContext(ctx).
		SetJSONBody(req).
		Do()
	if err != nil {
		return nil, parseAPIError(resp, err)
	}

	var result ChatCompletionResponse
	if err := resp.JSON(&result); err != nil {
		return nil, err
	}
	return &result, nil
}

// ============================================================================
// StreamChatCompletion — 流式（SSE）
// ============================================================================

// StreamChatCompletion 发送流式 Chat Completions 请求，通过回调处理每个 chunk。
//
// 阻塞直到流结束、出错或 context 取消。
//
// 返回值：
//   - nil: 流正常结束
//   - context.Canceled: 用户取消
//   - 其他 error: API 错误、网络错误
func (c *Client) StreamChatCompletion(
	ctx context.Context,
	model string,
	messages []Message,
	onChunk func(ChatCompletionResponse) error,
	opts ...RequestOption,
) error {
	return c.StreamChatCompletionWithConfig(ctx, model, messages, StreamConfig{}, onChunk, opts...)
}

// StreamConfig 流式请求的额外配置。
type StreamConfig struct {
	// WatchdogTimeout 看门狗超时。
	WatchdogTimeout time.Duration
	// RetryConfig 看门狗超时时的重试配置。
	RetryConfig *retry.Config
}

// StreamChatCompletionWithConfig 与 StreamChatCompletion 相同，但支持看门狗和重试。
func (c *Client) StreamChatCompletionWithConfig(
	ctx context.Context,
	model string,
	messages []Message,
	cfg StreamConfig,
	onChunk func(ChatCompletionResponse) error,
	opts ...RequestOption,
) error {
	req := ChatCompletionRequest{
		Model:    model,
		Messages: messages,
	}
	for _, opt := range opts {
		opt(&req)
	}
	return c.DoStreamChatCompletion(ctx, req, cfg, onChunk)
}

// DoStreamChatCompletion 发送完整的流式 ChatCompletionRequest。
func (c *Client) DoStreamChatCompletion(
	ctx context.Context,
	req ChatCompletionRequest,
	cfg StreamConfig,
	onChunk func(ChatCompletionResponse) error,
) error {
	if err := validateRequest(&req); err != nil {
		return err
	}

	req.Stream = true
	req.StreamOptions = &StreamOptions{IncludeUsage: true}

	sseCfg := httputil.SSEConfig{
		OnEvent: func(event httputil.SSEEvent) error {
			// [DONE] 标记
			if strings.TrimSpace(event.Data) == "[DONE]" {
				return nil
			}
			var chunk ChatCompletionResponse
			if err := json.Unmarshal([]byte(event.Data), &chunk); err != nil {
				return err
			}
			return onChunk(chunk)
		},
		OnError: func(err error) {
			log.Logger.Debugw("grok stream error", "err", err)
		},
	}

	if cfg.WatchdogTimeout > 0 {
		sseCfg.WatchdogTimeout = cfg.WatchdogTimeout
	}
	if cfg.RetryConfig != nil {
		sseCfg.RetryConfig = cfg.RetryConfig
	}

	r := c.newRequest("POST", "/v1/chat/completions").
		SetContext(ctx).
		SetJSONBody(req)

	return r.DoSSE(sseCfg)
}

// StreamChatCompletionChannel 发送流式请求，通过 channel 输出 chunk。
func (c *Client) StreamChatCompletionChannel(
	ctx context.Context,
	model string,
	messages []Message,
	cfg StreamConfig,
	opts ...RequestOption,
) (<-chan ChatCompletionResponse, <-chan error) {
	ch := make(chan ChatCompletionResponse, 64)
	errCh := make(chan error, 1)

	go func() {
		defer close(ch)
		defer close(errCh)

		err := c.StreamChatCompletionWithConfig(ctx, model, messages, cfg, func(chunk ChatCompletionResponse) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case ch <- chunk:
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

// StreamAccumulator 将流式 chunk 组装成完整响应。
//
// 用法：
//
//	acc := NewStreamAccumulator()
//	err := client.StreamChatCompletion(ctx, model, messages, acc.OnChunk)
//	if err == nil {
//	    resp := acc.Result()
//	}
type StreamAccumulator struct {
	id      string
	model   string
	created int64
	choices map[int]*accChoice
	usage   *Usage
	finish  map[int]FinishReason
}

type accChoice struct {
	role      string
	content   strings.Builder
	reasoning strings.Builder
	toolCalls []ToolCall
}

// NewStreamAccumulator 创建累积器。
func NewStreamAccumulator() *StreamAccumulator {
	return &StreamAccumulator{
		choices: make(map[int]*accChoice),
		finish:  make(map[int]FinishReason),
	}
}

// OnChunk 处理流式 chunk。
func (a *StreamAccumulator) OnChunk(chunk ChatCompletionResponse) error {
	a.id = chunk.ID
	a.model = chunk.Model
	if chunk.Created > a.created {
		a.created = chunk.Created
	}
	if chunk.Usage != nil {
		a.usage = chunk.Usage
	}
	for _, choice := range chunk.Choices {
		ac, ok := a.choices[choice.Index]
		if !ok {
			ac = &accChoice{}
			a.choices[choice.Index] = ac
		}
		if choice.Delta.Role != "" {
			ac.role = choice.Delta.Role
		}
		ac.content.WriteString(choice.Delta.Content)
		ac.reasoning.WriteString(choice.Delta.ReasoningContent)
		if len(choice.Delta.ToolCalls) > 0 {
			ac.toolCalls = append(ac.toolCalls, choice.Delta.ToolCalls...)
		}
		if choice.FinishReason != "" {
			a.finish[choice.Index] = choice.FinishReason
		}
	}
	return nil
}

// Result 返回组装好的完整 ChatCompletionResponse。
func (a *StreamAccumulator) Result() *ChatCompletionResponse {
	choices := make([]Choice, 0, len(a.choices))
	for idx, ac := range a.choices {
		content, _ := json.RawMessage(quoteJSONString(ac.content.String())).MarshalJSON()
		msg := Message{
			Role:    ac.role,
			Content: json.RawMessage(quoteJSONString(ac.content.String())),
		}
		if ac.reasoning.Len() > 0 {
			msg.ReasoningContent = ac.reasoning.String()
		}
		if len(ac.toolCalls) > 0 {
			msg.ToolCalls = ac.toolCalls
		}
		c := Choice{
			Index:        idx,
			Message:      msg,
			FinishReason: a.finish[idx],
		}
		_ = content
		choices = append(choices, c)
	}

	return &ChatCompletionResponse{
		ID:      a.id,
		Object:  "chat.completion",
		Created: a.created,
		Model:   a.model,
		Choices: choices,
		Usage:   a.usage,
	}
}

// ============================================================================
// 内部工具
// ============================================================================

func parseAPIError(resp *httputil.Response, httpErr error) error {
	if resp != nil && resp.StatusCode >= 400 {
		var errResp ErrorResponse
		if err := json.Unmarshal(resp.Body, &errResp); err == nil && errResp.Error.Message != "" {
			return &errResp.Error
		}
	}
	return httpErr
}

func validateRequest(req *ChatCompletionRequest) error {
	if req.Model == "" {
		return errors.New("grok: model is required")
	}
	if len(req.Messages) == 0 {
		return errors.New("grok: messages must not be empty")
	}
	return nil
}
