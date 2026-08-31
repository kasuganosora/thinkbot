package api

import (
	"testing"
	"time"

	"go.uber.org/zap"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/kasuganosora/thinkbot/dao"
)

func newTestChatHistory(t *testing.T) *ChatHistoryService {
	t.Helper()
	// 每个用例一个独立的私有内存库，互不干扰。
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=private"), &gorm.Config{
		Logger: logger.Discard,
	})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&dao.ChatMessage{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// 用裸构造避开 NewChatHistoryService 的启动清理，让每个用例自己控制数据。
	return &ChatHistoryService{db: db, logger: zap.NewNop().Sugar()}
}

func countAssistant(t *testing.T, s *ChatHistoryService, traceID string) int64 {
	t.Helper()
	var n int64
	if err := s.db.Model(&dao.ChatMessage{}).
		Where("trace_id = ? AND role = ?", traceID, dao.ChatRoleAssistant).
		Count(&n).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func loadAssistant(t *testing.T, s *ChatHistoryService, traceID string) dao.ChatMessage {
	t.Helper()
	var m dao.ChatMessage
	if err := s.db.Where("trace_id = ? AND role = ?", traceID, dao.ChatRoleAssistant).
		First(&m).Error; err != nil {
		t.Fatalf("load: %v", err)
	}
	return m
}

// TestUpsertAssistantByTrace_Idempotent 验证反复落库只产生一行。
//
// 这是流式增量落库的核心前提：回复过程中会被调用很多次，若每次都 Insert，
// 用户会在历史里看到同一条回复的几十个副本。
func TestUpsertAssistantByTrace_Idempotent(t *testing.T) {
	s := newTestChatHistory(t)
	const trace = "web-trace-1"

	for i, content := range []string{"partial", "partial more", "final text"} {
		streaming := i < 2
		if err := s.UpsertAssistantByTrace("bot1", "1", content, trace, "", "", "sess1", streaming); err != nil {
			t.Fatalf("upsert #%d: %v", i, err)
		}
	}

	if n := countAssistant(t, s, trace); n != 1 {
		t.Fatalf("row count = %d, want 1 (upsert must not duplicate rows)", n)
	}
	m := loadAssistant(t, s, trace)
	if m.Content != "final text" {
		t.Errorf("Content = %q, want the latest value", m.Content)
	}
	if m.Streaming {
		t.Error("Streaming should be false after the final (non-streaming) write")
	}
}

// TestUpsertAssistantByTrace_PreservesCreatedAt 验证更新不改 created_at。
//
// created_at 是历史分页的主排序键。若每次增量落库都刷新它，这条消息会在
// 回复过程中不断跳到列表末尾，顺序全乱。
func TestUpsertAssistantByTrace_PreservesCreatedAt(t *testing.T) {
	s := newTestChatHistory(t)
	const trace = "web-trace-2"

	if err := s.UpsertAssistantByTrace("bot1", "1", "first", trace, "", "", "sess1", true); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	created := loadAssistant(t, s, trace).CreatedAt

	time.Sleep(10 * time.Millisecond)
	if err := s.UpsertAssistantByTrace("bot1", "1", "second", trace, "", "", "sess1", false); err != nil {
		t.Fatalf("second upsert: %v", err)
	}

	if got := loadAssistant(t, s, trace).CreatedAt; !got.Equal(created) {
		t.Fatalf("CreatedAt changed from %v to %v; history ordering would break", created, got)
	}
}

// TestUpsertAssistantByTrace_UpdatesToolsAndParts 验证工具与 parts 会被刷新。
func TestUpsertAssistantByTrace_UpdatesToolsAndParts(t *testing.T) {
	s := newTestChatHistory(t)
	const trace = "web-trace-3"

	running := `[{"id":"t1","status":"running"}]`
	done := `[{"id":"t1","status":"success"}]`

	if err := s.UpsertAssistantByTrace("bot1", "1", "x", trace, running, running, "sess1", true); err != nil {
		t.Fatalf("upsert running: %v", err)
	}
	if err := s.UpsertAssistantByTrace("bot1", "1", "x", trace, done, done, "sess1", false); err != nil {
		t.Fatalf("upsert done: %v", err)
	}

	m := loadAssistant(t, s, trace)
	if m.ToolCalls != done {
		t.Errorf("ToolCalls = %q, want %q", m.ToolCalls, done)
	}
	if m.PartsJSON != done {
		t.Errorf("PartsJSON = %q, want %q", m.PartsJSON, done)
	}
}

// TestUpsertAssistantByTrace_EmptyTraceFallsBackToInsert 验证无幂等键时退化为插入。
//
// 没有 traceID 就无法判定「是同一条消息」，此时必须各自插入，绝不能相互覆盖。
func TestUpsertAssistantByTrace_EmptyTraceFallsBackToInsert(t *testing.T) {
	s := newTestChatHistory(t)

	for _, c := range []string{"a", "b"} {
		if err := s.UpsertAssistantByTrace("bot1", "1", c, "", "", "", "sess1", false); err != nil {
			t.Fatalf("upsert %q: %v", c, err)
		}
	}
	var n int64
	if err := s.db.Model(&dao.ChatMessage{}).Where("trace_id = ?", "").Count(&n).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Fatalf("row count = %d, want 2 (no idempotency key means no merging)", n)
	}
}

// TestMarkStreamingStale 验证启动清理把遗留中间态收敛。
//
// 进程被中断时留下的 streaming=true 行不可能再产出；不清理会让前端把其中的
// running 工具卡片渲染成永久转圈。
func TestMarkStreamingStale(t *testing.T) {
	s := newTestChatHistory(t)

	if err := s.UpsertAssistantByTrace("bot1", "1", "live", "t-live", "", "", "sess1", true); err != nil {
		t.Fatalf("seed streaming: %v", err)
	}
	if err := s.UpsertAssistantByTrace("bot1", "1", "done", "t-done", "", "", "sess1", false); err != nil {
		t.Fatalf("seed settled: %v", err)
	}

	n, err := s.MarkStreamingStale()
	if err != nil {
		t.Fatalf("MarkStreamingStale: %v", err)
	}
	if n != 1 {
		t.Fatalf("affected rows = %d, want 1 (only the streaming row)", n)
	}
	if loadAssistant(t, s, "t-live").Streaming {
		t.Error("stale streaming row should have been settled")
	}

	// 幂等：再次调用无行可改。
	if n2, err := s.MarkStreamingStale(); err != nil || n2 != 0 {
		t.Fatalf("second call = (%d, %v), want (0, nil)", n2, err)
	}
}
