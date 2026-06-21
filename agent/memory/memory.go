// Package memory 提供 agent 的记忆和上下文管理能力。
//
// 设计原则：
//   - DDD 领域驱动：Entry 是核心聚合根，Store/Retriever 是仓储层接口
//   - CQRS 读写分离：Store 只负责写入，Retriever 只负责检索
//   - 接口面向扩展：当前为内存实现，未来可替换为持久化后端（SQLite/Redis/向量DB）
//   - 可观测性内建：所有操作支持 tracing + metrics
//   - Scope 分层隔离：记忆按 channel/bot/user/global 分桶，互不干扰
//   - Think 标签过滤：写入前自动移除 LLM 深度思考标签，节省存储空间和 token
//
// 与 agent 模块的集成点：
//   - MemoryStage（Pipeline Stage）：在消息处理前检索上下文，处理后写入新记忆
//   - MemoryWriteStage：写入前自动调用 StripThinking 清理 <think> 标签
//   - ThinkFilterStore：Store 装饰器，可包装任意 Store 实现自动过滤
//   - NoteHandler 产出的 Note → 可选自动转存为 Memory Entry
//   - ContextBuilder 将记忆格式化为 LLM 可消费的 context 文本
//   - EventBus 旁路发射 memory.* 事件（检索命中、写入、淘汰等）
package memory

import (
	"context"
	"time"
)

// ============================================================================
// Scope — 记忆作用域（决定可见范围和存储分桶）
// ============================================================================

// Scope 标识一条记忆的可见范围。
// 记忆按 Scope 分桶存储和检索，不同 Scope 之间互不干扰。
type Scope struct {
	// Kind 作用域类型。
	Kind ScopeKind `json:"kind"`
	// ID 作用域标识（Channel ID / Bot ID / User ID 等）。
	// Global scope 时为空。
	ID string `json:"id,omitempty"`
}

// ScopeKind 定义记忆作用域类型。
type ScopeKind string

const (
	// ScopeChannel 会话级记忆（同一 Channel 内的所有消息共享）。
	// 最常用的 scope，对应一个"对话流"的记忆。
	// 典型场景：群聊上下文、私聊历史。
	ScopeChannel ScopeKind = "channel"

	// ScopeUser 用户级记忆（同一用户在不同 Channel 的记忆聚合）。
	// 典型场景：用户偏好、跨对话的长期记忆。
	ScopeUser ScopeKind = "user"

	// ScopeBot Bot 级记忆（Bot 自身的全局知识）。
	// 典型场景：Bot 学到的通用知识、系统配置备忘。
	ScopeBot ScopeKind = "bot"

	// ScopeGlobal 全局记忆（跨 Bot 共享）。
	// 典型场景：平台级配置、共享知识。
	ScopeGlobal ScopeKind = "global"
)

// Key 返回 Scope 的唯一标识键（用于存储分桶）。
func (s Scope) Key() string {
	if s.ID == "" {
		return string(s.Kind)
	}
	return string(s.Kind) + ":" + s.ID
}

// ChannelScope 创建会话级 scope。
func ChannelScope(channelID string) Scope {
	return Scope{Kind: ScopeChannel, ID: channelID}
}

// UserScope 创建用户级 scope。
func UserScope(userID string) Scope {
	return Scope{Kind: ScopeUser, ID: userID}
}

// BotScope 创建 Bot 级 scope。
func BotScope(botID string) Scope {
	return Scope{Kind: ScopeBot, ID: botID}
}

// GlobalScope 创建全局 scope。
func GlobalScope() Scope {
	return Scope{Kind: ScopeGlobal}
}

// ============================================================================
// Entry — 记忆条目（核心聚合根）
// ============================================================================

// Entry 表示一条记忆。
// 它是 memory 领域的核心值对象，承载 Bot 从对话/观察中积累的知识片段。
type Entry struct {
	// ID 记忆唯一标识。
	ID string `json:"id"`
	// Scope 记忆作用域（决定可见范围和存储分桶）。
	Scope Scope `json:"scope"`
	// Content 记忆内容文本。
	Content string `json:"content"`
	// Category 分类标签（"fact" / "preference" / "event" / "summary" / "observation" 等）。
	// 用于检索时的过滤和优先级排序。
	Category string `json:"category,omitempty"`
	// Source 记忆来源（"note" / "conversation" / "system" / "enricher"）。
	Source string `json:"source,omitempty"`
	// Importance 重要程度（0.0 ~ 1.0）。
	// 用于检索排序和淘汰策略。0 表示未评估。
	Importance float64 `json:"importance,omitempty"`
	// Metadata 扩展元数据。
	Metadata map[string]any `json:"metadata,omitempty"`
	// CreatedAt 创建时间。
	CreatedAt time.Time `json:"createdAt"`
	// LastAccessedAt 最后访问时间（用于 LRU 淘汰策略）。
	LastAccessedAt time.Time `json:"lastAccessedAt,omitempty"`
}

// ============================================================================
// Store — 记忆写入接口（命令侧）
// ============================================================================

// Store 定义记忆的写入能力。
// 遵循 CQRS 原则，只负责写入（创建/删除），不负责复杂检索。
//
// 设计要点：
//   - 接口足够小，方便多后端实现（内存 / SQLite / Redis）
//   - 每次写入按 scope 分桶
//   - Delete 支持按 ID 精确删除
//   - Clear 支持按 scope 批量清除
type Store interface {
	// Append 追加一条记忆到指定 scope。
	// 如果 entry.ID 为空，实现应自动生成。
	// 如果 entry.CreatedAt 为零值，实现应自动设为当前时间。
	Append(ctx context.Context, entry Entry) error

	// Delete 按 ID 删除指定 scope 下的一条记忆。
	// 记忆不存在时静默返回 nil。
	Delete(ctx context.Context, scope Scope, entryID string) error

	// Clear 清空指定 scope 的所有记忆。
	Clear(ctx context.Context, scope Scope) error
}

// ============================================================================
// Retriever — 记忆检索接口（查询侧）
// ============================================================================

// Query 描述一次记忆检索请求。
type Query struct {
	// Scopes 检索范围（可同时检索多个 scope）。
	// 空切片表示检索所有 scope（谨慎使用）。
	Scopes []Scope
	// Text 文本关键词（模糊匹配）。
	// 当前内存实现使用子串匹配；未来向量实现可做语义相似度搜索。
	Text string
	// Category 分类过滤（可选）。
	Category string
	// Limit 最多返回条目数（默认 10）。
	Limit int
	// MinImportance 最小重要度过滤（0 表示不过滤）。
	MinImportance float64
}

// Retriever 定义记忆的检索能力。
// 与 Store 分离，便于独立优化检索策略（加缓存、向量索引等）。
type Retriever interface {
	// Retrieve 根据查询条件检索记忆。
	// 返回按相关性/时间降序排列的记忆条目。
	Retrieve(ctx context.Context, query Query) ([]Entry, error)

	// Recent 获取指定 scope 的最近 N 条记忆（按时间倒序）。
	// 快捷方法，等价于不带 Text 过滤的 Retrieve。
	Recent(ctx context.Context, scope Scope, limit int) ([]Entry, error)

	// Count 返回指定 scope 的记忆总数。
	Count(ctx context.Context, scope Scope) (int, error)
}

// ============================================================================
// Repository — 完整仓储接口（组合 Store + Retriever）
// ============================================================================

// Repository 组合了 Store 和 Retriever，提供完整的记忆读写能力。
// 单一后端通常同时实现两者。
type Repository interface {
	Store
	Retriever
}

// Replacer 定义原子性替换能力。
// 将 Delete+Append 合并为单个锁内操作，避免中间状态导致的数据丢失。
// 实现此接口的存储后端（如 MemoryRepository）可被调用方优先使用，
// 未实现的则降级为 Append-before-Delete。
type Replacer interface {
	Replace(ctx context.Context, scope Scope, deleteID string, newEntry Entry) error
}
