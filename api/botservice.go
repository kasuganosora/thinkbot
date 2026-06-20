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
	"github.com/kasuganosora/thinkbot/agent/outbound"
	"github.com/kasuganosora/thinkbot/agent/pipeline"
	"github.com/kasuganosora/thinkbot/agent/stages"
	"github.com/kasuganosora/thinkbot/channel/misskey"
	"github.com/kasuganosora/thinkbot/channel/telegram"
	"github.com/kasuganosora/thinkbot/config"
	"github.com/kasuganosora/thinkbot/dao"
	"github.com/kasuganosora/thinkbot/llm"
	"github.com/kasuganosora/thinkbot/util/errs"
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

	mu           sync.RWMutex
	channels     map[string]*WebChannel // botID → WebChannel
	botInstances map[string]*bot.Bot    // botID → running Bot
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
		db:           db,
		store:        store,
		mgr:          mgr,
		logger:       logger.With("component", "bot_service"),
		tp:           tp,
		mp:           mp,
		eventBus:     eventBus,
		channels:     make(map[string]*WebChannel),
		botInstances: make(map[string]*bot.Bot),
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

	llmStage := stages.NewLLMStage(
		"llm",
		bundle.Main,
		stages.LLMConfig{
			SystemPrompt:   def.SystemPrompt,
			Model:          mainModel,
			Temperature:    temp,
			MaxTokens:      maxTok,
			MessageBuilder: messageBuilder,
		},
		s.tp,
		s.logger,
	)

	// 创建 Pipeline
	p, err := pipeline.New(
		[]core.StageInfo{
			{Stage: llmStage, Order: 100, Enabled: true},
		},
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

	// 创建 Bot
	botCfg := bot.BotConfig{
		Workers:      def.Workers,
		SystemPrompt: def.SystemPrompt,
		Model:        def.Model,
	}
	if def.Temperature > 0 {
		botCfg.Temperature = def.Temperature
	}
	if def.MaxTokens > 0 {
		botCfg.MaxTokens = def.MaxTokens
	}

	b, err := bot.New(bot.BotParams{
		ID:         id,
		Name:       def.Name,
		Config:     botCfg,
		Pipeline:   p,
		Dispatcher: dispatcher,
		Channels:   allChannels,
		EventBus:   s.eventBus,
		Logger:     s.logger,
		TP:         s.tp,
	})
	if err != nil {
		rollback()
		return errs.Wrap(err, "bot_service: create bot")
	}

	// 注册到 BotManager
	if err := s.mgr.Register(b); err != nil {
		b.Close()
		rollback()
		return errs.Wrap(err, "bot_service: register bot")
	}

	// 启动 Bot（bot.Run 内部会自动注册实现 Sender 接口的 Channel）
	go func() {
		if err := b.Run(ctx); err != nil {
			s.logger.Errorw("bot run failed", "bot_id", id, "err", err)
		}
	}()

	// 等待 Bot 就绪（带 30s 超时，防止永久挂起）
	readyTimeout := time.NewTimer(30 * time.Second)
	defer readyTimeout.Stop()
	select {
	case <-b.Ready():
		s.logger.Infow("bot started", "bot_id", id, "channels", len(allChannels))
	case <-readyTimeout.C:
		b.Stop()
		b.Close()
		s.mgr.Unregister(id)
		rollback()
		return errs.Internal("bot_service: bot startup timeout (30s)")
	case <-ctx.Done():
		b.Stop()
		b.Close()
		s.mgr.Unregister(id)
		rollback()
		return errs.Wrap(ctx.Err(), "bot_service: context cancelled")
	}

	s.mu.Lock()
	s.channels[id] = webCh
	s.botInstances[id] = b
	s.mu.Unlock()

	// 更新定义状态
	s.db.Model(&dao.BotDefinition{}).Where("id = ?", id).Update("status", dao.BotStatusRunning)

	return nil
}

// StopBot 停止运行中的 Bot。
func (s *BotService) StopBot(id string) {
	s.mu.Lock()
	b, exists := s.botInstances[id]
	delete(s.botInstances, id)
	delete(s.channels, id)
	s.mu.Unlock()

	if !exists || b == nil {
		return
	}

	b.Stop()
	b.Close()
	s.mgr.Unregister(id)

	s.db.Model(&dao.BotDefinition{}).Where("id = ?", id).Update("status", dao.BotStatusStopped)
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
