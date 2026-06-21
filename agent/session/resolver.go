package session

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/util/strutil"
)

// ============================================================================
// SessionResolver — 会话解析器（决定消息属于哪个 session）
// ============================================================================

// ResolveResult 是 SessionResolver.Resolve 的返回结果。
type ResolveResult struct {
	// SessionID 解析出的会话 ID。ok=false 时为空。
	SessionID string
	// OK 是否应该为此消息创建/获取 session。
	// false 表示此消息不参与 session（如时间线观察帖）。
	OK bool
	// CreatedBy 如果创建新 session，标记发起者。
	// "user" = 用户发起（被 @ 等），"bot" = Bot 主动参与。
	CreatedBy string
}

// SessionResolver 决定一条消息是否属于/应创建 session。
//
// 不同平台提供不同实现：
//   - Telegram: 一个 chat = 一个 session
//   - Misskey: 回复链 = 一个 session；时间线帖子返回 ok=false
//   - RSS/Feed: 永远返回 ok=false
type SessionResolver interface {
	// Resolve 根据消息内容判断其所属 session。
	Resolve(ctx context.Context, msg *core.Message) ResolveResult
}

// ============================================================================
// DefaultResolver — 通用 session 解析器
// ============================================================================

// DefaultResolver 是一个基于规则的通用 session 解析器。
//
// 解析策略（按优先级）：
//  1. 如果消息有 reply_id（metadata，表示本消息是回复另一条消息的）→ thread session
//  2. 如果消息被 @（Mentioned=true）→ channel session
//  3. 其他情况返回 ok=false（不创建 session）
//
// 注意：reply_target 不用于 session 解析——它是 outbound 回复目标标识，
// 每条消息都有，不代表对话链关系。
type DefaultResolver struct {
	// Prefix session ID 前缀（用于区分不同 channel 来源）。
	Prefix string
}

// NewDefaultResolver 创建通用解析器。
func NewDefaultResolver(prefix string) *DefaultResolver {
	if prefix == "" {
		prefix = "session"
	}
	return &DefaultResolver{Prefix: prefix}
}

// Resolve 实现 SessionResolver 接口。
func (r *DefaultResolver) Resolve(_ context.Context, msg *core.Message) ResolveResult {
	// 1. 有 reply_id（本消息是回复别人的）→ thread session
	if replyID, ok := msg.Metadata["reply_id"].(string); ok && replyID != "" {
		return ResolveResult{
			SessionID: fmt.Sprintf("%s:thread:%s", r.Prefix, replyID),
			OK:        true,
			CreatedBy: "user",
		}
	}

	// 2. 被 @ → channel session
	if msg.Mentioned {
		return ResolveResult{
			SessionID: fmt.Sprintf("%s:channel:%s", r.Prefix, msg.Channel),
			OK:        true,
			CreatedBy: "user",
		}
	}

	// 3. 其他 → 无 session
	return ResolveResult{OK: false}
}

// ============================================================================
// ChannelResolver — 基于 channel type 的复合解析器
// ============================================================================

// ChannelResolver 根据 Message.Source 路由到不同的子解析器。
// 未注册的 source 回退到 DefaultResolver。
type ChannelResolver struct {
	mu              sync.RWMutex
	defaultResolver SessionResolver
	bySource        map[string]SessionResolver
}

// NewChannelResolver 创建基于来源的复合解析器。
func NewChannelResolver() *ChannelResolver {
	return &ChannelResolver{
		defaultResolver: NewDefaultResolver("session"),
		bySource:        make(map[string]SessionResolver),
	}
}

// Register 为指定 source 注册专用解析器。
func (r *ChannelResolver) Register(source string, resolver SessionResolver) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.bySource[source] = resolver
}

// Resolve 实现SessionResolver 接口。
func (r *ChannelResolver) Resolve(ctx context.Context, msg *core.Message) ResolveResult {
	r.mu.RLock()
	resolver, ok := r.bySource[msg.Source]
	r.mu.RUnlock()
	if ok {
		return resolver.Resolve(ctx, msg)
	}
	return r.defaultResolver.Resolve(ctx, msg)
}

// ============================================================================
// MisskeyResolver — Misskey 平台专用解析器
// ============================================================================

// MisskeyResolver 实现 Misskey 平台的 session 解析逻辑。
//
//   - 有 reply_id（在一个帖子树里）→ thread session
//   - 被 @ → channel session（用户 ID）
//   - 时间线帖子（无 reply、未 @）→ ok=false
type MisskeyResolver struct{}

// NewMisskeyResolver 创建 Misskey 解析器。
func NewMisskeyResolver() *MisskeyResolver {
	return &MisskeyResolver{}
}

// Resolve 实现 SessionResolver 接口。
//
// Misskey 解析逻辑：
//   - 有 reply_id（本帖是回复别人的）→ thread session，以 parent note ID 为 key
//   - 被 @（Mentioned=true）→ channel session（以 channel 为 key）
//   - 时间线原创帖（无 reply_id、未被 @）→ ok=false，不创建 session
//
// 注意：reply_target metadata 不用于 session 解析。
// reply_target 是 outbound 用的"回复目标 noteID"（每个 note 都有），
// 不是"本帖回复了谁"的标记。只有 reply_id 才表示本帖参与了一个对话链。
func (r *MisskeyResolver) Resolve(_ context.Context, msg *core.Message) ResolveResult {
	// 有 reply_id → 本帖是某条帖子的回复，属于对话链 → thread session
	if replyID, ok := msg.Metadata["reply_id"].(string); ok && replyID != "" {
		return ResolveResult{
			SessionID: fmt.Sprintf("mk:thread:%s", replyID),
			OK:        true,
			CreatedBy: "user",
		}
	}

	// 被 @ → channel session
	if msg.Mentioned {
		return ResolveResult{
			SessionID: fmt.Sprintf("mk:channel:%s", msg.Channel),
			OK:        true,
			CreatedBy: "user",
		}
	}

	// 时间线原创帖 → 无 session（engagement 层可决定是否升级）
	return ResolveResult{OK: false}
}

// ============================================================================
// TelegramResolver — Telegram 平台专用解析器
// ============================================================================

// TelegramResolver 实现 Telegram 平台的 session 解析。
// Telegram 的每个 chat 天然是一个连续对话流。
type TelegramResolver struct{}

// NewTelegramResolver 创建 Telegram 解析器。
func NewTelegramResolver() *TelegramResolver {
	return &TelegramResolver{}
}

// Resolve 实现 SessionResolver 接口。
func (r *TelegramResolver) Resolve(_ context.Context, msg *core.Message) ResolveResult {
	// Telegram chat 天然是连续对话，直接按 channel 建 session
	return ResolveResult{
		SessionID: fmt.Sprintf("tg:%s", msg.Channel),
		OK:        true,
		CreatedBy: "user",
	}
}

// ============================================================================
// NeverResolver — 永不创建 session（用于 RSS 等纯信息流）
// ============================================================================

// NeverResolver 对所有消息返回 ok=false。
type NeverResolver struct{}

// NewNeverResolver 创建永不解析的 resolver。
func NewNeverResolver() *NeverResolver {
	return &NeverResolver{}
}

// Resolve 实现 SessionResolver 接口。
func (r *NeverResolver) Resolve(_ context.Context, _ *core.Message) ResolveResult {
	return ResolveResult{OK: false}
}

// ============================================================================
// ResolveResult 便捷方法
// ============================================================================

// String 返回 ResolveResult 的调试字符串。
func (r ResolveResult) String() string {
	if !r.OK {
		return "ResolveResult{ok=false}"
	}
	return fmt.Sprintf("ResolveResult{session=%s, creator=%s}", r.SessionID, r.CreatedBy)
}

// ============================================================================
// SessionContextBuilder — 将 session 工作记忆格式化为上下文文本
// ============================================================================

// FormatContext 将 session 的工作记忆格式化为 LLM 可消费的上下文文本。
// 通常注入到 Envelope KV 中供下游 Stage 使用。
func FormatContext(s *Session, maxMessages int) string {
	messages := s.RecentMessages(maxMessages)
	if len(messages) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("[Session Context]\n")

	if topic := s.Topic(); topic != "" {
		sb.WriteString("Topic: ")
		sb.WriteString(topic)
		sb.WriteByte('\n')
	}

	for _, msg := range messages {
		ts := msg.Timestamp.Format("15:04:05")
		role := msg.Role
		if role == "" {
			role = "unknown"
		}
		fmt.Fprintf(&sb, "[%s] %s: %s\n", ts, role, strutil.Truncate(msg.Text, 200))
	}

	sb.WriteString("[End Session Context]")
	return sb.String()
}

// SessionContextFromEnvelope 从 Envelope 中提取 session 上下文文本。
func SessionContextFromEnvelope(env *core.Envelope) string {
	if v, ok := env.Get("session.context"); ok {
		if text, ok := v.(string); ok {
			return text
		}
	}
	return ""
}

// SessionIDFromEnvelope 从 Envelope 中提取 session ID。
func SessionIDFromEnvelope(env *core.Envelope) string {
	if v, ok := env.Get("session.id"); ok {
		if id, ok := v.(string); ok {
			return id
		}
	}
	return ""
}

// IsNewSessionFromEnvelope 检查 Envelope 标记的是否是新创建的 session。
func IsNewSessionFromEnvelope(env *core.Envelope) bool {
	if v, ok := env.Get("session.is_new"); ok {
		if isNew, ok := v.(bool); ok {
			return isNew
		}
	}
	return false
}
