package session

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestSessionRunner_Serial(t *testing.T) {
	r := NewSessionRunner(RunnerConfig{})

	var order []int
	var mu sync.Mutex

	for i := 0; i < 5; i++ {
		idx := i
		err := r.Run(context.Background(), func(ctx context.Context) error {
			mu.Lock()
			order = append(order, idx)
			mu.Unlock()
			time.Sleep(10 * time.Millisecond)
			return nil
		})
		if err != nil {
			t.Fatalf("run %d: %v", idx, err)
		}
	}

	if len(order) != 5 {
		t.Fatalf("expected 5 executions, got %d", len(order))
	}

	// 应该按顺序执行（串行）
	for i, v := range order {
		if v != i {
			t.Errorf("expected order[%d]=%d, got %d", i, i, v)
		}
	}
}

func TestSessionRunner_State(t *testing.T) {
	r := NewSessionRunner(RunnerConfig{})

	if r.State() != RunnerStateIdle {
		t.Error("expected idle state initially")
	}

	done := make(chan struct{})
	go func() {
		_ = r.Run(context.Background(), func(ctx context.Context) error {
			close(done)
			time.Sleep(50 * time.Millisecond)
			return nil
		})
	}()

	<-done
	// Now should be busy
	if r.State() != RunnerStateBusy {
		t.Error("expected busy state during execution")
	}

	// Wait for completion
	time.Sleep(100 * time.Millisecond)
	if r.State() != RunnerStateIdle {
		t.Error("expected idle state after completion")
	}
}

func TestSessionRunner_TryRun(t *testing.T) {
	r := NewSessionRunner(RunnerConfig{})

	// First TryRun should succeed
	block := make(chan struct{})
	go func() {
		_ = r.TryRun(context.Background(), func(ctx context.Context) error {
			<-block
			return nil
		})
	}()

	time.Sleep(50 * time.Millisecond) // 等待 goroutine 获取锁

	// Second TryRun should return ErrSessionBusy
	err := r.TryRun(context.Background(), func(ctx context.Context) error {
		return nil
	})
	if err != ErrSessionBusy {
		t.Errorf("expected ErrSessionBusy, got %v", err)
	}

	close(block)
	time.Sleep(50 * time.Millisecond)
}

func TestSessionRunner_Cancel(t *testing.T) {
	r := NewSessionRunner(RunnerConfig{})

	cancelled := make(chan bool, 1)
	go func() {
		_ = r.Run(context.Background(), func(ctx context.Context) error {
			<-ctx.Done()
			cancelled <- true
			return ctx.Err()
		})
	}()

	time.Sleep(50 * time.Millisecond)
	r.Cancel()

	select {
	case <-cancelled:
		// OK
	case <-time.After(time.Second):
		t.Error("expected cancellation")
	}
}

func TestSessionRunnerManager_GetOrCreate(t *testing.T) {
	m := NewSessionRunnerManager(RunnerConfig{})

	r1 := m.GetOrCreate("session-1")
	r2 := m.GetOrCreate("session-1")
	r3 := m.GetOrCreate("session-2")

	if r1 != r2 {
		t.Error("expected same runner for same session ID")
	}
	if r1 == r3 {
		t.Error("expected different runners for different session IDs")
	}
}

func TestSessionRunnerManager_ActiveRunners(t *testing.T) {
	m := NewSessionRunnerManager(RunnerConfig{})

	m.GetOrCreate("s1")
	m.GetOrCreate("s2")
	m.GetOrCreate("s3")

	if m.ActiveRunners() != 3 {
		t.Errorf("expected 3 active runners, got %d", m.ActiveRunners())
	}

	m.Delete("s1")
	if m.ActiveRunners() != 2 {
		t.Errorf("expected 2 active runners after delete, got %d", m.ActiveRunners())
	}
}

func TestSessionRunnerManager_Cleanup(t *testing.T) {
	m := NewSessionRunnerManager(RunnerConfig{})

	m.GetOrCreate("s1")
	m.GetOrCreate("s2")

	removed := m.Cleanup()
	if removed != 2 {
		t.Errorf("expected 2 removed, got %d", removed)
	}
	if m.ActiveRunners() != 0 {
		t.Error("expected 0 active runners after cleanup")
	}
}
