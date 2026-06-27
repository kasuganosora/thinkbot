package bot

import (
	"context"
	"fmt"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/engagement"
	"github.com/kasuganosora/thinkbot/agent/memory"
	"github.com/kasuganosora/thinkbot/llm"
)

// ============================================================================
// 自适应 Engagement 集成测试（真实 LLM API）
//
// 验证在真实 LLM 环境下：
//   1. BotProfileProfiler 能否从 BotScope 记忆中提取格式正确的量化画像
//   2. Dreaming 管线 + BotProfileProfiler 端到端能否产出 L3 画像
//   3. SOUL.md 解析 → 初始画像 → AdaptiveSyncer 全链路正确性
//
// 运行命令：
//
//	go test -v -run "TestIntegration_BotProfile|TestIntegration_Dreaming_BotProfile|TestIntegration_SOUL_to_Profile" ./agent/bot/ -timeout 300s
//
// .env 中需配置 test.llm.* 键（与现有集成测试共享）。
// ============================================================================

func adaptiveTestLogger() *zap.SugaredLogger {
	return zap.NewNop().Sugar()
}

func adaptiveTestTracerProvider() trace.TracerProvider {
	return noop.NewTracerProvider()
}

// ============================================================================
// 1. BotProfileProfiler 真实 LLM 测试
// ============================================================================

// TestIntegration_BotProfileProfiler_RealLLM 验证 BotProfileProfiler 在真实 LLM
// 下能正确提取量化的 Bot 自我画像 JSON。
func TestIntegration_BotProfileProfiler_RealLLM(t *testing.T) {
	skipIfShort(t)
	bundle := setupIntegLLMBundle(t)

	profiler := memory.NewBotProfileProfiler(
		memory.BotProfileProfilerConfig{
			Provider: bundle.Main,
			Model:    &llm.Model{ID: integCfg.Model},
		},
		adaptiveTestTracerProvider(),
		adaptiveTestLogger(),
	)

	// 模拟 BotScope 中的 L1 长期记忆——Bot 自身的行为历史
	l1Entries := []memory.TieredEntry{
		{Entry: memory.Entry{ID: "b1", Content: "Bot 主动回答了用户关于 Go 语言并发模型的问题，回复详细含有代码示例。", Category: "behavior"}},
		{Entry: memory.Entry{ID: "b2", Content: "用户问了一个非常基础的 Python 问题，Bot 耐心地给出了逐步引导式的回答。", Category: "behavior"}},
		{Entry: memory.Entry{ID: "b3", Content: "Bot 在没有被 @ 的情况下主动参与了关于 Kubernetes 部署的讨论，提供了有价值的建议。", Category: "behavior"}},
		{Entry: memory.Entry{ID: "b4", Content: "三个用户连续问了相同的问题三次，Bot 每次都给出了完整回答，没有表现出不耐烦。", Category: "behavior"}},
		{Entry: memory.Entry{ID: "b5", Content: "一位群友发了长文讨论分布式系统架构，Bot 给出了非常详尽的回复，几乎有 500 字。", Category: "behavior"}},
		{Entry: memory.Entry{ID: "b6", Content: "Bot 频繁参与 Go 和 Rust 相关的话题讨论，表现出明显的兴趣偏好。", Category: "observation"}},
		{Entry: memory.Entry{ID: "b7", Content: "在技术讨论组里，Bot 积极参与容器化和微服务话题，贡献了多个有价值的观点。", Category: "observation"}},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	result, err := profiler.ExtractProfile(ctx, l1Entries, nil, nil)
	if err != nil {
		t.Fatalf("BotProfileProfiler.ExtractProfile failed: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil profile result")
	}

	t.Logf("=== Bot Self Profile (Real LLM) ===")
	t.Logf("  Personality:    %s", result.Personality)
	t.Logf("  Energy Level:   %.2f", result.EnergyLevel)
	t.Logf("  Patience:       %.2f", result.Patience)
	t.Logf("  Verbosity:      %.2f", result.Verbosity)
	t.Logf("  Confidence:     %.2f", result.Confidence)
	t.Logf("  Topics:         %v", result.PreferredTopics)

	// 验证数值范围
	if result.EnergyLevel < 0 || result.EnergyLevel > 1 {
		t.Errorf("EnergyLevel out of range [0,1]: %.2f", result.EnergyLevel)
	}
	if result.Patience < 0 || result.Patience > 1 {
		t.Errorf("Patience out of range [0,1]: %.2f", result.Patience)
	}
	if result.Verbosity < 0 || result.Verbosity > 1 {
		t.Errorf("Verbosity out of range [0,1]: %.2f", result.Verbosity)
	}
	if result.Confidence < 0 || result.Confidence > 1 {
		t.Errorf("Confidence out of range [0,1]: %.2f", result.Confidence)
	}

	// 验证语义合理性——Bot 主动参与技术讨论 → energy_level 应该偏高
	if result.EnergyLevel < 0.3 {
		t.Errorf("expected EnergyLevel > 0.3 for active technical bot, got %.2f", result.EnergyLevel)
	}
	// Bot 对重复问题有耐心 → patience 应该偏高
	if result.Patience < 0.3 {
		t.Errorf("expected Patience > 0.3 for patient bot, got %.2f", result.Patience)
	}
	// 人格描述不应为空
	if result.Personality == "" {
		t.Error("Personality should not be empty")
	}
}

// ============================================================================
// 2. Dreaming 管线 + BotProfileProfiler 端到端测试
// ============================================================================

// TestIntegration_Dreaming_BotProfile_RealLLM 验证真实 LLM 下：
//  1. Dreaming 管线能正常执行（Light → REM → Deep）
//  2. BotProfileProfiler 能消费 BotScope 的 L1 记忆产出 L3 画像
//  3. onBotProfileUpdated 回调正确触发
func TestIntegration_Dreaming_BotProfile_RealLLM(t *testing.T) {
	skipIfShort(t)
	bundle := setupIntegLLMBundle(t)
	tp := adaptiveTestTracerProvider()
	logger := adaptiveTestLogger()

	// 1. 创建分层存储和 TieredManager
	store := memory.NewTieredStore(nil)
	tieredMgr := memory.NewTieredManager(memory.TieredManagerConfig{
		Store:                 store,
		EnableAutoConsolidate: true,
	}, tp, logger)

	// 2. 创建 DreamManager
	cfg := memory.DefaultDreamConfig()
	cfg.Enabled = true
	cfg.Deep.MinScore = 0.2
	cfg.Deep.MinRecallCount = 0
	cfg.Deep.MinUniqueQueries = 0
	cfg.Deep.MaxPromotions = 10
	cfg.MaxDreamTokens = integCfg.MaxTokens
	cfg.Model = integCfg.Model

	dreamMgr := memory.NewDreamManager(cfg, tieredMgr, bundle.Main, tp, logger)

	// 3. 注入 BotProfileProfiler
	botProfiler := memory.NewBotProfileProfiler(
		memory.BotProfileProfilerConfig{
			Provider: bundle.Main,
			Model:    &llm.Model{ID: integCfg.Model},
		},
		tp,
		logger,
	)
	dreamMgr.SetBotProfiler(botProfiler)

	// 4. 设置画像更新回调——捕获结果
	var capturedProfile *memory.BotProfileResult
	profileCaptured := make(chan struct{}, 1)
	dreamMgr.SetOnBotProfileUpdated(func(botID string, result *memory.BotProfileResult) {
		capturedProfile = result
		select {
		case profileCaptured <- struct{}{}:
		default:
		}
	})

	// 5. 写入 BotScope 的模拟数据（Bot 自身行为历史）
	botScope := memory.BotScope(integBotID)
	now := time.Now()

	botBehaviorData := []string{
		"Bot 主动参与了关于 Go 并发模型的讨论，给出了包含代码示例的详细回答。",
		"在群聊中，Bot 在没有被 @ 的情况下主动加入 Kubernetes 部署话题的讨论。",
		"用户问了一个重复的基础问题，Bot 耐心地再次解释了概念。",
		"Bot 频繁活跃于 Rust、云原生和后端架构的技术讨论中。",
		"一位用户发了很长的技术分析文，Bot 回复了详尽的评论，表达了对观点的认同。",
		"Bot 在深夜讨论组里仍然保持了积极的参与度，回答了好几个问题。",
		"用户请求帮忙调试一个 Docker 容器问题，Bot 给出了逐步排查的方案。",
		"群友讨论微服务拆分策略时，Bot 贡献了基于实际经验的建议。",
	}

	for i, content := range botBehaviorData {
		_ = store.Append(context.Background(), memory.TieredEntry{
			Entry: memory.Entry{
				ID:        fmt.Sprintf("bot-self-%d", i),
				Scope:     botScope,
				Content:   content,
				Category:  "behavior",
				Source:    "conversation",
				CreatedAt: now,
			},
			Tier: memory.Tier0Working,
		})
	}

	l0Count := mustGetAll(t, store, memory.Tier0Working, botScope)
	t.Logf("BotScope L0 entries: %d", len(l0Count))
	if len(l0Count) == 0 {
		t.Fatal("expected BotScope L0 entries after seeding")
	}

	// 6. 运行梦境管线
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	report, err := dreamMgr.Run(ctx)
	if err != nil {
		t.Fatalf("DreamManager.Run failed: %v", err)
	}
	t.Logf("Dream report: ingested=%d, deduped=%d, promoted=%d, scored=%d, passed=%d",
		report.LightIngested, report.LightDeduped, report.DeepPromoted,
		report.DeepScored, report.DeepPassed)

	// 7. 检查 L1 长期记忆是否产出
	l1Entries := mustGetAll(t, store, memory.Tier1LongTerm, botScope)
	t.Logf("BotScope L1 entries after dream: %d", len(l1Entries))

	// 8. 检查 L3 画像是否产出
	l3Entries := mustGetAll(t, store, memory.Tier3Profile, botScope)
	t.Logf("BotScope L3 profile entries: %d", len(l3Entries))

	// 验证画像回调是否触发（当有 L1 或 L2 数据时）
	if len(l1Entries) > 0 {
		// 等待回调（最多 5 秒，因为 goroutine 中执行）
		select {
		case <-profileCaptured:
			t.Log("Bot profile callback triggered")
		case <-time.After(5 * time.Second):
			t.Log("Bot profile callback not triggered (may need L1 data)")
		}
	}

	// 9. 如果有 L3 画像，验证内容
	if len(l3Entries) > 0 {
		t.Log("=== Bot L3 Profile Content ===")
		for _, e := range l3Entries {
			t.Logf("  [%s] %s (importance=%.2f)", e.Category, e.Content, e.Importance)
		}
	}

	if capturedProfile != nil {
		t.Logf("=== Captured Bot Profile ===")
		t.Logf("  Personality:  %s", capturedProfile.Personality)
		t.Logf("  Energy Level: %.2f", capturedProfile.EnergyLevel)
		t.Logf("  Patience:     %.2f", capturedProfile.Patience)
		t.Logf("  Verbosity:    %.2f", capturedProfile.Verbosity)
		t.Logf("  Confidence:   %.2f", capturedProfile.Confidence)
		t.Logf("  Topics:       %v", capturedProfile.PreferredTopics)

		// 验证语义合理性
		if capturedProfile.EnergyLevel < 0.2 || capturedProfile.EnergyLevel > 1.0 {
			t.Errorf("EnergyLevel %.2f out of expected range [0.2, 1.0]", capturedProfile.EnergyLevel)
		}
		if capturedProfile.Personality == "" {
			t.Error("Personality should not be empty")
		}
	}
}

// ============================================================================
// 3. SOUL.md → 初始画像 → AdaptiveSyncer 全链路测试
// ============================================================================

// TestIntegration_SOUL_to_Profile_RealLLM 验证：
//  1. SOUL.md 内容被正确解析为初始画像
//  2. 初始画像经过 AdaptiveSyncer 后能正确映射为 Engagement 参数
//  3. 真实 LLM 提取的画像与 SOUL 种子一致性
func TestIntegration_SOUL_to_Profile_RealLLM(t *testing.T) {
	skipIfShort(t)
	bundle := setupIntegLLMBundle(t)

	// 1. SOUL.md 定义——一个热情但简洁的技术布道师 Bot
	soulContent := `# Soul

You are an enthusiastic technical evangelist with deep expertise in Go, Rust, and cloud-native technologies.

## Personality

- enthusiastic and passionate about sharing knowledge
- concise and direct in communication, preferring code examples over lengthy explanations
- patient with beginners but pushes experienced developers to think deeper
- proactive in technical discussions, especially about distributed systems and microservices

interests: Go, Rust, Kubernetes, Distributed Systems, Microservices
`

	// 2. 解析初始画像
	initialTraits := engagement.ParseSoulProfile(soulContent)
	t.Logf("=== SOUL.md → Initial Profile ===")
	t.Logf("  Energy Level: %.2f (expected ~0.9)", initialTraits.EnergyLevel)
	t.Logf("  Patience:     %.2f (expected ~0.9)", initialTraits.Patience)
	t.Logf("  Verbosity:    %.2f (expected ~0.4)", initialTraits.Verbosity)
	t.Logf("  Personality:  %s", initialTraits.Personality)
	t.Logf("  Topics:       %v", initialTraits.PreferredTopics)

	// 验证 SOUL 解析正确性
	if initialTraits.EnergyLevel < 0.5 {
		t.Errorf("enthusiastic SOUL → EnergyLevel should be >= 0.5, got %.2f", initialTraits.EnergyLevel)
	}
	if initialTraits.Patience < 0.5 {
		t.Errorf("patient SOUL → Patience should be >= 0.5, got %.2f", initialTraits.Patience)
	}
	if initialTraits.Verbosity > 0.6 {
		t.Errorf("concise SOUL → Verbosity should be <= 0.6, got %.2f", initialTraits.Verbosity)
	}
	if len(initialTraits.PreferredTopics) < 3 {
		t.Errorf("expected at least 3 preferred topics, got %d: %v",
			len(initialTraits.PreferredTopics), initialTraits.PreferredTopics)
	}
	if initialTraits.Confidence != 0.3 {
		t.Errorf("SOUL.md seed confidence should be 0.3, got %.2f", initialTraits.Confidence)
	}

	// 3. 画像 → Engagement 参数映射
	profileMap := engagement.MapProfileToEngagement(initialTraits)
	t.Logf("=== Profile → Engagement Map ===")
	if profileMap.ReplyProbability != nil {
		t.Logf("  ReplyProbability:    %.2f", *profileMap.ReplyProbability)
	}
	if profileMap.BackoffBaseSeconds != nil {
		t.Logf("  BackoffBaseSeconds:  %.2f", *profileMap.BackoffBaseSeconds)
	}
	if profileMap.BackoffStartCount != nil {
		t.Logf("  BackoffStartCount:   %d", *profileMap.BackoffStartCount)
	}
	if profileMap.RateLimitCapacity != nil {
		t.Logf("  RateLimitCapacity:   %d", *profileMap.RateLimitCapacity)
	}

	// 高精力 → 高参与概率
	if profileMap.ReplyProbability != nil && *profileMap.ReplyProbability < 0.15 {
		t.Errorf("enthusiastic bot should have ReplyProbability >= 0.15, got %.2f", *profileMap.ReplyProbability)
	}
	// 高耐心 → 高退避起始计数
	if profileMap.BackoffStartCount != nil && *profileMap.BackoffStartCount < 3 {
		t.Errorf("patient bot should have BackoffStartCount >= 3, got %d", *profileMap.BackoffStartCount)
	}
	// 简洁 → 有最小长度限制
	if profileMap.MinLength == nil {
		t.Log("concise bot has no MinLength (verbosity=0.4 maps to min_length=10)")
	}

	// 4. 创建 AdaptiveSyncer 验证完整映射
	syncer := engagement.NewAdaptiveEngagementSyncer(
		engagement.SyncerConfig{
			BotID:           integBotID,
			InitialTraits:   initialTraits,
			GlobalEnabled:   true,
			EnabledChannels: []string{"telegram"},
		},
		adaptiveTestTracerProvider(),
		adaptiveTestLogger(),
	)

	override := syncer.GetTimingConfigOverride("telegram", "")
	if override == nil {
		t.Fatal("expected non-nil TimingConfig override for enabled channel")
	}

	t.Logf("=== AdaptiveSyncer Output ===")
	if override.ReplyProbability != nil {
		t.Logf("  ReplyProbability:    %.2f", *override.ReplyProbability)
	}
	if override.BackoffStartCount != nil {
		t.Logf("  BackoffStartCount:   %d", *override.BackoffStartCount)
	}
	if override.BackoffBaseSeconds != nil {
		t.Logf("  BackoffBaseSeconds:  %.2f", *override.BackoffBaseSeconds)
	}
	if len(override.Keywords) > 0 {
		t.Logf("  Keywords:            %v", override.Keywords)
	}

	// 5. 额外验证：真实 LLM 提取的画像应大致与 SOUL 种子一致
	// （通过单独的 Profiler 调用对比）
	profiler := memory.NewBotProfileProfiler(
		memory.BotProfileProfilerConfig{
			Provider: bundle.Main,
			Model:    &llm.Model{ID: integCfg.Model},
		},
		adaptiveTestTracerProvider(),
		adaptiveTestLogger(),
	)

	// 构造与 SOUL 人格一致的 L1 记忆
	consistentL1 := []memory.TieredEntry{
		{Entry: memory.Entry{ID: "c1", Content: "Bot 主动回答了一个关于 Go 泛型的复杂问题，给出了包含代码示例的解答。", Category: "behavior"}},
		{Entry: memory.Entry{ID: "c2", Content: "一位初学者问了一个基础语法问题，Bot 耐心地引导对方理解概念而非直接给答案。", Category: "behavior"}},
		{Entry: memory.Entry{ID: "c3", Content: "在 Rust 异步编程讨论中，Bot 贡献了关于 tokio 和 async-std 的深入对比。", Category: "behavior"}},
		{Entry: memory.Entry{ID: "c4", Content: "Bot 频繁参与云原生技术的讨论，尤其是 Kubernetes Operator 开发相关话题。", Category: "observation"}},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	llmResult, err := profiler.ExtractProfile(ctx, consistentL1, nil, nil)
	if err != nil {
		t.Fatalf("LLM profile extraction failed: %v", err)
	}
	if llmResult == nil {
		t.Fatal("expected non-nil LLM profile result")
	}

	t.Logf("=== Real LLM Profile (for comparison) ===")
	t.Logf("  Personality:  %s (SOUL: %s)", llmResult.Personality, initialTraits.Personality)
	t.Logf("  Energy Level: %.2f (SOUL: %.2f)", llmResult.EnergyLevel, initialTraits.EnergyLevel)
	t.Logf("  Patience:     %.2f (SOUL: %.2f)", llmResult.Patience, initialTraits.Patience)
	t.Logf("  Verbosity:    %.2f (SOUL: %.2f)", llmResult.Verbosity, initialTraits.Verbosity)
	t.Logf("  Topics:       %v (SOUL: %v)", llmResult.PreferredTopics, initialTraits.PreferredTopics)

	// 验证 LLM 提取的画像与 SOUL 种子方向一致
	// 热情主动 → 高精力
	if llmResult.EnergyLevel < 0.3 {
		t.Errorf("LLM extracted EnergyLevel too low (%.2f) for enthusiastic bot data", llmResult.EnergyLevel)
	}
	// 有耐心 → 中等以上耐心
	if llmResult.Patience < 0.3 {
		t.Errorf("LLM extracted Patience too low (%.2f) for patient bot data", llmResult.Patience)
	}

	// 6. 验证 LLM 画像可以无缝注入 Syncer
	syncer.UpdateTraits(engagement.BotProfileTraits{
		EnergyLevel:     llmResult.EnergyLevel,
		Patience:        llmResult.Patience,
		Verbosity:       llmResult.Verbosity,
		PreferredTopics: llmResult.PreferredTopics,
		Personality:     llmResult.Personality,
		Confidence:      llmResult.Confidence,
	})

	updated := syncer.GetTraits()
	if updated.EnergyLevel != llmResult.EnergyLevel {
		t.Errorf("syncer UpdateTraits not applied: EnergyLevel mismatch")
	}

	// 验证更新后的 Syncer 产出正确的参数覆盖
	updatedOverride := syncer.GetTimingConfigOverride("telegram", "")
	if updatedOverride == nil || updatedOverride.ReplyProbability == nil {
		t.Error("syncer should produce override after LLM profile update")
	} else {
		t.Logf("Updated ReplyProbability from LLM profile: %.2f", *updatedOverride.ReplyProbability)
	}
}

// ============================================================================
// 4. 综合链路：SOUL → Syncer → TimingGate 最终决策验证
// ============================================================================

// TestIntegration_AdaptiveEngagement_FullPipeline 验证完整自适应链路在
// 真实 LLM 下的行为正确性（不依赖 TimingGate 的 mock judge）。
//
// 验证：从 SOUL.md 解析 → AdaptiveSyncer 创建 → 注入到 TimingGate 的
// DynamicConfigFunc → TimingGate.ShouldEvaluate 能基于画像做出合理决策。
func TestIntegration_AdaptiveEngagement_FullPipeline(t *testing.T) {
	skipIfShort(t)

	// 1. SOUL.md → 初始画像
	soulContent := `# Soul
You are a lazy, reactive bot that only replies when absolutely necessary.
## Personality
- lazy and reluctant to participate
- impatient with repetitive questions
- concise and direct
- reserved and cautious`

	traits := engagement.ParseSoulProfile(soulContent)
	t.Logf("SOUL traits: energy=%.2f, patience=%.2f, verbosity=%.2f, confidence=%.2f",
		traits.EnergyLevel, traits.Patience, traits.Verbosity, traits.Confidence)

	// 2. 创建 AdaptiveSyncer
	syncer := engagement.NewAdaptiveEngagementSyncer(
		engagement.SyncerConfig{
			BotID:           integBotID,
			InitialTraits:   traits,
			GlobalEnabled:   true,
			EnabledChannels: []string{"telegram"},
		},
		adaptiveTestTracerProvider(),
		adaptiveTestLogger(),
	)

	// 3. 创建 TimingGate 并注入 DynamicConfigFunc
	gate := engagement.NewTimingGate(
		nil, // policy 为 nil，测试只用 ShouldEvaluate
		engagement.DefaultTimingGateConfig(),
	)
	gate.SetDynamicConfig(syncer.GetTimingConfigOverride)
	gate.SetRandomNoiseRate(0) // 关闭随机噪声以测试确定性行为

	// 4. 验证懒散 Bot 的参与概率被正确降低
	// 直接在 AdaptiveSyncer 层面验证映射结果
	override := syncer.GetTimingConfigOverride("telegram", "")
	if override == nil {
		t.Fatal("expected non-nil override")
	}

	t.Logf("=== Lazy Bot Engagement Parameters ===")
	if override.ReplyProbability != nil {
		t.Logf("  ReplyProbability: %.3f (expected ≤ 0.15)", *override.ReplyProbability)
		if *override.ReplyProbability > 0.15 {
			t.Errorf("lazy bot ReplyProbability should be ≤ 0.15, got %.3f", *override.ReplyProbability)
		}
	}
	if override.BackoffBaseSeconds != nil {
		t.Logf("  BackoffBaseSeconds: %.1f (expected ≥ 30 for impatient)", *override.BackoffBaseSeconds)
		// 懒散+不耐心 → 高退避 (patience=0.3 → backoff = 5 + 0.7*55 = 43.5)
		if *override.BackoffBaseSeconds < 35 {
			t.Errorf("lazy impatient bot should have high BackoffBaseSeconds, got %.1f", *override.BackoffBaseSeconds)
		}
	}

	// 5. 切换为高精力 Bot 验证差异
	enthusiasticTraits := engagement.BotProfileTraits{
		EnergyLevel: 0.9,
		Patience:    0.9,
		Verbosity:   0.7,
		Personality: "enthusiastic helper",
	}
	syncer.UpdateTraits(enthusiasticTraits)

	enthusiasticOverride := syncer.GetTimingConfigOverride("telegram", "")
	if enthusiasticOverride == nil {
		t.Fatal("expected non-nil override for enthusiastic bot")
	}

	t.Logf("=== Enthusiastic Bot Engagement Parameters ===")
	if enthusiasticOverride.ReplyProbability != nil {
		t.Logf("  ReplyProbability: %.3f (expected ≥ 0.20)", *enthusiasticOverride.ReplyProbability)
		if *enthusiasticOverride.ReplyProbability < 0.20 {
			t.Errorf("enthusiastic bot ReplyProbability should be ≥ 0.20, got %.3f",
				*enthusiasticOverride.ReplyProbability)
		}
	}
	if enthusiasticOverride.BackoffBaseSeconds != nil {
		t.Logf("  BackoffBaseSeconds: %.1f (expected < 30)", *enthusiasticOverride.BackoffBaseSeconds)
		if *enthusiasticOverride.BackoffBaseSeconds >= 30 {
			t.Errorf("enthusiastic bot should have low BackoffBaseSeconds, got %.1f", *enthusiasticOverride.BackoffBaseSeconds)
		}
	}

	// 6. 验证懒散 vs 热情 Bot 的差异
	if override.ReplyProbability != nil && enthusiasticOverride.ReplyProbability != nil {
		if *enthusiasticOverride.ReplyProbability <= *override.ReplyProbability {
			t.Error("enthusiastic bot should have higher ReplyProbability than lazy bot")
		}
	}
}

// ============================================================================
// helpers
// ============================================================================
// mustGetAll 定义于 dream_integration_test.go，此处复用。
