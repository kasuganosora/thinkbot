package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kasuganosora/thinkbot/llm"
	"github.com/kasuganosora/thinkbot/util/traceid"
)

// ============================================================================
// Provider 接口适配器
// ============================================================================

// Name 实现 llm.Provider。
func (c *Client) Name() string { return "openai" }

// DoGenerate 将统一 GenerateParams 转换为 OpenAI 请求并返回统一 GenerateResult。
// 根据 chatMode 选择 Chat Completions API 或 Responses API。
func (c *Client) DoGenerate(ctx context.Context, params llm.GenerateParams) (*llm.GenerateResult, error) {
	if params.Model == nil {
		return nil, fmt.Errorf("openai: model is required")
	}

	if c.chatMode {
		return c.doGenerateChat(ctx, params)
	}

	req, err := c.responsesRequest(&params)
	if err != nil {
		return nil, err
	}

	resp, err := c.DoCreateResponse(ctx, *req)
	if err != nil {
		return nil, err
	}
	if err := responseFailure(resp); err != nil {
		return nil, err
	}

	return openAIResponseToResult(resp, c.providerKey(), params.Model.ID), nil
}

// DoStream 将统一 GenerateParams 转换为 OpenAI 流式请求并返回统一 StreamResult。
// 根据 chatMode 选择 Chat Completions API 或 Responses API。
func (c *Client) DoStream(ctx context.Context, params llm.GenerateParams) (*llm.StreamResult, error) {
	if params.Model == nil {
		return nil, fmt.Errorf("openai: model is required")
	}

	if c.chatMode {
		return c.doStreamChat(ctx, params)
	}

	req, err := c.responsesRequest(&params)
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
			emittedText      = map[string]string{}

			pendingToolCalls = map[int]*openAIStreamingToolCall{}
			emittedToolIDs   = map[string]struct{}{}
			toolPartsEmitted int
		)
		emitToolCall := func(callID, itemID, name, args string) bool {
			key := callID
			if key == "" {
				key = itemID
			}
			if key == "" || toolIDSeen(emittedToolIDs, callID, itemID) {
				rememberToolIDs(emittedToolIDs, callID, itemID)
				return false
			}
			var input any
			if args != "" {
				_ = json.Unmarshal([]byte(args), &input)
			}
			send(&llm.ToolInputEndPart{ID: key})
			send(&llm.StreamToolCallPart{
				ToolCallID: key,
				ToolName:   name,
				Input:      input,
			})
			toolPartsEmitted++
			rememberToolIDs(emittedToolIDs, callID, itemID, key)
			return true
		}
		emitFunctionItem := func(item *OutputItem, stc *openAIStreamingToolCall) {
			if item == nil || item.Type != TypeFunctionCall {
				return
			}
			hasFunctionCall = true
			callID, itemID, name, args := takeStreamToolCall(item, stc)
			emitToolCall(callID, itemID, name, args)
		}
		emitText := func(id, chunk string) {
			if chunk == "" {
				return
			}
			if !textStarted {
				send(&llm.TextStartPart{ID: id})
				textStarted = true
			}
			emittedText[id] += chunk
			send(&llm.TextDeltaPart{ID: id, Text: chunk})
		}
		reconcileText := func(id, final string) {
			if final == "" {
				return
			}
			have := emittedText[id]
			switch {
			case have == final || strings.HasPrefix(have, final):
				return
			case strings.HasPrefix(final, have):
				emitText(id, final[len(have):])
			case have == "":
				emitText(id, final)
			}
		}

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

		streamErr := c.DoStreamResponse(ctx, *req, StreamConfig{RetryConfig: c.streamRetryConfig()}, func(event StreamEvent) error {
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
					stc := &openAIStreamingToolCall{
						id:     callID,
						itemID: event.Item.ID,
						name:   event.Item.Name,
						args:   event.Item.Arguments,
					}
					if prev := pendingToolCalls[event.OutputIndex]; prev != nil && stc.args == "" {
						stc.args = prev.args
						stc.argsFinal = prev.argsFinal
					}
					pendingToolCalls[event.OutputIndex] = stc
					send(&llm.ToolInputStartPart{ID: callID, ToolName: event.Item.Name})
				}

			case EventResponseOutputTextDelta:
				if reasoningStarted {
					send(&llm.ReasoningEndPart{ID: responseID})
					reasoningStarted = false
				}
				emitText(event.ItemID, event.Delta)

			case EventResponseReasoningDelta:
				if !reasoningStarted {
					send(&llm.ReasoningStartPart{ID: event.ItemID})
					reasoningStarted = true
				}
				send(&llm.ReasoningDeltaPart{ID: event.ItemID, Text: event.Delta})

			case EventResponseFunctionCallArgumentsDelta:
				if stc, ok := pendingToolCalls[event.OutputIndex]; ok && !stc.argsFinal {
					stc.args += event.Delta
					send(&llm.ToolInputDeltaPart{ID: stc.id, Delta: event.Delta})
				}

			case EventResponseFunctionCallArgumentsDone:
				// The final arguments arrive here, before output_item.done.
				// Marking the call finished at this point drops it: both
				// output_item.done and the end flush skip finished slots.
				stc := pendingToolCalls[event.OutputIndex]
				if stc == nil {
					stc = &openAIStreamingToolCall{}
					pendingToolCalls[event.OutputIndex] = stc
				}
				if event.ItemID != "" && stc.itemID == "" {
					stc.itemID = event.ItemID
				}
				if event.Arguments != "" {
					stc.args = event.Arguments
					stc.argsFinal = true
				}

			case EventResponseOutputItemDone:
				if event.Item == nil {
					return nil
				}
				switch event.Item.Type {
				case TypeMessage:
					reconcileText(event.Item.ID, extractOutputText(event.Item.Content))
				case TypeReasoning:
					if summary := extractReasoningText(event.Item.Summary); summary != "" && !reasoningStarted {
						send(&llm.ReasoningStartPart{ID: event.Item.ID})
						send(&llm.ReasoningDeltaPart{ID: event.Item.ID, Text: summary})
					}
					send(&llm.ReasoningEndPart{ID: event.Item.ID, ProviderMetadata: reasoningItemMeta(event.Item, firstNonEmpty(responseModel, params.Model.ID), c.providerKey())})
					reasoningStarted = false
				case TypeFunctionCall:
					emitFunctionItem(event.Item, pendingToolCalls[event.OutputIndex])
				}

			case EventResponseCompleted, EventResponseIncomplete:
				if event.Response != nil {
					for i := range event.Response.Output {
						item := &event.Response.Output[i]
						if item.Type == TypeMessage {
							reconcileText(item.ID, extractOutputText(item.Content))
						}
						if event.Type == EventResponseCompleted && item.Type == TypeFunctionCall {
							emitFunctionItem(item, pendingByToolID(pendingToolCalls, item.CallID, item.ID))
						}
					}
					if event.Response.Usage != nil {
						usage = convertOpenAIUsage(event.Response.Usage)
					}
					if event.Response.IncompleteDetails != nil {
						rawFinishReason = event.Response.IncompleteDetails.Reason
					}
					if event.Type == EventResponseIncomplete {
						if err := responseFailure(event.Response); err != nil {
							return err
						}
					}
				}

			case EventResponseFailed:
				if event.Response != nil {
					if err := responseFailure(event.Response); err != nil {
						return err
					}
				}
				return inBandFailure(nil)

			case EventResponseError:
				return inBandFailure(streamEventError(&event))
			}
			return nil
		})

		flush()

		// Flush tool calls that never reached output_item.done or response.completed.
		for _, stc := range pendingToolCalls {
			if stc.finished {
				continue
			}
			if emitToolCall(stc.id, stc.itemID, stc.name, stc.args) {
				hasFunctionCall = true
			}
			stc.finished = true
		}

		finishReason = mapOpenAIFinishReason(rawFinishReason, hasFunctionCall)
		if finishReason == llm.FinishReasonToolCalls && toolPartsEmitted == 0 {
			traceid.L(ctx).Warnw("openai responses stream finished with tool-calls but emitted no tool parts")
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
			send(&llm.ErrorPart{Error: ClassifyStreamError(streamErr)})
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
	id        string
	itemID    string
	name      string
	args      string
	argsFinal bool
	finished  bool
}

// takeStreamToolCall prefers the output item's own name, arguments, and
// call id, and falls back to the pending slot. The slot is consumed so the
// end-of-stream flush does not emit it again.
func takeStreamToolCall(item *OutputItem, stc *openAIStreamingToolCall) (callID, itemID, name, args string) {
	if item != nil {
		callID = item.CallID
		itemID = item.ID
		name = item.Name
		args = item.Arguments
	}
	if stc != nil {
		if callID == "" {
			callID = stc.id
		}
		if itemID == "" {
			itemID = stc.itemID
		}
		if name == "" {
			name = stc.name
		}
		if args == "" {
			args = stc.args
		}
		stc.finished = true
	}
	return callID, itemID, name, args
}

func pendingByToolID(pending map[int]*openAIStreamingToolCall, ids ...string) *openAIStreamingToolCall {
	for _, stc := range pending {
		if stc == nil {
			continue
		}
		for _, id := range ids {
			if id == "" {
				continue
			}
			if stc.id == id || stc.itemID == id {
				return stc
			}
		}
	}
	return nil
}

func toolIDSeen(seen map[string]struct{}, ids ...string) bool {
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			return true
		}
	}
	return false
}

func rememberToolIDs(seen map[string]struct{}, ids ...string) {
	for _, id := range ids {
		if id != "" {
			seen[id] = struct{}{}
		}
	}
}

func (c *Client) providerKey() string {
	if c == nil || strings.TrimSpace(c.baseURL) == "" {
		return strings.TrimRight(DefaultBaseURL, "/")
	}
	return strings.TrimRight(c.baseURL, "/")
}

func (c *Client) responsesRequest(params *llm.GenerateParams) (*CreateResponseRequest, error) {
	key := ""
	if c != nil {
		key = c.providerKey()
	}
	return buildResponsesRequest(key, params)
}

func paramsToOpenAIRequest(params *llm.GenerateParams) (*CreateResponseRequest, error) {
	return buildResponsesRequest("", params)
}

func buildResponsesRequest(providerKey string, params *llm.GenerateParams) (*CreateResponseRequest, error) {
	req := &CreateResponseRequest{
		Model:       params.Model.ID,
		Temperature: wireSampling(params.Temperature),
		TopP:        wireSampling(params.TopP),
	}
	// Responses always uses max_output_tokens. The Chat Completions token-field
	// style does not apply here. Sync and stream both go through this function.
	// A missing or non-positive limit omits the field.
	if params.MaxTokens != nil && *params.MaxTokens > 0 {
		n := *params.MaxTokens
		req.MaxOutputTokens = &n
	}

	// Implicit prefix caching with cache key hint + store=false
	if params.CacheKey != "" {
		req.PromptCacheKey = params.CacheKey
	}
	store := false
	req.Store = &store
	// store=false drops server-side reasoning state, so ask for the encrypted
	// reasoning item and send it back on the next turn.
	req.Include = []string{"reasoning.encrypted_content"}

	if params.System != "" {
		req.Instructions = params.System
	}

	// 消息转换为 input items
	items, err := convertUnifiedToOpenAIInput(params.Messages, params.Model.ID, providerKey)
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
		// 思考模式不接受 required 的模型要降级，否则整轮请求会被 400 打回。
		if choice := llm.NormalizeToolChoice(params.Model.ID, params.ToolChoice); choice != nil {
			req.ToolChoice = toJSONRawMessage(choice)
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

func convertUnifiedToOpenAIInput(messages []llm.Message, modelID, providerKey string) ([]InputItem, error) {
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
				case llm.FilePart:
					return nil, fmt.Errorf("openai responses: file content is not sent on this path")
				default:
					return nil, fmt.Errorf("openai responses: unsupported content part %T", p)
				}
			}
			if hasImage && len(parts) > 0 {
				data, _ := json.Marshal(parts)
				items = append(items, InputItem{Type: TypeMessage, Role: RoleUser, Content: data})
			} else {
				items = append(items, InputText(RoleUser, llm.TextFromParts(msg.Content)))
			}

		case llm.MessageRoleAssistant:
			var produced []InputItem
			var reasoning *InputItem
			for _, p := range msg.Content {
				switch pp := p.(type) {
				case llm.TextPart:
					if strings.TrimSpace(pp.Text) == "" {
						continue
					}
					produced = append(produced, InputText(RoleAssistant, pp.Text))
				case llm.ReasoningPart:
					if item, ok := replayReasoningItem(pp, modelID, providerKey); ok {
						reasoning = &item
					}
				case llm.ToolCallPart:
					args, _ := json.Marshal(pp.Input)
					produced = append(produced, InputItem{
						Type:      TypeFunctionCall,
						CallID:    pp.ToolCallID,
						Name:      pp.ToolName,
						Arguments: string(args),
					})
				}
			}
			// 推理项紧挨在它产出的第一条内容之前。没有产出时不单独占一条。
			if reasoning != nil && len(produced) > 0 {
				items = append(items, *reasoning)
			}
			items = append(items, produced...)

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
		// Deferred tools expose a nil Parameters; emit a valid minimal schema
		// ({"type":"object","properties":{}}) rather than {} so providers that
		// require a type (e.g. Anthropic) accept the tool definition. GLM is
		// stricter: a function schema without a `properties` key is rejected
		// with 1210 "API 调用参数有误". normalizeToolParameters guarantees
		// `properties` is present on every object node.
		var params json.RawMessage = []byte(`{"type":"object","properties":{}}`)
		if t.Parameters != nil {
			if b, err := json.Marshal(t.Parameters); err == nil {
				params = b
			}
		}
		params = normalizeToolParameters(params)
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

func openAIResponseToResult(resp *Response, provider, fallbackModel string) *llm.GenerateResult {
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
			model := resp.Model
			if model == "" {
				model = fallbackModel
			}
			if meta := reasoningItemMeta(item, model, provider); meta != nil {
				if result.ReasoningProviderMetadata == nil || item.EncryptedContent != "" {
					result.ReasoningProviderMetadata = meta
				}
			}
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

func reasoningItemMeta(item *OutputItem, model, provider string) map[string]any {
	if item == nil || item.ID == "" || item.EncryptedContent == "" {
		return nil
	}
	return map[string]any{
		"id":                item.ID,
		"encrypted_content": item.EncryptedContent,
		"model":             model,
		"provider":          provider,
	}
}

// replayReasoningItem 只回放同一服务商、同一模型产出的、同时带 id 和
// encrypted_content 的推理项。只有摘要或思考正文的历史不会发给 Responses。
func replayReasoningItem(part llm.ReasoningPart, modelID, providerKey string) (InputItem, bool) {
	id, okID := metaString(part.ProviderMetadata, "id")
	enc, okEnc := metaString(part.ProviderMetadata, "encrypted_content")
	if !okID || !okEnc {
		return InputItem{}, false
	}
	if metaModel, ok := metaString(part.ProviderMetadata, "model"); !ok || metaModel != modelID {
		return InputItem{}, false
	}
	if metaProvider, ok := metaString(part.ProviderMetadata, "provider"); !ok || metaProvider != providerKey {
		return InputItem{}, false
	}
	item := InputItem{Type: TypeReasoning, ID: id, EncryptedContent: enc}
	if part.Text != "" {
		item.Summary = toJSONRawMessage([]SummaryItem{{Type: ContentTypeSummaryText, Text: part.Text}})
	} else {
		item.Summary = json.RawMessage(`[]`)
	}
	return item, true
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func metaString(meta map[string]any, key string) (string, bool) {
	if meta == nil {
		return "", false
	}
	s, ok := meta[key].(string)
	return s, ok && s != ""
}

// responseFailure reports an in-band failed response. A normal incomplete
// response only carries incomplete_details and is not an error.
func responseFailure(resp *Response) error {
	if resp == nil {
		return nil
	}
	failed := resp.Status == "failed"
	if resp.Error != nil && (resp.Error.Code != "" || resp.Error.Type != "" || resp.Error.Param != "") {
		failed = true
	}
	if !failed {
		return nil
	}
	var fields *ResponseError
	if resp.Error != nil {
		fields = resp.Error
	}
	return inBandFailure(fields)
}

func streamEventError(event *StreamEvent) *ResponseError {
	if event == nil {
		return nil
	}
	if event.Error != nil {
		return event.Error
	}
	if event.Code == "" && event.Param == "" {
		return nil
	}
	return &ResponseError{Code: event.Code, Param: event.Param}
}

func inBandFailure(e *ResponseError) error {
	code, param, typ := "", "", ""
	if e != nil {
		code, param, typ = e.Code, e.Param, e.Type
	}
	reason := openaiErrorTypeToReason(typ, code, 400)
	err := llm.NewLLMError(reason, "openai", "provider reported a failed response")
	err.SafeDetail = formatSafeErrorDetail(typ, code, param)
	return err
}

func toJSONRawMessage(v any) json.RawMessage {
	if v == nil {
		return nil
	}
	data, _ := json.Marshal(v)
	return data
}
