package outbound

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	noop_trace "go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/core"
)

// ============================================================================
// CallbackRegistry — 回调注册表
// ============================================================================

// CallbackFunc 是回调函数签名。
// ctx 是调用上下文，result 是 Action 携带的回调数据。
type CallbackFunc func(ctx context.Context, result CallbackResult) error

// CallbackResult 是回调携带的结果数据。
type CallbackResult struct {
	// CallbackID 回调标识。
	CallbackID string
	// Status 任务状态（"success" / "error" / "partial"）。
	Status string
	// Payload 回调结果数据（由双方约定结构）。
	Payload any
	// Error 错误描述（Status=error 时有值）。
	Error string
	// Metadata 附加的元数据。
	Metadata map[string]any
}

// CallbackRegistry 管理回调函数的注册、调用和注销。
// 线程安全。
type CallbackRegistry interface {
	// Register 注册一个回调函数，返回 callbackID。
	// 如果 id 为空，自动生成唯一 ID。
	Register(id string, fn CallbackFunc) string
	// Invoke 调用指定 ID 的回调函数。
	// 如果 ID 不存在返回 ErrCallbackNotFound。
	Invoke(ctx context.Context, id string, result CallbackResult) error
	// Unregister 注销回调。
	Unregister(id string)
	// Has 检查回调是否存在。
	Has(id string) bool
	// Count 返回注册的回调数量。
	Count() int
}

// ErrCallbackNotFound 表示指定的回调 ID 不存在。
var ErrCallbackNotFound = fmt.Errorf("callback not found")

// ============================================================================
// MemoryCallbackRegistry — 内存实现（带 TTL 过期清理）
// ============================================================================

// callbackEntry 是注册表中的单个回调条目。
type callbackEntry struct {
	fn        CallbackFunc
	createdAt time.Time
}

// MemoryCallbackRegistry 是 CallbackRegistry 的内存实现。
// 适用于单进程场景；分布式场景需要外部实现（如 Redis + pub/sub）。
// 支持 TTL：超过 DefaultCallbackTTL 未被调用的回调会被自动清理。
type MemoryCallbackRegistry struct {
	mu        sync.Mutex
	callbacks map[string]callbackEntry
	counter   int
	ttl       time.Duration
	stopOnce  sync.Once
	stopCh    chan struct{}
}

// DefaultCallbackTTL 是回调的默认过期时间（30 分钟）。
const DefaultCallbackTTL = 30 * time.Minute

// NewMemoryCallbackRegistry 创建内存回调注册表。
// 默认 TTL 为 30 分钟，可通过 WithCallbackTTL 覆盖。
func NewMemoryCallbackRegistry(opts ...CallbackRegistryOption) *MemoryCallbackRegistry {
	r := &MemoryCallbackRegistry{
		callbacks: make(map[string]callbackEntry),
		ttl:       DefaultCallbackTTL,
		stopCh:    make(chan struct{}),
	}
	for _, opt := range opts {
		opt(r)
	}
	// 启动后台清理 goroutine
	go r.cleanupLoop()
	return r
}

// CallbackRegistryOption 是 MemoryCallbackRegistry 的配置选项。
type CallbackRegistryOption func(*MemoryCallbackRegistry)

// WithCallbackTTL 设置回调过期时间。
func WithCallbackTTL(ttl time.Duration) CallbackRegistryOption {
	return func(r *MemoryCallbackRegistry) {
		if ttl > 0 {
			r.ttl = ttl
		}
	}
}

// cleanupLoop 定期清理过期回调。
func (r *MemoryCallbackRegistry) cleanupLoop() {
	ticker := time.NewTicker(r.ttl / 2)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			r.cleanup()
		case <-r.stopCh:
			return
		}
	}
}

// cleanup 清理所有过期的回调。
func (r *MemoryCallbackRegistry) cleanup() {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()
	for id, entry := range r.callbacks {
		if now.Sub(entry.createdAt) > r.ttl {
			delete(r.callbacks, id)
		}
	}
}

// Close 停止后台清理 goroutine。
func (r *MemoryCallbackRegistry) Close() {
	r.stopOnce.Do(func() {
		close(r.stopCh)
	})
}

func (r *MemoryCallbackRegistry) Register(id string, fn CallbackFunc) string {
	r.mu.Lock()
	defer r.mu.Unlock()

	if id == "" {
		r.counter++
		id = fmt.Sprintf("cb_%d", r.counter)
	}
	r.callbacks[id] = callbackEntry{fn: fn, createdAt: time.Now()}
	return id
}

func (r *MemoryCallbackRegistry) Invoke(ctx context.Context, id string, result CallbackResult) error {
	r.mu.Lock()
	entry, ok := r.callbacks[id]
	if ok {
		// 一次性语义：调用后自动移除，防止并发重复调用
		delete(r.callbacks, id)
	}
	r.mu.Unlock()

	if !ok {
		return fmt.Errorf("%w: %s", ErrCallbackNotFound, id)
	}
	return entry.fn(ctx, result)
}

func (r *MemoryCallbackRegistry) Unregister(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.callbacks, id)
}

func (r *MemoryCallbackRegistry) Has(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.callbacks[id]
	return ok
}

func (r *MemoryCallbackRegistry) Count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.callbacks)
}

// ============================================================================
// CallbackHandler — ActionHandler 实现
// ============================================================================

// CallbackHandler 处理 ActionCallback 类型的 Action。
// 它从 Action.Metadata["callback_id"] 获取回调 ID，
// 通过 CallbackRegistry 找到对应的回调函数并执行。
//
// 典型使用场景：
//   - 父 Agent 创建子任务，注册回调到 Registry
//   - 子 Agent 完成任务后产出 ActionCallback
//   - CallbackHandler 调用回调将结果回传给父 Agent
type CallbackHandler struct {
	registry CallbackRegistry
	logger   *zap.SugaredLogger
	tracer   trace.Tracer
}

// NewCallbackHandler 创建回调处理器。
func NewCallbackHandler(
	registry CallbackRegistry,
	logger *zap.SugaredLogger,
	tp trace.TracerProvider,
) *CallbackHandler {
	if tp == nil {
		tp = noop_trace.NewTracerProvider()
	}
	if logger == nil {
		logger = zap.NewNop().Sugar()
	}
	return &CallbackHandler{
		registry: registry,
		logger:   logger.Named("callback_handler"),
		tracer:   tp.Tracer("github.com/kasuganosora/thinkbot/agent/outbound/callback"),
	}
}

// Registry 返回内部的 CallbackRegistry（供外部注册回调）。
func (h *CallbackHandler) Registry() CallbackRegistry {
	return h.registry
}

// Handle 执行回调。
// 从 Action.Metadata["callback_id"] 获取目标回调 ID 并调用。
func (h *CallbackHandler) Handle(ctx context.Context, action core.Action) error {
	ctx, span := h.tracer.Start(ctx, "outbound.callback.handle",
		trace.WithAttributes(
			attribute.String("action.type", string(action.Type)),
			attribute.String("action.channel", action.Channel),
		))
	defer span.End()

	// 提取 callback_id
	callbackID, ok := h.extractCallbackID(action)
	if !ok {
		err := fmt.Errorf("callback_handler: action missing metadata[\"callback_id\"]")
		span.SetStatus(codes.Error, err.Error())
		h.logger.Warnw("missing callback_id",
			"channel", action.Channel,
			"user_id", action.UserID)
		return err
	}

	span.SetAttributes(attribute.String("callback.id", callbackID))

	// 构建 CallbackResult
	result := CallbackResult{
		CallbackID: callbackID,
		Status:     h.extractString(action.Metadata, "status", "success"),
		Payload:    action.Payload,
		Error:      h.extractString(action.Metadata, "error", ""),
		Metadata:   action.Metadata,
	}

	// 调用回调
	if err := h.registry.Invoke(ctx, callbackID, result); err != nil {
		span.SetStatus(codes.Error, err.Error())
		h.logger.Errorw("callback invocation failed",
			"callback_id", callbackID,
			"err", err)
		return err
	}

	h.logger.Debugw("callback invoked successfully",
		"callback_id", callbackID,
		"status", result.Status)
	return nil
}

// extractCallbackID 从 Action.Metadata 中提取 callback_id。
func (h *CallbackHandler) extractCallbackID(action core.Action) (string, bool) {
	if action.Metadata == nil {
		return "", false
	}
	v, ok := action.Metadata["callback_id"]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	if !ok || s == "" {
		return "", false
	}
	return s, true
}

// extractString 从 map 中提取 string 值，不存在时返回默认值。
func (h *CallbackHandler) extractString(m map[string]any, key, defaultVal string) string {
	if m == nil {
		return defaultVal
	}
	v, ok := m[key]
	if !ok {
		return defaultVal
	}
	s, ok := v.(string)
	if !ok {
		return defaultVal
	}
	return s
}
