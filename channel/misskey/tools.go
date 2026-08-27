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

// channelToolAwarenessSection 纠正 bot 的自我认知：它在 Misskey 上既能接收也能发言。
// 它既会被动接收 mention/reply/timeline 消息，也拥有主动读取与发布帖子的工具。
// 同时明确「回复」的正确姿势：直接用文本回复即可，框架会自动把回复作为带 @ 前缀的串接回复发出，
// 不需要、也不应该为了回复而去调用 misskey_create_note（那会发成孤立帖子，且可能与框架回复重复）。
// 挂在新读取工具与发布工具上，随 misskey 通道一起注入系统提示。
var channelToolAwarenessSection = &agenttools.ToolPromptSection{
	Name:    "misskey_observability",
	Order:   300,
	Enabled: true,
	Content: `# Misskey Capabilities

You are NOT "write-only" and NOT "tool-less" on Misskey. You both RECEIVE and CAN POST.

## Receiving (no tool needed)
- Direct @mentions or replies to you always arrive, and you can respond directly.
- The bot subscribes to home / local / hybrid timelines; posts on those timelines arrive as [Timeline] messages, which you may observe, learn from, or react to.

## Replying (just use text, do NOT call a tool)
When someone @mentions or replies to you, simply reply with your normal text — the system automatically sends your reply as a threaded reply (with an @mention prefix added automatically).
- You do NOT need, and should NOT, call misskey_create_note to reply: that would post an isolated new note instead of a reply to the original, and may be duplicated by the system's automatic reply.
- Therefore never say "I have no tool to reply" — you can always reply with text, and the framework sends it for you.
- Your final reply text IS the post body that will be sent to the other party. Write the reply content directly (e.g. "Got it! Here's my reply~"), and do NOT write operational reports like "I have replied / I added a reaction / I published a post" — those actions are handled by the system, not something you say to the user.

## Proactive reading (call tools on demand)
- To see what someone recently posted: use misskey_get_user_notes (resolve userId first with misskey_search_user).
- To search posts by keyword: use misskey_search_notes.

## Proactively publishing a new note (only when NOT replying)
Only call misskey_create_note when you want to start a brand-new note that is not a reply to anything. For replies, use the "reply with text" above.
- If the note needs to @ someone: for users on THIS instance (maid.lat) just write @username; for remote/federated users on other instances you MUST write @username@host (e.g. @alice@example.com). The @username@host that appears in the conversation context is the correct form — copy it as-is, otherwise the other party won't receive it.

## Current note ID (for tool calls only)
The message you receive may end with a line '[note_id: xxxxx]' — that is the ID of the CURRENT note. To add a reaction (misskey_react_to_note) or quote it, use it directly, and do NOT call misskey_search_notes to look it up — this instance's search backend (Meilisearch) is often unavailable and will fail.

## Internal information you must NEVER tell the user (hard rules)
- Whether a tool call succeeds or fails, NEVER reveal any internal process to the user (or timeline): do NOT write things like "let me search", "let me interact", "noteId was not passed correctly", "search service is unavailable", "HTTP 500", "tool call failed".
- When a tool fails: quietly respond in another natural way, or simply don't mention it — just like a real person wouldn't read backend errors aloud to the other party. Your final reply should only contain plain human language.
- When you receive a technical marker like '[note_id: ...]', it is for you to call tools with — NEVER write it verbatim into your reply body.
- The message context may also contain reply/renote context markers like '[Reply to @user: original text]' or '[Renote from @user: original text]'. These are ONLY context telling you what the other person is replying to or quoting — they are NOT part of the user's actual message. NEVER copy them into your reply body; just respond naturally to the actual content.
- For casual posts that directly @ you or reply to you (e.g. "checking in", "anyone there", "haha"), just reply with a relaxed line of text — don't search, create notes, or pile on reactions just to seem active.`,
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
		return "No matching notes found."
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
			text = "[empty post / media only]"
		}
		url := base + "/notes/" + n.ID
		fmt.Fprintf(&b, "%d. %s · %s\n%s\n🔗 %s\n\n", i+1, user, n.CreatedAt, text, url)
	}
	return strings.TrimSpace(b.String())
}

// searchNotesByUserFallback 是 notes/search 不可用时的代码级兜底：
// 当查询形如 @用户（显式 @handle）时，解析该用户并直接拉取其最近帖子。
// users/notes 走实例数据库，不依赖 Meilisearch，因此搜索后端宕机时仍可用。
// 返回 (格式化文本, 帖子数, 错误)。无 @ 前缀或解析失败时返回 count=0。
func (c *MisskeyChannel) searchNotesByUserFallback(ctx context.Context, query string, limit int) (string, int, error) {
	q := strings.TrimSpace(query)
	if !strings.HasPrefix(q, "@") {
		return "", 0, fmt.Errorf("not a user handle query")
	}
	// 去掉 @ 与可能的 @host 后缀，仅保留用户名用于搜索。
	bare := strings.TrimPrefix(q, "@")
	if at := strings.Index(bare, "@"); at >= 0 {
		bare = bare[:at]
	}
	if bare == "" {
		return "", 0, fmt.Errorf("empty username")
	}
	users, err := c.api.searchUser(ctx, bare, 1)
	if err != nil || len(users) == 0 {
		return "", 0, fmt.Errorf("user resolve failed: %w", err)
	}
	notes, err := c.api.getUserNotes(ctx, users[0].ID, limit)
	if err != nil {
		return "", 0, fmt.Errorf("user notes fetch failed: %w", err)
	}
	return formatNotes(notes, c.cfg.Host), len(notes), nil
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
				// 实例搜索后端（Meilisearch）常不可用：先尝试代码级兜底——
				// 若查询形如 @用户，则解析该用户并直接拉取其最近帖子（users/notes 不依赖 Meilisearch），
				// 让「看某人最近发了什么」这类意图仍可用，而非仅靠 LLM 临场自救。
				if fb, fbCount, ferr := c.searchNotesByUserFallback(ctx, query, limit); ferr == nil && fbCount > 0 {
					return map[string]any{
						"notes":   fb,
						"count":   fbCount,
						"query":   query,
						"fallback": "user_notes", // 提示模型这是用户帖子兜底，非关键词搜索
					}, nil
				}
				// 兜底也拿不到，返回干净文案，不把裸 HTTP 错误抛给 LLM，
				// 避免模型把内部报错复述给用户。模型侧的红线见 channelToolAwarenessSection。
				return nil, fmt.Errorf("note search is temporarily unavailable (instance search backend is down); try another approach or just reply directly")
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
					"message": fmt.Sprintf("Followed user %s", userID),
				}, nil
			}),
			RequiresUserIntent: true,
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
					"message": fmt.Sprintf("Unfollowed user %s", userID),
				}, nil
			}),
			RequiresUserIntent: true,
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

				// 回复语境拦截：禁止用本工具「回复」。覆盖两种场景：
				//   1. IsDirectReply —— 对方 @ 了你或回复了你（Mentioned=true）。
				//   2. IsFrameworkReplyContext —— 本轮由本渠道入站帖驱动、框架会串接回复
				//      （含未 @ Bot 的普通 timeline 帖）。手动发孤立帖会与框架自动回复重复。
				// 直接让 Bot 用普通文本回复即可，框架会自动带 @ 前缀并以串接回复发出。
				if llm.IsDirectReply(ctx) || llm.IsFrameworkReplyContext(ctx, c.name) {
					reason := "reply_context"
					if llm.IsFrameworkReplyContext(ctx, c.name) && !llm.IsDirectReply(ctx) {
						reason = "inbound_reply_context"
					}
					return map[string]any{
						"success": false,
						"blocked": true,
						"reason":  reason,
						"message": "You are in a 'replying to an existing note' context (the other party @mentioned you, replied to you, or is threading a reply on a timeline note). " +
							"Do NOT call misskey_create_note to reply — it would post an isolated new note that is not threaded to the original, and would duplicate the system's automatic reply. " +
							"Just reply with your normal text: the system will automatically send your reply as a threaded reply with an @ prefix.",
					}, nil
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
					"message":    fmt.Sprintf("Note published: %s", noteURL),
				}, nil
			}),
			RequiresUserIntent: true,
		},
		Category:      "misskey",
		PromptSection: channelToolAwarenessSection,
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
					"message":    fmt.Sprintf("Renoted note: %s", noteURL),
				}, nil
			}),
			RequiresUserIntent: true,
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
					"message": fmt.Sprintf("Note %s deleted", noteID),
				}, nil
			}),
			RequiresUserIntent: true,
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
				"Requires the noteId and a reaction (for example :heart:). " +
				"NOTE: Misskey forbids reacting to a Renote (a pure re-post). " +
				"If the target is a Renote the tool skips and returns reason=cannot_react_to_renote; " +
				"react to the original note (renoteId) instead.",
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
				// Misskey 不允许对 Renote（转贴）加反应，服务端会返回 400 CANNOT_REACT_TO_RENOTE。
				// 加反应前先拉取帖子，若是 Renote 则直接友好跳过，避免无谓的 400 错误。
				if note, gerr := c.api.getNote(ctx, noteID); gerr == nil && note != nil && note.RenoteID != "" {
					return map[string]any{
						"success": false,
						"skipped": true,
						"reason":  "cannot_react_to_renote",
						"message": "目标帖子是 Renote（转贴），Misskey 不允许对其加反应，已跳过。如需表达态度，请对原帖（renoteId）操作。",
					}, nil
				}
				if err := c.api.createReaction(ctx, noteID, reaction); err != nil {
					return nil, fmt.Errorf("react failed: %w", err)
				}
				return map[string]any{
					"success":  true,
					"noteId":   noteID,
					"reaction": reaction,
					"message":  fmt.Sprintf("Added reaction %s to note %s", noteID, reaction),
				}, nil
			}),
			RequiresUserIntent: true,
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
					"message": fmt.Sprintf("Removed reaction from note %s", noteID),
				}, nil
			}),
			RequiresUserIntent: true,
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
