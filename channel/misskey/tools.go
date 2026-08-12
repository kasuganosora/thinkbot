package misskey

import (
	"context"
	"fmt"
	"strings"

	agenttools "github.com/kasuganosora/thinkbot/agent/tools"
	"github.com/kasuganosora/thinkbot/llm"
)

// ============================================================================
// ChannelToolProvider 实现 — Misskey 平台专属工具
// ============================================================================

// channelToolAwarenessSection 纠正 bot 的自我认知：它在 Misskey 上并非「只写不读」。
// 它既会被动接收 mention/reply/timeline 消息，也拥有主动读取帖子的工具。
// 挂在新读取工具的 PromptSection 上，随 misskey 通道一起注入系统提示。
var channelToolAwarenessSection = &agenttools.ToolPromptSection{
	Name:    "misskey_observability",
	Order:   300,
	Enabled: true,
	Content: `# Misskey Observability

You are NOT "write-only" on Misskey. Besides the action tools (post, renote, react, follow, search user), you ALSO receive Misskey content automatically as normal incoming messages — no tool call is needed to "see" them:

- Direct mentions and replies addressed to you always arrive, so you can respond.
- The bot subscribes to the home, local, and hybrid timelines; notes from those timelines are delivered to you as [Timeline] messages that you can observe, learn from, or react to.

For on-demand reading — e.g. "what did @user post recently" or "search notes about X" — use the misskey_get_user_notes and misskey_search_notes tools below. Prefer them when you need specific history that has not streamed by yet. Do NOT claim you cannot read Misskey; you can both observe and act.`,
}

// ChannelTools 返回 MisskeyChannel 提供的平台专属工具定义。
// 工具通过闭包捕获 Channel 的 API 客户端，支持跨 Channel 调用。
func (c *MisskeyChannel) ChannelTools(ctx context.Context) ([]agenttools.ToolDef, error) {
	return []agenttools.ToolDef{
		c.followUserTool(),
		c.unfollowUserTool(),
		c.createNoteTool(),
		c.createRenoteTool(),
		c.deleteNoteTool(),
		c.reactToNoteTool(),
		c.unreactToNoteTool(),
		c.searchUserTool(),
		c.listFollowingTool(),
		c.getUserNotesTool(),
		c.searchNotesTool(),
	}, nil
}

// formatNotes 把帖子列表渲染为易读文本（含作者、时间、正文、链接）。
func formatNotes(notes []Note, host string) string {
	if len(notes) == 0 {
		return "（没有找到符合条件的嘟文）"
	}
	var b strings.Builder
	base := strings.TrimRight(host, "/")
	for i, n := range notes {
		user := "@" + n.User.Username
		if n.User.Host != "" {
			user += "@" + n.User.Host
		}
		text := strings.TrimSpace(n.Text)
		if text == "" && n.Renote != nil {
			text = "[Renote] " + strings.TrimSpace(n.Renote.Text)
		}
		if text == "" {
			text = "[空贴/仅媒体]"
		}
		url := base + "/notes/" + n.ID
		b.WriteString(fmt.Sprintf("%d. %s · %s\n%s\n🔗 %s\n\n", i+1, user, n.CreatedAt, text, url))
	}
	return strings.TrimSpace(b.String())
}

// getUserNotesTool 返回 misskey_get_user_notes 工具定义。
func (c *MisskeyChannel) getUserNotesTool() agenttools.ToolDef {
	return agenttools.ToolDef{
		Tool: llm.Tool{
			Name: "misskey_get_user_notes",
			Description: "Fetch a Misskey user's recent notes (their posted posts). " +
				"Requires the target userId — resolve it first with misskey_search_user if you only have a username. " +
				"Use this when you need to read what a specific user posted (e.g. \"what did @user say recently\"), " +
				"as opposed to notes that already arrived via the timeline stream.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"userId": map[string]any{
						"type":        "string",
						"description": "ID of the user whose notes to fetch",
					},
					"limit": map[string]any{
						"type":        "integer",
						"description": "Maximum number of notes to return (default 10)",
					},
				},
				"required": []string{"userId"},
			},
			Execute: llm.ToolExecuteFunc(func(ctx *llm.ToolExecContext, input any) (any, error) {
				args, ok := input.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("misskey_get_user_notes: invalid input type")
				}
				userID, _ := args["userId"].(string)
				if userID == "" {
					return nil, fmt.Errorf("misskey_get_user_notes: userId is required")
				}
				limit := 10
				if l, ok := args["limit"].(float64); ok {
					limit = int(l)
				}
				notes, err := c.api.getUserNotes(ctx, userID, limit)
				if err != nil {
					return nil, fmt.Errorf("get user notes failed: %w", err)
				}
				return map[string]any{
					"notes":  formatNotes(notes, c.cfg.Host),
					"count":  len(notes),
					"userId": userID,
				}, nil
			}),
		},
		Category:      "misskey",
		PromptSection: channelToolAwarenessSection,
	}
}

// searchNotesTool 返回 misskey_search_notes 工具定义。
func (c *MisskeyChannel) searchNotesTool() agenttools.ToolDef {
	return agenttools.ToolDef{
		Tool: llm.Tool{
			Name: "misskey_search_notes",
			Description: "Search Misskey notes across the instance by keyword. " +
				"Use this to find posts about a topic, hashtag, or phrase that may not have appeared in your timeline stream yet.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{
						"type":        "string",
						"description": "Search keyword or phrase",
					},
					"limit": map[string]any{
						"type":        "integer",
						"description": "Maximum number of notes to return (default 10)",
					},
				},
				"required": []string{"query"},
			},
			Execute: llm.ToolExecuteFunc(func(ctx *llm.ToolExecContext, input any) (any, error) {
				args, ok := input.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("misskey_search_notes: invalid input type")
				}
				query, _ := args["query"].(string)
				if query == "" {
					return nil, fmt.Errorf("misskey_search_notes: query is required")
				}
				limit := 10
				if l, ok := args["limit"].(float64); ok {
					limit = int(l)
				}
				notes, err := c.api.searchNotes(ctx, query, limit)
				if err != nil {
					return nil, fmt.Errorf("search notes failed: %w", err)
				}
				return map[string]any{
					"notes": formatNotes(notes, c.cfg.Host),
					"count": len(notes),
					"query": query,
				}, nil
			}),
		},
		Category:      "misskey",
		PromptSection: channelToolAwarenessSection,
	}
}

// followUserTool 返回 misskey_follow_user 工具定义。
func (c *MisskeyChannel) followUserTool() agenttools.ToolDef {
	return agenttools.ToolDef{
		Tool: llm.Tool{
			Name: "misskey_follow_user",
			Description: "Follow a user on Misskey. Requires the target userId. " +
				"IMPORTANT: A username is NOT a valid userId — resolve it with misskey_search_user first.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"userId": map[string]any{
						"type":        "string",
						"description": "ID of the target user (obtain it from misskey_search_user results)",
					},
				},
				"required": []string{"userId"},
			},
			Execute: llm.ToolExecuteFunc(func(ctx *llm.ToolExecContext, input any) (any, error) {
				args, ok := input.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("misskey_follow_user: invalid input type")
				}
				userID, _ := args["userId"].(string)
				if userID == "" {
					return nil, fmt.Errorf("misskey_follow_user: userId is required")
				}
				if err := c.api.followUser(ctx, userID); err != nil {
					return nil, fmt.Errorf("follow failed: %w", err)
				}
				return map[string]any{
					"success": true,
					"message": fmt.Sprintf("已关注用户 %s", userID),
				}, nil
			}),
		},
		Category: "misskey",
	}
}

// unfollowUserTool 返回 misskey_unfollow_user 工具定义。
func (c *MisskeyChannel) unfollowUserTool() agenttools.ToolDef {
	return agenttools.ToolDef{
		Tool: llm.Tool{
			Name: "misskey_unfollow_user",
			Description: "Unfollow a user on Misskey. Requires the target userId. " +
				"IMPORTANT: A username is NOT a valid userId — resolve it with misskey_search_user first.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"userId": map[string]any{
						"type":        "string",
						"description": "ID of the user to unfollow",
					},
				},
				"required": []string{"userId"},
			},
			Execute: llm.ToolExecuteFunc(func(ctx *llm.ToolExecContext, input any) (any, error) {
				args, ok := input.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("misskey_unfollow_user: invalid input type")
				}
				userID, _ := args["userId"].(string)
				if userID == "" {
					return nil, fmt.Errorf("misskey_unfollow_user: userId is required")
				}
				if err := c.api.unfollowUser(ctx, userID); err != nil {
					return nil, fmt.Errorf("unfollow failed: %w", err)
				}
				return map[string]any{
					"success": true,
					"message": fmt.Sprintf("已取消关注用户 %s", userID),
				}, nil
			}),
		},
		Category: "misskey",
	}
}

// createNoteTool 返回 misskey_create_note 工具定义。
func (c *MisskeyChannel) createNoteTool() agenttools.ToolDef {
	return agenttools.ToolDef{
		Tool: llm.Tool{
			Name: "misskey_create_note",
			Description: "Publish a note (post) on Misskey. " +
				"Supports visibility control (public/home/followers) and a CW (content warning) title. " +
				"IMPORTANT: The note is published to end users. You must write the note text in Chinese (中文).",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"text": map[string]any{
						"type":        "string",
						"description": "Body text of the note. Write it in Chinese (中文)",
					},
					"visibility": map[string]any{
						"type":        "string",
						"description": "Note visibility: public (default), home (home timeline), or followers (followers only)",
						"enum":        []string{"public", "home", "followers"},
					},
					"cw": map[string]any{
						"type":        "string",
						"description": "CW (content warning) title that collapses the note body, e.g. \"剧透警告\"",
					},
				},
				"required": []string{"text"},
			},
			Execute: llm.ToolExecuteFunc(func(ctx *llm.ToolExecContext, input any) (any, error) {
				args, ok := input.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("misskey_create_note: invalid input type")
				}
				text, _ := args["text"].(string)
				if text == "" {
					return nil, fmt.Errorf("misskey_create_note: text is required")
				}
				visibility, _ := args["visibility"].(string)
				if visibility == "" {
					visibility = VisibilityPublic
				}
				cw, _ := args["cw"].(string)

				noteID, err := c.api.createNoteFull(ctx, text, "", "", visibility, cw, nil)
				if err != nil {
					return nil, fmt.Errorf("create note failed: %w", err)
				}

				noteURL := fmt.Sprintf("%s/notes/%s", strings.TrimRight(c.cfg.Host, "/"), noteID)
				return map[string]any{
					"success":    true,
					"noteId":     noteID,
					"noteUrl":    noteURL,
					"visibility": visibility,
					"message":    fmt.Sprintf("帖子已发布: %s", noteURL),
				}, nil
			}),
		},
		Category: "misskey",
	}
}

// createRenoteTool 返回 misskey_create_renote 工具定义。
func (c *MisskeyChannel) createRenoteTool() agenttools.ToolDef {
	return agenttools.ToolDef{
		Tool: llm.Tool{
			Name: "misskey_create_renote",
			Description: "Renote (boost) an existing note on Misskey. " +
				"Requires the noteId of the original note.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"noteId": map[string]any{
						"type":        "string",
						"description": "ID of the original note to renote",
					},
					"visibility": map[string]any{
						"type":        "string",
						"description": "Renote visibility: public (default), home (home timeline), or followers (followers only)",
						"enum":        []string{"public", "home", "followers"},
					},
				},
				"required": []string{"noteId"},
			},
			Execute: llm.ToolExecuteFunc(func(ctx *llm.ToolExecContext, input any) (any, error) {
				args, ok := input.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("misskey_create_renote: invalid input type")
				}
				noteID, _ := args["noteId"].(string)
				if noteID == "" {
					return nil, fmt.Errorf("misskey_create_renote: noteId is required")
				}
				visibility, _ := args["visibility"].(string)
				if visibility == "" {
					visibility = VisibilityPublic
				}

				newNoteID, err := c.api.createNoteFull(ctx, "", "", noteID, visibility, "", nil)
				if err != nil {
					return nil, fmt.Errorf("renote failed: %w", err)
				}

				noteURL := fmt.Sprintf("%s/notes/%s", strings.TrimRight(c.cfg.Host, "/"), newNoteID)
				return map[string]any{
					"success":    true,
					"noteId":     newNoteID,
					"noteUrl":    noteURL,
					"visibility": visibility,
					"message":    fmt.Sprintf("已转发帖子: %s", noteURL),
				}, nil
			}),
		},
		Category: "misskey",
	}
}

// deleteNoteTool 返回 misskey_delete_note 工具定义。
func (c *MisskeyChannel) deleteNoteTool() agenttools.ToolDef {
	return agenttools.ToolDef{
		Tool: llm.Tool{
			Name: "misskey_delete_note",
			Description: "Delete a note (post) that this bot published on Misskey. Requires the noteId. " +
				"CRITICAL: You can ONLY delete notes the bot posted itself — NEVER attempt to delete another user's note.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"noteId": map[string]any{
						"type":        "string",
						"description": "ID of the note to delete",
					},
				},
				"required": []string{"noteId"},
			},
			Execute: llm.ToolExecuteFunc(func(ctx *llm.ToolExecContext, input any) (any, error) {
				args, ok := input.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("misskey_delete_note: invalid input type")
				}
				noteID, _ := args["noteId"].(string)
				if noteID == "" {
					return nil, fmt.Errorf("misskey_delete_note: noteId is required")
				}
				if err := c.api.deleteNote(ctx, noteID); err != nil {
					return nil, fmt.Errorf("delete note failed: %w", err)
				}
				return map[string]any{
					"success": true,
					"message": fmt.Sprintf("帖子 %s 已删除", noteID),
				}, nil
			}),
		},
		Category: "misskey",
	}
}

// reactToNoteTool 返回 misskey_react_to_note 工具定义。
func (c *MisskeyChannel) reactToNoteTool() agenttools.ToolDef {
	return agenttools.ToolDef{
		Tool: llm.Tool{
			Name: "misskey_react_to_note",
			Description: "Add an emoji reaction to a note on Misskey. " +
				"Requires the noteId and a reaction (for example :heart:).",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"noteId": map[string]any{
						"type":        "string",
						"description": "ID of the target note",
					},
					"reaction": map[string]any{
						"type":        "string",
						"description": "Reaction content, either a Unicode emoji or the :name: form, e.g. \"👍\" or \":heart:\"",
					},
				},
				"required": []string{"noteId", "reaction"},
			},
			Execute: llm.ToolExecuteFunc(func(ctx *llm.ToolExecContext, input any) (any, error) {
				args, ok := input.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("misskey_react_to_note: invalid input type")
				}
				noteID, _ := args["noteId"].(string)
				reaction, _ := args["reaction"].(string)
				if noteID == "" || reaction == "" {
					return nil, fmt.Errorf("misskey_react_to_note: noteId and reaction are required")
				}
				if err := c.api.createReaction(ctx, noteID, reaction); err != nil {
					return nil, fmt.Errorf("react failed: %w", err)
				}
				return map[string]any{
					"success":  true,
					"noteId":   noteID,
					"reaction": reaction,
					"message":  fmt.Sprintf("已对帖子 %s 添加反应 %s", noteID, reaction),
				}, nil
			}),
		},
		Category: "misskey",
	}
}

// unreactToNoteTool 返回 misskey_unreact_to_note 工具定义。
func (c *MisskeyChannel) unreactToNoteTool() agenttools.ToolDef {
	return agenttools.ToolDef{
		Tool: llm.Tool{
			Name: "misskey_unreact_to_note",
			Description: "Remove the bot's own emoji reaction from a note on Misskey. " +
				"Requires only the noteId — the bot's existing reaction on that note is removed automatically.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"noteId": map[string]any{
						"type":        "string",
						"description": "ID of the target note",
					},
				},
				"required": []string{"noteId"},
			},
			Execute: llm.ToolExecuteFunc(func(ctx *llm.ToolExecContext, input any) (any, error) {
				args, ok := input.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("misskey_unreact_to_note: invalid input type")
				}
				noteID, _ := args["noteId"].(string)
				if noteID == "" {
					return nil, fmt.Errorf("misskey_unreact_to_note: noteId is required")
				}
				if err := c.api.deleteReaction(ctx, noteID); err != nil {
					return nil, fmt.Errorf("unreact failed: %w", err)
				}
				return map[string]any{
					"success": true,
					"noteId":  noteID,
					"message": fmt.Sprintf("已移除对帖子 %s 的反应", noteID),
				}, nil
			}),
		},
		Category: "misskey",
	}
}

// searchUserTool 返回 misskey_search_user 工具定义。
func (c *MisskeyChannel) searchUserTool() agenttools.ToolDef {
	return agenttools.ToolDef{
		Tool: llm.Tool{
			Name: "misskey_search_user",
			Description: "Search for users on Misskey. " +
				"Returns matching users with userId, username, displayName, and related fields. " +
				"ALWAYS use this to resolve a userId before calling misskey_follow_user or misskey_unfollow_user.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{
						"type":        "string",
						"description": "Search keyword (username or display name)",
					},
					"limit": map[string]any{
						"type":        "integer",
						"description": "Maximum number of results to return (default 10)",
					},
				},
				"required": []string{"query"},
			},
			Execute: llm.ToolExecuteFunc(func(ctx *llm.ToolExecContext, input any) (any, error) {
				args, ok := input.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("misskey_search_user: invalid input type")
				}
				query, _ := args["query"].(string)
				if query == "" {
					return nil, fmt.Errorf("misskey_search_user: query is required")
				}
				limit := 10
				if l, ok := args["limit"].(float64); ok {
					limit = int(l)
				}

				users, err := c.api.searchUser(ctx, query, limit)
				if err != nil {
					return nil, fmt.Errorf("search user failed: %w", err)
				}

				var results []map[string]any
				for _, u := range users {
					displayName := u.Name
					if displayName == "" {
						displayName = u.Username
					}
					results = append(results, map[string]any{
						"userId":      u.ID,
						"username":    u.Username,
						"displayName": displayName,
						"host":        u.Host,
						"description": u.Description,
					})
				}
				return map[string]any{
					"users": results,
					"count": len(results),
					"query": query,
				}, nil
			}),
		},
		Category: "misskey",
	}
}

// listFollowingTool 返回 misskey_list_following 工具定义。
func (c *MisskeyChannel) listFollowingTool() agenttools.ToolDef {
	return agenttools.ToolDef{
		Tool: llm.Tool{
			Name: "misskey_list_following",
			Description: "List the accounts a user follows on Misskey. " +
				"When userId is omitted, returns the bot's own following list.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"userId": map[string]any{
						"type":        "string",
						"description": "ID of the user whose following list to fetch. Omit to list the bot's own following",
					},
					"limit": map[string]any{
						"type":        "integer",
						"description": "Maximum number of results to return (default 10)",
					},
				},
			},
			Execute: llm.ToolExecuteFunc(func(ctx *llm.ToolExecContext, input any) (any, error) {
				args, ok := input.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("misskey_list_following: invalid input type")
				}
				userID, _ := args["userId"].(string)
				limit := 10
				if l, ok := args["limit"].(float64); ok {
					limit = int(l)
				}

				following, err := c.api.listFollowing(ctx, userID, limit)
				if err != nil {
					return nil, fmt.Errorf("list following failed: %w", err)
				}

				var results []map[string]any
				for _, f := range following {
					displayName := f.Followee.Name
					if displayName == "" {
						displayName = f.Followee.Username
					}
					results = append(results, map[string]any{
						"userId":      f.Followee.ID,
						"username":    f.Followee.Username,
						"displayName": displayName,
						"host":        f.Followee.Host,
					})
				}
				return map[string]any{
					"following": results,
					"count":     len(results),
				}, nil
			}),
		},
		Category: "misskey",
	}
}
