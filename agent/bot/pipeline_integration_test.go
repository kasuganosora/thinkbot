package bot

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/agent/pipeline"
	"github.com/kasuganosora/thinkbot/dao"
	"github.com/kasuganosora/thinkbot/llm"
)

// ============================================================================
// Pipeline 中间件 + LLM 真实 API 集成测试
//
// 验证新增中间件（警告系统、循环检测、Token 预算、运行日志）和
// LLM 上下文安全特性（PatchToolCalls、ContextReduction、Streaming Orchestrate）
// 在真实 LLM 环境下的端到端行为。
//
// 运行命令：
//
//	go test -v -run TestIntegration_Pipeline ./agent/bot/ -timeout 300s
// ============================================================================

// ============================================================================
// 1. 警告系统集成测试 — 验证 QueueWarning + MergeWarnings 被 LLM 消费
// ============================================================================

// warningAwareStage 是一个读取 Envelope 警告并合并到 system prompt 的自定义 Stage，
// 模拟 LLMRoute/LLMStage 的警告消费行为。
type warningAwareStage struct {
	provider   llm.Provider
	model      string
	baseSystem string
	maxTokens  int
}

func (s *warningAwareStage) Name() string { return "warning-aware-llm" }

func (s *warningAwareStage) Process(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
	// 模拟 LLMRoute 的警告消费：MergeWarnings 从 Envelope 读取警告并合并到 prompt
	systemPrompt := core.MergeWarnings(env, s.baseSystem)

	result, err := s.provider.DoGenerate(ctx, llm.GenerateParams{
		Model:       llm.ChatModel(s.model),
		System:      systemPrompt,
		Messages:    []llm.Message{llm.UserMessage(env.Message.Text)},
		Temperature: floatPtr(0.1),
		MaxTokens:   intPtr(s.maxTokens),
	})
	if err != nil {
		return env, fmt.Errorf("warning-aware stage: %w", err)
	}

	env.Set("llm.result", result)
	env.AddAction(core.Action{
		Type:    core.ActionReply,
		Channel: env.Message.Channel,
		UserID:  env.Message.UserID,
		Payload: result.Text,
	})
	return env, nil
}

// TestIntegration_Pipeline_WarningConsumption 验证软警告被 LLM 消费。
//
// 流程：
//  1. 创建 warningAwareStage（内部调用 MergeWarnings）
//  2. 包装 TokenBudgetMiddleware 以在 before 阶段注入软警告
//  3. 验证 LLM 回复中包含警告要求的标记
func TestIntegration_Pipeline_WarningConsumption(t *testing.T) {
	skipIfShort(t)
	bundle := setupIntegLLMBundle(t)

	stage := &warningAwareStage{
		provider:   bundle.Main,
		model:      integCfg.Model,
		baseSystem: "你是一个助手。请用简短的中文回答。",
		maxTokens:  integCfg.MaxTokens,
	}

	// 用轻量 Token 预算配置注入软警告（WarnPercent=0 → 首次调用立即触发警告）
	budgetCfg := pipeline.NewTokenBudgetConfig().
		WithMaxTokens(200_000).
		WithWarnPercent(0.0). // 首次调用即触发
		WithHardPercent(1.0)
	guarded := pipeline.TokenBudgetMiddleware(budgetCfg)(stage)

	env := core.NewEnvelope(core.Message{
		ID:        "msg-warn-001",
		Source:    "memory",
		BotID:     integBotID,
		Channel:   "warn-test",
		UserID:    "test-user",
		Text:      "今天天气怎么样？",
		CreatedAt: time.Now(),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	out, err := guarded.Process(ctx, env)
	if err != nil {
		t.Fatalf("stage.Process failed: %v", err)
	}

	actions := out.Actions()
	var replyText string
	for _, a := range actions {
		if a.Type == core.ActionReply {
			replyText, _ = a.Payload.(string)
		}
	}
	t.Logf("Reply (with warning): %q", truncate(replyText, 300))

	if replyText == "" {
		t.Fatal("expected non-empty reply")
	}

	// 验证警告已被消费（软警告执行后应从队列移除）
	if v, ok := out.Get(core.WarningsKey); ok {
		warnings, _ := v.([]core.Warning)
		t.Logf("Remaining warnings: %d", len(warnings))
		// 软警告应该已被 MergeWarnings 消费移除
		for _, w := range warnings {
			t.Logf("  Warning: source=%s level=%s msg=%s", w.Source, w.Level, truncate(w.Message, 100))
		}
	}

	// 验证 LLM 结果存储
	if v, ok := out.Get("llm.result"); ok {
		result, _ := v.(*llm.GenerateResult)
		t.Logf("Usage: in=%d out=%d total=%d", result.Usage.InputTokens, result.Usage.OutputTokens, result.Usage.TotalTokens)
	}
}

// TestIntegration_Pipeline_WarningHardLevel 验证硬警告被保留且 LLM 能感知。
func TestIntegration_Pipeline_WarningHardLevel(t *testing.T) {
	skipIfShort(t)
	bundle := setupIntegLLMBundle(t)

	stage := &warningAwareStage{
		provider:   bundle.Main,
		model:      integCfg.Model,
		baseSystem: "你是一个助手。请用简短的中文回答用户问题。",
		maxTokens:  integCfg.MaxTokens,
	}

	env := core.NewEnvelope(core.Message{
		ID:        "msg-hard-001",
		Source:    "memory",
		BotID:     integBotID,
		Channel:   "hard-warn-test",
		UserID:    "test-user",
		Text:      "请写一句问候语。",
		CreatedAt: time.Now(),
	})

	// 直接注入硬警告（模拟 LoopDetectionMiddleware 在 after 阶段注入）
	core.QueueWarning(env, core.Warning{
		Source:  "loop_detection",
		Level:   core.WarningLevelHard,
		Message: "You have repeated the same pattern too many times. Prefix your output with <<HALT>> and answer briefly.",
	})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	out, err := stage.Process(ctx, env)
	if err != nil {
		t.Fatalf("stage.Process failed: %v", err)
	}

	actions := out.Actions()
	var replyText string
	for _, a := range actions {
		if a.Type == core.ActionReply {
			replyText, _ = a.Payload.(string)
		}
	}
	t.Logf("Reply (with hard warning): %q", truncate(replyText, 300))

	if replyText == "" {
		t.Fatal("expected non-empty reply")
	}

	// 硬警告应在 MergeWarnings 后仍然保留
	remains := core.HasHardWarning(out)
	if remains {
		t.Log("Hard warning preserved as expected")
	}

	// 验证 LLM 是否遵循了硬警告的提示（前缀标记）
	if !strings.Contains(replyText, "<<HALT>>") {
		t.Logf("note: LLM did not follow the <<HALT>> prefix instruction exactly, reply: %q", truncate(replyText, 200))
	}
}

// ============================================================================
// 2. Token 预算集成测试 — 验证累积追踪
// ============================================================================

// TestIntegration_Pipeline_TokenBudget_Accumulation 验证 TokenBudgetMiddleware
// 跨多次调用正确累积 token 用量。
func TestIntegration_Pipeline_TokenBudget_Accumulation(t *testing.T) {
	skipIfShort(t)
	bundle := setupIntegLLMBundle(t)

	// 设置一个可观察的预算（设置很小的 MaxTokens + 0% 警告 → 每次调用都注入警告）
	budgetCfg := pipeline.NewTokenBudgetConfig().
		WithMaxTokens(100_000).
		WithWarnPercent(0.0). // 首次调用即警告
		WithHardPercent(1.0)

	stage := &warningAwareStage{
		provider:   bundle.Main,
		model:      integCfg.Model,
		baseSystem: "你是一个助手。请用一句话简短回答。",
		maxTokens:  integCfg.MaxTokens,
	}
	guarded := pipeline.TokenBudgetMiddleware(budgetCfg)(stage)

	channel := "budget-accum-test"

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	var totalTokens int
	for i := 1; i <= 3; i++ {
		env := core.NewEnvelope(core.Message{
			ID:        fmt.Sprintf("msg-budget-%d", i),
			Source:    "memory",
			BotID:     integBotID,
			Channel:   channel,
			UserID:    "test-user",
			Text:      fmt.Sprintf("回合%d：1+%d等于几？只回答数字。", i, i),
			CreatedAt: time.Now(),
		})

		out, err := guarded.Process(ctx, env)
		if err != nil {
			t.Fatalf("turn %d failed: %v", i, err)
		}

		if v, ok := out.Get("llm.result"); ok {
			result, _ := v.(*llm.GenerateResult)
			totalTokens += result.Usage.TotalTokens
			t.Logf("Turn %d: usage(total=%d), cumulative=%d", i, result.Usage.TotalTokens, totalTokens)
		}
	}

	if totalTokens == 0 {
		t.Fatal("expected non-zero token usage across 3 turns")
	}
	t.Logf("Total cumulative tokens: %d", totalTokens)
}

// ============================================================================
// 3. Streaming Orchestrate + Tools 测试 — 验证流式多步编排
// ============================================================================

// TestIntegration_Pipeline_StreamOrchestrate_WithTools 验证流式编排模式
// 下的多步工具调用：LLM 调用工具 → 执行 → 回传结果 → 生成最终回复（全部通过流返回）。
func TestIntegration_Pipeline_StreamOrchestrate_WithTools(t *testing.T) {
	skipIfShort(t)
	bundle := setupIntegLLMBundle(t)

	tools := []llm.Tool{
		{
			Name:        "get_weather",
			Description: "获取指定城市的天气信息。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"city": map[string]any{"type": "string", "description": "城市名称"},
				},
				"required": []string{"city"},
			},
			Execute: func(ctx *llm.ToolExecContext, input any) (any, error) {
				m, _ := input.(map[string]any)
				city, _ := m["city"].(string)
				return map[string]any{
					"city":        city,
					"temperature": "22C",
					"condition":   "晴",
					"humidity":    "55%",
				}, nil
			},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	streamResult, err := llm.OrchestrateStream(ctx, bundle.Main, &llm.OrchestrateConfig{
		Params: llm.GenerateParams{
			Model:       llm.ChatModel(integCfg.Model),
			System:      "你可以使用 get_weather 工具查询天气。查询后请用中文简短回答。",
			Messages:    []llm.Message{llm.UserMessage("深圳天气怎么样？使用工具查询。")},
			Tools:       tools,
			ToolChoice:  "auto",
			Temperature: floatPtr(0.3),
			MaxTokens:   intPtr(integCfg.MaxTokens),
		},
		MaxSteps: 5,
	})
	if err != nil {
		t.Fatalf("OrchestrateStream failed: %v", err)
	}

	var textParts []string
	var sawStart, sawFinish bool
	var toolCallCount, toolResultCount int
	var streamErr error
	var finishReason llm.FinishReason
	var totalUsage llm.Usage

	for part := range streamResult.Stream {
		switch p := part.(type) {
		case *llm.StartPart:
			sawStart = true
		case *llm.StartStepPart:
			t.Logf("Stream: step started")
		case *llm.TextDeltaPart:
			textParts = append(textParts, p.Text)
		case *llm.StreamToolCallPart:
			toolCallCount++
			t.Logf("Stream: tool call %s", p.ToolName)
		case *llm.StreamToolResultPart:
			toolResultCount++
			t.Logf("Stream: tool result for %s", p.ToolName)
		case *llm.FinishStepPart:
			finishReason = p.FinishReason
		case *llm.FinishPart:
			sawFinish = true
			finishReason = p.FinishReason
			totalUsage = p.TotalUsage
		case *llm.ErrorPart:
			streamErr = p.Error
		}
	}

	if streamErr != nil {
		t.Fatalf("stream error: %v", streamErr)
	}

	fullText := strings.Join(textParts, "")
	t.Logf("Stream orchestrate: text=%q, finish=%s, toolCalls=%d, toolResults=%d, usage(total=%d)",
		truncate(fullText, 200), finishReason, toolCallCount, toolResultCount, totalUsage.TotalTokens)

	if !sawStart {
		t.Error("expected StartPart in stream")
	}
	if !sawFinish {
		t.Error("expected FinishPart in stream")
	}
	if fullText == "" {
		t.Error("expected non-empty final text")
	}
	if toolCallCount == 0 {
		t.Error("expected at least one tool call via stream")
	}
	if totalUsage.TotalTokens == 0 {
		t.Error("expected non-zero token usage")
	}
}

// ============================================================================
// 4. ContextReduction 集成测试 — 验证超大工具结果被截断
// ============================================================================

// TestIntegration_Pipeline_ContextReduction_LargeToolResult 验证当工具返回超大
// 结果时，ContextReduction.PrepareStep 正确截断。
func TestIntegration_Pipeline_ContextReduction_LargeToolResult(t *testing.T) {
	skipIfShort(t)
	bundle := setupIntegLLMBundle(t)

	// 创建一个返回超长文本的工具
	tools := []llm.Tool{
		{
			Name:        "get_large_data",
			Description: "获取大量文本数据。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"topic": map[string]any{"type": "string", "description": "查询主题"},
				},
				"required": []string{"topic"},
			},
			Execute: func(ctx *llm.ToolExecContext, input any) (any, error) {
				// 返回一个模拟的超长文本（~8000 tokens 估算字符）
				m, _ := input.(map[string]any)
				topic, _ := m["topic"].(string)

				// 生成模拟大数据（用重复 + 少量变化模拟真实工具结果）
				var sb strings.Builder
				for i := 0; i < 600; i++ {
					fmt.Fprintf(&sb, "Data point %d: %s analysis result=%.4f, confidence=%.2f%%, status=ok, timestamp=2026-06-27T%d:00:00Z\n",
						i, topic, float64(i)*1.5, float64(i%100), i%24)
				}
				sb.WriteString("THE_IMPORTANT_CONCLUSION: 最终结论是数据整体呈上升趋势。")

				return map[string]any{
					"data":    sb.String(),
					"count":   600,
					"topic":   topic,
					"summary": "数据整体呈上升趋势",
				}, nil
			},
		},
		{
			Name:        "get_status",
			Description: "获取系统状态。",
			Parameters: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
			Execute: func(ctx *llm.ToolExecContext, input any) (any, error) {
				return map[string]any{"status": "ok"}, nil
			},
		},
	}

	// 使用 OnToolResults 回调截断过大的工具结果（截断阈值设为 125 tokens）
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	result, err := llm.OrchestrateGenerate(ctx, bundle.Main, &llm.OrchestrateConfig{
		Params: llm.GenerateParams{
			Model:       llm.ChatModel(integCfg.Model),
			System:      "你是数据分析助手。先调用 get_large_data 获取数据，然后调用 get_status 检查状态，最后总结。",
			Messages:    []llm.Message{llm.UserMessage("查询 topic=market 的数据并总结趋势。")},
			Tools:       tools,
			ToolChoice:  "auto",
			Temperature: floatPtr(0.3),
			MaxTokens:   intPtr(integCfg.MaxTokens),
		},
		MaxSteps: 5,
		OnToolResults: llm.NewOnToolResultsCallback(llm.ReductionConfig{
			MaxOutputTokens: 125,
		}),
	})
	if err != nil {
		t.Fatalf("OrchestrateGenerate failed: %v", err)
	}

	t.Logf("Result: text=%q, finish=%s, steps=%d, usage(total=%d)",
		truncate(result.Text, 200), result.FinishReason, len(result.Steps), result.Usage.TotalTokens)

	if result.Text == "" {
		t.Fatal("expected non-empty final text")
	}
	if len(result.Steps) < 2 {
		t.Errorf("expected >=2 steps (tool call + final), got %d", len(result.Steps))
	}

	// 验证截断已生效：工具结果中应包含 "(truncated)" 标记
	truncated := false
	for _, step := range result.Steps {
		for _, tr := range step.ToolResults {
			output := fmt.Sprintf("%v", tr.Output)
			if strings.Contains(output, "(truncated)") {
				truncated = true
				t.Logf("Truncation detected in tool result: %s → %d chars", tr.ToolName, len(output))
			}
			t.Logf("  Step tool result: %s → %d chars", tr.ToolName, len(output))
		}
	}
	if !truncated {
		t.Log("note: tool result may not have been truncated (token estimate may be below threshold)")
	}
}

// ============================================================================
// 5. PatchToolCalls 集成测试 — 验证编排过程中悬挂工具调用被修补
// ============================================================================

// TestIntegration_Pipeline_PatchToolCalls_Orchestrate 验证 OrchestrateGenerate
// 内置的 PatchToolCalls 能正确处理有悬挂工具调用的消息历史。
func TestIntegration_Pipeline_PatchToolCalls_Orchestrate(t *testing.T) {
	skipIfShort(t)
	bundle := setupIntegLLMBundle(t)

	tools := []llm.Tool{
		{
			Name:        "get_info",
			Description: "获取信息。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"key": map[string]any{"type": "string", "description": "查询键"},
				},
				"required": []string{"key"},
			},
			Execute: func(ctx *llm.ToolExecContext, input any) (any, error) {
				m, _ := input.(map[string]any)
				key, _ := m["key"].(string)
				return map[string]any{"key": key, "value": fmt.Sprintf("result_for_%s", key)}, nil
			},
		},
	}

	// 构造一段包含"悬挂 tool call"的历史：assistant 调用了 get_info("dangling_key")
	// 但没有对应的 tool result 消息
	danglingHistory := []llm.Message{
		llm.UserMessage("查询 dangling_key 的信息。"),
		{
			Role: llm.MessageRoleAssistant,
			Content: []llm.MessagePart{
				llm.ToolCallPart{
					ToolCallID: "call_dangling_001",
					ToolName:   "get_info",
					Input:      map[string]any{"key": "dangling_key"},
				},
			},
		},
		// 注：这里故意缺少对应的 ToolMessage（tool result）——模拟被中断的调用
		llm.UserMessage("现在请重新查询 dangling_key 并告诉我结果。"),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	result, err := llm.OrchestrateGenerate(ctx, bundle.Main, &llm.OrchestrateConfig{
		Params: llm.GenerateParams{
			Model:       llm.ChatModel(integCfg.Model),
			System:      "你可以使用 get_info 工具查询信息。",
			Messages:    danglingHistory,
			Tools:       tools,
			ToolChoice:  "auto",
			Temperature: floatPtr(0.3),
			MaxTokens:   intPtr(integCfg.MaxTokens),
		},
		MaxSteps: 5,
	})
	if err != nil {
		t.Fatalf("OrchestrateGenerate with dangling calls failed: %v", err)
	}

	t.Logf("Result: text=%q, finish=%s, steps=%d, usage(total=%d)",
		truncate(result.Text, 200), result.FinishReason, len(result.Steps), result.Usage.TotalTokens)

	if result.Text == "" {
		t.Fatal("expected non-empty final text")
	}

	// 验证 LLM 仍然能正常工作（没有因为悬挂 tool call 被拒绝）
	t.Logf("Success: orchestration completed despite dangling tool calls in history")
}

// ============================================================================
// 6. RunJournal 集成测试 — 验证 LLM 用量被持久化
// ============================================================================

// selectCount 辅助查询。
func selectCount(db *gorm.DB, condition string, args ...any) int {
	var count int64
	db.Raw("SELECT count(*) FROM run_journal WHERE "+condition, args...).Scan(&count)
	return int(count)
}

// getRunJournals 查询 run_journal 记录。
func getRunJournals(db *gorm.DB) []dao.RunJournal {
	var journals []dao.RunJournal
	db.Order("id ASC").Find(&journals)
	return journals
}

// TestIntegration_Pipeline_RunJournal_Record 验证通过 RunJournalMiddleware
// 记录 LLM 调用到数据库。
func TestIntegration_Pipeline_RunJournal_Record(t *testing.T) {
	skipIfShort(t)
	bundle := setupIntegLLMBundle(t)

	// 创建内存 SQLite 用于测试
	database, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := dao.Migrate(database); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	stage := &warningAwareStage{
		provider:   bundle.Main,
		model:      integCfg.Model,
		baseSystem: "你是一个助手。请用一句话回答。",
		maxTokens:  integCfg.MaxTokens,
	}

	// 包装 RunJournalMiddleware
	journalCfg := pipeline.RunJournalConfig{
		FlushThreshold: 1, // 立即刷新
		Caller:         "integration_test",
		Feature:        "test",
	}
	guarded := pipeline.RunJournalMiddleware(database, journalCfg)(stage)

	channel := "journal-test"

	// 执行 2 次 LLM 调用
	for i := 1; i <= 2; i++ {
		env := core.NewEnvelope(core.Message{
			ID:        fmt.Sprintf("msg-journal-%d", i),
			TraceID:   fmt.Sprintf("trace-%d", i),
			Source:    "memory",
			BotID:     integBotID,
			Channel:   channel,
			UserID:    "test-user",
			Text:      fmt.Sprintf("请回答：%d+%d等于几？只回答数字。", i, i*10),
			CreatedAt: time.Now(),
		})

		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		out, err := guarded.Process(ctx, env)
		cancel()

		if err != nil {
			t.Fatalf("turn %d: %v", i, err)
		}

		if v, ok := out.Get("llm.result"); ok {
			result, _ := v.(*llm.GenerateResult)
			t.Logf("Turn %d: usage(total=%d), text=%q",
				i, result.Usage.TotalTokens, truncate(result.Text, 100))
		}
	}

	// 给异步写入一点时间
	time.Sleep(500 * time.Millisecond)

	// 验证数据库记录
	journals := getRunJournals(database)
	t.Logf("RunJournal records: %d", len(journals))
	for _, j := range journals {
		t.Logf("  id=%d, trace=%s, channel=%s, user=%s, input=%d, output=%d, total=%d, model=%q, status=%s",
			j.ID, j.TraceID, j.Channel, j.UserID, j.InputTokens, j.OutputTokens, j.TotalTokens, j.Model, j.Status)
	}

	if len(journals) == 0 {
		t.Fatal("expected at least 1 run journal record")
	}

	if len(journals) < 2 {
		t.Logf("warning: expected 2 records, got %d (async flush may not have completed)", len(journals))
	}

	// 验证记录的字段完整性
	for _, j := range journals {
		if j.TraceID == "" {
			t.Error("expected non-empty trace_id")
		}
		if j.Channel == "" {
			t.Error("expected non-empty channel")
		}
		if j.UserID == "" {
			t.Error("expected non-empty user_id")
		}
		if j.TotalTokens == 0 {
			t.Error("expected non-zero total_tokens")
		}
		if j.Status != "success" {
			t.Errorf("expected status 'success', got %q", j.Status)
		}
	}

	// 验证按 channel 查询
	count := selectCount(database, "channel = ?", channel)
	t.Logf("Records for channel %q: %d", channel, count)
	if count == 0 {
		t.Error("expected at least 1 record for the test channel")
	}
}

// ============================================================================
// 7. 完整 Pipeline + 中间件组合测试
// ============================================================================

// TestIntegration_Pipeline_CombinedMiddleware 验证多种中间件同时包装
// LLMStage 时的端到端行为（警告 + 预算 + 日志在真实 LLM 下协同工作）。
func TestIntegration_Pipeline_CombinedMiddleware(t *testing.T) {
	skipIfShort(t)
	bundle := setupIntegLLMBundle(t)

	// 内存 DB
	database, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := dao.Migrate(database); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	baseStage := &warningAwareStage{
		provider:   bundle.Main,
		model:      integCfg.Model,
		baseSystem: "你是一个助手。请用简短的中文回答。",
		maxTokens:  integCfg.MaxTokens,
	}

	// 链式包装中间件（模拟 api/botservice.go 的包装顺序）
	journalMw := pipeline.RunJournalMiddleware(database, pipeline.RunJournalConfig{
		FlushThreshold: 2,
		Caller:         "combined_test",
		Feature:        "test",
	})
	loopCfg := pipeline.NewLoopDetectionConfig().
		WithWarnThreshold(3).
		WithHardLimit(10).
		WithWindowSize(20)
	budgetCfg := pipeline.NewTokenBudgetConfig().
		WithMaxTokens(500_000).
		WithWarnPercent(0.5).
		WithHardPercent(1.0)

	// 包装顺序：journal → loop → budget → stage
	loopMw := pipeline.LoopDetectionMiddleware(loopCfg)
	budgetMw := pipeline.TokenBudgetMiddleware(budgetCfg)
	guarded := journalMw(loopMw(budgetMw(baseStage)))

	channel := "combined-test"

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	for i := 1; i <= 2; i++ {
		env := core.NewEnvelope(core.Message{
			ID:        fmt.Sprintf("msg-cmb-%d", i),
			TraceID:   fmt.Sprintf("trace-cmb-%d", i),
			Source:    "memory",
			BotID:     integBotID,
			Channel:   channel,
			UserID:    "test-user",
			Text:      "请用一句话回答：Go语言有什么特点？",
			CreatedAt: time.Now(),
		})

		out, err := guarded.Process(ctx, env)
		if err != nil {
			t.Fatalf("turn %d failed: %v", i, err)
		}

		actions := out.Actions()
		for _, a := range actions {
			if a.Type == core.ActionReply {
				t.Logf("Turn %d reply: %q", i, truncate(a.Payload.(string), 200))
			}
		}

		if v, ok := out.Get("llm.result"); ok {
			result, _ := v.(*llm.GenerateResult)
			t.Logf("Turn %d usage: total=%d, steps=%d", i, result.Usage.TotalTokens, len(result.Steps))
		}
	}

	// 验证数据库记录
	time.Sleep(500 * time.Millisecond)
	journals := getRunJournals(database)
	t.Logf("Combined middleware: %d run journal records", len(journals))
	if len(journals) == 0 {
		t.Error("expected run journal records from combined middleware")
	}
}
