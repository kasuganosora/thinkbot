package cron

import (
	"context"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kasuganosora/thinkbot/llm"
	"github.com/kasuganosora/thinkbot/util/log"
	"github.com/kasuganosora/thinkbot/util/traceid"
)

func TestMain(m *testing.M) {
	_ = log.Init()
	os.Exit(m.Run())
}

func TestParseCronExpr_Basic(t *testing.T) {
	tests := []struct {
		expr  string
		valid bool
	}{
		{"0 9 * * *", true},
		{"*/5 * * * *", true},
		{"0 9 * * 1-5", true},
		{"0,30 * * * *", true},
		{"0 0 1 1 *", true},
		{"60 9 * * *", false},  // minute out of range
		{"0 24 * * *", false},  // hour out of range
		{"0 9 * *", false},     // too few fields
		{"0 9 * * * *", false}, // too many fields
	}
	for _, tt := range tests {
		_, err := parseCronExpr(tt.expr)
		if tt.valid && err != nil {
			t.Errorf("expected %q to be valid, got error: %v", tt.expr, err)
		}
		if !tt.valid && err == nil {
			t.Errorf("expected %q to be invalid", tt.expr)
		}
	}
}

func TestCronExpr_Next(t *testing.T) {
	loc, _ := time.LoadLocation("Asia/Shanghai")
	ce, err := parseCronExpr("0 9 * * *")
	if err != nil {
		t.Fatal(err)
	}
	from := time.Date(2026, 6, 20, 8, 0, 0, 0, loc)
	next := ce.Next(from, loc)
	expected := time.Date(2026, 6, 20, 9, 0, 0, 0, loc)
	if !next.Equal(expected) {
		t.Errorf("expected %v, got %v", expected, next)
	}
}

func TestCronExpr_NextWeekday(t *testing.T) {
	loc, _ := time.LoadLocation("UTC")
	// Every Monday at 10:00
	ce, _ := parseCronExpr("0 10 * * 1")
	// 2026-06-20 is Saturday
	from := time.Date(2026, 6, 20, 12, 0, 0, 0, loc)
	next := ce.Next(from, loc)
	// Next Monday is 2026-06-22
	if next.Weekday() != time.Monday {
		t.Errorf("expected Monday, got %v", next.Weekday())
	}
	if next.Hour() != 10 {
		t.Errorf("expected hour 10, got %d", next.Hour())
	}
}

func TestParseSchedule_Cron(t *testing.T) {
	loc := time.UTC
	kind, _, cronE, nextRun, err := parseSchedule("0 9 * * 1-5", loc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if kind != ScheduleCron {
		t.Errorf("expected ScheduleCron, got %v", kind)
	}
	if cronE == nil {
		t.Error("expected non-nil cronExpr")
	}
	if nextRun == nil {
		t.Error("expected non-nil nextRun")
	}
}

func TestParseSchedule_Interval(t *testing.T) {
	loc := time.UTC
	kind, _, _, nextRun, err := parseSchedule("every 30m", loc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if kind != ScheduleInterval {
		t.Errorf("expected ScheduleInterval, got %v", kind)
	}
	if nextRun == nil {
		t.Error("expected non-nil nextRun")
	}
}

func TestParseSchedule_OnceDelay(t *testing.T) {
	loc := time.UTC
	kind, _, _, nextRun, err := parseSchedule("2h", loc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if kind != ScheduleOnce {
		t.Errorf("expected ScheduleOnce, got %v", kind)
	}
	if nextRun == nil {
		t.Fatal("expected non-nil nextRun")
	}
	// Should be ~2 hours from now
	if nextRun.Before(time.Now().Add(1 * time.Hour)) {
		t.Error("expected nextRun to be ~2h in the future")
	}
}

func TestParseSchedule_OnceISO(t *testing.T) {
	loc, _ := time.LoadLocation("Asia/Shanghai")
	kind, _, _, nextRun, err := parseSchedule("2026-12-25T09:00", loc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if kind != ScheduleOnce {
		t.Errorf("expected ScheduleOnce, got %v", kind)
	}
	if nextRun == nil {
		t.Fatal("expected non-nil nextRun")
	}
	// Should be 2026-12-25 09:00 Shanghai time
	expected := time.Date(2026, 12, 25, 9, 0, 0, 0, loc)
	if !nextRun.Equal(expected) {
		t.Errorf("expected %v, got %v", expected, nextRun)
	}
}

func TestParseSchedule_Invalid(t *testing.T) {
	_, _, _, _, err := parseSchedule("garbage", time.UTC)
	if err == nil {
		t.Error("expected error for invalid schedule")
	}
}

func TestParseDuration(t *testing.T) {
	tests := []struct {
		input string
		valid bool
	}{
		{"30s", true},
		{"5m", true},
		{"2h", true},
		{"1d", true},
		{"3h30m", true},
		{"0s", false},
		{"abc", false},
		{"", false},
	}
	for _, tt := range tests {
		_, err := parseDuration(tt.input)
		if tt.valid && err != nil {
			t.Errorf("expected %q to be valid: %v", tt.input, err)
		}
		if !tt.valid && err == nil {
			t.Errorf("expected %q to be invalid", tt.input)
		}
	}
}

// ============================================================================
// Store tests
// ============================================================================

func TestStore_CRUD(t *testing.T) {
	store := NewStore(t.TempDir() + "/cron.json")

	// Create
	job := &Job{
		ID:        "test-001",
		Name:      "Test Job",
		Prompt:    "Hello",
		Schedule:  "every 1m",
		Enabled:   true,
		State:     StateActive,
		CreatedAt: time.Now(),
	}
	if err := store.Save(job); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	// Get
	got, ok := store.Get("test-001")
	if !ok {
		t.Fatal("expected job to exist")
	}
	if got.Name != "Test Job" {
		t.Errorf("expected name 'Test Job', got %q", got.Name)
	}

	// List
	list := store.List()
	if len(list) != 1 {
		t.Fatalf("expected 1 job, got %d", len(list))
	}

	// Delete
	if err := store.Delete("test-001"); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}
	if _, ok := store.Get("test-001"); ok {
		t.Error("expected job to be deleted")
	}
}

func TestStore_PersistAndReload(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/cron.json"

	// Create and save
	store1 := NewStore(path)
	job := &Job{
		ID:        "persist-001",
		Name:      "Persist Test",
		Prompt:    "test",
		Schedule:  "every 5m",
		Enabled:   true,
		State:     StateActive,
		CreatedAt: time.Now(),
	}
	_ = store1.Save(job)

	// Create new store from same file
	store2 := NewStore(path)
	got, ok := store2.Get("persist-001")
	if !ok {
		t.Fatal("expected job to survive reload")
	}
	if got.Name != "Persist Test" {
		t.Errorf("expected 'Persist Test', got %q", got.Name)
	}
}

// ============================================================================
// Scheduler tests
// ============================================================================

func TestScheduler_ExecutesIntervalJob(t *testing.T) {
	store := NewStore(t.TempDir() + "/cron.json")
	var execCount atomic.Int64

	executor := ExecutorFunc(func(ctx context.Context, job *Job) (*ExecuteResult, error) {
		execCount.Add(1)
		return &ExecuteResult{Output: "done"}, nil
	})

	loc := time.UTC
	sched := NewScheduler(store, executor, SchedulerConfig{
		TickInterval:  100 * time.Millisecond,
		MaxConcurrent: 1,
		JobTimeout:    10 * time.Second,
		Location:      loc,
	})

	mgr := NewManager(store, loc)
	job, err := mgr.CreateJob(CreateJobRequest{
		Name:     "Interval Test",
		Prompt:   "test prompt",
		Schedule: "every 1m",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Set NextRunAt to past to trigger immediately
	now := time.Now().UTC().Add(-1 * time.Minute)
	job.NextRunAt = &now
	_ = store.Save(job)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sched.Start(ctx)

	// Wait for execution
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if execCount.Load() > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	sched.Stop()

	if execCount.Load() == 0 {
		t.Error("expected at least 1 execution")
	}

	// Verify state updated
	got, _ := store.Get(job.ID)
	if got.LastResult != "done" {
		t.Errorf("expected last_result 'done', got %q", got.LastResult)
	}
	if got.RunCount != 1 {
		t.Errorf("expected run_count 1, got %d", got.RunCount)
	}
}

func TestScheduler_OneShotDone(t *testing.T) {
	store := NewStore(t.TempDir() + "/cron.json")

	executor := ExecutorFunc(func(ctx context.Context, job *Job) (*ExecuteResult, error) {
		return &ExecuteResult{Output: "ok"}, nil
	})

	loc := time.UTC
	sched := NewScheduler(store, executor, SchedulerConfig{
		TickInterval:  100 * time.Millisecond,
		MaxConcurrent: 1,
		JobTimeout:    10 * time.Second,
		Location:      loc,
	})

	mgr := NewManager(store, loc)
	job, _ := mgr.CreateJob(CreateJobRequest{
		Name:     "One Shot",
		Prompt:   "test",
		Schedule: "1h",
	})

	// Set NextRunAt to past
	now := time.Now().UTC().Add(-10 * time.Second)
	job.NextRunAt = &now
	_ = store.Save(job)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sched.Start(ctx)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		got, _ := store.Get(job.ID)
		if got.State == StateDone {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	sched.Stop()

	got, _ := store.Get(job.ID)
	if got.State != StateDone {
		t.Errorf("expected StateDone, got %v", got.State)
	}
	if got.NextRunAt != nil {
		t.Error("expected NextRunAt to be nil for done job")
	}
}

// ============================================================================
// Manager tests
// ============================================================================

func TestManager_CreateJob(t *testing.T) {
	store := NewStore(t.TempDir() + "/cron.json")
	loc, _ := time.LoadLocation("Asia/Shanghai")
	mgr := NewManager(store, loc)

	job, err := mgr.CreateJob(CreateJobRequest{
		Name:     "Daily Standup",
		Prompt:   "Summarize today's tasks",
		Schedule: "0 9 * * 1-5",
		Tags:     []string{"work"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if job.ID == "" {
		t.Error("expected non-empty ID")
	}
	if job.ScheduleKind != ScheduleCron {
		t.Errorf("expected ScheduleCron, got %v", job.ScheduleKind)
	}
	if job.NextRunAt == nil {
		t.Error("expected NextRunAt to be set")
	}
	if job.State != StateActive {
		t.Errorf("expected StateActive, got %v", job.State)
	}

	// Verify it's stored
	got, ok := mgr.GetJob(job.ID)
	if !ok {
		t.Fatal("expected job to be retrievable")
	}
	if got.Name != "Daily Standup" {
		t.Errorf("unexpected name: %q", got.Name)
	}
}

func TestManager_CreateJobValidation(t *testing.T) {
	store := NewStore(t.TempDir() + "/cron.json")
	mgr := NewManager(store, time.UTC)

	// Missing name
	_, err := mgr.CreateJob(CreateJobRequest{Prompt: "test", Schedule: "every 1m"})
	if err == nil {
		t.Error("expected error for missing name")
	}

	// Missing prompt
	_, err = mgr.CreateJob(CreateJobRequest{Name: "test", Schedule: "every 1m"})
	if err == nil {
		t.Error("expected error for missing prompt")
	}

	// Invalid schedule
	_, err = mgr.CreateJob(CreateJobRequest{Name: "test", Prompt: "test", Schedule: "garbage"})
	if err == nil {
		t.Error("expected error for invalid schedule")
	}
}

func TestManager_PauseResume(t *testing.T) {
	store := NewStore(t.TempDir() + "/cron.json")
	mgr := NewManager(store, time.UTC)

	job, _ := mgr.CreateJob(CreateJobRequest{
		Name:     "Test",
		Prompt:   "test",
		Schedule: "every 5m",
	})

	if err := mgr.PauseJob(job.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := mgr.GetJob(job.ID)
	if got.State != StatePaused {
		t.Errorf("expected StatePaused, got %v", got.State)
	}

	if err := mgr.ResumeJob(job.ID); err != nil {
		t.Fatal(err)
	}
	got, _ = mgr.GetJob(job.ID)
	if got.State != StateActive {
		t.Errorf("expected StateActive after resume, got %v", got.State)
	}
}

func TestManager_TriggerJob(t *testing.T) {
	store := NewStore(t.TempDir() + "/cron.json")
	mgr := NewManager(store, time.UTC)

	job, _ := mgr.CreateJob(CreateJobRequest{
		Name:     "Test",
		Prompt:   "test",
		Schedule: "1d",
	})
	// One-shot job after creation, NextRunAt is ~1 day away
	if job.NextRunAt == nil {
		t.Fatal("expected NextRunAt to be set")
	}

	// Trigger sets NextRunAt to now
	if err := mgr.TriggerJob(job.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := mgr.GetJob(job.ID)
	if got.NextRunAt == nil {
		t.Fatal("expected NextRunAt to be set after trigger")
	}
	// Should be approximately now
	diff := time.Since(*got.NextRunAt)
	if diff > 5*time.Second || diff < -5*time.Second {
		t.Errorf("expected NextRunAt to be ~now, diff=%v", diff)
	}
}

func TestManager_DeleteJob(t *testing.T) {
	store := NewStore(t.TempDir() + "/cron.json")
	mgr := NewManager(store, time.UTC)

	job, _ := mgr.CreateJob(CreateJobRequest{
		Name:     "ToDelete",
		Prompt:   "test",
		Schedule: "every 1m",
	})

	if err := mgr.DeleteJob(job.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := mgr.GetJob(job.ID); ok {
		t.Error("expected job to be deleted")
	}
}

// ============================================================================
// Timezone test
// ============================================================================

func TestSchedule_UsesBotTimezone(t *testing.T) {
	// Cron "0 9 * * *" in UTC vs Asia/Shanghai (UTC+8)
	// From 2026-06-20 00:00 UTC:
	//   In UTC: next 09:00 is 2026-06-20 09:00 UTC
	//   In Shanghai: next 09:00 CST is 2026-06-20 09:00+08:00 = 2026-06-20 01:00 UTC
	utcLoc, _ := time.LoadLocation("UTC")
	shLoc, _ := time.LoadLocation("Asia/Shanghai")

	from := time.Date(2026, 6, 20, 0, 0, 0, 0, utcLoc)

	ce, _ := parseCronExpr("0 9 * * *")

	utcNext := ce.Next(from, utcLoc)
	shNext := ce.Next(from, shLoc)

	// UTC: 09:00 UTC
	utcExpected := time.Date(2026, 6, 20, 9, 0, 0, 0, utcLoc)
	if !utcNext.Equal(utcExpected) {
		t.Errorf("UTC: expected %v, got %v", utcExpected, utcNext)
	}

	// Shanghai: 09:00 CST = 01:00 UTC
	shExpectedUTC := time.Date(2026, 6, 20, 1, 0, 0, 0, utcLoc)
	if !shNext.In(utcLoc).Equal(shExpectedUTC) {
		t.Errorf("Shanghai: expected %v UTC, got %v UTC", shExpectedUTC, shNext.In(utcLoc))
	}
}

// ============================================================================
// TraceID tests
// ============================================================================

// TestScheduler_TraceIDInjected verifies that each job execution gets a unique
// trace_id injected into the context, accessible via traceid.FromContext.
func TestScheduler_TraceIDInjected(t *testing.T) {
	store := NewStore(t.TempDir() + "/cron.json")

	var capturedTraceIDs []string
	executor := ExecutorFunc(func(ctx context.Context, job *Job) (*ExecuteResult, error) {
		tid := traceid.FromContext(ctx)
		capturedTraceIDs = append(capturedTraceIDs, tid)
		return &ExecuteResult{Output: "ok"}, nil
	})

	loc := time.UTC
	sched := NewScheduler(store, executor, SchedulerConfig{
		TickInterval:  50 * time.Millisecond,
		MaxConcurrent: 1,
		JobTimeout:    5 * time.Second,
		Location:      loc,
	})

	mgr := NewManager(store, loc)
	job, _ := mgr.CreateJob(CreateJobRequest{
		Name:     "TraceID Test",
		Prompt:   "test",
		Schedule: "every 1m",
	})
	now := time.Now().UTC().Add(-1 * time.Minute)
	job.NextRunAt = &now
	_ = store.Save(job)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sched.Start(ctx)

	// Wait for execution
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(capturedTraceIDs) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	sched.Stop()

	if len(capturedTraceIDs) == 0 {
		t.Fatal("expected at least one execution")
	}
	// Verify trace_id is valid
	tid := capturedTraceIDs[0]
	if !traceid.IsValid(tid) {
		t.Errorf("expected valid trace_id, got %q", tid)
	}
}

// ============================================================================
// Token usage recording tests
// ============================================================================

// mockRecorder is a test llm.UsageRecorder.
type mockRecorder struct {
	metrics []llm.UsageMetric
}

func (m *mockRecorder) RecordUsage(_ context.Context, metric llm.UsageMetric) {
	m.metrics = append(m.metrics, metric)
}

func TestScheduler_RecordsTokenUsage(t *testing.T) {
	store := NewStore(t.TempDir() + "/cron.json")

	executor := ExecutorFunc(func(ctx context.Context, job *Job) (*ExecuteResult, error) {
		return &ExecuteResult{
			Output:    "summary of tasks",
			Usage:     llm.Usage{InputTokens: 500, OutputTokens: 200, TotalTokens: 700},
			ToolCalls: 2,
			Steps:     1,
		}, nil
	})

	rec := &mockRecorder{}
	loc := time.UTC
	sched := NewScheduler(store, executor, SchedulerConfig{
		TickInterval:  50 * time.Millisecond,
		MaxConcurrent: 1,
		JobTimeout:    5 * time.Second,
		Location:      loc,
		BotID:         "bot-001",
	})
	sched.WithUsageRecorder(rec)

	mgr := NewManager(store, loc)
	job, _ := mgr.CreateJob(CreateJobRequest{
		Name:     "Usage Test",
		Prompt:   "test",
		Schedule: "every 1m",
		Feature:  "cron_daily",
	})
	now := time.Now().UTC().Add(-1 * time.Minute)
	job.NextRunAt = &now
	_ = store.Save(job)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sched.Start(ctx)

	// Wait for execution
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(rec.metrics) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	sched.Stop()

	if len(rec.metrics) != 1 {
		t.Fatalf("expected 1 usage metric, got %d", len(rec.metrics))
	}

	m := rec.metrics[0]
	if m.BotID != "bot-001" {
		t.Errorf("expected BotID 'bot-001', got %q", m.BotID)
	}
	if m.Feature != "cron_daily" {
		t.Errorf("expected Feature 'cron_daily', got %q", m.Feature)
	}
	if m.Usage.InputTokens != 500 {
		t.Errorf("expected InputTokens 500, got %d", m.Usage.InputTokens)
	}
	if m.Usage.OutputTokens != 200 {
		t.Errorf("expected OutputTokens 200, got %d", m.Usage.OutputTokens)
	}
	if m.Usage.TotalTokens != 700 {
		t.Errorf("expected TotalTokens 700, got %d", m.Usage.TotalTokens)
	}
	if m.ToolCalls != 2 {
		t.Errorf("expected ToolCalls 2, got %d", m.ToolCalls)
	}
}

func TestScheduler_NoUsageWhenZeroTokens(t *testing.T) {
	store := NewStore(t.TempDir() + "/cron.json")

	executor := ExecutorFunc(func(ctx context.Context, job *Job) (*ExecuteResult, error) {
		return &ExecuteResult{Output: "no llm call"}, nil // zero Usage
	})

	rec := &mockRecorder{}
	loc := time.UTC
	sched := NewScheduler(store, executor, SchedulerConfig{
		TickInterval:  50 * time.Millisecond,
		MaxConcurrent: 1,
		JobTimeout:    5 * time.Second,
		Location:      loc,
	})
	sched.WithUsageRecorder(rec)

	mgr := NewManager(store, loc)
	job, _ := mgr.CreateJob(CreateJobRequest{
		Name:     "No Usage",
		Prompt:   "test",
		Schedule: "every 1m",
	})
	now := time.Now().UTC().Add(-1 * time.Minute)
	job.NextRunAt = &now
	_ = store.Save(job)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sched.Start(ctx)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		got, _ := store.Get(job.ID)
		if got.RunCount > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	sched.Stop()

	if len(rec.metrics) != 0 {
		t.Errorf("expected 0 metrics for zero-usage execution, got %d", len(rec.metrics))
	}
}

func TestScheduler_DefaultFeatureCron(t *testing.T) {
	store := NewStore(t.TempDir() + "/cron.json")

	executor := ExecutorFunc(func(ctx context.Context, job *Job) (*ExecuteResult, error) {
		return &ExecuteResult{
			Output: "ok",
			Usage:  llm.Usage{TotalTokens: 100},
		}, nil
	})

	rec := &mockRecorder{}
	loc := time.UTC
	sched := NewScheduler(store, executor, SchedulerConfig{
		TickInterval:  50 * time.Millisecond,
		MaxConcurrent: 1,
		JobTimeout:    5 * time.Second,
		Location:      loc,
		BotID:         "bot-x",
	})
	sched.WithUsageRecorder(rec)

	mgr := NewManager(store, loc)
	// No Feature specified — should default to "cron"
	job, _ := mgr.CreateJob(CreateJobRequest{
		Name:     "Default Feature",
		Prompt:   "test",
		Schedule: "every 1m",
	})
	now := time.Now().UTC().Add(-1 * time.Minute)
	job.NextRunAt = &now
	_ = store.Save(job)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sched.Start(ctx)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(rec.metrics) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	sched.Stop()

	if len(rec.metrics) != 1 {
		t.Fatalf("expected 1 metric, got %d", len(rec.metrics))
	}
	if rec.metrics[0].Feature != "cron" {
		t.Errorf("expected default Feature 'cron', got %q", rec.metrics[0].Feature)
	}
}
