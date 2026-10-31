package api

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/kasuganosora/thinkbot/dao"
	botsandbox "github.com/kasuganosora/thinkbot/docker/sandbox"
	"go.uber.org/zap"
)

func TestWaitForContainerImageResumesAcceptedStart(t *testing.T) {
	calls := 0
	err := waitForContainerImage(context.Background(), time.Millisecond, func(context.Context) error {
		calls++
		if calls < 3 {
			return fmt.Errorf("container not ready: %w", botsandbox.ErrImageBuilding)
		}
		return nil
	})
	if err != nil || calls != 3 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}

func TestWaitForContainerImageStopsOnFailureOrCancellation(t *testing.T) {
	failure := errors.New("build failed")
	calls := 0
	err := waitForContainerImage(context.Background(), time.Millisecond, func(context.Context) error { calls++; return failure })
	if !errors.Is(err, failure) || calls != 1 {
		t.Fatalf("terminal error retried: %d %v", calls, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	calls = 0
	err = waitForContainerImage(ctx, time.Hour, func(context.Context) error {
		calls++
		cancel()
		return botsandbox.ErrImageBuilding
	})
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("cancel ignored: %d %v", calls, err)
	}
}

func TestRunContainerStartPanicLeavesRetryableStatus(t *testing.T) {
	svc, db := testBotServiceDB(t)
	if err := db.Create(&dao.BotDefinition{ID: "panic", Name: "test", Status: dao.BotStatusStopped}).Error; err != nil {
		t.Fatal(err)
	}
	ctx, _ := svc.BeginContainerStart("panic")
	srv := &Server{botSvc: svc, logger: zap.NewNop().Sugar()}
	srv.runContainerStart("panic", nil, ctx) // A dependency panic must not leave a permanent spinner.
	if svc.HasContainerStart("panic") || svc.BotIntentStatus("panic") != dao.BotStatusStopped {
		t.Fatal("failed start cannot be retried")
	}
}

func TestStaleContainerStartCannotCancelOrCompleteRestart(t *testing.T) {
	svc, db := testBotServiceDB(t)
	if err := db.Create(&dao.BotDefinition{ID: "restart", Name: "test", Status: dao.BotStatusStopped}).Error; err != nil {
		t.Fatal(err)
	}
	old, _ := svc.BeginContainerStart("restart")
	svc.CancelContainerStart("restart")
	next, already := svc.BeginContainerStart("restart")
	if already {
		t.Fatal("restart was not accepted")
	}
	svc.FinishContainerStart("restart", old, true)
	if next.Err() != nil || !svc.HasContainerStart("restart") || svc.BotIntentStatus("restart") != dao.BotStatusStarting {
		t.Fatal("old worker changed the restarted attempt")
	}
	svc.FinishContainerStart("restart", next, false)
	if svc.HasContainerStart("restart") || svc.BotIntentStatus("restart") != dao.BotStatusStopped {
		t.Fatal("failed new attempt not finalized")
	}
}
