package auth

import (
	"golang.org/x/crypto/bcrypt"
)

// ============================================================================
// 密码哈希 & 验证
// ============================================================================

const bcryptCost = bcrypt.DefaultCost

// HashPassword 使用 bcrypt 哈希明文密码。
func HashPassword(plain string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(plain), bcryptCost)
	if err != nil {
		return "", err
	}
	return string(hash), nil
}

// VerifyPassword 比对 bcrypt 哈希与明文密码。
// 匹配返回 true，否则返回 false。
func VerifyPassword(hash, plain string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain)) == nil
}
