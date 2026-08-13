package llm

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
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

	// MaxSteps controls the tool auto-execution loop (the "soft" budget).
	//   0  = single LLM call, no auto-execution (default)
	//  >0  = target of at most N LLM calls; the loop may extend past this up to
	//        HardMaxSteps while the model keeps making *new* tool calls (see
	//        loopController), and stops early if it detects a repeat loop.
	//  -1  = unlimited loop until LLM stops producing tool calls
	MaxSteps int

	// HardMaxSteps is the absolute ceiling on LLM calls — the loop never
	// exceeds it regardless of progress. It only takes effect when MaxSteps > 0.
	//   <=0 = auto (MaxSteps * 3)
	//   >0  = explicit ceiling (clamped up to MaxSteps if smaller)
	// This lets a complex task (e.g. a large refactor) automatically extend
	// beyond the soft MaxSteps budget without an operator having to guess the
	// exact number, while still capping runaway loops.
	HardMaxSteps int

	// OnFinish is called once when all steps complete.
	OnFinish func(*GenerateResult)

	// OnStep is called after each step (LLM call + tool round).
	// If the callback returns a non-nil *GenerateParams, it overrides the params
	// for the next step.
	OnStep func(*StepResult) *GenerateParams

	// PrepareStep is called before each step (starting from the second step).
	// It receives the current params and may return new params to override them.
	PrepareStep func(*GenerateParams) *GenerateParams

	// OnToolResults is called after tools execute but before results are
	// appended to the message history. It can modify, filter, or truncate
	// the results (e.g., for context-window management). If nil, results
	// are used as-is.
	OnToolResults func(step int, results []ToolResultPart) []ToolResultPart

	// ApprovalHandler decides how to handle a tool call marked with RequireApproval.
	ApprovalHandler func(ctx context.Context, call ToolCall) (ToolApprovalResult, error)

	// ToolChoiceForStep overrides tool_choice on a per-step basis.
	// It is consulted immediately before each LLM call. If non-nil, its
	// return value replaces cfg.Params.ToolChoice for that single step.
	// The value must be compatible with GenerateParams.ToolChoice
	// (e.g. "auto" | "none" | "required" | {"type":"function",...}).
	//
	// step is the 0-based iteration index; toolsExecuted reports whether at
	// least one executable tool has already run in this orchestration.
	//
	// Typical use (anti-hallucination gate): force "required" on the first
	// step of a verification task so the model physically cannot emit a
	// final answer without calling a tool, then relax to nil once real tool
	// results exist. See agent/pipeline/verification_gate.go.
	ToolChoiceForStep func(step int, toolsExecuted bool) any

	// ToolDeferral enables lazy loading of deferred tools (Claude-style
	// defer_loading / tool search). When non-nil and the tool list contains
	// deferred tools, the orchestrator hides their Parameters from the model
	// and injects a tool_search tool; deferred tools are loaded on demand
	// (via tool_search or when the model references them by name). The
	// execution map always keeps the full definitions so loaded tools run.
	ToolDeferral *ToolDeferral

	// InterruptCh 实现 Claude-CLI 风格的「边思考/边输出边补充」：用户在 agent
	// 生成过程中追加的内容，由调用方通过此 channel 投递；编排循环会在下一步
	// 边界（或最终答案收尾前）将其作为一条用户消息注入「同一轮」对话，从而
	// 无需先终止当前生成再重新发起。nil 表示不启用。建议带缓冲（如 cap=16），
	// 避免上游在无人消费时阻塞。
	InterruptCh chan string
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

// WithHardMaxSteps sets the absolute ceiling on LLM calls. The loop may extend
// past MaxSteps (the soft budget) up to this value while the model keeps making
// new tool calls, but never beyond it.
//
//	<=0 = auto (MaxSteps * 3)
//	>0  = explicit ceiling
func WithHardMaxSteps(n int) OrchestrateOption {
	return func(c *OrchestrateConfig) { c.HardMaxSteps = n }
}

// WithInterruptChannel sets the mid-generation user-append channel (Claude-CLI
// style "add context while the agent is still generating"). See OrchestrateConfig.InterruptCh.
func WithInterruptChannel(ch chan string) OrchestrateOption {
	return func(c *OrchestrateConfig) { c.InterruptCh = ch }
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

// WithOnToolResults registers a callback invoked after tools execute,
// before results are appended to the message history.
func WithOnToolResults(fn func(int, []ToolResultPart) []ToolResultPart) OrchestrateOption {
	return func(c *OrchestrateConfig) { c.OnToolResults = fn }
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

	// Apply prompt caching policy based on the provider's capabilities.
	applyProviderCachePolicy(&cfg.Params, prov.Name())

	// Build the execution tool list (full schemas), keeping a stable copy
	// before any per-view mutation. If tool deferral is active, split it into
	// a model-facing view that hides deferred tools' Parameters and injects a
	// tool_search tool; the execution map always keeps the full definitions.
	fullTools := make([]Tool, len(cfg.Params.Tools))
	copy(fullTools, cfg.Params.Tools)
	fullTools = stripSandboxPrefixes(fullTools)

	deferActive := false
	if cfg.ToolDeferral != nil {
		cfg.ToolDeferral.SetTools(fullTools)
		if cfg.ToolDeferral.HasDeferred() {
			deferActive = true
			cfg.Params.Tools = cfg.ToolDeferral.View()
			fullTools = append(fullTools, cfg.ToolDeferral.ExecTool())
		} else {
			cfg.Params.Tools = fullTools
		}
	} else {
		cfg.Params.Tools = fullTools
	}

	// Single-step fast path
	if cfg.MaxSteps == 0 {
		// No orchestration loop, so tool_search cannot be used and deferred
		// tools would expose no schema. Bypass deferral and give the model the
		// full tool list so behavior is well-defined.
		if deferActive {
			cfg.Params.Tools = fullTools
		}
		cfg.Params.Messages = PatchToolCalls(cfg.Params.Messages)
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

	toolMap := buildToolMap(fullTools)
	messages := make([]Message, len(cfg.Params.Messages))
	copy(messages, cfg.Params.Messages)
	messages = PatchToolCalls(messages)

	var (
		totalUsage    Usage
		lastResult    *GenerateResult
		allSteps      []StepResult
		allMessages   []Message
		toolsExecuted bool
	)

	loop := newLoopController(cfg.MaxSteps, cfg.HardMaxSteps)
	for step := 0; ; step++ {
		// 中途追加：把用户在生成中补充的内容注入当前轮（Claude-CLI 风格）。
		// 在下一步边界消费；若已触及硬上限则不再为追加多跑一步，本轮结束。
		if drainInterruptMessages(cfg.InterruptCh, &messages) > 0 && loop.atHardLimit(step) {
			break
		}
		if !loop.shouldContinue(step) {
			break
		}
		if step > 0 {
			messages = applyPrepareStep(cfg, messages)
		}
		messages = PatchToolCalls(messages)

		params := cfg.Params
		params.Messages = messages
		// Refresh the model-facing tool view so tools loaded via tool_search
		// (or auto-loaded on reference) become visible this step.
		if deferActive {
			cfg.ToolDeferral.SetStep(step)
			params.Tools = cfg.ToolDeferral.View()
		}
		// Re-apply cache breakpoints for the current message set (the
		// last messages may have changed since the initial placement).
		applyProviderCachePolicy(&params, prov.Name())
		// Per-step tool_choice override (anti-hallucination gate, etc.).
		if cfg.ToolChoiceForStep != nil {
			params.ToolChoice = cfg.ToolChoiceForStep(step, toolsExecuted)
		}

		result, err := prov.DoGenerate(ctx, params)
		if err != nil {
			return nil, err
		}
		lastResult = result
		totalUsage.Add(&result.Usage)

		// Lazy tool loading: if the model referenced a deferred tool whose
		// schema isn't loaded yet, load it and re-prompt instead of executing
		// with guessed arguments.
		if deferActive {
			if names := loadTriggeredDeferredTools(result.ToolCalls, toolMap, cfg.ToolDeferral); len(names) > 0 {
				if logger := traceid.L(ctx); logger != nil {
					logger.Debugw("defer_loading: auto-load on reference", "tools", names)
				}
				for _, n := range names {
					cfg.ToolDeferral.Load(n)
				}
				// Execute any sibling tool calls that are already ready
				// (non-deferred or already-loaded deferred tools) so we don't
				// waste a round-trip; only the newly-referenced deferred tools
				// need a re-prompt to be called with proper arguments.
				exclude := make(map[string]bool, len(names))
				for _, n := range names {
					exclude[n] = true
				}
				ready := filterToolCalls(result.ToolCalls, exclude)
				if len(ready) > 0 {
					readyResults, rerr := executeTools(ctx, ready, toolMap, cfg.ApprovalHandler, nil)
					if rerr != nil {
						var deferred *ToolApprovalDeferredError
						if errors.As(rerr, &deferred) {
							stepMsgs := buildStepMessages(result.Text, result.Reasoning, result.ReasoningProviderMetadata, ready, nil, &result.Usage)
							sr := StepResult{
								Text:                 result.Text,
								Reasoning:            result.Reasoning,
								FinishReason:         result.FinishReason,
								RawFinishReason:      result.RawFinishReason,
								Usage:                result.Usage,
								ToolCalls:            ready,
								Response:             result.Response,
								DeferredToolApproval: &deferred.Approval,
								Messages:             stepMsgs,
							}
							allSteps = append(allSteps, sr)
							allMessages = append(allMessages, stepMsgs...)
							applyOnStep(cfg, &sr)
							result.DeferredToolApproval = &deferred.Approval
							lastResult = result
							break
						}
						return nil, rerr
					}
					toolsExecuted = true
					loop.recordStep(step, toolCallSignature(ready))
					if cfg.OnToolResults != nil {
						readyResults = cfg.OnToolResults(step, readyResults)
					}
					stepMsgs := buildStepMessages(result.Text, result.Reasoning, result.ReasoningProviderMetadata, ready, readyResults, &result.Usage)
					sr := StepResult{
						Text:            result.Text,
						Reasoning:       result.Reasoning,
						FinishReason:    result.FinishReason,
						RawFinishReason: result.RawFinishReason,
						Usage:           result.Usage,
						ToolCalls:       ready,
						ToolResults:     toolCallResultsFromParts(readyResults),
						Response:        result.Response,
						Messages:        stepMsgs,
					}
					allSteps = append(allSteps, sr)
					allMessages = append(allMessages, stepMsgs...)
					applyOnStep(cfg, &sr)
					messages = append(messages, stepMsgs...)
				}
				messages = append(messages, UserMessage(loadNote(names)))
				continue
			}
		}

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
		toolsExecuted = true

		// Keep loaded deferred tools that were just executed "fresh" so they
		// are not idle-evicted while still relevant.
		if deferActive {
			for _, tc := range result.ToolCalls {
				if t, ok := toolMap[tc.ToolName]; ok && t != nil && t.DeferredLoad {
					cfg.ToolDeferral.Touch(tc.ToolName)
				}
			}
		}

		// Update the dynamic loop controller with this step's tool-call
		// signature so it can extend past the soft budget while progressing
		// and stop early if the model gets stuck repeating the same calls.
		loop.recordStep(step, toolCallSignature(result.ToolCalls))

		// Apply post-execution result processing (e.g., truncation).
		if cfg.OnToolResults != nil {
			toolResults = cfg.OnToolResults(step, toolResults)
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

	logLoopStop(ctx, loop, len(allSteps))

	if lastResult != nil {
		// 暴露步数守卫停止信号，供上游（llmroute）向用户给出明确提示。
		lastResult.LoopStoppedByGuard = loop.stoppedByGuard(len(allSteps))
		lastResult.LoopStopReason = loop.describeLoopStop(len(allSteps))
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

	// Apply prompt caching policy based on the provider's capabilities.
	applyProviderCachePolicy(&cfg.Params, prov.Name())

	// Build the execution tool list (full schemas), keeping a stable copy
	// before any per-view mutation. If tool deferral is active, split it into
	// a model-facing view that hides deferred tools' Parameters and injects a
	// tool_search tool; the execution map always keeps the full definitions.
	fullTools := make([]Tool, len(cfg.Params.Tools))
	copy(fullTools, cfg.Params.Tools)
	fullTools = stripSandboxPrefixes(fullTools)

	deferActive := false
	if cfg.ToolDeferral != nil {
		cfg.ToolDeferral.SetTools(fullTools)
		if cfg.ToolDeferral.HasDeferred() {
			deferActive = true
			cfg.Params.Tools = cfg.ToolDeferral.View()
			fullTools = append(fullTools, cfg.ToolDeferral.ExecTool())
		} else {
			cfg.Params.Tools = fullTools
		}
	} else {
		cfg.Params.Tools = fullTools
	}

	// Single-step fast path
	if cfg.MaxSteps == 0 {
		// No orchestration loop, so tool_search cannot be used and deferred
		// tools would expose no schema. Bypass deferral and give the model the
		// full tool list so behavior is well-defined.
		if deferActive {
			cfg.Params.Tools = fullTools
		}
		cfg.Params.Messages = PatchToolCalls(cfg.Params.Messages)
		return prov.DoStream(ctx, cfg.Params)
	}

	toolMap := buildToolMap(fullTools)
	messages := make([]Message, len(cfg.Params.Messages))
	copy(messages, cfg.Params.Messages)
	messages = PatchToolCalls(messages)

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
		toolsExecuted := false

		loop := newLoopController(cfg.MaxSteps, cfg.HardMaxSteps)
		for step := 0; loop.shouldContinue(step); step++ {
			if step > 0 {
				messages = applyPrepareStep(cfg, messages)
			}
			messages = PatchToolCalls(messages)

			params := cfg.Params
			params.Messages = messages
			// Refresh the model-facing tool view so tools loaded via tool_search
			// (or auto-loaded on reference) become visible this step.
			if deferActive {
				cfg.ToolDeferral.SetStep(step)
				params.Tools = cfg.ToolDeferral.View()
			}
			// Re-apply cache breakpoints for the current message set.
			applyProviderCachePolicy(&params, prov.Name())
			// Per-step tool_choice override (anti-hallucination gate, etc.).
			if cfg.ToolChoiceForStep != nil {
				params.ToolChoice = cfg.ToolChoiceForStep(step, toolsExecuted)
			}

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

			// Lazy tool loading: if the model referenced a deferred tool whose
			// schema isn't loaded yet, load it and re-prompt instead of executing
			// with guessed arguments.
			if deferActive {
				if names := loadTriggeredDeferredTools(stepToolCalls, toolMap, cfg.ToolDeferral); len(names) > 0 {
					if logger := traceid.L(ctx); logger != nil {
						logger.Debugw("defer_loading: auto-load on reference", "tools", names)
					}
					for _, n := range names {
						cfg.ToolDeferral.Load(n)
					}
					// Execute any sibling tool calls that are already ready
					// (non-deferred or already-loaded deferred tools) so we don't
					// waste a round-trip; only the newly-referenced deferred tools
					// need a re-prompt to be called with proper arguments.
					exclude := make(map[string]bool, len(names))
					for _, n := range names {
						exclude[n] = true
					}
					ready := filterToolCalls(stepToolCalls, exclude)
					if len(ready) > 0 {
						sendProgress := func(part StreamPart) { send(part) }
						readyResults, rerr := executeTools(ctx, ready, toolMap, cfg.ApprovalHandler, sendProgress)
						if rerr != nil {
							var deferred *ToolApprovalDeferredError
							if errors.As(rerr, &deferred) {
								stepMsgs := buildStepMessages(stepText, stepReasoning, stepReasoningMeta, ready, nil, &stepUsage)
								stepR := StepResult{
									Text:                 stepText,
									Reasoning:            stepReasoning,
									FinishReason:         lastFinishReason,
									RawFinishReason:      lastRawFinishReason,
									Usage:                stepUsage,
									ToolCalls:            ready,
									Response:             stepResponse,
									DeferredToolApproval: &deferred.Approval,
									Messages:             stepMsgs,
								}
								allSteps = append(allSteps, stepR)
								allMessages = append(allMessages, stepMsgs...)
								applyOnStep(cfg, &stepR)
								break
							}
							send(&ErrorPart{Error: rerr})
							break
						}
						toolsExecuted = true
						loop.recordStep(step, toolCallSignature(ready))
						if cfg.OnToolResults != nil {
							readyResults = cfg.OnToolResults(step, readyResults)
						}
						stepMsgs := buildStepMessages(stepText, stepReasoning, stepReasoningMeta, ready, readyResults, &stepUsage)
						stepR := StepResult{
							Text:            stepText,
							Reasoning:       stepReasoning,
							FinishReason:    lastFinishReason,
							RawFinishReason: lastRawFinishReason,
							Usage:           stepUsage,
							ToolCalls:       ready,
							ToolResults:     toolCallResultsFromParts(readyResults),
							Response:        stepResponse,
							Messages:        stepMsgs,
						}
						allSteps = append(allSteps, stepR)
						allMessages = append(allMessages, stepMsgs...)
						applyOnStep(cfg, &stepR)
						messages = append(messages, stepMsgs...)
					}
					messages = append(messages, UserMessage(loadNote(names)))
					continue
				}
			}

			// If context was cancelled during streaming, stop immediately.
			if ctx.Err() != nil {
				break
			}

			// No tool calls or not a tool-calls finish → done
			if lastFinishReason != FinishReasonToolCalls || len(stepToolCalls) == 0 || !hasExecutableTools(stepToolCalls, toolMap) {
				// 收尾前再给一次「中途追加」机会：若用户在本步流式输出期间补充了
				// 内容，则作为用户消息加入上下文并继续循环，让模型结合新内容重新
				// 生成（无需先终止当前生成再补充）。
				if drainInterruptMessages(cfg.InterruptCh, &messages) > 0 {
					continue
				}
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
			toolsExecuted = true

			// Keep loaded deferred tools that were just executed "fresh" so they
			// are not idle-evicted while still relevant.
			if deferActive {
				for _, tc := range stepToolCalls {
					if t, ok := toolMap[tc.ToolName]; ok && t != nil && t.DeferredLoad {
						cfg.ToolDeferral.Touch(tc.ToolName)
					}
				}
			}

			// Update the dynamic loop controller with this step's tool-call
			// signature (same rationale as the non-streaming path).
			loop.recordStep(step, toolCallSignature(stepToolCalls))

			// Apply post-execution result processing (e.g., truncation).
			if cfg.OnToolResults != nil {
				toolResults = cfg.OnToolResults(step, toolResults)
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

		logLoopStop(ctx, loop, len(allSteps))

		// 暴露步数守卫停止信号，供上游（llmroute）向用户给出明确提示。
		sr.LoopStoppedByGuard = loop.stoppedByGuard(len(allSteps))
		sr.LoopStopReason = loop.describeLoopStop(len(allSteps))

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

// drainInterruptMessages 非阻塞地取出用户在生成过程中通过 InterruptCh 中途
// 追加的内容，作为用户消息追加到 messages，返回本次取出的条数。
//
// 用于 Claude-CLI 风格的「边思考/边输出边补充」：无需终止当前生成即可把新
// 上下文注入同一轮对话。调用方应保证通道带缓冲，因此本函数永不阻塞。
func drainInterruptMessages(ch chan string, messages *[]Message) int {
	if ch == nil {
		return 0
	}
	n := 0
	for {
		select {
		case extra := <-ch:
			if strings.TrimSpace(extra) != "" {
				*messages = append(*messages, UserMessage(extra))
				n++
			}
		default:
			return n
		}
	}
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

	// 可观测：把每次工具调用的「名称=结果片段」列出，失败也会以输出字符串
	// 形式落在 Output 里（如搜索 500 的错误文本），便于事后定位工具问题。
	details := make([]string, 0, totalCalls)
	for _, step := range steps {
		for _, tc := range step.ToolCalls {
			entry := tc.ToolName
			for _, tr := range step.ToolResults {
				if tr.ToolCallID != tc.ToolCallID {
					continue
				}
				s := fmt.Sprintf("%v", tr.Output)
				if len(s) > 200 {
					s = s[:200] + "...(truncated)"
				}
				entry += "=" + s
				break
			}
			details = append(details, entry)
		}
	}

	logger := traceid.L(ctx)
	if logger == nil {
		return
	}

	logger.Infow("tool call summary",
		"total_calls", totalCalls,
		"unique_tools", uniqueTools,
		"steps", len(steps),
		"details", details,
	)
}

func buildToolMap(tools []Tool) map[string]*Tool {
	m := make(map[string]*Tool, len(tools))
	for i := range tools {
		m[tools[i].Name] = &tools[i]
	}
	return m
}

// loadTriggeredDeferredTools returns the names of tool calls that reference a
// deferred tool whose schema is not yet loaded. Such calls should trigger a
// load + re-prompt rather than execution with guessed arguments.
func loadTriggeredDeferredTools(calls []ToolCall, toolMap map[string]*Tool, d *ToolDeferral) []string {
	var names []string
	for _, tc := range calls {
		t, ok := toolMap[tc.ToolName]
		if !ok || t == nil {
			continue
		}
		if t.DeferredLoad && !d.IsLoaded(tc.ToolName) {
			names = append(names, tc.ToolName)
		}
	}
	return names
}

// filterToolCalls returns the subset of calls whose tool name is NOT in the
// exclude set. Used to execute the ready sibling tool calls while deferring
// newly-referenced (not-yet-loaded) deferred tools to a re-prompt.
func filterToolCalls(calls []ToolCall, exclude map[string]bool) []ToolCall {
	out := make([]ToolCall, 0, len(calls))
	for _, tc := range calls {
		if !exclude[tc.ToolName] {
			out = append(out, tc)
		}
	}
	return out
}

// SandboxToolPrefix is the prefix used for sandbox-scoped tools. It is stripped
// before tools are presented to the LLM so the model sees generic names
// (exec, read_file, …) without knowing about the sandbox implementation.
const SandboxToolPrefix = "sandbox_"

// stripSandboxPrefixes returns a copy of tools with the SandboxToolPrefix
// removed from tool names. Tools whose stripped name would collide with an
// existing non-sandbox tool keep their original name.
func stripSandboxPrefixes(tools []Tool) []Tool {
	existing := make(map[string]bool)
	for _, t := range tools {
		if !strings.HasPrefix(t.Name, SandboxToolPrefix) {
			existing[t.Name] = true
		}
	}

	out := make([]Tool, len(tools))
	for i, t := range tools {
		out[i] = t // struct copy
		if stripped, ok := strings.CutPrefix(t.Name, SandboxToolPrefix); ok {
			if stripped != "" && !existing[stripped] {
				out[i].Name = stripped
			}
		}
	}
	return out
}

func hasExecutableTools(toolCalls []ToolCall, toolMap map[string]*Tool) bool {
	for _, tc := range toolCalls {
		if t, ok := toolMap[tc.ToolName]; ok && t.Execute != nil {
			return true
		}
	}
	return false
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
	// invocationID：本次「实际执行」的服务端唯一标识。它与模型下发的
	// ToolCallID 相互独立，用于在日志与前端稳定地区分「来自哪次调用」。
	invocationID := newInvocationID()

	var progressFn func(content any)
	if sendProgress != nil {
		progressFn = func(content any) {
			sendProgress(&ToolProgressPart{
				ToolCallID:   tc.ToolCallID,
				ToolName:     tc.ToolName,
				InvocationID: invocationID,
				Content:      content,
			})
		}
	}

	execCtx := &ToolExecContext{
		Context:      ctx,
		ToolCallID:   tc.ToolCallID,
		ToolName:     tc.ToolName,
		InvocationID: invocationID,
		SendProgress: progressFn,
	}

	output, err := tool.Execute(execCtx, tc.Input)
	if err != nil {
		if sendProgress != nil {
			sendProgress(&StreamToolErrorPart{
				ToolCallID:   tc.ToolCallID,
				ToolName:     tc.ToolName,
				InvocationID: invocationID,
				Error:        err,
			})
		}
		return ToolResultPart{
			ToolCallID:   tc.ToolCallID,
			ToolName:     tc.ToolName,
			InvocationID: invocationID,
			Result:       err.Error(),
			IsError:      true,
		}
	}

	// Apply output truncation to prevent context bloat.
	truncResult := TruncateOutput(output, DefaultTruncationConfig())
	finalOutput := truncResult.Output

	if sendProgress != nil {
		sendProgress(&StreamToolResultPart{
			ToolCallID:   tc.ToolCallID,
			ToolName:     tc.ToolName,
			InvocationID: invocationID,
			Input:        tc.Input,
			Output:       output,
		})
	}
	return ToolResultPart{
		ToolCallID:   tc.ToolCallID,
		ToolName:     tc.ToolName,
		InvocationID: invocationID,
		Result:       finalOutput,
	}
}
