package google

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
func (c *Client) Name() string { return "google" }

// DoGenerate 将统一 GenerateParams 转换为 Gemini generateContent 请求并返回统一 GenerateResult。
func (c *Client) DoGenerate(ctx context.Context, params llm.GenerateParams) (*llm.GenerateResult, error) {
	if params.Model == nil {
		return nil, fmt.Errorf("google: model is required")
	}

	req, err := paramsToGeminiRequest(&params)
	if err != nil {
		return nil, err
	}

	resp, err := c.GenerateContent(ctx, params.Model.ID, *req)
	if err != nil {
		return nil, err
	}

	return geminiResponseToResult(resp), nil
}

// DoStream 将统一 GenerateParams 转换为 Gemini 流式请求并返回统一 StreamResult。
func (c *Client) DoStream(ctx context.Context, params llm.GenerateParams) (*llm.StreamResult, error) {
	if params.Model == nil {
		return nil, fmt.Errorf("google: model is required")
	}

	req, err := paramsToGeminiRequest(&params)
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
			responseModel    string
		)

		flush := func() {
			if reasoningStarted {
				send(&llm.ReasoningEndPart{})
				reasoningStarted = false
			}
			if textStarted {
				send(&llm.TextEndPart{})
				textStarted = false
			}
		}

		streamErr := c.StreamGenerateContent(ctx, params.Model.ID, *req, func(resp GenerateContentResponse) error {
			if resp.ModelVersion != "" {
				responseModel = resp.ModelVersion
			}
			if resp.UsageMetadata != nil {
				usage = convertGeminiUsage(resp.UsageMetadata)
			}

			if len(resp.Candidates) == 0 {
				return nil
			}
			cand := resp.Candidates[0]

			if cand.FinishReason != "" {
				rawFinishReason = string(cand.FinishReason)
				finishReason = mapGeminiFinishReason(cand.FinishReason)
			}

			for _, part := range cand.Content.Parts {
				if part.Thought && part.Text != "" {
					// Reasoning content
					if !reasoningStarted {
						send(&llm.ReasoningStartPart{})
						reasoningStarted = true
					}
					send(&llm.ReasoningDeltaPart{Text: part.Text})
				} else if part.Text != "" {
					// Text content
					if reasoningStarted {
						send(&llm.ReasoningEndPart{})
						reasoningStarted = false
					}
					if !textStarted {
						send(&llm.TextStartPart{})
						textStarted = true
					}
					send(&llm.TextDeltaPart{Text: part.Text})
				} else if part.FunctionCall != nil {
					flush()
					args, _ := json.Marshal(part.FunctionCall.Args)
					callID := part.FunctionCall.ID
					if callID == "" {
						callID = part.FunctionCall.Name
					}
					send(&llm.ToolInputStartPart{ID: callID, ToolName: part.FunctionCall.Name})
					send(&llm.ToolInputDeltaPart{ID: callID, Delta: string(args)})
					send(&llm.ToolInputEndPart{ID: callID})
					var input any
					if len(args) > 0 {
						_ = json.Unmarshal(args, &input)
					}
					send(&llm.StreamToolCallPart{
						ToolCallID: callID,
						ToolName:   part.FunctionCall.Name,
						Input:      input,
					})
				}
			}

			// Sources from grounding
			if cand.GroundingMetadata != nil {
				for _, chunk := range cand.GroundingMetadata.GroundingChunks {
					if chunk.Web != nil {
						send(&llm.StreamSourcePart{
							Source: llm.Source{
								SourceType: "url",
								URL:        chunk.Web.URI,
								Title:      chunk.Web.Title,
							},
						})
					}
				}
			}
			return nil
		})

		flush()

		if finishReason == "" {
			finishReason = llm.FinishReasonStop
		}

		send(&llm.FinishStepPart{
			FinishReason:    finishReason,
			RawFinishReason: rawFinishReason,
			Usage:           usage,
			Response: llm.ResponseMetadata{
				ModelID: responseModel,
			},
		})

		if streamErr != nil && streamErr != context.Canceled {
			send(&llm.ErrorPart{Error: fmt.Errorf("google: stream failed: %w", streamErr)})
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

func paramsToGeminiRequest(params *llm.GenerateParams) (*GenerateContentRequest, error) {
	req := &GenerateContentRequest{}

	// 系统指令
	if params.System != "" {
		req.SystemInstruction = &Content{
			Role:  RoleUser,
			Parts: []Part{TextPart(params.System)},
		}
	}

	// 消息转换
	contents, err := convertUnifiedToGeminiContents(params.Messages)
	if err != nil {
		return nil, err
	}
	req.Contents = contents

	// 工具转换
	if len(params.Tools) > 0 {
		req.Tools = convertUnifiedToGeminiTools(params.Tools)
		if params.ToolChoice != nil {
			req.ToolConfig = convertUnifiedToGeminiToolConfig(params.ToolChoice)
		}
	}

	// 生成配置
	genConfig := &GenerationConfig{}
	hasConfig := false

	if params.Temperature != nil {
		genConfig.Temperature = params.Temperature
		hasConfig = true
	}
	if params.TopP != nil {
		genConfig.TopP = params.TopP
		hasConfig = true
	}
	if params.MaxTokens != nil {
		genConfig.MaxOutputTokens = *params.MaxTokens
		hasConfig = true
	}
	if len(params.StopSequences) > 0 {
		genConfig.StopSequences = params.StopSequences
		hasConfig = true
	}
	if params.Seed != nil {
		seed := int64(*params.Seed)
		genConfig.Seed = &seed
		hasConfig = true
	}
	if params.FrequencyPenalty != nil {
		genConfig.FrequencyPenalty = params.FrequencyPenalty
		hasConfig = true
	}
	if params.PresencePenalty != nil {
		genConfig.PresencePenalty = params.PresencePenalty
		hasConfig = true
	}

	// Response format
	if params.ResponseFormat != nil {
		switch params.ResponseFormat.Type {
		case llm.ResponseFormatJSONObject:
			genConfig.ResponseMIMEType = "application/json"
			hasConfig = true
		case llm.ResponseFormatJSONSchema:
			genConfig.ResponseMIMEType = "application/json"
			if m, ok := params.ResponseFormat.JSONSchema.(map[string]any); ok {
				if schema, ok := m["schema"]; ok {
					schemaJSON, _ := json.Marshal(schema)
					genConfig.ResponseSchema = schemaJSON
				}
			}
			hasConfig = true
		}
	}

	// Reasoning effort → thinking config
	if params.ReasoningEffort != nil {
		genConfig.ThinkingConfig = effortToThinkingConfig(*params.ReasoningEffort)
		hasConfig = true
	}

	if hasConfig {
		req.GenerationConfig = genConfig
	}

	return req, nil
}

func convertUnifiedToGeminiContents(messages []llm.Message) ([]Content, error) {
	var out []Content
	for _, msg := range messages {
		switch msg.Role {
		case llm.MessageRoleSystem:
			// Skip, handled by systemInstruction
			continue

		case llm.MessageRoleUser:
			var parts []Part
			for _, p := range msg.Content {
				switch pp := p.(type) {
				case llm.TextPart:
					parts = append(parts, TextPart(pp.Text))
				case llm.ImagePart:
					parts = append(parts, ImagePart(pp.MediaType, pp.Image))
				case llm.FilePart:
					parts = append(parts, InlineDataPart(pp.MediaType, pp.Data))
				}
			}
			if len(parts) > 0 {
				out = append(out, Content{Role: RoleUser, Parts: parts})
			}

		case llm.MessageRoleAssistant:
			var parts []Part
			for _, p := range msg.Content {
				switch pp := p.(type) {
				case llm.TextPart:
					parts = append(parts, TextPart(pp.Text))
				case llm.ReasoningPart:
					parts = append(parts, ThoughtPart(pp.Text))
				case llm.ToolCallPart:
					args := toStringMap(pp.Input)
					parts = append(parts, FunctionCallPart(pp.ToolName, args))
				}
			}
			if len(parts) > 0 {
				out = append(out, Content{Role: RoleModel, Parts: parts})
			}

		case llm.MessageRoleTool:
			for _, p := range msg.Content {
				if trp, ok := p.(llm.ToolResultPart); ok {
					resp := toStringMap(trp.Result)
					if trp.IsError {
						resp["error"] = true
					}
					parts := []Part{FunctionResponsePart(trp.ToolName, resp)}
					out = append(out, Content{Role: RoleUser, Parts: parts})
				}
			}
		}
	}
	return out, nil
}

func convertUnifiedToGeminiTools(tools []llm.Tool) []Tool {
	decls := make([]FunctionDeclaration, 0, len(tools))
	for _, t := range tools {
		params, _ := json.Marshal(t.Parameters)
		decls = append(decls, FunctionDeclaration{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  params,
		})
	}
	return []Tool{{FunctionDeclarations: decls}}
}

func convertUnifiedToGeminiToolConfig(choice any) *ToolConfig {
	cfg := &ToolConfig{FunctionCallingConfig: &FunctionCallingConfig{Mode: FunctionCallingModeAuto}}
	switch v := choice.(type) {
	case string:
		switch v {
		case "auto":
			cfg.FunctionCallingConfig.Mode = FunctionCallingModeAuto
		case "none":
			cfg.FunctionCallingConfig.Mode = FunctionCallingModeNone
		case "required":
			cfg.FunctionCallingConfig.Mode = FunctionCallingModeAny
		}
	case map[string]any:
		if fn, ok := v["function"].(map[string]any); ok {
			if name, ok := fn["name"].(string); ok {
				cfg.FunctionCallingConfig.Mode = FunctionCallingModeAny
				cfg.FunctionCallingConfig.AllowedFunctionNames = []string{name}
			}
		}
	}
	return cfg
}

func effortToThinkingConfig(effort string) *ThinkingConfig {
	switch effort {
	case "minimal", "none", "low":
		return &ThinkingConfig{ThinkingLevel: ThinkingLevelLow}
	case "medium":
		return &ThinkingConfig{ThinkingLevel: ThinkingLevelMedium}
	case "high":
		return &ThinkingConfig{ThinkingLevel: ThinkingLevelHigh}
	default:
		return &ThinkingConfig{IncludeThoughts: true}
	}
}

func geminiResponseToResult(resp *GenerateContentResponse) *llm.GenerateResult {
	result := &llm.GenerateResult{
		Response: llm.ResponseMetadata{
			ModelID: resp.ModelVersion,
		},
	}

	if resp.UsageMetadata != nil {
		result.Usage = convertGeminiUsage(resp.UsageMetadata)
	}

	if len(resp.Candidates) > 0 {
		cand := resp.Candidates[0]
		result.FinishReason = mapGeminiFinishReason(cand.FinishReason)
		result.RawFinishReason = string(cand.FinishReason)

		var hasFunctionCall bool
		for _, part := range cand.Content.Parts {
			if part.Thought && part.Text != "" {
				result.Reasoning += part.Text
			} else if part.Text != "" {
				result.Text += part.Text
			} else if part.FunctionCall != nil {
				hasFunctionCall = true
				callID := part.FunctionCall.ID
				if callID == "" {
					callID = part.FunctionCall.Name
				}
				result.ToolCalls = append(result.ToolCalls, llm.ToolCall{
					ToolCallID: callID,
					ToolName:   part.FunctionCall.Name,
					Input:      part.FunctionCall.Args,
				})
			}
		}

		if hasFunctionCall && result.FinishReason == llm.FinishReasonStop {
			result.FinishReason = llm.FinishReasonToolCalls
		}
	}

	return result
}

func convertGeminiUsage(u *UsageMetadata) llm.Usage {
	usage := llm.Usage{
		InputTokens:     u.PromptTokenCount,
		OutputTokens:    u.CandidatesTokenCount,
		TotalTokens:     u.TotalTokenCount,
		ReasoningTokens: u.ThoughtsTokenCount,
		OutputTokenDetails: llm.OutputTokenDetail{
			ReasoningTokens: u.ThoughtsTokenCount,
			TextTokens:      max(0, u.CandidatesTokenCount-u.ThoughtsTokenCount),
		},
	}

	// Map cache token details
	cachedTokens := 0
	for _, detail := range u.CacheTokensDetails {
		cachedTokens += detail.TokenCount
	}
	if cachedTokens > 0 {
		usage.CachedInputTokens = cachedTokens
		usage.InputTokenDetails = llm.InputTokenDetail{
			CacheReadTokens: cachedTokens,
			NoCacheTokens:   max(0, u.PromptTokenCount-cachedTokens),
		}
	}

	return usage
}

func mapGeminiFinishReason(reason FinishReason) llm.FinishReason {
	switch reason {
	case FinishReasonStop:
		return llm.FinishReasonStop
	case FinishReasonMaxTokens:
		return llm.FinishReasonLength
	case FinishReasonSafety, FinishReasonRecitation, FinishReasonBlocklist, FinishReasonProhibited, FinishReasonSPII:
		return llm.FinishReasonContentFilter
	default:
		return llm.FinishReasonOther
	}
}

func toStringMap(v any) map[string]any {
	if v == nil {
		return nil
	}
	if m, ok := v.(map[string]any); ok {
		return m
	}
	data, _ := json.Marshal(v)
	var m map[string]any
	_ = json.Unmarshal(data, &m)
	return m
}
