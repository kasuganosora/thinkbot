package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/kasuganosora/thinkbot/llm"
	"github.com/kasuganosora/thinkbot/util/errs"
	httputil "github.com/kasuganosora/thinkbot/util/http"
	"github.com/kasuganosora/thinkbot/util/retry"
	"github.com/kasuganosora/thinkbot/util/traceid"
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
			traceid.L(ctx).Debugw("openai chat stream error", "err", err)
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

		streamErr := c.DoStreamChatCompletion(ctx, *req, ChatStreamConfig{RetryConfig: c.streamRetryConfig()}, func(chunk ChatCompletionResponse) error {
			responseID = chunk.ID
			responseModel = chunk.Model

			if chunk.Usage != nil {
				usage = convertChatUsage(chunk.Usage)
			}

			for _, choice := range chunk.Choices {
				if choice.Delta == nil {
					continue
				}
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
			send(&llm.ErrorPart{Error: errs.Wrap(streamErr, "openai: chat stream failed")})
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

	// OpenAI does implicit prefix caching; a cache key hint helps cross-request
	// cache hits within the same conversation/session.
	if params.CacheKey != "" {
		req.PromptCacheKey = params.CacheKey
	}

	// store=false to avoid storing conversations server-side.
	store := false
	req.Store = &store

	// Reasoning effort for models that support it.
	if params.ReasoningEffort != nil {
		req.ReasoningEffort = *params.ReasoningEffort
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
			Content: nonEmptyContent(system),
		})
	}

	// GLM（BigModel）严格要求对话首条消息 role 必须是 user 或 system。
	// 若历史首条为 assistant/tool（如从某段对话接管），前置一条占位 user 消息，
	// 避免触发 1214 "messages[0] role must be user"。
	if system == "" && len(messages) > 0 {
		switch messages[0].Role {
		case llm.MessageRoleAssistant, llm.MessageRoleTool:
			out = append(out, ChatMessage{Role: RoleUser, Content: nonEmptyContent("（对话历史续接）")})
		}
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
				case llm.ReasoningPart:
					// 思考（reasoning_content）在 GLM 请求侧行为不确定，不回传：
					// 历史里的 ReasoningPart 仅用于本地上下文，发给模型时丢弃（保持现状）。
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
				m.Content = nonEmptyContent(textContent)
			}
			out = append(out, m)

		case llm.MessageRoleTool:
			for _, p := range msg.Content {
				if trp, ok := p.(llm.ToolResultPart); ok {
					resultStr, _ := json.Marshal(trp.Result)
					// 工具的 Result 必须作为「JSON 字符串」嵌入 tool 消息的 content——
					// OpenAI/GLM 严格要求 tool role 的 content 为 string 类型；若直接以
					// RawMessage（JSON 对象 {"count":18}）嵌入，GLM 整请求拒收 1210
					// "API 调用参数有误"（实测：stringify 后 GLM 返回 200）。
					// 但 Result 本身是 Go string 时，json.Marshal 已是合法 JSON 字符串，
					// 无需再包一层（否则双重转义，content 变成 "\"hello world\""）。
					c := json.RawMessage(quoteJSONString(string(resultStr)))
					s := string(resultStr)
					if len(s) == 0 || s == "null" || s == "\"\"" {
						c = nonEmptyContent("（工具无输出）") // 空结果补占位符，避免 GLM 1214
					} else if len(resultStr) > 0 && resultStr[0] == '"' {
						c = json.RawMessage(resultStr) // 字符串结果：保持单层转义
					}
					out = append(out, ChatMessage{
						Role:       RoleTool,
						ToolCallID: trp.ToolCallID,
						Content:    c,
					})
				}
			}
		}
	}

	// GLM（BigModel）要求对话消息严格交替 role：连续两条相同 role（user/user、
	// assistant/assistant）会被整请求拒收并返回 1210 "API 调用参数有误"。
	// 真实事故 wf-75adc58e5e08d704411a3fd0：工作流完成「系统通知」被多次注入为
	// user role，与用户消息连成 5 条连续 user，导致所有 delegate 节点首轮 GLM 调用
	// 即 1210。这里把连续同 role 的文本消息合并为一条，恢复交替结构。
	// tool 消息绝不合并——每条都带独立 tool_call_id，必须与发起它的 assistant
	// tool_call 成对，合并会破坏配对。带 tool_calls 的 assistant 回合也不合并，
	// 以免丢失后续回合的工具调用。
	out = coalesceMessages(out)

	// GLM（BigModel）要求 messages 至少含一条 user 角色消息：若整段对话只有
	// system（例如 delegate 节点重试/首轮仅注入 system prompt、任务写在 system
	// 内的场景），GLM 直接拒收 1214 "messages 参数非法"。OpenAI 允许 system-only，
	// 但 GLM 更严格——这里在 system 之后补一条占位 user 消息保证结构合法。
	// 已用真实请求 replay 验证：补 user 后 GLM 由 1214 变为 200。
	out = ensureUserMessage(out)

	return out, nil
}

// coalesceMessages 合并连续同 role 的消息，保证发给模型的消息严格交替。
// 仅对「纯文本 user/assistant/system」连续回合做文本拼接；tool 消息、带
// tool_calls 的 assistant 消息保持独立，避免破坏 tool_call 配对或丢失工具调用。
func coalesceMessages(msgs []ChatMessage) []ChatMessage {
	out := make([]ChatMessage, 0, len(msgs))
	for _, m := range msgs {
		if len(out) == 0 {
			out = append(out, m)
			continue
		}
		prev := &out[len(out)-1]
		// tool 消息永远独立保留
		if m.Role == RoleTool || prev.Role == RoleTool {
			out = append(out, m)
			continue
		}
		// 带 tool_calls 的 assistant 回合不合并，避免丢失工具调用配对
		if prev.Role == RoleAssistant && (len(prev.ToolCalls) > 0 || len(m.ToolCalls) > 0) {
			out = append(out, m)
			continue
		}
		if m.Role == prev.Role {
			prev.Content = mergeMessageContent(prev.Content, m.Content)
			continue
		}
		out = append(out, m)
	}
	return out
}

// mergeMessageContent 把两条同 role 消息的 content 拼接为一条。
// 仅当两条都是合法 JSON 字符串时做文本拼接；否则保留前一条（不破坏结构）。
func mergeMessageContent(a, b json.RawMessage) json.RawMessage {
	sa := rawMessageText(a)
	sb := rawMessageText(b)
	switch {
	case sb == "":
		return a
	case sa == "":
		return b
	default:
		return json.RawMessage(quoteJSONString(sa + "\n\n" + sb))
	}
}

// rawMessageText 把 JSON 字符串类型的 content 解码为 Go 字符串；
// 非字符串（如多模态数组、null）原样返回空串以外的值，由调用方决定如何处理。
func rawMessageText(r json.RawMessage) string {
	s := strings.TrimSpace(string(r))
	if s == "" || s == "null" {
		return ""
	}
	if len(s) >= 2 && s[0] == '"' {
		var str string
		if err := json.Unmarshal(r, &str); err == nil {
			return str
		}
	}
	return string(r)
}

// nonEmptyContent 将文本包装为合法的 Chat Completions content。
// GLM（BigModel）拒绝空字符串 content（报 1214 "messages 参数非法"），
// 因此当文本为空（或仅空白）时回退为占位符，保证 content 永不为空字符串。
func nonEmptyContent(text string) json.RawMessage {
	if strings.TrimSpace(text) == "" {
		text = "（无内容）"
	}
	return json.RawMessage(quoteJSONString(text))
}

// ensureUserMessage 保证消息序列至少含一条 user 角色消息。GLM（BigModel）在整段
// 对话缺失 user 角色时会拒收 1214 "messages 参数非法"；OpenAI 允许 system-only 首轮，
// 但 GLM 更严格。该情形出现在 delegate 节点首次 LLM 调用只带 system prompt（任务
// 写在 system 内）而尚未注入 user 轮时——典型如工作流节点重试/重跑的首个请求。
// 处理策略：若无任何 user 消息，在 system 之后（或序列开头）补一条非空占位 user 消息，
// 既不破坏既有 system/assistant/tool 配对，又满足 GLM 的结构要求。
func ensureUserMessage(msgs []ChatMessage) []ChatMessage {
	for _, m := range msgs {
		if m.Role == RoleUser {
			return msgs
		}
	}
	placeholder := ChatMessage{Role: RoleUser, Content: json.RawMessage(`"（请开始任务）"`)}
	if len(msgs) > 0 && msgs[0].Role == RoleSystem {
		out := make([]ChatMessage, 0, len(msgs)+1)
		out = append(out, msgs[0])
		out = append(out, placeholder)
		out = append(out, msgs[1:]...)
		return out
	}
	return append([]ChatMessage{placeholder}, msgs...)
}

func convertUnifiedToChatTools(tools []llm.Tool) []ChatTool {
	out := make([]ChatTool, 0, len(tools))
	for _, t := range tools {
		// Deferred tools expose a nil Parameters; emit a valid minimal schema
		// ({"type":"object","properties":{}}) rather than {} so providers that
		// require a type (e.g. Anthropic) accept the tool definition. GLM is
		// stricter: a function schema whose object has no `properties` key is
		// rejected with 1210 "API 调用参数有误". normalizeToolParameters
		// guarantees `properties` is present on every object node.
		var params json.RawMessage = []byte(`{"type":"object","properties":{}}`)
		if t.Parameters != nil {
			if b, err := json.Marshal(t.Parameters); err == nil {
				params = b
			}
		}
		params = normalizeToolParameters(params)
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

// normalizeToolParameters guarantees the JSON-Schema function parameters are
// accepted by strict providers. GLM rejects (1210 "API 调用参数有误") any
// function schema whose object-typed node lacks a `properties` key, so we add an
// empty `properties` object to every object node that omits one, recursively
// (covering nested properties and array items).
func normalizeToolParameters(raw json.RawMessage) json.RawMessage {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return raw
	}
	normalizeSchemaNode(m)
	if b, err := json.Marshal(m); err == nil {
		return b
	}
	return raw
}

func normalizeSchemaNode(m map[string]any) {
	if m == nil {
		return
	}
	if typ, ok := m["type"].(string); ok && typ == "object" {
		if _, exists := m["properties"]; !exists {
			m["properties"] = map[string]any{}
		}
	}
	if props, ok := m["properties"].(map[string]any); ok {
		for _, p := range props {
			if pm, ok := p.(map[string]any); ok {
				normalizeSchemaNode(pm)
			}
		}
	}
	switch it := m["items"].(type) {
	case map[string]any:
		normalizeSchemaNode(it)
	case []any:
		for _, sub := range it {
			if sm, ok := sub.(map[string]any); ok {
				normalizeSchemaNode(sm)
			}
		}
	}
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
		result.Usage = convertChatUsage(resp.Usage)
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

// convertChatUsage converts a ChatUsage into the unified llm.Usage, extracting
// cached/reasoning token details.
func convertChatUsage(u *ChatUsage) llm.Usage {
	inputTokens := u.PromptTokens
	outputTokens := u.CompletionTokens

	cachedTokens := 0
	reasoningTokens := 0
	if u.PromptTokensDetails != nil {
		cachedTokens = u.PromptTokensDetails.CachedTokens
	}
	if u.CompletionTokensDetails != nil {
		reasoningTokens = u.CompletionTokensDetails.ReasoningTokens
	}

	return llm.Usage{
		InputTokens:       inputTokens,
		OutputTokens:      outputTokens,
		TotalTokens:       u.TotalTokens,
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
