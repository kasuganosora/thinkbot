package outreach

import (
	"fmt"
	"strings"
	"time"

	agenttools "github.com/kasuganosora/thinkbot/agent/tools"
	"github.com/kasuganosora/thinkbot/cron"
	"github.com/kasuganosora/thinkbot/dao"
	"github.com/kasuganosora/thinkbot/llm"
)

var remindPromptSection = &agenttools.ToolPromptSection{
	Name:  "remind_tools",
	Order: 325,
	Content: `# Reminders and watches

Use the ` + "`remind`" + ` tool when the user asks you to remind them later, wake them at a time, or watch something and report back. Do **not** only write this into memory — short-lived TODOs belong here.

## Actions

- **create** — required: ` + "`when`" + ` + ` + "`text`" + `. Optional ` + "`kind`" + `: ` + "`reminder`" + ` (default, hard) or ` + "`watch`" + ` (soft, quota-gated).
- **list** — pending items for this user.
- **cancel** — required: ` + "`id`" + `.

## when formats

| Format | Example | Meaning |
|---|---|---|
| ISO timestamp | ` + "`2026-03-20T14:00`" + ` | that moment in the bot timezone |
| Relative delay | ` + "`2h`" + ` / ` + "`1d`" + ` | once, after the delay |
| Tomorrow | ` + "`tomorrow 09:00`" + ` | next calendar day (default 09:00) |

The system polls on a timer. It will only speak after the due time, and only if the per-platform quota allows it (hard reminders bypass the quiet window).

When talking to the user (confirming a reminder, or later delivering it), reply in the user's language.

<example>
user: remind me tomorrow to practice lighting questions
assistant: [calls remind(action='create', when='tomorrow 09:00', text='practice lighting questions')]
assistant: Got it — I'll ping you tomorrow morning at 9 to practice lighting questions.
</example>
`,
	Enabled: true,
}

// ToolConfig 注册 remind 工具所需依赖。
type ToolConfig struct {
	Repo     *Repo
	BotID    string
	Location *time.Location
	Now      func() time.Time
}

// RegisterTools 注册单一压缩工具 `remind`。
func RegisterTools(toolMgr *agenttools.ToolManager, cfg ToolConfig) error {
	if toolMgr == nil || cfg.Repo == nil {
		return nil
	}
	return toolMgr.Register(remindToolDef(cfg))
}

func remindToolDef(cfg ToolConfig) agenttools.ToolDef {
	loc := cfg.Location
	if loc == nil {
		loc = time.Local
	}
	return agenttools.ToolDef{
		Category: "outreach",
		Scopes:   []string{"private"},
		Tool: llm.Tool{
			Name: "remind",
			Description: "Create, list or cancel a reminder / watch so the bot can message this user later. " +
				"Use when the user asks to be reminded, woken at a time, or to watch something and report back. " +
				"action=create needs when+text; action=list; action=cancel needs id.",
			Keywords: []string{"提醒", "闹钟", "叫我", "盯着", "到点", "remind", "watch", "reminder"},
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"action": map[string]any{
						"type":        "string",
						"enum":        []string{"create", "list", "cancel"},
						"description": "create / list / cancel",
					},
					"when": map[string]any{
						"type":        "string",
						"description": "Required for create. ISO timestamp, relative delay (2h/1d), or tomorrow 09:00 / 明天 9:00.",
					},
					"text": map[string]any{
						"type":        "string",
						"description": "Required for create. What to remind / watch, in the user's words.",
					},
					"kind": map[string]any{
						"type":        "string",
						"enum":        []string{"reminder", "watch"},
						"description": "reminder (hard, default) or watch (soft, quota-gated).",
					},
					"id": map[string]any{
						"type":        "string",
						"description": "Required for cancel: commitment id from list.",
					},
				},
				"required": []string{"action"},
			},
			Execute: llm.ToolExecuteFunc(func(ctx *llm.ToolExecContext, input any) (any, error) {
				m, ok := input.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("invalid input: expected object")
				}
				action := strings.ToLower(strings.TrimSpace(getString(m, "action")))
				if action == "" {
					return toolErr("action is required"), nil
				}
				if cron.IsCronSession(ctx) || IsOutreachSession(ctx) {
					if action == "create" || action == "cancel" {
						return toolErr("anti-nesting: a cron/outreach session cannot create or cancel reminders"), nil
					}
				}
				switch action {
				case "create":
					return remindCreate(cfg, loc, ctx, m)
				case "list":
					return remindList(cfg, ctx)
				case "cancel":
					return remindCancel(cfg, ctx, m)
				default:
					return toolErr("unknown action; valid: create, list, cancel"), nil
				}
			}),
		},
		PromptSection: remindPromptSection,
	}
}

func remindCreate(cfg ToolConfig, loc *time.Location, ctx *llm.ToolExecContext, m map[string]any) (any, error) {
	whenRaw := strings.TrimSpace(getString(m, "when"))
	text := strings.TrimSpace(getString(m, "text"))
	kind := strings.ToLower(strings.TrimSpace(getString(m, "kind")))
	if kind == "" {
		kind = dao.OutreachKindReminder
	}
	if kind != dao.OutreachKindReminder && kind != dao.OutreachKindWatch {
		return toolErr("kind must be reminder or watch"), nil
	}
	if whenRaw == "" {
		return toolErr("when is required for action=create"), nil
	}
	if text == "" {
		return toolErr("text is required for action=create"), nil
	}
	due, err := ParseWhen(whenRaw, loc, nowOr(cfg.Now))
	if err != nil {
		return toolErr(err.Error()), nil
	}

	meta := agenttools.MessageMetaFromContext(ctx)
	origin := agenttools.CallOriginFromContext(ctx)
	botID := cfg.BotID
	if botID == "" {
		botID = meta.BotID
	}
	if botID == "" {
		botID = origin.BotID
	}
	userID := meta.UserID
	channelType := strings.ToLower(strings.TrimSpace(meta.ChannelType))
	if channelType == "" {
		channelType = "web"
	}
	channel := strings.TrimSpace(meta.Source)
	if channel == "" {
		channel = strings.TrimSpace(meta.ChatID)
	}
	sessionID := origin.SessionID
	convID := strings.TrimSpace(meta.ReplyTarget)
	if convID == "" && channelType != "web" {
		convID = strings.TrimSpace(meta.ChatID)
	}
	if userID == "" {
		return toolErr("cannot create reminder: missing user identity in this session"), nil
	}
	if channel == "" {
		return toolErr("cannot create reminder: missing channel in this session"), nil
	}

	ident := cfg.Repo.ResolveIdentityKey(ctx, channelType, userID)
	c := &dao.OutreachCommitment{
		BotID:          botID,
		Kind:           kind,
		IdentityKey:    ident,
		UserID:         userID,
		Channel:        channel,
		ChannelType:    channelType,
		ConversationID: convID,
		SessionID:      sessionID,
		DueAt:          due,
		Topic:          text,
		Context:        text,
		Status:         dao.OutreachPending,
		CreatedAt:      nowOr(cfg.Now).UTC(),
	}
	if err := cfg.Repo.CreateCommitment(ctx, c); err != nil {
		return toolErr(err.Error()), nil
	}
	return map[string]any{
		"success": true,
		"id":      c.ID,
		"kind":    c.Kind,
		"due_at":  c.DueAt.Format(time.RFC3339),
		"topic":   c.Topic,
		"message": fmt.Sprintf("已记下%s：%s，到期 %s", kindLabel(kind), text, c.DueAt.In(loc).Format("2006-01-02 15:04")),
	}, nil
}

func remindList(cfg ToolConfig, ctx *llm.ToolExecContext) (any, error) {
	meta := agenttools.MessageMetaFromContext(ctx)
	origin := agenttools.CallOriginFromContext(ctx)
	botID := cfg.BotID
	if botID == "" {
		botID = meta.BotID
	}
	if botID == "" {
		botID = origin.BotID
	}
	ident := cfg.Repo.ResolveIdentityKey(ctx, meta.ChannelType, meta.UserID)
	rows, err := cfg.Repo.ListPending(ctx, botID, "", ident)
	if err != nil {
		return toolErr(err.Error()), nil
	}
	items := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		items = append(items, map[string]any{
			"id":           r.ID,
			"kind":         r.Kind,
			"topic":        r.Topic,
			"due_at":       r.DueAt.Format(time.RFC3339),
			"channel_type": r.ChannelType,
		})
	}
	return map[string]any{"success": true, "items": items, "total": len(items)}, nil
}

func remindCancel(cfg ToolConfig, ctx *llm.ToolExecContext, m map[string]any) (any, error) {
	id := strings.TrimSpace(getString(m, "id"))
	if id == "" {
		return toolErr("id is required for action=cancel"), nil
	}
	meta := agenttools.MessageMetaFromContext(ctx)
	origin := agenttools.CallOriginFromContext(ctx)
	botID := cfg.BotID
	if botID == "" {
		botID = meta.BotID
	}
	if botID == "" {
		botID = origin.BotID
	}
	if err := cfg.Repo.Cancel(ctx, botID, id); err != nil {
		return toolErr("commitment not found or already closed"), nil
	}
	return map[string]any{"success": true, "id": id, "message": "已取消"}, nil
}

func kindLabel(kind string) string {
	if kind == dao.OutreachKindWatch {
		return "盯梢"
	}
	return "提醒"
}

func toolErr(msg string) map[string]any {
	return map[string]any{"success": false, "error": msg}
}

func getString(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}
