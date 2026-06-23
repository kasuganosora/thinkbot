package session

import (
	"context"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/agent/outbound"
	"github.com/kasuganosora/thinkbot/util/traceid"
)

// ============================================================================
// SessionManager — 会话管理器
// ============================================================================

// SessionManager 维护活跃 session 的生命周期。
//
// 职责：
//   - 按 sessionID 缓存活跃 session
//   - GetOrCreate: 获取或创建 session
//   - 空闲超时检测：长时间无活动的 session 自动归档
//   - 归档时将 session 精华写入 memory（可选）
//
// SessionManager 是线程安全的。
type SessionManager struct {
	mu       sync.RWMutex
	sessions map[string]*Session // sessionID → Session
	config   ManagerConfig
	resolver SessionResolver

	// 归档回调（可选）
	onArchive func(s *Session)

	tracer trace.Tracer
	logger *zap.SugaredLogger
}

// ManagerConfig 配置 SessionManager。
type ManagerConfig struct {
	// MaxMessages 每个 session 工作记忆的最大消息数（默认 20）。
	MaxMessages int
	// IdleTimeout session 空闲超时时间。
	// 超过此时间无活动的 session 在下次 Sweep 时被归档。
	// 默认 30 分钟。
	IdleTimeout time.Duration
	// SweepInterval 空闲检测间隔。
	// 0 表示禁用后台自动扫描（需手动调用 Sweep）。
	// 默认 5 分钟。
	SweepInterval time.Duration
}

// DefaultManagerConfig 返回默认管理器配置。
func DefaultManagerConfig() ManagerConfig {
	return ManagerConfig{
		MaxMessages:   20,
		IdleTimeout:   30 * time.Minute,
		SweepInterval: 5 * time.Minute,
	}
}

// NewSessionManager 创建会话管理器。
func NewSessionManager(
	resolver SessionResolver,
	config ManagerConfig,
	tp trace.TracerProvider,
	logger *zap.SugaredLogger,
) *SessionManager {
	if resolver == nil {
		resolver = NewDefaultResolver("session")
	}
	return &SessionManager{
		sessions: make(map[string]*Session),
		config:   config,
		resolver: resolver,
		tracer:   tp.Tracer("github.com/kasuganosora/thinkbot/agent/session"),
		logger:   logger.With("component", "session_manager"),
	}
}

// OnArchive 注册归档回调函数。
// session 归档时调用，用于将工作记忆写入长期 Memory。
func (m *SessionManager) OnArchive(fn func(s *Session)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onArchive = fn
}

// GetOrCreate 获取或创建指定 ID 的 session。
// 返回 session 和是否为新创建。
func (m *SessionManager) GetOrCreate(sessionID, botID, channel, createdBy string) (*Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if s, ok := m.sessions[sessionID]; ok && s.IsActive() {
		return s, false
	}

	s := NewSession(sessionID, botID, channel,
		WithMaxMessages(m.config.MaxMessages),
		WithCreatedBy(createdBy),
	)
	m.sessions[sessionID] = s
	return s, true
}

// Get 获取指定 ID 的 session（不存在返回 nil）。
func (m *SessionManager) Get(sessionID string) *Session {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.sessions[sessionID]
}

// ActiveCount 返回当前活跃 session 数量。
func (m *SessionManager) ActiveCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	count := 0
	for _, s := range m.sessions {
		if s.IsActive() {
			count++
		}
	}
	return count
}

// Archive 归档指定 session。
// 触发 onArchive 回调（如有），然后从活跃列表移除。
func (m *SessionManager) Archive(sessionID string) {
	m.mu.Lock()
	session, ok := m.sessions[sessionID]
	fn := m.onArchive
	if ok {
		delete(m.sessions, sessionID) // 先从 map 移除，防止并发获取
	}
	m.mu.Unlock()

	if !ok || session == nil {
		return
	}

	session.Archive()

	if fn != nil {
		fn(session)
	}

	m.logger.Debugw("session archived",
		"session_id", sessionID,
		"message_count", session.MessageCount(),
		"duration", time.Since(session.StartedAt()))
}

// Sweep 扫描并归档空闲超时的 session。
func (m *SessionManager) Sweep() int {
	if m.config.IdleTimeout <= 0 {
		return 0
	}

	m.mu.RLock()
	var toArchive []string
	for id, s := range m.sessions {
		if s.IsActive() && s.IdleDuration() > m.config.IdleTimeout {
			toArchive = append(toArchive, id)
		}
	}
	m.mu.RUnlock()

	for _, id := range toArchive {
		m.Archive(id)
	}

	if len(toArchive) > 0 {
		m.logger.Debugw("session sweep completed",
			"archived", len(toArchive),
			"remaining", m.ActiveCount())
	}

	return len(toArchive)
}

// StartSweeper 启动后台空闲检测 goroutine。
// 返回取消函数，调用以停止后台扫描。
func (m *SessionManager) StartSweeper(ctx context.Context) context.CancelFunc {
	if m.config.SweepInterval <= 0 {
		return func() {}
	}

	sweepCtx, cancel := context.WithCancel(ctx)
	go func() {
		ticker := time.NewTicker(m.config.SweepInterval)
		defer ticker.Stop()
		for {
			select {
			case <-sweepCtx.Done():
				return
			case <-ticker.C:
				m.Sweep()
			}
		}
	}()

	return cancel
}

// Resolve 为消息解析 session。
func (m *SessionManager) Resolve(ctx context.Context, msg *core.Message) ResolveResult {
	return m.resolver.Resolve(ctx, msg)
}

// ============================================================================
// SessionStage — Pipeline Session 集成 Stage
// ============================================================================

// SessionStage 是一个 Pipeline Stage，在消息处理过程中：
//  1. 通过 SessionResolver 判断消息是否属于/应创建 session
//  2. 获取或创建 session，将消息追加到工作记忆
//  3. 将 session 上下文注入 Envelope KV（供下游 Stage 使用）
//
// SessionStage 通常放在 Pipeline 靠前的位置（如 Order=50），
// 在 MemoryStage(100) 之前，让 Memory 检索时可以参考当前 session 上下文。
//
// 对于 resolve 结果为 ok=false 的消息（如时间线观察帖），
// SessionStage 跳过 session 逻辑，消息继续在 Pipeline 中流转。
type SessionStage struct {
	name   string
	mgr    *SessionManager
	config StageConfig
	tracer trace.Tracer
	logger *zap.SugaredLogger
}

// StageConfig 配置 SessionStage。
type StageConfig struct {
	// ContextMaxMessages 格式化 session 上下文时最多包含的消息数。
	// 默认 10。
	ContextMaxMessages int
}

// DefaultStageConfig 返回默认 Stage 配置。
func DefaultStageConfig() StageConfig {
	return StageConfig{
		ContextMaxMessages: 10,
	}
}

// NewSessionStage 创建 Session Pipeline Stage。
func NewSessionStage(
	name string,
	mgr *SessionManager,
	config StageConfig,
	tp trace.TracerProvider,
	logger *zap.SugaredLogger,
) *SessionStage {
	if name == "" {
		name = "session"
	}
	return &SessionStage{
		name:   name,
		mgr:    mgr,
		config: config,
		tracer: tp.Tracer("github.com/kasuganosora/thinkbot/agent/session"),
		logger: logger.With("component", "session_stage"),
	}
}

// Name 返回 Stage 名称。
func (s *SessionStage) Name() string { return s.name }

// Process 执行 session 解析和上下文注入。
func (s *SessionStage) Process(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
	ctx, span := s.tracer.Start(ctx, "stage.session.process",
		trace.WithAttributes(
			attribute.String("message.id", env.Message.ID),
			attribute.String("message.channel", env.Message.Channel),
			attribute.String("trace.id", traceid.FromContext(ctx)),
		))
	defer span.End()

	// 派生携带 trace_id 的 logger，使所有日志可通过 trace_id 关联
	logger := traceid.WithLoggerFrom(ctx, s.logger)

	// 1. 解析 session
	result := s.mgr.Resolve(ctx, &env.Message)

	if !result.OK {
		// 不参与 session 的消息（时间线观察等），直接放行
		span.SetAttributes(attribute.Bool("session.resolved", false))
		env.Set("session.active", false)
		return env, nil
	}

	// 2. 获取或创建 session
	session, isNew := s.mgr.GetOrCreate(
		result.SessionID,
		env.Message.BotID,
		env.Message.Channel,
		result.CreatedBy,
	)

	// 3. 将用户消息追加到 session 工作记忆
	session.AppendMessage(Message{
		Role:      "user",
		Text:      env.Message.Text,
		UserID:    env.Message.UserID,
		Timestamp: env.Message.CreatedAt,
	})

	// 4. 格式化 session 上下文并注入 Envelope
	contextText := FormatContext(session, s.config.ContextMaxMessages)
	if contextText != "" {
		env.Set("session.context", contextText)
	}

	env.Set("session.id", result.SessionID)
	env.Set("session.is_new", isNew)
	env.Set("session.active", true)
	env.Set("session.message_count", session.MessageCount())

	span.SetAttributes(
		attribute.String("session.id", result.SessionID),
		attribute.Bool("session.is_new", isNew),
		attribute.Int("session.message_count", session.MessageCount()),
		attribute.Bool("session.resolved", true),
	)

	// 5. 旁路事件
	emitter := outbound.EmitterFromContext(ctx)
	emitter.Emit(ctx, "session.resolved", env.Message.TraceID, map[string]any{
		"session_id":    result.SessionID,
		"is_new":        isNew,
		"message_count": session.MessageCount(),
		"created_by":    result.CreatedBy,
	})

	logger.Debugw("session resolved",
		"message_id", env.Message.ID,
		"session_id", result.SessionID,
		"is_new", isNew,
		"message_count", session.MessageCount())

	return env, nil
}

// ============================================================================
// SessionWriteStage — Pipeline Session 回写 Stage
// ============================================================================

// SessionWriteStage 在 Pipeline 后期将 Bot 的回复追加到 session 工作记忆。
//
// 通常放在 Pipeline 靠后的位置（如 Order=850），
// 在 ReplyStage(500) 之后、MemoryWriteStage(900) 之前。
//
// 它检查 Envelope 中的 ActionReply 内容，将 Bot 回复写入对应 session。
// 如果 session 不存在（消息未创建 session），则跳过。
type SessionWriteStage struct {
	name   string
	mgr    *SessionManager
	tracer trace.Tracer
	logger *zap.SugaredLogger
}

// NewSessionWriteStage 创建 session 回写 Stage。
func NewSessionWriteStage(
	name string,
	mgr *SessionManager,
	tp trace.TracerProvider,
	logger *zap.SugaredLogger,
) *SessionWriteStage {
	if name == "" {
		name = "session_write"
	}
	return &SessionWriteStage{
		name:   name,
		mgr:    mgr,
		tracer: tp.Tracer("github.com/kasuganosora/thinkbot/agent/session"),
		logger: logger.With("component", "session_write_stage"),
	}
}

// Name 返回 Stage 名称。
func (s *SessionWriteStage) Name() string { return s.name }

// Process 将 Bot 回复追加到 session 工作记忆。
func (s *SessionWriteStage) Process(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
	ctx, span := s.tracer.Start(ctx, "stage.session_write.process",
		trace.WithAttributes(
			attribute.String("message.id", env.Message.ID),
			attribute.String("trace.id", traceid.FromContext(ctx)),
		),
	)
	defer span.End()

	// 派生携带 trace_id 的 logger
	logger := traceid.WithLoggerFrom(ctx, s.logger)

	// 检查是否有活跃 session
	sessionID := SessionIDFromEnvelope(env)
	if sessionID == "" {
		return env, nil
	}

	session := s.mgr.Get(sessionID)
	if session == nil || !session.IsActive() {
		return env, nil
	}

	// 从 ActionReply 中提取 Bot 回复文本
	actions := env.Actions()
	var replyText string
	for _, action := range actions {
		if action.Type != core.ActionReply {
			continue
		}
		if text, ok := action.Payload.(string); ok && text != "" {
			replyText = text
			break
		}
	}

	if replyText == "" {
		return env, nil
	}

	// 追加到 session 工作记忆
	session.AppendMessage(Message{
		Role:      "assistant",
		Text:      replyText,
		Timestamp: time.Now(),
	})

	span.SetAttributes(attribute.Int("session.reply_len", len(replyText)))
	logger.Debugw("session reply recorded",
		"message_id", env.Message.ID,
		"session_id", sessionID,
		"reply_len", len(replyText))

	return env, nil
}
