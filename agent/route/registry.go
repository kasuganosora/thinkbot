package route

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/agent/session"
)

const (
	defaultMaxConcurrent = 3
	defaultMaxOpens      = 6
	defaultOpenWindow    = time.Minute
	summaryLimit         = 160
)

// Limits bounds how many execution pipelines one conversation may start.
type Limits struct {
	MaxConcurrent int
	MaxOpens      int
	OpenWindow    time.Duration
}

func (l Limits) orDefault() Limits {
	if l.MaxConcurrent <= 0 {
		l.MaxConcurrent = defaultMaxConcurrent
	}
	if l.MaxOpens <= 0 {
		l.MaxOpens = defaultMaxOpens
	}
	if l.OpenWindow <= 0 {
		l.OpenWindow = defaultOpenWindow
	}
	return l
}

// Run is one in-flight execution pipeline.
type Run struct {
	ID        string
	BotID     string
	Key       string
	TraceID   string
	Summary   string
	System    bool
	Interrupt chan string
	Cancel    context.CancelFunc
	Started   time.Time
}

// Snapshot is the L1 view of a running pipeline: identity and what it is doing.
type Snapshot struct {
	ID      string
	Summary string
	System  bool
	Started time.Time
}

// Registry tracks running execution pipelines, reply-stream ownership, and
// the new-pipeline budget.
type Registry struct {
	mu      sync.Mutex
	limits  Limits
	runs    map[string]*Run
	adopted map[string]string
	opens   map[string][]time.Time
	seq     int
}

// NewRegistry creates an empty registry.
func NewRegistry(limits Limits) *Registry {
	return &Registry{
		limits:  limits.orDefault(),
		runs:    map[string]*Run{},
		adopted: map[string]string{},
		opens:   map[string][]time.Time{},
	}
}

// Add registers a running pipeline. ID defaults to the trace id, then a
// generated id.
func (g *Registry) Add(run Run) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	if run.Started.IsZero() {
		run.Started = time.Now()
	}
	if run.ID == "" {
		run.ID = run.TraceID
	}
	if run.ID == "" {
		g.seq++
		run.ID = fmt.Sprintf("p%d", g.seq)
	}
	copied := run
	g.runs[copied.ID] = &copied
	return copied.ID
}

// Finish removes a pipeline and drops reply adoptions that pointed at it.
func (g *Registry) Finish(id string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	run := g.runs[id]
	delete(g.runs, id)
	if run == nil {
		return
	}
	for inbound, owner := range g.adopted {
		if owner == run.TraceID || inbound == run.TraceID {
			delete(g.adopted, inbound)
		}
	}
}

// List returns pipelines for one bot conversation, oldest first.
func (g *Registry) List(botID, key string) []Snapshot {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []Snapshot
	for _, run := range g.runs {
		if botID != "" && run.BotID != "" && run.BotID != botID {
			continue
		}
		if run.Key != key {
			continue
		}
		out = append(out, Snapshot{
			ID: run.ID, Summary: run.Summary, System: run.System, Started: run.Started,
		})
	}
	sortSnapshots(out)
	return out
}

func sortSnapshots(out []Snapshot) {
	for i := 1; i < len(out); i++ {
		j := i
		for j > 0 && out[j].Started.Before(out[j-1].Started) {
			out[j], out[j-1] = out[j-1], out[j]
			j--
		}
	}
}

// RunningCount is the number of user pipelines in a conversation.
func (g *Registry) RunningCount(key string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.userRunsLocked(key)
}

func (g *Registry) userRunsLocked(key string) int {
	n := 0
	for _, run := range g.runs {
		if run.Key == key && !run.System {
			n++
		}
	}
	return n
}

// Opens is how many pipelines this conversation has started inside the window.
func (g *Registry) Opens(key string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pruneOpensLocked(key, time.Now())
	return len(g.opens[key])
}

// AllowOpen reserves one user pipeline if the conversation is under budget.
func (g *Registry) AllowOpen(key string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	g.pruneOpensLocked(key, now)
	if g.userRunsLocked(key) >= g.limits.MaxConcurrent {
		return false
	}
	if len(g.opens[key]) >= g.limits.MaxOpens {
		return false
	}
	g.opens[key] = append(g.opens[key], now)
	return true
}

func (g *Registry) pruneOpensLocked(key string, now time.Time) {
	kept := g.opens[key][:0]
	for _, at := range g.opens[key] {
		if now.Sub(at) < g.limits.OpenWindow {
			kept = append(kept, at)
		}
	}
	if len(kept) == 0 {
		delete(g.opens, key)
		return
	}
	g.opens[key] = kept
}

// Preempt injects text into a running pipeline and gives that pipeline sole
// ownership of the reply stream. The inbound trace must not also write one.
func (g *Registry) Preempt(id, key, inboundTrace, text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return fmt.Errorf("empty preempt context")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	run, ok := g.runs[id]
	if !ok {
		return fmt.Errorf("pipeline %q is not running", id)
	}
	if key != "" && run.Key != key {
		return fmt.Errorf("pipeline %q belongs to another conversation", id)
	}
	if run.Interrupt == nil {
		return fmt.Errorf("pipeline %q cannot accept context", id)
	}
	select {
	case run.Interrupt <- text:
	default:
		return fmt.Errorf("pipeline %q is not accepting context", id)
	}
	if extra := clip(text, 80); extra != "" {
		run.Summary = clip(run.Summary+" | "+extra, summaryLimit)
	}
	if inboundTrace != "" && inboundTrace != run.TraceID {
		g.adopted[inboundTrace] = run.TraceID
	}
	return nil
}

// PreemptLatest injects text into the newest user pipeline in the conversation.
func (g *Registry) PreemptLatest(key, inboundTrace, text string) (string, error) {
	g.mu.Lock()
	var latest *Run
	for _, run := range g.runs {
		if run.Key != key || run.System || run.Interrupt == nil {
			continue
		}
		if latest == nil || run.Started.After(latest.Started) {
			latest = run
		}
	}
	id := ""
	if latest != nil {
		id = latest.ID
	}
	g.mu.Unlock()
	if id == "" {
		return "", fmt.Errorf("no running pipeline in this conversation")
	}
	if err := g.Preempt(id, key, inboundTrace, text); err != nil {
		return "", err
	}
	return id, nil
}

// Cancel stops a running pipeline. It does not start another one.
func (g *Registry) Cancel(id, key string) error {
	g.mu.Lock()
	run, ok := g.runs[id]
	if !ok {
		g.mu.Unlock()
		return fmt.Errorf("pipeline %q is not running", id)
	}
	if key != "" && run.Key != key {
		g.mu.Unlock()
		return fmt.Errorf("pipeline %q belongs to another conversation", id)
	}
	cancel := run.Cancel
	g.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	g.Finish(id)
	return nil
}

// OwnsReply reports whether this trace may write a reply stream.
// A trace adopted by preempt must not.
func (g *Registry) OwnsReply(trace string) bool {
	if trace == "" {
		return true
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	_, adopted := g.adopted[trace]
	return !adopted
}

// ConversationKey identifies the conversation a turn belongs to.
// The order matches the execution pipeline: session, chat session, then channel.
func ConversationKey(env *core.Envelope) string {
	if env == nil {
		return ""
	}
	if sid := session.SessionIDFromEnvelope(env); sid != "" {
		return "sess:" + sid
	}
	if env.Message.Metadata != nil {
		if sid, ok := env.Message.Metadata["chat_session_id"].(string); ok && sid != "" {
			return "chat:" + sid
		}
	}
	m := env.Message
	if m.Source == "" && m.Channel == "" {
		return ""
	}
	return "chan:" + m.BotID + ":" + m.Source + ":" + m.Channel
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
