package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/kasuganosora/thinkbot/util/errs"
)

// ============================================================================
// SchemaBuilder — 流式 JSON Schema 构建器
//
// 用于构建 Tool 的 InputSchema 字段，避免手写嵌套 map[string]any。
//
// 用法：
//
//	schema := NewSchema().
//	    PropString("location", "City name", true).
//	    PropStringEnum("unit", "Temperature unit", false, "celsius", "fahrenheit").
//	    Build()
//	tool := NewTool("get_weather", "Get weather for a location", schema)
// ============================================================================

// SchemaProperty 描述一个参数的 JSON Schema 片段。
type SchemaProperty struct {
	Type        string                    `json:"type"`
	Description string                    `json:"description,omitempty"`
	Enum        []string                  `json:"enum,omitempty"`
	Format      string                    `json:"format,omitempty"`
	Items       *SchemaProperty           `json:"items,omitempty"`
	Properties  map[string]SchemaProperty `json:"properties,omitempty"`
	Required    []string                  `json:"required,omitempty"`
	Minimum     *float64                  `json:"minimum,omitempty"`
}

// SchemaBuilder 流式构建工具参数的 JSON Schema。
type SchemaBuilder struct {
	properties map[string]SchemaProperty
	required   []string
}

// NewSchema 创建一个新的 SchemaBuilder。
func NewSchema() *SchemaBuilder {
	return &SchemaBuilder{
		properties: make(map[string]SchemaProperty),
	}
}

// Prop 添加一个任意类型的参数。
func (b *SchemaBuilder) Prop(name, desc, typ string, required bool) *SchemaBuilder {
	b.properties[name] = SchemaProperty{Type: typ, Description: desc}
	if required {
		b.required = append(b.required, name)
	}
	return b
}

// PropString 添加一个字符串参数。
func (b *SchemaBuilder) PropString(name, desc string, required bool) *SchemaBuilder {
	return b.Prop(name, desc, "string", required)
}

// PropStringFormat 添加一个带 format 的字符串参数（如 date-time、email、date）。
func (b *SchemaBuilder) PropStringFormat(name, desc, format string, required bool) *SchemaBuilder {
	b.properties[name] = SchemaProperty{Type: "string", Description: desc, Format: format}
	if required {
		b.required = append(b.required, name)
	}
	return b
}

// PropInteger 添加一个整数参数。
func (b *SchemaBuilder) PropInteger(name, desc string, required bool) *SchemaBuilder {
	return b.Prop(name, desc, "integer", required)
}

// PropNumber 添加一个数字参数。
func (b *SchemaBuilder) PropNumber(name, desc string, required bool) *SchemaBuilder {
	return b.Prop(name, desc, "number", required)
}

// PropBoolean 添加一个布尔参数。
func (b *SchemaBuilder) PropBoolean(name, desc string, required bool) *SchemaBuilder {
	return b.Prop(name, desc, "boolean", required)
}

// PropStringEnum 添加一个带枚举值的字符串参数。
func (b *SchemaBuilder) PropStringEnum(name, desc string, required bool, values ...string) *SchemaBuilder {
	b.properties[name] = SchemaProperty{
		Type:        "string",
		Description: desc,
		Enum:        values,
	}
	if required {
		b.required = append(b.required, name)
	}
	return b
}

// PropArray 添加一个数组参数。
func (b *SchemaBuilder) PropArray(name, desc, itemType string, required bool) *SchemaBuilder {
	b.properties[name] = SchemaProperty{
		Type:        "array",
		Description: desc,
		Items:       &SchemaProperty{Type: itemType},
	}
	if required {
		b.required = append(b.required, name)
	}
	return b
}

// Build 生成 JSON Schema 的 map[string]any。
func (b *SchemaBuilder) Build() map[string]any {
	m := map[string]any{
		"type":       "object",
		"properties": b.properties,
	}
	if len(b.required) > 0 {
		m["required"] = b.required
	}
	return m
}

// ============================================================================
// Tool 构造器
// ============================================================================

// NewTool 创建一个自定义工具定义。
//
// schema 通常由 SchemaBuilder.Build() 生成，也可传入手写的 map[string]any。
func NewTool(name, description string, schema map[string]any) Tool {
	return Tool{
		Name:        name,
		Description: description,
		InputSchema: schema,
	}
}

// NewSimpleTool 创建一个无参数的工具定义。
func NewSimpleTool(name, description string) Tool {
	return Tool{
		Name:        name,
		Description: description,
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
	}
}

// NewStrictTool 创建一个启用严格模式（strict: true）的工具定义。
// 严格模式保证 Claude 的工具调用输入严格匹配 schema。
func NewStrictTool(name, description string, schema map[string]any) Tool {
	t := NewTool(name, description, schema)
	strt := true
	t.Strict = &strt
	return t
}

// WithExamples 为工具添加输入示例（返回新 Tool，不修改原对象）。
func (t Tool) WithExamples(examples ...map[string]any) Tool {
	t.InputExamples = examples
	return t
}

// ============================================================================
// ToolChoice 构造器
// ============================================================================

// ChoiceAuto 创建让 Claude 自行决定的 ToolChoice（默认值）。
func ChoiceAuto() *ToolChoice {
	return &ToolChoice{Type: ToolChoiceAuto}
}

// ChoiceAny 创建要求 Claude 必须使用某个工具的 ToolChoice。
func ChoiceAny() *ToolChoice {
	return &ToolChoice{Type: ToolChoiceAny}
}

// ChoiceTool 创建要求 Claude 必须使用指定工具的 ToolChoice。
func ChoiceTool(name string) *ToolChoice {
	return &ToolChoice{Type: ToolChoiceTool, Name: name}
}

// ChoiceNone 创建禁止 Claude 使用任何工具的 ToolChoice。
func ChoiceNone() *ToolChoice {
	return &ToolChoice{Type: ToolChoiceNone}
}

// WithDisableParallel 设置是否禁用并行工具调用（返回新 ToolChoice）。
func (tc *ToolChoice) WithDisableParallel(disable bool) *ToolChoice {
	tc.DisableParallel = disable
	return tc
}

// ============================================================================
// 内容块构造器
// ============================================================================

// ToolUseBlock 创建一个 tool_use 类型的 ContentBlock。
//
// input 将被序列化为 JSON。通常用于多轮对话中回传 assistant 的 tool_use 块。
func ToolUseBlock(id, name string, input any) ContentBlock {
	inputJSON, _ := json.Marshal(input)
	return ContentBlock{
		Type:  ContentTypeToolUse,
		ID:    id,
		Name:  name,
		Input: inputJSON,
	}
}

// ToolResultBlock 创建一个 tool_result 类型的 ContentBlock。
//
// content 将被序列化为 JSON 字符串。
func ToolResultBlock(toolUseID string, content any) ContentBlock {
	contentJSON, _ := json.Marshal(content)
	return ContentBlock{
		Type:          ContentTypeToolResult,
		ToolUseID:     toolUseID,
		ResultContent: contentJSON,
	}
}

// ToolResultStringBlock 创建一个 content 为纯字符串的 tool_result 块。
func ToolResultStringBlock(toolUseID, content string) ContentBlock {
	return ContentBlock{
		Type:          ContentTypeToolResult,
		ToolUseID:     toolUseID,
		ResultContent: json.RawMessage(fmt.Sprintf("%q", content)),
	}
}

// ToolResultErrorBlock 创建一个标记为错误的 tool_result 块。
//
// Claude 看到错误后会尝试修正参数重试、询问用户或解释限制。
func ToolResultErrorBlock(toolUseID, errMsg string) ContentBlock {
	return ContentBlock{
		Type:          ContentTypeToolResult,
		ToolUseID:     toolUseID,
		ResultContent: json.RawMessage(fmt.Sprintf("%q", errMsg)),
		IsError:       true,
	}
}

// ============================================================================
// 响应检查辅助
// ============================================================================

// HasToolUse 检查响应中是否包含 tool_use 块。
func HasToolUse(resp *MessageResponse) bool {
	return len(ExtractToolUse(resp)) > 0
}

// ToolUseEntry 表示从响应中提取的一个 tool_use 块。
type ToolUseEntry struct {
	Index int             // 在 Content 数组中的位置
	ID    string          // tool_use ID
	Name  string          // 工具名称
	Input json.RawMessage // 工具调用参数（原始 JSON）
}

// ParsedInput 将 Input 解析到 v 指向的对象。
func (e *ToolUseEntry) ParsedInput(v any) error {
	return json.Unmarshal(e.Input, v)
}

// ExtractToolUse 从响应中提取所有 tool_use 块。
//
// 返回按出现顺序排列的所有 ToolUseEntry。适用于并行工具调用。
func ExtractToolUse(resp *MessageResponse) []ToolUseEntry {
	if resp == nil {
		return nil
	}
	var entries []ToolUseEntry
	for i, block := range resp.Content {
		if block.Type == ContentTypeToolUse {
			entries = append(entries, ToolUseEntry{
				Index: i,
				ID:    block.ID,
				Name:  block.Name,
				Input: block.Input,
			})
		}
	}
	return entries
}

// GetFirstToolUse 从响应中提取第一个 tool_use 块。
// 返回 nil 表示响应中没有 tool_use。
func GetFirstToolUse(resp *MessageResponse) *ToolUseEntry {
	entries := ExtractToolUse(resp)
	if len(entries) == 0 {
		return nil
	}
	return &entries[0]
}

// ExtractText 从响应中提取所有 text 块的文本（拼接为单个字符串）。
func ExtractText(resp *MessageResponse) string {
	if resp == nil {
		return ""
	}
	var text string
	for _, block := range resp.Content {
		if block.Type == ContentTypeText && block.Text != "" {
			text += block.Text
		}
	}
	return text
}

// ============================================================================
// ToolHandler & ToolRegistry — Go 函数注册表
//
// 将 Anthropic 的 tool_use 映射到 Go 函数，实现自动调度。
//
// 用法：
//
//	registry := NewToolRegistry()
//	registry.Register("get_weather", "Get weather",
//	    func(input map[string]any) (any, error) {
//	        city := input["city"].(string)
//	        return map[string]any{"temp": "25°C"}, nil
//	    }, NewSchema().PropString("city", "City name", true).Build())
//
//	// 自动构建工具列表
//	tools := registry.BuildTools()
//
//	// 执行模型发起的工具调用
//	results := registry.ExecuteToolCalls(resp)
// ============================================================================

// ToolHandler 是一个 Go 工具处理器的签名。
// 接收模型传入的参数 map，返回结果（将被放入 tool_result）或错误。
type ToolHandler func(input map[string]any) (any, error)

// ToolEntry 表示一个已注册的工具。
type ToolEntry struct {
	Handler     ToolHandler
	Description string
	Schema      map[string]any
}

// ToolRegistry 管理函数名到 Go 处理器的映射。
type ToolRegistry struct {
	entries map[string]ToolEntry
}

// NewToolRegistry 创建一个空的工具注册表。
func NewToolRegistry() *ToolRegistry {
	return &ToolRegistry{
		entries: make(map[string]ToolEntry),
	}
}

// Register 注册一个工具处理器。
//
// schema 可为 nil（表示无参数函数），或由 SchemaBuilder.Build() 生成。
func (r *ToolRegistry) Register(name, description string, handler ToolHandler, schema map[string]any) {
	r.entries[name] = ToolEntry{
		Handler:     handler,
		Description: description,
		Schema:      schema,
	}
}

// RegisterSimple 注册一个无参数的工具处理器。
func (r *ToolRegistry) RegisterSimple(name, description string, handler ToolHandler) {
	r.Register(name, description, handler, nil)
}

// Get 获取已注册的工具条目。返回 false 表示未注册。
func (r *ToolRegistry) Get(name string) (ToolEntry, bool) {
	entry, ok := r.entries[name]
	return entry, ok
}

// Names 返回所有已注册的工具名。
func (r *ToolRegistry) Names() []string {
	names := make([]string, 0, len(r.entries))
	for name := range r.entries {
		names = append(names, name)
	}
	return names
}

// BuildTools 将所有已注册的工具转换为 []Tool。
func (r *ToolRegistry) BuildTools() []Tool {
	tools := make([]Tool, 0, len(r.entries))
	for name, entry := range r.entries {
		schema := entry.Schema
		if schema == nil {
			schema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		tools = append(tools, Tool{
			Name:        name,
			Description: entry.Description,
			InputSchema: schema,
		})
	}
	return tools
}

// ============================================================================
// ExecuteToolCalls — 执行工具调用
// ============================================================================

// ToolResult 表示一次工具调用的执行结果。
type ToolResult struct {
	ToolUseID string       // 对应 tool_use 块的 ID
	Block     ContentBlock // 构建好的 tool_result 内容块
}

// ExecuteToolCalls 执行响应中的所有 tool_use 块，返回对应的 tool_result 内容块。
//
// 对于每个 tool_use：
//  1. 从 registry 中查找对应处理器
//  2. 执行处理器，获取结果
//  3. 构建 tool_result ContentBlock（ID 匹配）
//
// 如果某个工具未注册，返回包含错误信息的 tool_result（is_error=true）。
// 如果处理器返回 error，错误信息放入 tool_result（is_error=true）。
func (r *ToolRegistry) ExecuteToolCalls(resp *MessageResponse) []ContentBlock {
	entries := ExtractToolUse(resp)
	if len(entries) == 0 {
		return nil
	}

	blocks := make([]ContentBlock, 0, len(entries))
	for _, entry := range entries {
		blocks = append(blocks, r.executeOne(entry.ID, entry.Name, entry.Input))
	}
	return blocks
}

// executeOne 执行单个工具调用。
func (r *ToolRegistry) executeOne(toolUseID, name string, input json.RawMessage) ContentBlock {
	// 解析输入参数
	var args map[string]any
	if len(input) > 0 {
		if err := json.Unmarshal(input, &args); err != nil {
			return ToolResultErrorBlock(toolUseID, fmt.Sprintf("failed to parse tool input: %v", err))
		}
	}
	if args == nil {
		args = make(map[string]any)
	}

	// 查找处理器
	regEntry, ok := r.entries[name]
	if !ok {
		return ToolResultErrorBlock(toolUseID, fmt.Sprintf("unknown tool: %s", name))
	}

	// 执行
	result, err := regEntry.Handler(args)
	if err != nil {
		return ToolResultErrorBlock(toolUseID, err.Error())
	}

	return ToolResultBlock(toolUseID, result)
}

// ============================================================================
// RunToolLoop — 自动工具调用循环
//
// 自动处理多轮工具调用，直到 Claude 返回纯文本回复（end_turn）或达到最大轮次。
//
// 用法：
//
//	registry := NewToolRegistry()
//	registry.Register("get_weather", "Get weather", handler, schema)
//
//	resp, err := anthropic.RunToolLoop(ctx, client, req, registry, nil)
//	if err != nil { ... }
//	text := anthropic.ExtractText(resp)
// ============================================================================

// ToolLoopOptions 工具调用循环的可选配置。
type ToolLoopOptions struct {
	// MaxRounds 最大调用轮次（默认 10）。
	// 每轮包含一次 API 请求 + 可能的工具执行。
	MaxRounds int

	// OnToolUse 在每次工具被调用前触发（可用于日志或确认）。
	// 返回 error 会中断循环。
	OnToolUse func(entry *ToolUseEntry) error

	// OnToolResult 在每次工具执行完成后触发。
	OnToolResult func(entry *ToolUseEntry, block ContentBlock)
}

// ErrMaxRoundsExceeded 表示工具调用循环达到最大轮次。
var ErrMaxRoundsExceeded = errors.New("anthropic: tool loop exceeded max rounds")

// RunToolLoop 自动执行工具调用循环。
//
// 循环逻辑：
//  1. 发送初始请求
//  2. 如果响应包含 tool_use → 通过 registry 执行 → 追加 assistant 响应和 tool_result → 重新发送
//  3. 如果响应不包含 tool_use → 返回最终响应
//  4. 超过 MaxRounds → 返回最后一个响应和 ErrMaxRoundsExceeded
//
// opts 可为 nil，使用默认配置（MaxRounds=10）。
func RunToolLoop(
	ctx context.Context,
	client *Client,
	req MessageRequest,
	registry *ToolRegistry,
	opts *ToolLoopOptions,
) (*MessageResponse, error) {
	maxRounds := 10
	if opts != nil && opts.MaxRounds > 0 {
		maxRounds = opts.MaxRounds
	}

	// 确保 Tools 包含 registry 中的工具
	if registry != nil && len(req.Tools) == 0 {
		req.Tools = registry.BuildTools()
	}

	// 复制 messages 避免修改原始
	messages := make([]Message, len(req.Messages))
	copy(messages, req.Messages)

	var lastResp *MessageResponse

	for round := 0; round < maxRounds; round++ {
		req.Messages = messages
		resp, err := client.CreateMessage(ctx, req)
		if err != nil {
			return nil, errs.Wrapf(err, "round %d", round+1)
		}
		lastResp = resp

		// 检查是否有工具调用
		entries := ExtractToolUse(resp)
		if len(entries) == 0 {
			return resp, nil
		}

		// 回调：OnToolUse
		for i := range entries {
			entry := entries[i]
			if opts != nil && opts.OnToolUse != nil {
				if err := opts.OnToolUse(&entry); err != nil {
					return resp, errs.Wrapf(err, "on_tool_use %s", entry.Name)
				}
			}
		}

		// 执行工具调用
		var resultBlocks []ContentBlock
		if registry != nil {
			resultBlocks = registry.ExecuteToolCalls(resp)
		} else {
			// 没有 registry：为每个调用返回错误
			resultBlocks = make([]ContentBlock, len(entries))
			for i, entry := range entries {
				resultBlocks[i] = ToolResultErrorBlock(entry.ID, "no tool registry provided")
			}
		}

		// 回调：OnToolResult
		if opts != nil && opts.OnToolResult != nil {
			for i, entry := range entries {
				if i < len(resultBlocks) {
					opts.OnToolResult(&entry, resultBlocks[i])
				}
			}
		}

		// 追加 assistant 响应（完整回传 content）
		messages = append(messages, Message{
			Role:    RoleAssistant,
			Content: MessageContent(resp.Content),
		})

		// 追加 tool_result（必须在 user 角色的消息中）
		messages = append(messages, Message{
			Role:    RoleUser,
			Content: MessageContent(resultBlocks),
		})
	}

	return lastResp, ErrMaxRoundsExceeded
}

// ============================================================================
// 并行工具调用辅助
// ============================================================================

// BuildParallelToolResults 为并行工具调用构建 tool_result 块。
//
// results 是一个 map: toolUseID -> 结果。
func BuildParallelToolResults(entries []ToolUseEntry, results map[string]any) []ContentBlock {
	blocks := make([]ContentBlock, 0, len(entries))
	for _, entry := range entries {
		result, ok := results[entry.ID]
		if !ok {
			blocks = append(blocks, ToolResultErrorBlock(entry.ID, "no result for this tool call"))
			continue
		}
		blocks = append(blocks, ToolResultBlock(entry.ID, result))
	}
	return blocks
}

// BuildParallelToolResultsWithErrors 类似 BuildParallelToolResults，
// 但结果 map 中可以包含 error 值（自动转为 is_error=true 的 tool_result）。
func BuildParallelToolResultsWithErrors(entries []ToolUseEntry, results map[string]any) []ContentBlock {
	blocks := make([]ContentBlock, 0, len(entries))
	for _, entry := range entries {
		result, ok := results[entry.ID]
		if !ok {
			blocks = append(blocks, ToolResultErrorBlock(entry.ID, "no result for this tool call"))
			continue
		}
		if err, isErr := result.(error); isErr {
			blocks = append(blocks, ToolResultErrorBlock(entry.ID, err.Error()))
			continue
		}
		blocks = append(blocks, ToolResultBlock(entry.ID, result))
	}
	return blocks
}
