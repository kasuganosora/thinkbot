package telegram

import (
	"context"
	"fmt"
	"strconv"

	agenttools "github.com/kasuganosora/thinkbot/agent/tools"
	"github.com/kasuganosora/thinkbot/llm"
)

// ============================================================================
// ChannelToolProvider 实现 — Telegram 平台专属工具
// ============================================================================

// ChannelTools 返回 TelegramChannel 提供的平台专属工具定义。
// 工具通过闭包捕获 Channel 的 API 客户端，支持跨 Channel 调用。
func (c *TelegramChannel) ChannelTools(ctx context.Context) ([]agenttools.ToolDef, error) {
	return []agenttools.ToolDef{
		c.banMemberTool(),
		c.unbanMemberTool(),
		c.getChatInfoTool(),
		c.pinMessageTool(),
	}, nil
}

// banMemberTool 返回 telegram_ban_member 工具定义。
func (c *TelegramChannel) banMemberTool() agenttools.ToolDef {
	return agenttools.ToolDef{
		Tool: llm.Tool{
			Name: "telegram_ban_member",
			Description: "在 Telegram 群组/频道中封禁一个成员（被封禁的用户无法重新加入）。" +
				"需要提供 chatId 和 userId（均为数字 ID）。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"chatId": map[string]any{
						"type":        "integer",
						"description": "目标群组/频道的 ID",
					},
					"userId": map[string]any{
						"type":        "integer",
						"description": "要封禁的用户 ID",
					},
				},
				"required": []string{"chatId", "userId"},
			},
			Execute: llm.ToolExecuteFunc(func(ctx *llm.ToolExecContext, input any) (any, error) {
				args, ok := input.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("telegram_ban_member: invalid input type")
				}
				chatID := toInt64(args["chatId"])
				userID := toInt64(args["userId"])
				if chatID == 0 || userID == 0 {
					return nil, fmt.Errorf("telegram_ban_member: chatId and userId are required")
				}
				if err := c.api.banChatMember(ctx, chatID, userID); err != nil {
					return nil, fmt.Errorf("ban member failed: %w", err)
				}
				return map[string]any{
					"success": true,
					"message": fmt.Sprintf("已将用户 %d 从 %d 中封禁", userID, chatID),
				}, nil
			}),
		},
		Category: "telegram",
	}
}

// unbanMemberTool 返回 telegram_unban_member 工具定义。
func (c *TelegramChannel) unbanMemberTool() agenttools.ToolDef {
	return agenttools.ToolDef{
		Tool: llm.Tool{
			Name: "telegram_unban_member",
			Description: "在 Telegram 群组/频道中解除一个成员的封禁。" +
				"需要提供 chatId 和 userId（均为数字 ID）。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"chatId": map[string]any{
						"type":        "integer",
						"description": "目标群组/频道的 ID",
					},
					"userId": map[string]any{
						"type":        "integer",
						"description": "要解除封禁的用户 ID",
					},
				},
				"required": []string{"chatId", "userId"},
			},
			Execute: llm.ToolExecuteFunc(func(ctx *llm.ToolExecContext, input any) (any, error) {
				args, ok := input.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("telegram_unban_member: invalid input type")
				}
				chatID := toInt64(args["chatId"])
				userID := toInt64(args["userId"])
				if chatID == 0 || userID == 0 {
					return nil, fmt.Errorf("telegram_unban_member: chatId and userId are required")
				}
				if err := c.api.unbanChatMember(ctx, chatID, userID); err != nil {
					return nil, fmt.Errorf("unban member failed: %w", err)
				}
				return map[string]any{
					"success": true,
					"message": fmt.Sprintf("已解除用户 %d 在 %d 中的封禁", userID, chatID),
				}, nil
			}),
		},
		Category: "telegram",
	}
}

// getChatInfoTool 返回 telegram_get_chat_info 工具定义。
func (c *TelegramChannel) getChatInfoTool() agenttools.ToolDef {
	return agenttools.ToolDef{
		Tool: llm.Tool{
			Name: "telegram_get_chat_info",
			Description: "获取 Telegram 群组、频道或私聊的详细信息。" +
				"需要提供 chatId（数字 ID）。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"chatId": map[string]any{
						"type":        "integer",
						"description": "目标聊天的 ID",
					},
				},
				"required": []string{"chatId"},
			},
			Execute: llm.ToolExecuteFunc(func(ctx *llm.ToolExecContext, input any) (any, error) {
				args, ok := input.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("telegram_get_chat_info: invalid input type")
				}
				chatID := toInt64(args["chatId"])
				if chatID == 0 {
					return nil, fmt.Errorf("telegram_get_chat_info: chatId is required")
				}
				chat, err := c.api.getChat(ctx, chatID)
				if err != nil {
					return nil, fmt.Errorf("get chat info failed: %w", err)
				}
				return map[string]any{
					"id":          chat.ID,
					"type":        chat.Type,
					"title":       chat.Title,
					"username":    chat.Username,
					"firstName":   chat.FirstName,
					"lastName":    chat.LastName,
					"description": chat.Description,
					"memberCount": chat.MemberCount,
				}, nil
			}),
		},
		Category: "telegram",
	}
}

// pinMessageTool 返回 telegram_pin_message 工具定义。
func (c *TelegramChannel) pinMessageTool() agenttools.ToolDef {
	return agenttools.ToolDef{
		Tool: llm.Tool{
			Name: "telegram_pin_message",
			Description: "在 Telegram 群组/频道中置顶一条消息。" +
				"需要提供 chatId 和 messageId（均为数字 ID）。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"chatId": map[string]any{
						"type":        "integer",
						"description": "目标群组/频道的 ID",
					},
					"messageId": map[string]any{
						"type":        "integer",
						"description": "要置顶的消息 ID",
					},
				},
				"required": []string{"chatId", "messageId"},
			},
			Execute: llm.ToolExecuteFunc(func(ctx *llm.ToolExecContext, input any) (any, error) {
				args, ok := input.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("telegram_pin_message: invalid input type")
				}
				chatID := toInt64(args["chatId"])
				messageID := toInt64(args["messageId"])
				if chatID == 0 || messageID == 0 {
					return nil, fmt.Errorf("telegram_pin_message: chatId and messageId are required")
				}
				if err := c.api.pinChatMessage(ctx, chatID, messageID); err != nil {
					return nil, fmt.Errorf("pin message failed: %w", err)
				}
				return map[string]any{
					"success": true,
					"message": fmt.Sprintf("消息 %d 已置顶", messageID),
				}, nil
			}),
		},
		Category: "telegram",
	}
}

// toInt64 将 interface{} 安全转换为 int64，支持 float64（JSON 默认数字类型）和 string。
func toInt64(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	case string:
		if i, err := strconv.ParseInt(n, 10, 64); err == nil {
			return i
		}
	}
	return 0
}
