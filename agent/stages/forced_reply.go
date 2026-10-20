package stages

import (
	"sync"
	"time"

	"github.com/kasuganosora/thinkbot/agent/core"
)

const (
	defaultForcedReplyMaxPerThread = 3
	defaultForcedReplyWindow       = 10 * time.Minute
)

// forcedReplyLimiter is a per-thread sliding-window counter for replies forced
// by an explicit @mention, so repeated @-spam cannot drag the bot into
// replying endlessly. It only gates the *forcing*; the model may still reply
// on its own via the normal path.
type forcedReplyLimiter struct {
	mu   sync.Mutex
	hits map[string][]time.Time
	now  func() time.Time // test hook
}

func (l *forcedReplyLimiter) allow(key string, max int, window time.Duration) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if l.now != nil {
		now = l.now()
	}
	if l.hits == nil {
		l.hits = make(map[string][]time.Time)
	}
	cutoff := now.Add(-window)
	// opportunistic GC of stale threads
	if len(l.hits) > 1024 {
		for k, ts := range l.hits {
			if len(ts) == 0 || !ts[len(ts)-1].After(cutoff) {
				delete(l.hits, k)
			}
		}
	}
	kept := l.hits[key][:0]
	for _, t := range l.hits[key] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= max {
		l.hits[key] = kept
		return false
	}
	l.hits[key] = append(kept, now)
	return true
}

// forcedReplyThreadKey identifies the conversation thread: the reply target's
// parent note when present (Misskey reply_id), otherwise the chat channel.
func forcedReplyThreadKey(env *core.Envelope) string {
	key := env.Message.Source + "|" + env.Message.Channel
	if env.Message.Metadata != nil {
		if rid, ok := env.Message.Metadata["reply_id"].(string); ok && rid != "" {
			return key + "|" + rid
		}
	}
	return key
}

// forceDirectReply reports whether this message's explicit @mention should
// override the model's send decision, consuming one slot of the per-thread
// budget. Bot authors never force (anti-loop, see isExplicitlyAddressed).
func (s *LLMStage) forceDirectReply(env *core.Envelope) bool {
	if !isExplicitlyAddressed(env) {
		return false
	}
	max := s.config.ForcedReplyMaxPerThread
	if max < 0 {
		return false
	}
	if max == 0 {
		max = defaultForcedReplyMaxPerThread
	}
	window := s.config.ForcedReplyWindow
	if window <= 0 {
		window = defaultForcedReplyWindow
	}
	if !s.forcedReplies.allow(forcedReplyThreadKey(env), max, window) {
		s.logger.Infow("direct mention: forced-reply cap reached for thread, deferring to model decision",
			"message_id", env.Message.ID)
		return false
	}
	return true
}
