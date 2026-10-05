package api

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	noop_trace "go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/kasuganosora/thinkbot/agent/bot"
	"github.com/kasuganosora/thinkbot/dao"
)

func testBotServiceDB(t *testing.T) (*BotService, *gorm.DB) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "t.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&dao.BotDefinition{}); err != nil {
		t.Fatal(err)
	}
	svc := &BotService{
		db:              db,
		logger:          zap.NewNop().Sugar(),
		mgr:             bot.NewBotManager(zap.NewNop().Sugar(), noop_trace.NewTracerProvider()),
		botInstances:    make(map[string]*bot.Bot),
		containerStarts: make(map[string]context.CancelFunc),
		cancelFuncs:     make(map[string]context.CancelFunc),
		closeFuncs:      make(map[string]func()),
		channels:        make(map[string]*WebChannel),
	}
	return svc, db
}

func TestBeginContainerStartMarksStartingAndDetaches(t *testing.T) {
	svc, db := testBotServiceDB(t)
	if err := db.Create(&dao.BotDefinition{ID: "b1", Name: "n", Status: dao.BotStatusStopped}).Error; err != nil {
		t.Fatal(err)
	}
	ctx, already := svc.BeginContainerStart("b1")
	if already || ctx == nil {
		t.Fatalf("first begin: already=%v ctx=%v", already, ctx)
	}
	if got := svc.BotIntentStatus("b1"); got != dao.BotStatusStarting {
		t.Fatalf("status=%s", got)
	}
	_, already = svc.BeginContainerStart("b1")
	if !already {
		t.Fatal("second begin should be already in flight")
	}
	svc.CancelContainerStart("b1")
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("cancel did not abort detached context")
	}
}

func TestStopBotCancelsContainerStart(t *testing.T) {
	svc, db := testBotServiceDB(t)
	if err := db.Create(&dao.BotDefinition{ID: "b2", Name: "n", Status: dao.BotStatusStarting}).Error; err != nil {
		t.Fatal(err)
	}
	ctx, _ := svc.BeginContainerStart("b2")
	svc.StopBot("b2")
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("StopBot did not cancel container start")
	}
	if got := svc.BotIntentStatus("b2"); got != dao.BotStatusStopped {
		t.Fatalf("status after stop=%s", got)
	}
	if svc.HasContainerStart("b2") {
		t.Fatal("start still tracked after stop")
	}
}

func TestStartAllQueryIncludesStarting(t *testing.T) {
	_, db := testBotServiceDB(t)
	for _, def := range []dao.BotDefinition{
		{ID: "run", Name: "r", Status: dao.BotStatusRunning},
		{ID: "start", Name: "s", Status: dao.BotStatusStarting},
		{ID: "stop", Name: "x", Status: dao.BotStatusStopped},
	} {
		if err := db.Create(&def).Error; err != nil {
			t.Fatal(err)
		}
	}
	var defs []dao.BotDefinition
	if err := db.Where("status IN ?", []string{dao.BotStatusRunning, dao.BotStatusStarting}).Find(&defs).Error; err != nil {
		t.Fatal(err)
	}
	if len(defs) != 2 {
		t.Fatalf("want 2 candidates, got %d", len(defs))
	}
}

func TestBotTaskStatusSeparatesAgentFromContainer(t *testing.T) {
	svc, db := testBotServiceDB(t)
	if err := db.Create(&dao.BotDefinition{ID: "b3", Name: "n", Status: dao.BotStatusStopped}).Error; err != nil {
		t.Fatal(err)
	}
	srv := &Server{botSvc: svc}
	if got := srv.botTaskStatus("b3", "running", "abc"); got != "stopped" {
		t.Fatalf("running container + stopped bot → %s", got)
	}
	svc.SetBotStatus("b3", dao.BotStatusStarting)
	if got := srv.botTaskStatus("b3", "stopped", ""); got != "starting" {
		t.Fatalf("starting intent → %s", got)
	}
	svc.SetBotStatus("b3", dao.BotStatusStopped)
	if got := srv.botTaskStatus("b3", "stopped", ""); got != "not-created" {
		t.Fatalf("no container → %s", got)
	}
}
