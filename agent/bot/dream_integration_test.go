package bot

import (
	"context"
	"fmt"
	"testing"
	"time"

	noop_trace "go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/memory"
	"github.com/kasuganosora/thinkbot/llm"
)

// mustGetAll 辅助函数：从 TieredStore 获取指定 tier + scope 的所有条目，失败则 Fatal。
func mustGetAll(t *testing.T, store *memory.TieredStore, tier memory.MemoryTier, scope memory.Scope) []memory.TieredEntry {
	t.Helper()
	entries, err := store.GetAll(context.Background(), tier, scope)
	if err != nil {
		t.Fatalf("GetAll(tier=%d, scope=%s): %v", tier, scope.Key(), err)
	}
	return entries
}

// ============================================================================
// Dreaming / 梦境巩固系统集成测试
//
// 验证三相位梦境管线（Light → REM → Deep）在真实 LLM 环境下的端到端行为：
//   - L0 原始记忆 → LLM 提取候选 → 去重 → 主题聚类 → 评分门控 → 晋升 L1
//
// 运行命令：
//
//	go test -v -run TestIntegration_Dream ./agent/bot/ -timeout 180s
// ============================================================================

// seedConversationData 向 TieredStore 写入模拟的 L0 对话数据（按 scope 分桶）。
// 模拟一个用户在多轮对话中暴露的个人信息和偏好。
func seedConversationData(t *testing.T, store *memory.TieredStore, scope memory.Scope) {
	t.Helper()

	now := time.Now()
	entries := []string{
		"用户说他叫张三，英文名是 Alex Zhang。",
		"张三在北京工作，公司在中关村，做后端开发。",
		"他主要用 Go 语言写微服务，也懂 Python 和 TypeScript。",
		"张三说他最近在学习 Kubernetes 和 Docker，觉得容器化很重要。",
		"他的团队有 8 个人，使用 GitLab CI/CD 做持续集成。",
		"张三说他每天喝两杯咖啡，喜欢美式不要太甜的。",
		"他使用 VS Code 和 GoLand 写代码，觉得终端很高效。",
		"张三说他喜欢在周末跑马拉松，已经跑过 3 次半马了。",
		"他最喜欢的书是《人月神话》，觉得软件工程里的很多问题都是人的问题。",
		"张三说他的下一个目标是学 Rust，但还在犹豫。",
		"他喜欢用 Linux 作为开发环境，觉得 macOS 也很好但太贵了。",
		"张三说最近项目延期了，因为需求变更太频繁。",
		"他在考虑换工作，想要一个更注重工程质量的团队。",
		"张三说他女朋友也做技术，是前端工程师，用 React。",
		"他喜欢吃川菜，特别是火锅，但不太能吃辣。",
	}

	for i, content := range entries {
		err := store.Append(context.Background(), memory.TieredEntry{
			Entry: memory.Entry{
				ID:        fmt.Sprintf("seed-%s-%d", scope.ID, i),
				Scope:     scope,
				Content:   content,
				Category:  "fact",
				Source:    "conversation",
				CreatedAt: now.Add(-time.Duration(15-i) * time.Hour),
			},
			Tier: memory.Tier0Working,
		})
		if err != nil {
			t.Fatalf("seed entry %d failed: %v", i, err)
		}
	}

	t.Logf("Seeded %d entries for scope %s", len(entries), scope.Key())
}

// TestIntegration_Dream_FullPipeline 验证完整的 Light → REM → Deep 三相位管线。
//
// 预期：
//   - Light 从 L0 提取候选事实（LLM 降级到规则模式也能工作）
//   - Deep 对候选评分并晋升到 L1
//   - DreamReport 包含有效统计数据
func TestIntegration_Dream_FullPipeline(t *testing.T) {
	skipIfShort(t)
	bundle := setupIntegLLMBundle(t)

	logger := zap.NewNop().Sugar()
	tp := noop_trace.NewTracerProvider()

	// 1. 创建 TieredStore + TieredManager
	store := memory.NewTieredStore(nil)
	tieredMgr := memory.NewTieredManager(memory.TieredManagerConfig{
		Store:                 store,
		EnableAutoConsolidate: false, // 禁用自动合并，手动控制
	}, tp, logger)

	// 2. 配置梦境（缩短深睡眠门控以适应测试数据量）
	cfg := memory.DefaultDreamConfig()
	cfg.Enabled = true
	cfg.VerboseLogging = true
	cfg.Deep.MinScore = 0.2       // 首次无 Recall/Query 历史，总分较低
	cfg.Deep.MinRecallCount = 0   // 首次运行无历史召回
	cfg.Deep.MinUniqueQueries = 0 // 首次运行无查询历史
	cfg.Deep.MaxPromotions = 10
	cfg.MaxDreamTokens = integCfg.MaxTokens

	// 3. 创建 DreamManager
	cfg.Model = integCfg.Model
	dreamMgr := memory.NewDreamManager(cfg, tieredMgr, bundle.Main, tp, logger)

	// 4. 写入模拟对话数据到两个 scope
	scope1 := memory.ChannelScope("dream-bot-001")
	seedConversationData(t, store, scope1)

	// 验证 L0 有数据
	l0Entries := mustGetAll(t, store, memory.Tier0Working, scope1)
	t.Logf("L0 entries before dream: %d", len(l0Entries))
	if len(l0Entries) == 0 {
		t.Fatal("expected L0 entries after seeding")
	}

	// 5. 运行梦境管线
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	report, err := dreamMgr.Run(ctx)
	if err != nil {
		t.Fatalf("DreamManager.Run failed: %v", err)
	}

	// 6. 验证 DreamReport
	t.Logf("Dream Report:")
	t.Logf("  Phase:        %s", report.Phase)
	t.Logf("  Light Ingested: %d", report.LightIngested)
	t.Logf("  Light Deduped:  %d", report.LightDeduped)
	t.Logf("  Light Dropped:  %d", report.LightDropped)
	t.Logf("  REM Themes:     %d", report.REMThemes)
	t.Logf("  REM Candidates: %d", report.REMCandidates)
	t.Logf("  Deep Scored:    %d", report.DeepScored)
	t.Logf("  Deep Passed:    %d", report.DeepPassed)
	t.Logf("  Deep Promoted:  %d", report.DeepPromoted)
	t.Logf("  Duration:       %v", report.Duration())
	if report.Error != "" {
		t.Errorf("dream report contains error: %s", report.Error)
	}

	// Light 应提取出候选
	if report.LightIngested == 0 {
		t.Error("expected LightIngested > 0 (no candidates extracted from L0)")
	}

	// Deep 应至少评出一些分数
	if report.DeepScored == 0 {
		t.Error("expected DeepScored > 0 (no candidates scored)")
	}

	// 7. 验证 L1 晋升结果
	l1Entries := mustGetAll(t, store, memory.Tier1LongTerm, scope1)
	t.Logf("L1 entries after dream: %d", len(l1Entries))

	if report.DeepPromoted > 0 && len(l1Entries) == 0 {
		t.Errorf("report says %d promoted but L1 has %d entries",
			report.DeepPromoted, len(l1Entries))
	}

	for _, e := range l1Entries {
		t.Logf("  L1 entry: tier=%d, category=%s, source=%s, content=%q",
			e.Tier, e.Category, e.Source, truncate(e.Content, 100))
		if source, ok := e.Metadata["dream_source"]; ok {
			t.Logf("    dream_source=%v, dream_score=%v, dream_theme=%v",
				source, e.Metadata["dream_score"], e.Metadata["dream_theme"])
		}
	}

	// 8. 验证梦境日记
	diary := dreamMgr.DreamDiary()
	t.Logf("Dream Diary entries: %d", len(diary))
	for _, d := range diary {
		t.Logf("  diary: %s", truncate(d, 200))
	}
}

// TestIntegration_Dream_LLMExtraction 验证 Light 相位的 LLM 提取能力。
//
// 通过直接调用 DreamManager 的内部能力，验证 LLM 能从对话片段中
// 正确提取结构化事实。
func TestIntegration_Dream_LLMExtraction(t *testing.T) {
	skipIfShort(t)
	bundle := setupIntegLLMBundle(t)

	logger := zap.NewNop().Sugar()
	tp := noop_trace.NewTracerProvider()

	store := memory.NewTieredStore(nil)
	tieredMgr := memory.NewTieredManager(memory.TieredManagerConfig{
		Store:                 store,
		EnableAutoConsolidate: false,
	}, tp, logger)

	cfg := memory.DefaultDreamConfig()
	cfg.Enabled = true
	cfg.VerboseLogging = true
	cfg.Deep.MinScore = 0.3
	cfg.Deep.MinRecallCount = 1
	cfg.Deep.MinUniqueQueries = 1
	cfg.Deep.MaxPromotions = 5
	cfg.MaxDreamTokens = integCfg.MaxTokens
	cfg.Model = integCfg.Model

	dreamMgr := memory.NewDreamManager(cfg, tieredMgr, bundle.Main, tp, logger)

	// 写入少量高质量数据（用户偏好 + 事实）
	scope := memory.ChannelScope("dream-extract-test")
	now := time.Now()

	extractEntries := []string{
		"我是李四，住在上海浦东，工作快十年了。我主要做数据分析和机器学习，用 Python 和 SQL 比较多。",
		"李四说他最喜欢的编程语言是 Python，因为语法简洁库又多。",
		"他在公司负责一个推荐系统的项目，用 PySpark 处理数据，用 XGBoost 做模型训练。",
		"李四周末喜欢去图书馆看书，最近在看《统计学习方法》第二版。",
		"他说他的职业目标是成为数据科学总监，也想学 Rust 做高性能计算。",
	}

	for i, content := range extractEntries {
		store.Append(context.Background(), memory.TieredEntry{
			Entry: memory.Entry{
				ID:        fmt.Sprintf("extract-%d", i),
				Scope:     scope,
				Content:   content,
				Category:  "fact",
				Source:    "conversation",
				CreatedAt: now.Add(-time.Duration(5-i) * time.Hour),
			},
			Tier: memory.Tier0Working,
		})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	report, err := dreamMgr.Run(ctx)
	if err != nil {
		t.Fatalf("DreamManager.Run failed: %v", err)
	}

	t.Logf("Dream Report: light_ingested=%d, deep_scored=%d, deep_passed=%d, deep_promoted=%d",
		report.LightIngested, report.DeepScored, report.DeepPassed, report.DeepPromoted)

	if report.LightIngested == 0 {
		t.Error("expected LightIngested > 0 (LLM extraction should produce candidates)")
	}

	// 验证 L1 中是否有有意义的条目
	l1Entries := mustGetAll(t, store, memory.Tier1LongTerm, scope)
	t.Logf("L1 entries: %d", len(l1Entries))
	for _, e := range l1Entries {
		t.Logf("  promoted: category=%s, content=%q", e.Category, truncate(e.Content, 100))
	}
}

// TestIntegration_Dream_BasicPipeline 验证最简单的梦境流水线（Light + Deep，
// 不使用 REM 主题聚类），确保基本路径在真实 LLM 下能跑通。
//
// 这是一个快速冒烟测试，不拆解相位，只验证"能跑通且产出有意义的报告"。
func TestIntegration_Dream_BasicPipeline(t *testing.T) {
	skipIfShort(t)
	bundle := setupIntegLLMBundle(t)

	logger := zap.NewNop().Sugar()
	tp := noop_trace.NewTracerProvider()

	store := memory.NewTieredStore(nil)
	tieredMgr := memory.NewTieredManager(memory.TieredManagerConfig{
		Store:                 store,
		EnableAutoConsolidate: false,
	}, tp, logger)

	cfg := memory.DefaultDreamConfig()
	cfg.Enabled = true
	cfg.Light.LookbackDays = 1
	cfg.Deep.MinScore = 0.1 // 极低门槛 → 应该有人晋升
	cfg.Deep.MinRecallCount = 1
	cfg.Deep.MinUniqueQueries = 1
	cfg.Deep.MaxPromotions = 20
	cfg.MaxDreamTokens = integCfg.MaxTokens
	cfg.Model = integCfg.Model

	dreamMgr := memory.NewDreamManager(cfg, tieredMgr, bundle.Main, tp, logger)

	// 少量但信息密集的对话
	scope := memory.ChannelScope("dream-basic-test")
	now := time.Now()

	contents := []string{
		"王五说他住在深圳南山，已经工作 5 年了，是一名前端工程师。",
		"王五擅长 React 和 TypeScript，最近在学 Next.js 和 Tailwind CSS。",
		"他说他对设计系统很感兴趣，觉得组件化是前端的未来。",
		"王五喜欢骑自行车上班，觉得比坐地铁舒服。",
		"他的理想公司是可以远程办公的，正在考虑投简历给一家做 SaaS 的公司。",
	}

	for i, c := range contents {
		store.Append(context.Background(), memory.TieredEntry{
			Entry: memory.Entry{
				ID:        fmt.Sprintf("basic-%d", i),
				Scope:     scope,
				Content:   c,
				Category:  "fact",
				Source:    "conversation",
				CreatedAt: now.Add(-time.Duration(5-i) * time.Hour),
			},
			Tier: memory.Tier0Working,
		})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	report, err := dreamMgr.Run(ctx)
	if err != nil {
		t.Fatalf("DreamManager.Run failed: %v", err)
	}

	// 冒烟断言
	if report.Phase != "deep" {
		t.Errorf("expected phase 'deep', got %q", report.Phase)
	}
	if report.Error != "" {
		t.Errorf("dream report error: %s", report.Error)
	}
	if report.LightIngested == 0 {
		t.Error("expected LightIngested > 0")
	}
	if report.DeepScored == 0 {
		t.Error("expected DeepScored > 0")
	}

	t.Logf("Basic dream: light=%d, deep_scored=%d, deep_passed=%d, deep_promoted=%d, duration=%v",
		report.LightIngested, report.DeepScored, report.DeepPassed, report.DeepPromoted, report.Duration())

	l1 := mustGetAll(t, store, memory.Tier1LongTerm, scope)
	t.Logf("L1 promoted entries: %d", len(l1))
	for _, e := range l1 {
		t.Logf("  [%s] %q (score=%v, source=%v)",
			e.Category, truncate(e.Content, 120),
			e.Metadata["dream_score"], e.Metadata["dream_source"])
	}
}

// TestIntegration_Dream_NoLLM_Fallback 验证 LLM 不可用时的降级行为。
// 当 provider==nil 时，Light 应降级为规则提取（直接切原文），整个管线仍然完成。
func TestIntegration_Dream_NoLLM_Fallback(t *testing.T) {
	skipIfShort(t)

	logger := zap.NewNop().Sugar()
	tp := noop_trace.NewTracerProvider()

	store := memory.NewTieredStore(nil)
	tieredMgr := memory.NewTieredManager(memory.TieredManagerConfig{
		Store:                 store,
		EnableAutoConsolidate: false,
	}, tp, logger)

	cfg := memory.DefaultDreamConfig()
	cfg.Enabled = true
	cfg.Deep.MinScore = 0.3
	cfg.Deep.MinRecallCount = 1
	cfg.Deep.MinUniqueQueries = 1

	// provider = nil → Light 降级到规则提取
	dreamMgr := memory.NewDreamManager(cfg, tieredMgr, nil, tp, logger)

	scope := memory.ChannelScope("dream-fallback-test")
	now := time.Now()

	for i, c := range []string{
		"用户叫王小明，住在上海。",
		"王小明喜欢看电影，特别是科幻片。",
		"他是后端工程师，用 Python 和 Go 写代码。",
	} {
		store.Append(context.Background(), memory.TieredEntry{
			Entry: memory.Entry{
				ID:        fmt.Sprintf("fallback-%d", i),
				Scope:     scope,
				Content:   c,
				Category:  "fact",
				Source:    "conversation",
				CreatedAt: now.Add(-time.Duration(3-i) * time.Hour),
			},
			Tier: memory.Tier0Working,
		})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	report, err := dreamMgr.Run(ctx)
	if err != nil {
		t.Fatalf("Dream fallback failed: %v", err)
	}

	t.Logf("NoLLM fallback: light=%d, deep_scored=%d, deep_promoted=%d",
		report.LightIngested, report.DeepScored, report.DeepPromoted)

	// 即使没有 LLM，Light 应降级提取候选
	if report.LightIngested == 0 {
		t.Error("expected LightIngested > 0 even in fallback mode")
	}
	if report.Phase != "deep" {
		t.Errorf("expected phase 'deep', got %q", report.Phase)
	}
}

// TestIntegration_Dream_ScopeIsolation 验证不同 scope 的记忆互不干扰。
func TestIntegration_Dream_ScopeIsolation(t *testing.T) {
	skipIfShort(t)
	bundle := setupIntegLLMBundle(t)

	logger := zap.NewNop().Sugar()
	tp := noop_trace.NewTracerProvider()

	store := memory.NewTieredStore(nil)
	tieredMgr := memory.NewTieredManager(memory.TieredManagerConfig{
		Store:                 store,
		EnableAutoConsolidate: false,
	}, tp, logger)

	cfg := memory.DefaultDreamConfig()
	cfg.Enabled = true
	cfg.Deep.MinScore = 0.3
	cfg.Deep.MinRecallCount = 1
	cfg.Deep.MinUniqueQueries = 1
	cfg.Deep.MaxPromotions = 10
	cfg.MaxDreamTokens = integCfg.MaxTokens
	cfg.Model = integCfg.Model

	dreamMgr := memory.NewDreamManager(cfg, tieredMgr, bundle.Main, tp, logger)

	// 两个不同 scope，写入不同用户的互斥信息
	scopeA := memory.ChannelScope("isolation-alpha")
	scopeB := memory.ChannelScope("isolation-beta")
	now := time.Now()

	for i, c := range []string{
		"用户Alice说她是北京人，做设计师，喜欢插画。",
		"Alice最喜欢用Figma做UI，觉得Sketch太慢了。",
	} {
		store.Append(context.Background(), memory.TieredEntry{
			Entry: memory.Entry{
				ID:        fmt.Sprintf("iso-a-%d", i),
				Scope:     scopeA,
				Content:   c,
				Category:  "fact",
				Source:    "conversation",
				CreatedAt: now,
			},
			Tier: memory.Tier0Working,
		})
	}

	for i, c := range []string{
		"用户Bob说他是上海人，做后端开发，喜欢跑步。",
		"Bob主要用Go和Java，最近在学Rust。",
	} {
		store.Append(context.Background(), memory.TieredEntry{
			Entry: memory.Entry{
				ID:        fmt.Sprintf("iso-b-%d", i),
				Scope:     scopeB,
				Content:   c,
				Category:  "fact",
				Source:    "conversation",
				CreatedAt: now,
			},
			Tier: memory.Tier0Working,
		})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	report, err := dreamMgr.Run(ctx)
	if err != nil {
		t.Fatalf("dream isolation test failed: %v", err)
	}

	t.Logf("Isolation dream: light=%d, deep_promoted=%d", report.LightIngested, report.DeepPromoted)

	// 分别检查两个 scope 的 L1
	l1A := mustGetAll(t, store, memory.Tier1LongTerm, scopeA)
	l1B := mustGetAll(t, store, memory.Tier1LongTerm, scopeB)

	t.Logf("Scope A (Alice) L1 entries: %d", len(l1A))
	for _, e := range l1A {
		t.Logf("  [A] %q", truncate(e.Content, 80))
	}
	t.Logf("Scope B (Bob) L1 entries: %d", len(l1B))
	for _, e := range l1B {
		t.Logf("  [B] %q", truncate(e.Content, 80))
	}

	// 验证 scope B 没有被 scope A 的内容污染
	for _, e := range l1B {
		if containsAny(e.Content, "Alice", "北京", "设计师", "Figma", "插画") {
			t.Errorf("scope B (Bob) contains scope A (Alice) content: %q", truncate(e.Content, 80))
		}
	}
	for _, e := range l1A {
		if containsAny(e.Content, "Bob", "上海人", "跑步", "Java") {
			t.Errorf("scope A (Alice) contains scope B (Bob) content: %q", truncate(e.Content, 80))
		}
	}
}

// TestIntegration_Dream_Diary 验证梦境日记（审计日志）正常工作。
func TestIntegration_Dream_Diary(t *testing.T) {
	skipIfShort(t)
	bundle := setupIntegLLMBundle(t)

	logger := zap.NewNop().Sugar()
	tp := noop_trace.NewTracerProvider()

	store := memory.NewTieredStore(nil)
	tieredMgr := memory.NewTieredManager(memory.TieredManagerConfig{
		Store:                 store,
		EnableAutoConsolidate: false,
	}, tp, logger)

	cfg := memory.DefaultDreamConfig()
	cfg.Enabled = true
	cfg.Deep.MinScore = 0.3
	cfg.Deep.MinRecallCount = 1
	cfg.Deep.MinUniqueQueries = 1
	cfg.Deep.MaxPromotions = 5
	cfg.MaxDreamTokens = integCfg.MaxTokens
	cfg.Model = integCfg.Model

	dreamMgr := memory.NewDreamManager(cfg, tieredMgr, bundle.Main, tp, logger)

	scope := memory.ChannelScope("dream-diary-test")
	now := time.Now()

	for i, c := range []string{
		"我叫赵六，在成都做产品经理，工作 3 年了。",
		"赵六说他负责一个协同编辑产品的迭代，觉得需求优先级很难排。",
		"他觉得用户反馈应该更快速地被响应，想引入 A/B 测试。",
	} {
		store.Append(context.Background(), memory.TieredEntry{
			Entry: memory.Entry{
				ID:        fmt.Sprintf("diary-%d", i),
				Scope:     scope,
				Content:   c,
				Category:  "fact",
				Source:    "conversation",
				CreatedAt: now,
			},
			Tier: memory.Tier0Working,
		})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	_, err := dreamMgr.Run(ctx)
	if err != nil {
		t.Fatalf("dream diary test failed: %v", err)
	}

	diary := dreamMgr.DreamDiary()
	t.Logf("Dream diary entries: %d", len(diary))

	if len(diary) == 0 {
		t.Error("expected non-empty dream diary")
	}

	for i, entry := range diary {
		t.Logf("  diary[%d]: %s", i, truncate(entry, 300))
	}
}

// ============================================================================
// L3 用户画像提取集成测试
// ============================================================================

// TestIntegration_Dream_ProfileExtraction 验证完整的 L3 画像提取流水线。
//
// 流程：L1（梦镜产出的长期记忆）→ LLMProfiler.ExtractProfile → L3（用户画像）
// 同时测试 BuildProfilePrompt 将画像格式化为 system prompt 片段。
func TestIntegration_Dream_ProfileExtraction(t *testing.T) {
	skipIfShort(t)
	bundle := setupIntegLLMBundle(t)

	logger := zap.NewNop().Sugar()
	tp := noop_trace.NewTracerProvider()

	// 1. 创建带 Profiler 的 TieredManager
	profilerCfg := memory.DefaultLLMProfilerConfig()
	profilerCfg.Provider = bundle.Main
	profilerCfg.Model = llm.ChatModel(integCfg.Model)
	profiler := memory.NewLLMProfiler(profilerCfg, tp, logger)

	store := memory.NewTieredStore(nil)
	tieredMgr := memory.NewTieredManager(memory.TieredManagerConfig{
		Store:    store,
		Profiler: profiler,
	}, tp, logger)

	// 2. 先通过 Dreaming 产出 L1 条目（复用已有的 LLM 路径）
	cfg := memory.DefaultDreamConfig()
	cfg.Enabled = true
	cfg.Deep.MinScore = 0.2
	cfg.Deep.MinRecallCount = 0
	cfg.Deep.MinUniqueQueries = 0
	cfg.Deep.MaxPromotions = 15
	cfg.MaxDreamTokens = integCfg.MaxTokens
	cfg.Model = integCfg.Model

	dreamMgr := memory.NewDreamManager(cfg, tieredMgr, bundle.Main, tp, logger)
	scope := memory.ChannelScope("profile-test")

	// 写入模拟对话
	seedConversationData(t, store, scope)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// 运行 dream → 产出 L1
	dreamReport, err := dreamMgr.Run(ctx)
	if err != nil {
		t.Fatalf("dream run failed: %v", err)
	}
	t.Logf("Dream: promoted=%d L1 entries", dreamReport.DeepPromoted)

	l1Entries := mustGetAll(t, store, memory.Tier1LongTerm, scope)
	t.Logf("L1 entries available: %d", len(l1Entries))

	// 3. 执行画像提取（L1+L2 → L3）
	n, err := tieredMgr.ExtractProfile(ctx, scope)
	if err != nil {
		t.Fatalf("ExtractProfile failed: %v", err)
	}
	t.Logf("Profile items extracted: %d", n)

	if n == 0 {
		t.Error("expected >0 profile items from L1 entries")
	}

	// 4. 验证 L3 条目
	l3Entries := mustGetAll(t, store, memory.Tier3Profile, scope)
	t.Logf("L3 profile entries: %d", len(l3Entries))
	for _, e := range l3Entries {
		t.Logf("  [%s] (confidence=%.2f) %q",
			e.Category, e.Importance, truncate(e.Content, 120))
	}

	if len(l3Entries) == 0 {
		t.Error("expected L3 profile entries after extraction")
	}

	// 验证画像类型（LLM 分类可能不完美，只验证有产出即可）
	validTypes := map[string]bool{memory.ProfileTypeFact: true, memory.ProfileTypeTrait: true,
		memory.ProfileTypePreference: true, memory.ProfileTypeBehavior: true, "profile": true}
	typeCount := 0
	for _, e := range l3Entries {
		if _, ok := validTypes[e.Category]; ok {
			typeCount++
		}
		if e.Source != "profiler" {
			t.Errorf("expected source 'profiler', got %q", e.Source)
		}
	}
	t.Logf("Valid profile type count: %d/%d", typeCount, len(l3Entries))

	// 5. 测试 BuildProfilePrompt
	prompt := memory.BuildProfilePrompt(l3Entries)
	t.Logf("Profile prompt (%d chars):\n%s", len(prompt), truncate(prompt, 500))

	if prompt == "" {
		t.Error("BuildProfilePrompt returned empty string")
	}
	if !containsAny(prompt, "[User Profile]", "[End Profile]") {
		t.Error("BuildProfilePrompt missing [User Profile] / [End Profile] markers")
	}
	// 至少应包含画像内容的关键词
	if !containsAny(prompt, "profile", "fact", "trait", "preference", "behavior") {
		t.Logf("note: prompt doesn't contain known category tags")
	}
}

// TestIntegration_Dream_ProfileWithL2Episodic 验证 L2 场景数据参与画像提取。
//
// 手动写入 L2 场景条目后，Profile 提取应能利用场景维度的信息。
func TestIntegration_Dream_ProfileWithL2Episodic(t *testing.T) {
	skipIfShort(t)
	bundle := setupIntegLLMBundle(t)

	logger := zap.NewNop().Sugar()
	tp := noop_trace.NewTracerProvider()

	profilerCfg := memory.DefaultLLMProfilerConfig()
	profilerCfg.Provider = bundle.Main
	profilerCfg.Model = llm.ChatModel(integCfg.Model)
	profiler := memory.NewLLMProfiler(profilerCfg, tp, logger)

	store := memory.NewTieredStore(nil)
	tieredMgr := memory.NewTieredManager(memory.TieredManagerConfig{
		Store:    store,
		Profiler: profiler,
	}, tp, logger)

	scope := memory.ChannelScope("profile-l2-test")
	now := time.Now()

	// 1. 手动写入 L1 长期记忆条目
	l1Contents := []struct {
		content  string
		category string
	}{
		{"用户是后端工程师，主要用 Go 和 Python", "fact"},
		{"用户偏好简短直接的回复，不喜欢啰嗦", "preference"},
		{"用户经常在深夜提问，可能有时差或习惯晚睡", "observation"},
		{"用户喜欢问性能优化相关的问题", "fact"},
		{"用户在讨论技术方案时倾向于先问可行性再要具体方案", "observation"},
	}

	for i, c := range l1Contents {
		store.Append(context.Background(), memory.TieredEntry{
			Entry: memory.Entry{
				ID:        fmt.Sprintf("l1-%d", i),
				Scope:     scope,
				Content:   c.content,
				Category:  c.category,
				Source:    "dreaming",
				CreatedAt: now.Add(-10 * time.Minute),
			},
			Tier: memory.Tier1LongTerm,
		})
	}
	t.Logf("Written %d L1 entries", len(l1Contents))

	// 2. 手动写入 L2 场景记忆（模拟 Aggregator 产出）
	l2Episodes := []struct {
		content string
		ids     []string
	}{
		{"开发环境与技术选型：用户偏好 Go 生态，关注性能和并发模型", []string{"l1-0", "l1-3", "l1-4"}},
		{"沟通与交互模式：用户偏好高效沟通，常在非工作时间活跃", []string{"l1-1", "l1-2"}},
	}

	for _, ep := range l2Episodes {
		tieredMgr.WriteEpisodic(context.Background(), memory.Entry{
			Scope:    scope,
			Content:  ep.content,
			Category: "episode",
			Source:   "aggregator",
		}, ep.ids)
	}
	t.Logf("Written %d L2 episodes", len(l2Episodes))

	l2Entries := mustGetAll(t, store, memory.Tier2Episodic, scope)
	t.Logf("L2 entries: %d", len(l2Entries))

	// 3. 执行画像提取（L1 + L2 → L3）
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	n, err := tieredMgr.ExtractProfile(ctx, scope)
	if err != nil {
		t.Fatalf("ExtractProfile failed: %v", err)
	}
	t.Logf("Profile items extracted (with L2): %d", n)

	if n == 0 {
		t.Error("expected >0 profile items with L2 episodic data")
	}

	// 4. 验证 L3 产出
	l3Entries := mustGetAll(t, store, memory.Tier3Profile, scope)
	t.Logf("L3 entries: %d", len(l3Entries))
	for _, e := range l3Entries {
		t.Logf("  [%s] conf=%.2f %q", e.Category, e.Importance, truncate(e.Content, 120))
	}

	// 5. 再次提取应能更新已有画像（增量更新）
	n2, err := tieredMgr.ExtractProfile(ctx, scope)
	if err != nil {
		t.Fatalf("second ExtractProfile failed: %v", err)
	}
	t.Logf("Second extraction: %d items", n2)

	// 6. BuildProfilePrompt 验证
	prompt := memory.BuildProfilePrompt(l3Entries)
	t.Logf("Profile prompt (%d chars)", len(prompt))
	if prompt == "" {
		t.Error("BuildProfilePrompt returned empty")
	}
}

// TestIntegration_Dream_TieredRetrieval 验证分层检索（L3→L2→L1→L0）正常工作。
func TestIntegration_Dream_TieredRetrieval(t *testing.T) {
	skipIfShort(t)
	bundle := setupIntegLLMBundle(t)

	logger := zap.NewNop().Sugar()
	tp := noop_trace.NewTracerProvider()

	profilerCfg := memory.DefaultLLMProfilerConfig()
	profilerCfg.Provider = bundle.Main
	profilerCfg.Model = llm.ChatModel(integCfg.Model)
	profiler := memory.NewLLMProfiler(profilerCfg, tp, logger)

	store := memory.NewTieredStore(nil)
	tieredMgr := memory.NewTieredManager(memory.TieredManagerConfig{
		Store:    store,
		Profiler: profiler,
	}, tp, logger)

	scope := memory.ChannelScope("tiered-retrieve-test")
	now := time.Now()

	// 写入各层级的记忆
	// L0: 原始工作记忆
	store.Append(context.Background(), memory.TieredEntry{
		Entry: memory.Entry{ID: "w0", Scope: scope, Content: "刚才用户问了天气", Category: "observation", CreatedAt: now},
		Tier:  memory.Tier0Working,
	})

	// L1: 长期事实
	for i, c := range []string{
		"用户名叫钱七，是杭州人",
		"钱七是做前端开发的，擅长 React 和 Vue",
	} {
		store.Append(context.Background(), memory.TieredEntry{
			Entry: memory.Entry{
				ID: fmt.Sprintf("l1-%d", i), Scope: scope, Content: c, Category: "fact", Source: "dreaming", CreatedAt: now,
			},
			Tier: memory.Tier1LongTerm,
		})
	}

	// L2: 场景
	tieredMgr.WriteEpisodic(context.Background(), memory.Entry{
		Scope: scope, Content: "前端技术偏好：用户使用 React/Vue，关注组件化和性能", Category: "episode", Source: "aggregator",
	}, []string{"l1-0", "l1-1"})

	// L3: 画像
	tieredMgr.WriteProfile(context.Background(), memory.Entry{
		Scope: scope, Content: "用户是前端开发工程师，来自杭州", Category: "fact", Source: "profiler", Importance: 0.9,
	})

	// 分层检索验证
	ctx := context.Background()

	l0R, _ := store.Retrieve(ctx, memory.Tier0Working, []memory.Scope{scope}, 10)
	l1R, _ := store.Retrieve(ctx, memory.Tier1LongTerm, []memory.Scope{scope}, 10)
	l2R, _ := store.Retrieve(ctx, memory.Tier2Episodic, []memory.Scope{scope}, 10)
	l3R, _ := store.Retrieve(ctx, memory.Tier3Profile, []memory.Scope{scope}, 10)

	t.Logf("Tier 0 (Working):  %d entries", len(l0R))
	t.Logf("Tier 1 (LongTerm):  %d entries", len(l1R))
	t.Logf("Tier 2 (Episodic):  %d entries", len(l2R))
	t.Logf("Tier 3 (Profile):   %d entries", len(l3R))

	if len(l0R) != 1 {
		t.Errorf("expected 1 L0 entry, got %d", len(l0R))
	}
	if len(l1R) != 2 {
		t.Errorf("expected 2 L1 entries, got %d", len(l1R))
	}
	if len(l2R) != 1 {
		t.Errorf("expected 1 L2 entry, got %d", len(l2R))
	}
	if len(l3R) != 1 {
		t.Errorf("expected 1 L3 entry, got %d", len(l3R))
	}

	// 合并检索
	merged, err := tieredMgr.RetrieveMerged(ctx, []memory.Scope{scope}, 10)
	if err != nil {
		t.Fatalf("RetrieveMerged failed: %v", err)
	}
	t.Logf("Merged retrieval: %d entries", len(merged))
	// 合并检索按 L3→L2→L1→L0 优先级，应至少包含高级别条目
	if len(merged) < 3 {
		t.Errorf("expected >=3 merged entries, got %d", len(merged))
	}
}

// ============================================================================
// 幂等性测试 — 二次 Dream 不重复处理已标记的 L0
// ============================================================================

// TestIntegration_Dream_Idempotent 验证第二次运行时不重复处理已标记的 L0 条目。
//
// 流程：
//  1. 写入 L0 条目 → 首次 Dream → 产出 L1 + 标记 L0 consolidated
//  2. 再次 Dream → Light 阶段因 GetUnprocessed 跳过已标记条目
//  3. 验证第二次 LightIngested=0
func TestIntegration_Dream_Idempotent(t *testing.T) {
	skipIfShort(t)
	bundle := setupIntegLLMBundle(t)

	logger := zap.NewNop().Sugar()
	tp := noop_trace.NewTracerProvider()

	store := memory.NewTieredStore(nil)
	tieredMgr := memory.NewTieredManager(memory.TieredManagerConfig{
		Store:                 store,
		EnableAutoConsolidate: false,
	}, tp, logger)

	cfg := memory.DefaultDreamConfig()
	cfg.Enabled = true
	cfg.Deep.MinScore = 0.2
	cfg.Deep.MinRecallCount = 0
	cfg.Deep.MinUniqueQueries = 0
	cfg.Deep.MaxPromotions = 20
	cfg.MaxDreamTokens = integCfg.MaxTokens
	cfg.Model = integCfg.Model

	dreamMgr := memory.NewDreamManager(cfg, tieredMgr, bundle.Main, tp, logger)

	scope := memory.ChannelScope("idempotent-test")
	now := time.Now()

	for i, c := range []string{
		"用户叫周八，在深圳做游戏开发。",
		"周八主要用 C++ 和 Lua 写游戏逻辑。",
		"他对 AI 游戏 NPC 很感兴趣，在学强化学习。",
	} {
		store.Append(context.Background(), memory.TieredEntry{
			Entry: memory.Entry{
				ID:        fmt.Sprintf("idem-%d", i),
				Scope:     scope,
				Content:   c,
				Category:  "fact",
				Source:    "conversation",
				CreatedAt: now,
			},
			Tier: memory.Tier0Working,
		})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// 第一次运行
	report1, err := dreamMgr.Run(ctx)
	if err != nil {
		t.Fatalf("first dream failed: %v", err)
	}
	t.Logf("Run 1: light_ingested=%d, deep_promoted=%d",
		report1.LightIngested, report1.DeepPromoted)

	// 验证 L0 已被标记为 consolidated
	unprocessed, _ := store.GetUnprocessed(ctx, scope, 100)
	t.Logf("Unprocessed L0 after run 1: %d", len(unprocessed))

	// 第二次运行 — 应该跳过所有已处理的条目
	report2, err := dreamMgr.Run(ctx)
	if err != nil {
		t.Fatalf("second dream failed: %v", err)
	}
	t.Logf("Run 2: light_ingested=%d, deep_promoted=%d",
		report2.LightIngested, report2.DeepPromoted)

	// 第二次不应有新条目被摄入
	if report2.LightIngested > 0 {
		t.Errorf("expected LightIngested=0 on second run (all L0 entries should be marked processed), got %d",
			report2.LightIngested)
	}

	// 验证 L0 全部被标记
	unprocessed2, _ := store.GetUnprocessed(ctx, scope, 100)
	t.Logf("Unprocessed L0 after run 2: %d", len(unprocessed2))
	if len(unprocessed2) > 0 {
		t.Errorf("expected 0 unprocessed L0 entries, got %d", len(unprocessed2))
	}

	// L1 应至少含有第一次晋升的内容
	l1 := mustGetAll(t, store, memory.Tier1LongTerm, scope)
	t.Logf("L1 entries total: %d", len(l1))
	if len(l1) == 0 {
		t.Error("expected L1 entries from first dream run")
	}
}

// ============================================================================
// 辅助函数
// ============================================================================

// containsAny 检查 s 是否包含任意一个关键词。
func containsAny(s string, keywords ...string) bool {
	for _, kw := range keywords {
		if len(kw) > 0 && len(s) >= len(kw) {
			for i := 0; i <= len(s)-len(kw); i++ {
				if s[i:i+len(kw)] == kw {
					return true
				}
			}
		}
	}
	return false
}
