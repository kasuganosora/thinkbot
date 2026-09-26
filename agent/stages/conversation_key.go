package stages

import (
	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/agent/session"
)

// conversationKey identifies the conversation a turn belongs to, for state
// that must be scoped to one conversation (deferred-tool load state, the
// automatic compactor's anchored summary, compact_context cooldowns).
//
// It is derived from what the pipeline actually carries:
//  1. "sess:<id>"  — session.id, when a SessionStage is wired in;
//  2. "chat:<id>"  — the chat_messages session (web / telegram, e.g. "tg:<chat>");
//  3. "chan:<bot>:<source>:<channel>" — the platform conversation space
//     (Telegram chat id, Misskey user / timeline, ...).
//
// Background (2026-09-26): state used to be keyed by session.id alone, which
// is never set in production (SessionStage is not in the pipeline), so every
// turn of a bot shared ONE ToolDeferral and ONE "__default__" compactor: a
// concurrent Misskey timeline turn replaced the tool list of a running
// Telegram turn, and summaries could leak across conversations.
//
// Returns "" only when nothing identifies the conversation (no session, no
// source and no channel); callers must then use ephemeral per-turn state and
// never a shared per-bot object.
func conversationKey(env *core.Envelope) string {
	if env == nil {
		return ""
	}
	if sid := session.SessionIDFromEnvelope(env); sid != "" {
		return "sess:" + sid
	}
	if cs := chatSessionIDFromEnvelope(env); cs != "" {
		return "chat:" + cs
	}
	m := env.Message
	if m.Source == "" && m.Channel == "" {
		return ""
	}
	return "chan:" + m.BotID + ":" + m.Source + ":" + m.Channel
}
