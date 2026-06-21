package api

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"

	"github.com/kasuganosora/thinkbot/dao"
)

// ============================================================================
// JWT-in-Cookie 无状态认证
//
// 用户登录成功后，将关键信息（userID/username/role/status）编码为 JWT，
// 用 HMAC-SHA256 签名后存入 httpOnly Cookie。
// 每次请求中间件读取 Cookie → 验证签名 → 解析 Claims → 注入用户到 context。
// 完全无状态，无需服务端 session 存储。
// ============================================================================

const (
	// CookieName Cookie 名称。
	CookieName = "thinkbot_session"

	// CookieMaxAge Cookie 有效期（7 天）。
	CookieMaxAge = 7 * 24 * 60 * 60

	// ContextKeyUser gin.Context 中存储当前用户的 key。
	ContextKeyUser = "current_user"
)

// SessionClaims 是 JWT 中携带的用户会话信息。
type SessionClaims struct {
	UserID   uint   `json:"uid"`
	Username string `json:"usr"`
	Role     string `json:"rol"`
	Status   string `json:"sts"`
	jwt.RegisteredClaims
}

// CookieManager 管理 JWT Cookie 的签发和验证。
type CookieManager struct {
	secret []byte
	secure bool // Cookie 是否仅通过 HTTPS 传输
}

// NewCookieManager 创建 CookieManager。
// secret 为空时使用随机生成的密钥（重启后失效）。
// secure 控制 Cookie 的 Secure 标志（true=仅 HTTPS）。
func NewCookieManager(secret string, secure bool) *CookieManager {
	if secret == "" {
		// 生成随机密钥（重启后失效，仅用于开发/未配置场景）
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			panic("crypto/rand failed: " + err.Error())
		}
		secret = hex.EncodeToString(b)
	}
	return &CookieManager{secret: []byte(secret), secure: secure}
}

// EncodeToken 将用户信息编码为 JWT 字符串。
func (m *CookieManager) EncodeToken(user *dao.User) (string, error) {
	now := time.Now()
	claims := SessionClaims{
		UserID:   user.ID,
		Username: user.Username,
		Role:     user.Role,
		Status:   user.Status,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(now.Add(CookieMaxAge * time.Second)),
			IssuedAt:  jwt.NewNumericDate(now),
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString(m.secret)
}

// DecodeToken 解析并验证 JWT 字符串，返回 Claims。
func (m *CookieManager) DecodeToken(tokenStr string) (*SessionClaims, error) {
	claims := &SessionClaims{}
	token, err := jwt.ParseWithClaims(tokenStr, claims, func(t *jwt.Token) (any, error) {
		return m.secret, nil
	})
	if err != nil || !token.Valid {
		return nil, err
	}
	return claims, nil
}

// SetCookie 将 JWT 写入 httpOnly Cookie。
func (m *CookieManager) SetCookie(c *gin.Context, token string) {
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(CookieName, token, CookieMaxAge, "/", "", m.secure, true)
}

// ClearCookie 清除认证 Cookie。
func (m *CookieManager) ClearCookie(c *gin.Context) {
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(CookieName, "", -1, "/", "", m.secure, true)
}
