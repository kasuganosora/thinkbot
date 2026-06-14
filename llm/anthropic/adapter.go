package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kasuganosora/thinkbot/llm"
)

// ============================================================================
// Provider 接口适配器
// ============================================================================

// Name 实现 llm.Provider。
func (c *Client) Name() string { return "anthropic" }

// DoGenerate 将统一 GenerateParams 转换为 Anthropic Messages API 请求并返回统一 GenerateResult。
func (c *Client) DoGenerate(ctx context.Context, params llm.GenerateParams) (*llm.GenerateResult, error) {
	if params.Model == nil {
		return nil, fmt.Errorf("anthropic: model is required")
	}

	req, err := paramsToAnthropicRequest(&params)
	if err != nil {
		return nil, err
	}

	resp, err := c.CreateMessage(ctx, *req)
	if err != nil {
		return nil, err
	}

	return anthropicResponseToResult(resp), nil
}

// DoStream 将统一 GenerateParams 转换为 Anthropic 流式请求并返回统一 StreamResult。
func (c *Client) DoStream(ctx context.Context, params llm.GenerateParams) (*llm.StreamResult, error) {
	if params.Model == nil {
		return nil, fmt.Errorf("anthropic: model is required")
	}

	req, err := paramsToAnthropicRequest(&params)
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
			usage            llm.Usage
			responseID       string
			responseModel    string

			pendingToolCalls = map[int]*streamingToolCall{}
			textBlockIDs     = map[int]string{}
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

		streamErr := c.StreamMessage(ctx, *req, func(event StreamEvent) error {
			switch event.Type {
			case EventMessageStart:
				if event.Message != nil {
					responseID = event.Message.ID
					responseModel = event.Message.Model
					if event.Message.Usage.InputTokens > 0 || event.Message.Usage.OutputTokens > 0 {
						usage.InputTokens = event.Message.Usage.InputTokens
						usage.OutputTokens = event.Message.Usage.OutputTokens
					}
				}

			case EventContentBlockStart:
				if event.Index == nil || event.ContentBlock == nil {
					return nil
				}
				idx := *event.Index
				switch event.ContentBlock.Type {
				case ContentTypeText:
					if !textStarted {
						send(&llm.TextStartPart{ID: event.ContentBlock.ID})
						textStarted = true
						textBlockIDs[idx] = event.ContentBlock.ID
					}
				case ContentTypeThinking:
					if !reasoningStarted {
						send(&llm.ReasoningStartPart{ID: event.ContentBlock.ID})
						reasoningStarted = true
					}
				case ContentTypeToolUse:
					flush()
					pendingToolCalls[idx] = &streamingToolCall{
						id:   event.ContentBlock.ID,
						name: event.ContentBlock.Name,
					}
					send(&llm.ToolInputStartPart{
						ID:       event.ContentBlock.ID,
						ToolName: event.ContentBlock.Name,
					})
				}

			case EventContentBlockDelta:
				if event.Index == nil || event.Delta == nil {
					return nil
				}
				idx := *event.Index
				switch event.Delta.Type {
				case "text_delta":
					send(&llm.TextDeltaPart{ID: textBlockIDs[idx], Text: event.Delta.Text})
				case "thinking_delta":
					if !reasoningStarted {
						send(&llm.ReasoningStartPart{})
						reasoningStarted = true
					}
					send(&llm.ReasoningDeltaPart{Text: event.Delta.Thinking})
				case "input_json_delta":
					if stc, ok := pendingToolCalls[idx]; ok {
						stc.args += event.Delta.PartialJSON
						send(&llm.ToolInputDeltaPart{ID: stc.id, Delta: event.Delta.PartialJSON})
					}
				}

			case EventContentBlockStop:
				if event.Index == nil {
					return nil
				}
				idx := *event.Index
				if stc, ok := pendingToolCalls[idx]; ok && !stc.finished {
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

			case EventMessageDelta:
				if event.Delta != nil {
					switch event.Delta.StopReason {
					case StopReasonEndTurn:
						finishReason = llm.FinishReasonStop
					case StopReasonMaxTokens:
						finishReason = llm.FinishReasonLength
					case StopReasonToolUse:
						finishReason = llm.FinishReasonToolCalls
					case StopReasonStopSequence:
						finishReason = llm.FinishReasonStop
					}
				}
				if event.Usage != nil {
					usage.OutputTokens = event.Usage.OutputTokens
					usage.CachedInputTokens = event.Usage.CacheReadTokens
					usage.InputTokenDetails.CacheReadTokens = event.Usage.CacheReadTokens
					usage.InputTokenDetails.CacheWriteTokens = event.Usage.CacheCreationTokens
				}

			case EventMessageStop:
				// 正常结束

			case EventError:
				if event.Error != nil {
					return event.Error
				}
			}
			return nil
		})

		flush()

		// Flush any pending tool calls (e.g. stream ended before EventContentBlockStop)
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

		if finishReason == "" {
			finishReason = llm.FinishReasonStop
		}
		usage.TotalTokens = usage.InputTokens + usage.OutputTokens

		send(&llm.FinishStepPart{
			FinishReason: finishReason,
			Usage:        usage,
			Response: llm.ResponseMetadata{
				ID:      responseID,
				ModelID: responseModel,
			},
		})

		if streamErr != nil && streamErr != context.Canceled {
			send(&llm.ErrorPart{Error: fmt.Errorf("anthropic: stream failed: %w", streamErr)})
		}

		send(&llm.FinishPart{
			FinishReason: finishReason,
			TotalUsage:   usage,
		})
	}()

	return &llm.StreamResult{Stream: ch}, nil
}

// ListModelsUnified 返回统一 llm.Model 列表。
func (c *Client) ListModelsUnified(ctx context.Context) ([]llm.Model, error) {
	resp, err := c.ListModels(ctx, nil)
	if err != nil {
		return nil, err
	}
	models := make([]llm.Model, 0, len(resp.Data))
	for _, m := range resp.Data {
		models = append(models, llm.Model{
			ID:          m.ID,
			DisplayName: m.DisplayName,
			Type:        llm.ModelTypeChat,
		})
	}
	return models, nil
}

// ============================================================================
// 类型转换
// ============================================================================

type streamingToolCall struct {
	id       string
	name     string
	args     string
	finished bool
}

// Reasoning budget token values for Anthropic extended thinking.
const (
	reasoningBudgetHigh   = 32000
	reasoningBudgetMedium = 16000
	reasoningBudgetLow    = 8000
)

// DefaultMaxTokens is the default max_tokens when params.MaxTokens is not set.
const DefaultMaxTokens = 4096

func paramsToAnthropicRequest(params *llm.GenerateParams) (*MessageRequest, error) {
	req := &MessageRequest{
		Model:       params.Model.ID,
		Temperature: params.Temperature,
		TopP:        params.TopP,
	}

	if params.System != "" {
		req.System = SystemText(params.System)
	}

	if params.MaxTokens != nil {
		req.MaxTokens = *params.MaxTokens
	} else {
		req.MaxTokens = DefaultMaxTokens
	}

	if len(params.StopSequences) > 0 {
		req.StopSequences = params.StopSequences
	}

	// 消息转换
	messages, err := convertUnifiedMessages(params.Messages)
	if err != nil {
		return nil, err
	}
	req.Messages = messages

	// 工具转换
	if len(params.Tools) > 0 {
		req.Tools = convertUnifiedTools(params.Tools)
		if params.ToolChoice != nil {
			req.ToolChoice = convertUnifiedToolChoice(params.ToolChoice)
		}
	}

	// 推理配置
	if params.ReasoningEffort != nil {
		effort := strings.ToLower(*params.ReasoningEffort)
		var budget int
		switch effort {
		case "high":
			budget = reasoningBudgetHigh
		case "medium":
			budget = reasoningBudgetMedium
		case "low", "minimal":
			budget = reasoningBudgetLow
		default:
			budget = reasoningBudgetMedium
		}
		req.Thinking = ThinkingEnabled(budget)
	}

	return req, nil
}

func convertUnifiedMessages(messages []llm.Message) ([]Message, error) {
	var out []Message
	for _, msg := range messages {
		switch msg.Role {
		case llm.MessageRoleSystem:
			// Anthropic 通过 system 字段处理系统消息
			// 如果统一消息中包含系统角色，这里跳过（已在 params.System 中处理）
			continue

		case llm.MessageRoleTool:
			// 工具结果消息
			for _, part := range msg.Content {
				if trp, ok := part.(llm.ToolResultPart); ok {
					out = append(out, Message{
						Role: RoleUser,
						Content: MessageContent{{
							Type:          ContentTypeToolResult,
							ToolUseID:     trp.ToolCallID,
							ResultContent: toJSONRaw(trp.Result),
							IsError:       trp.IsError,
						}},
					})
				}
			}

		case llm.MessageRoleUser:
			var blocks []ContentBlock
			for _, part := range msg.Content {
				switch p := part.(type) {
				case llm.TextPart:
					blocks = append(blocks, ContentBlock{Type: ContentTypeText, Text: p.Text, CacheControl: convertCacheControl(p.CacheControl)})
				case llm.ImagePart:
					blocks = append(blocks, ContentBlock{Type: ContentTypeImage, Source: convertImageSource(p)})
				case llm.FilePart:
					blocks = append(blocks, ContentBlock{Type: ContentTypeDocument, Source: convertFileSource(p)})
				}
			}
			if len(blocks) > 0 {
				out = append(out, Message{Role: RoleUser, Content: MessageContent(blocks)})
			}

		case llm.MessageRoleAssistant:
			var blocks []ContentBlock
			for _, part := range msg.Content {
				switch p := part.(type) {
				case llm.TextPart:
					blocks = append(blocks, ContentBlock{Type: ContentTypeText, Text: p.Text})
				case llm.ReasoningPart:
					blocks = append(blocks, ContentBlock{Type: ContentTypeThinking, Thinking: p.Text})
				case llm.ToolCallPart:
					blocks = append(blocks, ContentBlock{
						Type:  ContentTypeToolUse,
						ID:    p.ToolCallID,
						Name:  p.ToolName,
						Input: toJSONRaw(p.Input),
					})
				}
			}
			if len(blocks) > 0 {
				out = append(out, Message{Role: RoleAssistant, Content: MessageContent(blocks)})
			}
		}
	}
	return out, nil
}

func convertUnifiedTools(tools []llm.Tool) []Tool {
	out := make([]Tool, 0, len(tools))
	for _, t := range tools {
		out = append(out, Tool{
			Name:         t.Name,
			Description:  t.Description,
			InputSchema:  t.Parameters,
			CacheControl: convertCacheControl(t.CacheControl),
		})
	}
	return out
}

func convertUnifiedToolChoice(choice any) *ToolChoice {
	switch v := choice.(type) {
	case string:
		switch v {
		case "auto":
			return &ToolChoice{Type: ToolChoiceAuto}
		case "none":
			return &ToolChoice{Type: ToolChoiceNone}
		case "required":
			return &ToolChoice{Type: ToolChoiceAny}
		}
	case map[string]any:
		if fn, ok := v["function"].(map[string]any); ok {
			if name, ok := fn["name"].(string); ok {
				return &ToolChoice{Type: ToolChoiceTool, Name: name}
			}
		}
	}
	return &ToolChoice{Type: ToolChoiceAuto}
}

func convertImageSource(p llm.ImagePart) *ImageSource {
	if strings.HasPrefix(p.Image, "http://") || strings.HasPrefix(p.Image, "https://") {
		return URLImageSource(p.Image)
	}
	return Base64ImageSource(p.MediaType, p.Image)
}

func convertFileSource(p llm.FilePart) *ImageSource {
	return Base64ImageSource(p.MediaType, p.Data)
}

func convertCacheControl(cc *llm.CacheControl) *CacheControl {
	if cc == nil {
		return nil
	}
	return &CacheControl{Type: cc.Type, TTL: cc.TTL}
}

func anthropicResponseToResult(resp *MessageResponse) *llm.GenerateResult {
	result := &llm.GenerateResult{
		Response: llm.ResponseMetadata{
			ID:      resp.ID,
			ModelID: resp.Model,
		},
	}

	result.Usage = llm.Usage{
		InputTokens:       resp.Usage.InputTokens,
		OutputTokens:      resp.Usage.OutputTokens,
		TotalTokens:       resp.Usage.InputTokens + resp.Usage.OutputTokens,
		CachedInputTokens: resp.Usage.CacheReadTokens,
		InputTokenDetails: llm.InputTokenDetail{
			CacheReadTokens:  resp.Usage.CacheReadTokens,
			CacheWriteTokens: resp.Usage.CacheCreationTokens,
			NoCacheTokens:    resp.Usage.InputTokens - resp.Usage.CacheReadTokens,
		},
	}

	var hasToolCall bool
	for _, block := range resp.Content {
		switch block.Type {
		case ContentTypeText:
			result.Text += block.Text
		case ContentTypeThinking:
			result.Reasoning += block.Thinking
		case ContentTypeToolUse:
			hasToolCall = true
			var input any
			if len(block.Input) > 0 {
				_ = json.Unmarshal(block.Input, &input)
			}
			result.ToolCalls = append(result.ToolCalls, llm.ToolCall{
				ToolCallID: block.ID,
				ToolName:   block.Name,
				Input:      input,
			})
		}
	}

	switch resp.StopReason {
	case StopReasonEndTurn:
		if hasToolCall {
			result.FinishReason = llm.FinishReasonToolCalls
		} else {
			result.FinishReason = llm.FinishReasonStop
		}
	case StopReasonMaxTokens:
		result.FinishReason = llm.FinishReasonLength
	case StopReasonToolUse:
		result.FinishReason = llm.FinishReasonToolCalls
	default:
		result.FinishReason = llm.FinishReasonStop
	}
	result.RawFinishReason = string(resp.StopReason)

	return result
}

func toJSONRaw(v any) json.RawMessage {
	if v == nil {
		return nil
	}
	data, _ := json.Marshal(v)
	return data
}
