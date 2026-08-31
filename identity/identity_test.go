package identity

import (
	"context"
	"testing"
	"time"

	"go.uber.org/zap"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/dao"
)

// ============================================================================
// Test helpers
// ============================================================================

func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	if err := db.AutoMigrate(&dao.User{}, &dao.BindCode{}, &dao.IdentityMapping{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func newTestService(t *testing.T) *BindService {
	t.Helper()
	return New(newTestDB(t), &logAdapter{l: zap.NewNop().Sugar()})
}

func seedUser(t *testing.T, db *gorm.DB, username, role string) *dao.User {
	t.Helper()
	u := &dao.User{
		Username:     username,
		PasswordHash: "$2a$10$dummy",
		Role:         role,
		Status:       "active",
	}
	if err := db.Create(u).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}
	return u
}

func makeEnv(text, source, userID string) *core.Envelope {
	return core.NewEnvelope(core.Message{
		ID:     "msg-1",
		UserID: userID,
		Text:   text,
		Source: source,
	})
}

// ============================================================================
// Code generation tests
// ============================================================================

func TestGenerateCode_Format(t *testing.T) {
	for range 100 {
		code := generateCode()
		if !bindCodeRegex.MatchString(code) {
			t.Errorf("generated code %q does not match expected format", code)
		}
		if code[:3] != "TB-" {
			t.Errorf("code should start with TB-, got %q", code[:3])
		}
	}
}

func TestIsBindCode(t *testing.T) {
	tests := []struct {
		text string
		want bool
	}{
		{"TB-ABCD-EFGH", true},
		{"tb-abcd-efgh", true},
		{"TB-ABCD-EFG", false},     // too short
		{"TB-ABCDE-EFGH", false},   // too long segment
		{"  TB-ABCD-EFGH  ", true}, // with spaces
		{"hello world", false},
		{"", false},
		{"TB-1234-5678", false}, // contains 1 (not in safe alphabet)
		{"TB-2345-6789", true},
		{"TB-O1I-LEFG", false}, // O, 1, I, L not in alphabet
	}
	for _, tt := range tests {
		got := IsBindCode(tt.text)
		if got != tt.want {
			t.Errorf("IsBindCode(%q) = %v, want %v", tt.text, got, tt.want)
		}
	}
}

func TestNormalizeCode(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"TB-ABCD-EFGH", "TB-ABCD-EFGH"},
		{"tb-abcd-efgh", "TB-ABCD-EFGH"},
		{"TB.ABCD.EFGH", "TB-ABCD-EFGH"},
		{"TB ABCD EFGH", "TB-ABCD-EFGH"},
		{"  tb-abcd-efgh  ", "TB-ABCD-EFGH"},
		{"invalid", ""},
		{"", ""},
	}
	for _, tt := range tests {
		got := NormalizeCode(tt.input)
		if got != tt.want {
			t.Errorf("NormalizeCode(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestExtractPlatform(t *testing.T) {
	tests := []struct {
		source string
		want   string
	}{
		{"telegram-bot1", "telegram"},
		{"misskey-bot1", "misskey"},
		{"web-bot1", "web"},
		{"discord-mybot", "discord"},
		{"unknown", "unknown"},
	}
	for _, tt := range tests {
		got := extractPlatform(tt.source)
		if got != tt.want {
			t.Errorf("extractPlatform(%q) = %q, want %q", tt.source, got, tt.want)
		}
	}
}

// ============================================================================
// BindService tests
// ============================================================================

func TestGenerateCode_CreatesValidCode(t *testing.T) {
	svc := newTestService(t)
	db := svc.db
	u := seedUser(t, db, "alice", "member")

	code, err := svc.GenerateCode(context.Background(), u.ID)
	if err != nil {
		t.Fatalf("GenerateCode: %v", err)
	}
	if code.Code == "" {
		t.Fatal("Code should not be empty")
	}
	if code.UserID != u.ID {
		t.Errorf("UserID = %d, want %d", code.UserID, u.ID)
	}
	if code.UsedAt != nil {
		t.Error("UsedAt should be nil for new code")
	}
	// ExpiresAt should be ~5 minutes in the future
	diff := time.Until(code.ExpiresAt)
	if diff < 4*time.Minute || diff > 6*time.Minute {
		t.Errorf("ExpiresAt diff = %v, want ~5 min", diff)
	}
}

func TestGenerateCode_MultipleCodesAllowed(t *testing.T) {
	svc := newTestService(t)
	db := svc.db
	u := seedUser(t, db, "alice", "member")

	code1, _ := svc.GenerateCode(context.Background(), u.ID)
	code2, _ := svc.GenerateCode(context.Background(), u.ID)

	if code1.Code == code2.Code {
		t.Error("Two codes should not be identical")
	}
}

func TestConsumeCode_Success(t *testing.T) {
	svc := newTestService(t)
	db := svc.db
	u := seedUser(t, db, "alice", "member")

	code, _ := svc.GenerateCode(context.Background(), u.ID)

	result, err := svc.ConsumeCode(context.Background(), code.Code, "telegram", "123456789")
	if err != nil {
		t.Fatalf("ConsumeCode: %v", err)
	}
	if result.Mapping.UserID != u.ID {
		t.Errorf("Mapping UserID = %d, want %d", result.Mapping.UserID, u.ID)
	}
	if result.Mapping.Platform != "telegram" {
		t.Errorf("Mapping Platform = %q, want telegram", result.Mapping.Platform)
	}
	if result.Mapping.PlatformUserID != "123456789" {
		t.Errorf("Mapping PlatformUserID = %q, want 123456789", result.Mapping.PlatformUserID)
	}
	if result.Username != "alice" {
		t.Errorf("Username = %q, want alice", result.Username)
	}

	// Verify code is marked as used
	var dbCode dao.BindCode
	db.First(&dbCode, code.ID)
	if dbCode.UsedAt == nil {
		t.Error("Code should be marked as used")
	}
}

func TestConsumeCode_AlreadyUsed(t *testing.T) {
	svc := newTestService(t)
	db := svc.db
	u := seedUser(t, db, "alice", "member")

	code, _ := svc.GenerateCode(context.Background(), u.ID)

	// First use - success
	_, err := svc.ConsumeCode(context.Background(), code.Code, "telegram", "123")
	if err != nil {
		t.Fatalf("First ConsumeCode: %v", err)
	}

	// Second use - should fail
	_, err = svc.ConsumeCode(context.Background(), code.Code, "telegram", "456")
	if err == nil {
		t.Fatal("Second ConsumeCode should fail")
	}
}

func TestConsumeCode_Expired(t *testing.T) {
	svc := newTestService(t)
	db := svc.db
	u := seedUser(t, db, "alice", "member")

	// Create an expired code directly
	expired := &dao.BindCode{
		UserID:    u.ID,
		Code:      generateCode(),
		ExpiresAt: time.Now().Add(-1 * time.Minute),
	}
	db.Create(expired)

	_, err := svc.ConsumeCode(context.Background(), expired.Code, "telegram", "123")
	if err == nil {
		t.Fatal("ConsumeCode with expired code should fail")
	}
}

func TestConsumeCode_NotFound(t *testing.T) {
	svc := newTestService(t)

	_, err := svc.ConsumeCode(context.Background(), "TB-XXXX-YYYY", "telegram", "123")
	if err == nil {
		t.Fatal("ConsumeCode with non-existent code should fail")
	}
}

func TestConsumeCode_AlreadyBound_DifferentUser(t *testing.T) {
	svc := newTestService(t)
	db := svc.db
	u1 := seedUser(t, db, "alice", "member")
	u2 := seedUser(t, db, "bob", "member")

	// Alice binds first
	code1, _ := svc.GenerateCode(context.Background(), u1.ID)
	_, err := svc.ConsumeCode(context.Background(), code1.Code, "telegram", "123456789")
	if err != nil {
		t.Fatalf("Alice bind: %v", err)
	}

	// Bob tries to bind the same platform account
	code2, _ := svc.GenerateCode(context.Background(), u2.ID)
	_, err = svc.ConsumeCode(context.Background(), code2.Code, "telegram", "123456789")
	if err == nil {
		t.Fatal("Bob should not be able to bind account already bound to Alice")
	}
}

func TestConsumeCode_Idempotent_SameUser(t *testing.T) {
	svc := newTestService(t)
	db := svc.db
	u := seedUser(t, db, "alice", "member")

	// Alice binds
	code1, _ := svc.GenerateCode(context.Background(), u.ID)
	result1, err := svc.ConsumeCode(context.Background(), code1.Code, "telegram", "123")
	if err != nil {
		t.Fatalf("First bind: %v", err)
	}

	// Alice binds again with same platform account (idempotent)
	code2, _ := svc.GenerateCode(context.Background(), u.ID)
	result2, err := svc.ConsumeCode(context.Background(), code2.Code, "telegram", "123")
	if err != nil {
		t.Fatalf("Idempotent bind: %v", err)
	}
	if result2.Mapping.ID != result1.Mapping.ID {
		t.Error("Should return existing mapping")
	}
}

func TestResolveMapping_Success(t *testing.T) {
	svc := newTestService(t)
	db := svc.db
	u := seedUser(t, db, "alice", "member")

	code, _ := svc.GenerateCode(context.Background(), u.ID)
	if _, err := svc.ConsumeCode(context.Background(), code.Code, "misskey", "misskey-user-123"); err != nil {
		t.Fatalf("ConsumeCode: %v", err)
	}

	mapping, err := svc.ResolveMapping(context.Background(), "misskey", "misskey-user-123")
	if err != nil {
		t.Fatalf("ResolveMapping: %v", err)
	}
	if mapping == nil {
		t.Fatal("Mapping should not be nil")
	}
	if mapping.UserID != u.ID {
		t.Errorf("UserID = %d, want %d", mapping.UserID, u.ID)
	}
}

func TestResolveMapping_NotFound(t *testing.T) {
	svc := newTestService(t)

	mapping, err := svc.ResolveMapping(context.Background(), "telegram", "nonexistent")
	if err != nil {
		t.Fatalf("ResolveMapping should not error: %v", err)
	}
	if mapping != nil {
		t.Error("Mapping should be nil for non-existent")
	}
}

func TestListBindings(t *testing.T) {
	svc := newTestService(t)
	db := svc.db
	u := seedUser(t, db, "alice", "member")

	// Bind two platforms
	code1, _ := svc.GenerateCode(context.Background(), u.ID)
	if _, err := svc.ConsumeCode(context.Background(), code1.Code, "telegram", "111"); err != nil {
		t.Fatalf("ConsumeCode telegram: %v", err)
	}

	code2, _ := svc.GenerateCode(context.Background(), u.ID)
	if _, err := svc.ConsumeCode(context.Background(), code2.Code, "misskey", "222"); err != nil {
		t.Fatalf("ConsumeCode misskey: %v", err)
	}

	bindings, err := svc.ListBindings(context.Background(), u.ID)
	if err != nil {
		t.Fatalf("ListBindings: %v", err)
	}
	if len(bindings) != 2 {
		t.Errorf("Expected 2 bindings, got %d", len(bindings))
	}
}

func TestDeleteBinding(t *testing.T) {
	svc := newTestService(t)
	db := svc.db
	u := seedUser(t, db, "alice", "member")

	code, _ := svc.GenerateCode(context.Background(), u.ID)
	if _, err := svc.ConsumeCode(context.Background(), code.Code, "telegram", "111"); err != nil {
		t.Fatalf("ConsumeCode: %v", err)
	}

	// Delete
	err := svc.DeleteBinding(context.Background(), u.ID, "telegram", "111")
	if err != nil {
		t.Fatalf("DeleteBinding: %v", err)
	}

	// Verify deleted
	mapping, _ := svc.ResolveMapping(context.Background(), "telegram", "111")
	if mapping != nil {
		t.Error("Mapping should be nil after deletion")
	}
}

func TestDeleteBinding_NotFound(t *testing.T) {
	svc := newTestService(t)
	db := svc.db
	u := seedUser(t, db, "alice", "member")

	err := svc.DeleteBinding(context.Background(), u.ID, "telegram", "nonexistent")
	if err == nil {
		t.Fatal("DeleteBinding should fail for non-existent")
	}
}

// ============================================================================
// BindStage tests
// ============================================================================

func TestBindStage_NonCode_PassesThrough(t *testing.T) {
	svc := newTestService(t)
	stage := NewBindStage(svc, nil, zap.NewNop().Sugar())

	env := makeEnv("hello world", "telegram-bot1", "123")
	result, err := stage.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if result == nil {
		t.Fatal("Process returned nil")
	}
	if result.Aborted() {
		t.Error("Non-code message should not be aborted")
	}
}

func TestBindStage_ValidCode_Aborts(t *testing.T) {
	svc := newTestService(t)
	db := svc.db
	u := seedUser(t, db, "alice", "admin")

	code, _ := svc.GenerateCode(context.Background(), u.ID)

	stage := NewBindStage(svc, nil, zap.NewNop().Sugar())
	env := makeEnv(code.Code, "telegram-bot1", "123456789")
	result, err := stage.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if !result.Aborted() {
		t.Error("Valid bind code should abort pipeline")
	}

	actions := result.Actions()
	if len(actions) != 1 {
		t.Fatalf("Expected 1 action, got %d", len(actions))
	}
	if actions[0].Type != core.ActionReply {
		t.Errorf("Action type = %s, want %s", actions[0].Type, core.ActionReply)
	}
}

func TestBindStage_InvalidCode_AbortsWithError(t *testing.T) {
	svc := newTestService(t)
	stage := NewBindStage(svc, nil, zap.NewNop().Sugar())

	env := makeEnv("TB-XXXX-YYYY", "telegram-bot1", "123456789")
	result, err := stage.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if !result.Aborted() {
		t.Error("Invalid bind code should abort pipeline")
	}

	actions := result.Actions()
	if len(actions) != 1 {
		t.Fatalf("Expected 1 action, got %d", len(actions))
	}
}

// ============================================================================
// AdminChecker tests
// ============================================================================

func TestStaticAdminChecker(t *testing.T) {
	c := NewStaticAdminChecker("telegram:123456789", "misskey:abc")
	if !c.IsAdmin(context.Background(), "telegram-bot1", "123456789") {
		t.Error("telegram:123456789 should be admin")
	}
	if c.IsAdmin(context.Background(), "telegram-bot1", "999") {
		t.Error("telegram:999 should not be admin")
	}
}

func TestAllowAllChecker(t *testing.T) {
	c := AllowAllChecker{}
	if !c.IsAdmin(context.Background(), "any-source", "anyone") {
		t.Error("AllowAllChecker should allow everyone")
	}
}

func TestDenyAllChecker(t *testing.T) {
	c := DenyAllChecker{}
	if c.IsAdmin(context.Background(), "any-source", "anyone") {
		t.Error("DenyAllChecker should deny everyone")
	}
}
