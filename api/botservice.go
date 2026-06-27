package api

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/metric"
	noop_metric "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/trace"
	noop_trace "go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/kasuganosora/thinkbot/agent/bot"
	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/agent/engagement"
	"github.com/kasuganosora/thinkbot/agent/inbound"
	"github.com/kasuganosora/thinkbot/agent/memory"
	"github.com/kasuganosora/thinkbot/agent/outbound"
	"github.com/kasuganosora/thinkbot/agent/pipeline"
	"github.com/kasuganosora/thinkbot/agent/prompt"
	"github.com/kasuganosora/thinkbot/agent/stages"
	agenttools "github.com/kasuganosora/thinkbot/agent/tools"
	"github.com/kasuganosora/thinkbot/channel/misskey"
	"github.com/kasuganosora/thinkbot/channel/telegram"
	"github.com/kasuganosora/thinkbot/config"
	"github.com/kasuganosora/thinkbot/cron"
	"github.com/kasuganosora/thinkbot/dao"
	"github.com/kasuganosora/thinkbot/llm"
	"github.com/kasuganosora/thinkbot/subagent"
	"github.com/kasuganosora/thinkbot/tools"
	"github.com/kasuganosora/thinkbot/util/errs"
	"github.com/kasuganosora/thinkbot/util/strutil"
	"github.com/kasuganosora/thinkbot/workflow"
)

// ============================================================================
// BotService — Bot 定义管理 + 运行时生命周期
//
// 职责：
//   - BotDefinition 的 DB 持久化 CRUD
//   - 运行时 Bot 实例创建（组装 Pipeline + Dispatcher + WebChannel）
//   - 启动/停止/删除 Bot
//   - 管理每个 Bot 的 WebChannel（供 SSE 聊天使用）
// ============================================================================

// BotService 管理 Bot 定义和运行时实例。
type BotService struct {
	db       *gorm.DB
	store    *config.Store
	mgr      *bot.BotManager
	logger   *zap.SugaredLogger
	tp       trace.TracerProvider
	mp       metric.MeterProvider
	eventBus outbound.EventBus

	mu              sync.RWMutex
	channels        map[string]*WebChannel         // botID → WebChannel
	botInstances    map[string]*bot.Bot            // botID → running Bot
	dreamingBundles map[string]*bot.DreamingBundle // botID → DreamingBundle
	cancelFuncs     map[string]context.CancelFunc  // botID → bot context cancel
	closeFuncs      map[string]func()              // botID → sub-agent managers cleanup
}

// NewBotService 创建 BotService。
func NewBotService(db *gorm.DB, store *config.Store, mgr *bot.BotManager, logger *zap.SugaredLogger, tp trace.TracerProvider, mp metric.MeterProvider, eventBus outbound.EventBus) *BotService {
	if tp == nil {
		tp = noop_trace.NewTracerProvider()
	}
	if mp == nil {
		mp = noop_metric.NewMeterProvider()
	}
	if eventBus == nil {
		eventBus = outbound.NewMemoryEventBus(outbound.DefaultMemoryEventBusConfig(), logger)
	}
	return &BotService{
		db:              db,
		store:           store,
		mgr:             mgr,
		logger:          logger.With("component", "bot_service"),
		tp:              tp,
		mp:              mp,
		eventBus:        eventBus,
		channels:        make(map[string]*WebChannel),
		botInstances:    make(map[string]*bot.Bot),
		dreamingBundles: make(map[string]*bot.DreamingBundle),
		cancelFuncs:     make(map[string]context.CancelFunc),
		closeFuncs:      make(map[string]func()),
	}
}

// --- BotDefinition CRUD ---

// ListDefinitions 返回所有 Bot 定义。
func (s *BotService) ListDefinitions() ([]dao.BotDefinition, error) {
	var defs []dao.BotDefinition
	if err := s.db.Order("created_at DESC").Find(&defs).Error; err != nil {
		return nil, errs.Wrap(err, "bot_service: list definitions")
	}
	return defs, nil
}

// GetDefinition 返回指定 Bot 定义。
func (s *BotService) GetDefinition(id string) (*dao.BotDefinition, error) {
	var def dao.BotDefinition
	if err := s.db.First(&def, "id = ?", id).Error; err != nil {
		return nil, errs.Wrap(err, "bot_service: get definition")
	}
	return &def, nil
}

// CreateDefinition 创建 Bot 定义。
func (s *BotService) CreateDefinition(def *dao.BotDefinition) error {
	if def.ID == "" {
		return errs.BadRequest("bot id is required")
	}
	if def.Name == "" {
		return errs.BadRequest("bot name is required")
	}
	if def.Status == "" {
		def.Status = dao.BotStatusStopped
	}
	if err := s.db.Create(def).Error; err != nil {
		return errs.Wrap(err, "bot_service: create definition")
	}
	return nil
}

// UpdateDefinition 更新 Bot 定义。
func (s *BotService) UpdateDefinition(id string, updates map[string]any) error {
	result := s.db.Model(&dao.BotDefinition{}).Where("id = ?", id).Updates(updates)
	if result.Error != nil {
		return errs.Wrap(result.Error, "bot_service: update definition")
	}
	if result.RowsAffected == 0 {
		return errs.NotFound("bot definition not found")
	}
	return nil
}

// DeleteDefinition 删除 Bot 定义（如果正在运行则先停止）。
func (s *BotService) DeleteDefinition(id string) error {
	// 先停止运行中的实例
	s.StopBot(id)

	if err := s.db.Delete(&dao.BotDefinition{}, "id = ?", id).Error; err != nil {
		return errs.Wrap(err, "bot_service: delete definition")
	}
	return nil
}

// --- 运行时管理 ---

// StartBot 从定义创建并启动 Bot 实例。
func (s *BotService) StartBot(ctx context.Context, id string) error {
	def, err := s.GetDefinition(id)
	if err != nil {
		return err
	}

	// 竞态修复：reserve 占位模式
	s.mu.Lock()
	if _, exists := s.botInstances[id]; exists {
		s.mu.Unlock()
		return errs.Conflict("bot is already running")
	}
	s.botInstances[id] = nil // 占位，防止并发启动
	s.mu.Unlock()

	// 创建失败时回滚占位
	rollback := func() {
		s.mu.Lock()
		// 只有 nil 占位才删除，避免删除已赋值的实例
		if s.botInstances[id] == nil {
			delete(s.botInstances, id)
		}
		s.mu.Unlock()
	}

	// 将 Bot 定义的 LLM 分配同步到 config store
	// （CreateLLMBundle 从 config store 读 bot.<id>.main/light，而定义存在 DB 表中）
	syncCtx := context.Background()
	if def.LLMMain != "" {
		if err := s.store.Set(syncCtx, config.BotLLMKey(id, "main"), def.LLMMain); err != nil {
			rollback()
			return errs.Wrap(err, "bot_service: sync LLM main assignment")
		}
	}
	if def.LLMLight != "" {
		if err := s.store.Set(syncCtx, config.BotLLMKey(id, "light"), def.LLMLight); err != nil {
			rollback()
			return errs.Wrap(err, "bot_service: sync LLM light assignment")
		}
	}

	// 创建 LLM Bundle
	builder := config.NewBuilder(s.store, s.logger)
	bundle, err := bot.CreateLLMBundle(builder, id)
	if err != nil {
		rollback()
		return errs.Wrap(err, "bot_service: create LLM bundle")
	}

	// 创建 LLM Stage
	mainModel := &llm.Model{ID: def.LLMMain}
	if def.Model != "" {
		mainModel.DisplayName = def.Model
	}

	var temp *float64
	if def.Temperature > 0 {
		t := def.Temperature
		temp = &t
	}
	var maxTok *int
	if def.MaxTokens > 0 {
		mt := def.MaxTokens
		maxTok = &mt
	}

	// MessageBuilder：从 Message.Metadata["chat_history"] 加载历史上下文
	messageBuilder := func(msg core.Message) []llm.Message {
		var messages []llm.Message
		if history, ok := msg.Metadata["chat_history"]; ok {
			if msgs, ok := history.([]dao.ChatMessage); ok {
				for _, m := range msgs {
					switch m.Role {
					case dao.ChatRoleUser:
						messages = append(messages, llm.UserMessage(m.Content))
					case dao.ChatRoleAssistant:
						messages = append(messages, llm.AssistantMessage(m.Content))
					}
				}
			}
		}
		messages = append(messages, llm.UserMessage(msg.Text))
		return messages
	}

	// 创建 Prompt Registry + Tool Manager
	promptReg := prompt.NewRegistry()
	toolMgr := agenttools.NewToolManager(promptReg, s.store, s.logger)

	// 注册通用工具（web_fetch, calculate, now, web_search 等）
	if err := tools.RegisterTools(toolMgr, tools.Config{
		TimezoneResolver: builder.GetBotTimezone,
	}); err != nil {
		rollback()
		return errs.Wrap(err, "bot_service: register tools")
	}

	// 注册记忆工具
	memRepo := memory.NewMemoryRepository()
	if err := memory.RegisterTools(toolMgr, memory.DefaultToolConfig(memRepo)); err != nil {
		s.logger.Warnw("failed to register memory tools", "err", err)
	}

	// 注册工作流工具
	wfMgr, wfSaMgr := workflow.Setup(workflow.WireConfig{
		Provider:       bundle.Main,
		Model:          bundle.MainDef.Model,
		DB:             s.db,
		Logger:         s.logger,
		TracerProvider: s.tp,
		Store:          s.store,
		EventBus:       s.eventBus,
	})
	if err := workflow.RegisterTools(toolMgr, wfMgr); err != nil {
		s.logger.Warnw("failed to register workflow tools", "err", err)
	}

	// 注册 SubAgent 工具
	saMgr := subagent.NewSubAgentManager(bundle.Main, bundle.MainDef.Model)
	if err := subagent.RegisterTools(toolMgr, saMgr); err != nil {
		s.logger.Warnw("failed to register subagent tools", "err", err)
	}

	// 注册子代理清理回调，Bot 停止时释放 goroutine 和 LLM 连接
	wfCleanup := func() {
		wfSaMgr.CloseAll()
		saMgr.CloseAll()
	}

	// 创建 RunJournal 记录器（LLM 调用事件持久化）
	journalCfg := pipeline.DefaultRunJournalConfig()
	journalCfg.Caller = "lead_agent"
	journal := pipeline.NewRunJournalRecorder(s.db, journalCfg)

	// RunJournal cleanup（引用 journal，必须在 journal 创建之后）
	journalCleanup := func(ctx context.Context) {
		if err := journal.Shutdown(ctx); err != nil {
			s.logger.Warnw("run journal shutdown failed", "err", err)
		}
	}

	llmStage := stages.NewLLMStage(
		"llm",
		bundle.Main,
		stages.LLMConfig{
			SystemPrompt:    def.SystemPrompt,
			Model:           mainModel,
			Temperature:     temp,
			MaxTokens:       maxTok,
			ReasoningEffort: def.ReasoningEffort,
			MessageBuilder:  messageBuilder,
			ToolResolver:    toolMgr,
			MaxSteps:        10,
			StreamPublisher: s.eventBus,
			UsageRecorder:   journal, // RunJournal 记录每次 LLM 调用
			ReductionConfig: llm.DefaultReductionConfigPtr(),
		},
		s.tp,
		s.logger,
	)

	// 用安全中间件包装 LLMStage：
	//   Token 预算 → 循环检测 → RunJournal 元数据注入 → LLMStage
	wrappedLLM := pipeline.WithMiddleware(llmStage,
		journal.Middleware(),
		pipeline.LoopDetectionMiddleware(pipeline.NewLoopDetectionConfig()),
		pipeline.TokenBudgetMiddleware(pipeline.NewTokenBudgetConfig()),
	)

	// 创建共享 SelfIDSet——Ingress 和 Engagement 两层防线引用同一份数据。
	// Channel 在 Start 时通过 RegisterSelfUserID 注册自身 ID，
	// 两层防线同时生效，无需时序协调。
	selfIDSet := inbound.NewSelfIDSet()

	// 创建 Engagement Stage（主动参与）
	engCfg := builder.GetEngagementConfig()
	var engagementStage *engagement.EngagementStage
	var burstBuf *engagement.BurstBuffer
	if engCfg.Enabled {
		// 构建 LLM Judge（Tier 2 快判）
		var judge engagement.LLMJudge
		if engCfg.LLMJudgeEnabled {
			// 优先使用 Light LLM 做快判（更便宜、更快）
			judgeProvider := bundle.Main
			modelID := bundle.MainDef.Model
			if bundle.Light != nil {
				judgeProvider = bundle.Light
				modelID = bundle.LightDef.Model
			}
			adapter := newLLMJudgeAdapter(judgeProvider, modelID)

			promptCfg := engagement.PromptConfig{
				BotName:    def.Name,
				BotPersona: strutil.Truncate(def.SystemPrompt, 200),
				Interests:  engCfg.Keywords,
			}

			if engCfg.EngagementThreshold > 0 {
				judge = engagement.NewScoredSimpleJudge(adapter, promptCfg)
			} else {
				judge = engagement.NewSimpleJudge(adapter, promptCfg)
			}
		}

		// 构建全部 engagement 组件（policy + gate + rateLimit）
		// 使用共享 SelfIDSet 作为自消息检查器：
		// - selfIDSet.Contains 绑定到 Engagement 的 SelfExclusionRule
		// - 同一个 selfIDSet 也注入到 Ingress（通过 BotParams.SelfIDSet）
		// - Channel 在 Start 时注册的 ID 会同时被两层防线感知
		result := engagement.BuildFromConfigSelfChecker(engCfg, selfIDSet.Contains, judge)
		stageCfg := engagement.BuildStageConfig(engCfg)
		engagementStage = engagement.NewEngagementStage(
			"engagement", result.Policy, stageCfg,
			s.tp, s.logger,
		)
		if result.Gate != nil {
			engagementStage = engagementStage.WithTimingGate(result.Gate)
		}
		if engCfg.BurstIntervalSeconds > 0 {
			burstBuf = engagement.NewBurstBuffer(
				time.Duration(engCfg.BurstIntervalSeconds * float64(time.Second)),
			)
		}

		s.logger.Infow("engagement stage enabled",
			"bot_id", id,
			"profile", engCfg.Profile,
			"reply_probability", engCfg.ReplyProbability,
			"llm_judge", engCfg.LLMJudgeEnabled,
			"threshold", engCfg.EngagementThreshold,
			"auto_adjust_freq", engCfg.AutoAdjustFrequency)
	}

	// 创建 Pipeline
	stages := []core.StageInfo{
		{Stage: wrappedLLM, Order: 100, Enabled: true},
	}
	if engagementStage != nil {
		// Engagement 放在 LLM 之前——先决定是否参与，再生成回复
		stages = append([]core.StageInfo{
			{Stage: engagementStage, Order: 40, Enabled: true},
		}, stages...)
	}
	p, err := pipeline.New(
		stages,
		s.tp,
		s.mp,
		s.logger,
	)
	if err != nil {
		rollback()
		return errs.Wrap(err, "bot_service: create pipeline")
	}

	// 创建 Dispatcher（bot.New 内部会自动创建 handler 并注册）
	dispatcher := outbound.NewMultiDispatcher(s.logger, s.tp)

	// 创建 WebChannel（始终自动添加）
	webCh := NewWebChannel("web-"+id, id)

	// 从 DB 加载已启用的 Channel 定义并实例化
	channelDefs, err := s.ListEnabledChannelDefinitions(id)
	if err != nil {
		rollback()
		return errs.Wrap(err, "bot_service: load channel definitions")
	}

	allChannels := []bot.Channel{webCh}
	for _, cd := range channelDefs {
		ch, err := s.createChannel(cd)
		if err != nil {
			s.logger.Warnw("failed to create channel, skipping",
				"channel_def_id", cd.ID, "type", cd.Type, "err", err)
			continue
		}
		allChannels = append(allChannels, ch)
		s.logger.Infow("channel created", "type", cd.Type, "name", cd.Name)
	}

	// 创建梦境巩固子系统（如果配置了）
	var dreamScheduler *cron.Scheduler
	var dreamBundle *bot.DreamingBundle
	dreamCfg := builder.GetDreamingConfig(id)
	if dreamCfg.Enabled {
		loc := builder.GetBotTimezoneLocation(id)
		cronFile := fmt.Sprintf("data/cron/%s_dream.json", id)

		dBundle := bot.NewDreamingBundle(
			memory.DreamConfig{
				Enabled:          dreamCfg.Enabled,
				Schedule:         dreamCfg.Schedule,
				JaccardThreshold: 0.9,
				MaxDreamTokens:   10000,
			},
			bundle.Main,          // 使用 bot 的主 LLM
			bundle.MainDef.Model, // 模型名从 bot 主模型配置读取
			loc,
			s.tp,
			s.logger,
			id,
			cronFile,
		)
		if dBundle != nil {
			dreamScheduler = dBundle.Scheduler
			dreamBundle = dBundle
			s.logger.Infow("dreaming enabled",
				"bot_id", id,
				"schedule", dreamCfg.Schedule)
		}
	}

	// 创建自适应 Engagement 组件（Bot 自我画像 → 动态参数映射）
	var adaptiveSyncer *engagement.AdaptiveEngagementSyncer
	var rejectionDetector *engagement.RejectionDetector

	// 从 config store 读取自适应开关配置
	adaptiveEnabled := s.store.GetBool(config.BotAdaptiveEngagementKey(id, "enabled"), false)
	adaptiveChannels := s.store.GetStringSlice(config.BotAdaptiveEngagementKey(id, "channels"), nil)

	// 从 SOUL.md 解析初始画像
	soulContent := def.SystemPrompt
	initialTraits := engagement.ParseSoulProfile(soulContent)

	// 创建自适应同步器
	adaptiveSyncer = engagement.NewAdaptiveEngagementSyncer(
		engagement.SyncerConfig{
			BotID:           id,
			InitialTraits:   initialTraits,
			GlobalEnabled:   adaptiveEnabled,
			EnabledChannels: adaptiveChannels,
		},
		s.tp,
		s.logger,
	)

	// 创建被无视检测器
	rejectionDetector = engagement.NewRejectionDetector(
		engagement.RejectionDetectorConfig{
			SilenceWindowSeconds: 120.0,
			StreakThreshold:      3,
			StreakDuration:       1 * time.Hour,
			ChannelType:          "",
			BotName:              def.Name,
		},
		s.tp,
		s.logger,
	)

	// 将 BotProfileProfiler 注入 DreamManager（如果启用了梦境）
	if dreamBundle != nil {
		botProfiler := memory.NewBotProfileProfiler(
			memory.BotProfileProfilerConfig{
				Provider: bundle.Main,
				Model:    &llm.Model{ID: bundle.MainDef.Model},
			},
			s.tp,
			s.logger,
		)
		dreamBundle.Manager.SetBotProfiler(botProfiler)
		dreamBundle.BotProfiler = botProfiler

		// 回调：画像更新后同步到 AdaptiveEngagementSyncer
		dreamBundle.Manager.SetOnBotProfileUpdated(func(botID string, result *memory.BotProfileResult) {
			if result == nil {
				return
			}
			adaptiveSyncer.UpdateTraits(engagement.BotProfileTraits{
				EnergyLevel:     result.EnergyLevel,
				Patience:        result.Patience,
				PreferredTopics: result.PreferredTopics,
				Verbosity:       result.Verbosity,
				Personality:     result.Personality,
				Confidence:      result.Confidence,
			})
			s.logger.Infow("adaptive engagement synced from dreaming",
				"bot_id", botID,
				"personality", result.Personality,
				"energy", result.EnergyLevel)
		})

		s.logger.Infow("bot profile profiler wired into dreaming",
			"bot_id", id)
	}

	// 创建 Bot
	botCfg := bot.BotConfig{
		Workers:      def.Workers,
		SystemPrompt: def.SystemPrompt,
		Model:        def.Model,
	}
	if def.Temperature > 0 {
		t := def.Temperature
		botCfg.Temperature = &t
	}
	if def.MaxTokens > 0 {
		botCfg.MaxTokens = def.MaxTokens
	}

	// 梦境开启时桥接 NoteHandler 写入到分层存储
	//   NoteHandler → MultiStore → MemoryRepository (检索) + TieredStore (梦境管线)
	var memStore memory.Store
	if dreamBundle != nil {
		tieredAdapter := memory.NewTieredStoreAdapter(dreamBundle.TieredStore)
		// ThinkFilterStore 在写入前清理 <think> 标签
		filtered := memory.NewThinkFilterStore(tieredAdapter)
		repo := memory.NewMemoryRepository()
		memStore = memory.NewMultiStore(filtered, repo)
	}

	b, err := bot.New(bot.BotParams{
		ID:                id,
		Name:              def.Name,
		Config:            botCfg,
		Pipeline:          p,
		Dispatcher:        dispatcher,
		Channels:          allChannels,
		EventBus:          s.eventBus,
		MemoryStore:       memStore,
		Logger:            s.logger,
		TP:                s.tp,
		DreamScheduler:    dreamScheduler,
		SelfIDSet:         selfIDSet,
		PromptRegistry:    promptReg,
		ToolManager:       toolMgr,
		AdaptiveSyncer:    adaptiveSyncer,
		RejectionDetector: rejectionDetector,
	})
	if err != nil {
		rollback()
		return errs.Wrap(err, "bot_service: create bot")
	}

	// Wire BurstBuffer reenqueue——需要 bot 创建后才能访问 Ingress
	if engagementStage != nil && burstBuf != nil {
		engagementStage.WithBurstBuffer(burstBuf, func(env *core.Envelope) {
			if err := b.Ingress().Receive(context.Background(), env.Message); err != nil {
				s.logger.Warnw("engagement: burst buffer reenqueue failed",
					"message_id", env.Message.ID, "err", err)
			}
		})
	}

	// 接线自适应 Engagement：TimingGate + AdaptiveSyncer + RejectionDetector
	if engagementStage != nil && engagementStage.TimingGate() != nil {
		gate := engagementStage.TimingGate()

		// 注入动态配置回调 + 开启随机噪声（只在启用自适应时生效）
		if adaptiveSyncer != nil {
			gate.SetDynamicConfig(adaptiveSyncer.GetTimingConfigOverride)
			gate.SetRandomNoiseRate(0.08) // 8% 随机跨界参与，模拟真人灵光乍现
			s.logger.Infow("adaptive engagement: dynamic config wired to timing gate", "bot_id", id)
		}

		// 注入被无视检测器
		if rejectionDetector != nil {
			gate.SetRejectionDetector(rejectionDetector)
			s.logger.Infow("adaptive engagement: rejection detector wired to timing gate", "bot_id", id)
		}
	}

	// 联动 SoulLoader → AdaptiveSyncer：
	// Bot 内部有 SoulLoader 实时加载 SOUL.md。将真实 SOUL.md 内容作为
	// 初始画像种子（覆盖 def.SystemPrompt 的 fallback 解析），
	// 并接线热重载回调。
	if adaptiveSyncer != nil && b.SoulLoader() != nil && b.SoulLoader().Loaded() {
		soulContent := b.SoulLoader().Content()
		if soulContent != "" {
			realTraits := engagement.ParseSoulProfile(soulContent)
			adaptiveSyncer.UpdateTraits(realTraits)
			s.logger.Infow("adaptive engagement: synced from actual SOUL.md",
				"bot_id", id,
				"personality", realTraits.Personality,
				"energy", realTraits.EnergyLevel,
				"confidence", realTraits.Confidence)
		}

		// 热重载联动：SOUL.md 变更后自动重新解析画像
		b.SoulLoader().SetOnReload(func(content string) {
			traits := engagement.ParseSoulProfile(content)
			adaptiveSyncer.UpdateTraits(traits)
			s.logger.Infow("adaptive engagement: profile updated from SOUL.md hot-reload",
				"bot_id", id,
				"personality", traits.Personality,
				"energy", traits.EnergyLevel)
		})
		s.logger.Infow("adaptive engagement: SoulLoader hot-reload wired", "bot_id", id)
	}

	// 注册到 BotManager
	if err := s.mgr.Register(b); err != nil {
		b.Close()
		rollback()
		return errs.Wrap(err, "bot_service: register bot")
	}

	// 用独立 context 启动 Bot，避免 HTTP 请求结束后 ctx 被取消导致 Bot 立即关闭
	botCtx, botCancel := context.WithCancel(context.Background())

	// 启动 RunJournal 后台 flush goroutine
	go journal.Run(botCtx)

	// 启动 Bot（bot.Run 内部会自动注册实现 Sender 接口的 Channel）
	go func() {
		defer func() {
			if r := recover(); r != nil {
				s.logger.Errorw("bot run panic", "bot_id", id, "err", r)
			}
		}()
		if err := b.Run(botCtx); err != nil {
			s.logger.Errorw("bot run failed", "bot_id", id, "err", err)
		}
	}()

	// 等待 Bot 就绪（带 30s 超时，防止永久挂起）
	// 注意：仍然监听 HTTP 请求的 ctx.Done()，但仅用于中断等待，不取消 Bot
	readyTimeout := time.NewTimer(30 * time.Second)
	defer readyTimeout.Stop()
	select {
	case <-b.Ready():
		s.logger.Infow("bot started", "bot_id", id, "channels", len(allChannels))
	case <-readyTimeout.C:
		botCancel()
		journalCleanup(context.Background())
		b.Stop()
		b.Close()
		s.mgr.Unregister(id)
		rollback()
		return errs.Internal("bot_service: bot startup timeout (30s)")
	case <-ctx.Done():
		botCancel()
		journalCleanup(context.Background())
		b.Stop()
		b.Close()
		s.mgr.Unregister(id)
		rollback()
		return errs.Wrap(ctx.Err(), "bot_service: context cancelled")
	}

	s.mu.Lock()
	s.channels[id] = webCh
	s.botInstances[id] = b
	s.cancelFuncs[id] = botCancel
	s.closeFuncs[id] = func() {
		wfCleanup()
		journalCleanup(context.Background())
	}
	if dreamBundle != nil {
		s.dreamingBundles[id] = dreamBundle
	}
	s.mu.Unlock()

	// 更新定义状态
	if err := s.db.Model(&dao.BotDefinition{}).Where("id = ?", id).Update("status", dao.BotStatusRunning).Error; err != nil {
		s.logger.Warnw("failed to update bot status to running", "bot_id", id, "err", err)
	}

	return nil
}

// StopBot 停止运行中的 Bot。
func (s *BotService) StopBot(id string) {
	s.mu.Lock()
	b, exists := s.botInstances[id]
	delete(s.botInstances, id)
	delete(s.channels, id)
	if cancel, ok := s.cancelFuncs[id]; ok {
		cancel()
		delete(s.cancelFuncs, id)
	}
	if closeFn, ok := s.closeFuncs[id]; ok {
		closeFn()
		delete(s.closeFuncs, id)
	}
	if dreamBundle, ok := s.dreamingBundles[id]; ok {
		dreamBundle.Stop()
		delete(s.dreamingBundles, id)
	}
	s.mu.Unlock()

	if !exists || b == nil {
		return
	}

	b.Stop()
	b.Close()
	s.mgr.Unregister(id)

	if err := s.db.Model(&dao.BotDefinition{}).Where("id = ?", id).Update("status", dao.BotStatusStopped).Error; err != nil {
		s.logger.Warnw("failed to update bot status to stopped", "bot_id", id, "err", err)
	}
	s.logger.Infow("bot stopped", "bot_id", id)
}

// GetWebChannel 返回指定 Bot 的 WebChannel（供 SSE 聊天使用）。
func (s *BotService) GetWebChannel(botID string) (*WebChannel, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ch, ok := s.channels[botID]
	return ch, ok
}

// IsRunning 返回 Bot 是否正在运行。
func (s *BotService) IsRunning(id string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, ok := s.botInstances[id]
	return ok && b != nil
}

// RunningCount 返回当前运行中的 Bot 数量。
func (s *BotService) RunningCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	count := 0
	for _, b := range s.botInstances {
		if b != nil {
			count++
		}
	}
	return count
}

// StartAll 从 DB 加载所有定义并启动状态为 running 的 Bot。
func (s *BotService) StartAll(ctx context.Context) error {
	var defs []dao.BotDefinition
	if err := s.db.Where("status = ?", dao.BotStatusRunning).Find(&defs).Error; err != nil {
		return errs.Wrap(err, "bot_service: load running bots")
	}

	for _, def := range defs {
		if err := s.StartBot(ctx, def.ID); err != nil {
			s.logger.Errorw("failed to start bot on boot",
				"bot_id", def.ID, "err", err)
		}
	}

	if len(defs) > 0 {
		s.logger.Infow("started bots from DB", "count", len(defs))
	}
	return nil
}

// GetBotInfo 返回 Bot 信息。
func (s *BotService) GetBotInfo(id string) (*bot.BotInfo, error) {
	for _, info := range s.mgr.Info() {
		if info.ID == id {
			return &info, nil
		}
	}
	return nil, fmt.Errorf("bot %q not found or not running", id)
}

// --- ChannelDefinition CRUD ---

// ListChannelDefinitions 返回指定 Bot 的所有 Channel 定义。
func (s *BotService) ListChannelDefinitions(botID string) ([]dao.ChannelDefinition, error) {
	var defs []dao.ChannelDefinition
	if err := s.db.Where("bot_id = ?", botID).Order("created_at ASC").Find(&defs).Error; err != nil {
		return nil, errs.Wrap(err, "bot_service: list channel definitions")
	}
	return defs, nil
}

// ListEnabledChannelDefinitions 返回指定 Bot 已启用的 Channel 定义。
func (s *BotService) ListEnabledChannelDefinitions(botID string) ([]dao.ChannelDefinition, error) {
	var defs []dao.ChannelDefinition
	if err := s.db.Where("bot_id = ? AND enabled = ?", botID, true).Order("created_at ASC").Find(&defs).Error; err != nil {
		return nil, errs.Wrap(err, "bot_service: list enabled channel definitions")
	}
	return defs, nil
}

// CreateChannelDefinition 创建 Channel 定义。
func (s *BotService) CreateChannelDefinition(botID, name, channelType, configJSON string) (*dao.ChannelDefinition, error) {
	def := &dao.ChannelDefinition{
		BotID:   botID,
		Name:    name,
		Type:    channelType,
		Config:  configJSON,
		Enabled: true,
	}
	if err := s.db.Create(def).Error; err != nil {
		return nil, errs.Wrap(err, "bot_service: create channel definition")
	}
	return def, nil
}

// UpdateChannelDefinition 更新 Channel 定义。
func (s *BotService) UpdateChannelDefinition(botID, channelID string, req UpdateChannelReq) (*dao.ChannelDefinition, error) {
	var def dao.ChannelDefinition
	if err := s.db.Where("id = ? AND bot_id = ?", channelID, botID).First(&def).Error; err != nil {
		return nil, errs.NotFound("channel definition not found")
	}

	updates := map[string]any{}
	if req.Name != nil {
		updates["name"] = *req.Name
	}
	if req.Config != nil {
		updates["config"] = *req.Config
	}
	if req.Enabled != nil {
		updates["enabled"] = *req.Enabled
	}

	if len(updates) > 0 {
		if err := s.db.Model(&def).Updates(updates).Error; err != nil {
			return nil, errs.Wrap(err, "bot_service: update channel definition")
		}
		// 重新查询以获取更新后的值（Updates(map) 不会回写结构体字段）
		if err := s.db.Where("id = ? AND bot_id = ?", channelID, botID).First(&def).Error; err != nil {
			return nil, errs.Wrap(err, "bot_service: reload channel definition after update")
		}
	}

	// 如果 Bot 正在运行且 Channel 配置变更，提示需重启
	if s.IsRunning(botID) {
		s.logger.Infow("channel definition updated, bot restart recommended", "bot_id", botID, "channel_id", channelID)
	}

	return &def, nil
}

// DeleteChannelDefinition 删除 Channel 定义。
func (s *BotService) DeleteChannelDefinition(botID, channelID string) error {
	result := s.db.Where("id = ? AND bot_id = ?", channelID, botID).Delete(&dao.ChannelDefinition{})
	if result.Error != nil {
		return errs.Wrap(result.Error, "bot_service: delete channel definition")
	}
	if result.RowsAffected == 0 {
		return errs.NotFound("channel definition not found")
	}
	return nil
}

// --- Channel 工厂 ---

// createChannel 根据 ChannelDefinition 创建 Channel 实例。
func (s *BotService) createChannel(def dao.ChannelDefinition) (bot.Channel, error) {
	switch def.Type {
	case "telegram":
		return s.createTelegramChannel(def)
	case "misskey":
		return s.createMisskeyChannel(def)
	default:
		return nil, fmt.Errorf("unsupported channel type: %s", def.Type)
	}
}

// createTelegramChannel 从 ChannelDefinition 创建 Telegram Channel。
func (s *BotService) createTelegramChannel(def dao.ChannelDefinition) (bot.Channel, error) {
	var raw map[string]any
	if def.Config != "" && def.Config != "{}" {
		if err := json.Unmarshal([]byte(def.Config), &raw); err != nil {
			return nil, fmt.Errorf("invalid telegram config JSON: %w", err)
		}
	}

	cfg := telegram.Config{}
	if v, ok := raw["token"]; ok {
		if s, ok := v.(string); ok {
			cfg.Token = s
		}
	}
	if cfg.Token == "" {
		return nil, fmt.Errorf("telegram channel: token is required")
	}
	if v, ok := raw["pollTimeout"]; ok {
		cfg.PollTimeout = toInt(v)
	}
	if v, ok := raw["apiBaseUrl"]; ok {
		if s, ok := v.(string); ok {
			cfg.APIBaseURL = s
		}
	}
	if v, ok := raw["parseMode"]; ok {
		if s, ok := v.(string); ok {
			cfg.ParseMode = s
		}
	}
	if v, ok := raw["allowedUpdates"]; ok {
		if s, ok := v.(string); ok && s != "" {
			cfg.AllowedUpdates = strings.Split(s, ",")
		}
	}

	return telegram.NewChannel(def.Name, def.BotID, cfg), nil
}

// createMisskeyChannel 从 ChannelDefinition 创建 Misskey Channel。
func (s *BotService) createMisskeyChannel(def dao.ChannelDefinition) (bot.Channel, error) {
	var raw map[string]any
	if def.Config != "" && def.Config != "{}" {
		if err := json.Unmarshal([]byte(def.Config), &raw); err != nil {
			return nil, fmt.Errorf("invalid misskey config JSON: %w", err)
		}
	}

	cfg := misskey.Config{}
	if v, ok := raw["host"]; ok {
		if s, ok := v.(string); ok {
			cfg.Host = s
		}
	}
	if cfg.Host == "" {
		return nil, fmt.Errorf("misskey channel: host is required")
	}
	if v, ok := raw["token"]; ok {
		if s, ok := v.(string); ok {
			cfg.Token = s
		}
	}
	if cfg.Token == "" {
		return nil, fmt.Errorf("misskey channel: token is required")
	}
	if v, ok := raw["subscribeTimeline"]; ok {
		if b, ok := v.(bool); ok {
			cfg.SubscribeTimeline = b
		}
	}

	return misskey.NewChannel(def.Name, def.BotID, cfg), nil
}

// toInt 将 interface{} 安全转换为 int。
func toInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case string:
		if i, err := strconv.Atoi(n); err == nil {
			return i
		}
	}
	return 0
}

// --- 子系统访问器 ---

// GetDreamingBundle 返回指定 Bot 的梦境巩固子系统（如果已启用）。
func (s *BotService) GetDreamingBundle(botID string) (*bot.DreamingBundle, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	bundle, ok := s.dreamingBundles[botID]
	return bundle, ok
}

// GetCronManager 为指定 Bot 创建 cron.Manager（从 cron store 文件加载）。
func (s *BotService) GetCronManager(botID string) *cron.Manager {
	builder := config.NewBuilder(s.store, s.logger)
	loc := builder.GetBotTimezoneLocation(botID)
	cronFile := fmt.Sprintf("data/cron/%s_cron.json", botID)
	store := cron.NewStore(cronFile)
	return cron.NewManager(store, loc).WithLogger(s.logger)
}

// CreateLLMProvider 从配置创建 LLM Provider（用于 workflow 等全局子系统）。
// 选择第一个配置了 LLM 的 Bot 作为 provider 来源。
func (s *BotService) CreateLLMProvider() (llm.Provider, string, error) {
	builder := config.NewBuilder(s.store, s.logger)
	defs, err := s.ListDefinitions()
	if err != nil {
		return nil, "", errs.Wrap(err, "list definitions for LLM")
	}
	for _, def := range defs {
		bundle, err := bot.CreateLLMBundle(builder, def.ID)
		if err != nil {
			continue
		}
		return bundle.Main, bundle.MainDef.Model, nil
	}
	return nil, "", errs.New("no LLM provider available — configure at least one bot with an LLM")
}

// EventBus 返回事件总线。
func (s *BotService) EventBus() outbound.EventBus {
	return s.eventBus
}
