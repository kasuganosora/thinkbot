package auth

import (
	"context"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/kasuganosora/thinkbot/dao"
)

// newTestDB 创建内存 SQLite 数据库并迁移 users 表。
func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Discard,
	})
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	if err := db.AutoMigrate(&dao.User{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

// --- password tests ---

func TestHashPassword_VerifyPassword(t *testing.T) {
	hash, err := HashPassword("secret123")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if hash == "" {
		t.Fatal("hash is empty")
	}
	if hash == "secret123" {
		t.Fatal("hash equals plaintext")
	}

	if !VerifyPassword(hash, "secret123") {
		t.Fatal("VerifyPassword should return true for correct password")
	}
	if VerifyPassword(hash, "wrong") {
		t.Fatal("VerifyPassword should return false for wrong password")
	}
}

// --- role / permission tests ---

func TestHasPermission(t *testing.T) {
	tests := []struct {
		role, perm string
		want       bool
	}{
		{RoleAdmin, PermBotCreate, true},
		{RoleAdmin, PermBotManage, true},
		{RoleAdmin, PermUserManage, true},
		{RoleAdmin, PermBotUse, true},
		{RoleAdmin, PermSystemConfig, true},
		{RoleMember, PermBotCreate, false},
		{RoleMember, PermBotManage, false},
		{RoleMember, PermUserManage, false},
		{RoleMember, PermBotUse, true},
		{RoleMember, PermSystemConfig, false},
		{"unknown", PermBotUse, false},
	}
	for _, tt := range tests {
		got := HasPermission(tt.role, tt.perm)
		if got != tt.want {
			t.Errorf("HasPermission(%q, %q) = %v, want %v", tt.role, tt.perm, got, tt.want)
		}
	}
}

func TestIsValidRole(t *testing.T) {
	if !IsValidRole(RoleAdmin) {
		t.Error("admin should be valid")
	}
	if !IsValidRole(RoleMember) {
		t.Error("member should be valid")
	}
	if IsValidRole("superuser") {
		t.Error("superuser should be invalid")
	}
}

// --- service tests ---

func TestCreateUser_Success(t *testing.T) {
	db := newTestDB(t)
	svc := New(db)
	ctx := context.Background()

	user, err := svc.CreateUser(ctx, CreateUserInput{
		Username:    "alice",
		Password:    "password123",
		Email:       "alice@example.com",
		Role:        RoleAdmin,
		DisplayName: "Alice",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if user.ID == 0 {
		t.Error("ID should be set")
	}
	if user.Username != "alice" {
		t.Errorf("Username = %q, want alice", user.Username)
	}
	if user.Role != RoleAdmin {
		t.Errorf("Role = %q, want admin", user.Role)
	}
	if user.PasswordHash == "" || user.PasswordHash == "password123" {
		t.Error("PasswordHash should be hashed")
	}
	if user.Status != StatusActive {
		t.Errorf("Status = %q, want active", user.Status)
	}
}

func TestCreateUser_Duplicate(t *testing.T) {
	db := newTestDB(t)
	svc := New(db)
	ctx := context.Background()

	_, err := svc.CreateUser(ctx, CreateUserInput{
		Username: "bob",
		Password: "password123",
	})
	if err != nil {
		t.Fatalf("first CreateUser: %v", err)
	}

	_, err = svc.CreateUser(ctx, CreateUserInput{
		Username: "bob",
		Password: "password456",
	})
	if err == nil {
		t.Fatal("second CreateUser with same username should fail")
	}
}

func TestCreateUser_DefaultRole(t *testing.T) {
	db := newTestDB(t)
	svc := New(db)
	ctx := context.Background()

	user, err := svc.CreateUser(ctx, CreateUserInput{
		Username: "carol",
		Password: "password123",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if user.Role != RoleMember {
		t.Errorf("Role = %q, want member (default)", user.Role)
	}
}

func TestCreateUser_InvalidRole(t *testing.T) {
	db := newTestDB(t)
	svc := New(db)
	ctx := context.Background()

	_, err := svc.CreateUser(ctx, CreateUserInput{
		Username: "dave",
		Password: "password123",
		Role:     "superuser",
	})
	if err == nil {
		t.Fatal("should fail with invalid role")
	}
}

func TestCreateUser_ShortPassword(t *testing.T) {
	db := newTestDB(t)
	svc := New(db)
	ctx := context.Background()

	_, err := svc.CreateUser(ctx, CreateUserInput{
		Username: "eve",
		Password: "123",
	})
	if err == nil {
		t.Fatal("should fail with short password")
	}
}

func TestCreateUser_EmptyUsername(t *testing.T) {
	db := newTestDB(t)
	svc := New(db)
	ctx := context.Background()

	_, err := svc.CreateUser(ctx, CreateUserInput{
		Username: "  ",
		Password: "password123",
	})
	if err == nil {
		t.Fatal("should fail with empty username")
	}
}

func TestAuthenticate_Success(t *testing.T) {
	db := newTestDB(t)
	svc := New(db)
	ctx := context.Background()

	_, err := svc.CreateUser(ctx, CreateUserInput{
		Username: "frank",
		Password: "mypassword",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	user, err := svc.Authenticate(ctx, "frank", "mypassword")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if user.Username != "frank" {
		t.Errorf("Username = %q, want frank", user.Username)
	}
	if user.LastLoginAt == nil {
		t.Error("LastLoginAt should be set after authentication")
	}
}

func TestAuthenticate_WrongPassword(t *testing.T) {
	db := newTestDB(t)
	svc := New(db)
	ctx := context.Background()

	_, err := svc.CreateUser(ctx, CreateUserInput{
		Username: "grace",
		Password: "correctpass",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	_, err = svc.Authenticate(ctx, "grace", "wrongpass")
	if err != ErrInvalidCredentials {
		t.Errorf("Authenticate err = %v, want ErrInvalidCredentials", err)
	}
}

func TestAuthenticate_UserNotFound(t *testing.T) {
	db := newTestDB(t)
	svc := New(db)
	ctx := context.Background()

	_, err := svc.Authenticate(ctx, "nobody", "whatever")
	if err != ErrInvalidCredentials {
		t.Errorf("Authenticate err = %v, want ErrInvalidCredentials", err)
	}
}

func TestAuthenticate_DisabledUser(t *testing.T) {
	db := newTestDB(t)
	svc := New(db)
	ctx := context.Background()

	user, err := svc.CreateUser(ctx, CreateUserInput{
		Username: "henry",
		Password: "password123",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	if err := svc.DisableUser(ctx, user.ID); err != nil {
		t.Fatalf("DisableUser: %v", err)
	}

	_, err = svc.Authenticate(ctx, "henry", "password123")
	if err != ErrUserDisabled {
		t.Errorf("Authenticate err = %v, want ErrUserDisabled", err)
	}
}

func TestGetUser(t *testing.T) {
	db := newTestDB(t)
	svc := New(db)
	ctx := context.Background()

	created, err := svc.CreateUser(ctx, CreateUserInput{
		Username: "ivan",
		Password: "password123",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	user, err := svc.GetUser(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	if user.Username != "ivan" {
		t.Errorf("Username = %q, want ivan", user.Username)
	}
}

func TestGetUser_NotFound(t *testing.T) {
	db := newTestDB(t)
	svc := New(db)
	ctx := context.Background()

	_, err := svc.GetUser(ctx, 99999)
	if err != ErrUserNotFound {
		t.Errorf("GetUser err = %v, want ErrUserNotFound", err)
	}
}

func TestGetUserByUsername(t *testing.T) {
	db := newTestDB(t)
	svc := New(db)
	ctx := context.Background()

	_, err := svc.CreateUser(ctx, CreateUserInput{
		Username: "judy",
		Password: "password123",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	user, err := svc.GetUserByUsername(ctx, "judy")
	if err != nil {
		t.Fatalf("GetUserByUsername: %v", err)
	}
	if user.Username != "judy" {
		t.Errorf("Username = %q, want judy", user.Username)
	}
}

func TestListUsers(t *testing.T) {
	db := newTestDB(t)
	svc := New(db)
	ctx := context.Background()

	for _, name := range []string{"user1", "user2", "user3"} {
		_, err := svc.CreateUser(ctx, CreateUserInput{
			Username: name,
			Password: "password123",
		})
		if err != nil {
			t.Fatalf("CreateUser %s: %v", name, err)
		}
	}

	users, err := svc.ListUsers(ctx)
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if len(users) != 3 {
		t.Errorf("len(users) = %d, want 3", len(users))
	}
}

func TestUpdateRole(t *testing.T) {
	db := newTestDB(t)
	svc := New(db)
	ctx := context.Background()

	user, err := svc.CreateUser(ctx, CreateUserInput{
		Username: "kate",
		Password: "password123",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	if err := svc.UpdateRole(ctx, user.ID, RoleAdmin); err != nil {
		t.Fatalf("UpdateRole: %v", err)
	}

	updated, _ := svc.GetUser(ctx, user.ID)
	if updated.Role != RoleAdmin {
		t.Errorf("Role = %q, want admin", updated.Role)
	}
}

func TestUpdateRole_Invalid(t *testing.T) {
	db := newTestDB(t)
	svc := New(db)
	ctx := context.Background()

	user, _ := svc.CreateUser(ctx, CreateUserInput{
		Username: "leo",
		Password: "password123",
	})

	err := svc.UpdateRole(ctx, user.ID, "superuser")
	if err != ErrInvalidRole {
		t.Errorf("UpdateRole err = %v, want ErrInvalidRole", err)
	}
}

func TestUpdatePassword(t *testing.T) {
	db := newTestDB(t)
	svc := New(db)
	ctx := context.Background()

	user, _ := svc.CreateUser(ctx, CreateUserInput{
		Username: "mia",
		Password: "oldpassword",
	})

	if err := svc.UpdatePassword(ctx, user.ID, "newpassword"); err != nil {
		t.Fatalf("UpdatePassword: %v", err)
	}

	// 旧密码应失效
	_, err := svc.Authenticate(ctx, "mia", "oldpassword")
	if err != ErrInvalidCredentials {
		t.Errorf("old password should fail: err = %v", err)
	}

	// 新密码应生效
	_, err = svc.Authenticate(ctx, "mia", "newpassword")
	if err != nil {
		t.Errorf("new password should work: %v", err)
	}
}

func TestDisableEnableUser(t *testing.T) {
	db := newTestDB(t)
	svc := New(db)
	ctx := context.Background()

	user, _ := svc.CreateUser(ctx, CreateUserInput{
		Username: "nina",
		Password: "password123",
	})

	if err := svc.DisableUser(ctx, user.ID); err != nil {
		t.Fatalf("DisableUser: %v", err)
	}
	disabled, _ := svc.GetUser(ctx, user.ID)
	if disabled.Status != StatusDisabled {
		t.Errorf("Status = %q, want disabled", disabled.Status)
	}

	if err := svc.EnableUser(ctx, user.ID); err != nil {
		t.Fatalf("EnableUser: %v", err)
	}
	enabled, _ := svc.GetUser(ctx, user.ID)
	if enabled.Status != StatusActive {
		t.Errorf("Status = %q, want active", enabled.Status)
	}
}

func TestDeleteUser(t *testing.T) {
	db := newTestDB(t)
	svc := New(db)
	ctx := context.Background()

	user, _ := svc.CreateUser(ctx, CreateUserInput{
		Username: "oscar",
		Password: "password123",
	})

	if err := svc.DeleteUser(ctx, user.ID); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}

	_, err := svc.GetUser(ctx, user.ID)
	if err != ErrUserNotFound {
		t.Errorf("GetUser after delete err = %v, want ErrUserNotFound", err)
	}
}

func TestUpdateProfile(t *testing.T) {
	db := newTestDB(t)
	svc := New(db)
	ctx := context.Background()

	user, _ := svc.CreateUser(ctx, CreateUserInput{
		Username: "paul",
		Password: "password123",
	})

	email := "paul@new.com"
	name := "Paul Smith"
	if err := svc.UpdateProfile(ctx, user.ID, UpdateProfileInput{
		Email:       &email,
		DisplayName: &name,
	}); err != nil {
		t.Fatalf("UpdateProfile: %v", err)
	}

	updated, _ := svc.GetUser(ctx, user.ID)
	if updated.Email != email {
		t.Errorf("Email = %q, want %q", updated.Email, email)
	}
	if updated.DisplayName != name {
		t.Errorf("DisplayName = %q, want %q", updated.DisplayName, name)
	}
}

func TestCan(t *testing.T) {
	db := newTestDB(t)
	svc := New(db)
	ctx := context.Background()

	admin, _ := svc.CreateUser(ctx, CreateUserInput{
		Username: "admin1",
		Password: "password123",
		Role:     RoleAdmin,
	})
	member, _ := svc.CreateUser(ctx, CreateUserInput{
		Username: "member1",
		Password: "password123",
		Role:     RoleMember,
	})

	if !svc.Can(admin, PermBotCreate) {
		t.Error("admin should have bot.create")
	}
	if svc.Can(member, PermBotCreate) {
		t.Error("member should NOT have bot.create")
	}
	if !svc.Can(member, PermBotUse) {
		t.Error("member should have bot.use")
	}

	// nil user
	if svc.Can(nil, PermBotUse) {
		t.Error("nil user should have no permissions")
	}

	// disabled admin
	_ = svc.DisableUser(ctx, admin.ID)
	disabledAdmin, _ := svc.GetUser(ctx, admin.ID)
	if svc.Can(disabledAdmin, PermBotCreate) {
		t.Error("disabled admin should have no permissions")
	}
}
