package openai

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/kasuganosora/thinkbot/llm"
)

// ============================================================================
// Provider 接口适配器
// ============================================================================

// Name 实现 llm.Provider。
func (c *Client) Name() string { return "openai" }

// DoGenerate 将统一 GenerateParams 转换为 OpenAI Responses API 请求并返回统一 GenerateResult。
func (c *Client) DoGenerate(ctx context.Context, params llm.GenerateParams) (*llm.GenerateResult, error) {
	if params.Model == nil {
		return nil, fmt.Errorf("openai: model is required")
	}

	req, err := paramsToOpenAIRequest(&params)
	if err != nil {
		return nil, err
	}

	resp, err := c.DoCreateResponse(ctx, *req)
	if err != nil {
		return nil, err
	}

	return openAIResponseToResult(resp), nil
}

// DoStream 将统一 GenerateParams 转换为 OpenAI 流式请求并返回统一 StreamResult。
func (c *Client) DoStream(ctx context.Context, params llm.GenerateParams) (*llm.StreamResult, error) {
	if params.Model == nil {
		return nil, fmt.Errorf("openai: model is required")
	}

	req, err := paramsToOpenAIRequest(&params)
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
			hasFunctionCall  bool

			pendingToolCalls = map[int]*openAIStreamingToolCall{}
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

		streamErr := c.DoStreamResponse(ctx, *req, StreamConfig{}, func(event StreamEvent) error {
			switch event.Type {
			case EventResponseCreated, EventResponseInProgress:
				if event.Response != nil {
					responseID = event.Response.ID
					responseModel = event.Response.Model
				}

			case EventResponseOutputItemAdded:
				if event.Item == nil {
					return nil
				}
				switch event.Item.Type {
				case TypeMessage:
					if !textStarted {
						send(&llm.TextStartPart{ID: event.Item.ID})
						textStarted = true
					}
				case TypeReasoning:
					if !reasoningStarted {
						send(&llm.ReasoningStartPart{ID: event.Item.ID})
						reasoningStarted = true
					}
				case TypeFunctionCall:
					flush()
					callID := event.Item.CallID
					if callID == "" {
						callID = event.Item.ID
					}
					pendingToolCalls[event.OutputIndex] = &openAIStreamingToolCall{
						id:   callID,
						name: event.Item.Name,
					}
					send(&llm.ToolInputStartPart{ID: callID, ToolName: event.Item.Name})
				}

			case EventResponseOutputTextDelta:
				if reasoningStarted {
					send(&llm.ReasoningEndPart{ID: responseID})
					reasoningStarted = false
				}
				if !textStarted {
					send(&llm.TextStartPart{ID: event.ItemID})
					textStarted = true
				}
				send(&llm.TextDeltaPart{ID: event.ItemID, Text: event.Delta})

			case EventResponseReasoningDelta:
				if !reasoningStarted {
					send(&llm.ReasoningStartPart{ID: event.ItemID})
					reasoningStarted = true
				}
				send(&llm.ReasoningDeltaPart{ID: event.ItemID, Text: event.Delta})

			case EventResponseFunctionCallArgumentsDelta:
				if stc, ok := pendingToolCalls[event.OutputIndex]; ok {
					stc.args += event.Delta
					send(&llm.ToolInputDeltaPart{ID: stc.id, Delta: event.Delta})
				}

			case EventResponseFunctionCallArgumentsDone:
				if stc, ok := pendingToolCalls[event.OutputIndex]; ok && !stc.finished {
					stc.finished = true
				}

			case EventResponseOutputItemDone:
				if event.Item != nil && event.Item.Type == TypeFunctionCall {
					hasFunctionCall = true
					stc, ok := pendingToolCalls[event.OutputIndex]
					if ok && !stc.finished {
						var input any
						args := event.Item.Arguments
						if args == "" {
							args = stc.args
						}
						if args != "" {
							_ = json.Unmarshal([]byte(args), &input)
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

			case EventResponseCompleted, EventResponseIncomplete:
				if event.Response != nil {
					if event.Response.Usage != nil {
						usage = convertOpenAIUsage(event.Response.Usage)
					}
					if event.Response.IncompleteDetails != nil {
						rawFinishReason = event.Response.IncompleteDetails.Reason
					}
				}

			case EventResponseFailed:
				if event.Response != nil && event.Response.Error != nil {
					send(&llm.ErrorPart{Error: fmt.Errorf("openai: %s", event.Response.Error.Message)})
				}
			}
			return nil
		})

		flush()

		// Flush any pending tool calls
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

		finishReason = mapOpenAIFinishReason(rawFinishReason, hasFunctionCall)

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
			send(&llm.ErrorPart{Error: fmt.Errorf("openai: stream failed: %w", streamErr)})
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

type openAIStreamingToolCall struct {
	id       string
	name     string
	args     string
	finished bool
}

func paramsToOpenAIRequest(params *llm.GenerateParams) (*CreateResponseRequest, error) {
	req := &CreateResponseRequest{
		Model:           params.Model.ID,
		Temperature:     params.Temperature,
		TopP:            params.TopP,
		MaxOutputTokens: params.MaxTokens,
	}

	if params.System != "" {
		req.Instructions = params.System
	}

	// 消息转换为 input items
	items, err := convertUnifiedToOpenAIInput(params.Messages)
	if err != nil {
		return nil, err
	}
	if len(items) > 0 {
		data, _ := json.Marshal(items)
		req.Input = data
	}

	// 工具转换
	if len(params.Tools) > 0 {
		tools, err := convertUnifiedToOpenAITools(params.Tools)
		if err != nil {
			return nil, err
		}
		req.Tools = tools
		if params.ToolChoice != nil {
			req.ToolChoice = toJSONRawMessage(params.ToolChoice)
		}
	}

	// 响应格式
	if params.ResponseFormat != nil {
		switch params.ResponseFormat.Type {
		case llm.ResponseFormatJSONObject:
			req.Text = &TextConfig{Format: &TextFormatConfig{Type: "json_object"}}
		case llm.ResponseFormatJSONSchema:
			req.Text = convertUnifiedToOpenAISchema(params.ResponseFormat.JSONSchema)
		}
	}

	// 推理配置
	if params.ReasoningEffort != nil {
		req.Reasoning = &ReasoningConfig{Effort: *params.ReasoningEffort}
	}

	return req, nil
}

func convertUnifiedToOpenAIInput(messages []llm.Message) ([]InputItem, error) {
	var items []InputItem
	for _, msg := range messages {
		switch msg.Role {
		case llm.MessageRoleSystem:
			text := llm.TextFromParts(msg.Content)
			items = append(items, InputText(RoleSystem, text))

		case llm.MessageRoleUser:
			var hasImage bool
			var parts []ContentPart
			for _, p := range msg.Content {
				switch pp := p.(type) {
				case llm.TextPart:
					parts = append(parts, ContentPart{Type: ContentTypeInputText, Text: pp.Text})
				case llm.ImagePart:
					parts = append(parts, ContentPart{Type: ContentTypeInputImage, ImageURL: pp.Image})
					hasImage = true
				}
			}
			if hasImage && len(parts) > 0 {
				data, _ := json.Marshal(parts)
				items = append(items, InputItem{Type: TypeMessage, Role: RoleUser, Content: data})
			} else {
				items = append(items, InputText(RoleUser, llm.TextFromParts(msg.Content)))
			}

		case llm.MessageRoleAssistant:
			for _, p := range msg.Content {
				switch pp := p.(type) {
				case llm.TextPart:
					items = append(items, InputText(RoleAssistant, pp.Text))
				case llm.ReasoningPart:
					items = append(items, InputItem{
						Type:    TypeReasoning,
						Summary: toJSONRawMessage([]SummaryItem{{Type: ContentTypeSummaryText, Text: pp.Text}}),
					})
				case llm.ToolCallPart:
					args, _ := json.Marshal(pp.Input)
					items = append(items, InputItem{
						Type:      TypeFunctionCall,
						CallID:    pp.ToolCallID,
						Name:      pp.ToolName,
						Arguments: string(args),
					})
				}
			}

		case llm.MessageRoleTool:
			for _, p := range msg.Content {
				if trp, ok := p.(llm.ToolResultPart); ok {
					output, _ := json.Marshal(trp.Result)
					items = append(items, InputItem{
						Type:   TypeFunctionCallOutput,
						CallID: trp.ToolCallID,
						Output: string(output),
					})
				}
			}
		}
	}
	return items, nil
}

func convertUnifiedToOpenAITools(tools []llm.Tool) ([]json.RawMessage, error) {
	out := make([]json.RawMessage, 0, len(tools))
	for _, t := range tools {
		params, _ := json.Marshal(t.Parameters)
		tool := FunctionTool{
			Type:        ToolTypeFunction,
			Name:        t.Name,
			Description: t.Description,
			Parameters:  params,
		}
		data, _ := json.Marshal(tool)
		out = append(out, data)
	}
	return out, nil
}

func convertUnifiedToOpenAISchema(schema any) *TextConfig {
	if m, ok := schema.(map[string]any); ok {
		name, _ := m["name"].(string)
		rawSchema, _ := json.Marshal(m["schema"])
		var strict *bool
		if s, ok := m["strict"].(bool); ok {
			strict = &s
		}
		return &TextConfig{
			Format: &TextFormatConfig{
				Type:   "json_schema",
				Name:   name,
				Schema: rawSchema,
				Strict: strict,
			},
		}
	}
	// Fallback: json_object
	return &TextConfig{Format: &TextFormatConfig{Type: "json_object"}}
}

func openAIResponseToResult(resp *Response) *llm.GenerateResult {
	result := &llm.GenerateResult{
		Response: llm.ResponseMetadata{
			ID:      resp.ID,
			ModelID: resp.Model,
		},
	}

	if resp.Usage != nil {
		result.Usage = convertOpenAIUsage(resp.Usage)
	}

	var hasFunctionCall bool
	for i := range resp.Output {
		item := &resp.Output[i]
		switch item.Type {
		case TypeMessage:
			result.Text += extractOutputText(item.Content)
		case TypeReasoning:
			result.Reasoning += extractReasoningText(item.Summary)
		case TypeFunctionCall:
			hasFunctionCall = true
			var input any
			if item.Arguments != "" {
				_ = json.Unmarshal([]byte(item.Arguments), &input)
			}
			callID := item.CallID
			if callID == "" {
				callID = item.ID
			}
			result.ToolCalls = append(result.ToolCalls, llm.ToolCall{
				ToolCallID: callID,
				ToolName:   item.Name,
				Input:      input,
			})
		}
	}

	var incompleteReason string
	if resp.IncompleteDetails != nil {
		incompleteReason = resp.IncompleteDetails.Reason
	}
	result.FinishReason = mapOpenAIFinishReason(incompleteReason, hasFunctionCall)
	result.RawFinishReason = incompleteReason

	return result
}

func convertOpenAIUsage(u *ResponseUsage) llm.Usage {
	inputTokens := u.InputTokens
	outputTokens := u.OutputTokens
	cachedTokens := 0
	reasoningTokens := 0
	if u.InputTokensDetails != nil {
		cachedTokens = u.InputTokensDetails.CachedTokens
	}
	if u.OutputTokensDetails != nil {
		reasoningTokens = u.OutputTokensDetails.ReasoningTokens
	}
	return llm.Usage{
		InputTokens:       inputTokens,
		OutputTokens:      outputTokens,
		TotalTokens:       inputTokens + outputTokens,
		ReasoningTokens:   reasoningTokens,
		CachedInputTokens: cachedTokens,
		InputTokenDetails: llm.InputTokenDetail{
			CacheReadTokens: cachedTokens,
			NoCacheTokens:   max(0, inputTokens-cachedTokens),
		},
		OutputTokenDetails: llm.OutputTokenDetail{
			ReasoningTokens: reasoningTokens,
			TextTokens:      max(0, outputTokens-reasoningTokens),
		},
	}
}

func mapOpenAIFinishReason(incompleteReason string, hasFunctionCall bool) llm.FinishReason {
	switch incompleteReason {
	case "max_output_tokens":
		return llm.FinishReasonLength
	case "content_filter":
		return llm.FinishReasonContentFilter
	case "":
		if hasFunctionCall {
			return llm.FinishReasonToolCalls
		}
		return llm.FinishReasonStop
	default:
		if hasFunctionCall {
			return llm.FinishReasonToolCalls
		}
		return llm.FinishReasonOther
	}
}

func extractOutputText(content json.RawMessage) string {
	var contents []MessageContent
	if err := json.Unmarshal(content, &contents); err != nil {
		return ""
	}
	var text string
	for _, c := range contents {
		if c.Type == ContentTypeOutputText || c.Type == ContentTypeText {
			text += c.Text
		}
	}
	return text
}

func extractReasoningText(summary json.RawMessage) string {
	var items []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(summary, &items); err != nil {
		return ""
	}
	var text string
	for _, item := range items {
		if item.Type == ContentTypeSummaryText || item.Type == ContentTypeReasoningText {
			text += item.Text
		}
	}
	return text
}

func toJSONRawMessage(v any) json.RawMessage {
	if v == nil {
		return nil
	}
	data, _ := json.Marshal(v)
	return data
}
