package auth

import (
	"context"
	"errors"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/kasuganosora/thinkbot/dao"
	"github.com/kasuganosora/thinkbot/util/errs"
)

// ============================================================================
// AuthService — 用户管理 & 认证服务
// ============================================================================

// User status 常量。
const (
	StatusActive   = "active"
	StatusDisabled = "disabled"
)

// 领域错误。
var (
	ErrUserNotFound       = errors.New("auth: user not found")
	ErrUserExists         = errors.New("auth: user already exists")
	ErrInvalidCredentials = errors.New("auth: invalid credentials")
	ErrUserDisabled       = errors.New("auth: user is disabled")
	ErrInvalidRole        = errors.New("auth: invalid role")
)

// AuthService 提供用户 CRUD 和认证能力。
type AuthService struct {
	db *gorm.DB
}

// New 创建 AuthService。
func New(db *gorm.DB) *AuthService {
	return &AuthService{db: db}
}

// ----------------------------------------------------------------------------
// 创建用户
// ----------------------------------------------------------------------------

// CreateUserInput 创建用户参数。
type CreateUserInput struct {
	Username    string
	Password    string
	Email       string // 可选
	Role        string // admin | member，空时默认 member
	DisplayName string // 可选
}

// CreateUser 创建一个新用户。
func (s *AuthService) CreateUser(ctx context.Context, input CreateUserInput) (*dao.User, error) {
	input.Username = strings.TrimSpace(input.Username)
	if input.Username == "" {
		return nil, errs.BadRequest("username is required")
	}
	if len(input.Password) < 6 {
		return nil, errs.BadRequest("password must be at least 6 characters")
	}

	role := input.Role
	if role == "" {
		role = RoleMember
	}
	if !IsValidRole(role) {
		return nil, errs.BadRequest("invalid role: " + role)
	}

	// 检查用户名是否已存在
	var count int64
	if err := s.db.WithContext(ctx).Model(&dao.User{}).
		Where("username = ?", input.Username).
		Count(&count).Error; err != nil {
		return nil, errs.Wrap(err, "auth: check username existence")
	}
	if count > 0 {
		return nil, errs.Conflict("username already exists")
	}

	// 哈希密码
	hash, err := HashPassword(input.Password)
	if err != nil {
		return nil, errs.Wrap(err, "auth: hash password")
	}

	user := &dao.User{
		Username:     input.Username,
		Email:        strings.TrimSpace(input.Email),
		PasswordHash: hash,
		Role:         role,
		Status:       StatusActive,
		DisplayName:  strings.TrimSpace(input.DisplayName),
	}

	if err := s.db.WithContext(ctx).Create(user).Error; err != nil {
		return nil, errs.Wrap(err, "auth: create user")
	}

	return user, nil
}

// ----------------------------------------------------------------------------
// 认证（登录）
// ----------------------------------------------------------------------------

// Authenticate 验证用户名+密码，返回用户信息。
// 成功时自动更新 LastLoginAt。
func (s *AuthService) Authenticate(ctx context.Context, username, password string) (*dao.User, error) {
	var user dao.User
	err := s.db.WithContext(ctx).Where("username = ?", strings.TrimSpace(username)).First(&user).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrInvalidCredentials
		}
		return nil, errs.Wrap(err, "auth: query user")
	}

	if !VerifyPassword(user.PasswordHash, password) {
		return nil, ErrInvalidCredentials
	}

	if user.Status != StatusActive {
		return nil, ErrUserDisabled
	}

	// 更新最后登录时间（失败不影响登录流程）
	now := time.Now()
	if err := s.db.WithContext(ctx).Model(&user).Update("last_login_at", &now).Error; err == nil {
		user.LastLoginAt = &now
	}

	return &user, nil
}

// ----------------------------------------------------------------------------
// 查询
// ----------------------------------------------------------------------------

// GetUser 根据 ID 获取用户。
func (s *AuthService) GetUser(ctx context.Context, id uint) (*dao.User, error) {
	var user dao.User
	err := s.db.WithContext(ctx).First(&user, id).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrUserNotFound
		}
		return nil, errs.Wrap(err, "auth: get user")
	}
	return &user, nil
}

// GetUserByUsername 根据用户名获取用户。
func (s *AuthService) GetUserByUsername(ctx context.Context, username string) (*dao.User, error) {
	var user dao.User
	err := s.db.WithContext(ctx).Where("username = ?", username).First(&user).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrUserNotFound
		}
		return nil, errs.Wrap(err, "auth: get user by username")
	}
	return &user, nil
}

// ListUsers 返回所有用户（按创建时间降序）。
func (s *AuthService) ListUsers(ctx context.Context) ([]dao.User, error) {
	var users []dao.User
	err := s.db.WithContext(ctx).Order("created_at DESC").Find(&users).Error
	if err != nil {
		return nil, errs.Wrap(err, "auth: list users")
	}
	return users, nil
}

// ----------------------------------------------------------------------------
// 修改用户
// ----------------------------------------------------------------------------

// UpdateRole 修改用户角色。
func (s *AuthService) UpdateRole(ctx context.Context, id uint, role string) error {
	if !IsValidRole(role) {
		return ErrInvalidRole
	}
	result := s.db.WithContext(ctx).Model(&dao.User{}).
		Where("id = ?", id).
		Update("role", role)
	if result.Error != nil {
		return errs.Wrap(result.Error, "auth: update role")
	}
	if result.RowsAffected == 0 {
		return ErrUserNotFound
	}
	return nil
}

// UpdatePassword 修改用户密码。
func (s *AuthService) UpdatePassword(ctx context.Context, id uint, newPassword string) error {
	if len(newPassword) < 6 {
		return errs.BadRequest("password must be at least 6 characters")
	}
	hash, err := HashPassword(newPassword)
	if err != nil {
		return errs.Wrap(err, "auth: hash password")
	}
	result := s.db.WithContext(ctx).Model(&dao.User{}).
		Where("id = ?", id).
		Update("password_hash", hash)
	if result.Error != nil {
		return errs.Wrap(result.Error, "auth: update password")
	}
	if result.RowsAffected == 0 {
		return ErrUserNotFound
	}
	return nil
}

// UpdateProfile 更新用户资料（邮箱、显示名、头像）。
type UpdateProfileInput struct {
	Email       *string
	DisplayName *string
	Avatar      *string
}

// UpdateProfile 修改用户资料，仅更新非 nil 的字段。
func (s *AuthService) UpdateProfile(ctx context.Context, id uint, input UpdateProfileInput) error {
	updates := map[string]any{}
	if input.Email != nil {
		updates["email"] = strings.TrimSpace(*input.Email)
	}
	if input.DisplayName != nil {
		updates["display_name"] = strings.TrimSpace(*input.DisplayName)
	}
	if input.Avatar != nil {
		updates["avatar"] = strings.TrimSpace(*input.Avatar)
	}
	if len(updates) == 0 {
		return nil
	}
	result := s.db.WithContext(ctx).Model(&dao.User{}).
		Where("id = ?", id).
		Updates(updates)
	if result.Error != nil {
		return errs.Wrap(result.Error, "auth: update profile")
	}
	if result.RowsAffected == 0 {
		return ErrUserNotFound
	}
	return nil
}

// ----------------------------------------------------------------------------
// 启用 / 禁用用户
// ----------------------------------------------------------------------------

// DisableUser 禁用用户。
func (s *AuthService) DisableUser(ctx context.Context, id uint) error {
	result := s.db.WithContext(ctx).Model(&dao.User{}).
		Where("id = ?", id).
		Update("status", StatusDisabled)
	if result.Error != nil {
		return errs.Wrap(result.Error, "auth: disable user")
	}
	if result.RowsAffected == 0 {
		return ErrUserNotFound
	}
	return nil
}

// EnableUser 启用用户。
func (s *AuthService) EnableUser(ctx context.Context, id uint) error {
	result := s.db.WithContext(ctx).Model(&dao.User{}).
		Where("id = ?", id).
		Update("status", StatusActive)
	if result.Error != nil {
		return errs.Wrap(result.Error, "auth: enable user")
	}
	if result.RowsAffected == 0 {
		return ErrUserNotFound
	}
	return nil
}

// ----------------------------------------------------------------------------
// 删除用户
// ----------------------------------------------------------------------------

// DeleteUser 删除用户。
func (s *AuthService) DeleteUser(ctx context.Context, id uint) error {
	result := s.db.WithContext(ctx).Delete(&dao.User{}, id)
	if result.Error != nil {
		return errs.Wrap(result.Error, "auth: delete user")
	}
	if result.RowsAffected == 0 {
		return ErrUserNotFound
	}
	return nil
}

// ----------------------------------------------------------------------------
// 权限检查（便捷方法）
// ----------------------------------------------------------------------------

// Can 对指定用户检查是否拥有指定权限。
func (s *AuthService) Can(user *dao.User, permission string) bool {
	if user == nil || user.Status != StatusActive {
		return false
	}
	return HasPermission(user.Role, permission)
}
