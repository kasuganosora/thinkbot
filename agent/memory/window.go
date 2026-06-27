package memory

import (
	"sync"
	"sync/atomic"
)

// ============================================================================
// Window — 动态上下文窗口管理器
// ============================================================================

// Window 跟踪 LLM 的 token 用量，动态维护可用的上下文空间。
//
// 每次 LLM 调用返回 Usage 后，Window 更新已消耗的 token 数，
// 计算剩余可用空间。ContextManager 根据剩余空间决定注入多少记忆、
// 是否需要触发压缩。
//
// 设计要点：
//   - 线程安全：支持多 goroutine 并发读写
//   - 动态调整：每轮 LLM 调用后更新，反映真实消耗
//   - 预留空间：为 system prompt、tool definitions 等固定消耗预留
//   - 与 Model 无关：通过 MaxTokens 配置适配不同模型的窗口大小
//
// 使用流程：
//  1. 创建 Window（设定模型最大上下文长度）
//  2. 每次 LLM 调用后调用 RecordUsage 更新消耗
//  3. ContextManager 调用 Available() 获取可用空间
//  4. 超限时触发 Compressor 压缩
type Window struct {
	config WindowConfig

	mu         sync.RWMutex
	usedTokens int // 累计已消耗的 input tokens
	roundCount int // LLM 调用轮数

	// metrics
	totalInputTokens  atomic.Int64
	totalOutputTokens atomic.Int64
	compressions      atomic.Int64 // 触发压缩次数
}

// WindowConfig 配置上下文窗口。
type WindowConfig struct {
	// MaxContextTokens 模型的最大上下文窗口大小（token 数）。
	// 例如 GPT-4: 128000, Claude: 200000, GPT-3.5: 16385。
	MaxContextTokens int

	// ReservedTokens 为系统 prompt、tool 定义等固定内容预留的 token 数。
	// 这部分不计入 memory 可用空间。
	// 默认 2000。
	ReservedTokens int

	// OutputReserve 为 LLM 输出预留的 token 数。
	// 确保 LLM 有足够空间生成回复。
	// 默认 4096。
	OutputReserve int

	// MemoryBudgetRatio memory 可使用的窗口比例（0.0 ~ 1.0）。
	// 基于可用空间计算 memory 上限。
	// 默认 0.15（即 15% 的可用空间分配给 memory）。
	// 注意：0.3 对成本不友好——128K 窗口首轮有 ~36K tokens 记忆预算，
	// 实际注入的检索结果极少超 1K tokens，过多预算会让 LLM 压缩触发过晚。
	MemoryBudgetRatio float64

	// MaxMemoryTokens memory 注入的硬上限（token 数）。
	// 无论 Available() 返回多少，实际注入的 memory context 不超过此值。
	// 0 表示不设硬上限（使用 Available() 结果）。
	// 推荐值：2000-4000（约 6000-12000 字符的记忆上下文）。
	MaxMemoryTokens int

	// CompressThreshold 触发压缩的阈值比例。
	// 当 memory 内容超过 MemoryBudget 的此比例时触发压缩。
	// 默认 0.8（80%）。
	CompressThreshold float64
}

// DefaultWindowConfig 返回默认窗口配置。
func DefaultWindowConfig() WindowConfig {
	return WindowConfig{
		MaxContextTokens:  128000,
		ReservedTokens:    2000,
		OutputReserve:     4096,
		MemoryBudgetRatio: 0.15,
		CompressThreshold: 0.8,
		MaxMemoryTokens:   3000, // 硬上限 ~9000 字符，防止 memory 膨胀
	}
}

// NewWindow 创建动态上下文窗口管理器。
func NewWindow(opts ...WindowConfig) *Window {
	cfg := DefaultWindowConfig()
	if len(opts) > 0 {
		o := opts[0]
		if o.MaxContextTokens > 0 {
			cfg.MaxContextTokens = o.MaxContextTokens
		}
		if o.ReservedTokens > 0 {
			cfg.ReservedTokens = o.ReservedTokens
		}
		if o.OutputReserve > 0 {
			cfg.OutputReserve = o.OutputReserve
		}
		if o.MemoryBudgetRatio > 0 && o.MemoryBudgetRatio <= 1.0 {
			cfg.MemoryBudgetRatio = o.MemoryBudgetRatio
		}
		if o.MaxMemoryTokens > 0 {
			cfg.MaxMemoryTokens = o.MaxMemoryTokens
		}
		if o.CompressThreshold > 0 && o.CompressThreshold <= 1.0 {
			cfg.CompressThreshold = o.CompressThreshold
		}
	}
	return &Window{config: cfg}
}

// RecordUsage 记录一次 LLM 调用的 token 用量。
// 每次 LLM 返回结果后调用此方法更新窗口状态。
func (w *Window) RecordUsage(inputTokens, outputTokens int) {
	w.mu.Lock()
	w.usedTokens = inputTokens // 使用最新一轮的 input tokens 作为当前消耗
	w.roundCount++
	w.mu.Unlock()

	w.totalInputTokens.Add(int64(inputTokens))
	w.totalOutputTokens.Add(int64(outputTokens))
}

// Available 返回当前 memory 可用的 token 预算。
// 计算公式：
//
//	available = min((MaxContext - Reserved - OutputReserve - usedTokens) * MemoryBudgetRatio, MaxMemoryTokens)
//
// 返回值 <= 0 表示无剩余空间，应触发压缩或跳过 memory 注入。
// MaxMemoryTokens 为硬上限，防止 memory 预算随窗口变大而无限膨胀。
func (w *Window) Available() int {
	w.mu.RLock()
	used := w.usedTokens
	w.mu.RUnlock()

	totalAvailable := w.config.MaxContextTokens - w.config.ReservedTokens - w.config.OutputReserve - used
	if totalAvailable <= 0 {
		return 0
	}

	memoryBudget := int(float64(totalAvailable) * w.config.MemoryBudgetRatio)

	// 硬上限：防止大窗口模型（如 200K Claude）注入过多记忆
	if w.config.MaxMemoryTokens > 0 && memoryBudget > w.config.MaxMemoryTokens {
		memoryBudget = w.config.MaxMemoryTokens
	}

	return memoryBudget
}

// MemoryBudget 返回 memory 的 token 总预算（不考虑已消耗的 token）。
// 用于初始状态下的规划。同样受 MaxMemoryTokens 硬上限约束。
func (w *Window) MemoryBudget() int {
	totalAvailable := w.config.MaxContextTokens - w.config.ReservedTokens - w.config.OutputReserve
	if totalAvailable <= 0 {
		return 0
	}
	budget := int(float64(totalAvailable) * w.config.MemoryBudgetRatio)
	if w.config.MaxMemoryTokens > 0 && budget > w.config.MaxMemoryTokens {
		budget = w.config.MaxMemoryTokens
	}
	return budget
}

// ShouldCompress 判断给定 token 数是否超过压缩阈值。
// 当 memory context 的 token 数超过 Available * CompressThreshold 时返回 true。
func (w *Window) ShouldCompress(memoryTokens int) bool {
	available := w.Available()
	if available <= 0 {
		return memoryTokens > 0
	}
	threshold := int(float64(available) * w.config.CompressThreshold)
	return memoryTokens > threshold
}

// NeedsTruncation 判断给定 token 数是否超过可用空间（需要截断或压缩）。
func (w *Window) NeedsTruncation(memoryTokens int) bool {
	return memoryTokens > w.Available()
}

// RecordCompression 记录一次压缩触发。
func (w *Window) RecordCompression() {
	w.compressions.Add(1)
}

// Reset 重置窗口状态（新会话开始时调用）。
func (w *Window) Reset() {
	w.mu.Lock()
	w.usedTokens = 0
	w.roundCount = 0
	w.mu.Unlock()
}

// ============================================================================
// Metrics
// ============================================================================

// WindowMetrics 是窗口管理器的运行指标快照。
type WindowMetrics struct {
	MaxContextTokens   int   `json:"max_context_tokens"`
	UsedTokens         int   `json:"used_tokens"`
	AvailableForMemory int   `json:"available_for_memory"`
	RoundCount         int   `json:"round_count"`
	TotalInputTokens   int64 `json:"total_input_tokens"`
	TotalOutputTokens  int64 `json:"total_output_tokens"`
	Compressions       int64 `json:"compressions"`
}

// Metrics 返回当前窗口指标快照。
func (w *Window) Metrics() WindowMetrics {
	w.mu.RLock()
	used := w.usedTokens
	rounds := w.roundCount
	w.mu.RUnlock()

	return WindowMetrics{
		MaxContextTokens:   w.config.MaxContextTokens,
		UsedTokens:         used,
		AvailableForMemory: w.Available(),
		RoundCount:         rounds,
		TotalInputTokens:   w.totalInputTokens.Load(),
		TotalOutputTokens:  w.totalOutputTokens.Load(),
		Compressions:       w.compressions.Load(),
	}
}

// ============================================================================
// Snapshot / Restore — 持久化支持
// ============================================================================

// WindowState 是 Window 的可序列化状态快照。
// 用于持久化到外部存储（SQLite、文件等）并在重启后恢复。
type WindowState struct {
	UsedTokens        int   `json:"used_tokens"`
	RoundCount        int   `json:"round_count"`
	TotalInputTokens  int64 `json:"total_input_tokens"`
	TotalOutputTokens int64 `json:"total_output_tokens"`
	Compressions      int64 `json:"compressions"`
}

// Snapshot 导出当前窗口状态的快照（用于持久化）。
func (w *Window) Snapshot() WindowState {
	w.mu.RLock()
	used := w.usedTokens
	rounds := w.roundCount
	w.mu.RUnlock()

	return WindowState{
		UsedTokens:        used,
		RoundCount:        rounds,
		TotalInputTokens:  w.totalInputTokens.Load(),
		TotalOutputTokens: w.totalOutputTokens.Load(),
		Compressions:      w.compressions.Load(),
	}
}

// Restore 从持久化快照恢复窗口状态。
// 通常在 Bot 启动时调用，从存储层加载上次状态。
func (w *Window) Restore(state WindowState) {
	w.mu.Lock()
	w.usedTokens = state.UsedTokens
	w.roundCount = state.RoundCount
	w.mu.Unlock()

	w.totalInputTokens.Store(state.TotalInputTokens)
	w.totalOutputTokens.Store(state.TotalOutputTokens)
	w.compressions.Store(state.Compressions)
}
