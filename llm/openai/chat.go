package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/kasuganosora/thinkbot/llm"
	httputil "github.com/kasuganosora/thinkbot/util/http"
	"github.com/kasuganosora/thinkbot/util/log"
	"github.com/kasuganosora/thinkbot/util/retry"
)

// ============================================================================
// Chat Completions — 同步（非流式）
// ============================================================================

// DoChatCompletion 发送同步 Chat Completions 请求并返回完整响应。
func (c *Client) DoChatCompletion(ctx context.Context, req ChatCompletionRequest) (*ChatCompletionResponse, error) {
	if req.Model == "" {
		return nil, fmt.Errorf("openai: model is required")
	}
	if len(req.Messages) == 0 {
		return nil, fmt.Errorf("openai: messages must not be empty")
	}

	req.Stream = false

	resp, err := c.newRequest("POST", c.chatPath).
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
// Chat Completions — 流式（SSE）
// ============================================================================

// ChatStreamConfig 流式请求的额外配置。
type ChatStreamConfig struct {
	WatchdogTimeout time.Duration
	RetryConfig     *retry.Config
}

// DoStreamChatCompletion 发送流式 Chat Completions 请求，通过回调处理每个 chunk。
func (c *Client) DoStreamChatCompletion(
	ctx context.Context,
	req ChatCompletionRequest,
	cfg ChatStreamConfig,
	onChunk func(ChatCompletionResponse) error,
) error {
	if req.Model == "" {
		return fmt.Errorf("openai: model is required")
	}
	if len(req.Messages) == 0 {
		return fmt.Errorf("openai: messages must not be empty")
	}

	req.Stream = true
	req.StreamOptions = &ChatStreamOptions{IncludeUsage: true}

	sseCfg := httputil.SSEConfig{
		OnEvent: func(event httputil.SSEEvent) error {
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
			log.Logger.Debugw("openai chat stream error", "err", err)
		},
	}

	if cfg.WatchdogTimeout > 0 {
		sseCfg.WatchdogTimeout = cfg.WatchdogTimeout
	}
	if cfg.RetryConfig != nil {
		sseCfg.RetryConfig = cfg.RetryConfig
	}

	r := c.newRequest("POST", c.chatPath).
		SetContext(ctx).
		SetJSONBody(req)

	return r.DoSSE(sseCfg)
}

// ============================================================================
// Chat Completions — 统一 Provider 接口适配
// ============================================================================

type chatStreamingToolCall struct {
	id       string
	name     string
	args     string
	finished bool
}

// doGenerateChat 通过 Chat Completions API 执行 DoGenerate。
func (c *Client) doGenerateChat(ctx context.Context, params llm.GenerateParams) (*llm.GenerateResult, error) {
	req, err := paramsToChatRequest(&params)
	if err != nil {
		return nil, err
	}

	resp, err := c.DoChatCompletion(ctx, *req)
	if err != nil {
		return nil, err
	}

	return chatResponseToResult(resp), nil
}

// doStreamChat 通过 Chat Completions API 执行 DoStream。
func (c *Client) doStreamChat(ctx context.Context, params llm.GenerateParams) (*llm.StreamResult, error) {
	req, err := paramsToChatRequest(&params)
	if err != nil {
		return nil, err
	}

	ch := make(chan llm.StreamPart, 64)

	go func() {
		defer close(ch)

		send := func(part llm.StreamPart) bool {
			select {
			case ch <- part:
				return true
			case <-ctx.Done():
				return false
			}
		}

		if !send(&llm.StartPart{}) {
			return
		}
		if !send(&llm.StartStepPart{}) {
			return
		}

		var (
			textStarted      bool
			reasoningStarted bool
			finishReason     llm.FinishReason
			rawFinishReason  string
			usage            llm.Usage
			responseID       string
			responseModel    string
			pendingToolCalls = map[int]*chatStreamingToolCall{}
		)

		flush := func() {
			if reasoningStarted {
				send(&llm.ReasoningEndPart{ID: responseID})
				reasoningStarted = false
			}
			if textStarted {
				send(&llm.TextEndPart{ID: responseID})
				textStarted = false
			}
		}

		streamErr := c.DoStreamChatCompletion(ctx, *req, ChatStreamConfig{}, func(chunk ChatCompletionResponse) error {
			responseID = chunk.ID
			responseModel = chunk.Model

			if chunk.Usage != nil {
				usage.InputTokens = chunk.Usage.PromptTokens
				usage.OutputTokens = chunk.Usage.CompletionTokens
				usage.TotalTokens = chunk.Usage.TotalTokens
			}

			for _, choice := range chunk.Choices {
				// Reasoning content
				if choice.Delta.ReasoningContent != "" {
					if !reasoningStarted {
						send(&llm.ReasoningStartPart{ID: chunk.ID})
						reasoningStarted = true
					}
					send(&llm.ReasoningDeltaPart{ID: chunk.ID, Text: choice.Delta.ReasoningContent})
				}

				// Text content
				if choice.Delta.Content != "" {
					if reasoningStarted {
						send(&llm.ReasoningEndPart{ID: chunk.ID})
						reasoningStarted = false
					}
					if !textStarted {
						send(&llm.TextStartPart{ID: chunk.ID})
						textStarted = true
					}
					send(&llm.TextDeltaPart{ID: chunk.ID, Text: choice.Delta.Content})
				}

				// Tool calls
				for _, tc := range choice.Delta.ToolCalls {
					flush()
					stc, exists := pendingToolCalls[tc.Index]
					if !exists {
						id := tc.ID
						name := tc.Function.Name
						if id == "" {
							id = name
						}
						stc = &chatStreamingToolCall{id: id, name: name}
						pendingToolCalls[tc.Index] = stc
						send(&llm.ToolInputStartPart{ID: stc.id, ToolName: stc.name})
					}
					if tc.ID != "" && stc.id == "" {
						stc.id = tc.ID
					}
					if tc.Function.Name != "" && stc.name == "" {
						stc.name = tc.Function.Name
					}
					if tc.Function.Arguments != "" {
						stc.args += tc.Function.Arguments
						send(&llm.ToolInputDeltaPart{ID: stc.id, Delta: tc.Function.Arguments})
					}
				}

				// Finish reason
				if choice.FinishReason != "" {
					rawFinishReason = choice.FinishReason
					finishReason = mapChatFinishReason(choice.FinishReason)
				}
			}
			return nil
		})

		// Flush pending tool calls
		for _, stc := range pendingToolCalls {
			if !stc.finished {
				var input any
				if stc.args != "" {
					_ = json.Unmarshal([]byte(stc.args), &input)
				}
				send(&llm.ToolInputEndPart{ID: stc.id})
				send(&llm.StreamToolCallPart{
					ToolCallID: stc.id,
					ToolName:   stc.name,
					Input:      input,
				})
				stc.finished = true
			}
		}

		flush()

		if finishReason == "" {
			finishReason = llm.FinishReasonStop
		}

		send(&llm.FinishStepPart{
			FinishReason:    finishReason,
			RawFinishReason: rawFinishReason,
			Usage:           usage,
			Response: llm.ResponseMetadata{
				ID:      responseID,
				ModelID: responseModel,
			},
		})

		if streamErr != nil && streamErr != context.Canceled {
			send(&llm.ErrorPart{Error: fmt.Errorf("openai: chat stream failed: %w", streamErr)})
		}

		send(&llm.FinishPart{
			FinishReason:    finishReason,
			RawFinishReason: rawFinishReason,
			TotalUsage:      usage,
		})
	}()

	return &llm.StreamResult{Stream: ch}, nil
}

// ============================================================================
// Chat Completions — 类型转换
// ============================================================================

func paramsToChatRequest(params *llm.GenerateParams) (*ChatCompletionRequest, error) {
	req := &ChatCompletionRequest{
		Model:            params.Model.ID,
		Temperature:      params.Temperature,
		TopP:             params.TopP,
		MaxTokens:        params.MaxTokens,
		Seed:             params.Seed,
		FrequencyPenalty: params.FrequencyPenalty,
		PresencePenalty:  params.PresencePenalty,
	}

	if len(params.StopSequences) > 0 {
		data, _ := json.Marshal(params.StopSequences)
		req.Stop = data
	}

	// 消息转换（含 system）
	messages, err := convertUnifiedToChatMessages(params.System, params.Messages)
	if err != nil {
		return nil, err
	}
	req.Messages = messages

	// 工具转换
	if len(params.Tools) > 0 {
		req.Tools = convertUnifiedToChatTools(params.Tools)
		if params.ToolChoice != nil {
			req.ToolChoice = toJSONRawMessage(params.ToolChoice)
		}
	}

	// 响应格式
	if params.ResponseFormat != nil {
		req.ResponseFormat = convertUnifiedToChatFormat(params.ResponseFormat)
	}

	return req, nil
}

func convertUnifiedToChatMessages(system string, messages []llm.Message) ([]ChatMessage, error) {
	var out []ChatMessage

	if system != "" {
		out = append(out, ChatMessage{
			Role:    RoleSystem,
			Content: json.RawMessage(quoteJSONString(system)),
		})
	}

	for _, msg := range messages {
		switch msg.Role {
		case llm.MessageRoleSystem:
			text := llm.TextFromParts(msg.Content)
			out = append(out, ChatMessage{
				Role:    RoleSystem,
				Content: json.RawMessage(quoteJSONString(text)),
			})

		case llm.MessageRoleUser:
			var hasImage bool
			var parts []ChatContentPart
			for _, p := range msg.Content {
				switch pp := p.(type) {
				case llm.TextPart:
					parts = append(parts, ChatContentPart{Type: "text", Text: pp.Text})
				case llm.ImagePart:
					parts = append(parts, ChatContentPart{Type: "image_url", ImageURL: &ChatImageURL{URL: pp.Image}})
					hasImage = true
				}
			}
			if hasImage && len(parts) > 0 {
				data, _ := json.Marshal(parts)
				out = append(out, ChatMessage{Role: RoleUser, Content: data})
			} else {
				text := llm.TextFromParts(msg.Content)
				out = append(out, ChatMessage{Role: RoleUser, Content: json.RawMessage(quoteJSONString(text))})
			}

		case llm.MessageRoleAssistant:
			m := ChatMessage{Role: RoleAssistant}
			var textContent string
			for _, p := range msg.Content {
				switch pp := p.(type) {
				case llm.TextPart:
					textContent += pp.Text
				case llm.ToolCallPart:
					args, _ := json.Marshal(pp.Input)
					m.ToolCalls = append(m.ToolCalls, ChatToolCall{
						ID:   pp.ToolCallID,
						Type: "function",
						Function: ChatFunctionCall{
							Name:      pp.ToolName,
							Arguments: string(args),
						},
					})
				}
			}
			// 当有 tool_calls 且无文本内容时，content 应为 null（BigModel 等供应商要求）
			if len(m.ToolCalls) > 0 && textContent == "" {
				m.Content = json.RawMessage("null")
			} else {
				m.Content = json.RawMessage(quoteJSONString(textContent))
			}
			out = append(out, m)

		case llm.MessageRoleTool:
			for _, p := range msg.Content {
				if trp, ok := p.(llm.ToolResultPart); ok {
					resultStr, _ := json.Marshal(trp.Result)
					out = append(out, ChatMessage{
						Role:       RoleTool,
						ToolCallID: trp.ToolCallID,
						Content:    json.RawMessage(quoteJSONString(string(resultStr))),
					})
				}
			}
		}
	}
	return out, nil
}

func convertUnifiedToChatTools(tools []llm.Tool) []ChatTool {
	out := make([]ChatTool, 0, len(tools))
	for _, t := range tools {
		params, _ := json.Marshal(t.Parameters)
		out = append(out, ChatTool{
			Type: "function",
			Function: ChatToolFunction{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  params,
			},
		})
	}
	return out
}

func convertUnifiedToChatFormat(rf *llm.ResponseFormat) *ChatResponseFormat {
	switch rf.Type {
	case llm.ResponseFormatJSONObject:
		return &ChatResponseFormat{Type: "json_object"}
	case llm.ResponseFormatJSONSchema:
		if m, ok := rf.JSONSchema.(map[string]any); ok {
			name, _ := m["name"].(string)
			schema, _ := json.Marshal(m["schema"])
			return &ChatResponseFormat{
				Type: "json_schema",
				JSONSchema: &ChatJSONSchemaConfig{
					Name:   name,
					Schema: schema,
				},
			}
		}
		return &ChatResponseFormat{Type: "json_object"}
	default:
		return &ChatResponseFormat{Type: "text"}
	}
}

func chatResponseToResult(resp *ChatCompletionResponse) *llm.GenerateResult {
	result := &llm.GenerateResult{
		Response: llm.ResponseMetadata{
			ID:      resp.ID,
			ModelID: resp.Model,
		},
	}

	if resp.Usage != nil {
		result.Usage = llm.Usage{
			InputTokens:  resp.Usage.PromptTokens,
			OutputTokens: resp.Usage.CompletionTokens,
			TotalTokens:  resp.Usage.TotalTokens,
		}
	}

	if len(resp.Choices) > 0 {
		choice := resp.Choices[0]
		result.Text = choice.Message.ContentStr()
		result.Reasoning = choice.Message.ReasoningContent
		result.FinishReason = mapChatFinishReason(choice.FinishReason)
		result.RawFinishReason = choice.FinishReason

		for _, tc := range choice.Message.ToolCalls {
			var input any
			if tc.Function.Arguments != "" {
				_ = json.Unmarshal([]byte(tc.Function.Arguments), &input)
			}
			result.ToolCalls = append(result.ToolCalls, llm.ToolCall{
				ToolCallID: tc.ID,
				ToolName:   tc.Function.Name,
				Input:      input,
			})
		}
	}

	return result
}

func mapChatFinishReason(reason string) llm.FinishReason {
	switch reason {
	case "stop":
		return llm.FinishReasonStop
	case "length":
		return llm.FinishReasonLength
	case "tool_calls":
		return llm.FinishReasonToolCalls
	case "content_filter":
		return llm.FinishReasonContentFilter
	default:
		return llm.FinishReasonOther
	}
}
