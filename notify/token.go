package notify

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/kasuganosora/thinkbot/dao"
)

// TokenPrefix 是 notify token 明文前缀，便于在日志 / 密钥扫描里识别。
const TokenPrefix = "tbn_"

// 鉴权错误。
var (
	ErrTokenMissing = errors.New("notify: token missing")
	ErrTokenInvalid = errors.New("notify: token invalid")
	ErrTokenScope   = errors.New("notify: token not valid for this bot")
)

// dummyHash 用于「token ID 不存在」时仍做一次等长常量时间比较，避免按耗时枚举 ID。
var dummyHash = sha256.Sum256([]byte("thinkbot-notify-dummy"))

// HashToken 返回明文 token 的 SHA-256（hex）。token 是 256bit 随机数，无需慢哈希。
func HashToken(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

// parseTokenID 从 "tbn_<id>_<secret>" 解析 ID；格式不对返回 ""。
func parseTokenID(plain string) string {
	if !strings.HasPrefix(plain, TokenPrefix) {
		return ""
	}
	rest := plain[len(TokenPrefix):]
	i := strings.IndexByte(rest, '_')
	if i <= 0 || i >= len(rest)-1 || i > 32 {
		return ""
	}
	id := rest[:i]
	for _, r := range id {
		if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')) {
			return ""
		}
	}
	return id
}

// TokenStore 管理 notify token（只落哈希）。
type TokenStore struct {
	db  *gorm.DB
	now func() time.Time
}

// NewTokenStore 创建 token 仓储。
func NewTokenStore(db *gorm.DB) *TokenStore {
	return &TokenStore{db: db, now: time.Now}
}

// Create 为 botID 生成新 token，返回明文（只此一次）与落库行。
func (s *TokenStore) Create(ctx context.Context, botID, name string) (string, *dao.NotifyToken, error) {
	botID = strings.TrimSpace(botID)
	if botID == "" {
		return "", nil, errors.New("notify: bot id required")
	}
	name = strings.TrimSpace(name)
	if len([]rune(name)) > 128 {
		name = string([]rune(name)[:128])
	}
	idBytes := make([]byte, 6)
	secret := make([]byte, 32)
	if _, err := rand.Read(idBytes); err != nil {
		return "", nil, fmt.Errorf("notify: random: %w", err)
	}
	if _, err := rand.Read(secret); err != nil {
		return "", nil, fmt.Errorf("notify: random: %w", err)
	}
	id := hex.EncodeToString(idBytes)
	plain := TokenPrefix + id + "_" + base64.RawURLEncoding.EncodeToString(secret)
	row := &dao.NotifyToken{
		ID:        id,
		BotID:     botID,
		Name:      name,
		Hash:      HashToken(plain),
		CreatedAt: s.now().UTC(),
	}
	if err := s.db.WithContext(ctx).Create(row).Error; err != nil {
		return "", nil, fmt.Errorf("notify: save token: %w", err)
	}
	return plain, row, nil
}

// List 列出 token（botID 为空则全部）。不含哈希。
func (s *TokenStore) List(ctx context.Context, botID string) ([]dao.NotifyToken, error) {
	var rows []dao.NotifyToken
	q := s.db.WithContext(ctx).Model(&dao.NotifyToken{}).Order("created_at ASC")
	if botID != "" {
		q = q.Where("bot_id = ?", botID)
	}
	if err := q.Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// Revoke 吊销 token。botID 非空时要求归属匹配。返回是否命中。
func (s *TokenStore) Revoke(ctx context.Context, botID, id string) (bool, error) {
	now := s.now().UTC()
	q := s.db.WithContext(ctx).Model(&dao.NotifyToken{}).Where("id = ? AND revoked_at IS NULL", id)
	if botID != "" {
		q = q.Where("bot_id = ?", botID)
	}
	res := q.Update("revoked_at", &now)
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}

// Authenticate 校验明文 token 并确认其属于 botID。
// 返回 ErrTokenMissing / ErrTokenInvalid（→401）或 ErrTokenScope（→403）。
func (s *TokenStore) Authenticate(ctx context.Context, plain, botID string) (*dao.NotifyToken, error) {
	plain = strings.TrimSpace(plain)
	if plain == "" {
		return nil, ErrTokenMissing
	}
	got := sha256.Sum256([]byte(plain))
	id := parseTokenID(plain)
	var row dao.NotifyToken
	found := false
	if id != "" {
		err := s.db.WithContext(ctx).Where("id = ?", id).Take(&row).Error
		if err == nil {
			found = true
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("notify: load token: %w", err)
		}
	}
	want := dummyHash[:]
	if found {
		if b, err := hex.DecodeString(row.Hash); err == nil && len(b) == sha256.Size {
			want = b
		} else {
			found = false
		}
	}
	match := subtle.ConstantTimeCompare(got[:], want) == 1
	if !found || !match || row.RevokedAt != nil {
		return nil, ErrTokenInvalid
	}
	if row.BotID != botID {
		return &row, ErrTokenScope
	}
	now := s.now().UTC()
	_ = s.db.WithContext(ctx).Model(&dao.NotifyToken{}).Where("id = ?", row.ID).Update("last_used_at", &now).Error
	return &row, nil
}
