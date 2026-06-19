package grok

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/kasuganosora/thinkbot/llm"
	"github.com/kasuganosora/thinkbot/util/errs"
)

// ============================================================================
// Provider 接口适配器
// ============================================================================

// Name 实现 llm.Provider。
func (c *Client) Name() string { return "grok" }

// DoGenerate 将统一 GenerateParams 转换为 xAI Chat Completions 请求并返回统一 GenerateResult。
func (c *Client) DoGenerate(ctx context.Context, params llm.GenerateParams) (*llm.GenerateResult, error) {
	if params.Model == nil {
		return nil, fmt.Errorf("grok: model is required")
	}

	req, err := paramsToGrokRequest(&params)
	if err != nil {
		return nil, err
	}

	resp, err := c.DoChatCompletion(ctx, *req)
	if err != nil {
		return nil, err
	}

	return grokResponseToResult(resp), nil
}

// DoStream 将统一 GenerateParams 转换为 xAI 流式请求并返回统一 StreamResult。
func (c *Client) DoStream(ctx context.Context, params llm.GenerateParams) (*llm.StreamResult, error) {
	if params.Model == nil {
		return nil, fmt.Errorf("grok: model is required")
	}

	req, err := paramsToGrokRequest(&params)
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
			pendingToolCalls = map[int]*grokStreamingToolCall{}
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

		streamErr := c.DoStreamChatCompletion(ctx, *req, StreamConfig{}, func(chunk ChatCompletionResponse) error {
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
					// Use index-based key for reliability across streaming deltas.
					// Subsequent delta frames for the same tool call share the same index
					// but may omit id and function.name.
					stc, exists := pendingToolCalls[tc.Index]
					if !exists {
						id := tc.ID
						name := tc.Function.Name
						if id == "" {
							id = name
						}
						stc = &grokStreamingToolCall{id: id, name: name}
						pendingToolCalls[tc.Index] = stc
						send(&llm.ToolInputStartPart{ID: stc.id, ToolName: stc.name})
					}
					// Update id/name if this frame carries them (first frame usually does)
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
					rawFinishReason = string(choice.FinishReason)
					finishReason = mapGrokFinishReason(choice.FinishReason)
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
			send(&llm.ErrorPart{Error: errs.Wrap(streamErr, "grok: stream failed")})
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
// 类型转换
// ============================================================================

type grokStreamingToolCall struct {
	id       string
	name     string
	args     string
	finished bool
}

func paramsToGrokRequest(params *llm.GenerateParams) (*ChatCompletionRequest, error) {
	req := &ChatCompletionRequest{
		Model:       params.Model.ID,
		Temperature: params.Temperature,
		TopP:        params.TopP,
		MaxTokens:   params.MaxTokens,
		Seed:        params.Seed,
	}

	if len(params.StopSequences) > 0 {
		data, _ := json.Marshal(params.StopSequences)
		req.Stop = data
	}

	if params.FrequencyPenalty != nil {
		req.FrequencyPenalty = params.FrequencyPenalty
	}
	if params.PresencePenalty != nil {
		req.PresencePenalty = params.PresencePenalty
	}

	// 消息转换
	messages, err := convertUnifiedToGrokMessages(params.System, params.Messages)
	if err != nil {
		return nil, err
	}
	req.Messages = messages

	// 工具转换
	if len(params.Tools) > 0 {
		req.Tools = convertUnifiedToGrokTools(params.Tools)
		if params.ToolChoice != nil {
			req.ToolChoice = toJSONRaw(params.ToolChoice)
		}
	}

	// 响应格式
	if params.ResponseFormat != nil {
		req.ResponseFormat = convertUnifiedToGrokFormat(params.ResponseFormat)
	}

	// 推理配置
	if params.ReasoningEffort != nil {
		req.ReasoningEffort = ReasoningEffort(*params.ReasoningEffort)
	}

	return req, nil
}

func convertUnifiedToGrokMessages(system string, messages []llm.Message) ([]Message, error) {
	var out []Message

	if system != "" {
		out = append(out, Message{
			Role:    RoleSystem,
			Content: json.RawMessage(quoteJSONString(system)),
		})
	}

	for _, msg := range messages {
		switch msg.Role {
		case llm.MessageRoleSystem:
			text := llm.TextFromParts(msg.Content)
			out = append(out, Message{
				Role:    RoleSystem,
				Content: json.RawMessage(quoteJSONString(text)),
			})

		case llm.MessageRoleUser:
			var hasImage bool
			var parts []ContentPart
			for _, p := range msg.Content {
				switch pp := p.(type) {
				case llm.TextPart:
					parts = append(parts, ContentPart{Type: ContentTypeText, Text: pp.Text})
				case llm.ImagePart:
					parts = append(parts, ContentPart{Type: ContentTypeImageURL, ImageURL: &ImageURL{URL: pp.Image}})
					hasImage = true
				}
			}
			if hasImage && len(parts) > 0 {
				data, _ := json.Marshal(parts)
				out = append(out, Message{Role: RoleUser, Content: data})
			} else {
				text := llm.TextFromParts(msg.Content)
				out = append(out, Message{Role: RoleUser, Content: json.RawMessage(quoteJSONString(text))})
			}

		case llm.MessageRoleAssistant:
			m := Message{Role: RoleAssistant}
			var textContent string
			for _, p := range msg.Content {
				switch pp := p.(type) {
				case llm.TextPart:
					textContent += pp.Text
				case llm.ToolCallPart:
					args, _ := json.Marshal(pp.Input)
					m.ToolCalls = append(m.ToolCalls, ToolCall{
						ID:   pp.ToolCallID,
						Type: "function",
						Function: FunctionCall{
							Name:      pp.ToolName,
							Arguments: string(args),
						},
					})
				}
			}
			m.Content = json.RawMessage(quoteJSONString(textContent))
			out = append(out, m)

		case llm.MessageRoleTool:
			for _, p := range msg.Content {
				if trp, ok := p.(llm.ToolResultPart); ok {
					resultStr, _ := json.Marshal(trp.Result)
					// API 要求 tool 消息的 content 是 JSON 字符串而非原始对象
					out = append(out, Message{
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

func convertUnifiedToGrokTools(tools []llm.Tool) []Tool {
	out := make([]Tool, 0, len(tools))
	for _, t := range tools {
		params, _ := json.Marshal(t.Parameters)
		out = append(out, Tool{
			Type: "function",
			Function: ToolFunction{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  params,
			},
		})
	}
	return out
}

func convertUnifiedToGrokFormat(rf *llm.ResponseFormat) *ResponseFormat {
	switch rf.Type {
	case llm.ResponseFormatJSONObject:
		return &ResponseFormat{Type: ResponseFormatJSONObject}
	case llm.ResponseFormatJSONSchema:
		if m, ok := rf.JSONSchema.(map[string]any); ok {
			name, _ := m["name"].(string)
			schema, _ := json.Marshal(m["schema"])
			return &ResponseFormat{
				Type: ResponseFormatJSONSchema,
				JSONSchema: &JSONSchemaConfig{
					Name:   name,
					Schema: schema,
				},
			}
		}
		return &ResponseFormat{Type: ResponseFormatJSONObject}
	default:
		return &ResponseFormat{Type: ResponseFormatText}
	}
}

func grokResponseToResult(resp *ChatCompletionResponse) *llm.GenerateResult {
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

		result.FinishReason = mapGrokFinishReason(choice.FinishReason)
		result.RawFinishReason = string(choice.FinishReason)

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

func mapGrokFinishReason(reason FinishReason) llm.FinishReason {
	switch reason {
	case FinishReasonStop:
		return llm.FinishReasonStop
	case FinishReasonLength:
		return llm.FinishReasonLength
	case FinishReasonToolCalls:
		return llm.FinishReasonToolCalls
	case FinishReasonContentFilter:
		return llm.FinishReasonContentFilter
	default:
		return llm.FinishReasonOther
	}
}

// ContentStr 返回 Message.Content 的字符串形式。
func (m Message) ContentStr() string {
	var s string
	if json.Unmarshal(m.Content, &s) == nil {
		return s
	}
	return string(m.Content)
}

func toJSONRaw(v any) json.RawMessage {
	if v == nil {
		return nil
	}
	data, _ := json.Marshal(v)
	return data
}
