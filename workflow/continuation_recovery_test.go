package workflow

import (
	"context"
	"sync"
	"testing"
	"time"
)

func finishedWorkflow(id string, status WorkflowStatus) *Workflow {
	wf := NewWorkflow(id, "req", []*DAGNode{{ID: "n1", Task: "t"}})
	wf.Status = status
	wf.Nodes[0].Status = NodeCompleted
	fin := time.Now().Add(-time.Hour)
	wf.FinishedAt = &fin
	wf.BotID, wf.SessionID = "bot-a", "tg:1"
	return wf
}

type continuationRecorder struct {
	mu  sync.Mutex
	ids []string
}

func (r *continuationRecorder) fn(wf *Workflow) {
	r.mu.Lock()
	r.ids = append(r.ids, wf.ID)
	r.mu.Unlock()
}

func (r *continuationRecorder) got() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.ids...)
}

func TestContinuationRecoverySkipReason(t *testing.T) {
	now := time.Now()
	recent := now.Add(-10 * time.Minute)
	old := now.Add(-13 * time.Hour)
	mk := func(mut func(wf *Workflow)) *Workflow {
		wf := finishedWorkflow("wf", WorkflowCompleted)
		wf.NeedsContinuation = true
		wf.ContinuationInjectedAt = &recent
		mut(wf)
		return wf
	}
	cases := []struct {
		name string
		wf   *Workflow
		want string
	}{
		{"interrupted continuation", mk(func(*Workflow) {}), ""},
		{"interrupted continuation of failed wf", mk(func(wf *Workflow) { wf.Status = WorkflowFailed }), ""},
		// The prod case: flag left over by old code after a finished continuation.
		{"legacy flag", mk(func(wf *Workflow) { wf.ContinuationInjectedAt = nil }), "legacy_flag"},
		{"already recovered once", mk(func(wf *Workflow) { wf.ContinuationRecoveries = 1 }), "already_recovered"},
		{"too old", mk(func(wf *Workflow) { wf.ContinuationInjectedAt = &old }), "too_old"},
		{"not terminal", mk(func(wf *Workflow) { wf.Status = WorkflowRunning }), "not_terminal"},
	}
	for _, c := range cases {
		if got := continuationRecoverySkipReason(c.wf, now); got != c.want {
			t.Errorf("%s: reason = %q, want %q", c.name, got, c.want)
		}
	}
}

// Reproduces 2026-09-28: two workflows whose continuations had already run
// (one completed, one failed) were re-injected on restart. Only a genuinely
// interrupted continuation may be re-injected, and only once.
func TestRecoverContinuations_OnlyInterruptedAndOnlyOnce(t *testing.T) {
	repo := NewRepository(nil, noopLogger())
	recent := time.Now().Add(-5 * time.Minute)

	legacyDone := finishedWorkflow("wf-legacy-completed", WorkflowCompleted)
	legacyDone.NeedsContinuation = true // old code never cleared it
	legacyFailed := finishedWorkflow("wf-legacy-failed", WorkflowFailed)
	legacyFailed.NeedsContinuation = true
	interrupted := finishedWorkflow("wf-interrupted", WorkflowCompleted)
	interrupted.NeedsContinuation = true
	interrupted.ContinuationInjectedAt = &recent
	confirmed := finishedWorkflow("wf-confirmed", WorkflowCompleted)
	confirmed.ContinuationInjectedAt = &recent // continuation ran, flag cleared
	for _, wf := range []*Workflow{legacyDone, legacyFailed, interrupted, confirmed} {
		if err := repo.Save(wf); err != nil {
			t.Fatal(err)
		}
	}

	rec := &continuationRecorder{}
	m := NewManager(repo, nil, nil, nil, EngineConfig{MaxParallel: 1}, noopLogger(), nil)
	m.SetOnWorkflowCompleted(rec.fn)
	res, err := m.Recover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.got(); len(got) != 1 || got[0] != "wf-interrupted" || res.Continued != 1 {
		t.Fatalf("re-injected %v (continued=%d), want only wf-interrupted", got, res.Continued)
	}
	for _, id := range []string{"wf-legacy-completed", "wf-legacy-failed", "wf-interrupted"} {
		wf, _ := repo.Get(id)
		if wf.NeedsContinuation {
			t.Errorf("%s: flag must be cleared after recovery", id)
		}
	}
	if wf, _ := repo.Get("wf-interrupted"); wf.ContinuationRecoveries != 1 {
		t.Fatalf("recoveries = %d", wf.ContinuationRecoveries)
	}

	// Simulate the recovered continuation being interrupted again (the
	// callback re-flags it) and another restart: it must not be injected again.
	m.SetNeedsContinuation("wf-interrupted", true)
	rec2 := &continuationRecorder{}
	m2 := NewManager(repo, nil, nil, nil, EngineConfig{MaxParallel: 1}, noopLogger(), nil)
	m2.SetOnWorkflowCompleted(rec2.fn)
	if _, err := m2.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := rec2.got(); len(got) != 0 {
		t.Fatalf("second restart re-injected %v", got)
	}
}

// A continuation turn that ran to the end confirms the workflow, so the next
// restart does not inject it again.
func TestConfirmContinuationPreventsReinjection(t *testing.T) {
	repo := NewRepository(nil, noopLogger())
	wf := finishedWorkflow("wf-1", WorkflowCompleted)
	if err := repo.Save(wf); err != nil {
		t.Fatal(err)
	}
	m := NewManager(repo, nil, nil, nil, EngineConfig{MaxParallel: 1}, noopLogger(), nil)
	m.SetNeedsContinuation("wf-1", true)
	got, _ := repo.Get("wf-1")
	if !got.NeedsContinuation || got.ContinuationInjectedAt == nil {
		t.Fatalf("SetNeedsContinuation(true) must persist flag and injection time: %+v", got)
	}
	m.ConfirmContinuation("wf-1")
	if got, _ := repo.Get("wf-1"); got.NeedsContinuation {
		t.Fatal("ConfirmContinuation must clear the persisted flag")
	}
	// The in-memory flag used by the web panel poll is left alone.
	if !m.consumeNeedsContinuation("wf-1") {
		t.Fatal("in-memory flag for the web panel must survive confirmation")
	}

	rec := &continuationRecorder{}
	m2 := NewManager(repo, nil, nil, nil, EngineConfig{MaxParallel: 1}, noopLogger(), nil)
	m2.SetOnWorkflowCompleted(rec.fn)
	if _, err := m2.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(rec.got()) != 0 {
		t.Fatalf("confirmed workflow re-injected: %v", rec.got())
	}
}

// The continuation bookkeeping survives a DB round trip (a fresh repository
// instance, as after a restart).
func TestContinuationFieldsPersist(t *testing.T) {
	db := newSharedDB(t)
	writer := NewRepository(db, noopLogger())
	wf := finishedWorkflow("wf-o", WorkflowCompleted)
	now := time.Now().Truncate(time.Second)
	wf.NeedsContinuation = true
	wf.ContinuationInjectedAt = &now
	wf.ContinuationRecoveries = 1
	if err := writer.Save(wf); err != nil {
		t.Fatal(err)
	}
	reader := NewRepository(db, noopLogger())
	got, err := reader.Get("wf-o")
	if err != nil {
		t.Fatal(err)
	}
	if got.ContinuationRecoveries != 1 || got.ContinuationInjectedAt == nil || !got.ContinuationInjectedAt.Equal(now) {
		t.Fatalf("fields not persisted: %+v", got)
	}
	flagged, err := reader.FindNeedingContinuation()
	if err != nil || len(flagged) != 1 {
		t.Fatalf("FindNeedingContinuation = %v, %v", flagged, err)
	}
}
