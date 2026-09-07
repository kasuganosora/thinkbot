package command

import (
	"context"
	"fmt"
	"strings"

	"github.com/kasuganosora/thinkbot/agent/core"
)

// ============================================================================
// 内建命令：/chatid
// ============================================================================

// ChatIDHandler 处理 /chatid 命令。
// 返回当前会话的标识信息（群/会话 ID、发送者 ID、平台账号名），
// 便于用户把群 ID 直接粘进「工具权限」规则的「会话/群 ID」字段，
// 从而实现「仅对某个 tg 群单独配置工具权限」。
//
// 要求发送者已绑定 thinkbot 内部账号（RequireBound）：只有绑定过的用户
// 才需要、也才配得上这套权限配置能力，未绑定者拿不到任何标识信息。
type ChatIDHandler struct{}

// NewChatIDHandler 创建 /chatid 命令处理器。
func NewChatIDHandler() *ChatIDHandler { return &ChatIDHandler{} }

// Name 返回命令名称。
func (h *ChatIDHandler) Name() string { return "chatid" }

// Description 返回命令描述。
func (h *ChatIDHandler) Description() string {
	return "显示当前会话/群的 ID（需绑定账号，用于工具权限按群配置）"
}

// AdminOnly 命令是否需要管理员权限。
func (h *ChatIDHandler) AdminOnly() bool { return false }

// RequireBound 命令是否要求绑定账号。
func (h *ChatIDHandler) RequireBound() bool { return true }

// Execute 执行 /chatid 命令。
func (h *ChatIDHandler) Execute(_ context.Context, env *core.Envelope, _ string) (*CommandResult, error) {
	chatID := env.Message.Channel
	userID := env.Message.UserID
	platform := env.Message.Source

	username := ""
	if env.Message.Metadata != nil {
		if u, ok := env.Message.Metadata["username"].(string); ok {
			username = u
		}
	}

	var b strings.Builder
	b.WriteString("🆔 **当前会话标识**\n\n")
	b.WriteString(fmt.Sprintf("- 会话/群 ID：`%s`\n", chatID))
	b.WriteString(fmt.Sprintf("- 你的用户 ID：`%s`\n", userID))
	if username != "" {
		b.WriteString(fmt.Sprintf("- 你的平台账号：`%s`\n", username))
	}
	b.WriteString(fmt.Sprintf("- 来源平台：`%s`\n\n", platform))
	b.WriteString("把上面的「会话/群 ID」粘进「工具权限」规则的 **会话/群 ID** 字段，\n")
	b.WriteString("即可只对该群单独配置工具权限（留空则对该平台全部会话生效）。")

	return &CommandResult{Reply: b.String(), OK: true}, nil
}
