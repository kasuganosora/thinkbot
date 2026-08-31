package command

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/agent/session"
)

// ============================================================================
// SessionAccessor — Session 访问接口
// ============================================================================

// SessionAccessor 提供命令处理器对 Session 的访问能力。
// 通常由 session.SessionManager 实现。
type SessionAccessor interface {
	// GetSession 根据 Envelope KV 中的 session 信息获取 Session。
	// 如果不存在或无 session，返回 nil。
	GetFromEnvelope(env *core.Envelope) *session.Session
}

// ============================================================================
// SessionManagerAccessor — 适配 session.SessionManager
// ============================================================================

// SessionManagerAccessor 包装 *session.SessionManager，实现 SessionAccessor 接口。
type SessionManagerAccessor struct {
	Mgr      *session.SessionManager
	Resolver session.SessionResolver
}

// GetFromEnvelope 实现 SessionAccessor 接口。
func (a *SessionManagerAccessor) GetFromEnvelope(env *core.Envelope) *session.Session {
	// 先从 Envelope KV 读取已解析的 session ID（上游 SessionStage 可能已注入）
	if id, ok := env.Get("session.id"); ok {
		if sid, ok := id.(string); ok && sid != "" {
			return a.Mgr.Get(sid)
		}
	}

	// 自行解析 session ID
	if a.Resolver != nil {
		result := a.Resolver.Resolve(context.Background(), &env.Message)
		if result.OK {
			s, _ := a.Mgr.GetOrCreate(result.SessionID, env.Message.BotID, env.Message.Channel, result.CreatedBy)
			return s
		}
	}
	return nil
}

// ============================================================================
// 内建命令：/clear
// ============================================================================

// ClearHandler 处理 /clear 命令。
// 清空当前 session 的工作记忆（对话上下文）。
// 系统提示词由 PromptStage 在下一条消息时自动重新组装，无需额外处理。
type ClearHandler struct {
	accessor SessionAccessor
}

// NewClearHandler 创建 /clear 命令处理器。
func NewClearHandler(accessor SessionAccessor) *ClearHandler {
	return &ClearHandler{accessor: accessor}
}

// Name 返回命令名称。
func (h *ClearHandler) Name() string { return "clear" }

// Description 返回命令描述。
func (h *ClearHandler) Description() string { return "清空当前会话上下文" }

// AdminOnly 命令是否需要管理员权限。
func (h *ClearHandler) AdminOnly() bool { return true }

// Execute 执行 /clear 命令。
func (h *ClearHandler) Execute(_ context.Context, env *core.Envelope, _ string) (*CommandResult, error) {
	if h.accessor == nil {
		return &CommandResult{Reply: "⚠️ Session 管理器未配置，无法执行此命令。", OK: false}, nil
	}

	s := h.accessor.GetFromEnvelope(env)
	if s == nil {
		return &CommandResult{Reply: "ℹ️ 当前没有活跃会话，无需清空。", OK: true}, nil
	}

	count := s.MessageCount()
	s.Clear()

	return &CommandResult{
		Reply: fmt.Sprintf("✅ 已清空会话上下文（移除 %d 条消息）。系统提示词将在下次对话时自动重载。", count),
		OK:    true,
	}, nil
}

// ============================================================================
// 内建命令：/compact
// ============================================================================

// CompactHandler 处理 /compact 命令。
// 压缩当前 session 的工作记忆，只保留最近 N 条消息。
type CompactHandler struct {
	accessor   SessionAccessor
	keepRecent int // 默认保留的最近消息数
}

// NewCompactHandler 创建 /compact 命令处理器。
// keepRecent <= 0 时使用默认值 3。
func NewCompactHandler(accessor SessionAccessor, keepRecent int) *CompactHandler {
	if keepRecent <= 0 {
		keepRecent = 3
	}
	return &CompactHandler{accessor: accessor, keepRecent: keepRecent}
}

// Name 返回命令名称。
func (h *CompactHandler) Name() string { return "compact" }

// Description 返回命令描述。
func (h *CompactHandler) Description() string {
	return "压缩当前会话上下文（保留最近 N 条消息）"
}

// AdminOnly 命令是否需要管理员权限。
func (h *CompactHandler) AdminOnly() bool { return true }

// Execute 执行 /compact 命令。
func (h *CompactHandler) Execute(_ context.Context, env *core.Envelope, args string) (*CommandResult, error) {
	if h.accessor == nil {
		return &CommandResult{Reply: "⚠️ Session 管理器未配置，无法执行此命令。", OK: false}, nil
	}

	s := h.accessor.GetFromEnvelope(env)
	if s == nil {
		return &CommandResult{Reply: "ℹ️ 当前没有活跃会话，无需压缩。", OK: true}, nil
	}

	keep := h.keepRecent
	if args != "" {
		if n, err := strconv.Atoi(args); err == nil && n > 0 {
			keep = n
		}
	}

	before := s.MessageCount()
	s.Compact(keep)
	after := s.MessageCount()

	return &CommandResult{
		Reply: fmt.Sprintf("✅ 已压缩会话上下文（%d → %d 条消息，保留最近 %d 条）。", before, after, keep),
		OK:    true,
	}, nil
}

// ============================================================================
// 内建命令：/help
// ============================================================================

// HelpHandler 处理 /help 命令。
// 列出所有已注册的命令及其描述。
type HelpHandler struct {
	registry *Registry
}

// NewHelpHandler 创建 /help 命令处理器。
func NewHelpHandler(registry *Registry) *HelpHandler {
	return &HelpHandler{registry: registry}
}

// Name 返回命令名称。
func (h *HelpHandler) Name() string { return "help" }

// Description 返回命令描述。
func (h *HelpHandler) Description() string { return "显示可用命令列表" }

// AdminOnly 命令是否需要管理员权限。
func (h *HelpHandler) AdminOnly() bool { return false }

// Execute 执行 /help 命令。
func (h *HelpHandler) Execute(_ context.Context, _ *core.Envelope, _ string) (*CommandResult, error) {
	commands := h.registry.List()
	if len(commands) == 0 {
		return &CommandResult{Reply: "ℹ️ 当前没有注册任何命令。", OK: true}, nil
	}

	var sb strings.Builder
	sb.WriteString("📋 **可用命令列表**\n\n")
	for _, cmd := range commands {
		prefix := ""
		if cmd.AdminOnly() {
			prefix = " 🔒"
		}
		fmt.Fprintf(&sb, "- `/%s`%s — %s\n", cmd.Name(), prefix, cmd.Description())
	}
	sb.WriteString("\n_🔒 = 仅管理员可用_")

	return &CommandResult{Reply: sb.String(), OK: true}, nil
}

// ============================================================================
// 内建命令：/status
// ============================================================================

// StatusHandler 处理 /status 命令。
// 显示当前 session 的状态信息。
type StatusHandler struct {
	accessor SessionAccessor
}

// NewStatusHandler 创建 /status 命令处理器。
func NewStatusHandler(accessor SessionAccessor) *StatusHandler {
	return &StatusHandler{accessor: accessor}
}

// Name 返回命令名称。
func (h *StatusHandler) Name() string { return "status" }

// Description 返回命令描述。
func (h *StatusHandler) Description() string { return "显示当前会话状态" }

// AdminOnly 命令是否需要管理员权限。
func (h *StatusHandler) AdminOnly() bool { return false }

// Execute 执行 /status 命令。
func (h *StatusHandler) Execute(_ context.Context, env *core.Envelope, _ string) (*CommandResult, error) {
	if h.accessor == nil {
		return &CommandResult{Reply: "⚠️ Session 管理器未配置。", OK: false}, nil
	}

	s := h.accessor.GetFromEnvelope(env)
	if s == nil {
		return &CommandResult{Reply: "ℹ️ 当前没有活跃会话。", OK: true}, nil
	}

	msg := fmt.Sprintf("📊 **会话状态**\n\n"+
		"- Session ID: `%s`\n"+
		"- 状态: %s\n"+
		"- 消息数: %d\n"+
		"- 话题: %s\n"+
		"- 创建时间: %s\n"+
		"- 最后活动: %s",
		s.ID(),
		s.Status(),
		s.MessageCount(),
		func() string {
			if t := s.Topic(); t != "" {
				return t
			}
			return "（未设置）"
		}(),
		s.StartedAt().Format("2006-01-02 15:04:05"),
		s.LastActivityAt().Format("2006-01-02 15:04:05"),
	)

	return &CommandResult{Reply: msg, OK: true}, nil
}

// ============================================================================
// 便捷函数：注册所有内建命令
// ============================================================================

// RegisterBuiltins 将所有内建命令注册到 Registry。
// accessor 为 nil 时跳过需要 session 的命令（/clear、/compact、/status）。
func RegisterBuiltins(registry *Registry, accessor SessionAccessor, keepRecent int) {
	if registry == nil {
		return
	}

	// /help 始终注册
	registry.MustRegister(NewHelpHandler(registry))

	if accessor != nil {
		registry.MustRegister(NewClearHandler(accessor))
		registry.MustRegister(NewCompactHandler(accessor, keepRecent))
		registry.MustRegister(NewStatusHandler(accessor))
	}
}
