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

// channelToolAwarenessSection 纠正 bot 在 Telegram 上的自我认知，并交代内容格式。
//
// 与 misskey 的同名段落同源：bot 既能接收也能发言，回复直接用文本即可。
// 但 Telegram 有一条 misskey 没有的硬约束——出站文本若与 parse_mode 不匹配，
// Telegram 会**整条 400 丢弃**（比 misskey 裸显示标签更糟：对方什么都收不到）。
// 因此在默认纯文本下必须禁 HTML，并预先交代 MarkdownV2 的转义义务。
var channelToolAwarenessSection = &agenttools.ToolPromptSection{
	Name:    "telegram_observability",
	Order:   300,
	Enabled: true,
	Content: `# Telegram Capabilities

You are NOT "write-only" and NOT "tool-less" on Telegram. You both RECEIVE and CAN REPLY.

## Reactions on your messages
When someone reacts to your message you may receive a [Telegram 反应] InjectContext. That is awareness only — do not reply, react back, or call tools for it. Just know they reacted. Telegram only delivers these updates when the bot is a group administrator; DMs and non-admin groups typically never see them.

## Replying (just use text, do NOT call a tool)
When someone @mentions you, replies to you, or sends you a command, simply reply with your normal text — the system sends it for you.
- You do NOT need, and should NOT, call any tool to reply. There is no "send message" tool: your final reply text IS the message that gets delivered.
- Therefore never say "I have no tool to reply" — you can always reply with text.
- Your final reply text is delivered verbatim. Write the reply content directly (e.g. "Got it! Here's my answer~"), and do NOT write operational reports like "I have replied / I pinned the message / the tool call failed" — those are handled by the system, not something you say to the user.

## Reply content format (Telegram-specific — read carefully)
Telegram rejects the ENTIRE message if its formatting does not match the configured parse mode. A rejected message means the other party receives nothing at all — worse than ugly formatting. Follow these rules:
- The default is PLAIN TEXT. NEVER use HTML tags: no <b>, <i>, <p>, <ul>, <li>, <br>, <div>, <span>. They will either show up as literal garbage or get the message rejected.
- NEVER invent wrapper tags like <long>, <summary>, <note>, <details>. The ONLY wrapper tags you may use are the framework's <public> (your public reply) and <internal> (private thoughts) — both defined by the reply-control protocol. Anything else is stripped and may mangle your text.
- Format with plain text: line breaks, "- item" for lists, quotes, and spacing. This is always safe.
- If (and only if) you are explicitly told that Markdown formatting is enabled for this channel, you may use Markdown — but then you MUST escape the special characters _ * [ ] ( ) ~ > # + - = | { } . ! when you mean them literally, otherwise the whole message is rejected.
- When in doubt, write plain text.

## Group behavior
In groups you are only triggered when you are @mentioned, when someone replies to your message, or when a command is addressed to you. Casual chatter between other people is not yours to answer.
- Do NOT announce "I wasn't mentioned" or explain your trigger conditions. If you were triggered, just respond to the actual content.
- If a message is clearly not addressed to you, stay silent rather than explaining why.

## Tools you have
- Read-only (safe, use freely): telegram_get_chat_info, telegram_get_chat_member_count, telegram_get_chat_administrators.
- Write operations (irreversible and visible to everyone — be certain before using): telegram_ban_member, telegram_unban_member, telegram_delete_message, telegram_pin_message.
- Chat IDs are numeric. Use the chatId from the current conversation context when it is the same chat.

## Internal information you must NEVER tell the user (hard rules)
- Whether a tool call succeeds or fails, NEVER reveal any internal process: do NOT write "let me check", "tool call failed", "HTTP 400", "parse error", "rate limited", or similar.
- When a tool fails: quietly respond in another natural way, or simply don't mention it — just like a real person wouldn't read backend errors aloud to the other party.
- Your final reply should only contain plain human language.`,
}

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
		Category:      "telegram",
		PromptSection: channelToolAwarenessSection,
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
		Category:      "telegram",
		PromptSection: channelToolAwarenessSection,
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
		Category:      "telegram",
		PromptSection: channelToolAwarenessSection,
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
