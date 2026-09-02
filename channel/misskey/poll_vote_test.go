package misskey

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/kasuganosora/thinkbot/internal/interaction"
)

func TestAddPollChoice(t *testing.T) {
	st := &pollNoteState{OptionN: 3, Multiple: true}
	if !addPollChoice(st, 0) || !addPollChoice(st, 2) {
		t.Fatal("first unique choices should be added")
	}
	if addPollChoice(st, 0) {
		t.Fatal("duplicate must be rejected")
	}
	if addPollChoice(st, 3) || addPollChoice(st, -1) {
		t.Fatal("out of range must be rejected")
	}
	if len(st.Selected) != 2 || st.Selected[0] != 0 || st.Selected[1] != 2 {
		t.Fatalf("selected = %v", st.Selected)
	}
}

func TestApplyPollVoteSingleAndMulti(t *testing.T) {
	single := &pollNoteState{QuestionID: "q", Multiple: false, OptionN: 3}
	if applyPollVote(single, 1) != pollVoteResolveNow {
		t.Fatal("single first vote should resolve now")
	}
	if len(single.Selected) != 1 || single.Selected[0] != 1 {
		t.Fatalf("selected = %v", single.Selected)
	}
	if applyPollVote(&pollNoteState{Multiple: false, OptionN: 2}, 5) != pollVoteIgnore {
		t.Fatal("single out-of-range should ignore")
	}

	multi := &pollNoteState{QuestionID: "q", Multiple: true, OptionN: 3}
	if applyPollVote(multi, 0) != pollVoteDebounce {
		t.Fatal("first multi vote should debounce")
	}
	if applyPollVote(multi, 0) != pollVoteIgnore {
		t.Fatal("duplicate multi vote should ignore")
	}
	if applyPollVote(multi, 1) != pollVoteDebounce {
		t.Fatal("second multi vote still debounce")
	}
	if applyPollVote(multi, 2) != pollVoteResolveNow {
		t.Fatal("all options selected should resolve immediately")
	}
	if len(multi.Selected) != 3 {
		t.Fatalf("selected = %v", multi.Selected)
	}
}

func TestHandlePollVotedSingleResolveFrom(t *testing.T) {
	qid := "uc-mk-single-" + t.Name()
	q := interaction.Question{
		ID: qid, BotID: "b", ChatID: "user-1",
		Question: "q",
		Options:  []interaction.Option{{Label: "a"}, {Label: "b"}},
		Mode:     interaction.ModeSingle, TimeoutSecs: 30,
	}
	if _, err := interaction.Default().RegisterQuestion(q); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { interaction.Default().AbortPending(qid) })

	c := &MisskeyChannel{name: "mk", pollNotes: map[string]*pollNoteState{
		"note-1": {QuestionID: qid, Multiple: false, OptionN: 2},
	}}
	body, _ := json.Marshal(map[string]any{"noteId": "note-1", "choice": 1, "userId": "user-1"})
	c.handlePollVoted(context.Background(), body)

	snap, err := interaction.Default().Lookup(qid)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Status != interaction.StatusAnswered {
		t.Fatalf("status = %s", snap.Status)
	}
	if _, ok := c.pollNotes["note-1"]; ok {
		t.Fatal("single vote must drop mapping")
	}
}

func TestHandlePollVotedWrongVoterIgnored(t *testing.T) {
	qid := "uc-mk-wrong-" + t.Name()
	q := interaction.Question{
		ID: qid, BotID: "b", ChatID: "user-1",
		Question: "q",
		Options:  []interaction.Option{{Label: "a"}, {Label: "b"}},
		Mode:     interaction.ModeSingle, TimeoutSecs: 30,
	}
	if _, err := interaction.Default().RegisterQuestion(q); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { interaction.Default().AbortPending(qid) })

	c := &MisskeyChannel{name: "mk", pollNotes: map[string]*pollNoteState{
		"note-2": {QuestionID: qid, Multiple: false, OptionN: 2},
	}}
	body, _ := json.Marshal(map[string]any{"noteId": "note-2", "choice": 0, "userId": "stranger"})
	c.handlePollVoted(context.Background(), body)

	snap, err := interaction.Default().Lookup(qid)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Status != interaction.StatusPending {
		t.Fatalf("wrong voter must not resolve, status = %s", snap.Status)
	}
	if c.pollNotes["note-2"] == nil {
		t.Fatal("wrong voter must not drop mapping so the owner can still vote")
	}
}

func TestHandlePollVotedMultiKeepsMapping(t *testing.T) {
	qid := "uc-mk-multi-" + t.Name()
	q := interaction.Question{
		ID: qid, BotID: "b", ChatID: "user-1",
		Question: "q",
		Options:  []interaction.Option{{Label: "a"}, {Label: "b"}, {Label: "c"}},
		Mode:     interaction.ModeMulti, TimeoutSecs: 30,
	}
	if _, err := interaction.Default().RegisterQuestion(q); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { interaction.Default().AbortPending(qid) })

	c := &MisskeyChannel{name: "mk", pollNotes: map[string]*pollNoteState{
		"note-3": {QuestionID: qid, Multiple: true, OptionN: 3},
	}}
	body, _ := json.Marshal(map[string]any{"noteId": "note-3", "choice": 1, "userId": "user-1"})
	c.handlePollVoted(context.Background(), body)

	st := c.pollNotes["note-3"]
	if st == nil {
		t.Fatal("multi first vote must keep mapping")
	}
	if len(st.Selected) != 1 || st.Selected[0] != 1 {
		t.Fatalf("selected = %v", st.Selected)
	}
	if st.timer != nil {
		st.timer.Stop()
	}
	snap, _ := interaction.Default().Lookup(qid)
	if snap.Status != interaction.StatusPending {
		t.Fatalf("should still be pending, status = %s", snap.Status)
	}
}

func TestHandlePollVotedMultiAllSelectedResolves(t *testing.T) {
	qid := "uc-mk-all-" + t.Name()
	q := interaction.Question{
		ID: qid, BotID: "b", ChatID: "user-1",
		Question: "q",
		Options:  []interaction.Option{{Label: "a"}, {Label: "b"}},
		Mode:     interaction.ModeMulti, TimeoutSecs: 30,
	}
	if _, err := interaction.Default().RegisterQuestion(q); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { interaction.Default().AbortPending(qid) })

	c := &MisskeyChannel{name: "mk", pollNotes: map[string]*pollNoteState{
		"note-4": {QuestionID: qid, Multiple: true, OptionN: 2},
	}}
	b0, _ := json.Marshal(map[string]any{"noteId": "note-4", "choice": 0, "userId": "user-1"})
	b1, _ := json.Marshal(map[string]any{"noteId": "note-4", "choice": 1, "userId": "user-1"})
	c.handlePollVoted(context.Background(), b0)
	c.handlePollVoted(context.Background(), b1)

	snap, err := interaction.Default().Lookup(qid)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Status != interaction.StatusAnswered {
		t.Fatalf("status = %s", snap.Status)
	}
	if _, ok := c.pollNotes["note-4"]; ok {
		t.Fatal("mapping should be dropped after full selection")
	}
}

func TestHandlePollVotedResolveFailureDropsMapping(t *testing.T) {
	qid := "uc-mk-fail-" + t.Name()
	q := interaction.Question{
		ID: qid, BotID: "b", ChatID: "user-1",
		Question: "q",
		Options:  []interaction.Option{{Label: "a"}, {Label: "b"}},
		Mode:     interaction.ModeSingle, TimeoutSecs: 30,
	}
	if _, err := interaction.Default().RegisterQuestion(q); err != nil {
		t.Fatal(err)
	}
	if err := interaction.Default().Resolve(qid, interaction.Answer{Selected: []int{0}, Via: interaction.ViaMisskey}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { interaction.Default().CleanupFinal(qid) })

	c := &MisskeyChannel{name: "mk", pollNotes: map[string]*pollNoteState{
		"note-5": {QuestionID: qid, Multiple: false, OptionN: 2},
	}}
	body, _ := json.Marshal(map[string]any{"noteId": "note-5", "choice": 1, "userId": "user-1"})
	c.handlePollVoted(context.Background(), body)
	if _, ok := c.pollNotes["note-5"]; ok {
		t.Fatal("mapping should be dropped even if Resolve fails")
	}
}

func TestExpirePollNoteDeletesMapping(t *testing.T) {
	c := &MisskeyChannel{name: "mk", pollNotes: map[string]*pollNoteState{
		"note-x": {QuestionID: "q-x", OptionN: 2},
	}}
	c.pollNotesMu.Lock()
	st := c.pollNotes["note-x"]
	c.armPollExpiryLocked(st, "note-x", 600)
	if st.expire == nil {
		c.pollNotesMu.Unlock()
		t.Fatal("CreatePollNote path should store expire timer")
	}
	c.pollNotesMu.Unlock()

	c.expirePollNote("note-x", "q-x")
	if _, ok := c.pollNotes["note-x"]; ok {
		t.Fatal("expirePollNote should delete mapping")
	}
}

func TestExpirePollNoteDefaultTimeoutArmsTimer(t *testing.T) {
	c := &MisskeyChannel{name: "mk", pollNotes: map[string]*pollNoteState{
		"note-z": {QuestionID: "q-z", OptionN: 2},
	}}
	c.pollNotesMu.Lock()
	st := c.pollNotes["note-z"]
	c.armPollExpiryLocked(st, "note-z", 0)
	if st.expire == nil {
		c.pollNotesMu.Unlock()
		t.Fatal("timeoutSecs<=0 should still arm expire timer")
	}
	st.expire.Stop()
	st.expire = nil
	c.pollNotesMu.Unlock()
}

func TestExpirePollNoteIgnoresMismatchedQuestion(t *testing.T) {
	c := &MisskeyChannel{name: "mk", pollNotes: map[string]*pollNoteState{
		"note-y": {QuestionID: "q-keep", OptionN: 2},
	}}
	c.expirePollNote("note-y", "q-other")
	if c.pollNotes["note-y"] == nil {
		t.Fatal("mismatched questionID must not delete mapping")
	}
}

func TestExpirePollNoteStopsDebounceTimer(t *testing.T) {
	c := &MisskeyChannel{name: "mk", pollNotes: map[string]*pollNoteState{
		"note-d": {QuestionID: "q-d", Multiple: true, OptionN: 3},
	}}
	st := c.pollNotes["note-d"]
	st.timer = time.AfterFunc(time.Hour, func() {})
	c.pollNotesMu.Lock()
	c.armPollExpiryLocked(st, "note-d", 600)
	c.pollNotesMu.Unlock()
	c.expirePollNote("note-d", "q-d")
	if _, ok := c.pollNotes["note-d"]; ok {
		t.Fatal("mapping should be gone")
	}
	if st.timer != nil || st.expire != nil {
		t.Fatal("both debounce and expire timers should be stopped")
	}
}
