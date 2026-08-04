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
			Description: "Ban a member from a Telegram group or channel. " +
				"Requires chatId and userId (both numeric IDs). " +
				"IMPORTANT: The ban is permanent unless untilDate is set.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"chatId": map[string]any{
						"type":        "integer",
						"description": "ID of the target group or channel",
					},
					"userId": map[string]any{
						"type":        "integer",
						"description": "ID of the user to ban",
					},
					"untilDate": map[string]any{
						"type":        "integer",
						"description": "Unix timestamp at which the ban is lifted. 0 or omitted means a permanent ban",
					},
					"revokeMessages": map[string]any{
						"type":        "boolean",
						"description": "Whether to also delete all messages from this user. Defaults to false",
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
			Description: "Lift a ban on a member in a Telegram group or channel. " +
				"Requires chatId and userId (both numeric IDs). " +
				"IMPORTANT: By default this only acts on users who are currently banned.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"chatId": map[string]any{
						"type":        "integer",
						"description": "ID of the target group or channel",
					},
					"userId": map[string]any{
						"type":        "integer",
						"description": "ID of the user to unban",
					},
					"onlyIfBanned": map[string]any{
						"type":        "boolean",
						"description": "Only run if the user is currently banned. Defaults to true (safe mode)",
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
			Description: "Delete a message in a Telegram group or channel. " +
				"Requires chatId and messageId. " +
				"IMPORTANT: The bot MUST hold delete-message permission in that chat, otherwise the call fails.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"chatId": map[string]any{
						"type":        "integer",
						"description": "ID of the target group or channel",
					},
					"messageId": map[string]any{
						"type":        "integer",
						"description": "ID of the message to delete",
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
			Description: "Get the member count of a Telegram group or channel. Requires chatId. " +
				"ALWAYS prefer this over telegram_get_chat_info when the member count is all you need — it returns only that number.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"chatId": map[string]any{
						"type":        "integer",
						"description": "ID of the target group or channel",
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
			Description: "List the administrators of a Telegram group or channel. Requires chatId. " +
				"Returns each administrator's user ID, username, and role (creator/administrator).",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"chatId": map[string]any{
						"type":        "integer",
						"description": "ID of the target group or channel",
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
			Description: "Get detailed information about a Telegram group, channel, or private chat. " +
				"Requires chatId (numeric ID).",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"chatId": map[string]any{
						"type":        "integer",
						"description": "ID of the target chat",
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
			Description: "Pin a message in a Telegram group or channel. " +
				"Requires chatId and messageId (both numeric IDs).",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"chatId": map[string]any{
						"type":        "integer",
						"description": "ID of the target group or channel",
					},
					"messageId": map[string]any{
						"type":        "integer",
						"description": "ID of the message to pin",
					},
					"disableNotification": map[string]any{
						"type":        "boolean",
						"description": "Whether to pin silently without notifying members. Defaults to false",
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
