package core

import (
	"strings"
	"sync"
)

// ============================================================================
// Listen channels
//
// A listen channel is a passive feed the bot reads but is not addressed in
// (e.g. the Misskey / Mastodon timeline, where every unaddressed post of the
// timeline shares one Message.Channel). Its memory scope fills with raw
// notes all day; it is digested once per day instead of being deduplicated
// on the fly. Channel adapters register the channel IDs they use for such
// feeds; the operator setting memory.listen_scopes overrides the list.
// ============================================================================

var listenChannels = struct {
	sync.RWMutex
	registered map[string]bool
	override   map[string]bool // nil = no override
}{registered: map[string]bool{}}

// RegisterListenChannel marks a Message.Channel value as a passive listen feed.
func RegisterListenChannel(channelID string) {
	listenChannels.Lock()
	defer listenChannels.Unlock()
	listenChannels.registered[channelID] = true
}

// SetListenChannelsOverride replaces the registered list (operator setting).
// nil restores the registered list; an empty slice disables listen handling.
func SetListenChannelsOverride(ids []string) {
	listenChannels.Lock()
	defer listenChannels.Unlock()
	if ids == nil {
		listenChannels.override = nil
		return
	}
	m := map[string]bool{}
	for _, id := range ids {
		if id = strings.TrimSpace(id); id != "" {
			m[id] = true
		}
	}
	listenChannels.override = m
}

// IsListenChannel reports whether channelID is a passive listen feed.
func IsListenChannel(channelID string) bool {
	listenChannels.RLock()
	defer listenChannels.RUnlock()
	if listenChannels.override != nil {
		return listenChannels.override[channelID]
	}
	return listenChannels.registered[channelID]
}

// ListenChannels returns the effective listen channel IDs.
func ListenChannels() []string {
	listenChannels.RLock()
	defer listenChannels.RUnlock()
	src := listenChannels.registered
	if listenChannels.override != nil {
		src = listenChannels.override
	}
	out := make([]string, 0, len(src))
	for id := range src {
		out = append(out, id)
	}
	return out
}
