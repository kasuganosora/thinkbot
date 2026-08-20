package api

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
	"gorm.io/gorm/clause"

	"github.com/kasuganosora/thinkbot/agent/bot"
	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/agent/engagement"
	"github.com/kasuganosora/thinkbot/agent/heartbeat"
	"github.com/kasuganosora/thinkbot/agent/inbound"
	"github.com/kasuganosora/thinkbot/agent/memory"
	"github.com/kasuganosora/thinkbot/agent/outbound"
	"github.com/kasuganosora/thinkbot/agent/pipeline"
	"github.com/kasuganosora/thinkbot/agent/prompt"
	"github.com/kasuganosora/thinkbot/agent/stages"
	"github.com/kasuganosora/thinkbot/agent/storage"
	agenttools "github.com/kasuganosora/thinkbot/agent/tools"
	"github.com/kasuganosora/thinkbot/channel/misskey"
	"github.com/kasuganosora/thinkbot/channel/telegram"
	"github.com/kasuganosora/thinkbot/config"
	"github.com/kasuganosora/thinkbot/cron"
	"github.com/kasuganosora/thinkbot/dao"
	"github.com/kasuganosora/thinkbot/llm"
	"github.com/kasuganosora/thinkbot/sandbox"
	"github.com/kasuganosora/thinkbot/subagent"
	"github.com/kasuganosora/thinkbot/toolperm"
	"github.com/kasuganosora/thinkbot/tools"
	"github.com/kasuganosora/thinkbot/util/errs"
	"github.com/kasuganosora/thinkbot/util/idgen"
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

	mu                sync.RWMutex
	channels          map[string]*WebChannel            // botID → WebChannel
	botInstances      map[string]*bot.Bot               // botID → running Bot
	toolManagers      map[string]*agenttools.ToolManager // botID → tool manager (for listing)
	dreamingBundles   map[string]*bot.DreamingBundle    // botID → DreamingBundle
	heartbeatBundles  map[string]*heartbeat.Bundle      // botID → HeartbeatBundle
	cancelFuncs       map[string]context.CancelFunc     // botID → bot context cancel
	closeFuncs        map[string]func()                 // botID → sub-agent managers cleanup
	messageCancels    map[string]context.CancelFunc     // "botID:traceID" → message context cancel
	messageInterrupts map[string]chan string            // "botID:traceID" → 用户中途追加通道（生成中补充）

	// wfEngines 保存每个已启动 bot 的**已装配工作区工具**的工作流引擎。
	//
	// 存在意义（2026-08-06 线上事故）：WorkflowService 原先自己 Setup 一个
	// ToolMgr=nil 的引擎来跑 Recover / Sweeper / UI 重试。进程重启后 Recover
	// 接管工作流，节点执行的 SubAgent 拿不到工作区工具 → 产出从 5000~10000字的
	// 真实审查报告退化成 48~117 字的「我将先探索项目结构…」纯计划，
	// 且照样被 review 判 completed（空壳产物混进终态，等于静默丢工作成果）。
	// → WorkflowService 必须优先复用这里的引擎，而不是自建残废实例。
	wfEngines map[string]*workflow.Manager // botID → 已装配 ToolMgr 的工作流引擎

	// chatHistory 用于续跑注入时加载会话历史并落库续跑指令。
	chatHistory *ChatHistoryService

	tokenBudget *pipeline.TokenBudgetState // 共享 token 预算状态（支持空闲自动重置 / 手动重置）

	// permSvc bot 工具权限服务（按 bot 维度控制工具可用性）。
	permSvc *toolperm.Service

	// heartbeatStore 心跳配置/日志存储。由 BotService 持有并共享给 API Server，
	// 保证「运行时执行器写日志」与「HTTP 读写配置」用的是同一把 per-bot 锁。
	heartbeatStore *heartbeat.Store
}

// NewBotService 创建 BotService。
func NewBotService(db *gorm.DB, store *config.Store, mgr *bot.BotManager, logger *zap.SugaredLogger, tp trace.TracerProvider, mp metric.MeterProvider, eventBus outbound.EventBus, statsRecorder llm.UsageRecorder, chatHistory *ChatHistoryService, permSvc *toolperm.Service) *BotService {
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
		db:                db,
		store:             store,
		mgr:               mgr,
		logger:            logger.With("component", "bot_service"),
		tp:                tp,
		mp:                mp,
		eventBus:          eventBus,
		statsRecorder:     statsRecorder,
		channels:          make(map[string]*WebChannel),
		botInstances:      make(map[string]*bot.Bot),
		dreamingBundles:   make(map[string]*bot.DreamingBundle),
		heartbeatBundles:  make(map[string]*heartbeat.Bundle),
		cancelFuncs:       make(map[string]context.CancelFunc),
		closeFuncs:        make(map[string]func()),
		messageCancels:    make(map[string]context.CancelFunc),
		messageInterrupts: make(map[string]chan string),
		wfEngines:         make(map[string]*workflow.Manager),
		chatHistory:       chatHistory,

		// token 预算状态：空闲 1 小时后自动清零，防止预算永久卡死导致 bot 无响应；
		// 也可通过 ResetTokenBudgets() 手动重置。
		tokenBudget:    pipeline.NewTokenBudgetState(time.Hour),
		permSvc:        permSvc,
		toolManagers:   make(map[string]*agenttools.ToolManager),
		heartbeatStore: heartbeat.NewStore("data/heartbeat"),
	}
}

// HeartbeatStore 返回心跳存储，供 API Server 复用同一实例（共享 per-bot 锁）。
func (s *BotService) HeartbeatStore() *heartbeat.Store {
	if s == nil {
		return nil
	}
	return s.heartbeatStore
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

	// 清理该 bot 的浏览器 cookie（凭据残留，删除 bot 后不应遗留）。
	if err := s.db.Where("bot_id = ?", id).Delete(&dao.BotBrowserCookie{}).Error; err != nil {
		s.logger.Warnw("delete bot browser cookies failed", "bot_id", id, "err", err)
	}

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

// RegisterMessageInterrupt 注册一条正在执行消息的「用户中途追加」通道。
func (s *BotService) RegisterMessageInterrupt(botID, traceID string, ch chan string) {
	if botID == "" || traceID == "" || ch == nil {
		return
	}
	key := messageCancelKey(botID, traceID)
	s.mu.Lock()
	s.messageInterrupts[key] = ch
	s.mu.Unlock()
}

// UnregisterMessageInterrupt 注销一条消息的「用户中途追加」通道。
func (s *BotService) UnregisterMessageInterrupt(botID, traceID string) {
	if botID == "" || traceID == "" {
		return
	}
	key := messageCancelKey(botID, traceID)
	s.mu.Lock()
	delete(s.messageInterrupts, key)
	s.mu.Unlock()
}

// AppendToMessage 在一条正在执行的消息（botID+traID）生成过程中，把用户
// 中途补充的内容追加进同一轮对话（Claude-CLI 风格的「边思考/边输出边补充」）。
//
// 返回 true 表示成功投递（本轮仍在执行、已被接管）；返回 false 表示本轮已
// 结束或不存在，调用方应退化为一次普通的 /send 新消息。
func (s *BotService) AppendToMessage(botID, traceID, text string) bool {
	if botID == "" || traceID == "" || strings.TrimSpace(text) == "" {
		return false
	}
	key := messageCancelKey(botID, traceID)
	s.mu.Lock()
	ch, ok := s.messageInterrupts[key]
	s.mu.Unlock()
	if !ok {
		return false
	}
	select {
	case ch <- text:
		return true
	default:
		// 缓冲已满（极端情况）：丢弃避免阻塞，调用方退化为普通发送。
		s.logger.Warnw("message interrupt channel full, drop append",
			"botID", botID, "traceID", traceID)
		return false
	}
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

// ActiveMessageTraceIDs 返回指定 bot 当前仍在执行（未结束）的消息 traceID 列表。
// 用于前端重连后恢复对「断连但后台仍运行」的任务的订阅与终止能力：
// 用户关页面后后台长任务继续跑，其 cancel 仍注册在 messageCancels 中，直到消息真正完成
// （OnMessageDone 注销）。前端据此知道哪些 traceID 仍可 resume / abort。
func (s *BotService) ActiveMessageTraceIDs(botID string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	prefix := botID + ":"
	out := make([]string, 0, len(s.messageCancels))
	for key := range s.messageCancels {
		if strings.HasPrefix(key, prefix) {
			out = append(out, strings.TrimPrefix(key, prefix))
		}
	}
	return out
}

// ResetTokenBudgets 重置所有 channel 的 token 预算追踪。
// 当某 channel 累计 token 超过硬限制后，Pipeline 会在每次请求前直接中止，
// 若不重置则该 channel 将永久拒绝新消息。空闲 1 小时会自动清零，但手动重置可立即恢复。
func (s *BotService) ResetTokenBudgets() {
	s.tokenBudget.ResetAll()
}

// publishWorkflowEngine 登记某bot 已装配工作区工具的工作流引擎。
func (s *BotService) publishWorkflowEngine(botID string, mgr *workflow.Manager) {
	if mgr == nil {
		return
	}
	s.mu.Lock()
	s.wfEngines[botID] = mgr
	s.mu.Unlock()
	// 注入终态回调：工作流跑完且阻塞等待方已退出（超时/取消）时，唤醒 agent 续跑。
	mgr.SetOnWorkflowCompleted(s.onWorkflowCompleted)
}

// onWorkflowCompleted 是工作流进入终态后的回调。
//
// 仅当阻塞等待方（task 工具的 waitForTerminal）已退出（超时/取消/历史或外部触发）
// 时才会被 Manager 调用（Manager 已做去重）。此时 agent 回合已结束，需要把工作流
// 结果作为一条系统消息注入原会话，唤醒 agent 继续后续流程——否则长工作流（>18min）
// 超时后 agent 会被告知「别再等」，工作流在后台跑完后无人接手，表现为「跑完了但
// agent 没继续」。
//
// 续跑以 sessionID 作为 traceID 注入，便于前端在 workflow 卡片终态时按会话 resume
// 收到 agent 续跑的流式回复。
func (s *BotService) onWorkflowCompleted(wf *workflow.Workflow) {
	botID := wf.BotID
	sessionID := wf.SessionID
	if botID == "" || sessionID == "" {
		s.logger.Warnw("workflow completed but missing bot/session, skip continuation",
			"workflow_id", wf.ID, "bot_id", botID, "session_id", sessionID)
		return
	}
	ch, ok := s.GetWebChannel(botID)
	if !ok {
		s.logger.Warnw("workflow completed but web channel unavailable, skip continuation",
			"workflow_id", wf.ID, "bot_id", botID)
		return
	}

	// 续跑以「系统通知」形式注入：traceID 用 sessionID，便于前端按会话 resume 收到流式回复。
	traceID := sessionID
	const userID = "system"

	// 按会话加载聊天历史作为上下文，让 agent 续跑时保有完整背景。
	limit := s.store.GetInt(config.KeyChatContextLimit, 20)
	history, err := s.chatHistory.LoadContextBySession(botID, sessionID, limit)
	if err != nil {
		s.logger.Warnw("failed to load context for workflow continuation", "err", err)
		history = nil
	}

	done := 0
	for _, n := range wf.Nodes {
		if n.Status == workflow.NodeCompleted {
			done++
		}
	}
	text := fmt.Sprintf(
		"系统通知：你此前通过 task 工具提交的工作流 %s 已执行完成（共 %d 个节点，%d 个已完成）。"+
			"请基于工作流各节点的实际产出，继续完成用户最初的需求：%s。"+
			"若需求已经被工作流结果满足，请向用户做简明总结；若还需要进一步操作，请直接继续执行。",
		wf.ID, len(wf.Nodes), done, wf.Requirement,
	)

	extraMeta := map[string]any{
		agenttools.ExtraKeyChatSessionID: sessionID,
	}
	if len(history) > 0 {
		extraMeta["chat_history"] = history
	}

	// 落库续跑指令（作为 user 消息），保证会话连贯、刷新后可回溯。
	if s.chatHistory != nil {
		if err := s.chatHistory.SaveMessage(botID, userID, "user", text, traceID, sessionID); err != nil {
			s.logger.Warnw("failed to save workflow continuation message", "err", err)
		}
	}

	if err := ch.Inject(context.Background(), traceID, userID, text, extraMeta); err != nil {
		s.logger.Warnw("failed to inject workflow continuation", "err", err, "workflow_id", wf.ID)
		return
	}

	// 标记供前端 status 轮询感知，触发 resume 接收流式回复。
	s.mu.RLock()
	mgr := s.wfEngines[botID]
	s.mu.RUnlock()
	if mgr != nil {
		mgr.SetNeedsContinuation(wf.ID, true)
	}

	s.logger.Infow("workflow continuation injected", "workflow_id", wf.ID, "bot_id", botID, "session_id", sessionID)
}

// WorkflowEngine 返回一个已装配工作区工具的工作流引擎（无则返回 nil）。
//
// 供 WorkflowService 的 Recover / Sweeper / UI 重试路径复用，避免自建
// ToolMgr=nil 的残废引擎——后者会让节点执行的 SubAgent 碰不到工作区，
// 产出退化成纯计划（2026-08-06 线上事故，详见 wfEngines 字段注释）。
//
// botID 非空时优先精确匹配该 bot 的引擎（工作流已落 BotID，见 workflow.Workflow）；
// 匹配不到（bot 已停止/ 历史工作流没有 BotID）则退回任取一个已装配实例——
// 那仍然远优于用一个碰不到工作区的引擎。
func (s *BotService) WorkflowEngine(botID string) *workflow.Manager {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if botID != "" {
		if mgr := s.wfEngines[botID]; mgr != nil {
			return mgr
		}
	}
	for _, mgr := range s.wfEngines {
		if mgr != nil {
			return mgr
		}
	}
	return nil
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
//     不会因步数预算耗尽被腰斩。这是默认行为。
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
		// 心跳等触发源：Text 故意留空（防 L0 污染），真正内容在 InjectContext。
		// 必须 fallback 到它，否则会拼出空 user message → GLM 400 拒收，心跳静默失败。
		content := msg.Text
		if content == "" && msg.InjectContext != "" {
			content = msg.InjectContext
		}
		messages = append(messages, llm.UserMessage(content))
		return messages
	}

	// 创建 Prompt Registry + Tool Manager
	promptReg := prompt.NewRegistry()
	toolMgr := agenttools.NewToolManager(promptReg, s.store, s.logger)

	// 接入 bot 维度的工具权限控制（按 platform + userID 过滤）。
	// 设置后取代 legacy ToolPolicyProvider，所有工具解析都走权限表裁决。
	if s.permSvc != nil {
		toolMgr.SetAccessEvaluator(s.permSvc.NewEvaluator())
	}

	// 注册通用工具（web_fetch, calculate, now, web_search 等）
	// 注意：shell 命令执行与文件列举工具（sandbox_exec / sandbox_read_file 等）
	// 由 sandbox 包通过 BotWorkspaceManager 在 Bot 构造时统一注册，这里不再注册。
	if err := tools.RegisterTools(toolMgr, tools.Config{
		TimezoneResolver: builder.GetBotTimezone,
	}); err != nil {
		rollback()
		return errs.Wrap(err, "bot_service: register tools")
	}

	// 注册记忆工具（SQLite 持久化，记忆可跨重启累积）
	//
	// memWindow：动态上下文窗口，记忆块字符上限由 Window.MemoryBudget()*3 派生
	// （随模型上下文窗口自适应），与 context.go / snapshot.renderBlock 口径一致。
	// memCompactor：到达字符预算时对该 scope 做语义合并、归档来源（压缩后入库），
	// 取代简单的截断。repo 通过 SetRepository 注入，打破构造循环依赖。
	memWindow := memory.NewWindow(memory.WindowConfig{})
	memCompactor := storage.NewSQLiteCompactor(storage.SQLiteCompactorConfig{
		Provider: bundle.Main,
		Model:    &llm.Model{ID: bundle.MainDef.Model},
	}, s.logger)
	memRepo := storage.NewSQLiteRepository(s.db, storage.SQLiteRepositoryConfig{
		Window:    memWindow,
		Compactor: memCompactor,
	})
	memCompactor.SetRepository(memRepo)
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
		// 让工作流内部 SubAgent（需求分析/节点执行/审查）继承本 bot 的工作空间工具
		// （exec/读/写/列目录等），使其能像主 Agent 的 SubAgent 一样操作仓库——
		// 例如「审查并修复代码」类目标模式可真正读取源码、运行 go build/vet、落地修改。
		// 经 scope 自动排除 workflow/spawn/记忆工具，不会套娃。
		ToolMgr:   toolMgr,
		ToolBotID: id,
	})
	if err := workflow.RegisterTools(toolMgr, wfMgr); err != nil {
		s.logger.Warnw("failed to register workflow tools", "err", err)
	}

	// 发布这个**已装配工具**的引擎，供 WorkflowService（Recover / Sweeper / UI 重试）复用。
	// 不发布的话它会自建 ToolMgr=nil 的残废引擎，重启后接管的工作流只能产出计划而非结果。
	s.publishWorkflowEngine(id, wfMgr)

	// 注册 SubAgent 工具
	// 将当前模型的 MaxTokens 作为默认输出上限注入，避免 SubAgent 写死 4096：
	// 调用方未显式 WithMaxTokens 时，自动跟随模型配置（如 glm-5.2=128K）。
	saMgr := subagent.NewSubAgentManager(bundle.Main, bundle.MainDef.Model,
		subagent.WithMaxTokens(bundle.MainDef.MaxTokens))
	// 让子 Agent 继承主 Agent 在子 Agent 场景可用的工具（exec/读/写/列目录等），
	// 使其能像主 Agent 一样操作工作空间。spawn 工具由 scope 排除防套娃。
	saMgr.SetToolResolver(toolMgr, agenttools.ToolSessionContext{BotID: id})
	// 子 Agent 重任务（读大量文件 + 多轮模型推理）常超过默认 120s：放宽到 10 分钟，
	// 避免 context deadline exceeded（日志曾见单次 LLM 调用 ~108s 被 120s 上限杀掉）。
	saMgr.SetDelegateTimeout(10 * time.Minute)
	// 提高子 Agent 并发上限：默认对齐单次 spawn 任务上限（5），避免一次派多任务时
	// 被默认 2 的信号量限流成"分批"。可通过全局配置 subagent.max_concurrency 覆盖。
	saMgr.SetMaxConcurrency(s.store.GetInt("subagent.max_concurrency", subagent.DefaultMaxConcurrency))
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

	// 回复控制门控（per-bot opt-in）：仅对显式开启的 bot 生效。
	requireReplyControl := builder.Store().GetBool(config.BotReplyControlKey(id), false)
	if requireReplyControl {
		s.logger.Infow("reply control gate enabled", "bot_id", id)
	}

	// 延迟加载工具（defer_loading）：MCP 等"多工具"类工具初始仅暴露名称 +
	// 描述，完整 input schema 经注入的 tool_search 工具或「模型直接引用」按需
	// 加载，节省 token 并减少工具选择错误。按会话隔离（DeferralStore），
	// 同一会话内跨轮持久可用，且不与其它并发会话串扰。
	deferralStore := llm.NewDeferralStore(true).SetLogger(s.logger)

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
			// 回复控制门控（per-bot opt-in）：仅对显式开启的 bot 生效，
			// 治理「模型决定不互动却把独白当回复发出」。默认关闭。
			RequireReplyControl: requireReplyControl,
			// 动态步数预算：常规任务在 soft 内自然收尾；复杂任务（如大规模
			// 代码修复）在持续产生新工具调用时自动延长至 hard 安全网，
			// 陷入重复循环则提前停止。详见 llm.loopController。
			MaxSteps:        softSteps,
			HardMaxSteps:    hardSteps,
			StreamPublisher: s.eventBus,
			UsageRecorder:   s.statsRecorder, // 统一记账到 stats
			ReductionConfig: llm.DefaultReductionConfigPtr(),
			ToolDeferral:    deferralStore,
		},
		s.tp,
		s.logger,
	)

	// HITL 续跑锚点存储：默认接入主库（自动迁移 deferred_approvals 表）。
	// 为 nil 时不持久化（仅记日志），不影响默认路径（默认无 ApprovalHandler）。
	if hitlStore, herr := stages.NewDeferredApprovalStore(s.db); herr != nil {
		s.logger.Warnw("hitl: init deferred approval store failed", "err", herr)
	} else {
		llmStage.SetDeferredApprovalStore(hitlStore)
	}

	// 用安全中间件包装 LLMStage：
	//   执行顺序（从外到内）：Token 配额(月) → 循环检测 → Token 预算 → LLMStage
	//   TokenQuotaMiddlewareWithState 使用共享的 quotaState，使嵌套 LLM 调用
	//   （subagent、workflow、memory）也能通过 QuotaRecordingProvider 自动记账。
	quotaResolver := pipeline.NewQuotaResolver(s.store)
	// userMessageEventWriter 把摄取到的入站用户消息并行写入持久化事件流
	// （user_message_events 表），使 dreaming 回灌消费事件流而非扫描 chat_messages。
	umeWriter := &userMessageEventWriter{db: s.db}
	wrappedLLM := pipeline.WithMiddleware(llmStage,
		// 捕获 LLM 回复为 L0 工作记忆笔记（category=exchange），供 dreaming 巩固。
		// 必须放在最外层：在 LLMStage 产生 ActionReply 之后才补 ActionNote。
		// 同时把用户入站原文并行写入事件流（writer 非 nil 时）。
		stages.NoteCaptureMiddleware("exchange", umeWriter),
		pipeline.VerificationGateMiddleware(pipeline.NewVerificationGateConfig()),
		pipeline.TokenQuotaMiddlewareWithState(quotaResolver, quotaState, s.tp, s.logger),
		// 不豁免任何工具：workflow 的 task 已改为「提交即阻塞」，一次调用就等到终态，
		// 因此**重复调用 task 属于真异常**（每次都会新建一个工作流），必须保留循环检测守卫。
		// 历史上豁免的task_status 轮询工具已随阻塞化移除。
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

	// 潜水检测 enricher：渠道只读（潜水）时标记 envelope，供 LLMStage 切换为
	// 「观察者模式」——正常思考但只写内部学习笔记、绝不发帖。
	// 必须在 LLMStage（Order=100）之前运行；engagement 在 40，本 enricher 放 45。
	lurkEnricher := stages.NewEnricherStage("lurk-detect", func(ctx context.Context, env *core.Envelope) error {
		platform := ""
		if env.Message.Metadata != nil {
			if ct, ok := env.Message.Metadata["channel_type"]; ok {
				if s, ok := ct.(string); ok {
					platform = s
				}
			}
		}
		if platform == "" {
			platform = env.Message.Source
		}
		if platform != "" && s.permSvc != nil && s.permSvc.IsReadOnly(id, platform) {
			env.Set(core.KVLurkMode, true)
		}
		return nil
	}, s.logger)

	// 被动回复 enricher：仅被动回复（被 @ 才回）。
	// 发言模式三态里，① LLM 调工具主动发帖由 misskey_create_* deny 拦截（outbound.go），
	// ③ 心跳主动发帖由 AllowProactivePost 拦截（outbound.go）；唯独 ② Pipeline 产出
	// ActionReply → Channel.Send 这条路径「不经工具层」，且 OutboundGuard 只能拦 mute
	// （看不到「是否被 @」）。因此路径②必须在 LLMStage 产出 ActionReply 之前做门控——
	// 非真人 @ 的消息一律设 KVSuppressReply，复用 llmroute.go 既有的 replySuppressed 出口。
	// 顺序 46，紧接 lurk-detect(45) / engagement(40) 之后，此时 engagement.proactive 已就绪，
	// 可正确识别「engagement 升级的伪提及」（Mentioned=true 但非真人 @）。
	passiveEnricher := stages.NewEnricherStage("passive-speak", func(ctx context.Context, env *core.Envelope) error {
		platform := ""
		if env.Message.Metadata != nil {
			if ct, ok := env.Message.Metadata["channel_type"]; ok {
				if s, ok := ct.(string); ok {
					platform = s
				}
			}
		}
		if platform == "" {
			platform = env.Message.Source
		}
		if platform == "" || s.permSvc == nil {
			return nil
		}
		if s.permSvc.SpeakMode(id, platform) != toolperm.ModePassive {
			return nil
		}
		// 仅「真人显式 @」才允许被动回复；engagement 升级的伪提及视为非真人 @。
		isHumanMention := env.Message.Mentioned && !isEngagementProactive(env)
		if !isHumanMention {
			env.Set(core.KVSuppressReply, true)
			env.Set(core.KVSuppressReplyReason, "passive_mode_unmentioned")
		}
		return nil
	}, s.logger)

	// 记忆召回 stage：每轮对话前按 [bot, channel, user] 三 scope 检索长期记忆
	// （含潜水学到的经验），注入 system prompt，让 bot 在真人交互时带「实时经验」。
	// memRepo（函数前面 717 行已创建）即 SQLite 仓储（实现 memory.Retriever），
	// 潜水笔记经 MultiStore 代理写入其中，故这里能直接召回。非 nil 时生效；
	// 检索失败属非致命，内部 WARN 跳过。
	//
	// memWindow：函数前部已创建并注入 memRepo，此处复用于召回 stage。
	// 使记忆块字符上限由 Window.MemoryBudget()*3 派生（随模型上下文窗口自适应），
	// 取代此前硬编码的 2200，与 context.go 使用 window 模块的口径一致。
	recallStage := stages.NewRecallStage("memory-recall", memRepo, memWindow, s.logger)

	// 聊天节奏 stage：按「平台 + 会话类型」抑制过度发言。
	// web 平台硬禁用；单聊(private)默认关闭节奏（即时回复）；群聊/频道默认受控。
	// 解析逻辑在 provider 内完成（从 config store 读取按平台配置），保持 stages 包与 api 解耦。
	rhythmProvider := func(platform, chatType string) stages.RhythmPolicy {
		if platform == "web" {
			return stages.RhythmPolicy{Apply: false} // web 永不参与节奏控制
		}
		cfg := s.getBotRhythmConfig(id, platform)
		if cfg == nil || !cfg.Enabled {
			return stages.RhythmPolicy{Apply: false}
		}
		params := selectRhythmParams(cfg, chatType)
		if !params.Enabled {
			return stages.RhythmPolicy{Apply: false}
		}
		// 连续中断需 Interrupt.Enabled 显式开启；关闭时上限传 0（stage 内视为不限），
		// 否则用户在 UI 关掉「连续发言中断」仍会被限制。
		maxConsec := 0
		interruptWindow := 0
		if params.Interrupt.Enabled {
			maxConsec = params.Interrupt.MaxConsecutive
			// 中断计数窗口：优先用 MaxRounds（轮次语义近似秒级窗口不合适），
			// 这里按「限流间隔 × 上限 + 缓冲」推导一个与限流解耦的默认窗口，
			// 保证窗口至少覆盖一轮正常往返，且不随 QuietWait 线性放大沉默。
			interruptWindow = params.Debounce.MaxWait
			if interruptWindow <= 0 {
				interruptWindow = 15
			}
		}
		return stages.RhythmPolicy{
			Apply:           true,
			QuietWait:       params.Debounce.QuietWait,
			SpeakTendency:   params.SpeakTendency,
			MaxConsecutive:  maxConsec,
			InterruptWindow: interruptWindow,
		}
	}
	rhythmStage := stages.NewRhythmStage("chat-rhythm", rhythmProvider, s.logger)

	// 创建心跳子系统（周期性「自主唤醒」，见 docs/heartbeat-redesign.md）。
	// 必须在 pipeline 组装前创建：pipeline 要挂一个极轻量 stage，把「真实外部活动」
	// 回传给心跳频控以重置连续唤醒预算（§9.3）。
	// 真实编排入口（Engine）在 bot.New 内部创建 → 走后置注入 SetRunner。
	hbBundle := heartbeat.NewBundle(heartbeat.BundleConfig{
		BotID:    id,
		Store:    s.heartbeatStore,
		Location: builder.GetBotTimezoneLocation(id),
		Logger:   s.logger,
		// 准入关卡信号源：自上次唤醒以来是否有新消息 / 新记忆条目。
		AdmissionFn: s.newHeartbeatAdmissionFn(id),
		// 枚举本 bot 可主动发帖的真实渠道/会话（供心跳 LLM 决策选择）。
		ChannelLister: s.heartbeatChannelLister(id),
		// 把决策内容发到选定真实渠道（绕过伪频道 "heartbeat" 的 dispatcher）。
		ChannelPoster: s.heartbeatChannelPoster(id),
		// 把决策的内部笔记写入本 bot 长期记忆（DecisionNote 时调用，复用 ActionNote 链路）。
		NoteSaver: s.heartbeatNoteSaver(id),
	})

	// 创建 Pipeline：用声明式 Builder 累积各 Stage，取代此前手写字面量 +
	// 条件 append/prepend 的易漂移写法。每个 Stage 的 Order 即其在链路中的相对位置，
	// Builder.Build() 与 pipeline.New 都会按 Order 排序，顺序由 Order 唯一决定，
	// 与 Add 调用次序无关——新增 Stage 只需加一行 Add/AddIf。
	// 模式词汇见 pipeline.PipelineMode（standard/lurk-only/code），由配置
	// pipeline.mode / bot.<id>.pipeline_mode 驱动（C2-b 接线）。模式通过
	// pipeline.ModeGroups 真正门控 stage/tool 花名册（#4）：engagement/heartbeat/
	// code 各组在 lurk-only 下整体关闭，standard/code 下全部启用；标准模式行为
	// 与旧实现逐字节等价（Order 45/90/95/100 + 可选 5/40 不变）。
	mode := pipeline.ModeStandard
	switch builder.GetPipelineMode(id) {
	case "lurk-only":
		mode = pipeline.ModeLurkOnly
	case "code":
		mode = pipeline.ModeCode
	}
	groups := pipeline.ModeGroups(mode)
	pb := pipeline.NewBuilder().WithMode(mode)
	// 始终开启的核心 stage：潜水资源富化 / 记忆召回 / 节奏门控 / LLM（lurk-only 下走潜水分支）。
	pb.Add(45, lurkEnricher)
	pb.Add(46, passiveEnricher)
	pb.Add(90, recallStage)
	pb.Add(95, rhythmStage)
	pb.Add(100, wrappedLLM)
	if hbBundle != nil && groups[pipeline.GroupHeartbeat] {
		// 心跳频控预算重置：任何真实外部入站消息（非心跳自身）都说明 bot 不在自激真空，
		// 立即恢复连续唤醒预算。纯内存操作，置于链首（Order=5），不影响任何既有语义。
		// lurk-only 下关闭（bot 不自主发帖，无需唤醒预算）。
		hb := hbBundle
		pb.Add(5, &core.StageFunc{
			StageName: "heartbeat-activity",
			Fn: func(_ context.Context, env *core.Envelope) (*core.Envelope, error) {
				if env.Message.Source != core.SourceHeartbeat && env.Message.UserID != "" {
					hb.NotifyUserActivity()
				}
				return env, nil
			},
		})
	}
	// Engagement 放在 LLM 之前（Order=40）——先决定是否参与，再生成回复。
	// lurk-only 下关闭（bot 只学不说，不进入「是否回复」决策）。
	pb.AddIf(engagementStage != nil && groups[pipeline.GroupEngagement], 40, engagementStage)
	p, err := pipeline.New(
		pb.Build(),
		s.tp,
		s.mp,
		s.logger,
	)
	if err != nil {
		rollback()
		return errs.Wrap(err, "bot_service: create pipeline")
	}
	// 注入事件轨迹接收器（append-only，有界内存环）：供可观测性 / HITL 续跑锚点 /
	// 记忆回灌去扫库消费。默认 NoopSink 零开销，此处用有界实现（每 bot 独立，
	// bot 停止后随 Pipeline 一起回收），不影响任何既有语义。
	p.SetSink(core.NewMemorySink(2048))

	// 创建 Dispatcher（bot.New 内部会自动创建 handler 并注册）
	dispatcher := outbound.NewMultiDispatcher(s.logger, s.tp)

	// 创建 WebChannel（始终自动添加）
	webCh := NewWebChannel("web-"+id, id)
	// 挂载 chatHistory，使续跑等无人实时订阅的回复能兜底落库（见 webchannel.Send 的 fallback 路径）。
	webCh.chatHistory = s.chatHistory

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
			s.db,
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
		repo := memRepo // 复用前面已创建的 SQLite 仓储（同时持有潜水笔记）
		memStore = memory.NewMultiStore(filtered, repo)
	}

	// 历史对话回灌：若分层记忆为空，则把该 bot 的历史消息补灌进 L0，
	// 让 dreaming 能处理此前从未进入记忆系统的历史 backlog。
	//
	// 数据源（根治回灌陷阱）：回灌消费的是「入站用户消息事件流」(user_message_events)，
	// 而非原始 chat_messages 表。运行期由 NoteCaptureMiddleware 直接写入事件流；
	// 历史部分由 SeedUserMessageEvents 一次性幂等补齐（仅当事件流为空时）。
	// 此后清空 tiered/memory 表，重启也因事件流与水位线仍在而跳过回灌，不再扫 chat_messages。
	//
	// 守卫：水位线独立于 tiered_memories 持久化于 config 键
	// bot.<id>.memory.backfill.event_watermark，记录已补灌的最大事件流 id。
	// 一旦 bootstrap 完成，即使后续清空 tiered/memory 表，重启也因水位线仍在而跳过
	// 回灌——测试 spam 不再回潮、无需「三表同清」。要强制重新回灌只需删除该水位线键。
	if dreamBundle != nil && memStore != nil {
		cfg := config.NewBuilder(s.store, s.logger)
		wmKey := config.BotMemoryBackfillEventWatermarkKey(id)
		// 一次性补齐事件流（仅当为空；幂等；独立于 L0 是否为空）。
		// 即使 L0 已存在历史数据，也确保事件流被补齐，使未来的 L0 清空仍能从事件流回灌。
		if seeded, serr := memory.SeedUserMessageEvents(context.Background(), s.db, id, s.logger); serr != nil {
			s.logger.Warnw("memory event stream seed failed", "err", serr, "bot_id", id)
		} else if seeded > 0 {
			s.logger.Infow("memory event stream seeded", "bot_id", id, "seeded", seeded)
		}
		// 仅当 L0 为空时从事件流回灌（bootstrap）。
		var l0Count int64
		if err := s.db.Table("tiered_memories").Count(&l0Count).Error; err == nil && l0Count == 0 {
			sinceID := uint64(s.store.GetInt(wmKey, 0))
			switch {
			case !cfg.GetMemoryBackfillEnabled(id):
				s.logger.Infow("memory backfill skipped (disabled)", "bot_id", id)
			case sinceID > 0:
				s.logger.Infow("memory backfill skipped (already bootstrapped)", "bot_id", id, "watermark", sinceID)
			default:
				// 从事件流回灌 L0（带 id 水位线，增量幂等）。
				n, maxID, berr := memory.BackfillFromChatHistory(
					context.Background(), memStore, memory.NewDBUserMessageSource(s.db), id, 0, s.logger,
				)
				if berr != nil {
					s.logger.Warnw("memory backfill failed", "err", berr, "bot_id", id)
				} else {
					if n > 0 {
						s.logger.Infow("memory backfill completed", "bot_id", id, "written", n, "max_id", maxID)
					}
					// 持久化水位线，阻断未来回灌回潮
					if maxID > 0 {
						if serr := s.store.Set(context.Background(), wmKey, strconv.FormatUint(maxID, 10)); serr != nil {
							s.logger.Warnw("memory backfill watermark persist failed", "err", serr, "bot_id", id)
						}
					}
				}
			}
		}
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
	// 镜像与后端必须从这个 sbCfg 读取：bot.New 会用它自建 BotWorkspaceManager 并据此
	// docker run（见 agent/bot/bot.go），否则会回落到 DefaultConfig 的 alpine:latest。
	if img := s.store.GetString(config.KeySandboxImage, ""); img != "" {
		sbCfg.Image = img
	}
	if backend := s.store.GetString(config.KeySandboxBackend, ""); backend != "" {
		sbCfg.Backend = backend
	}
	// 浏览器 MCP 服务开关（per-bot，docker 持久容器模式 + 浏览器镜像时启用）。
	sbCfg.BrowserEnabled = s.store.GetBool(config.KeySandboxBrowserEnabled, false)
	// 浏览器出口代理（IP 归部署侧）：空值直连。
	sbCfg.BrowserProxy = s.store.GetString(config.KeySandboxBrowserProxy, "")

	// 出站只读守卫：Pipeline 自动回复（ActionReply → Channel.Send）不经过工具权限，
	// 因此「只看不发」的潜水bot 必须在出站链路拦。
	// 权限规则按**渠道类型**（misskey/telegram/web）配置，而 Action 只带 Channel 名称，
	// 故在此建立 name → type 的快照映射供守卫查询。
	var outboundGuard outbound.OutboundGuard
	if s.permSvc != nil {
		chanTypes := make(map[string]string, len(allChannels))
		for _, ch := range allChannels {
			chanTypes[ch.Name()] = ch.Type()
		}
		outboundGuard = s.permSvc.NewOutboundGuard(id, func(name string) string {
			return chanTypes[name]
		})
	}

	// 浏览器 cookie 投递/回收桥：把 Web 面板管理的 cookie 在会话前写入容器内状态文件，
	// 并在会话结束（bot 关闭、wrapper 回写）后读回持久化到 DB。bot 不直接依赖 dao，
	// 通过注入的闭包解耦（见 agent/bot/browser.go）。
	browserCookieLoader := func(ctx context.Context) ([]byte, error) {
		var cookies []dao.BotBrowserCookie
		if err := s.db.WithContext(ctx).Where("bot_id = ?", id).Order("domain, name").Find(&cookies).Error; err != nil {
			return nil, errs.Wrap(err, "load browser cookies")
		}
		return json.Marshal(buildStorageState(cookies))
	}
	browserCookieSaver := func(ctx context.Context, stateJSON []byte) error {
		parsed, err := parseBrowserCookieImport(string(stateJSON), "")
		if err != nil {
			return errs.Wrap(err, "parse browser cookies from session")
		}
		// 空结果保护：绝不用空集合触发下面的全量替换，否则一次退化的状态文件
		// （如 close 时浏览器从未启动、saveState 未落盘）会把面板管理的 cookie 全部抹掉。
		if len(parsed) == 0 {
			s.logger.Warnw("browser cookie recover skipped: session state has no cookie", "bot_id", id)
			return nil
		}
		// 以浏览器会话实际产生的 cookie 为准，全量替换该 bot 的 cookie。
		err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			if err := tx.Where("bot_id = ?", id).Delete(&dao.BotBrowserCookie{}).Error; err != nil {
				return err
			}
			now := time.Now().UTC()
			for i := range parsed {
				parsed[i].ID = idgen.New("bc")
				parsed[i].BotID = id
				parsed[i].CreatedAt = now
				parsed[i].UpdatedAt = now
			}
			if len(parsed) > 0 {
				if err := tx.Create(&parsed).Error; err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
		// 可观测性：只记条数，绝不记 cookie 值（凭据本体）。
		s.logger.Infow("browser cookies recovered from session", "bot_id", id, "count", len(parsed))
		return nil
	}

	// browserCookieStartupRecover 在 bot 启动期、loader 覆盖写状态文件之前调用：
	// 把容器内「上一轮残留」的 cookie 状态文件合并回 DB，修复非优雅关闭（SIGKILL / 崩溃 / OOM）
	// 导致 Close() 未跑、cookie 永不进 DB 的缺口。
	// 采用合并 upsert 而非全量替换：保留 DB 中独有项（如 Web 面板新增/编辑），对文件与 DB
	// 同键项以文件为准更新；优雅重启时文件==DB，此步幂等。空状态文件直接视为成功，避免误报。
	// 冲突键 (bot_id,domain,name,path) 由 DB 唯一索引保证，故用原子 upsert 而非「先查后改」，
	// 避免并发/重试下产生重复行。
	browserCookieStartupRecover := func(ctx context.Context, stateJSON []byte) error {
		parsed, err := parseBrowserCookieImport(string(stateJSON), "")
		if err != nil {
			return errs.Wrap(err, "parse browser cookies from session")
		}
		if len(parsed) == 0 {
			return nil
		}
		now := time.Now().UTC()
		for i := range parsed {
			parsed[i].ID = idgen.New("bc")
			parsed[i].BotID = id
			parsed[i].CreatedAt = now
			parsed[i].UpdatedAt = now
		}
		// 先数一下已存在多少条同键项，用于日志区分 created / updated（仅计数，不读 value）。
		var existed int64
		if err := s.db.WithContext(ctx).Model(&dao.BotBrowserCookie{}).
			Where("bot_id = ?", id).Count(&existed).Error; err != nil {
			return errs.Wrap(err, "count existing browser cookies")
		}
		if err := s.db.WithContext(ctx).Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "bot_id"}, {Name: "domain"}, {Name: "name"}, {Name: "path"}},
			DoUpdates: clause.AssignmentColumns([]string{"value", "expires", "http_only", "secure", "same_site", "updated_at"}),
		}).Create(&parsed).Error; err != nil {
			return errs.Wrap(err, "upsert browser cookies from session")
		}
		var total int64
		if err := s.db.WithContext(ctx).Model(&dao.BotBrowserCookie{}).
			Where("bot_id = ?", id).Count(&total).Error; err != nil {
			return errs.Wrap(err, "count browser cookies")
		}
		// 可观测性：只记条数与增量，绝不记 cookie 值（凭据本体）。
		s.logger.Infow("browser cookies recovered at startup",
			"bot_id", id, "from_session", len(parsed), "created", total-existed, "rows_total", total)
		return nil
	}

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
		OutboundGuard:     outboundGuard,
		Logger:            s.logger,
		TP:                s.tp,
		DreamScheduler:    dreamScheduler,
		SelfIDSet:         selfIDSet,
		PromptRegistry:    promptReg,
		ToolManager:       toolMgr,
		AdaptiveSyncer:    adaptiveSyncer,
		RejectionDetector: rejectionDetector,
		OnMessageStart: func(botID, traceID string, cancel context.CancelFunc, interruptCh chan string) {
			s.RegisterMessageCancel(botID, traceID, cancel)
			s.RegisterMessageInterrupt(botID, traceID, interruptCh)
		},
		OnMessageDone: func(botID, traceID string) {
			s.UnregisterMessageCancel(botID, traceID)
			s.UnregisterMessageInterrupt(botID, traceID)
		},
		WorkspaceDir:  workspaceDir,
		SandboxConfig: sbCfg,
		Mode:          mode,

		BrowserCookieLoader:         browserCookieLoader,
		BrowserCookieSaver:          browserCookieSaver,
		BrowserCookieStartupRecover: browserCookieStartupRecover,
	})
	if err != nil {
		rollback()
		return errs.Wrap(err, "bot_service: create bot")
	}

	// 心跳后置接线：把真实编排入口（Engine：pipeline + dispatcher 全链路）交给心跳执行器。
	// 必须在 bot.Run 启动 Scheduler 之前完成，否则首次唤醒会以 runner=nil 失败。
	if hbBundle != nil {
		hbBundle.SetRunner(b.Engine())
		s.logger.Infow("heartbeat wired to engine", "bot_id", id)
	}

	// 注入工具输出落盘指针接收器（借鉴 opencode 的 token 优化）：
	// 当工具输出被截断时，把完整原文写入 bot 工作空间的 tool-output 子目录，
	// 主上下文仅留预览+指针+子 agent 委托提示，把深挖代码的代价隔离到独立子 agent 上下文。
	if wm := b.WorkspaceMgr(); wm != nil {
		toolOutCfg := builder.GetToolOutputConfig()
		// 阈值透传到 LLMConfig.ToolOutput（runTool 内零值回退默认；这里传 0 即「用默认」）。
		llmStage.SetToolOutputConfig(llm.ToolOutputConfig{
			MaxLines: toolOutCfg.MaxLines,
			MaxBytes: toolOutCfg.MaxBytes,
		})
		if toolOutCfg.OffloadEnabled && toolOutCfg.OffloadSubdir != "" {
			llmStage.SetToolOutputSink(buildToolOutputOffloadSink(wm, toolOutCfg.OffloadSubdir))
		}
	}

	// 注册 Soul 人格维护工具：让 bot 能读/改写自己的 SOUL.md 并热生效。
	// 仅当该 bot 启用了 SoulLoader（即配置了工作空间人格文件）时注册；
	// 无 SoulLoader 的 bot 注册会无意义（无文件可写），故跳过。
	if b.SoulLoader() != nil {
		if regErr := toolMgr.Register(bot.NewSoulTool(b.SoulLoader())); regErr != nil {
			s.logger.Warnw("failed to register soul tool", "err", regErr)
		}
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

	// 启动心跳调度器（若启用）：在 bot 主循环之前启动，共享 botCtx，
	// 随 bot 停止（ctx 取消）一起收尾；StopBot 再显式 Stop 兜底。
	if hbBundle != nil {
		hbBundle.Start(botCtx)
	}

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
	s.toolManagers[id] = toolMgr

	// HITL 续跑入口：人类确认后，ResumeDeferredApproval 通过此闭包重新编排原始消息。
	// 走完整 Engine 管线（Recall → LLM → 工具），并在 ctx 中携带预批准，使被 defer
	// 的工具直接采用人类决策而非再次挂起。默认路径无 ApprovalHandler，此闭包不会被调用。
	llmStage.SetResumeDispatch(func(ctx context.Context, msg core.Message) (*core.Envelope, error) {
		env := &core.Envelope{Message: msg}
		res, _, err := b.Engine().ProcessSync(ctx, env)
		return res, err
	})
	s.cancelFuncs[id] = botCancel
	s.closeFuncs[id] = func() {
		wfCleanup()
	}
	if dreamBundle != nil {
		s.dreamingBundles[id] = dreamBundle
	}
	if hbBundle != nil {
		s.heartbeatBundles[id] = hbBundle
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
	delete(s.toolManagers, id)
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
	if hbBundle, ok := s.heartbeatBundles[id]; ok {
		hbBundle.Stop()
		delete(s.heartbeatBundles, id)
	}
	// 该 bot 的工作流引擎随 bot 停止而失效（其 SubAgent 管理器已由 closeFuncs 关闭），
	// 必须摘掉，否则 WorkflowService 会复用一个已关闭的引擎。
	delete(s.wfEngines, id)
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

// ListBotTools 返回某 Bot 已注册的全部工具列表（名称+描述+分类），
// 用于工具权限管理页面的工具名自动补全。
// 若 Bot 未在运行，返回空列表而非错误（管理页面仍可用）。
//
// 包含动态 ToolProvider 提供的工具（MCP / 浏览器等）：这些工具在
// ResolveTools 中同样会过 toolperm 评估，若管理界面看不到它们，
// 管理员就无法为其配置允许/禁止规则。
func (s *BotService) ListBotTools(ctx context.Context, botID string) []agenttools.ToolInfo {
	s.mu.RLock()
	tm, ok := s.toolManagers[botID]
	s.mu.RUnlock()
	if !ok {
		return nil
	}
	return tm.ListAllTools(ctx)
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

// BuildDreamingBundleOnDemand 在 Bot 未启动的情况下按需构建梦境 bundle，
// 供「手动触发梦境」接口调试使用。返回 (nil, nil) 表示该 Bot 未启用 dreaming。
//
// 为维护单一事实来源，本方法与 StartBot 共用相同的构建逻辑
// （config.Builder + bot.CreateLLMBundle + bot.NewDreamingBundle）。
// cron 文件使用临时路径，避免为一次性触发创建持久 cron job。
// 调用方在完成触发后应调用 bundle.Stop() 释放资源（通常由 defer 处理）。
func (s *BotService) BuildDreamingBundleOnDemand(botID string) (*bot.DreamingBundle, error) {
	builder := config.NewBuilder(s.store, s.logger)
	dreamCfg := builder.GetDreamingConfig(botID)
	if !dreamCfg.Enabled {
		return nil, nil
	}

	llmBundle, err := bot.CreateLLMBundle(builder, botID)
	if err != nil {
		return nil, errs.Wrap(err, "build dreaming bundle: create llm bundle")
	}
	loc := builder.GetBotTimezoneLocation(botID)

	// 临时 cron 文件路径：一次性触发不应污染 data/cron/<botID>_dream.json
	cronFile := filepath.Join(os.TempDir(), "dream_trigger_"+botID+".json")
	dBundle := bot.NewDreamingBundle(
		memory.DreamConfig{
			Enabled:          dreamCfg.Enabled,
			Schedule:         dreamCfg.Schedule,
			JaccardThreshold: 0.9,
			MaxDreamTokens:   10000,
		},
		llmBundle.Main,
		llmBundle.MainDef.Model,
		loc,
		s.tp,
		s.logger,
		botID,
		cronFile,
		s.db,
	)
	if dBundle == nil {
		return nil, fmt.Errorf("dreaming bundle construction failed for bot %s", botID)
	}

	// 注入 Bot 自我画像提取器（仅用于画像蒸馏，不影响触发流程）
	botProfiler := memory.NewBotProfileProfiler(
		memory.BotProfileProfilerConfig{
			Provider: llmBundle.Main,
			Model:    &llm.Model{ID: llmBundle.MainDef.Model},
		},
		s.tp,
		s.logger,
	)
	dBundle.Manager.SetBotProfiler(botProfiler)
	dBundle.BotProfiler = botProfiler
	dBundle.Manager.SetOnBotProfileUpdated(func(bid string, result *memory.BotProfileResult) {
		if result == nil {
			return
		}
		s.logger.Infow("dreaming on-demand profile updated", "bot_id", bid, "personality", result.Personality)
	})
	return dBundle, nil
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

// ============================================================================
// 工具输出落盘指针（借鉴 opencode 的 token 优化）
// ============================================================================

// maxOffloadFiles 单个 bot 的落盘目录文件数上限。超过则清理最旧的若干，
// 避免长期运行无限增长（落盘文件是一次性中间产物，过期即无用）。
const maxOffloadFiles = 200

// buildToolOutputOffloadSink 构造落盘指针接收器：把完整工具输出经 sandbox 写入
// bot 工作空间（docker 模式写入容器内 volume 的 /data/<subdir>/<id>.txt，local 模式
// 写入宿主目录），返回工作空间相对路径。子 agent（spawn 派生的 sub-agent）的 read
// 工具同样在 sandbox 内执行，据此相对路径即可读到——从而把深挖代码的完整 dump
// 隔离在子 agent 上下文，主上下文只留预览+指针。任何失败都返回 error，由截断层
// fail-safe 退化成纯 head+tail 截断。
func buildToolOutputOffloadSink(wm *sandbox.BotWorkspaceManager, subdir string) llm.ToolOutputOffloadSink {
	var mu sync.Mutex // 串行化同 bot 的落盘 + 清理，避免并行工具调用竞争同一目录。
	return func(botID, toolCallID string, content []byte) (string, error) {
		// 工具调用 ID 由模型下发，可能含路径分隔符或 ".."，必须净化避免穿越出 subdir。
		fname := filepath.Base(toolCallID)
		if fname == "" || fname == "." || fname == string(os.PathSeparator) {
			return "", fmt.Errorf("invalid toolCallID for offload: %q", toolCallID)
		}
		rel := filepath.Join(subdir, fname+".txt")
		mu.Lock()
		defer mu.Unlock()
		// 经 sandbox 写入：docker 模式落到容器内 named volume（/data），local 模式落宿主目录。
		// 主进程不直接操作系统隔离的文件系统，子 agent 的 read 才能在同一 sandbox 内读到。
		if err := wm.WriteToolOutput(botID, rel, content); err != nil {
			return "", err
		}
		wm.PruneToolOutput(botID, subdir, maxOffloadFiles)
		return rel, nil
	}
}

// ============================================================================
// userMessageEventWriter — 入站用户消息事件流写入器
// ============================================================================

// userMessageEventWriter 实现 stages.UserMessageEventWriter：把摄取到的入站用户消息
// 写入 user_message_events 表，作为 dreaming 回灌（backfill）的权威数据源，
// 取代此前直接扫 chat_messages 的做法（根治回灌陷阱）。
type userMessageEventWriter struct {
	db *gorm.DB
}

// WriteUserMessageEvent 将一条已摄取的入站用户消息持久化到事件流。
func (w *userMessageEventWriter) WriteUserMessageEvent(ctx context.Context, msg stages.CapturedUserMessage) error {
	if w.db == nil {
		return nil
	}
	rec := dao.UserMessageEvent{
		BotID:     msg.BotID,
		Channel:   msg.Channel,
		UserID:    msg.UserID,
		MessageID: msg.MessageID,
		Content:   msg.Content,
		CreatedAt: time.Now(),
	}
	return w.db.WithContext(ctx).Create(&rec).Error
}

// isEngagementProactive 判定当前 envelope 是否由 engagement 子系统升级而来的
// 「伪提及」（Mentioned=true 但 bot 是主动决定参与，而非真人显式 @）。
// 被动回复门控必须排除这类情况——engagement 主动插话不属于「被 @ 回复」。
func isEngagementProactive(env *core.Envelope) bool {
	if v, ok := env.Get("engagement.proactive"); ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return false
}
