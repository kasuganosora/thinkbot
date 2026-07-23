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
	"github.com/kasuganosora/thinkbot/sandbox"
	"github.com/kasuganosora/thinkbot/subagent"
	"github.com/kasuganosora/thinkbot/tools"
	"github.com/kasuganosora/thinkbot/util/errs"
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
	db            *gorm.DB
	store         *config.Store
	mgr           *bot.BotManager
	logger        *zap.SugaredLogger
	tp            trace.TracerProvider
	mp            metric.MeterProvider
	eventBus      outbound.EventBus
	statsRecorder llm.UsageRecorder // 可选，nil 时不记录 token 统计

	mu              sync.RWMutex
	channels        map[string]*WebChannel         // botID → WebChannel
	botInstances    map[string]*bot.Bot            // botID → running Bot
	dreamingBundles map[string]*bot.DreamingBundle // botID → DreamingBundle
	cancelFuncs     map[string]context.CancelFunc  // botID → bot context cancel
	closeFuncs      map[string]func()              // botID → sub-agent managers cleanup
	messageCancels  map[string]context.CancelFunc  // "botID:traceID" → message context cancel

	tokenBudget *pipeline.TokenBudgetState // 共享 token 预算状态（支持空闲自动重置 / 手动重置）
}

// NewBotService 创建 BotService。
func NewBotService(db *gorm.DB, store *config.Store, mgr *bot.BotManager, logger *zap.SugaredLogger, tp trace.TracerProvider, mp metric.MeterProvider, eventBus outbound.EventBus, statsRecorder llm.UsageRecorder) *BotService {
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
		statsRecorder:   statsRecorder,
		channels:        make(map[string]*WebChannel),
		botInstances:    make(map[string]*bot.Bot),
		dreamingBundles: make(map[string]*bot.DreamingBundle),
		cancelFuncs:     make(map[string]context.CancelFunc),
		closeFuncs:      make(map[string]func()),
		messageCancels:  make(map[string]context.CancelFunc),

		// token 预算状态：空闲 1 小时后自动清零，防止预算永久卡死导致 bot 无响应；
		// 也可通过 ResetTokenBudgets() 手动重置。
		tokenBudget: pipeline.NewTokenBudgetState(time.Hour),
	}
}

// --- BotDefinition CRUD ---

// RunningBotWorkspaceMgr 返回指定 bot 运行时的工作空间管理器（若 bot 正在运行且已启用）。
// 文件管理 API 借此复用与运行时完全一致的后端（docker 持久容器 / local）。
func (s *BotService) RunningBotWorkspaceMgr(botID string) (*sandbox.BotWorkspaceManager, bool) {
	s.mu.RLock()
	b, ok := s.botInstances[botID]
	s.mu.RUnlock()
	if !ok || b == nil {
		return nil, false
	}
	mgr := b.WorkspaceMgr()
	if mgr == nil {
		return nil, false
	}
	return mgr, true
}

// ResolveWorkspace 返回指定 bot 的工作空间兼容层入口（docker/local 都返回）。
//
// 这是终端执行、agent shell/list_files 工具统一的执行出口：调用方只面向
// sandbox.Workspace.Exec / ListDir 等接口，完全不感知底层是 docker 容器还是
// 物理机进程 —— 有 Docker 环境走容器隔离，无则走宿主机本地进程。
//
// 与 botFileWorkspace（api/handler_bot_detail.go，仅 docker 模式返回）不同，
// 本方法在 local 模式下也会返回一个可用的 workspace。
func (s *BotService) ResolveWorkspace(botID string) (sandbox.Workspace, error) {
	if botID == "" {
		return nil, errs.New("bot_service: botID is required")
	}
	mgr, err := s.WorkspaceManagerForBot(botID)
	if err != nil {
		return nil, err
	}
	return mgr.GetOrCreate(botID)
}

// WorkspaceManagerForBot 返回用于该 bot 的工作空间管理器：
// 运行中优先复用运行时管理器（状态一致），否则按当前配置临时构造。
func (s *BotService) WorkspaceManagerForBot(botID string) (*sandbox.BotWorkspaceManager, error) {
	if botID == "" {
		return nil, errs.New("bot_service: botID is required")
	}
	if mgr, ok := s.RunningBotWorkspaceMgr(botID); ok {
		return mgr, nil
	}
	mgr, err := sandbox.NewBotWorkspaceManager(s.GetWorkspaceBaseDir(), s.SandboxConfigForBot(), s.logger)
	if err != nil {
		return nil, errs.Wrap(err, "bot_service: build workspace manager")
	}
	return mgr, nil
}

// SandboxConfigForBot 构造文件管理 API 使用的 sandbox 配置，与运行时保持一致。
// Backend 由 config 决定（默认 auto：有 Docker 则容器隔离，否则 local）。
// 不带 per-bot 内存覆盖，使用系统默认（2G）。
func (s *BotService) SandboxConfigForBot() sandbox.Config {
	return s.sandboxConfigWithMemoryLimit(-1)
}

// sandboxConfigWithMemoryLimit 构造基础 sandbox 配置并按 mb 决定内存限制。
//   - mb > 0：限制为该 MB 数（如 "2048m"）。
//   - mb == 0：不限制（docker run 不加 --memory）。
//   - mb < 0：使用系统默认（DefaultConfig 中的 2G）—— 用于无 per-bot 上下文的场景。
func (s *BotService) sandboxConfigWithMemoryLimit(mb int64) sandbox.Config {
	builder := config.NewBuilder(s.store, s.logger)
	cfg := sandbox.DefaultConfig()
	cfg.Timezone = builder.GetTimezone()
	if img := s.store.GetString(config.KeySandboxImage, ""); img != "" {
		cfg.Image = img
	}
	if backend := s.store.GetString(config.KeySandboxBackend, ""); backend != "" {
		cfg.Backend = backend
	}
	// auto 模式下是否强制要求 Docker 可用：避免 DooD 部署探测失败时静默降级 local 裸跑。
	if s.store.GetString(config.KeySandboxRequireDocker, "") == "true" {
		cfg.RequireDocker = true
	}
	// 单条命令硬上限（秒）。默认 0 表示自动 = 卡死阈值 × 3（默认即 15 分钟），
	// 不写死固定时长；设为正整数时显式覆盖。作为卡死看门狗的最终兜底，
	// 避免 go build / go install / golangci-lint run 等耗时命令被无限挂起。
	// 注意：命令默认不再用固定超时一刀切杀掉，真正判定「卡死」的是 StuckTimeout 看门狗。
	if sec := s.store.GetInt(config.KeySandboxTimeout, 0); sec > 0 {
		cfg.Timeout = time.Duration(sec) * time.Second
	}
	// 卡死看门狗阈值（秒）。默认 300（5 分钟）：命令连续无输出超过该时长即判定卡死并终止。
	// 区分「编译慢（持续输出）」与「死锁卡死（无输出）」。
	if sec := s.store.GetInt(config.KeySandboxStuckTimeout, 300); sec > 0 {
		cfg.StuckTimeout = time.Duration(sec) * time.Second
	}
	switch {
	case mb > 0:
		cfg.MemoryLimit = fmt.Sprintf("%dm", mb)
	case mb == 0:
		cfg.MemoryLimit = "" // 0 = 不限制
	default: // mb < 0：保留 DefaultConfig 的 2G 默认
	}
	return cfg
}

// GetWorkspaceBaseDir 返回 bot 工作空间根目录。
func (s *BotService) GetWorkspaceBaseDir() string {
	return config.NewBuilder(s.store, s.logger).GetWorkspaceDir()
}

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

func messageCancelKey(botID, traceID string) string {
	return botID + ":" + traceID
}

// RegisterMessageCancel 注册一条正在执行消息的取消函数（botID+traceID 维度）。
func (s *BotService) RegisterMessageCancel(botID, traceID string, cancel context.CancelFunc) {
	if botID == "" || traceID == "" || cancel == nil {
		return
	}
	key := messageCancelKey(botID, traceID)
	s.mu.Lock()
	s.messageCancels[key] = cancel
	s.mu.Unlock()
}

// UnregisterMessageCancel 注销一条消息取消函数。
func (s *BotService) UnregisterMessageCancel(botID, traceID string) {
	if botID == "" || traceID == "" {
		return
	}
	key := messageCancelKey(botID, traceID)
	s.mu.Lock()
	delete(s.messageCancels, key)
	s.mu.Unlock()
}

// AbortMessage 取消一条正在执行的消息。
// 返回 true 表示找到了对应执行并发起取消，false 表示未命中（可能已结束）。
func (s *BotService) AbortMessage(botID, traceID string) bool {
	if botID == "" || traceID == "" {
		return false
	}
	key := messageCancelKey(botID, traceID)
	s.mu.Lock()
	cancel, ok := s.messageCancels[key]
	if ok {
		delete(s.messageCancels, key)
	}
	s.mu.Unlock()
	if !ok || cancel == nil {
		return false
	}
	cancel()
	return true
}

// ResetTokenBudgets 重置所有 channel 的 token 预算追踪。
// 当某 channel 累计 token 超过硬限制后，Pipeline 会在每次请求前直接中止，
// 若不重置则该 channel 将永久拒绝新消息。空闲 1 小时会自动清零，但手动重置可立即恢复。
func (s *BotService) ResetTokenBudgets() {
	s.tokenBudget.ResetAll()
}

// 工具调用步数预算的全局默认值。
// soft：常规任务在此内自然收尾；hard：复杂任务持续产生新调用时自动延长至该安全网。
const (
	defaultSoftMaxSteps = 30
	defaultHardMaxSteps = 90 // = defaultSoftMaxSteps * 3
)

// effectiveStepBudgets 计算 per-bot 的工具调用步数预算。
//
// MaxSteps(soft)：
//   - <= 0（含未设置）→ 全局默认 defaultSoftMaxSteps(30)。
//
// HardMaxSteps(hard，面向用户「步数限制」设置)：
//   - > 0  → 有限硬上限（用户设置的具体步数）。
//   - == 0 → 不限制（无限）：用户未设上限，Bot 跑到任务完成为止，
//            不会因步数预算耗尽被腰斩。这是默认行为。
//   - < 0  → 内置默认安全网（soft * defaultHardMultiplier），历史/内部语义。
//
// 返回值中 hard==0 表示不限制，交由 loopController 解释为无限。
func effectiveStepBudgets(def *dao.BotDefinition) (soft, hard int) {
	soft = def.MaxSteps
	if soft <= 0 {
		soft = defaultSoftMaxSteps
	}
	hard = def.HardMaxSteps
	switch {
	case hard > 0:
		// 有限硬上限，原样返回
	case hard == 0:
		// 不限制（无限）：返回 0，loopController 会解释为无限
	default: // hard < 0
		hard = soft * 3 // 内置默认安全网（soft × 3）
	}
	if hard > 0 && hard < soft {
		hard = soft
	}
	return soft, hard
}

// StartBot 从定义创建并启动 Bot 实例。
func (s *BotService) StartBot(ctx context.Context, id string) error {
	def, err := s.GetDefinition(id)
	if err != nil {
		return err
	}

	// 如果已在运行，先停止再重启（支持 Channel/配置热更新）
	s.mu.Lock()
	if inst, exists := s.botInstances[id]; exists && inst != nil {
		s.mu.Unlock()
		s.StopBot(id)
		// 重新获取锁
		s.mu.Lock()
	}
	if _, exists := s.botInstances[id]; exists {
		s.mu.Unlock()
		return errs.Conflict("bot is already starting, please wait")
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

	// 创建共享 Token 配额状态 + 包裹 Provider
	// - TokenQuotaState 由 pipeline 中间件和 QuotaRecordingProvider 共享
	// - QuotaRecordingProvider 拦截所有 LLM 调用（subagent、workflow、memory 等）
	//   通过 context 中的 dimension 自动记账配额
	// - StatsRecordingProvider 拦截所有 LLM 调用，自动记录到 stats_usage_daily
	//   feature 从 context 读取（WithStatsFeature），pipeline stages 通过
	//   WithStatsSkip 跳过以避免双重计数
	quotaState := pipeline.NewTokenQuotaState().WithStatsRecorder(s.statsRecorder)
	// 从 stats_usage_daily 恢复本月已用 token，防止重启后计数器归零绕过限额
	if err := quotaState.RestoreFromStats(syncCtx, s.db, id); err != nil {
		s.logger.Warnw("bot_service: failed to restore quota counters from stats", "bot_id", id, "err", err)
	}
	quotaRecorder := llm.QuotaUsageRecorder(quotaState.AddUsage)
	bundle.Main = llm.NewStatsRecordingProvider(
		llm.NewQuotaRecordingProvider(bundle.Main, quotaRecorder),
		s.statsRecorder, id,
	)
	if bundle.Light != nil {
		bundle.Light = llm.NewStatsRecordingProvider(
			llm.NewQuotaRecordingProvider(bundle.Light, quotaRecorder),
			s.statsRecorder, id,
		)
	}
	if bundle.Vision != nil {
		bundle.Vision = llm.NewStatsRecordingProvider(
			llm.NewQuotaRecordingProvider(bundle.Vision, quotaRecorder),
			s.statsRecorder, id,
		)
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
	// 注意：shell 命令执行与文件列举工具（sandbox_exec / sandbox_read_file 等）
	// 由 sandbox 包通过 BotWorkspaceManager 在 Bot 构造时统一注册，这里不再注册。
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
		ModelDef:       &bundle.MainDef,
		EventBus:       s.eventBus,
	})
	if err := workflow.RegisterTools(toolMgr, wfMgr); err != nil {
		s.logger.Warnw("failed to register workflow tools", "err", err)
	}

	// 注册 SubAgent 工具
	// 将当前模型的 MaxTokens 作为默认输出上限注入，避免 SubAgent 写死 4096：
	// 调用方未显式 WithMaxTokens 时，自动跟随模型配置（如 glm-5.2=128K）。
	saMgr := subagent.NewSubAgentManager(bundle.Main, bundle.MainDef.Model,
		subagent.WithMaxTokens(bundle.MainDef.MaxTokens))
	if err := subagent.RegisterTools(toolMgr, saMgr); err != nil {
		s.logger.Warnw("failed to register subagent tools", "err", err)
	}

	// 注册子代理清理回调，Bot 停止时释放 goroutine 和 LLM 连接
	wfCleanup := func() {
		wfSaMgr.CloseAll()
		saMgr.CloseAll()
	}

	// 计算 per-bot 工具调用步数预算（soft/hard）。DB 中为 0 时回退全局默认。
	// 该值后续同时用于 LLMStage 装配与 AgentConfig.MaxSteps，保证两处一致。
	softSteps, hardSteps := effectiveStepBudgets(def)

	llmStage := stages.NewLLMStage(
		"llm",
		bundle.Main,
		stages.LLMConfig{
			SystemPrompt:    "", // 由 PromptStage 从 SOUL.md 注入
			Model:           mainModel,
			Temperature:     temp,
			MaxTokens:       maxTok,
			ReasoningEffort: def.ReasoningEffort,
			MessageBuilder:  messageBuilder,
			ToolResolver:    toolMgr,
			// 动态步数预算：常规任务在 soft 内自然收尾；复杂任务（如大规模
			// 代码修复）在持续产生新工具调用时自动延长至 hard 安全网，
			// 陷入重复循环则提前停止。详见 llm.loopController。
			MaxSteps:        softSteps,
			HardMaxSteps:    hardSteps,
			StreamPublisher: s.eventBus,
			UsageRecorder:   s.statsRecorder, // 统一记账到 stats
			ReductionConfig: llm.DefaultReductionConfigPtr(),
		},
		s.tp,
		s.logger,
	)

	// 用安全中间件包装 LLMStage：
	//   执行顺序（从外到内）：Token 配额(月) → 循环检测 → Token 预算 → LLMStage
	//   TokenQuotaMiddlewareWithState 使用共享的 quotaState，使嵌套 LLM 调用
	//   （subagent、workflow、memory）也能通过 QuotaRecordingProvider 自动记账。
	quotaResolver := pipeline.NewQuotaResolver(s.store)
	wrappedLLM := pipeline.WithMiddleware(llmStage,
		pipeline.VerificationGateMiddleware(pipeline.NewVerificationGateConfig()),
		pipeline.TokenQuotaMiddlewareWithState(quotaResolver, quotaState, s.tp, s.logger),
		pipeline.LoopDetectionMiddleware(pipeline.NewLoopDetectionConfig()),
		pipeline.LazyResponseMiddleware(pipeline.NewLazyResponseConfig()),
		pipeline.TokenBudgetMiddlewareWithState(pipeline.NewTokenBudgetConfig().WithStatsRecorder(s.statsRecorder), s.tokenBudget),
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
				BotPersona: "", // 由 SOUL.md 提供
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

	// 从 config store 加载平台配置（前端 BotPlatforms 组件写入）并实例化为 Channel
	// 注意：旧的 DB ChannelDefinition 路径已废弃，统一使用 Platform API 管理
	var platforms []struct {
		ID      string         `json:"id"`
		Type    string         `json:"type"`
		Name    string         `json:"name"`
		Enabled bool           `json:"enabled"`
		Config  map[string]any `json:"config"`
	}
	if raw, ok := s.store.Get("bot." + id + ".detail.platforms"); ok && raw != "" {
		_ = json.Unmarshal([]byte(raw), &platforms)
	}

	allChannels := []bot.Channel{webCh}
	for _, p := range platforms {
		if !p.Enabled {
			continue
		}
		configJSON, _ := json.Marshal(p.Config)
		cd := dao.ChannelDefinition{
			BotID:   id,
			Name:    p.Name,
			Type:    p.Type,
			Config:  string(configJSON),
			Enabled: true,
		}
		ch, err := s.createChannel(cd)
		if err != nil {
			s.logger.Warnw("failed to create platform channel, skipping",
				"platform_id", p.ID, "type", p.Type, "err", err)
			continue
		}
		allChannels = append(allChannels, ch)
		s.logger.Infow("platform channel created", "type", p.Type, "name", p.Name)
	}

	// 注册 Channel 专属工具（每个 Channel 实现 ChannelToolProvider 接口）
	// 通过闭包持有 Channel API 客户端，支持跨 Channel 工具调用
	for _, ch := range allChannels {
		if ctp, ok := ch.(agenttools.ChannelToolProvider); ok {
			defs, err := ctp.ChannelTools(context.Background())
			if err != nil {
				s.logger.Warnw("failed to get channel tools",
					"channel_name", ch.Name(), "channel_type", ch.Type(), "err", err)
				continue
			}
			for _, def := range defs {
				if err := toolMgr.Register(def); err != nil {
					s.logger.Warnw("failed to register channel tool",
						"tool", def.Name, "channel", ch.Name(), "err", err)
				}
			}
			s.logger.Infow("channel tools registered",
				"channel_name", ch.Name(), "channel_type", ch.Type(), "count", len(defs))
		}
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
	initialTraits := engagement.ParseSoulProfile("")

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
		SystemPrompt: "", // 由 PromptStage 从 SOUL.md 注入
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

	// AgentConfig：读取 compaction 等运行时行为配置
	var agentCfg bot.AgentConfig
	// 让 AgentConfig.MaxSteps 与 LLMStage 实际使用的 soft 预算保持一致，
	// 避免该字段长期空洞（此前 DefaultAgentConfig 写死 10，与运行时脱节）。
	agentCfg.MaxSteps = softSteps
	if raw, ok := s.store.Get("bot." + id + ".detail.compaction"); ok && raw != "" {
		var cc struct {
			Enabled   bool `json:"enabled"`
			Threshold int  `json:"threshold"`
			Ratio     int  `json:"ratio"`
		}
		if err := json.Unmarshal([]byte(raw), &cc); err == nil && cc.Enabled {
			enabled := true
			agentCfg.CompactionEnabled = &enabled
		}
	}

	// 持久化工作空间接线：每个 bot 独立目录 {workspaceDir}/{botID}/。
	// 传入 WorkspaceDir 后，bot.New 会创建目录并注册工作空间文件工具。
	// SandboxConfig 留空 Backend 时，bot.New 会用 sandbox.DefaultConfig()（非 docker/local）。
	workspaceDir := builder.GetWorkspaceDir()
	sbCfg := sandbox.DefaultConfig()
	sbCfg.Timezone = builder.GetTimezone()

	b, err := bot.New(bot.BotParams{
		ID:                id,
		Name:              def.Name,
		Config:            botCfg,
		AgentConfig:       agentCfg,
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
		OnMessageStart: func(botID, traceID string, cancel context.CancelFunc) {
			s.RegisterMessageCancel(botID, traceID, cancel)
		},
		OnMessageDone: func(botID, traceID string) {
			s.UnregisterMessageCancel(botID, traceID)
		},
		WorkspaceDir:  workspaceDir,
		SandboxConfig: sbCfg,
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
	// Bot 内部有 SoulLoader 实时加载 SOUL.md，并接线热重载回调。
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
		b.Stop()
		b.Close()
		s.mgr.Unregister(id)
		rollback()
		return errs.Internal("bot_service: bot startup timeout (30s)")
	case <-ctx.Done():
		botCancel()
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
	pendingCancels := make([]context.CancelFunc, 0)
	prefix := id + ":"
	for key, cancel := range s.messageCancels {
		if strings.HasPrefix(key, prefix) {
			pendingCancels = append(pendingCancels, cancel)
			delete(s.messageCancels, key)
		}
	}
	s.mu.Unlock()
	for _, cancel := range pendingCancels {
		if cancel != nil {
			cancel()
		}
	}

	// 无论内存中是否存在 agent 实例，都确保 DB 状态置为 stopped。
	// （容器停止按钮也会调用本方法，需保证「任务状态」与容器状态一致。）
	if err := s.db.Model(&dao.BotDefinition{}).Where("id = ?", id).Update("status", dao.BotStatusStopped).Error; err != nil {
		s.logger.Warnw("failed to update bot status to stopped", "bot_id", id, "err", err)
	}

	if !exists || b == nil {
		return
	}

	b.Stop()
	b.Close()
	s.mgr.Unregister(id)

	s.logger.Infow("bot stopped", "bot_id", id)
}

// SetBotStatus 直接更新 Bot 的 DB 状态（不启停 agent 实例，仅修改持久化状态）。
// 供容器启动/停止按钮使用，保证 DB status 与容器实际状态一致。
func (s *BotService) SetBotStatus(id, status string) {
	if err := s.db.Model(&dao.BotDefinition{}).Where("id = ?", id).Update("status", status).Error; err != nil {
		s.logger.Warnw("failed to update bot status", "bot_id", id, "status", status, "err", err)
	}
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
	// 订阅的 timeline 频道（homeTimeline/localTimeline/hybridTimeline/globalTimeline，可多选）。
	if v, ok := raw["timelineChannels"]; ok {
		if arr, ok := v.([]any); ok {
			for _, item := range arr {
				if s, ok := item.(string); ok && s != "" {
					cfg.TimelineChannels = append(cfg.TimelineChannels, s)
				}
			}
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
func (s *BotService) CreateLLMProvider() (llm.Provider, string, *config.ModelDef, error) {
	builder := config.NewBuilder(s.store, s.logger)
	defs, err := s.ListDefinitions()
	if err != nil {
		return nil, "", nil, errs.Wrap(err, "list definitions for LLM")
	}
	for _, def := range defs {
		bundle, err := bot.CreateLLMBundle(builder, def.ID)
		if err != nil {
			continue
		}
		md := bundle.MainDef
		return bundle.Main, bundle.MainDef.Model, &md, nil
	}
	return nil, "", nil, errs.New("no LLM provider available — configure at least one bot with an LLM")
}

// EventBus 返回事件总线。
func (s *BotService) EventBus() outbound.EventBus {
	return s.eventBus
}
