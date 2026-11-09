package pipeline

import (
	"bufio"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
)

// Replay test against an external JSONL export of persisted assistant turns.
// The dataset lives outside the repository on purpose: it contains real user
// conversations and must never be committed.
//
//	THINKBOT_CLAIM_REPLAY=/path/to/turns.jsonl \
//	THINKBOT_CLAIM_POSITIVE_ID=1234 go test ./agent/pipeline -run TestClaimCheckReplay -v
//
// Each line: {"id":<number>,"content":"...","tool_calls":"<json array or empty>"}
func TestClaimCheckReplay(t *testing.T) {
	path := os.Getenv("THINKBOT_CLAIM_REPLAY")
	if path == "" {
		t.Skip("no replay dataset configured")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	positiveID, _ := strconv.ParseUint(os.Getenv("THINKBOT_CLAIM_POSITIVE_ID"), 10, 64)

	type row struct {
		ID        uint64 `json:"id"`
		Content   string `json:"content"`
		ToolCalls string `json:"tool_calls"`
	}
	var rows []row
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for sc.Scan() {
		var r row
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatalf("decode line: %v", err)
		}
		rows = append(rows, r)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}

	var withTools, withoutTools, toolHits, emptyHits int
	var positiveHit bool
	var hitIDs []string
	var prior []string
	for _, r := range rows {
		calls := parseReplayToolCalls(t, r.ToolCalls)
		ev := ClaimEvidence{
			Commands:      commandsFromReplayCalls(calls),
			PriorCommands: prior,
		}
		// Production runs reconciliation on ClaimVisibleText(genResult.Text);
		// keep the replay on exactly the same input so the acceptance numbers
		// describe what ships, not a slightly different text shape.
		claim := CheckCompletionClaims(ClaimVisibleText(r.Content), ev)
		if len(calls) > 0 {
			withTools++
			if claim != nil {
				toolHits++
				hitIDs = append(hitIDs, strconv.FormatUint(r.ID, 10)+":"+claim.Kind)
			}
		} else {
			withoutTools++
			if claim != nil {
				emptyHits++
				hitIDs = append(hitIDs, strconv.FormatUint(r.ID, 10)+":"+claim.Kind)
			}
		}
		if r.ID == positiveID && claim != nil {
			positiveHit = true
		}
		prior = commandsFromReplayCalls(calls)
	}

	t.Logf("rows=%d with_tools=%d without_tools=%d tool_hits=%d empty_hits=%d",
		len(rows), withTools, withoutTools, toolHits, emptyHits)
	t.Logf("hits=%s", strings.Join(hitIDs, ","))
	if positiveID != 0 && !positiveHit {
		t.Errorf("positive sample %d was not detected", positiveID)
	}
	// Acceptance bar: a turn that really called tools must not be challenged
	// by a retrospective report. Any hit here would loop-back a legitimate
	// reply in production.
	if toolHits != 0 {
		t.Errorf("false positives on turns with tool calls: %d (%s)", toolHits, strings.Join(hitIDs, ","))
	}
}

type replayCall struct {
	Name  string `json:"name"`
	Input any    `json:"input"`
}

func parseReplayToolCalls(t *testing.T, raw string) []replayCall {
	t.Helper()
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "null" {
		return nil
	}
	var calls []replayCall
	if err := json.Unmarshal([]byte(raw), &calls); err != nil {
		return nil
	}
	return calls
}

func commandsFromReplayCalls(calls []replayCall) []string {
	var out []string
	for _, c := range calls {
		if !isShellTool(strings.ToLower(c.Name)) {
			continue
		}
		if s := strings.ToLower(flatReplayInput(c.Input)); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func flatReplayInput(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	default:
		b, err := json.Marshal(x)
		if err != nil {
			return ""
		}
		return string(b)
	}
}
