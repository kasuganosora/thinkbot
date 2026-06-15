package pipeline

import (
	"context"
	"regexp"
	"testing"

	"github.com/kasuganosora/thinkbot/agent/core"
)

// ============================================================================
// Predicate 测试
// ============================================================================

func predEnvelope(text, source, channel string, meta map[string]any) *core.Envelope {
	return core.NewEnvelope(core.Message{
		ID:       "test",
		Text:     text,
		Source:   source,
		Channel:  channel,
		Metadata: meta,
	})
}

func TestPredicateFunc(t *testing.T) {
	p := PredicateFunc(func(env *core.Envelope) bool {
		return env.Message.Text == "hello"
	})
	if !p.Match(predEnvelope("hello", "", "", nil)) {
		t.Error("should match")
	}
	if p.Match(predEnvelope("world", "", "", nil)) {
		t.Error("should not match")
	}
}

func TestTextContains(t *testing.T) {
	p := &TextContains{Substring: "world"}
	if !p.Match(predEnvelope("hello world", "", "", nil)) {
		t.Error("should match")
	}
	if p.Match(predEnvelope("hello", "", "", nil)) {
		t.Error("should not match")
	}
	// 空子串始终匹配
	empty := &TextContains{Substring: ""}
	if !empty.Match(predEnvelope("anything", "", "", nil)) {
		t.Error("empty substring should always match")
	}
}

func TestTextHasPrefix(t *testing.T) {
	p := &TextHasPrefix{Prefix: "/cmd"}
	if !p.Match(predEnvelope("/cmd foo", "", "", nil)) {
		t.Error("should match prefix")
	}
	if p.Match(predEnvelope("hello /cmd", "", "", nil)) {
		t.Error("should not match non-prefix")
	}
}

func TestTextRegex(t *testing.T) {
	p := &TextRegex{Pattern: regexp.MustCompile(`^\d+$`)}
	if !p.Match(predEnvelope("12345", "", "", nil)) {
		t.Error("should match digits")
	}
	if p.Match(predEnvelope("abc", "", "", nil)) {
		t.Error("should not match non-digits")
	}
}

func TestSourceEquals(t *testing.T) {
	p := &SourceEquals{Source: "webhook"}
	if !p.Match(predEnvelope("", "webhook", "", nil)) {
		t.Error("should match webhook")
	}
	if p.Match(predEnvelope("", "websocket", "", nil)) {
		t.Error("should not match websocket")
	}
}

func TestChannelEquals(t *testing.T) {
	p := &ChannelEquals{Channel: "general"}
	if !p.Match(predEnvelope("", "", "general", nil)) {
		t.Error("should match")
	}
	if p.Match(predEnvelope("", "", "random", nil)) {
		t.Error("should not match")
	}
}

func TestMetadataExists(t *testing.T) {
	p := &MetadataExists{Key: "vip"}
	if !p.Match(predEnvelope("", "", "", map[string]any{"vip": true})) {
		t.Error("should match when key exists")
	}
	if p.Match(predEnvelope("", "", "", map[string]any{"other": true})) {
		t.Error("should not match when key missing")
	}
	if p.Match(predEnvelope("", "", "", nil)) {
		t.Error("should not match nil metadata")
	}
}

func TestMetadataEquals(t *testing.T) {
	p := &MetadataEquals{Key: "role", Value: "admin"}
	if !p.Match(predEnvelope("", "", "", map[string]any{"role": "admin"})) {
		t.Error("should match")
	}
	if p.Match(predEnvelope("", "", "", map[string]any{"role": "user"})) {
		t.Error("should not match different value")
	}
}

func TestValueExists(t *testing.T) {
	p := &ValueExists{Key: "processed"}
	env := core.NewEnvelope(core.Message{ID: "test"})
	if p.Match(env) {
		t.Error("should not match before set")
	}
	env.Set("processed", true)
	if !p.Match(env) {
		t.Error("should match after set")
	}
}

// ============================================================================
// 组合谓词测试
// ============================================================================

func TestAnd(t *testing.T) {
	p := &And{Predicates: []Predicate{
		&SourceEquals{Source: "webhook"},
		&TextContains{Substring: "hello"},
	}}
	if !p.Match(predEnvelope("hello", "webhook", "", nil)) {
		t.Error("should match when both conditions true")
	}
	if p.Match(predEnvelope("world", "webhook", "", nil)) {
		t.Error("should not match when one condition false")
	}
	// Empty And matches everything
	empty := &And{}
	if !empty.Match(predEnvelope("anything", "", "", nil)) {
		t.Error("empty And should match everything")
	}
}

func TestOr(t *testing.T) {
	p := &Or{Predicates: []Predicate{
		&SourceEquals{Source: "webhook"},
		&SourceEquals{Source: "websocket"},
	}}
	if !p.Match(predEnvelope("", "webhook", "", nil)) {
		t.Error("should match webhook")
	}
	if !p.Match(predEnvelope("", "websocket", "", nil)) {
		t.Error("should match websocket")
	}
	if p.Match(predEnvelope("", "polling", "", nil)) {
		t.Error("should not match polling")
	}
	// Empty Or matches nothing
	empty := &Or{}
	if empty.Match(predEnvelope("anything", "", "", nil)) {
		t.Error("empty Or should match nothing")
	}
}

func TestNot(t *testing.T) {
	p := &Not{Inner: &SourceEquals{Source: "webhook"}}
	if p.Match(predEnvelope("", "webhook", "", nil)) {
		t.Error("should not match webhook")
	}
	if !p.Match(predEnvelope("", "websocket", "", nil)) {
		t.Error("should match non-webhook")
	}
}

// ============================================================================
// 便捷构造函数测试
// ============================================================================

func TestMatchAll(t *testing.T) {
	if !MatchAll().Match(predEnvelope("anything", "", "", nil)) {
		t.Error("MatchAll should always match")
	}
}

func TestMatchNone(t *testing.T) {
	if MatchNone().Match(predEnvelope("anything", "", "", nil)) {
		t.Error("MatchNone should never match")
	}
}

// ============================================================================
// Router 测试
// ============================================================================

func TestRouter_MatchingRoute(t *testing.T) {
	router := NewRouter("test-router",
		Route{
			Name:      "webhook-route",
			Predicate: MatchSource("webhook"),
			Stages: []core.Stage{
				&core.StageFunc{
					StageName: "webhook-handler",
					Fn: func(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
						env.Set("route", "webhook")
						return env, nil
					},
				},
			},
		},
		Route{
			Name:      "ws-route",
			Predicate: MatchSource("websocket"),
			Stages: []core.Stage{
				&core.StageFunc{
					StageName: "ws-handler",
					Fn: func(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
						env.Set("route", "websocket")
						return env, nil
					},
				},
			},
		},
	)

	env := core.NewEnvelope(core.Message{ID: "msg-1", Source: "webhook"})
	result, err := router.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	v, _ := result.Get("route")
	if v != "webhook" {
		t.Errorf("expected webhook route, got %v", v)
	}
}

func TestRouter_Fallback(t *testing.T) {
	router := NewRouter("test-router",
		Route{
			Name:      "specific",
			Predicate: MatchSource("webhook"),
			Stages: []core.Stage{
				&core.StageFunc{
					StageName: "webhook-handler",
					Fn: func(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
						env.Set("route", "webhook")
						return env, nil
					},
				},
			},
		},
		Route{
			Name:     "fallback",
			Fallback: true,
			Stages: []core.Stage{
				&core.StageFunc{
					StageName: "fallback-handler",
					Fn: func(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
						env.Set("route", "fallback")
						return env, nil
					},
				},
			},
		},
	)

	env := core.NewEnvelope(core.Message{ID: "msg-1", Source: "unknown"})
	result, err := router.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	v, _ := result.Get("route")
	if v != "fallback" {
		t.Errorf("expected fallback route, got %v", v)
	}
}

func TestRouter_NoMatch_Passthrough(t *testing.T) {
	router := NewRouter("test-router",
		Route{
			Name:      "specific",
			Predicate: MatchSource("webhook"),
			Stages: []core.Stage{
				&core.StageFunc{
					StageName: "handler",
					Fn: func(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
						t.Error("should not be called")
						return env, nil
					},
				},
			},
		},
	)

	env := core.NewEnvelope(core.Message{ID: "msg-1", Source: "unknown"})
	result, err := router.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Message.ID != "msg-1" {
		t.Error("should passthrough when no match")
	}
}
