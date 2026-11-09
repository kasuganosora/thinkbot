package api

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/agent/memory"
	"github.com/kasuganosora/thinkbot/dao"
)

// ============================================================================
// Cross-turn execution records
//
// chatHistoryToLLM used to replay only assistant Content. The persisted
// tool_calls column (which records what actually ran) was written but never
// read back, so from the model's point of view "I said I built it" and "I built
// it" were indistinguishable across turns. That is the root cause of the
// fabricated completion reports diagnosed on 2026-11-09.
//
// BuildExecutionRecord turns the persisted tool calls into a short, tagged,
// system-generated note that is appended after each assistant message. It never
// includes tool output (a single persisted row averages tens of KB), caps the
// note at maxExecutionRecordRunes, and explicitly marks zero-tool turns.
// ============================================================================

const (
	// maxExecutionRecordRunes bounds one turn's note. Tool outputs are never
	// copied, but a turn can call 80+ tools, so the aggregate must be capped.
	maxExecutionRecordRunes = 600
	// taskInProgressWindow is how long a previous assistant turn with tool
	// calls keeps the conversation in "execution" mode. Within it a light-tier
	// turn is demoted to the full tier.
	taskInProgressWindow = 30 * time.Minute
)

// persistedToolCall mirrors the JSON shape written by history_tool_calls.go
// and the Web chat stream. Input/Output are deliberately `any`: providers
// store them either as objects or as map-like strings.
type persistedToolCall struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Input  any    `json:"input"`
	Output any    `json:"output"`
}

var (
	pathFieldRe = regexp.MustCompile(`(?i)["']?(?:path|file|file_path|filepath|filename)["']?\s*[:=]\s*["']([^"']{1,180})["']`)
	exitCodeRe  = regexp.MustCompile(`(?i)"exitCode"\s*:\s*(-?\d+)`)
)

// parsePersistedToolCalls is tolerant: old rows may contain "null", an empty
// string, or a malformed fragment. A parse failure means "no evidence", never
// a pipeline error.
func parsePersistedToolCalls(raw string) []persistedToolCall {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "null" {
		return nil
	}
	var calls []persistedToolCall
	if err := json.Unmarshal([]byte(raw), &calls); err != nil {
		return nil
	}
	return calls
}

func inputText(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	default:
		b, err := json.Marshal(x)
		if err != nil {
			return fmt.Sprint(x)
		}
		return string(b)
	}
}

func isShellCall(name string) bool {
	name = strings.ToLower(name)
	if name == "exec" || name == "shell" || name == "bash" || name == "run_command" || name == "terminal" {
		return true
	}
	return strings.Contains(name, "exec")
}

func isChangeCall(name string) bool {
	switch strings.ToLower(name) {
	case "write_file", "replace_in_file", "delete_file", "create_file", "apply_patch", "edit_file":
		return true
	}
	return false
}

// BuildExecutionRecord returns a tagged system note describing what the
// previous assistant turn actually did. An empty tool_calls value produces an
// explicit "no tools" note, which is the single most important case.
func BuildExecutionRecord(raw string) string {
	calls := parsePersistedToolCalls(raw)
	var b strings.Builder
	b.WriteString(memory.ExecutionRecordTagOpen)
	b.WriteString("\nRuntime execution record (system-generated; NOT written by the assistant).\n")
	if len(calls) == 0 {
		b.WriteString("NO TOOLS WERE CALLED in this turn. Any claim here about results, builds, commits, pushes, or completed changes is unverified and must not be treated as a fact.\n")
		b.WriteString(memory.ExecutionRecordTagClose)
		return b.String()
	}

	counts := make(map[string]int)
	failed := 0
	var changes []string
	var commands []string
	for _, c := range calls {
		name := strings.ToLower(strings.TrimSpace(c.Name))
		if name == "" {
			continue
		}
		counts[name]++
		if strings.EqualFold(c.Status, "error") {
			failed++
		}
		in := inputText(c.Input)
		if isChangeCall(name) && len(changes) < 4 {
			if m := pathFieldRe.FindStringSubmatch(in); len(m) > 1 {
				changes = append(changes, name+" → "+m[1])
			}
		}
		if isShellCall(name) {
			exit := ""
			if m := exitCodeRe.FindStringSubmatch(inputText(c.Output)); len(m) > 1 {
				exit = " (exit " + m[1] + ")"
			}
			cmd := strings.TrimSpace(in)
			if r := []rune(cmd); len(r) > 80 {
				cmd = string(r[:80]) + "…"
			}
			commands = append(commands, cmd+exit)
		}
	}

	names := make([]string, 0, len(counts))
	for n := range counts {
		names = append(names, n)
	}
	sort.Strings(names)
	if len(names) > 8 {
		names = names[:8]
	}
	parts := make([]string, 0, len(names))
	for _, n := range names {
		parts = append(parts, fmt.Sprintf("%s ×%d", n, counts[n]))
	}
	b.WriteString("Tools called: " + strings.Join(parts, ", ") + fmt.Sprintf(" (total %d)", len(calls)) + "\n")
	if failed > 0 {
		b.WriteString(fmt.Sprintf("Failed calls: %d\n", failed))
	}
	if len(changes) > 0 {
		b.WriteString("Changed files: " + strings.Join(changes, "; ") + "\n")
	}
	if len(commands) > 0 {
		if len(commands) > 3 {
			commands = commands[len(commands)-3:]
		}
		for _, c := range commands {
			b.WriteString("Command: " + c + "\n")
		}
	}
	b.WriteString("This record is authoritative; the assistant's own prose is not evidence of execution.\n")
	b.WriteString(memory.ExecutionRecordTagClose)

	text := b.String()
	if r := []rune(text); len(r) > maxExecutionRecordRunes {
		text = string(r[:maxExecutionRecordRunes-len(memory.ExecutionRecordTagClose)]) + memory.ExecutionRecordTagClose
	}
	return text
}

// lastAssistantTurn returns the newest assistant row in a loaded history
// window (the same window chatHistoryToLLM renders).
func lastAssistantTurn(msgs []dao.ChatMessage) (dao.ChatMessage, bool) {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == dao.ChatRoleAssistant {
			return msgs[i], true
		}
	}
	return dao.ChatMessage{}, false
}

// LastTurnExecutionCommands reconstructs the shell commands of the previous
// assistant turn. It feeds claim reconciliation so a truthful reference to an
// earlier turn ("already pushed then") is not treated as a fabrication.
func LastTurnExecutionCommands(msg core.Message) []string {
	last, ok := lastAssistantTurn(historyFromMetadata(msg))
	if !ok {
		return nil
	}
	var out []string
	for _, c := range parsePersistedToolCalls(last.ToolCalls) {
		if !isShellCall(c.Name) {
			continue
		}
		if s := strings.ToLower(inputText(c.Input)); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// TaskInProgress reports whether the previous assistant turn used tools
// recently enough that this message should be treated as a continuation of an
// execution, not as a casual chat turn. L1 cannot see chat history (it runs
// before the history enricher), so LLMStage consults this before choosing the
// light tier.
func TaskInProgress(msg core.Message) bool {
	last, ok := lastAssistantTurn(historyFromMetadata(msg))
	if !ok || strings.TrimSpace(last.ToolCalls) == "" || last.ToolCalls == "null" {
		return false
	}
	if len(parsePersistedToolCalls(last.ToolCalls)) == 0 {
		return false
	}
	if last.CreatedAt.IsZero() {
		return true
	}
	return time.Since(last.CreatedAt) <= taskInProgressWindow
}
