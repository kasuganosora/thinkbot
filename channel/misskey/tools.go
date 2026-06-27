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

// ChannelTools 返回 MisskeyChannel 提供的平台专属工具定义。
// 工具通过闭包捕获 Channel 的 API 客户端，支持跨 Channel 调用。
func (c *MisskeyChannel) ChannelTools(ctx context.Context) ([]agenttools.ToolDef, error) {
	return []agenttools.ToolDef{
		c.followUserTool(),
		c.unfollowUserTool(),
		c.createNoteTool(),
		c.deleteNoteTool(),
		c.searchUserTool(),
		c.listFollowingTool(),
	}, nil
}

// followUserTool 返回 misskey_follow_user 工具定义。
func (c *MisskeyChannel) followUserTool() agenttools.ToolDef {
	return agenttools.ToolDef{
		Tool: llm.Tool{
			Name: "misskey_follow_user",
			Description: "在 Misskey 平台上关注一个用户。" +
				"需要提供目标用户的 userId（可从 misskey_search_user 获取）。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"userId": map[string]any{
						"type":        "string",
						"description": "目标用户的 ID（可从 misskey_search_user 结果中获取）",
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
			Description: "在 Misskey 平台上取消关注一个用户。" +
				"需要提供目标用户的 userId。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"userId": map[string]any{
						"type":        "string",
						"description": "要取消关注的用户 ID",
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
			Description: "在 Misskey 平台上发布一条帖子（Note）。" +
				"支持设置可见性（public/home/followers）和 CW（内容折叠）。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"text": map[string]any{
						"type":        "string",
						"description": "帖子文本内容",
					},
					"visibility": map[string]any{
						"type":        "string",
						"description": "帖子可见性：public（公开，默认）、home（首页）、followers（仅关注者）",
						"enum":        []string{"public", "home", "followers"},
					},
					"cw": map[string]any{
						"type":        "string",
						"description": "CW（内容折叠）标题，如 \"剧透警告\"",
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

// deleteNoteTool 返回 misskey_delete_note 工具定义。
func (c *MisskeyChannel) deleteNoteTool() agenttools.ToolDef {
	return agenttools.ToolDef{
		Tool: llm.Tool{
			Name: "misskey_delete_note",
			Description: "删除自己在 Misskey 平台上发送的帖子（Note）。" +
				"只能删除自己发送的帖子，需要提供帖子的 noteId。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"noteId": map[string]any{
						"type":        "string",
						"description": "要删除的帖子 ID",
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

// searchUserTool 返回 misskey_search_user 工具定义。
func (c *MisskeyChannel) searchUserTool() agenttools.ToolDef {
	return agenttools.ToolDef{
		Tool: llm.Tool{
			Name: "misskey_search_user",
			Description: "在 Misskey 平台上搜索用户。" +
				"返回匹配的用户列表（含 userId、username、displayName 等信息）。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{
						"type":        "string",
						"description": "搜索关键词（用户名或显示名）",
					},
					"limit": map[string]any{
						"type":        "integer",
						"description": "返回结果数量上限（默认 10）",
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
			Description: "获取指定用户的关注列表。" +
				"如果不指定 userId，默认获取当前 Bot 的关注列表。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"userId": map[string]any{
						"type":        "string",
						"description": "要查看关注列表的用户 ID。不指定则查看 Bot 自身的关注列表。",
					},
					"limit": map[string]any{
						"type":        "integer",
						"description": "返回结果数量上限（默认 10）",
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
