package mastodon

import (
	"context"
	"fmt"
	"strings"

	agenttools "github.com/kasuganosora/thinkbot/agent/tools"
	"github.com/kasuganosora/thinkbot/llm"
)

// ============================================================================
// ChannelToolProvider 实现 — Mastodon 平台专属工具
// ============================================================================

// channelToolAwarenessSection 纠正 bot 的自我认知：它在 Mastodon 上既能接收也能发言。
// 内容与 Misskey 版本对等，差异处按 Mastodon 事实改写：
//   - 表态只有「收藏」（favourite），没有自定义 emoji 反应；
//   - 提及必须写全 @user@domain（acct），联邦用户才收得到；
//   - 平台搜索（/api/v2/search）依赖实例是否开启全文索引，搜不到是常态。
var channelToolAwarenessSection = &agenttools.ToolPromptSection{
	Name:    "mastodon_observability",
	Order:   300,
	Enabled: true,
	Content: `# Mastodon Capabilities

You are NOT "write-only" and NOT "tool-less" on Mastodon. You both RECEIVE and CAN POST.

## Receiving (no tool needed)
- Direct @mentions or replies to you arrive as notifications, and you can respond directly.
- The bot subscribes to the configured public timelines; posts on those timelines arrive as [Timeline] messages, which you may observe, learn from, or react to.

## Favourites on your posts
When someone favourites your post you may receive a [Mastodon 收藏] InjectContext. That is awareness only — do not reply, boost, favourite back, or call tools for it. Just know they liked it.

## Replying (just use text, do NOT call a tool)
When someone @mentions or replies to you, simply reply with your normal text — the system automatically sends your reply as a threaded reply (with an @mention prefix added automatically).
- You do NOT need, and should NOT, call mastodon_post_status to reply: that would post an isolated new status instead of a reply to the original, and may be duplicated by the system's automatic reply.
- Therefore never say "I have no tool to reply" — you can always reply with text, and the framework sends it for you.
- Your final reply text IS the post body that will be sent to the other party. Write the reply content directly (e.g. "Got it! Here's my reply~"), and do NOT write operational reports like "I have replied / I favourited / I published a post" — those actions are handled by the system, not something you say to the user.

## Proactive reading (call tools on demand)
- To see what someone recently posted: use mastodon_get_account_statuses (resolve the account ID first with mastodon_search_accounts).
- To search posts by keyword: use mastodon_search_statuses, but remember most instances do NOT run full-text search — an empty result is normal and NOT a failure. Do not retry repeatedly; reply with what you know instead.
- NEVER call mastodon_unfavourite_status on a post you just favourited in the same turn — that undoes your own favourite. Only unfavourite to deliberately take back a prior one.

## Proactively publishing a new post (only when NOT replying)
Only call mastodon_post_status when you want to start a brand-new status that is not a reply to anything. For replies, use the "reply with text" above.
- If the post needs to @ someone: you MUST write the full @user@domain form (e.g. @alice@example.social). The @acct that appears in the conversation context is already the correct form — copy it as-is. Writing only @user will silently fail to reach federated users.

## Current status ID (for tool calls only)
The message you receive may end with a line '[status_id: xxxxx]' — that is the ID of the CURRENT post. To favourite (mastodon_favourite_status), boost, or quote it, use it directly, and do NOT call mastodon_search_statuses to look it up — search on this instance is unreliable.

## Reply content format (Mastodon-specific — read carefully)
Mastodon renders HTML, but the framework strips any tag it does not recognize. Getting this wrong makes your reply show literal garbage like "<p><ul><li>". Follow these rules:
- NEVER use HTML tags for formatting: no <p>, <ul>, <li>, <b>, <div>, <br>, <span>. They will appear verbatim in the post. Use Markdown instead: **bold**, *italic*, lists with "- item", > quote, inline code.
- NEVER invent wrapper tags like <long>, <summary>, <note>, <details>. The ONLY wrapper tags you may use are the framework's <public> (your public reply) and <internal> (private thoughts) — both defined by the reply-control protocol. Anything else is stripped and may mangle your text.
- Write your reply as clean, human-readable text exactly as the reader should see it. The framework sends your <public> block verbatim (tags removed), so what you type IS what gets posted.
- Keep it within the instance's character limit (usually 500). Longer text is truncated automatically, so lead with what matters.

## Internal information you must NEVER tell the user (hard rules)
- Whether a tool call succeeds or fails, NEVER reveal any internal process to the user (or timeline): do NOT write things like "let me search", "let me interact", "statusId was not passed correctly", "search returned nothing", "HTTP 500", "tool call failed".
- When a tool fails: quietly respond in another natural way, or simply don't mention it — just like a real person wouldn't read backend errors aloud to the other party. Your final reply should only contain plain human language.
- When you receive a technical marker like '[status_id: ...]', it is for you to call tools with — NEVER write it verbatim into your reply body.
- The message context may also contain reply/boost context markers like '[Reply to @user: original text]' or '[Boost from @user: original text]'. These are ONLY context telling you what the other person is replying to or boosting — they are NOT part of the user's actual message. NEVER copy them into your reply body; just respond naturally to the actual content.
- For casual posts that directly @ you or reply to you (e.g. "checking in", "anyone there", "haha"), just reply with a relaxed line of text — don't search, post, or pile on favourites just to seem active.`,
}

// ChannelTools 返回 MastodonChannel 提供的平台专属工具定义。
// 工具通过闭包捕获 Channel 的 API 客户端，支持跨 Channel 调用。
func (c *MastodonChannel) ChannelTools(ctx context.Context) ([]agenttools.ToolDef, error) {
	return []agenttools.ToolDef{
		c.followAccountTool(),
		c.unfollowAccountTool(),
		c.postStatusTool(),
		c.boostStatusTool(),
		c.deleteStatusTool(),
		c.favouriteStatusTool(),
		c.unfavouriteStatusTool(),
		c.searchAccountsTool(),
		c.listFollowingTool(),
		c.getAccountStatusesTool(),
		c.searchStatusesTool(),
	}, nil
}

// statusURL 拼出**本 bot 自己**发的帖子在本实例上的可点击链接。
//
// ⚠️ Mastodon 的帖子路由是 `/@<username>/<id>`，既没有 Misskey 那种
// `/notes/<id>` 短链，也**不存在** `/@_/<id>`。写成 `/@_/<id>` 得到的
// 是一个 404 死链，而 LLM 会把它当成真链接直接发给用户且毫不自知。
func (c *MastodonChannel) statusURL(id string) string {
	user := c.selfUsername()
	if user == "" {
		user = "_"
	}
	return strings.TrimRight(c.cfg.Server, "/") + "/@" + user + "/" + id
}

// statusLink 给出一条帖子的可点击链接。
//
// 优先用服务端返回的 `url` 字段：它是权威值，且对**远程实例**的帖子会指向
// 原站（本地拼接反而错）。拿不到时退回按作者 username 拼。
func (c *MastodonChannel) statusLink(s *Status) string {
	if s == nil {
		return ""
	}
	if s.URL != "" {
		return s.URL
	}
	if s.Account.Username != "" {
		return strings.TrimRight(c.cfg.Server, "/") + "/@" + s.Account.Username + "/" + s.ID
	}
	return c.statusURL(s.ID)
}

// formatStatuses 把帖子列表渲染为易读文本（含作者、时间、正文、链接）。
func (c *MastodonChannel) formatStatuses(statuses []Status) string {
	if len(statuses) == 0 {
		return "No matching posts found."
	}
	var b strings.Builder
	for i, s := range statuses {
		handle := s.Account.handle()
		text := htmlToText(s.Content)
		switch {
		case text == "" && s.Reblog != nil:
			text = "[Boost] " + htmlToText(s.Reblog.Content)
		case text == "":
			text = "[empty post / media only]"
		}
		if s.SpoilerText != "" {
			text = "[CW: " + s.SpoilerText + "] " + text
		}
		fmt.Fprintf(&b, "%d. %s · %s\n%s\n🔗 %s\n\n", i+1, handle, s.CreatedAt, text, c.statusLink(&s))
	}
	return strings.TrimSpace(b.String())
}

// formatAccounts 把账号列表渲染为易读文本（含 acct、ID、简介计数）。
func formatAccounts(accounts []Account) string {
	if len(accounts) == 0 {
		return "No matching accounts found."
	}
	var b strings.Builder
	for i, a := range accounts {
		name := a.DisplayName
		if name == "" {
			name = a.Username
		}
		fmt.Fprintf(&b, "%d. %s (%s) · id=%s · followers=%d · posts=%d\n",
			i+1, name, a.handle(), a.ID, a.FollowersCount, a.StatusesCount)
	}
	return strings.TrimSpace(b.String())
}

// searchAccountsTool 返回 mastodon_search_accounts 工具定义。
func (c *MastodonChannel) searchAccountsTool() agenttools.ToolDef {
	return agenttools.ToolDef{
		Tool: llm.Tool{
			Name: "mastodon_search_accounts",
			Description: "Search Mastodon accounts by username or display name. " +
				"Returns each account's id (needed by other tools), acct (the @user@domain form used for mentions), " +
				"follower/post counts. Set resolve=true when you have a full @user@domain handle from another instance.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{
						"type":        "string",
						"description": "Search keyword (username or display name)",
					},
					"limit": map[string]any{
						"type":        "integer",
						"description": "Maximum number of accounts to return (default 10)",
					},
					"resolve": map[string]any{
						"type":        "boolean",
						"description": "Look up federated accounts by full @user@domain handle (default false)",
					},
				},
				"required": []string{"query"},
			},
			Execute: llm.ToolExecuteFunc(func(ctx *llm.ToolExecContext, input any) (any, error) {
				args, ok := input.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("mastodon_search_accounts: invalid input type")
				}
				query, _ := args["query"].(string)
				if query == "" {
					return nil, fmt.Errorf("mastodon_search_accounts: query is required")
				}
				limit := 10
				if l, ok := args["limit"].(float64); ok {
					limit = int(l)
				}
				resolve, _ := args["resolve"].(bool)
				accounts, err := c.api.searchAccounts(ctx, query, limit, resolve)
				if err != nil {
					return nil, fmt.Errorf("search accounts failed: %w", err)
				}
				return map[string]any{
					"accounts": formatAccounts(accounts),
					"count":    len(accounts),
				}, nil
			}),
		},
		Category:      "mastodon",
		PromptSection: channelToolAwarenessSection,
	}
}

// getAccountStatusesTool 返回 mastodon_get_account_statuses 工具定义。
func (c *MastodonChannel) getAccountStatusesTool() agenttools.ToolDef {
	return agenttools.ToolDef{
		Tool: llm.Tool{
			Name: "mastodon_get_account_statuses",
			Description: "Fetch a Mastodon account's recent posts. " +
				"Requires the target account id — resolve it first with mastodon_search_accounts if you only have a username. " +
				"Use this to read what a specific person posted recently.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"accountId": map[string]any{
						"type":        "string",
						"description": "ID of the account whose posts to fetch",
					},
					"limit": map[string]any{
						"type":        "integer",
						"description": "Maximum number of posts to return (default 10)",
					},
					"excludeReplies": map[string]any{
						"type":        "boolean",
						"description": "Exclude the account's replies (default false)",
					},
				},
				"required": []string{"accountId"},
			},
			Execute: llm.ToolExecuteFunc(func(ctx *llm.ToolExecContext, input any) (any, error) {
				args, ok := input.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("mastodon_get_account_statuses: invalid input type")
				}
				accountID, _ := args["accountId"].(string)
				if accountID == "" {
					return nil, fmt.Errorf("mastodon_get_account_statuses: accountId is required")
				}
				limit := 10
				if l, ok := args["limit"].(float64); ok {
					limit = int(l)
				}
				excludeReplies, _ := args["excludeReplies"].(bool)
				statuses, err := c.api.getAccountStatuses(ctx, accountID, limit, excludeReplies)
				if err != nil {
					return nil, fmt.Errorf("get account statuses failed: %w", err)
				}
				return map[string]any{
					"statuses":  c.formatStatuses(statuses),
					"count":     len(statuses),
					"accountId": accountID,
				}, nil
			}),
		},
		Category:      "mastodon",
		PromptSection: channelToolAwarenessSection,
	}
}

// searchStatusesTool 返回 mastodon_search_statuses 工具定义。
func (c *MastodonChannel) searchStatusesTool() agenttools.ToolDef {
	return agenttools.ToolDef{
		Tool: llm.Tool{
			Name: "mastodon_search_statuses",
			Description: "Search Mastodon posts by keyword. " +
				"IMPORTANT: most instances do not run full-text search, so results are limited (often only your own posts) " +
				"and an empty result is normal, not an error. Do not retry repeatedly — reply with what you know instead.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{
						"type":        "string",
						"description": "Search keyword or phrase",
					},
					"limit": map[string]any{
						"type":        "integer",
						"description": "Maximum number of posts to return (default 10)",
					},
				},
				"required": []string{"query"},
			},
			Execute: llm.ToolExecuteFunc(func(ctx *llm.ToolExecContext, input any) (any, error) {
				args, ok := input.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("mastodon_search_statuses: invalid input type")
				}
				query, _ := args["query"].(string)
				if query == "" {
					return nil, fmt.Errorf("mastodon_search_statuses: query is required")
				}
				limit := 10
				if l, ok := args["limit"].(float64); ok {
					limit = int(l)
				}
				statuses, err := c.api.searchStatuses(ctx, query, limit)
				if err != nil {
					return nil, fmt.Errorf("search statuses failed: %w", err)
				}
				return map[string]any{
					"statuses": c.formatStatuses(statuses),
					"count":    len(statuses),
				}, nil
			}),
		},
		Category:      "mastodon",
		PromptSection: channelToolAwarenessSection,
	}
}

// postStatusTool 返回 mastodon_post_status 工具定义（主动发新帖，不是回复）。
func (c *MastodonChannel) postStatusTool() agenttools.ToolDef {
	return agenttools.ToolDef{
		Tool: llm.Tool{
			Name: "mastodon_post_status",
			Description: "Publish a brand-new public post (toot) on Mastodon. " +
				"Use this ONLY when you want to start a new post that is not a reply — to reply to someone, just " +
				"answer with your normal text and the framework sends it as a threaded reply.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"text": map[string]any{
						"type":        "string",
						"description": "Post body (keep within the instance limit, usually 500 characters)",
					},
					"visibility": map[string]any{
						"type":        "string",
						"description": "public / unlisted / private / direct (default unlisted)",
					},
					"spoilerText": map[string]any{
						"type":        "string",
						"description": "Content warning text shown before the body (optional)",
					},
				},
				"required": []string{"text"},
			},
			Execute: llm.ToolExecuteFunc(func(ctx *llm.ToolExecContext, input any) (any, error) {
				args, ok := input.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("mastodon_post_status: invalid input type")
				}
				text, _ := args["text"].(string)
				if strings.TrimSpace(text) == "" {
					return nil, fmt.Errorf("mastodon_post_status: text is required")
				}
				visibility, _ := args["visibility"].(string)
				spoiler, _ := args["spoilerText"].(string)
				id, err := c.PostTimeline(ctx, text, visibility, spoiler)
				if err != nil {
					return nil, fmt.Errorf("post status failed: %w", err)
				}
				return map[string]any{
					"success":  true,
					"statusId": id,
					"url":      c.statusURL(id),
				}, nil
			}),
		},
		Category:      "mastodon",
		PromptSection: channelToolAwarenessSection,
	}
}

// boostStatusTool 返回 mastodon_boost_status 工具定义（转发）。
func (c *MastodonChannel) boostStatusTool() agenttools.ToolDef {
	return agenttools.ToolDef{
		Tool: llm.Tool{
			Name: "mastodon_boost_status",
			Description: "Boost (reblog) a post to your own timeline. " +
				"Equivalent to Misskey's renote. Boosting twice is harmless — the server treats it as idempotent.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"statusId": map[string]any{
						"type":        "string",
						"description": "ID of the post to boost",
					},
				},
				"required": []string{"statusId"},
			},
			Execute: llm.ToolExecuteFunc(func(ctx *llm.ToolExecContext, input any) (any, error) {
				args, ok := input.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("mastodon_boost_status: invalid input type")
				}
				id, _ := args["statusId"].(string)
				if id == "" {
					return nil, fmt.Errorf("mastodon_boost_status: statusId is required")
				}
				if err := c.Boost(ctx, id); err != nil {
					return nil, fmt.Errorf("boost status failed: %w", err)
				}
				return map[string]any{"success": true, "statusId": id}, nil
			}),
		},
		Category:      "mastodon",
		PromptSection: channelToolAwarenessSection,
	}
}

// deleteStatusTool 返回 mastodon_delete_status 工具定义。
func (c *MastodonChannel) deleteStatusTool() agenttools.ToolDef {
	return agenttools.ToolDef{
		Tool: llm.Tool{
			Name: "mastodon_delete_status",
			Description: "Delete one of your own posts. " +
				"Only works on posts published by this bot; deleting someone else's post is impossible.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"statusId": map[string]any{
						"type":        "string",
						"description": "ID of your post to delete",
					},
				},
				"required": []string{"statusId"},
			},
			Execute: llm.ToolExecuteFunc(func(ctx *llm.ToolExecContext, input any) (any, error) {
				args, ok := input.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("mastodon_delete_status: invalid input type")
				}
				id, _ := args["statusId"].(string)
				if id == "" {
					return nil, fmt.Errorf("mastodon_delete_status: statusId is required")
				}
				if err := c.api.deleteStatus(ctx, id); err != nil {
					return nil, fmt.Errorf("delete status failed: %w", err)
				}
				return map[string]any{"success": true, "statusId": id}, nil
			}),
		},
		Category:      "mastodon",
		PromptSection: channelToolAwarenessSection,
	}
}

// favouriteStatusTool 返回 mastodon_favourite_status 工具定义（收藏/点赞）。
func (c *MastodonChannel) favouriteStatusTool() agenttools.ToolDef {
	return agenttools.ToolDef{
		Tool: llm.Tool{
			Name: "mastodon_favourite_status",
			Description: "Favourite (like) a post. This is the only reaction Mastodon has — there are no custom emoji reactions. " +
				"Favouriting an already-favourited post succeeds silently (idempotent).",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"statusId": map[string]any{
						"type":        "string",
						"description": "ID of the post to favourite",
					},
				},
				"required": []string{"statusId"},
			},
			Execute: llm.ToolExecuteFunc(func(ctx *llm.ToolExecContext, input any) (any, error) {
				args, ok := input.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("mastodon_favourite_status: invalid input type")
				}
				id, _ := args["statusId"].(string)
				if id == "" {
					return nil, fmt.Errorf("mastodon_favourite_status: statusId is required")
				}
				if err := c.Favourite(ctx, id); err != nil {
					return nil, fmt.Errorf("favourite status failed: %w", err)
				}
				return map[string]any{"success": true, "statusId": id}, nil
			}),
		},
		Category:      "mastodon",
		PromptSection: channelToolAwarenessSection,
	}
}

// unfavouriteStatusTool 返回 mastodon_unfavourite_status 工具定义。
func (c *MastodonChannel) unfavouriteStatusTool() agenttools.ToolDef {
	return agenttools.ToolDef{
		Tool: llm.Tool{
			Name: "mastodon_unfavourite_status",
			Description: "Remove your favourite from a post. " +
				"Never call this on a post you just favourited in the same turn — that undoes your own reaction.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"statusId": map[string]any{
						"type":        "string",
						"description": "ID of the post to unfavourite",
					},
				},
				"required": []string{"statusId"},
			},
			Execute: llm.ToolExecuteFunc(func(ctx *llm.ToolExecContext, input any) (any, error) {
				args, ok := input.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("mastodon_unfavourite_status: invalid input type")
				}
				id, _ := args["statusId"].(string)
				if id == "" {
					return nil, fmt.Errorf("mastodon_unfavourite_status: statusId is required")
				}
				if err := c.Unfavourite(ctx, id); err != nil {
					return nil, fmt.Errorf("unfavourite status failed: %w", err)
				}
				return map[string]any{"success": true, "statusId": id}, nil
			}),
		},
		Category:      "mastodon",
		PromptSection: channelToolAwarenessSection,
	}
}

// followAccountTool 返回 mastodon_follow_account 工具定义。
func (c *MastodonChannel) followAccountTool() agenttools.ToolDef {
	return agenttools.ToolDef{
		Tool: llm.Tool{
			Name: "mastodon_follow_account",
			Description: "Follow a Mastodon account. " +
				"Requires the account id — resolve it with mastodon_search_accounts first. " +
				"Locked accounts will enter a pending follow-request state instead of failing.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"accountId": map[string]any{
						"type":        "string",
						"description": "ID of the account to follow",
					},
				},
				"required": []string{"accountId"},
			},
			Execute: llm.ToolExecuteFunc(func(ctx *llm.ToolExecContext, input any) (any, error) {
				args, ok := input.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("mastodon_follow_account: invalid input type")
				}
				id, _ := args["accountId"].(string)
				if id == "" {
					return nil, fmt.Errorf("mastodon_follow_account: accountId is required")
				}
				if err := c.api.followAccount(ctx, id); err != nil {
					return nil, fmt.Errorf("follow account failed: %w", err)
				}
				return map[string]any{"success": true, "accountId": id}, nil
			}),
		},
		Category:      "mastodon",
		PromptSection: channelToolAwarenessSection,
	}
}

// unfollowAccountTool 返回 mastodon_unfollow_account 工具定义。
func (c *MastodonChannel) unfollowAccountTool() agenttools.ToolDef {
	return agenttools.ToolDef{
		Tool: llm.Tool{
			Name:        "mastodon_unfollow_account",
			Description: "Unfollow a Mastodon account.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"accountId": map[string]any{
						"type":        "string",
						"description": "ID of the account to unfollow",
					},
				},
				"required": []string{"accountId"},
			},
			Execute: llm.ToolExecuteFunc(func(ctx *llm.ToolExecContext, input any) (any, error) {
				args, ok := input.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("mastodon_unfollow_account: invalid input type")
				}
				id, _ := args["accountId"].(string)
				if id == "" {
					return nil, fmt.Errorf("mastodon_unfollow_account: accountId is required")
				}
				if err := c.api.unfollowAccount(ctx, id); err != nil {
					return nil, fmt.Errorf("unfollow account failed: %w", err)
				}
				return map[string]any{"success": true, "accountId": id}, nil
			}),
		},
		Category:      "mastodon",
		PromptSection: channelToolAwarenessSection,
	}
}

// listFollowingTool 返回 mastodon_list_following 工具定义。
func (c *MastodonChannel) listFollowingTool() agenttools.ToolDef {
	return agenttools.ToolDef{
		Tool: llm.Tool{
			Name: "mastodon_list_following",
			Description: "List the accounts that a given account follows. " +
				"Leave accountId empty to list the accounts this bot follows.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"accountId": map[string]any{
						"type":        "string",
						"description": "Account id to inspect (default: this bot)",
					},
					"limit": map[string]any{
						"type":        "integer",
						"description": "Maximum number of accounts to return (default 10)",
					},
				},
			},
			Execute: llm.ToolExecuteFunc(func(ctx *llm.ToolExecContext, input any) (any, error) {
				args, _ := input.(map[string]any)
				id, _ := args["accountId"].(string)
				if id == "" {
					id = c.selfAccountID() // 并发安全读：工具可能在 Start 之后任意 goroutine 上执行
				}
				limit := 10
				if l, ok := args["limit"].(float64); ok {
					limit = int(l)
				}
				if id == "" {
					return nil, fmt.Errorf("mastodon_list_following: accountId is required (bot id not resolved yet)")
				}
				accounts, err := c.api.listFollowing(ctx, id, limit)
				if err != nil {
					return nil, fmt.Errorf("list following failed: %w", err)
				}
				return map[string]any{
					"accounts": formatAccounts(accounts),
					"count":    len(accounts),
				}, nil
			}),
		},
		Category:      "mastodon",
		PromptSection: channelToolAwarenessSection,
	}
}
