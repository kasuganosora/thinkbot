package sandbox

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.uber.org/zap"
)

func TestCancelledContainerStartCannotClearStopMarker(t *testing.T) {
	// No Docker exists on this PATH; the cancelled start must do no work.
	t.Setenv("PATH", t.TempDir())
	c := newBotContainer("b", Config{}, zap.NewNop().Sugar())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.mu.Lock()
	attempted := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(attempted)
		done <- c.start(ctx)
	}()
	<-attempted
	// Model stop winning c.mu while the old start is pending on that lock.
	cancel()
	c.stopped = true
	c.ready = false
	c.mu.Unlock()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("start = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled startup blocked")
	}
	if !c.stopped || c.ready {
		t.Fatal("cancelled startup cleared the user stop marker")
	}
}

func TestExplicitContainerStartStillClearsStopMarker(t *testing.T) {
	fakeLimitDocker(t, "running")
	c := newBotContainer("b", Config{Image: "example.invalid/custom:v1", NetworkDisabled: true}, zap.NewNop().Sugar())
	c.stopped = true
	if err := c.start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c.stopped || !c.ready {
		t.Fatal("explicit restart did not resume the stopped container")
	}
	if err := c.stop(); err != nil {
		t.Fatal(err)
	}
	if !c.stopped || c.ready {
		t.Fatal("stop following startup did not retain the stop marker")
	}
}
