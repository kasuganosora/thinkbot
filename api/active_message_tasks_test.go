package api

import (
	"context"
	"testing"

	"github.com/kasuganosora/thinkbot/dao"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestActiveMessageTasks_FiltersBySession(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:active_tasks?mode=memory&cache=shared"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&dao.ChatMessage{}); err != nil {
		t.Fatal(err)
	}

	svc := &BotService{
		db:              db,
		messageCancels:  make(map[string]context.CancelFunc),
		messageSessions: make(map[string]string),
	}
	noop := func() {}
	svc.RegisterMessageCancel("bot1", "web-aaa", noop)
	svc.RememberMessageSession("bot1", "web-aaa", "10")
	svc.RegisterMessageCancel("bot1", "web-bbb", noop)
	svc.RememberMessageSession("bot1", "web-bbb", "20")
	svc.RegisterMessageCancel("bot1", "99", noop) // 续跑：trace==session，无 Remember 时靠数字回退

	all := svc.ActiveMessageTasks("bot1", "")
	if len(all) != 3 {
		t.Fatalf("all tasks = %d, want 3: %+v", len(all), all)
	}

	only10 := svc.ActiveMessageTasks("bot1", "10")
	if len(only10) != 1 || only10[0].TraceID != "web-aaa" || only10[0].SessionID != "10" {
		t.Fatalf("session 10 filter = %+v", only10)
	}

	only99 := svc.ActiveMessageTasks("bot1", "99")
	if len(only99) != 1 || only99[0].TraceID != "99" || only99[0].SessionID != "99" {
		t.Fatalf("session 99 numeric fallback = %+v", only99)
	}

	// DB 回退：cancel 在、Remember 无、消息表有 session
	svc.RegisterMessageCancel("bot1", "web-ccc", noop)
	if err := db.Create(&dao.ChatMessage{
		BotID: "bot1", TraceID: "web-ccc", SessionID: "30", Role: "user", Content: "hi",
	}).Error; err != nil {
		t.Fatal(err)
	}
	only30 := svc.ActiveMessageTasks("bot1", "30")
	if len(only30) != 1 || only30[0].TraceID != "web-ccc" {
		t.Fatalf("db fallback = %+v", only30)
	}

	ids := svc.ActiveMessageTraceIDs("bot1")
	if len(ids) != 4 {
		t.Fatalf("trace ids = %v", ids)
	}
}
