package api

import (
	"context"
	"testing"
	"time"

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
		messageStarts:   make(map[string]time.Time),
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

// TestActiveMessageTasks_ReportsElapsed 钉死「跑了多久」：墙钟硬上限默认关闭后，
// Web 靠它判断这一轮是不是挂死了。
func TestActiveMessageTasks_ReportsElapsed(t *testing.T) {
	svc := &BotService{
		messageCancels:  make(map[string]context.CancelFunc),
		messageSessions: make(map[string]string),
		messageStarts:   make(map[string]time.Time),
	}
	svc.RegisterMessageCancel("bot1", "web-aaa", func() {})
	svc.RememberMessageSession("bot1", "web-aaa", "10")
	// 回拨开始时刻 2 分钟，elapsed 应约为 2 分钟
	svc.mu.Lock()
	svc.messageStarts[messageCancelKey("bot1", "web-aaa")] = time.Now().Add(-2 * time.Minute)
	svc.mu.Unlock()

	got := svc.ActiveMessageTasks("bot1", "10")
	if len(got) != 1 {
		t.Fatalf("tasks = %+v", got)
	}
	if got[0].StartedAt == "" {
		t.Fatal("startedAt must be reported")
	}
	if got[0].ElapsedMs < 119_000 || got[0].ElapsedMs > 121_000 {
		t.Fatalf("elapsedMs = %d, want ~120000", got[0].ElapsedMs)
	}

	// 同一 traceID 重复注册不得把计时清零（续跑场景）
	svc.RegisterMessageCancel("bot1", "web-aaa", func() {})
	if again := svc.ActiveMessageTasks("bot1", "10"); again[0].ElapsedMs < 119_000 {
		t.Fatalf("re-register must not reset the timer, elapsedMs = %d", again[0].ElapsedMs)
	}
}

// TestAbortSession 钉死「按会话一次收口」：用户中止时只认会话，不认 traceID。
func TestAbortSession(t *testing.T) {
	svc := &BotService{
		messageCancels:  make(map[string]context.CancelFunc),
		messageSessions: make(map[string]string),
		messageStarts:   make(map[string]time.Time),
	}
	cancelled := map[string]bool{}
	register := func(trace string) {
		svc.RegisterMessageCancel("bot1", trace, func() { cancelled[trace] = true })
	}
	register("t1") // session 10
	svc.RememberMessageSession("bot1", "t1", "10")
	register("t2") // session 10（同一会话第二条）
	svc.RememberMessageSession("bot1", "t2", "10")
	register("t3") // session 20 —— 不该被误杀
	svc.RememberMessageSession("bot1", "t3", "20")
	register("10") // traceID 即数字 sessionID（web 工作流续跑），也应命中
	svc.RememberMessageSession("bot1", "10", "10")

	if n := svc.AbortSession("bot1", "10"); n != 3 {
		t.Fatalf("AbortSession(10) = %d, want 3 (t1,t2,10)", n)
	}
	for _, want := range []string{"t1", "t2", "10"} {
		if !cancelled[want] {
			t.Fatalf("trace %q must be cancelled, got %+v", want, cancelled)
		}
	}
	if cancelled["t3"] {
		t.Fatal("t3 belongs to another session and must not be cancelled")
	}
	// 其它会话不受影响，且再次中止应无命中（幂等）
	if left := svc.ActiveMessageTasks("bot1", "20"); len(left) != 1 {
		t.Fatalf("session 20 must still have 1 task, got %+v", left)
	}
	if n := svc.AbortSession("bot1", "10"); n != 0 {
		t.Fatalf("second AbortSession must be a no-op, got %d", n)
	}
	if n := svc.AbortSession("bot1", ""); n != 0 {
		t.Fatalf("empty sessionID must abort nothing, got %d", n)
	}
}
