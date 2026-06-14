package google

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// ============================================================================
// SchemaBuilder — 流式 JSON Schema 构建器
//
// 用于构建 FunctionDeclaration 的 Parameters 字段，避免手写 JSON 字符串。
//
// 用法：
//
//	schema := NewSchema().
//	    PropString("location", "City name", true).
//	    PropStringEnum("unit", "Temperature unit", false, "celsius", "fahrenheit").
//	    Build()
//	fd := NewFunctionDeclaration("get_weather", "Get weather for a location", schema)
// ============================================================================

// SchemaProperty 描述一个参数的 JSON Schema 片段。
type SchemaProperty struct {
	Type        string          `json:"type"`
	Description string          `json:"description,omitempty"`
	Enum        []string        `json:"enum,omitempty"`
	Items       *SchemaProperty `json:"items,omitempty"`
}

// SchemaBuilder 流式构建函数参数的 JSON Schema。
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

// Build 生成 JSON Schema 的 json.RawMessage。
func (b *SchemaBuilder) Build() json.RawMessage {
	schema := struct {
		Type       string                    `json:"type"`
		Properties map[string]SchemaProperty `json:"properties"`
		Required   []string                  `json:"required,omitempty"`
	}{
		Type:       "object",
		Properties: b.properties,
		Required:   b.required,
	}
	data, _ := json.Marshal(schema)
	return data
}

// ============================================================================
// 函数声明 & 工具构建器
// ============================================================================

// NewFunctionDeclaration 创建一个函数声明。
//
// schema 通常由 SchemaBuilder.Build() 生成，也可传入手写的 json.RawMessage。
func NewFunctionDeclaration(name, description string, schema json.RawMessage) FunctionDeclaration {
	return FunctionDeclaration{
		Name:        name,
		Description: description,
		Parameters:  schema,
	}
}

// NewSimpleFunctionDeclaration 创建一个无参数的函数声明。
func NewSimpleFunctionDeclaration(name, description string) FunctionDeclaration {
	return FunctionDeclaration{
		Name:        name,
		Description: description,
	}
}

// NewFunctionTool 创建一个包含单个函数声明的 Tool。
func NewFunctionTool(decl FunctionDeclaration) Tool {
	return Tool{FunctionDeclarations: []FunctionDeclaration{decl}}
}

// NewFunctionToolFromDecls 创建一个包含多个函数声明的 Tool。
func NewFunctionToolFromDecls(decls ...FunctionDeclaration) Tool {
	return Tool{FunctionDeclarations: decls}
}

// NewToolConfig 创建工具配置。
func NewToolConfig(mode FunctionCallingMode, allowedFunctionNames ...string) *ToolConfig {
	tc := &ToolConfig{
		FunctionCallingConfig: &FunctionCallingConfig{
			Mode: mode,
		},
	}
	if len(allowedFunctionNames) > 0 {
		tc.FunctionCallingConfig.AllowedFunctionNames = allowedFunctionNames
	}
	return tc
}

// ============================================================================
// 响应检查辅助
// ============================================================================

// HasFunctionCalls 检查响应中是否包含函数调用。
func HasFunctionCalls(resp *GenerateContentResponse) bool {
	if resp == nil {
		return false
	}
	for _, cand := range resp.Candidates {
		for _, part := range cand.Content.Parts {
			if part.FunctionCall != nil {
				return true
			}
		}
	}
	return false
}

// ExtractFunctionCalls 从响应中提取所有函数调用。
//
// 返回按出现顺序排列的所有 FunctionCall。适用于并行函数调用。
func ExtractFunctionCalls(resp *GenerateContentResponse) []*FunctionCall {
	if resp == nil {
		return nil
	}
	var calls []*FunctionCall
	for _, cand := range resp.Candidates {
		for _, part := range cand.Content.Parts {
			if part.FunctionCall != nil {
				calls = append(calls, part.FunctionCall)
			}
		}
	}
	return calls
}

// GetFirstFunctionCall 从响应中提取第一个函数调用。
// 返回 nil 表示响应中没有函数调用。
func GetFirstFunctionCall(resp *GenerateContentResponse) *FunctionCall {
	calls := ExtractFunctionCalls(resp)
	if len(calls) == 0 {
		return nil
	}
	return calls[0]
}

// ExtractText 从响应中提取所有非思考文本（拼接为单个字符串）。
func ExtractText(resp *GenerateContentResponse) string {
	if resp == nil {
		return ""
	}
	var text string
	for _, cand := range resp.Candidates {
		for _, part := range cand.Content.Parts {
			if part.Text != "" && !part.Thought {
				text += part.Text
			}
		}
	}
	return text
}

// ============================================================================
// ToolHandler & ToolRegistry — Go 函数注册表
//
// 将 Gemini 的函数调用映射到 Go 函数，实现自动调度。
//
// 用法：
//
//	registry := NewToolRegistry()
//	registry.Register("get_weather", "Get weather", func(args map[string]any) (any, error) {
//	    city := args["city"].(string)
//	    return map[string]any{"temp": "25°C"}, nil
//	})
//
//	// 自动构建声明
//	tool := registry.BuildTool()
//
//	// 执行模型发起的函数调用
//	respParts, err := registry.Execute(resp)
// ============================================================================

// ToolHandler 是一个 Go 函数处理器的签名。
// 接收模型传入的参数 map，返回结果（将被放入 FunctionResponse.response）或错误。
type ToolHandler func(args map[string]any) (any, error)

// ToolEntry 表示一个已注册的工具。
type ToolEntry struct {
	Handler     ToolHandler
	Description string
	Schema      json.RawMessage // 可选，如未设置则使用空参数 schema
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
func (r *ToolRegistry) Register(name, description string, handler ToolHandler, schema json.RawMessage) {
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

// Names 返回所有已注册的函数名。
func (r *ToolRegistry) Names() []string {
	names := make([]string, 0, len(r.entries))
	for name := range r.entries {
		names = append(names, name)
	}
	return names
}

// BuildTool 将所有已注册的函数转换为 Tool。
func (r *ToolRegistry) BuildTool() Tool {
	decls := make([]FunctionDeclaration, 0, len(r.entries))
	for name, entry := range r.entries {
		decl := FunctionDeclaration{
			Name:        name,
			Description: entry.Description,
			Parameters:  entry.Schema,
		}
		decls = append(decls, decl)
	}
	return Tool{FunctionDeclarations: decls}
}

// ============================================================================
// ExecuteFunctionCalls — 执行函数调用
// ============================================================================

// ExecuteFunctionCalls 执行响应中的所有函数调用，返回对应的函数响应 Parts。
//
// 对于每个 FunctionCall：
//  1. 从 registry 中查找对应处理器
//  2. 执行处理器，获取结果
//  3. 构建 FunctionResponse Part（保留 FunctionCall 的 ID）
//
// 如果某个函数未注册，返回包含错误信息的 FunctionResponse。
// 如果处理器返回 error，错误信息放入 response.error 字段。
func (r *ToolRegistry) ExecuteFunctionCalls(resp *GenerateContentResponse) []Part {
	calls := ExtractFunctionCalls(resp)
	if len(calls) == 0 {
		return nil
	}

	parts := make([]Part, 0, len(calls))
	for _, fc := range calls {
		entry, ok := r.entries[fc.Name]
		if !ok {
			parts = append(parts, FunctionResponsePartWithID(fc.Name, fc.ID, map[string]any{
				"error": fmt.Sprintf("unknown function: %s", fc.Name),
			}))
			continue
		}

		result, err := entry.Handler(fc.Args)
		if err != nil {
			parts = append(parts, FunctionResponsePartWithID(fc.Name, fc.ID, map[string]any{
				"error": err.Error(),
			}))
			continue
		}

		// 将结果转为 map[string]any
		response := normalizeResponse(result)
		parts = append(parts, FunctionResponsePartWithID(fc.Name, fc.ID, response))
	}

	return parts
}

// normalizeResponse 将处理器的返回值转换为 map[string]any。
//
// 如果返回值已经是 map[string]any，直接使用。
// 否则包装为 {"result": value}。
func normalizeResponse(result any) map[string]any {
	if result == nil {
		return map[string]any{"result": nil}
	}
	if m, ok := result.(map[string]any); ok {
		return m
	}
	return map[string]any{"result": result}
}

// ============================================================================
// RunFunctionCallLoop — 自动函数调用循环
//
// 自动处理多轮函数调用，直到模型返回纯文本回复或达到最大轮次。
//
// 用法：
//
//	registry := NewToolRegistry()
//	registry.Register("get_weather", "Get weather", handler, schema)
//
//	resp, err := google.RunFunctionCallLoop(ctx, client, model, req, registry, 10)
//	if err != nil { ... }
//	text := google.ExtractText(resp)
// ============================================================================

// FunctionCallLoopOptions 函数调用循环的可选配置。
type FunctionCallLoopOptions struct {
	// MaxRounds 最大调用轮次（默认 10）。
	// 每轮包含一次 API 请求 + 可能的函数执行。
	MaxRounds int

	// OnFunctionCall 在每次函数被调用前触发（可用于日志或确认）。
	// 返回 error 会中断循环。
	OnFunctionCall func(fc *FunctionCall) error

	// OnFunctionResponse 在每次函数执行完成后触发。
	OnFunctionResponse func(fc *FunctionCall, response map[string]any)

	// PreserveSignatures 是否保留思考签名（Gemini 3 需要）。
	// 默认 true。
	PreserveSignatures *bool
}

// RunFunctionCallLoop 自动执行函数调用循环。
//
// 循环逻辑：
//  1. 发送初始请求
//  2. 如果响应包含函数调用 → 通过 registry 执行 → 构建 FunctionResponse → 重新发送
//  3. 如果响应不包含函数调用 → 返回最终响应
//  4. 超过 MaxRounds → 返回最后一个响应和 ErrMaxRoundsExceeded
//
// opts 可为 nil，使用默认配置（MaxRounds=10, PreserveSignatures=true）。
func RunFunctionCallLoop(
	ctx context.Context,
	client *Client,
	model string,
	req GenerateContentRequest,
	registry *ToolRegistry,
	opts *FunctionCallLoopOptions,
) (*GenerateContentResponse, error) {
	maxRounds := 10
	preserveSigs := true

	if opts != nil {
		if opts.MaxRounds > 0 {
			maxRounds = opts.MaxRounds
		}
		if opts.PreserveSignatures != nil {
			preserveSigs = *opts.PreserveSignatures
		}
	}

	// 确保 Tools 包含 registry 中的函数
	if registry != nil && len(req.Tools) == 0 {
		req.Tools = []Tool{registry.BuildTool()}
	}

	// 复制 contents 避免修改原始
	contents := make([]Content, len(req.Contents))
	copy(contents, req.Contents)

	var lastResp *GenerateContentResponse

	for round := 0; round < maxRounds; round++ {
		req.Contents = contents
		resp, err := client.GenerateContent(ctx, model, req)
		if err != nil {
			return nil, fmt.Errorf("round %d: %w", round+1, err)
		}
		lastResp = resp

		// 检查是否有函数调用
		if !HasFunctionCalls(resp) {
			return resp, nil
		}

		// 回调
		calls := ExtractFunctionCalls(resp)
		for _, fc := range calls {
			if opts != nil && opts.OnFunctionCall != nil {
				if err := opts.OnFunctionCall(fc); err != nil {
					return resp, fmt.Errorf("on_function_call %s: %w", fc.Name, err)
				}
			}
		}

		// 执行函数调用
		var respParts []Part
		if registry != nil {
			respParts = registry.ExecuteFunctionCalls(resp)
		} else {
			// 没有 registry：为每个调用返回错误
			respParts = make([]Part, len(calls))
			for i, fc := range calls {
				respParts[i] = FunctionResponsePartWithID(fc.Name, fc.ID, map[string]any{
					"error": "no tool registry provided",
				})
			}
		}

		// 回调
		for i, fc := range calls {
			if opts != nil && opts.OnFunctionResponse != nil && i < len(respParts) {
				if respParts[i].FunctionResponse != nil {
					opts.OnFunctionResponse(fc, respParts[i].FunctionResponse.Response)
				}
			}
		}

		// 保留模型响应（包含签名）并追加函数响应
		var modelContent *Content
		if preserveSigs {
			modelContent = PreserveModelContent(resp)
		} else {
			// 不保留签名，但仍需要模型内容
			if resp != nil && len(resp.Candidates) > 0 {
				modelContent = &Content{
					Role:  RoleModel,
					Parts: make([]Part, len(resp.Candidates[0].Content.Parts)),
				}
				copy(modelContent.Parts, resp.Candidates[0].Content.Parts)
				// 清除签名
				for i := range modelContent.Parts {
					modelContent.Parts[i].ThoughtSignature = ""
				}
			}
		}

		if modelContent != nil {
			contents = append(contents, *modelContent)
		}
		contents = append(contents, Content{
			Role:  RoleUser,
			Parts: respParts,
		})
	}

	// 达到最大轮次，直接返回 ErrMaxRoundsExceeded。
	// 注意：不做签名验证 — API 本身会在下次请求时验证（Gemini 3 返回 400）。
	return lastResp, ErrMaxRoundsExceeded
}

// ErrMaxRoundsExceeded 表示函数调用循环达到最大轮次。
var ErrMaxRoundsExceeded = errors.New("google: function call loop exceeded max rounds")

// ============================================================================
// 并行函数调用辅助
// ============================================================================

// BuildParallelFunctionResponses 为并行函数调用构建函数响应。
//
// 每个 FunctionCall 通过 ID 匹配对应的执行结果。
// results 是一个 map: functionCallID -> 结果。
//
// 用法：
//
//	calls := ExtractFunctionCalls(resp)
//	results := make(map[string]any)
//	for _, fc := range calls {
//	    results[fc.ID] = myHandler(fc.Args)
//	}
//	parts := BuildParallelFunctionResponses(calls, results)
func BuildParallelFunctionResponses(calls []*FunctionCall, results map[string]any) []Part {
	parts := make([]Part, 0, len(calls))
	for _, fc := range calls {
		result, ok := results[fc.ID]
		if !ok {
			parts = append(parts, FunctionResponsePartWithID(fc.Name, fc.ID, map[string]any{
				"error": "no result for this function call",
			}))
			continue
		}
		parts = append(parts, FunctionResponsePartWithID(fc.Name, fc.ID, normalizeResponse(result)))
	}
	return parts
}

// BuildParallelFunctionResponsesWithErrors 类似 BuildParallelFunctionResponses，
// 但结果 map 中可以包含 error 值。
//
// results map: functionCallID -> (any result or error)
func BuildParallelFunctionResponsesWithErrors(calls []*FunctionCall, results map[string]any) []Part {
	parts := make([]Part, 0, len(calls))
	for _, fc := range calls {
		result, ok := results[fc.ID]
		if !ok {
			parts = append(parts, FunctionResponsePartWithID(fc.Name, fc.ID, map[string]any{
				"error": "no result for this function call",
			}))
			continue
		}
		if err, isErr := result.(error); isErr {
			parts = append(parts, FunctionResponsePartWithID(fc.Name, fc.ID, map[string]any{
				"error": err.Error(),
			}))
			continue
		}
		parts = append(parts, FunctionResponsePartWithID(fc.Name, fc.ID, normalizeResponse(result)))
	}
	return parts
}
