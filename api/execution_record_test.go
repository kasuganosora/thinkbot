package api

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/dao"
)

func TestBuildExecutionRecord_NoTools(t *testing.T) {
	for _, raw := range []string{"", "null", "  "} {
		got := BuildExecutionRecord(raw)
		if !strings.Contains(got, "NO TOOLS WERE CALLED") {
			t.Fatalf("raw=%q must be marked as zero-tool turn: %s", raw, got)
		}
		if !strings.HasPrefix(got, "<execution_record>") || !strings.HasSuffix(got, "</execution_record>") {
			t.Fatalf("record must be a complete tagged block: %s", got)
		}
	}
}

func TestBuildExecutionRecord_NoOutputAndCapped(t *testing.T) {
	// Inputs/outputs are large in real rows; the record must aggregate only.
	raw := `[` +
		`{"name":"exec","status":"success","input":{"command":"go build ./..."},"output":"` + strings.Repeat("x", 5000) + `"},` +
		`{"name":"exec","status":"error","input":{"command":"make proto"},"output":"{\"exitCode\":2}"},` +
		`{"name":"replace_in_file","status":"success","input":{"path":"app/service/admin.go"}},` +
		`{"name":"read_file","status":"success","input":{"path":"README.md"}}` +
		`]`
	got := BuildExecutionRecord(raw)
	if strings.Contains(got, strings.Repeat("x", 100)) {
		t.Fatal("tool output must never be copied into the record")
	}
	for _, want := range []string{"exec ×2", "Failed calls: 1", "go build", "make proto", "admin.go"} {
		if !strings.Contains(got, want) {
			t.Fatalf("record missing %q: %s", want, got)
		}
	}
	if utf8.RuneCountInString(got) > maxExecutionRecordRunes {
		t.Fatalf("record too long: %d runes", utf8.RuneCountInString(got))
	}
}

func TestBuildExecutionRecord_ManyCallsStaysCapped(t *testing.T) {
	var b strings.Builder
	b.WriteByte('[')
	for i := 0; i < 120; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"name":"exec","status":"success","input":{"command":"very long command number `)
		b.WriteString(strings.Repeat("x", 40))
		b.WriteString(`"}}`)
	}
	b.WriteByte(']')
	got := BuildExecutionRecord(b.String())
	if n := utf8.RuneCountInString(got); n > maxExecutionRecordRunes {
		t.Fatalf("record not capped: %d runes", n)
	}
	if !strings.HasSuffix(got, "</execution_record>") {
		t.Fatal("capped record must still be a closed tag")
	}
}

func TestLastTurnExecutionCommands(t *testing.T) {
	history := []dao.ChatMessage{
		{Role: dao.ChatRoleUser, Content: "继续"},
		{Role: dao.ChatRoleAssistant, Content: "改完了", ToolCalls: `[{"name":"exec","input":{"command":"Go Build ./... && Make Proto"}}]`},
	}
	msg := core.Message{Metadata: map[string]any{"chat_history": history}}
	cmds := LastTurnExecutionCommands(msg)
	if len(cmds) != 1 || !strings.Contains(cmds[0], "go build") || !strings.Contains(cmds[0], "make proto") {
		t.Fatalf("commands = %#v", cmds)
	}
}

func TestTaskInProgress(t *testing.T) {
	withHistory := func(last dao.ChatMessage) core.Message {
		return core.Message{Metadata: map[string]any{
			"chat_history": []dao.ChatMessage{{Role: dao.ChatRoleUser, Content: "q"}, last},
		}}
	}
	if TaskInProgress(core.Message{}) {
		t.Fatal("no history must not be task-in-progress")
	}
	if TaskInProgress(withHistory(dao.ChatMessage{Role: dao.ChatRoleAssistant, Content: "ok"})) {
		t.Fatal("zero-tool assistant turn must not be task-in-progress")
	}
	active := dao.ChatMessage{
		Role:      dao.ChatRoleAssistant,
		Content:   "done",
		ToolCalls: `[{"name":"exec","input":{"command":"go build"}}]`,
		CreatedAt: time.Now().Add(-2 * time.Minute),
	}
	if !TaskInProgress(withHistory(active)) {
		t.Fatal("recent turn with tool calls must be task-in-progress")
	}
	active.CreatedAt = time.Now().Add(-2 * time.Hour)
	if TaskInProgress(withHistory(active)) {
		t.Fatal("stale turn must not be task-in-progress")
	}
}
