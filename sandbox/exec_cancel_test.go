package sandbox

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
)

// Cancelling one command must stop its descendants, without stopping another
// invocation in the same workspace. Waiting for the survivor also gives the
// cancelled descendant enough time to write its marker if it escaped cleanup.
func assertExecCancellationIsScoped(t *testing.T, ws Workspace) {
	t.Helper()
	stream, ok := ws.(StreamWorkspace)
	if !ok {
		t.Fatal("workspace does not support streaming")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	type outcome struct {
		result *ExecResult
		err    error
	}
	survivorReady := make(chan struct{})
	var readyOnce sync.Once
	survivor := make(chan outcome, 1)
	go func() {
		r, err := stream.ExecStream(ctx, ExecRequest{Command: "echo survivor-ready; sleep 3; echo survived > surviving.txt"}, func(c ExecChunk) {
			if strings.Contains(c.Data, "survivor-ready") {
				readyOnce.Do(func() { close(survivorReady) })
			}
		})
		survivor <- outcome{r, err}
	}()
	select {
	case <-survivorReady:
	case out := <-survivor:
		t.Fatalf("survivor did not start: %+v %v", out.result, out.err)
	case <-ctx.Done():
		t.Fatal("survivor startup timed out")
	}
	stopCtx, stop := context.WithCancel(ctx)
	defer stop()
	r, err := stream.ExecStream(stopCtx, ExecRequest{Command: "(sleep 2; echo escaped > cancelled.txt) & echo cancel-ready; wait"}, func(c ExecChunk) {
		if strings.Contains(c.Data, "cancel-ready") {
			stop()
		}
	})
	if err != nil {
		t.Fatalf("cancelled exec: %v", err)
	}
	if !r.Aborted || r.Reliable || r.ExitCode != -1 || !containsWarning(r.Warnings, "cancelled") {
		t.Fatalf("cancelled execution must report incomplete status: %+v", r)
	}
	if containsWarning(r.Warnings, "could not confirm") {
		t.Fatalf("cancellation failed: %+v", r)
	}
	select {
	case out := <-survivor:
		if out.err != nil || out.result.ExitCode != 0 || !out.result.Reliable {
			t.Fatalf("independent exec was interrupted: %+v %v", out.result, out.err)
		}
	case <-ctx.Done():
		t.Fatal("independent exec did not finish")
	}
	if data, err := ws.ReadFile(ctx, "surviving.txt"); err != nil || strings.TrimSpace(string(data)) != "survived" {
		t.Fatalf("surviving command's file: %q %v", data, err)
	}
	if data, err := ws.ReadFile(ctx, "cancelled.txt"); err == nil {
		t.Fatalf("cancelled descendant kept running and wrote %q", data)
	}
}

func TestLocalExecCancellationIsScoped(t *testing.T) {
	cfg := DefaultConfig()
	cfg.BaseDir = t.TempDir()
	sb, err := newLocalSandbox(cfg, zap.NewNop().Sugar())
	if err != nil {
		t.Fatal(err)
	}
	ws, err := sb.Create("cancel")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	assertExecCancellationIsScoped(t, ws)
}
