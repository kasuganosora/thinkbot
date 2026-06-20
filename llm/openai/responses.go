package openai

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
	"github.com/kasuganosora/thinkbot/util/errs"
	httputil "github.com/kasuganosora/thinkbot/util/http"
)

// ============================================================================
// 输入构造辅助函数
// ============================================================================

// InputText 创建一个文本输入项。
func InputText(role, content string) InputItem {
	return InputItem{
		Type:    TypeMessage,
		Role:    role,
		Content: json.RawMessage(quoteJSONString(content)),
	}
}

// InputSystem 创建系统消息输入项。
func InputSystem(content string) InputItem {
	return InputText(RoleSystem, content)
}

// InputUser 创建用户消息输入项。
func InputUser(content string) InputItem {
	return InputText(RoleUser, content)
}

// InputAssistant 创建助手消息输入项。
func InputAssistant(content string) InputItem {
	return InputText(RoleAssistant, content)
}

// InputDeveloper 创建开发者消息输入项。
func InputDeveloper(content string) InputItem {
	return InputText(RoleDeveloper, content)
}

// InputUserWithImage 创建包含文本和图片的用户消息。
func InputUserWithImage(text, imageURL string) InputItem {
	parts := []ContentPart{
		{Type: ContentTypeInputImage, ImageURL: imageURL},
		{Type: ContentTypeInputText, Text: text},
	}
	data, _ := json.Marshal(parts)
	return InputItem{Type: TypeMessage, Role: RoleUser, Content: data}
}

// InputFunctionCallOutput 创建函数调用结果输入项。
func InputFunctionCallOutput(callID, output string) InputItem {
	return InputItem{
		Type:   TypeFunctionCallOutput,
		CallID: callID,
		Output: output,
	}
}

// InputString 将简单字符串包装为 json.RawMessage（用于 Input 字段）。
func InputString(s string) json.RawMessage {
	return json.RawMessage(quoteJSONString(s))
}

// InputItems 将多个 InputItem 编码为 json.RawMessage。
func InputItems(items []InputItem) json.RawMessage {
	data, _ := json.Marshal(items)
	return data
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
type RequestOption func(*CreateResponseRequest)

// WithInstructions 设置系统指令。
func WithInstructions(instructions string) RequestOption {
	return func(r *CreateResponseRequest) { r.Instructions = instructions }
}

// WithTemperature 设置 temperature。
func WithTemperature(t float64) RequestOption {
	return func(r *CreateResponseRequest) { r.Temperature = &t }
}

// WithTopP 设置 top_p。
func WithTopP(p float64) RequestOption {
	return func(r *CreateResponseRequest) { r.TopP = &p }
}

// WithMaxOutputTokens 设置 max_output_tokens。
func WithMaxOutputTokens(n int) RequestOption {
	return func(r *CreateResponseRequest) { r.MaxOutputTokens = &n }
}

// WithReasoning 设置推理配置。
func WithReasoning(effort, summary string) RequestOption {
	return func(r *CreateResponseRequest) {
		r.Reasoning = &ReasoningConfig{Effort: effort, Summary: summary}
	}
}

// WithReasoningEffort 仅设置推理努力程度。
func WithReasoningEffort(effort string) RequestOption {
	return func(r *CreateResponseRequest) {
		if r.Reasoning == nil {
			r.Reasoning = &ReasoningConfig{}
		}
		r.Reasoning.Effort = effort
	}
}

// WithJSONSchema 设置 JSON Schema 结构化输出。
func WithJSONSchema(name string, schema json.RawMessage, strict bool) RequestOption {
	return func(r *CreateResponseRequest) {
		r.Text = &TextConfig{
			Format: &TextFormatConfig{
				Type:   "json_schema",
				Name:   name,
				Schema: schema,
				Strict: &strict,
			},
		}
	}
}

// WithJSONText 设置 JSON 文本模式输出。
func WithJSONText() RequestOption {
	return func(r *CreateResponseRequest) {
		r.Text = &TextConfig{
			Format: &TextFormatConfig{
				Type: "json_object",
			},
		}
	}
}

// WithFunctionTools 设置函数工具列表。
func WithFunctionTools(tools ...FunctionTool) RequestOption {
	return func(r *CreateResponseRequest) {
		raw := make([]json.RawMessage, len(tools))
		for i, t := range tools {
			data, _ := json.Marshal(t)
			raw[i] = data
		}
		r.Tools = raw
	}
}

// WithRawTools 设置原始工具列表（JSON 数组）。
func WithRawTools(tools json.RawMessage) RequestOption {
	return func(r *CreateResponseRequest) {
		var arr []json.RawMessage
		if err := json.Unmarshal(tools, &arr); err == nil {
			r.Tools = arr
		}
	}
}

// WithWebSearch 添加网络搜索工具。
func WithWebSearch(contextSize string) RequestOption {
	return func(r *CreateResponseRequest) {
		tool := WebSearchTool{Type: ToolTypeWebSearch, SearchContextSize: contextSize}
		data, _ := json.Marshal(tool)
		r.Tools = append(r.Tools, data)
	}
}

// WithFileSearch 添加文件搜索工具。
func WithFileSearch(vectorStoreIDs []string, maxResults int) RequestOption {
	return func(r *CreateResponseRequest) {
		tool := FileSearchTool{Type: ToolTypeFileSearch, VectorStoreIDs: vectorStoreIDs}
		if maxResults > 0 {
			tool.MaxNumResults = &maxResults
		}
		data, _ := json.Marshal(tool)
		r.Tools = append(r.Tools, data)
	}
}

// WithCodeInterpreter 添加代码解释器工具。
func WithCodeInterpreter(fileIDs ...string) RequestOption {
	return func(r *CreateResponseRequest) {
		tool := CodeInterpreterTool{Type: ToolTypeCodeInterpreter, FileIDs: fileIDs}
		data, _ := json.Marshal(tool)
		r.Tools = append(r.Tools, data)
	}
}

// WithPreviousResponse 设置 previous_response_id（用于多轮对话）。
func WithPreviousResponse(id string) RequestOption {
	return func(r *CreateResponseRequest) { r.PreviousResponseID = id }
}

// WithStore 设置是否存储响应。
func WithStore(store bool) RequestOption {
	return func(r *CreateResponseRequest) { r.Store = &store }
}

// WithUser 设置用户标识符。
func WithUser(user string) RequestOption {
	return func(r *CreateResponseRequest) { r.User = user }
}

// WithMetadata 设置元数据。
func WithMetadata(meta map[string]string) RequestOption {
	return func(r *CreateResponseRequest) { r.Metadata = meta }
}

// WithToolChoice 设置工具选择策略。
func WithToolChoice(choice json.RawMessage) RequestOption {
	return func(r *CreateResponseRequest) { r.ToolChoice = choice }
}

// WithParallelToolCalls 设置并行工具调用。
func WithParallelToolCalls(parallel bool) RequestOption {
	return func(r *CreateResponseRequest) { r.ParallelToolCalls = &parallel }
}

// WithInclude 设置额外包含内容（如 "reasoning.encrypted_content"）。
func WithInclude(include ...string) RequestOption {
	return func(r *CreateResponseRequest) { r.Include = include }
}

// ============================================================================
// CreateResponse — 同步（非流式）
// ============================================================================

// CreateResponse 发送同步 Responses API 请求并返回完整响应。
//
// input 可以是 string、[]InputItem 或 json.RawMessage。
func (c *Client) CreateResponse(ctx context.Context, model string, input any, opts ...RequestOption) (*Response, error) {
	req := CreateResponseRequest{
		Model: model,
	}
	if err := setInput(&req, input); err != nil {
		return nil, err
	}
	for _, opt := range opts {
		opt(&req)
	}
	return c.DoCreateResponse(ctx, req)
}

// DoCreateResponse 发送完整的 CreateResponseRequest。
func (c *Client) DoCreateResponse(ctx context.Context, req CreateResponseRequest) (*Response, error) {
	if err := validateRequest(&req); err != nil {
		return nil, err
	}

	req.Stream = false

	resp, err := c.newRequest("POST", "/v1/responses").
		SetContext(ctx).
		SetJSONBody(req).
		Do()
	if err != nil {
		return nil, parseAPIError(resp, err)
	}

	var result Response
	if err := resp.JSON(&result); err != nil {
		return nil, err
	}
	return &result, nil
}

// ============================================================================
// Retrieve / Delete / Cancel
// ============================================================================

// RetrieveResponse 获取一个已有的响应。
func (c *Client) RetrieveResponse(ctx context.Context, responseID string) (*Response, error) {
	resp, err := c.newRequest("GET", "/v1/responses/"+responseID).
		SetContext(ctx).
		Do()
	if err != nil {
		return nil, parseAPIError(resp, err)
	}

	var result Response
	if err := resp.JSON(&result); err != nil {
		return nil, err
	}
	return &result, nil
}

// DeleteResponse 删除一个响应。
func (c *Client) DeleteResponse(ctx context.Context, responseID string) error {
	resp, err := c.newRequest("DELETE", "/v1/responses/"+responseID).
		SetContext(ctx).
		Do()
	if err != nil {
		return parseAPIError(resp, err)
	}
	return nil
}

// CancelResponse 取消一个进行中的响应。
func (c *Client) CancelResponse(ctx context.Context, responseID string) (*Response, error) {
	resp, err := c.newRequest("POST", "/v1/responses/"+responseID+"/cancel").
		SetContext(ctx).
		Do()
	if err != nil {
		return nil, parseAPIError(resp, err)
	}

	var result Response
	if err := resp.JSON(&result); err != nil {
		return nil, err
	}
	return &result, nil
}

// ============================================================================
// 响应解析辅助函数
// ============================================================================

// OutputText 提取响应中所有输出文本。
func (r *Response) OutputText() string {
	var result string
	for _, item := range r.Output {
		if item.Type != TypeMessage {
			continue
		}
		var contents []MessageContent
		if err := json.Unmarshal(item.Content, &contents); err != nil {
			continue
		}
		for _, c := range contents {
			if c.Type == ContentTypeOutputText || c.Type == ContentTypeText {
				result += c.Text
			}
		}
	}
	return result
}

// FunctionCalls 提取响应中所有函数调用。
func (r *Response) FunctionCalls() []FunctionCallOutput {
	var calls []FunctionCallOutput
	for _, item := range r.Output {
		if item.Type == TypeFunctionCall {
			calls = append(calls, FunctionCallOutput{
				ID:        item.ID,
				CallID:    item.CallID,
				Name:      item.Name,
				Arguments: item.Arguments,
				Status:    item.Status,
			})
		}
	}
	return calls
}

// Messages 提取响应中所有消息输出项。
func (r *Response) Messages() []MessageOutput {
	var msgs []MessageOutput
	for _, item := range r.Output {
		if item.Type != TypeMessage {
			continue
		}
		var contents []MessageContent
		_ = json.Unmarshal(item.Content, &contents)
		msgs = append(msgs, MessageOutput{
			ID:      item.ID,
			Role:    item.Role,
			Status:  item.Status,
			Content: contents,
		})
	}
	return msgs
}

// FirstMessageText 返回第一个消息的文本内容。
func (r *Response) FirstMessageText() string {
	msgs := r.Messages()
	if len(msgs) == 0 {
		return ""
	}
	for _, c := range msgs[0].Content {
		if c.Type == ContentTypeOutputText || c.Type == ContentTypeText {
			return c.Text
		}
	}
	return ""
}

// ============================================================================
// 内部工具
// ============================================================================

func parseAPIError(resp *httputil.Response, httpErr error) error {
	if resp != nil && resp.StatusCode >= 400 {
		var errResp ErrorResponse
		if err := json.Unmarshal(resp.Body, &errResp); err == nil && errResp.Error.Message != "" {
			return openAIErrorToLLMError(&errResp.Error, resp.StatusCode, resp.Headers)
		}
		// Body wasn't parseable — fall back with status code.
		return llm.NewLLMError(
			openaiHttpStatusToReason(resp.StatusCode),
			"openai",
			fmt.Sprintf("HTTP %d: %s", resp.StatusCode, truncateBody(resp.Body)),
			llm.WithCause(httpErr),
		)
	}
	if httpErr != nil {
		return llm.NewLLMError(llm.ErrorReasonTransport, "openai", httpErr.Error(), llm.WithCause(httpErr))
	}
	return httpErr
}

// openAIErrorToLLMError converts an OpenAI APIError into a unified llm.LLMError.
func openAIErrorToLLMError(apiErr *APIError, statusCode int, headers http.Header) *llm.LLMError {
	reason := openaiErrorTypeToReason(apiErr.Type, apiErr.Code, statusCode)
	opts := []llm.LLMErrorOpt{llm.WithCause(apiErr)}

	if delay := parseOpenAIRetryAfter(headers); delay > 0 {
		opts = append(opts, llm.WithRetryAfter(delay))
	}

	return llm.NewLLMError(reason, "openai", apiErr.Message, opts...)
}

func openaiErrorTypeToReason(errType, code string, statusCode int) llm.ErrorReason {
	switch errType {
	case "invalid_request_error":
		if code == "model_not_found" {
			return llm.ErrorReasonNoRoute
		}
		return llm.ErrorReasonInvalidRequest
	case "authentication_error":
		return llm.ErrorReasonAuthentication
	case "rate_limit_exceeded", "tokens":
		return llm.ErrorReasonRateLimit
	case "insufficient_quota", "billing":
		return llm.ErrorReasonQuotaExceeded
	case "content_policy_violation":
		return llm.ErrorReasonContentPolicy
	case "server_error":
		return llm.ErrorReasonProviderInternal
	case "api_error":
		return llm.ErrorReasonProviderInternal
	}
	// Fallback to status code mapping.
	if code == "insufficient_quota" {
		return llm.ErrorReasonQuotaExceeded
	}
	return openaiHttpStatusToReason(statusCode)
}

func openaiHttpStatusToReason(statusCode int) llm.ErrorReason {
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

func parseOpenAIRetryAfter(headers http.Header) time.Duration {
	if headers == nil {
		return 0
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

func validateRequest(req *CreateResponseRequest) error {
	if req.Model == "" {
		return errors.New("openai: model is required")
	}
	return nil
}

// setInput 设置请求的 input 字段。
func setInput(req *CreateResponseRequest, input any) error {
	switch v := input.(type) {
	case nil:
		return nil
	case string:
		req.Input = json.RawMessage(quoteJSONString(v))
	case []InputItem:
		data, err := json.Marshal(v)
		if err != nil {
			return errs.Wrap(err, "openai: failed to marshal input items")
		}
		req.Input = data
	case json.RawMessage:
		req.Input = v
	default:
		data, err := json.Marshal(v)
		if err != nil {
			return errs.Wrap(err, "openai: failed to marshal input")
		}
		req.Input = data
	}
	return nil
}
