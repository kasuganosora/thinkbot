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

// 本文件钉死「人工兜底中止」的三种粒度边界（2026-10-12 复查 77a573a）。
//
// 墙钟硬上限默认关闭后（3655d0f），人工中止是唯一的收口手段，所以三条不变量必须成立：
//  1. AbortSession 只停该会话的，不误杀别的会话；
//  2. 解析不出会话归属的系统回合（heartbeat / outreach / cron）AbortSession 停不到，
//     但必须能被 AbortBot 收口，并且能被前端看见（UnattributedRunning）；
//  3. StopBot 必须把计时一并清掉，否则长跑进程里 messageStarts 会缓慢累积。

// newAbortScopeSvc 造一个只带「在跑消息」内存的 BotService（不碰 DB / bot 实例）。
func newAbortScopeSvc() *BotService {
	return &BotService{
		messageCancels:  make(map[string]context.CancelFunc),
		messageSessions: make(map[string]string),
		messageStarts:   make(map[string]time.Time),
	}
}

func TestAbortBot_StopsEverythingIncludingUnattributed(t *testing.T) {
	svc := newAbortScopeSvc()
	cancelled := map[string]bool{}
	register := func(trace string) {
		svc.RegisterMessageCancel("bot1", trace, func() { cancelled[trace] = true })
	}
	register("t1")
	svc.RememberMessageSession("bot1", "t1", "10")
	register("t2")
	svc.RememberMessageSession("bot1", "t2", "20")
	register("sys-heartbeat") // 无会话归属：heartbeat / cron 触发

	if n := svc.UnattributedRunning("bot1"); n != 1 {
		t.Fatalf("UnattributedRunning = %d, want 1 (only sys-heartbeat)", n)
	}

	if n := svc.AbortBot("bot1"); n != 3 {
		t.Fatalf("AbortBot = %d, want 3", n)
	}
	for _, want := range []string{"t1", "t2", "sys-heartbeat"} {
		if !cancelled[want] {
			t.Fatalf("trace %q must be cancelled, got %+v", want, cancelled)
		}
	}
	if left := svc.UnattributedRunning("bot1"); left != 0 {
		t.Fatalf("UnattributedRunning after abort = %d, want 0", left)
	}
}

func TestAbortSession_LeavesUnattributedAlone(t *testing.T) {
	svc := newAbortScopeSvc()
	cancelled := map[string]bool{}
	register := func(trace string) {
		svc.RegisterMessageCancel("bot1", trace, func() { cancelled[trace] = true })
	}
	register("t1")
	svc.RememberMessageSession("bot1", "t1", "10")
	register("sys-heartbeat")

	// 会话维度收口不该牵连系统回合：它不属于任何会话，误杀了没人知道。
	if n := svc.AbortSession("bot1", "10"); n != 1 {
		t.Fatalf("AbortSession(10) = %d, want 1", n)
	}
	if cancelled["sys-heartbeat"] {
		t.Fatal("unattributed turn must not be cancelled by a session-scoped abort")
	}
	// 但它们必须仍在 bot 级入口的射程内
	if n := svc.AbortBot("bot1"); n != 1 {
		t.Fatalf("AbortBot must still reach the unattributed turn, got %d", n)
	}
	if !cancelled["sys-heartbeat"] {
		t.Fatal("unattributed turn must be cancellable via AbortBot")
	}
}

func TestUnattributedRunning_IgnoresResolvableTasks(t *testing.T) {
	svc := newAbortScopeSvc()
	noop := func() {}
	svc.RegisterMessageCancel("bot1", "web-aaa", noop)
	svc.RememberMessageSession("bot1", "web-aaa", "10")
	svc.RegisterMessageCancel("bot1", "55", noop) // traceID 即数字会话：可解析，不算无归属
	svc.RegisterMessageCancel("bot1", "web-unknown", noop)

	if n := svc.UnattributedRunning("bot1"); n != 1 {
		t.Fatalf("UnattributedRunning = %d, want 1 (web-unknown only; 55 resolves to itself)", n)
	}
	if n := svc.UnattributedRunning(""); n != 0 {
		t.Fatalf("empty botID must report 0, got %d", n)
	}
}

func TestStopBot_ClearsMessageStarts(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:abort_scope_stop?mode=memory&cache=shared"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&dao.BotDefinition{}); err != nil {
		t.Fatal(err)
	}
	svc := newAbortScopeSvc()
	svc.db = db
	svc.RegisterMessageCancel("bot1", "t1", func() {})
	svc.RememberMessageSession("bot1", "t1", "10")
	svc.RegisterMessageCancel("bot1", "t2", func() {})
	if len(svc.messageStarts) != 2 {
		t.Fatalf("messageStarts = %d, want 2", len(svc.messageStarts))
	}
	svc.StopBot("bot1")
	if len(svc.messageStarts) != 0 {
		t.Fatalf("messageStarts must be cleared on stop, got %d", len(svc.messageStarts))
	}
	if len(svc.messageCancels) != 0 || len(svc.messageSessions) != 0 {
		t.Fatalf("cancels/sessions must be cleared too: %d / %d", len(svc.messageCancels), len(svc.messageSessions))
	}
}
