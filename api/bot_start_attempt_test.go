package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kasuganosora/thinkbot/dao"
)

func reserveTestBotStart(t *testing.T, svc *BotService, ctx context.Context, id string) *botStartAttempt {
	t.Helper()
	svc.mu.Lock()
	attempt := svc.reserveBotStartLocked(ctx, id)
	svc.mu.Unlock()
	t.Cleanup(attempt.cancel)
	return attempt
}

func TestBotStartReadyAfterStopCannotPublish(t *testing.T) {
	svc, db := testBotServiceDB(t)
	if err := db.Create(&dao.BotDefinition{ID: "b", Name: "b", Status: dao.BotStatusStarting}).Error; err != nil {
		t.Fatal(err)
	}
	ctx, _ := svc.BeginContainerStart("b")
	attempt := reserveTestBotStart(t, svc, ctx, "b")

	// Force the problematic select outcome: Ready and ctx.Done are both ready,
	// then execute the publication path as though select chose Ready.
	ready := make(chan struct{})
	close(ready)
	svc.StopBot("b")
	<-ready
	if err := svc.publishBotStart("b", attempt, func() error {
		t.Error("cancelled startup published an instance")
		return nil
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("publication error = %v, want context.Canceled", err)
	}
	svc.FinishContainerStart("b", ctx, true)
	if got := svc.BotIntentStatus("b"); got != dao.BotStatusStopped {
		t.Fatalf("old startup resurrected DB status: %s", got)
	}
	if _, exists := svc.botInstances["b"]; exists || svc.mgr.Count() != 0 {
		t.Fatal("stopped startup left a published instance or placeholder")
	}
}

func TestBotStartOldPublicationAndRollbackCannotTouchReplacement(t *testing.T) {
	svc, db := testBotServiceDB(t)
	if err := db.Create(&dao.BotDefinition{ID: "b", Name: "b", Status: dao.BotStatusStarting}).Error; err != nil {
		t.Fatal(err)
	}
	old := reserveTestBotStart(t, svc, context.Background(), "b")
	svc.StopBot("b")
	next := reserveTestBotStart(t, svc, context.Background(), "b")

	if err := svc.publishBotStart("b", old, func() error {
		t.Error("old startup replaced the new attempt")
		return nil
	}); err == nil {
		t.Fatal("old startup publication succeeded")
	}
	svc.rollbackBotStart("b", old)
	if svc.botStarts["b"] != next {
		t.Fatal("old rollback removed replacement startup token")
	}
	if _, exists := svc.botInstances["b"]; !exists {
		t.Fatal("old rollback removed replacement placeholder")
	}
	published := false
	if err := svc.publishBotStart("b", next, func() error {
		published = true
		return nil
	}); err != nil || !published {
		t.Fatalf("replacement publication: published=%v err=%v", published, err)
	}
}

func TestBotStartPublicationRequiresMatchingToken(t *testing.T) {
	svc, _ := testBotServiceDB(t)
	old := reserveTestBotStart(t, svc, context.Background(), "b")
	// Identity is a separate safeguard from ctx cancellation: a stale but still
	// live context cannot publish over an independently installed attempt.
	next := reserveTestBotStart(t, svc, context.Background(), "b")
	if err := svc.publishBotStart("b", old, func() error {
		t.Error("unowned startup published")
		return nil
	}); err == nil {
		t.Fatal("missing token conflict")
	}
	svc.rollbackBotStart("b", old)
	if svc.botStarts["b"] != next {
		t.Fatal("stale cleanup removed current token")
	}
}

func TestBotStopAfterPublicationKeepsStoppedStatus(t *testing.T) {
	svc, db := testBotServiceDB(t)
	if err := db.Create(&dao.BotDefinition{ID: "b", Name: "b", Status: dao.BotStatusStarting}).Error; err != nil {
		t.Fatal(err)
	}
	attempt := reserveTestBotStart(t, svc, context.Background(), "b")
	inPublication := make(chan struct{})
	releasePublication := make(chan struct{})
	publicationDone := make(chan error, 1)
	go func() {
		publicationDone <- svc.publishBotStart("b", attempt, func() error {
			close(inPublication)
			<-releasePublication
			return db.Model(&dao.BotDefinition{}).Where("id = ?", "b").Update("status", dao.BotStatusRunning).Error
		})
	}()
	select {
	case <-inPublication:
	case <-time.After(5 * time.Second):
		t.Fatal("publication did not reach barrier")
	}
	stopDone := make(chan struct{})
	go func() {
		svc.StopBot("b")
		close(stopDone)
	}()
	// Publication owns the service lock until its running-status write; stop
	// must serialize after it, even when requested before that write completes.
	close(releasePublication)
	if err := <-publicationDone; err != nil {
		t.Fatal(err)
	}
	select {
	case <-stopDone:
	case <-time.After(5 * time.Second):
		t.Fatal("stop did not finish")
	}
	if got := svc.BotIntentStatus("b"); got != dao.BotStatusStopped {
		t.Fatalf("publication overwrote stop: %s", got)
	}
}
