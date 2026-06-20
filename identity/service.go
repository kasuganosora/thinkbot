package identity

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/kasuganosora/thinkbot/dao"
	"github.com/kasuganosora/thinkbot/util/errs"
)

// ============================================================================
// BindService — 授权码 & 身份映射服务
//
// 职责：
//   - 生成一次性授权码（5 分钟有效）
//   - 消费授权码（验证有效性 + 创建身份映射）
//   - 查询身份映射（平台用户 → 内部用户）
//   - 管理身份映射（列出、删除）
// ============================================================================

// 授权码有效期。
const codeTTL = 5 * time.Minute

// 领域错误。
var (
	ErrCodeNotFound    = errors.New("identity: bind code not found")
	ErrCodeExpired     = errors.New("identity: bind code expired")
	ErrCodeUsed        = errors.New("identity: bind code already used")
	ErrAlreadyBound    = errors.New("identity: platform account already bound")
	ErrMappingNotFound = errors.New("identity: mapping not found")
)

// BindService 提供授权码和身份映射的完整管理。
type BindService struct {
	db     *gorm.DB
	logger Logger
}

// Logger 是 BindService 使用的最小日志接口。
type Logger interface {
	Infow(msg string, kv ...any)
	Warnw(msg string, kv ...any)
}

// noopLogger 不做任何日志输出。
type noopLogger struct{}

func (noopLogger) Infow(string, ...any) {}
func (noopLogger) Warnw(string, ...any) {}

// New 创建 BindService。
func New(db *gorm.DB, logger Logger) *BindService {
	if logger == nil {
		logger = noopLogger{}
	}
	return &BindService{db: db, logger: logger}
}

// ----------------------------------------------------------------------------
// 授权码生成
// ----------------------------------------------------------------------------

// GenerateCode 为指定用户生成一个一次性授权码。
// 有效期为 5 分钟。同一用户可多次生成（旧码不失效，但各自只能使用一次）。
func (s *BindService) GenerateCode(ctx context.Context, userID uint) (*dao.BindCode, error) {
	if userID == 0 {
		return nil, errs.BadRequest("user id is required")
	}

	now := time.Now()
	code := &dao.BindCode{
		UserID:    userID,
		Code:      generateCode(),
		ExpiresAt: now.Add(codeTTL),
	}

	if err := s.db.WithContext(ctx).Create(code).Error; err != nil {
		return nil, errs.Wrap(err, "identity: create bind code")
	}

	s.logger.Infow("bind code generated",
		"user_id", userID,
		"expires_at", code.ExpiresAt)

	return code, nil
}

// ListCodes 列出指定用户的未使用且未过期的授权码。
func (s *BindService) ListCodes(ctx context.Context, userID uint) ([]dao.BindCode, error) {
	var codes []dao.BindCode
	now := time.Now()
	err := s.db.WithContext(ctx).
		Where("user_id = ? AND used_at IS NULL AND expires_at > ?", userID, now).
		Order("created_at DESC").
		Find(&codes).Error
	if err != nil {
		return nil, errs.Wrap(err, "identity: list bind codes")
	}
	return codes, nil
}

// ----------------------------------------------------------------------------
// 授权码消费（绑定）
// ----------------------------------------------------------------------------

// ConsumeResult 是消费授权码后的结果。
type ConsumeResult struct {
	// Mapping 新建的身份映射。
	Mapping dao.IdentityMapping
	// Username 内部用户名（用于友好提示）。
	Username string
}

// ConsumeCode 消费授权码，创建身份映射。
//
// 流程：
//  1. 查找授权码（大小写不敏感）
//  2. 检查是否已使用、是否过期
//  3. 检查该平台账号是否已绑定其他用户
//  4. 原子操作：标记码已使用 + 创建映射
//
// 如果该平台账号已绑定同一用户，返回已有映射（幂等）。
func (s *BindService) ConsumeCode(ctx context.Context, rawCode, platform, platformUserID string) (*ConsumeResult, error) {
	code := NormalizeCode(rawCode)
	if code == "" {
		return nil, ErrCodeNotFound
	}

	// 查找授权码
	var bindCode dao.BindCode
	err := s.db.WithContext(ctx).Where("code = ?", code).First(&bindCode).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrCodeNotFound
		}
		return nil, errs.Wrap(err, "identity: query bind code")
	}

	// 检查是否已使用
	if bindCode.UsedAt != nil {
		return nil, ErrCodeUsed
	}

	// 检查是否过期
	if time.Now().After(bindCode.ExpiresAt) {
		return nil, ErrCodeExpired
	}

	// 检查该平台账号是否已绑定（幂等：同一用户已绑定则直接返回）
	var existing dao.IdentityMapping
	findErr := s.db.WithContext(ctx).
		Where("platform = ? AND platform_user_id = ?", platform, platformUserID).
		First(&existing).Error
	if findErr == nil {
		// 已绑定
		if existing.UserID == bindCode.UserID {
			// 同一用户 → 幂等返回
			// 仍标记码为已使用
			now := time.Now()
			s.db.WithContext(ctx).Model(&bindCode).Update("used_at", &now)
			return &ConsumeResult{Mapping: existing, Username: s.getUsername(ctx, bindCode.UserID)}, nil
		}
		// 不同用户 → 拒绝
		return nil, ErrAlreadyBound
	}

	// 原子操作：事务内标记码已使用 + 创建映射
	now := time.Now()
	mapping := dao.IdentityMapping{
		UserID:         bindCode.UserID,
		Platform:       platform,
		PlatformUserID: platformUserID,
	}

	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 标记码已使用（乐观锁：WHERE used_at IS NULL）
		result := tx.Model(&dao.BindCode{}).
			Where("id = ? AND used_at IS NULL", bindCode.ID).
			Update("used_at", &now)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			// 并发竞态：码已被其他请求使用
			return ErrCodeUsed
		}

		// 创建映射
		return tx.Create(&mapping).Error
	})
	if err != nil {
		if errors.Is(err, ErrCodeUsed) {
			return nil, ErrCodeUsed
		}
		return nil, errs.Wrap(err, "identity: consume code")
	}

	s.logger.Infow("identity mapping created",
		"user_id", mapping.UserID,
		"platform", mapping.Platform,
		"platform_user_id", mapping.PlatformUserID)

	return &ConsumeResult{
		Mapping:  mapping,
		Username: s.getUsername(ctx, mapping.UserID),
	}, nil
}

// getUsername 查询用户名。
func (s *BindService) getUsername(ctx context.Context, userID uint) string {
	var user dao.User
	if err := s.db.WithContext(ctx).Select("username").First(&user, userID).Error; err != nil {
		return fmt.Sprintf("user-%d", userID)
	}
	return user.Username
}

// ----------------------------------------------------------------------------
// 身份映射查询
// ----------------------------------------------------------------------------

// ResolveMapping 根据平台和平台用户 ID 查找身份映射。
// 返回 nil 表示未绑定。
func (s *BindService) ResolveMapping(ctx context.Context, platform, platformUserID string) (*dao.IdentityMapping, error) {
	var mapping dao.IdentityMapping
	err := s.db.WithContext(ctx).
		Where("platform = ? AND platform_user_id = ?", platform, platformUserID).
		First(&mapping).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, errs.Wrap(err, "identity: resolve mapping")
	}
	return &mapping, nil
}

// ResolveBySource 根据 Message.Source 和 UserID 查找身份映射。
// Source 自动提取平台类型。
func (s *BindService) ResolveBySource(ctx context.Context, source, userID string) (*dao.IdentityMapping, error) {
	return s.ResolveMapping(ctx, extractPlatform(source), userID)
}

// ListBindings 列出指定用户的所有身份映射。
func (s *BindService) ListBindings(ctx context.Context, userID uint) ([]dao.IdentityMapping, error) {
	var mappings []dao.IdentityMapping
	err := s.db.WithContext(ctx).
		Where("user_id = ?", userID).
		Order("created_at DESC").
		Find(&mappings).Error
	if err != nil {
		return nil, errs.Wrap(err, "identity: list bindings")
	}
	return mappings, nil
}

// DeleteBinding 删除指定用户的某个平台绑定。
func (s *BindService) DeleteBinding(ctx context.Context, userID uint, platform, platformUserID string) error {
	result := s.db.WithContext(ctx).
		Where("user_id = ? AND platform = ? AND platform_user_id = ?", userID, platform, platformUserID).
		Delete(&dao.IdentityMapping{})
	if result.Error != nil {
		return errs.Wrap(result.Error, "identity: delete binding")
	}
	if result.RowsAffected == 0 {
		return ErrMappingNotFound
	}
	return nil
}
