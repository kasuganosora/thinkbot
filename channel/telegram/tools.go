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
		c.deleteMessageTool(),
		c.getChatInfoTool(),
		c.getChatMemberCountTool(),
		c.getChatAdministratorsTool(),
		c.pinMessageTool(),
	}, nil
}

// banMemberTool 返回 telegram_ban_member 工具定义。
func (c *TelegramChannel) banMemberTool() agenttools.ToolDef {
	return agenttools.ToolDef{
		Tool: llm.Tool{
			Name: "telegram_ban_member",
			Description: "在 Telegram 群组/频道中封禁一个成员。默认永久封禁。" +
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
					"untilDate": map[string]any{
						"type":        "integer",
						"description": "解封时间（Unix 时间戳）。0 或不传表示永久封禁",
					},
					"revokeMessages": map[string]any{
						"type":        "boolean",
						"description": "是否同时删除该用户所有消息。默认 false",
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
				untilDate := toInt64(args["untilDate"])
				revokeMessages, _ := args["revokeMessages"].(bool)
				if err := c.api.banChatMember(ctx, chatID, userID, untilDate, revokeMessages); err != nil {
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
				"需要提供 chatId 和 userId（均为数字 ID）。默认仅在被封状态下执行。",
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
					"onlyIfBanned": map[string]any{
						"type":        "boolean",
						"description": "仅当用户当前被封禁时才执行。默认 true（安全模式）",
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
				onlyIfBanned := true // 默认安全模式
				if v, ok := args["onlyIfBanned"].(bool); ok {
					onlyIfBanned = v
				}
				if err := c.api.unbanChatMember(ctx, chatID, userID, onlyIfBanned); err != nil {
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

// deleteMessageTool 返回 telegram_delete_message 工具定义。
func (c *TelegramChannel) deleteMessageTool() agenttools.ToolDef {
	return agenttools.ToolDef{
		Tool: llm.Tool{
			Name: "telegram_delete_message",
			Description: "在 Telegram 群组/频道中删除一条消息。" +
				"Bot 必须有删除消息的权限。需要提供 chatId 和 messageId。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"chatId": map[string]any{
						"type":        "integer",
						"description": "目标群组/频道的 ID",
					},
					"messageId": map[string]any{
						"type":        "integer",
						"description": "要删除的消息 ID",
					},
				},
				"required": []string{"chatId", "messageId"},
			},
			Execute: llm.ToolExecuteFunc(func(ctx *llm.ToolExecContext, input any) (any, error) {
				args, ok := input.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("telegram_delete_message: invalid input type")
				}
				chatID := toInt64(args["chatId"])
				messageID := toInt64(args["messageId"])
				if chatID == 0 || messageID == 0 {
					return nil, fmt.Errorf("telegram_delete_message: chatId and messageId are required")
				}
				if err := c.api.deleteMessage(ctx, chatID, messageID); err != nil {
					return nil, fmt.Errorf("delete message failed: %w", err)
				}
				return map[string]any{
					"success": true,
					"message": fmt.Sprintf("消息 %d 已删除", messageID),
				}, nil
			}),
		},
		Category: "telegram",
	}
}

// getChatMemberCountTool 返回 telegram_get_chat_member_count 工具定义。
func (c *TelegramChannel) getChatMemberCountTool() agenttools.ToolDef {
	return agenttools.ToolDef{
		Tool: llm.Tool{
			Name: "telegram_get_chat_member_count",
			Description: "获取 Telegram 群组/频道的成员数量。" +
				"比 getChatInfo 更轻量，仅返回成员数。需要提供 chatId。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"chatId": map[string]any{
						"type":        "integer",
						"description": "目标群组/频道的 ID",
					},
				},
				"required": []string{"chatId"},
			},
			Execute: llm.ToolExecuteFunc(func(ctx *llm.ToolExecContext, input any) (any, error) {
				args, ok := input.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("telegram_get_chat_member_count: invalid input type")
				}
				chatID := toInt64(args["chatId"])
				if chatID == 0 {
					return nil, fmt.Errorf("telegram_get_chat_member_count: chatId is required")
				}
				count, err := c.api.getChatMemberCount(ctx, chatID)
				if err != nil {
					return nil, fmt.Errorf("get member count failed: %w", err)
				}
				return map[string]any{
					"chatId":      chatID,
					"memberCount": count,
				}, nil
			}),
		},
		Category: "telegram",
	}
}

// getChatAdministratorsTool 返回 telegram_get_chat_administrators 工具定义。
func (c *TelegramChannel) getChatAdministratorsTool() agenttools.ToolDef {
	return agenttools.ToolDef{
		Tool: llm.Tool{
			Name: "telegram_get_chat_administrators",
			Description: "获取 Telegram 群组/频道的管理员列表。" +
				"返回管理员的 user ID、username、角色（creator/administrator）等信息。需要提供 chatId。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"chatId": map[string]any{
						"type":        "integer",
						"description": "目标群组/频道的 ID",
					},
				},
				"required": []string{"chatId"},
			},
			Execute: llm.ToolExecuteFunc(func(ctx *llm.ToolExecContext, input any) (any, error) {
				args, ok := input.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("telegram_get_chat_administrators: invalid input type")
				}
				chatID := toInt64(args["chatId"])
				if chatID == 0 {
					return nil, fmt.Errorf("telegram_get_chat_administrators: chatId is required")
				}
				admins, err := c.api.getChatAdministrators(ctx, chatID)
				if err != nil {
					return nil, fmt.Errorf("get administrators failed: %w", err)
				}
				var results []map[string]any
				for _, a := range admins {
					displayName := a.User.FirstName
					if a.User.LastName != "" {
						displayName += " " + a.User.LastName
					}
					if a.User.Username != "" {
						displayName = fmt.Sprintf("@%s (%s)", a.User.Username, displayName)
					}
					results = append(results, map[string]any{
						"userId":   a.User.ID,
						"username": a.User.Username,
						"name":     displayName,
						"role":     a.Status,
						"isBot":    a.User.IsBot,
					})
				}
				return map[string]any{
					"administrators": results,
					"count":          len(results),
					"chatId":         chatID,
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
					"disableNotification": map[string]any{
						"type":        "boolean",
						"description": "是否静默置顶（不发通知）。默认 false",
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
				disableNotification, _ := args["disableNotification"].(bool)
				if err := c.api.pinChatMessage(ctx, chatID, messageID, disableNotification); err != nil {
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
