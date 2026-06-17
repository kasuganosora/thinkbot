package stages

import (
	"context"
	"testing"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/llm"
)

func TestPrefixDecider_Reply(t *testing.T) {
	result := &llm.GenerateResult{Text: "[REPLY] Hello!"}
	decision, reply, note, _ := PrefixDecider(context.Background(), core.Message{}, result)

	if decision != DecisionReply {
		t.Errorf("decision = %q, want %q", decision, DecisionReply)
	}
	if reply != "Hello!" {
		t.Errorf("reply = %q, want %q", reply, "Hello!")
	}
	if note != "" {
		t.Errorf("note should be empty, got %q", note)
	}
}

func TestPrefixDecider_NoteOnly(t *testing.T) {
	result := &llm.GenerateResult{Text: "[NOTE] User likes dark mode"}
	decision, reply, note, category := PrefixDecider(context.Background(), core.Message{}, result)

	if decision != DecisionNoteOnly {
		t.Errorf("decision = %q, want %q", decision, DecisionNoteOnly)
	}
	if reply != "" {
		t.Errorf("reply should be empty, got %q", reply)
	}
	if note != "User likes dark mode" {
		t.Errorf("note = %q, want %q", note, "User likes dark mode")
	}
	if category != "observation" {
		t.Errorf("category = %q, want %q", category, "observation")
	}
}

func TestPrefixDecider_ReplyWithNote(t *testing.T) {
	result := &llm.GenerateResult{Text: "[REPLY+NOTE] Here's the fix.[---]User uses Go 1.21"}
	decision, reply, note, category := PrefixDecider(context.Background(), core.Message{}, result)

	if decision != DecisionReplyWithNote {
		t.Errorf("decision = %q, want %q", decision, DecisionReplyWithNote)
	}
	if reply != "Here's the fix." {
		t.Errorf("reply = %q, want %q", reply, "Here's the fix.")
	}
	if note != "User uses Go 1.21" {
		t.Errorf("note = %q, want %q", note, "User uses Go 1.21")
	}
	if category != "insight" {
		t.Errorf("category = %q, want %q", category, "insight")
	}
}

func TestPrefixDecider_ReplyWithNote_NoSeparator(t *testing.T) {
	// No [---] separator → falls back to plain reply
	result := &llm.GenerateResult{Text: "[REPLY+NOTE] Just a reply without separator"}
	decision, reply, note, _ := PrefixDecider(context.Background(), core.Message{}, result)

	if decision != DecisionReply {
		t.Errorf("decision = %q, want %q", decision, DecisionReply)
	}
	if reply != "Just a reply without separator" {
		t.Errorf("reply = %q, want %q", reply, "Just a reply without separator")
	}
	if note != "" {
		t.Errorf("note should be empty, got %q", note)
	}
}

func TestPrefixDecider_Skip(t *testing.T) {
	result := &llm.GenerateResult{Text: "[SKIP]"}
	decision, reply, note, _ := PrefixDecider(context.Background(), core.Message{}, result)

	if decision != DecisionDrop {
		t.Errorf("decision = %q, want %q", decision, DecisionDrop)
	}
	if reply != "" {
		t.Errorf("reply should be empty, got %q", reply)
	}
	if note != "" {
		t.Errorf("note should be empty, got %q", note)
	}
}

func TestPrefixDecider_NoPrefixDefaultsToReply(t *testing.T) {
	result := &llm.GenerateResult{Text: "Just normal text without any prefix"}
	decision, reply, note, _ := PrefixDecider(context.Background(), core.Message{}, result)

	if decision != DecisionReply {
		t.Errorf("decision = %q, want %q", decision, DecisionReply)
	}
	if reply != "Just normal text without any prefix" {
		t.Errorf("reply = %q, want %q", reply, "Just normal text without any prefix")
	}
	if note != "" {
		t.Errorf("note should be empty, got %q", note)
	}
}

func TestPrefixDecider_EmptyText(t *testing.T) {
	result := &llm.GenerateResult{Text: ""}
	decision, _, _, _ := PrefixDecider(context.Background(), core.Message{}, result)

	if decision != DecisionDrop {
		t.Errorf("decision = %q, want %q", decision, DecisionDrop)
	}
}

func TestResolveReplyTarget_WithMetadata(t *testing.T) {
	msg := core.Message{
		Channel: "user-123",
		Metadata: map[string]any{
			"reply_target": "note-abc",
		},
	}
	target := resolveReplyTarget(msg)
	if target != "note-abc" {
		t.Errorf("target = %q, want %q", target, "note-abc")
	}
}

func TestResolveReplyTarget_FallbackToChannel(t *testing.T) {
	msg := core.Message{
		Channel: "chat-456",
	}
	target := resolveReplyTarget(msg)
	if target != "chat-456" {
		t.Errorf("target = %q, want %q", target, "chat-456")
	}
}

func TestResolveReplyTarget_EmptyReplyTarget(t *testing.T) {
	msg := core.Message{
		Channel: "chat-789",
		Metadata: map[string]any{
			"reply_target": "", // empty → fallback
		},
	}
	target := resolveReplyTarget(msg)
	if target != "chat-789" {
		t.Errorf("target = %q, want %q", target, "chat-789")
	}
}

func TestTrimSpace(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"  hello  ", "hello"},
		{"\n\thello\n\t", "hello"},
		{"hello", "hello"},
		{"", ""},
		{"   ", ""},
	}
	for _, tc := range tests {
		got := trimSpace(tc.input)
		if got != tc.expected {
			t.Errorf("trimSpace(%q) = %q, want %q", tc.input, got, tc.expected)
		}
	}
}

func TestIndexOf(t *testing.T) {
	tests := []struct {
		s, sub string
		want   int
	}{
		{"hello[---]world", "[---]", 5},
		{"no separator", "[---]", -1},
		{"[---]start", "[---]", 0},
		{"end[---]", "[---]", 3},
	}
	for _, tc := range tests {
		got := indexOf(tc.s, tc.sub)
		if got != tc.want {
			t.Errorf("indexOf(%q, %q) = %d, want %d", tc.s, tc.sub, got, tc.want)
		}
	}
}
