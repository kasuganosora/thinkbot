package telegram

import (
	"context"
	"time"

	"github.com/kasuganosora/thinkbot/util/traceid"
)

// ============================================================================
// Typing keepalive — refresh sendChatAction("typing") while a reply is generating.
//
// Telegram's typing indicator expires ~5s after sendChatAction. Previously we
// fired it once on inbound and then waited for the full reply, so long agent
// runs looked stuck. This keepalive refreshes ~every 4s until Send / cancel /
// error / max age.
//
// Groups and private chats both get typing refresh. Placeholder + editMessageText
// streaming is private-only — see stream.go and isPrivateChat.
// ============================================================================

const (
	// typingRefreshInterval must stay under Telegram's ~5s typing expiry.
	typingRefreshInterval = 4 * time.Second

	// typingMaxAge is the hard stop when pipeline never replies (silent /
	// suppressed / error without Send) and liveness cannot observe end
	// (mid-generation hang). Ops: 2–3 min; was 10 min and felt stuck.
	typingMaxAge = 3 * time.Minute

	// typingActionTimeout bounds a single sendChatAction call so a hung API
	// cannot pin the keepalive loop.
	typingActionTimeout = 5 * time.Second
)

// Overridable in tests (same pattern as wecom deltaLivenessInterval).
var (
	typingRefresh = typingRefreshInterval
	typingMaxAgeD = typingMaxAge
)

// typingSession is one in-flight typing keepalive for a chat.
type typingSession struct {
	cancel  context.CancelFunc
	chatID  int64
	traceID string // orchestrator trace; empty → max-age only (no liveness)
	gen     uint64 // bumped on each restart so a stale loop can exit cleanly
}

// startTypingKeepalive begins (or restarts) typing refresh for chatID.
//
// Safe to call from handleUpdate before Ingress.Receive: the first action is
// sent immediately, then every typingRefresh until stopTypingKeepalive /
// stopAllTyping / turn inactive (cancel / timeout / silent) / max age.
// A new inbound for the same chat cancels the old session — no leaked
// goroutines across superseding turns.
//
// traceID binds the keepalive to the orchestrator turn so cancel / silent /
// drop / hard-timeout (OnMessageDone → IsMessageActive false) release typing
// without waiting for max age. Empty traceID → max-age only.
func (c *TelegramChannel) startTypingKeepalive(chatID int64, traceID string) {
	if chatID == 0 {
		return
	}

	// Resolve parent BEFORE taking typingMu: typingParent takes c.mu, and Stop
	// holds c.mu then typingMu — inverted order here would deadlock.
	parent := c.typingParent()

	c.typingMu.Lock()
	if c.typing == nil {
		c.typing = make(map[int64]*typingSession)
	}
	if old, ok := c.typing[chatID]; ok && old != nil {
		old.cancel()
	}
	ctx, cancel := context.WithCancel(parent)
	c.typingGen++
	gen := c.typingGen
	c.typing[chatID] = &typingSession{cancel: cancel, chatID: chatID, traceID: traceID, gen: gen}
	c.typingMu.Unlock()

	c.wg.Add(1)
	go c.typingLoop(ctx, chatID, gen, traceID)
}

// stopTypingKeepalive cancels the keepalive for chatID (idempotent).
// Called when the final reply is sent, Receive fails, or the turn is done.
func (c *TelegramChannel) stopTypingKeepalive(chatID int64) {
	c.typingMu.Lock()
	defer c.typingMu.Unlock()
	if s, ok := c.typing[chatID]; ok && s != nil {
		s.cancel()
		delete(c.typing, chatID)
	}
}

// stopAllTyping cancels every keepalive. Called from Stop before wg.Wait.
func (c *TelegramChannel) stopAllTyping() {
	c.typingMu.Lock()
	defer c.typingMu.Unlock()
	for id, s := range c.typing {
		if s != nil {
			s.cancel()
		}
		delete(c.typing, id)
	}
}

// typingParent returns the parent ctx for keepalive goroutines.
// Prefer runCtx so Stop cancels keepalives; Background when Start has not run.
//
// Must not be called while holding typingMu (Stop takes c.mu then typingMu).
func (c *TelegramChannel) typingParent() context.Context {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.runCtx != nil {
		return c.runCtx
	}
	return context.Background()
}

// typingLoop sends typing immediately, then every typingRefresh until cancel,
// orchestrator turn end, or typingMaxAgeD. Short replies that stop before the
// first tick produce exactly one sendChatAction — no API spam.
func (c *TelegramChannel) typingLoop(ctx context.Context, chatID int64, gen uint64, traceID string) {
	defer c.wg.Done()
	defer c.clearTypingIfGen(chatID, gen)

	c.sendTypingOnce(ctx, chatID)

	ticker := time.NewTicker(typingRefresh)
	defer ticker.Stop()
	deadline := time.NewTimer(typingMaxAgeD)
	defer deadline.Stop()

	// Liveness watchdog: pipeline cancel / silent / drop / hard-timeout end the
	// turn without Send. Without this, typing would refresh until max age.
	// Grace matches streamStartGrace — startTyping runs before OnMessageStart.
	var liveWatch *time.Ticker
	if traceID != "" && c.liveness != nil {
		liveWatch = time.NewTicker(streamLivenessInterval)
		defer liveWatch.Stop()
	}
	start := time.Now()
	sawActive := false

	for {
		var liveC <-chan time.Time
		if liveWatch != nil {
			liveC = liveWatch.C
		}
		select {
		case <-ctx.Done():
			return
		case <-deadline.C:
			traceid.L(ctx).Debugw("telegram: typing keepalive reached max age",
				"channel", c.name, "chat_id", chatID, "trace_id", traceID)
			return
		case <-liveC:
			if c.turnAlive(traceID) {
				sawActive = true
				continue
			}
			if !sawActive && time.Since(start) < streamStartGrace {
				continue
			}
			traceid.L(ctx).Debugw("telegram: typing keepalive released (turn inactive)",
				"channel", c.name, "chat_id", chatID, "trace_id", traceID,
				"saw_active", sawActive, "elapsed", time.Since(start).String())
			return
		case <-ticker.C:
			if err := c.sendTypingOnce(ctx, chatID); err != nil {
				// Transient failures are fine — next tick retries. Permanent
				// ctx cancel exits via the Done case on the next select.
				traceid.L(ctx).Debugw("telegram: typing keepalive refresh failed",
					"channel", c.name, "chat_id", chatID, "err", err)
			}
		}
	}
}

// clearTypingIfGen drops the map entry only if it still belongs to this loop
// generation (a newer startTypingKeepalive must not be wiped by a stale defer).
func (c *TelegramChannel) clearTypingIfGen(chatID int64, gen uint64) {
	c.typingMu.Lock()
	defer c.typingMu.Unlock()
	if s, ok := c.typing[chatID]; ok && s != nil && s.gen == gen {
		delete(c.typing, chatID)
	}
}

// sendTypingOnce fires one sendChatAction("typing") with a short timeout.
// typingAction (if set) replaces the real API — used by unit tests.
func (c *TelegramChannel) sendTypingOnce(ctx context.Context, chatID int64) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	actionCtx, cancel := context.WithTimeout(ctx, typingActionTimeout)
	defer cancel()
	if c.typingAction != nil {
		return c.typingAction(actionCtx, chatID)
	}
	if c.api == nil {
		return nil
	}
	return c.api.sendChatAction(actionCtx, chatID, "typing")
}

// isPrivateChat reports whether chatType is a 1:1 DM.
//
// Placeholder + editMessageText streaming (stream.go) must stay private-only:
// group/supergroup placeholders would spam the room. Typing keepalive still
// runs in every chat type.
func isPrivateChat(chatType string) bool {
	return chatType == "private"
}

// typingActive reports whether a keepalive is registered for chatID (tests).
func (c *TelegramChannel) typingActive(chatID int64) bool {
	c.typingMu.Lock()
	defer c.typingMu.Unlock()
	_, ok := c.typing[chatID]
	return ok
}
