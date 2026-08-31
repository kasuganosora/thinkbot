package command

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/kasuganosora/thinkbot/agent/core"
)

// ============================================================================
// AdminChecker — 管理员权限检查接口
// ============================================================================

// AdminChecker 检查指定用户是否为管理员。
// 调用方可提供基于 auth.AuthService + identity 映射的实现，或使用简单的 admin ID 集合。
type AdminChecker interface {
	// IsAdmin 返回指定来源的用户是否拥有管理员权限。
	// source 是消息来源标识（如 "telegram-bot1"、"web-bot1"），
	// userID 是平台侧用户 ID（Web 渠道下为内部用户 ID 的字符串形式）。
	IsAdmin(ctx context.Context, source, userID string) bool
}

// AdminCheckerFunc 将函数适配为 AdminChecker 接口。
type AdminCheckerFunc func(ctx context.Context, source, userID string) bool

// IsAdmin 实现 AdminChecker 接口。
func (f AdminCheckerFunc) IsAdmin(ctx context.Context, source, userID string) bool {
	return f(ctx, source, userID)
}

// StaticAdminChecker 基于 source+userID 组合的简单管理员检查器。
type StaticAdminChecker struct {
	admins map[string]bool
}

// NewStaticAdminChecker 创建静态管理员检查器。
// adminKeys 格式为 "platform:userID"（如 "telegram:123456789"）。
func NewStaticAdminChecker(adminKeys ...string) *StaticAdminChecker {
	m := make(map[string]bool, len(adminKeys))
	for _, k := range adminKeys {
		m[k] = true
	}
	return &StaticAdminChecker{admins: m}
}

// IsAdmin 实现 AdminChecker 接口。
func (c *StaticAdminChecker) IsAdmin(_ context.Context, source, userID string) bool {
	return c.admins[source+":"+userID]
}

// AllowAllChecker 始终返回 true（用于测试或开放环境）。
type AllowAllChecker struct{}

// IsAdmin 实现 AdminChecker 接口。
func (AllowAllChecker) IsAdmin(context.Context, string, string) bool { return true }

// ============================================================================
// CommandHandler — 命令处理器接口
// ============================================================================

// CommandResult 是命令执行的结果。
type CommandResult struct {
	// Reply 要回复给用户的文本（空字符串表示不回复）。
	Reply string
	// OK 命令是否执行成功（影响日志级别和回复格式）。
	OK bool
}

// CommandHandler 处理一个特定的斜杠命令。
type CommandHandler interface {
	// Name 命令名称（不含 /），如 "clear"、"compact"。
	Name() string
	// Description 命令描述（用于 /help）。
	Description() string
	// AdminOnly 是否仅管理员可执行。
	AdminOnly() bool
	// Execute 执行命令。
	// args 是命令后面的参数文本（已 trim）。
	// env 是当前消息信封，可通过 env.Set/Get 读取 session 等 KV。
	Execute(ctx context.Context, env *core.Envelope, args string) (*CommandResult, error)
}

// ============================================================================
// Registry — 命令注册表
// ============================================================================

// Registry 管理已注册的命令处理器。
// 线程安全。
type Registry struct {
	mu       sync.RWMutex
	commands map[string]CommandHandler
}

// NewRegistry 创建空命令注册表。
func NewRegistry() *Registry {
	return &Registry{commands: make(map[string]CommandHandler)}
}

// Register 注册一个命令处理器。
// 如果命令名已存在，返回 error。
func (r *Registry) Register(h CommandHandler) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	name := strings.ToLower(h.Name())
	if _, exists := r.commands[name]; exists {
		return fmt.Errorf("command %q already registered", name)
	}
	r.commands[name] = h
	return nil
}

// MustRegister 注册命令，冲突时 panic。
func (r *Registry) MustRegister(h CommandHandler) {
	if err := r.Register(h); err != nil {
		panic(err)
	}
}

// Lookup 查找命令处理器（大小写不敏感）。
func (r *Registry) Lookup(name string) (CommandHandler, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	h, ok := r.commands[strings.ToLower(name)]
	return h, ok
}

// List 返回所有已注册的命令（无特定顺序）。
func (r *Registry) List() []CommandHandler {
	r.mu.RLock()
	defer r.mu.RUnlock()
	list := make([]CommandHandler, 0, len(r.commands))
	for _, h := range r.commands {
		list = append(list, h)
	}
	return list
}

// ============================================================================
// CommandFunc — 将函数适配为 CommandHandler
// ============================================================================

// CommandFunc 将函数适配为 CommandHandler 接口。
type CommandFunc struct {
	CmdName      string
	CmdDesc      string
	CmdAdminOnly bool
	Fn           func(ctx context.Context, env *core.Envelope, args string) (*CommandResult, error)
}

// Name 实现 CommandHandler 接口。
func (c *CommandFunc) Name() string { return c.CmdName }

// Description 实现 CommandHandler 接口。
func (c *CommandFunc) Description() string { return c.CmdDesc }

// AdminOnly 实现 CommandHandler 接口。
func (c *CommandFunc) AdminOnly() bool { return c.CmdAdminOnly }

// Execute 实现 CommandHandler 接口。
func (c *CommandFunc) Execute(ctx context.Context, env *core.Envelope, args string) (*CommandResult, error) {
	return c.Fn(ctx, env, args)
}
