package command

import (
	"context"
	"errors"
	"testing"

	"go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/agent/session"
)

// ============================================================================
// Test helpers
// ============================================================================

func testLogger() *zap.SugaredLogger {
	return zap.NewNop().Sugar()
}

func testTP() noop.TracerProvider {
	return noop.NewTracerProvider()
}

func makeEnvelope(text, userID string) *core.Envelope {
	return core.NewEnvelope(core.Message{
		ID:     "msg-1",
		UserID: userID,
		Text:   text,
		Source: "test",
	})
}

// mockAccessor 模拟 SessionAccessor。
type mockAccessor struct {
	session *session.Session
}

func (m *mockAccessor) GetFromEnvelope(_ *core.Envelope) *session.Session {
	return m.session
}

// collectHandler 收集 Execute 调用参数。
type collectHandler struct {
	name         string
	adminOnly    bool
	executed     bool
	receivedEnv  *core.Envelope
	receivedArgs string
	result       *CommandResult
	err          error
}

func (h *collectHandler) Name() string        { return h.name }
func (h *collectHandler) Description() string { return "test handler for " + h.name }
func (h *collectHandler) AdminOnly() bool     { return h.adminOnly }
func (h *collectHandler) Execute(_ context.Context, env *core.Envelope, args string) (*CommandResult, error) {
	h.executed = true
	h.receivedEnv = env
	h.receivedArgs = args
	if h.err != nil {
		return nil, h.err
	}
	if h.result != nil {
		return h.result, nil
	}
	return &CommandResult{Reply: "ok", OK: true}, nil
}

// ============================================================================
// Parser tests
// ============================================================================

func TestParse(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		wantNil  bool
		wantName string
		wantArgs string
	}{
		{name: "simple", text: "/clear", wantName: "clear", wantArgs: ""},
		{name: "with args", text: "/compact 5", wantName: "compact", wantArgs: "5"},
		{name: "uppercase command", text: "/CLEAR", wantName: "clear", wantArgs: ""},
		{name: "mixed case", text: "/Help me", wantName: "help", wantArgs: "me"},
		{name: "leading space", text: "  /clear", wantName: "clear", wantArgs: ""},
		{name: "not a command", text: "hello world", wantNil: true},
		{name: "empty", text: "", wantNil: true},
		{name: "just slash", text: "/", wantNil: true},
		{name: "slash with space only", text: "/ ", wantNil: true},
		{name: "multiple args", text: "/foo bar baz", wantName: "foo", wantArgs: "bar baz"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := Parse(tt.text)
			if tt.wantNil {
				if result != nil {
					t.Errorf("Parse(%q) = %+v, want nil", tt.text, result)
				}
				return
			}
			if result == nil {
				t.Fatalf("Parse(%q) = nil, want non-nil", tt.text)
			}
			if result.Name != tt.wantName {
				t.Errorf("Name = %q, want %q", result.Name, tt.wantName)
			}
			if result.Args != tt.wantArgs {
				t.Errorf("Args = %q, want %q", result.Args, tt.wantArgs)
			}
		})
	}
}

// ============================================================================
// Registry tests
// ============================================================================

func TestRegistry_Register_Lookup(t *testing.T) {
	r := NewRegistry()
	h := &collectHandler{name: "test"}

	if err := r.Register(h); err != nil {
		t.Fatalf("Register: %v", err)
	}

	found, ok := r.Lookup("test")
	if !ok {
		t.Fatal("Lookup failed")
	}
	if found.Name() != "test" {
		t.Errorf("Lookup returned wrong handler: %s", found.Name())
	}

	// Case-insensitive lookup
	found, ok = r.Lookup("TEST")
	if !ok {
		t.Fatal("Case-insensitive lookup failed")
	}
	if found.Name() != "test" {
		t.Errorf("Lookup returned wrong handler: %s", found.Name())
	}
}

func TestRegistry_DuplicateRegister(t *testing.T) {
	r := NewRegistry()
	h := &collectHandler{name: "dup"}

	if err := r.Register(h); err != nil {
		t.Fatalf("First Register: %v", err)
	}
	if err := r.Register(h); err == nil {
		t.Fatal("Second Register should fail")
	}
}

func TestRegistry_List(t *testing.T) {
	r := NewRegistry()
	r.MustRegister(&collectHandler{name: "a"})
	r.MustRegister(&collectHandler{name: "b"})

	list := r.List()
	if len(list) != 2 {
		t.Errorf("List() = %d items, want 2", len(list))
	}
}

// ============================================================================
// Stage tests
// ============================================================================

func TestStage_NonCommand_PassesThrough(t *testing.T) {
	r := NewRegistry()
	stage := NewCommandStage("command", r, nil, testTP(), testLogger())

	env := makeEnvelope("hello world", "user1")
	result, err := stage.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if result == nil {
		t.Fatal("Process returned nil for non-command message")
	}
	if result.Aborted() {
		t.Error("Non-command message should not be aborted")
	}
}

func TestStage_UnknownCommand_PassesThrough(t *testing.T) {
	r := NewRegistry()
	stage := NewCommandStage("command", r, nil, testTP(), testLogger())

	env := makeEnvelope("/unknown", "user1")
	result, err := stage.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if result == nil {
		t.Fatal("Process returned nil for unknown command")
	}
	if result.Aborted() {
		t.Error("Unknown command should not be aborted")
	}
}

func TestStage_KnownCommand_ExecutedAndAborted(t *testing.T) {
	r := NewRegistry()
	h := &collectHandler{name: "clear"}
	r.MustRegister(h)

	stage := NewCommandStage("command", r, AllowAllChecker{}, testTP(), testLogger())

	env := makeEnvelope("/clear", "user1")
	result, err := stage.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if result == nil {
		t.Fatal("Process returned nil")
	}
	if !h.executed {
		t.Error("Handler was not executed")
	}
	if !result.Aborted() {
		t.Error("Envelope should be aborted after command execution")
	}

	actions := result.Actions()
	if len(actions) != 1 {
		t.Fatalf("Expected 1 action, got %d", len(actions))
	}
	if actions[0].Type != core.ActionReply {
		t.Errorf("Action type = %s, want %s", actions[0].Type, core.ActionReply)
	}
}

func TestStage_CommandWithArgs(t *testing.T) {
	r := NewRegistry()
	h := &collectHandler{name: "compact"}
	r.MustRegister(h)

	stage := NewCommandStage("command", r, AllowAllChecker{}, testTP(), testLogger())

	env := makeEnvelope("/compact 10", "user1")
	_, err := stage.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if h.receivedArgs != "10" {
		t.Errorf("Args = %q, want %q", h.receivedArgs, "10")
	}
}

func TestStage_AdminOnly_DeniedWithoutChecker(t *testing.T) {
	r := NewRegistry()
	h := &collectHandler{name: "clear", adminOnly: true}
	r.MustRegister(h)

	// No checker → denied
	stage := NewCommandStage("command", r, nil, testTP(), testLogger())

	env := makeEnvelope("/clear", "user1")
	result, _ := stage.Process(context.Background(), env)

	if h.executed {
		t.Error("Handler should not execute when admin check fails")
	}
	if !result.Aborted() {
		t.Error("Envelope should be aborted even on denial")
	}
	actions := result.Actions()
	if len(actions) != 1 {
		t.Fatalf("Expected 1 action (denial reply), got %d", len(actions))
	}
}

func TestStage_AdminOnly_AllowedWithChecker(t *testing.T) {
	r := NewRegistry()
	h := &collectHandler{name: "clear", adminOnly: true}
	r.MustRegister(h)

	stage := NewCommandStage("command", r, AllowAllChecker{}, testTP(), testLogger())

	env := makeEnvelope("/clear", "admin1")
	_, err := stage.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if !h.executed {
		t.Error("Handler should execute when admin check passes")
	}
}

func TestStage_AdminOnly_DeniedForNonAdmin(t *testing.T) {
	r := NewRegistry()
	h := &collectHandler{name: "clear", adminOnly: true}
	r.MustRegister(h)

	checker := AdminCheckerFunc(func(_ context.Context, _, userID string) bool {
		return userID == "admin"
	})
	stage := NewCommandStage("command", r, checker, testTP(), testLogger())

	env := makeEnvelope("/clear", "regular_user")
	result, _ := stage.Process(context.Background(), env)

	if h.executed {
		t.Error("Handler should not execute for non-admin user")
	}
	if !result.Aborted() {
		t.Error("Envelope should be aborted")
	}
}

func TestStage_HandlerError(t *testing.T) {
	r := NewRegistry()
	h := &collectHandler{
		name: "fail",
		err:  errors.New("something went wrong"),
	}
	r.MustRegister(h)

	stage := NewCommandStage("command", r, AllowAllChecker{}, testTP(), testLogger())

	env := makeEnvelope("/fail", "user1")
	result, err := stage.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("Process should not return error for handler failure: %v", err)
	}
	if !result.Aborted() {
		t.Error("Envelope should be aborted on handler error")
	}
}

// ============================================================================
// Built-in handler tests
// ============================================================================

func TestClearHandler_NoSession(t *testing.T) {
	h := NewClearHandler(nil)
	env := makeEnvelope("/clear", "user1")
	result, err := h.Execute(context.Background(), env, "")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.OK {
		t.Error("Should not be OK when accessor is nil")
	}
}

func TestClearHandler_WithSession(t *testing.T) {
	s := session.NewSession("s1", "bot1", "ch1")
	s.AppendMessage(session.Message{Role: "user", Text: "hello"})
	s.AppendMessage(session.Message{Role: "assistant", Text: "hi"})

	h := NewClearHandler(&mockAccessor{session: s})
	env := makeEnvelope("/clear", "user1")
	result, err := h.Execute(context.Background(), env, "")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !result.OK {
		t.Error("Should be OK")
	}
	// Check actual messages are cleared
	msgs := s.Messages()
	if len(msgs) != 0 {
		t.Errorf("Messages after clear = %d, want 0", len(msgs))
	}
}

func TestCompactHandler(t *testing.T) {
	s := session.NewSession("s1", "bot1", "ch1")
	for range 10 {
		s.AppendMessage(session.Message{Role: "user", Text: "msg"})
	}

	h := NewCompactHandler(&mockAccessor{session: s}, 3)
	env := makeEnvelope("/compact", "user1")
	result, err := h.Execute(context.Background(), env, "")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !result.OK {
		t.Error("Should be OK")
	}
	msgs := s.Messages()
	if len(msgs) != 3 {
		t.Errorf("Messages after compact = %d, want 3", len(msgs))
	}
}

func TestCompactHandler_WithArgs(t *testing.T) {
	s := session.NewSession("s1", "bot1", "ch1")
	for range 10 {
		s.AppendMessage(session.Message{Role: "user", Text: "msg"})
	}

	h := NewCompactHandler(&mockAccessor{session: s}, 3)
	env := makeEnvelope("/compact 5", "user1")
	result, err := h.Execute(context.Background(), env, "5")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !result.OK {
		t.Error("Should be OK")
	}
	msgs := s.Messages()
	if len(msgs) != 5 {
		t.Errorf("Messages after compact with args = %d, want 5", len(msgs))
	}
}

func TestHelpHandler(t *testing.T) {
	r := NewRegistry()
	r.MustRegister(NewClearHandler(nil))
	r.MustRegister(NewCompactHandler(nil, 3))

	h := NewHelpHandler(r)
	env := makeEnvelope("/help", "user1")
	result, err := h.Execute(context.Background(), env, "")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !result.OK {
		t.Error("Should be OK")
	}
	if result.Reply == "" {
		t.Error("Reply should not be empty")
	}
}

func TestStatusHandler(t *testing.T) {
	s := session.NewSession("s1", "bot1", "ch1")
	s.AppendMessage(session.Message{Role: "user", Text: "hi"})

	h := NewStatusHandler(&mockAccessor{session: s})
	env := makeEnvelope("/status", "user1")
	result, err := h.Execute(context.Background(), env, "")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !result.OK {
		t.Error("Should be OK")
	}
	if result.Reply == "" {
		t.Error("Reply should not be empty")
	}
}

// ============================================================================
// AdminChecker tests
// ============================================================================

func TestStaticAdminChecker(t *testing.T) {
	c := NewStaticAdminChecker("test:admin1", "test:admin2")
	if !c.IsAdmin(context.Background(), "test", "admin1") {
		t.Error("admin1 should be admin")
	}
	if c.IsAdmin(context.Background(), "test", "user1") {
		t.Error("user1 should not be admin")
	}
}

func TestAllowAllChecker(t *testing.T) {
	c := AllowAllChecker{}
	if !c.IsAdmin(context.Background(), "test", "anyone") {
		t.Error("AllowAllChecker should allow everyone")
	}
}

// ============================================================================
// Integration: Stage + Builtins
// ============================================================================

func TestStageWithBuiltins_Clear(t *testing.T) {
	s := session.NewSession("s1", "bot1", "ch1")
	s.AppendMessage(session.Message{Role: "user", Text: "hello"})

	accessor := &mockAccessor{session: s}
	r := NewRegistry()
	RegisterBuiltins(r, accessor, 3)

	stage := NewCommandStage("command", r, AllowAllChecker{}, testTP(), testLogger())

	env := makeEnvelope("/clear", "admin")
	result, err := stage.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if !result.Aborted() {
		t.Error("Should abort pipeline")
	}
	actions := result.Actions()
	if len(actions) != 1 {
		t.Fatalf("Expected 1 action, got %d", len(actions))
	}
}

func TestNewCommandStageWithBuiltins(t *testing.T) {
	mgr := session.NewSessionManager(
		session.NewDefaultResolver("test"),
		session.DefaultManagerConfig(),
		testTP(),
		testLogger(),
	)

	stage := NewCommandStageWithBuiltins(
		AllowAllChecker{},
		mgr,
		session.NewDefaultResolver("test"),
		3,
		testTP(),
		testLogger(),
	)

	commands := stage.Registry().List()
	if len(commands) < 3 {
		t.Errorf("Expected at least 3 builtins, got %d", len(commands))
	}

	// Verify /help works
	env := makeEnvelope("/help", "user1")
	result, err := stage.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("Process /help: %v", err)
	}
	if !result.Aborted() {
		t.Error("Should abort pipeline")
	}
	actions := result.Actions()
	if len(actions) != 1 {
		t.Fatalf("Expected 1 action, got %d", len(actions))
	}
}
