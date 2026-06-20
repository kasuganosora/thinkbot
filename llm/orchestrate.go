package llm

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/kasuganosora/thinkbot/util/errs"
	"github.com/kasuganosora/thinkbot/util/traceid"
)

// ============================================================================
// Multi-step orchestration: automatic tool execution loop
// ============================================================================

// OrchestrateConfig holds both provider-level params and orchestration settings.
type OrchestrateConfig struct {
	Params GenerateParams

	// MaxSteps controls the tool auto-execution loop.
	//   0  = single LLM call, no auto-execution (default)
	//  >0  = at most N LLM calls
	//  -1  = unlimited loop until LLM stops producing tool calls
	MaxSteps int

	// OnFinish is called once when all steps complete.
	OnFinish func(*GenerateResult)

	// OnStep is called after each step (LLM call + tool round).
	// If the callback returns a non-nil *GenerateParams, it overrides the params
	// for the next step.
	OnStep func(*StepResult) *GenerateParams

	// PrepareStep is called before each step (starting from the second step).
	// It receives the current params and may return new params to override them.
	PrepareStep func(*GenerateParams) *GenerateParams

	// ApprovalHandler decides how to handle a tool call marked with RequireApproval.
	ApprovalHandler func(ctx context.Context, call ToolCall) (ToolApprovalResult, error)
}

// OrchestrateOption configures a multi-step generation request.
type OrchestrateOption func(*OrchestrateConfig)

// WithMaxSteps sets the maximum number of LLM calls in the tool-execution loop.
//
//	0  (default) = single call, no auto tool execution
//	N  (N > 0)   = at most N calls
//	-1           = unlimited, loops until LLM stops requesting tools
func WithMaxSteps(n int) OrchestrateOption {
	return func(c *OrchestrateConfig) { c.MaxSteps = n }
}

// WithOnFinish registers a callback invoked once when all steps complete.
func WithOnFinish(fn func(*GenerateResult)) OrchestrateOption {
	return func(c *OrchestrateConfig) { c.OnFinish = fn }
}

// WithOnStep registers a callback invoked after each step (LLM call + tool round).
func WithOnStep(fn func(*StepResult) *GenerateParams) OrchestrateOption {
	return func(c *OrchestrateConfig) { c.OnStep = fn }
}

// WithPrepareStep registers a callback invoked before each step.
func WithPrepareStep(fn func(*GenerateParams) *GenerateParams) OrchestrateOption {
	return func(c *OrchestrateConfig) { c.PrepareStep = fn }
}

// WithApprovalHandler registers a function that decides how to handle a tool
// call marked with RequireApproval.
func WithApprovalHandler(fn func(ctx context.Context, call ToolCall) (ToolApprovalResult, error)) OrchestrateOption {
	return func(c *OrchestrateConfig) { c.ApprovalHandler = fn }
}

// ErrToolApprovalDeferred is returned when a tool approval is deferred.
var ErrToolApprovalDeferred = errors.New("llm: tool approval deferred")

// ToolApprovalDeferredError wraps a deferred approval result.
type ToolApprovalDeferredError struct {
	Approval ToolApprovalResult
}

func (e *ToolApprovalDeferredError) Error() string {
	if e == nil || e.Approval.ApprovalID == "" {
		return ErrToolApprovalDeferred.Error()
	}
	return fmt.Sprintf("%s: %s", ErrToolApprovalDeferred, e.Approval.ApprovalID)
}

func (e *ToolApprovalDeferredError) Is(target error) bool {
	return target == ErrToolApprovalDeferred
}

// --- Orchestrated generate (non-streaming) ---

// OrchestrateGenerate performs a multi-step generation with automatic tool execution.
// If cfg.MaxSteps == 0, it delegates to a single provider call.
func OrchestrateGenerate(ctx context.Context, prov Provider, cfg *OrchestrateConfig) (*GenerateResult, error) {
	// Resolve tool schemas
	for i := range cfg.Params.Tools {
		schema, err := resolveSchema(cfg.Params.Tools[i].Parameters)
		if err != nil {
			return nil, errs.Wrapf(err, "llm: tool %q", cfg.Params.Tools[i].Name)
		}
		cfg.Params.Tools[i].Parameters = schema
	}

	// Single-step fast path
	if cfg.MaxSteps == 0 {
		result, err := prov.DoGenerate(ctx, cfg.Params)
		if err != nil {
			return nil, err
		}
		stepMsgs := buildStepMessages(result.Text, result.Reasoning, result.ReasoningProviderMetadata, result.ToolCalls, nil, &result.Usage)
		step := StepResult{
			Text:            result.Text,
			Reasoning:       result.Reasoning,
			FinishReason:    result.FinishReason,
			RawFinishReason: result.RawFinishReason,
			Usage:           result.Usage,
			ToolCalls:       result.ToolCalls,
			Response:        result.Response,
			Messages:        stepMsgs,
		}
		result.Steps = []StepResult{step}
		result.Messages = stepMsgs
		applyOnStep(cfg, &step)
		if cfg.OnFinish != nil {
			cfg.OnFinish(result)
		}
		return result, nil
	}

	toolMap := buildToolMap(cfg.Params.Tools)
	messages := make([]Message, len(cfg.Params.Messages))
	copy(messages, cfg.Params.Messages)

	var (
		totalUsage  Usage
		lastResult  *GenerateResult
		allSteps    []StepResult
		allMessages []Message
	)

	for step := 0; shouldContinueLoop(cfg.MaxSteps, step); step++ {
		if step > 0 {
			messages = applyPrepareStep(cfg, messages)
		}

		params := cfg.Params
		params.Messages = messages

		result, err := prov.DoGenerate(ctx, params)
		if err != nil {
			return nil, err
		}
		lastResult = result
		totalUsage.Add(&result.Usage)

		// No tool calls or not a tool-calls finish → final step
		if result.FinishReason != FinishReasonToolCalls || len(result.ToolCalls) == 0 || !hasExecutableTools(result.ToolCalls, toolMap) {
			stepMsgs := buildStepMessages(result.Text, result.Reasoning, result.ReasoningProviderMetadata, result.ToolCalls, nil, &result.Usage)
			sr := StepResult{
				Text:            result.Text,
				Reasoning:       result.Reasoning,
				FinishReason:    result.FinishReason,
				RawFinishReason: result.RawFinishReason,
				Usage:           result.Usage,
				ToolCalls:       result.ToolCalls,
				Response:        result.Response,
				Messages:        stepMsgs,
			}
			allSteps = append(allSteps, sr)
			allMessages = append(allMessages, stepMsgs...)
			applyOnStep(cfg, &sr)
			break
		}

		// Execute tools
		toolResults, err := executeTools(ctx, result.ToolCalls, toolMap, cfg.ApprovalHandler, nil)
		if err != nil {
			var deferred *ToolApprovalDeferredError
			if errors.As(err, &deferred) {
				stepMsgs := buildStepMessages(result.Text, result.Reasoning, result.ReasoningProviderMetadata, result.ToolCalls, nil, &result.Usage)
				sr := StepResult{
					Text:                 result.Text,
					Reasoning:            result.Reasoning,
					FinishReason:         result.FinishReason,
					RawFinishReason:      result.RawFinishReason,
					Usage:                result.Usage,
					ToolCalls:            result.ToolCalls,
					Response:             result.Response,
					DeferredToolApproval: &deferred.Approval,
					Messages:             stepMsgs,
				}
				allSteps = append(allSteps, sr)
				allMessages = append(allMessages, stepMsgs...)
				applyOnStep(cfg, &sr)
				result.DeferredToolApproval = &deferred.Approval
				break
			}
			return nil, err
		}

		stepMsgs := buildStepMessages(result.Text, result.Reasoning, result.ReasoningProviderMetadata, result.ToolCalls, toolResults, &result.Usage)
		sr := StepResult{
			Text:            result.Text,
			Reasoning:       result.Reasoning,
			FinishReason:    result.FinishReason,
			RawFinishReason: result.RawFinishReason,
			Usage:           result.Usage,
			ToolCalls:       result.ToolCalls,
			ToolResults:     toolCallResultsFromParts(toolResults),
			Response:        result.Response,
			Messages:        stepMsgs,
		}
		allSteps = append(allSteps, sr)
		allMessages = append(allMessages, stepMsgs...)
		applyOnStep(cfg, &sr)

		messages = append(messages, stepMsgs...)
	}

	if lastResult != nil {
		lastResult.Usage = totalUsage
		lastResult.Steps = allSteps
		lastResult.Messages = allMessages
		if lastResult.DeferredToolApproval == nil {
			for i := range allSteps {
				if allSteps[i].DeferredToolApproval != nil {
					lastResult.DeferredToolApproval = allSteps[i].DeferredToolApproval
					break
				}
			}
		}
	}

	logToolCallSummary(ctx, allSteps)

	if cfg.OnFinish != nil && lastResult != nil {
		cfg.OnFinish(lastResult)
	}

	return lastResult, nil
}

// --- Orchestrated stream (multi-step) ---

// OrchestrateStream performs a multi-step streaming generation with automatic
// tool execution. All stream parts from all steps are forwarded through a single
// channel. If cfg.MaxSteps == 0, it delegates directly to the provider.
func OrchestrateStream(ctx context.Context, prov Provider, cfg *OrchestrateConfig) (*StreamResult, error) {
	// Resolve tool schemas
	for i := range cfg.Params.Tools {
		schema, err := resolveSchema(cfg.Params.Tools[i].Parameters)
		if err != nil {
			return nil, errs.Wrapf(err, "llm: tool %q", cfg.Params.Tools[i].Name)
		}
		cfg.Params.Tools[i].Parameters = schema
	}

	// Single-step fast path
	if cfg.MaxSteps == 0 {
		return prov.DoStream(ctx, cfg.Params)
	}

	toolMap := buildToolMap(cfg.Params.Tools)
	messages := make([]Message, len(cfg.Params.Messages))
	copy(messages, cfg.Params.Messages)

	ch := make(chan StreamPart, 64)
	sr := &StreamResult{Stream: ch}

	go func() {
		send := func(part StreamPart) bool {
			select {
			case ch <- part:
				return true
			case <-ctx.Done():
				return false
			}
		}

		var totalUsage Usage
		var lastFinishReason FinishReason
		var lastRawFinishReason string
		var allSteps []StepResult
		var allMessages []Message

		for step := 0; shouldContinueLoop(cfg.MaxSteps, step); step++ {
			if step > 0 {
				messages = applyPrepareStep(cfg, messages)
			}

			params := cfg.Params
			params.Messages = messages

			provSR, err := prov.DoStream(ctx, params)
			if err != nil {
				send(&ErrorPart{Error: errs.Wrapf(err, "llm: stream step %d", step)})
				break
			}

			var (
				stepText          string
				stepReasoning     string
				stepReasoningMeta map[string]any
				stepToolCalls     []ToolCall
				stepUsage         Usage
				stepResponse      ResponseMetadata
			)

			for part := range provSR.Stream {
				switch p := part.(type) {
				case *TextDeltaPart:
					stepText += p.Text
				case *ReasoningDeltaPart:
					stepReasoning += p.Text
				case *ReasoningEndPart:
					if p.ProviderMetadata != nil {
						stepReasoningMeta = p.ProviderMetadata
					}
				case *StreamToolCallPart:
					stepToolCalls = append(stepToolCalls, ToolCall{
						ToolCallID: p.ToolCallID,
						ToolName:   p.ToolName,
						Input:      p.Input,
					})
				case *FinishStepPart:
					stepUsage = p.Usage
					stepResponse = p.Response
					lastFinishReason = p.FinishReason
					lastRawFinishReason = p.RawFinishReason
				case *FinishPart:
					lastFinishReason = p.FinishReason
					lastRawFinishReason = p.RawFinishReason
					continue
				}

				if !send(part) {
					break
				}
			}

			totalUsage.Add(&stepUsage)

			// If context was cancelled during streaming, stop immediately.
			if ctx.Err() != nil {
				break
			}

			// No tool calls or not a tool-calls finish → done
			if lastFinishReason != FinishReasonToolCalls || len(stepToolCalls) == 0 || !hasExecutableTools(stepToolCalls, toolMap) {
				stepMsgs := buildStepMessages(stepText, stepReasoning, stepReasoningMeta, stepToolCalls, nil, &stepUsage)
				stepR := StepResult{
					Text:            stepText,
					Reasoning:       stepReasoning,
					FinishReason:    lastFinishReason,
					RawFinishReason: lastRawFinishReason,
					Usage:           stepUsage,
					ToolCalls:       stepToolCalls,
					Response:        stepResponse,
					Messages:        stepMsgs,
				}
				allSteps = append(allSteps, stepR)
				allMessages = append(allMessages, stepMsgs...)
				applyOnStep(cfg, &stepR)
				break
			}

			// Execute tools
			sendProgress := func(part StreamPart) { send(part) }
			toolResults, err := executeTools(ctx, stepToolCalls, toolMap, cfg.ApprovalHandler, sendProgress)
			if err != nil {
				var deferred *ToolApprovalDeferredError
				if errors.As(err, &deferred) {
					stepMsgs := buildStepMessages(stepText, stepReasoning, stepReasoningMeta, stepToolCalls, nil, &stepUsage)
					stepR := StepResult{
						Text:                 stepText,
						Reasoning:            stepReasoning,
						FinishReason:         lastFinishReason,
						RawFinishReason:      lastRawFinishReason,
						Usage:                stepUsage,
						ToolCalls:            stepToolCalls,
						Response:             stepResponse,
						DeferredToolApproval: &deferred.Approval,
						Messages:             stepMsgs,
					}
					allSteps = append(allSteps, stepR)
					allMessages = append(allMessages, stepMsgs...)
					applyOnStep(cfg, &stepR)
					break
				}
				send(&ErrorPart{Error: err})
				break
			}

			stepMsgs := buildStepMessages(stepText, stepReasoning, stepReasoningMeta, stepToolCalls, toolResults, &stepUsage)
			stepR := StepResult{
				Text:            stepText,
				Reasoning:       stepReasoning,
				FinishReason:    lastFinishReason,
				RawFinishReason: lastRawFinishReason,
				Usage:           stepUsage,
				ToolCalls:       stepToolCalls,
				ToolResults:     toolCallResultsFromParts(toolResults),
				Response:        stepResponse,
				Messages:        stepMsgs,
			}
			allSteps = append(allSteps, stepR)
			allMessages = append(allMessages, stepMsgs...)
			applyOnStep(cfg, &stepR)

			messages = append(messages, stepMsgs...)
		}

		// Populate StreamResult fields before closing the channel.
		sr.Steps = allSteps
		sr.Messages = allMessages
		for i := range allSteps {
			if allSteps[i].DeferredToolApproval != nil {
				sr.DeferredToolApproval = allSteps[i].DeferredToolApproval
				break
			}
		}

		send(&FinishPart{
			FinishReason:    lastFinishReason,
			RawFinishReason: lastRawFinishReason,
			TotalUsage:      totalUsage,
		})

		logToolCallSummary(ctx, allSteps)

		if cfg.OnFinish != nil {
			cfg.OnFinish(&GenerateResult{
				FinishReason:         lastFinishReason,
				RawFinishReason:      lastRawFinishReason,
				Usage:                totalUsage,
				Steps:                allSteps,
				Messages:             allMessages,
				DeferredToolApproval: sr.DeferredToolApproval,
			})
		}

		close(ch)
	}()

	return sr, nil
}

// ============================================================================
// Step helpers
// ============================================================================

// logToolCallSummary 从所有步骤中汇总工具调用统计并记录日志。
// 记录总调用次数和去重后的工具名称列表。
func logToolCallSummary(ctx context.Context, steps []StepResult) {
	totalCalls := 0
	toolSet := make(map[string]struct{})

	for _, step := range steps {
		for _, tc := range step.ToolCalls {
			totalCalls++
			toolSet[tc.ToolName] = struct{}{}
		}
	}

	if totalCalls == 0 {
		return
	}

	// 去重排序后的工具名列表
	uniqueTools := make([]string, 0, len(toolSet))
	for name := range toolSet {
		uniqueTools = append(uniqueTools, name)
	}
	sort.Strings(uniqueTools)

	logger := traceid.L(ctx)
	if logger == nil {
		return
	}

	logger.Infow("tool call summary",
		"total_calls", totalCalls,
		"unique_tools", uniqueTools,
		"steps", len(steps),
	)
}

func buildToolMap(tools []Tool) map[string]*Tool {
	m := make(map[string]*Tool, len(tools))
	for i := range tools {
		m[tools[i].Name] = &tools[i]
	}
	return m
}

func hasExecutableTools(toolCalls []ToolCall, toolMap map[string]*Tool) bool {
	for _, tc := range toolCalls {
		if t, ok := toolMap[tc.ToolName]; ok && t.Execute != nil {
			return true
		}
	}
	return false
}

func shouldContinueLoop(maxSteps, step int) bool {
	if maxSteps < 0 {
		return true
	}
	return step < maxSteps
}

// buildStepMessages creates the messages produced by a step: an assistant
// message (text/reasoning/tool-calls) and optionally a tool message.
func buildStepMessages(text, reasoning string, reasoningMeta map[string]any, toolCalls []ToolCall, toolResults []ToolResultPart, usage *Usage) []Message {
	var assistantParts []MessagePart
	if reasoning != "" {
		assistantParts = append(assistantParts, ReasoningPart{Text: reasoning, ProviderMetadata: reasoningMeta})
	}
	if text != "" {
		assistantParts = append(assistantParts, TextPart{Text: text})
	}
	for _, tc := range toolCalls {
		assistantParts = append(assistantParts, ToolCallPart{
			ToolCallID: tc.ToolCallID,
			ToolName:   tc.ToolName,
			Input:      tc.Input,
		})
	}

	msgs := []Message{{Role: MessageRoleAssistant, Content: assistantParts, Usage: usage}}
	if len(toolResults) > 0 {
		msgs = append(msgs, ToolMessage(toolResults...))
	}
	return msgs
}

func applyOnStep(cfg *OrchestrateConfig, stepResult *StepResult) {
	if cfg.OnStep == nil {
		return
	}
	if override := cfg.OnStep(stepResult); override != nil {
		// Preserve Tools from override if provided, otherwise keep original
		if len(override.Tools) == 0 {
			override.Tools = cfg.Params.Tools
		}
		cfg.Params = *override
	}
}

func applyPrepareStep(cfg *OrchestrateConfig, messages []Message) []Message {
	if cfg.PrepareStep == nil {
		return messages
	}
	cfg.Params.Messages = messages
	if override := cfg.PrepareStep(&cfg.Params); override != nil {
		// Preserve Tools from override if provided, otherwise keep original
		if len(override.Tools) == 0 {
			override.Tools = cfg.Params.Tools
		}
		cfg.Params = *override
	}
	return cfg.Params.Messages
}

func toolCallResultsFromParts(parts []ToolResultPart) []ToolResult {
	out := make([]ToolResult, len(parts))
	for i, p := range parts {
		out[i] = ToolResult{
			ToolCallID: p.ToolCallID,
			ToolName:   p.ToolName,
			Output:     p.Result,
		}
	}
	return out
}

// ============================================================================
// Tool execution (with approval + parallel execution)
// ============================================================================

type pendingToolExec struct {
	idx  int
	tc   ToolCall
	tool *Tool
}

func executeTools(
	ctx context.Context,
	toolCalls []ToolCall,
	toolMap map[string]*Tool,
	approvalHandler func(context.Context, ToolCall) (ToolApprovalResult, error),
	sendProgress func(StreamPart),
) ([]ToolResultPart, error) {
	results := make([]ToolResultPart, len(toolCalls))
	pending := make([]pendingToolExec, 0, len(toolCalls))

	// Phase 1: resolve tools and check approvals (sequential, user-facing).
	for i, tc := range toolCalls {
		tool, ok := toolMap[tc.ToolName]
		if !ok || tool.Execute == nil {
			results[i] = ToolResultPart{
				ToolCallID: tc.ToolCallID,
				ToolName:   tc.ToolName,
				Result:     fmt.Sprintf("tool %q not found or has no execute handler", tc.ToolName),
				IsError:    true,
			}
			continue
		}

		if tool.RequireApproval {
			if approvalHandler == nil {
				results[i] = ToolResultPart{
					ToolCallID: tc.ToolCallID,
					ToolName:   tc.ToolName,
					Result:     "tool execution denied: no approval handler",
					IsError:    true,
				}
				continue
			}

			approval, err := approvalHandler(ctx, tc)
			if err != nil {
				return nil, errs.Wrapf(err, "llm: approval handler for %q", tc.ToolName)
			}
			switch approval.Decision {
			case "", ToolApprovalApproved:
				// Continue to execution below.
			case ToolApprovalRejected:
				results[i] = ToolResultPart{
					ToolCallID: tc.ToolCallID,
					ToolName:   tc.ToolName,
					Result:     rejectedToolResultText(approval),
					IsError:    true,
				}
				continue
			case ToolApprovalDeferred:
				return nil, &ToolApprovalDeferredError{Approval: approval}
			default:
				return nil, fmt.Errorf("llm: unknown approval decision %q for %q", approval.Decision, tc.ToolName)
			}
		}

		pending = append(pending, pendingToolExec{idx: i, tc: tc, tool: tool})
	}

	// Phase 2: execute approved tools in parallel.
	if len(pending) == 1 {
		results[pending[0].idx] = runTool(ctx, pending[0].tc, pending[0].tool, sendProgress)
	} else if len(pending) > 1 {
		var wg sync.WaitGroup
		wg.Add(len(pending))
		for _, p := range pending {
			go func(p pendingToolExec) {
				defer wg.Done()
				results[p.idx] = runTool(ctx, p.tc, p.tool, sendProgress)
			}(p)
		}
		wg.Wait()
	}

	return results, nil
}

func rejectedToolResultText(approval ToolApprovalResult) string {
	if approval.Reason != "" {
		return "tool execution denied by user: " + approval.Reason
	}
	return "tool execution denied by user"
}

func runTool(ctx context.Context, tc ToolCall, tool *Tool, sendProgress func(StreamPart)) ToolResultPart {
	var progressFn func(content any)
	if sendProgress != nil {
		progressFn = func(content any) {
			sendProgress(&ToolProgressPart{
				ToolCallID: tc.ToolCallID,
				ToolName:   tc.ToolName,
				Content:    content,
			})
		}
	}

	execCtx := &ToolExecContext{
		Context:      ctx,
		ToolCallID:   tc.ToolCallID,
		ToolName:     tc.ToolName,
		SendProgress: progressFn,
	}

	output, err := tool.Execute(execCtx, tc.Input)
	if err != nil {
		if sendProgress != nil {
			sendProgress(&StreamToolErrorPart{
				ToolCallID: tc.ToolCallID,
				ToolName:   tc.ToolName,
				Error:      err,
			})
		}
		return ToolResultPart{
			ToolCallID: tc.ToolCallID,
			ToolName:   tc.ToolName,
			Result:     err.Error(),
			IsError:    true,
		}
	}

	if sendProgress != nil {
		sendProgress(&StreamToolResultPart{
			ToolCallID: tc.ToolCallID,
			ToolName:   tc.ToolName,
			Input:      tc.Input,
			Output:     output,
		})
	}
	return ToolResultPart{
		ToolCallID: tc.ToolCallID,
		ToolName:   tc.ToolName,
		Result:     output,
	}
}
