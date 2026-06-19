package session

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/kasuganosora/thinkbot/agent/core"
)

func noopTracerProvider() trace.TracerProvider {
	return trace.NewNoopTracerProvider()
}

// ============================================================================
// Session 实体测试
// ============================================================================

func TestNewSession(t *testing.T) {
	s := NewSession("s1", "bot1", "ch1")
	if s.ID() != "s1" {
		t.Errorf("ID = %q, want %q", s.ID(), "s1")
	}
	if s.BotID() != "bot1" {
		t.Errorf("BotID = %q, want %q", s.BotID(), "bot1")
	}
	if s.Channel() != "ch1" {
		t.Errorf("Channel = %q, want %q", s.Channel(), "ch1")
	}
	if !s.IsActive() {
		t.Error("new session should be active")
	}
	if s.CreatedBy() != "user" {
		t.Errorf("CreatedBy = %q, want %q", s.CreatedBy(), "user")
	}
	if s.MessageCount() != 0 {
		t.Errorf("MessageCount = %d, want 0", s.MessageCount())
	}
}

func TestSessionWithOptions(t *testing.T) {
	s := NewSession("s1", "bot1", "ch1",
		WithMaxMessages(5),
		WithCreatedBy("bot"),
	)
	if s.maxMessages != 5 {
		t.Errorf("maxMessages = %d, want 5", s.maxMessages)
	}
	if s.CreatedBy() != "bot" {
		t.Errorf("CreatedBy = %q, want %q", s.CreatedBy(), "bot")
	}
}

func TestSessionAppendMessage(t *testing.T) {
	s := NewSession("s1", "bot1", "ch1", WithMaxMessages(3))

	for i := 0; i < 5; i++ {
		s.AppendMessage(Message{
			Role: "user",
			Text: "msg",
		})
	}

	if s.MessageCount() != 5 {
		t.Errorf("MessageCount = %d, want 5", s.MessageCount())
	}

	msgs := s.Messages()
	if len(msgs) != 3 {
		t.Errorf("len(Messages) = %d, want 3 (FIFO eviction)", len(msgs))
	}
}

func TestSessionRecentMessages(t *testing.T) {
	s := NewSession("s1", "bot1", "ch1", WithMaxMessages(10))

	for i := 0; i < 5; i++ {
		s.AppendMessage(Message{
			Role: "user",
			Text: "msg",
		})
	}

	recent := s.RecentMessages(3)
	if len(recent) != 3 {
		t.Errorf("len(RecentMessages(3)) = %d, want 3", len(recent))
	}

	all := s.RecentMessages(0)
	if len(all) != 5 {
		t.Errorf("len(RecentMessages(0)) = %d, want 5", len(all))
	}
}

func TestSessionArchive(t *testing.T) {
	s := NewSession("s1", "bot1", "ch1")
	s.AppendMessage(Message{Role: "user", Text: "hi"})

	if !s.IsActive() {
		t.Error("should be active before archive")
	}

	s.Archive()

	if s.IsActive() {
		t.Error("should not be active after archive")
	}
	if s.Status() != StatusArchived {
		t.Errorf("Status = %q, want %q", s.Status(), StatusArchived)
	}
}

func TestSessionTopic(t *testing.T) {
	s := NewSession("s1", "bot1", "ch1")
	if s.Topic() != "" {
		t.Errorf("Topic = %q, want empty", s.Topic())
	}
	s.SetTopic("Go memory leak")
	if s.Topic() != "Go memory leak" {
		t.Errorf("Topic = %q, want %q", s.Topic(), "Go memory leak")
	}
}

func TestSessionIdleDuration(t *testing.T) {
	s := NewSession("s1", "bot1", "ch1")
	s.lastActivityAt = time.Now().Add(-10 * time.Minute)

	d := s.IdleDuration()
	if d < 9*time.Minute {
		t.Errorf("IdleDuration = %v, want >= 9m", d)
	}
}

// ============================================================================
// DefaultResolver 测试
// ============================================================================

func TestDefaultResolver_ReplyID(t *testing.T) {
	r := NewDefaultResolver("session")
	msg := &core.Message{
		Metadata: map[string]any{"reply_id": "note123"},
	}

	result := r.Resolve(context.Background(), msg)
	if !result.OK {
		t.Error("should resolve for reply_id")
	}
	if result.SessionID != "session:thread:note123" {
		t.Errorf("SessionID = %q, want %q", result.SessionID, "session:thread:note123")
	}
}

func TestDefaultResolver_Mentioned(t *testing.T) {
	r := NewDefaultResolver("session")
	msg := &core.Message{
		Channel:   "ch1",
		Mentioned: true,
	}

	result := r.Resolve(context.Background(), msg)
	if !result.OK {
		t.Error("should resolve for mentioned")
	}
	if result.SessionID != "session:channel:ch1" {
		t.Errorf("SessionID = %q, want %q", result.SessionID, "session:channel:ch1")
	}
}

func TestDefaultResolver_NoSession(t *testing.T) {
	r := NewDefaultResolver("session")
	msg := &core.Message{
		Channel:   "ch1",
		Mentioned: false,
	}

	result := r.Resolve(context.Background(), msg)
	if result.OK {
		t.Error("should not resolve for non-mentioned, no reply_id")
	}
}

// ============================================================================
// MisskeyResolver 测试
// ============================================================================

func TestMisskeyResolver_ReplyID(t *testing.T) {
	r := NewMisskeyResolver()
	msg := &core.Message{
		Metadata: map[string]any{"reply_id": "note_abc"},
	}

	result := r.Resolve(context.Background(), msg)
	if !result.OK {
		t.Error("should resolve for reply_id")
	}
	if result.SessionID != "mk:thread:note_abc" {
		t.Errorf("SessionID = %q, want %q", result.SessionID, "mk:thread:note_abc")
	}
}

func TestMisskeyResolver_Mentioned(t *testing.T) {
	r := NewMisskeyResolver()
	msg := &core.Message{
		Channel:   "user123",
		Mentioned: true,
	}

	result := r.Resolve(context.Background(), msg)
	if !result.OK {
		t.Error("should resolve for mentioned")
	}
	if result.SessionID != "mk:channel:user123" {
		t.Errorf("SessionID = %q, want %q", result.SessionID, "mk:channel:user123")
	}
}

func TestMisskeyResolver_TimelineNoSession(t *testing.T) {
	r := NewMisskeyResolver()
	msg := &core.Message{
		Channel:   "user123",
		Mentioned: false,
		Metadata:  map[string]any{"event_type": "timeline"},
	}

	result := r.Resolve(context.Background(), msg)
	if result.OK {
		t.Error("timeline post should not create session")
	}
}

// ============================================================================
// TelegramResolver 测试
// ============================================================================

func TestTelegramResolver(t *testing.T) {
	r := NewTelegramResolver()
	msg := &core.Message{
		Channel: "tg_chat_123",
	}

	result := r.Resolve(context.Background(), msg)
	if !result.OK {
		t.Error("Telegram should always create session")
	}
	if result.SessionID != "tg:tg_chat_123" {
		t.Errorf("SessionID = %q, want %q", result.SessionID, "tg:tg_chat_123")
	}
}

// ============================================================================
// NeverResolver 测试
// ============================================================================

func TestNeverResolver(t *testing.T) {
	r := NewNeverResolver()
	msg := &core.Message{
		Channel:   "ch1",
		Mentioned: true,
	}

	result := r.Resolve(context.Background(), msg)
	if result.OK {
		t.Error("NeverResolver should never resolve")
	}
}

// ============================================================================
// ChannelResolver 测试
// ============================================================================

func TestChannelResolver(t *testing.T) {
	r := NewChannelResolver()
	r.Register("misskey", NewMisskeyResolver())
	r.Register("rss", NewNeverResolver())

	// Misskey 消息
	mkMsg := &core.Message{
		Source:   "misskey",
		Channel:  "user1",
		Metadata: map[string]any{"reply_id": "n1"},
	}
	mkResult := r.Resolve(context.Background(), mkMsg)
	if !mkResult.OK || mkResult.SessionID != "mk:thread:n1" {
		t.Errorf("Misskey resolve failed: %+v", mkResult)
	}

	// RSS 消息
	rssMsg := &core.Message{
		Source:  "rss",
		Channel: "feed1",
	}
	rssResult := r.Resolve(context.Background(), rssMsg)
	if rssResult.OK {
		t.Error("RSS should not resolve")
	}

	// 未知 source → default resolver
	unknownMsg := &core.Message{
		Source:    "webhook",
		Channel:   "ch1",
		Mentioned: true,
	}
	unknownResult := r.Resolve(context.Background(), unknownMsg)
	if !unknownResult.OK {
		t.Error("unknown source with Mentioned should resolve via default")
	}
}

// ============================================================================
// SessionManager 测试
// ============================================================================

func TestSessionManager_GetOrCreate(t *testing.T) {
	mgr := NewSessionManager(
		NewDefaultResolver("test"),
		DefaultManagerConfig(),
		noopTracerProvider(),
		testLogger(),
	)

	// 首次创建
	s1, isNew := mgr.GetOrCreate("s1", "bot1", "ch1", "user")
	if !isNew {
		t.Error("first GetOrCreate should return isNew=true")
	}
	if s1 == nil || s1.ID() != "s1" {
		t.Error("session should be created")
	}

	// 第二次获取
	s2, isNew := mgr.GetOrCreate("s1", "bot1", "ch1", "user")
	if isNew {
		t.Error("second GetOrCreate should return isNew=false")
	}
	if s2.ID() != "s1" {
		t.Error("should return same session")
	}
}

func TestSessionManager_Get(t *testing.T) {
	mgr := NewSessionManager(
		NewDefaultResolver("test"),
		DefaultManagerConfig(),
		noopTracerProvider(),
		testLogger(),
	)

	mgr.GetOrCreate("s1", "bot1", "ch1", "user")

	if mgr.Get("s1") == nil {
		t.Error("Get should return session")
	}
	if mgr.Get("nonexistent") != nil {
		t.Error("Get should return nil for nonexistent")
	}
}

func TestSessionManager_Archive(t *testing.T) {
	mgr := NewSessionManager(
		NewDefaultResolver("test"),
		DefaultManagerConfig(),
		noopTracerProvider(),
		testLogger(),
	)

	archived := make(chan string, 1)
	mgr.OnArchive(func(s *Session) {
		archived <- s.ID()
	})

	mgr.GetOrCreate("s1", "bot1", "ch1", "user")
	mgr.Archive("s1")

	select {
	case id := <-archived:
		if id != "s1" {
			t.Errorf("archive callback got %q, want %q", id, "s1")
		}
	case <-time.After(time.Second):
		t.Error("archive callback not called")
	}

	if mgr.Get("s1") != nil {
		t.Error("archived session should be removed from manager")
	}
}

func TestSessionManager_Sweep(t *testing.T) {
	mgr := NewSessionManager(
		NewDefaultResolver("test"),
		ManagerConfig{
			MaxMessages:   5,
			IdleTimeout:   50 * time.Millisecond,
			SweepInterval: 0, // 手动 Sweep
		},
		noopTracerProvider(),
		testLogger(),
	)

	// 创建两个 session
	s1, _ := mgr.GetOrCreate("s1", "bot1", "ch1", "user")
	_, _ = mgr.GetOrCreate("s2", "bot1", "ch2", "user")

	// s1 设为很久以前活动
	s1.mu.Lock()
	s1.lastActivityAt = time.Now().Add(-1 * time.Hour)
	s1.mu.Unlock()

	// 等待 idle timeout
	time.Sleep(60 * time.Millisecond)

	// 刷新 s2 的活动时间
	if s2 := mgr.Get("s2"); s2 != nil {
		s2.AppendMessage(Message{Role: "user", Text: "ping"})
	}

	archived := mgr.Sweep()
	if archived != 1 {
		t.Errorf("Sweep archived %d, want 1", archived)
	}

	// s2 应该还在
	if mgr.Get("s2") == nil {
		t.Error("s2 should still be active")
	}
}

func TestSessionManager_ActiveCount(t *testing.T) {
	mgr := NewSessionManager(
		NewDefaultResolver("test"),
		DefaultManagerConfig(),
		noopTracerProvider(),
		testLogger(),
	)

	if mgr.ActiveCount() != 0 {
		t.Errorf("ActiveCount = %d, want 0", mgr.ActiveCount())
	}

	mgr.GetOrCreate("s1", "bot1", "ch1", "user")
	mgr.GetOrCreate("s2", "bot1", "ch2", "user")

	if mgr.ActiveCount() != 2 {
		t.Errorf("ActiveCount = %d, want 2", mgr.ActiveCount())
	}
}

// ============================================================================
// SessionStage 测试
// ============================================================================

func TestSessionStage_ResolveAndInject(t *testing.T) {
	resolver := NewDefaultResolver("test")
	mgr := NewSessionManager(resolver, DefaultManagerConfig(), noopTracerProvider(), testLogger())
	stage := NewSessionStage("session", mgr, DefaultStageConfig(), noopTracerProvider(), testLogger())

	// 被 @ 的消息
	msg := core.Message{
		ID:        "m1",
		BotID:     "bot1",
		Channel:   "ch1",
		Text:      "hello",
		Mentioned: true,
	}
	env := core.NewEnvelope(msg)

	out, err := stage.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("Process error: %v", err)
	}
	if out == nil {
		t.Fatal("Process returned nil envelope")
	}

	// 验证注入
	if id := SessionIDFromEnvelope(env); id == "" {
		t.Error("session.id should be injected")
	}
	if active, _ := env.Get("session.active"); active != true {
		t.Error("session.active should be true")
	}
	if isNew, _ := env.Get("session.is_new"); isNew != true {
		t.Error("session.is_new should be true for first message")
	}
}

func TestSessionStage_NoSessionForTimeline(t *testing.T) {
	resolver := NewMisskeyResolver()
	mgr := NewSessionManager(resolver, DefaultManagerConfig(), noopTracerProvider(), testLogger())
	stage := NewSessionStage("session", mgr, DefaultStageConfig(), noopTracerProvider(), testLogger())

	// 时间线消息（无 reply, 未 @）
	msg := core.Message{
		ID:        "m1",
		BotID:     "bot1",
		Channel:   "user1",
		Text:      "timeline post",
		Mentioned: false,
		Metadata:  map[string]any{"event_type": "timeline"},
	}
	env := core.NewEnvelope(msg)

	out, err := stage.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("Process error: %v", err)
	}
	if out == nil {
		t.Fatal("Process should not drop non-session messages")
	}

	if active, _ := env.Get("session.active"); active == true {
		t.Error("session.active should be false for timeline post")
	}
	if id := SessionIDFromEnvelope(env); id != "" {
		t.Error("session.id should be empty for timeline post")
	}
}

// ============================================================================
// SessionWriteStage 测试
// ============================================================================

func TestSessionWriteStage(t *testing.T) {
	resolver := NewDefaultResolver("test")
	mgr := NewSessionManager(resolver, DefaultManagerConfig(), noopTracerProvider(), testLogger())
	readStage := NewSessionStage("session", mgr, DefaultStageConfig(), noopTracerProvider(), testLogger())
	writeStage := NewSessionWriteStage("session_write", mgr, noopTracerProvider(), testLogger())

	// 用户消息
	msg := core.Message{
		ID:        "m1",
		BotID:     "bot1",
		Channel:   "ch1",
		Text:      "hello bot",
		Mentioned: true,
	}
	env := core.NewEnvelope(msg)

	// ReadStage 解析 session
	readStage.Process(context.Background(), env)

	// 模拟 Bot 回复
	env.AddAction(core.Action{
		Type:    core.ActionReply,
		Channel: "ch1",
		Payload: "hi there!",
	})

	// WriteStage 写入回复
	writeStage.Process(context.Background(), env)

	// 验证 session 包含 user + assistant
	sessionID := SessionIDFromEnvelope(env)
	session := mgr.Get(sessionID)
	if session == nil {
		t.Fatal("session should exist")
	}

	msgs := session.Messages()
	if len(msgs) != 2 {
		t.Fatalf("session should have 2 messages, got %d", len(msgs))
	}
	if msgs[0].Role != "user" {
		t.Errorf("msgs[0].Role = %q, want %q", msgs[0].Role, "user")
	}
	if msgs[1].Role != "assistant" {
		t.Errorf("msgs[1].Role = %q, want %q", msgs[1].Role, "assistant")
	}
	if msgs[1].Text != "hi there!" {
		t.Errorf("msgs[1].Text = %q, want %q", msgs[1].Text, "hi there!")
	}
}

func TestSessionWriteStage_NoSession(t *testing.T) {
	mgr := NewSessionManager(NewDefaultResolver("test"), DefaultManagerConfig(), noopTracerProvider(), testLogger())
	writeStage := NewSessionWriteStage("session_write", mgr, noopTracerProvider(), testLogger())

	// 没有 session 的消息
	msg := core.Message{
		ID:      "m1",
		Channel: "ch1",
	}
	env := core.NewEnvelope(msg)
	env.AddAction(core.Action{
		Type:    core.ActionReply,
		Payload: "reply",
	})

	// 不应 panic
	out, err := writeStage.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("Process error: %v", err)
	}
	if out == nil {
		t.Fatal("Process should not drop")
	}
}

// ============================================================================
// FormatContext 测试
// ============================================================================

func TestFormatContext(t *testing.T) {
	s := NewSession("s1", "bot1", "ch1", WithMaxMessages(10))
	s.AppendMessage(Message{
		Role:      "user",
		Text:      "what is 2+2?",
		Timestamp: time.Now(),
	})
	s.AppendMessage(Message{
		Role:      "assistant",
		Text:      "4",
		Timestamp: time.Now(),
	})

	ctx := FormatContext(s, 10)
	if ctx == "" {
		t.Fatal("FormatContext should not be empty")
	}
	if !contains(ctx, "what is 2+2?") {
		t.Error("context should contain user message")
	}
	if !contains(ctx, "4") {
		t.Error("context should contain assistant message")
	}
}

func TestFormatContext_Empty(t *testing.T) {
	s := NewSession("s1", "bot1", "ch1")
	if ctx := FormatContext(s, 10); ctx != "" {
		t.Errorf("empty session context should be empty string, got %q", ctx)
	}
}

func TestFormatContext_Topic(t *testing.T) {
	s := NewSession("s1", "bot1", "ch1")
	s.SetTopic("math homework")
	s.AppendMessage(Message{Role: "user", Text: "what is 2+2?"})

	ctx := FormatContext(s, 10)
	if !contains(ctx, "math homework") {
		t.Error("context should contain topic")
	}
}

// ============================================================================
// 辅助函数
// ============================================================================

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || indexOf(s, substr) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
