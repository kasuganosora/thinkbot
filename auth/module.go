package auth

import (
	"context"
	"os"
	"strings"

	"go.uber.org/fx"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/kasuganosora/thinkbot/dao"
	"github.com/kasuganosora/thinkbot/util/errs"
)

// ============================================================================
// fx Module — auth 依赖注入
// ============================================================================

// AuthParams 是创建 AuthService 所需的依赖。
type AuthParams struct {
	fx.In

	DB     *gorm.DB
	Logger *zap.SugaredLogger
}

// Module 是 auth 的 fx 模块。
//
// 生命周期：
//   - OnStart: 确保存在至少一个管理员账户（从环境变量读取引导管理员信息）
var Module = fx.Module("auth",
	fx.Provide(NewModule),
	fx.Invoke(registerAuthLifecycle),
)

// NewModule 创建 AuthService。
func NewModule(p AuthParams) *AuthService {
	return New(p.DB)
}

// LifecycleParams 用于注册生命周期钩子。
type LifecycleParams struct {
	fx.In

	Service   *AuthService
	Lifecycle fx.Lifecycle
	Logger    *zap.SugaredLogger
}

func registerAuthLifecycle(p LifecycleParams) {
	p.Lifecycle.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			// 引导管理员：如果设置了环境变量，则创建初始管理员
			adminUser := strings.TrimSpace(os.Getenv("AUTH_BOOTSTRAP_ADMIN"))
			adminPass := os.Getenv("AUTH_BOOTSTRAP_PASSWORD")

			if adminUser != "" && adminPass != "" {
				// 检查是否已有 admin（使用 count 查询，避免全表加载）
				var adminCount int64
				if err := p.Service.DB().WithContext(ctx).
					Model(&dao.User{}).
					Where("role = ?", RoleAdmin).
					Count(&adminCount).Error; err != nil {
					return errs.Wrap(err, "auth: count admins for bootstrap check")
				}

				if adminCount == 0 {
					_, err := p.Service.CreateUser(ctx, CreateUserInput{
						Username: adminUser,
						Password: adminPass,
						Role:     RoleAdmin,
					})
					if err != nil {
						p.Logger.Errorw("auth: bootstrap admin creation failed", "err", err)
					} else {
						p.Logger.Infow("auth: bootstrap admin created", "username", adminUser)
					}
				}
			}

			p.Logger.Infow("auth service initialized")
			return nil
		},
		OnStop: func(ctx context.Context) error {
			return nil
		},
	})
}
