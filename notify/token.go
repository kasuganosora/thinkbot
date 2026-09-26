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
	"regexp"
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

// ScopeAll 是「全部 bot」作用域。
const ScopeAll = "*"

// botIDRE 限定作用域里的 bot ID 形态（与 bot_definitions.id 一致的朴素标识符）。
var botIDRE = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,64}$`)

// NormalizeScope 清洗作用域：去空白、去重、保序；含 "*" 时收敛为 ["*"]。
// 空列表或含非法 ID 时返回错误。
func NormalizeScope(bots []string) ([]string, error) {
	seen := map[string]bool{}
	out := make([]string, 0, len(bots))
	for _, b := range bots {
		for _, part := range strings.Split(b, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			if part == ScopeAll {
				return []string{ScopeAll}, nil
			}
			if !botIDRE.MatchString(part) {
				return nil, fmt.Errorf("notify: invalid bot id %q in scope", part)
			}
			if !seen[part] {
				seen[part] = true
				out = append(out, part)
			}
		}
	}
	if len(out) == 0 {
		return nil, errors.New("notify: token scope is empty (give at least one bot, or all bots)")
	}
	return out, nil
}

// TokenScope 返回 token 的有效作用域。旧 token（Scope 为空）= 只含 BotID。
func TokenScope(t *dao.NotifyToken) []string {
	if t == nil {
		return nil
	}
	if strings.TrimSpace(t.Scope) == "" {
		if t.BotID == "" {
			return nil
		}
		return []string{t.BotID}
	}
	parts := strings.Split(t.Scope, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// TokenAllows 报告 token 是否可替 botID 发通知。
func TokenAllows(t *dao.NotifyToken, botID string) bool {
	if botID == "" {
		return false
	}
	for _, b := range TokenScope(t) {
		if b == ScopeAll || b == botID {
			return true
		}
	}
	return false
}

// TokenView 是 token 的对外视图（不含哈希；scope 展开为列表）。
type TokenView struct {
	ID         string     `json:"id"`
	BotID      string     `json:"botId"`
	Scope      []string   `json:"scope"`
	AllBots    bool       `json:"allBots"`
	Name       string     `json:"name"`
	CreatedAt  time.Time  `json:"createdAt"`
	LastUsedAt *time.Time `json:"lastUsedAt,omitempty"`
	RevokedAt  *time.Time `json:"revokedAt,omitempty"`
}

// ViewOf 构造 TokenView。
func ViewOf(t dao.NotifyToken) TokenView {
	sc := TokenScope(&t)
	return TokenView{ID: t.ID, BotID: t.BotID, Scope: sc, AllBots: len(sc) == 1 && sc[0] == ScopeAll,
		Name: t.Name, CreatedAt: t.CreatedAt, LastUsedAt: t.LastUsedAt, RevokedAt: t.RevokedAt}
}

// MigrateTokens 建表并回填旧 token 的作用域（scope = bot_id）。幂等。
func MigrateTokens(db *gorm.DB) error {
	if err := db.AutoMigrate(&dao.NotifyToken{}, &dao.NotifyEvent{}); err != nil {
		return err
	}
	return db.Model(&dao.NotifyToken{}).
		Where("(scope IS NULL OR scope = '') AND bot_id <> ''").
		Update("scope", gorm.Expr("bot_id")).Error
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

// Create 生成新 token，作用域为 bots（bot ID 列表，或 ["*"] 表示全部 bot）。
// 返回明文（只此一次）与落库行。
func (s *TokenStore) Create(ctx context.Context, bots []string, name string) (string, *dao.NotifyToken, error) {
	scope, err := NormalizeScope(bots)
	if err != nil {
		return "", nil, err
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
		BotID:     scope[0],
		Scope:     strings.Join(scope, ","),
		Name:      name,
		Hash:      HashToken(plain),
		CreatedAt: s.now().UTC(),
	}
	if err := s.db.WithContext(ctx).Create(row).Error; err != nil {
		return "", nil, fmt.Errorf("notify: save token: %w", err)
	}
	return plain, row, nil
}

// List 列出 token。botID 非空时只列作用域覆盖该 bot 的（含全部 bot token）。不含哈希。
func (s *TokenStore) List(ctx context.Context, botID string) ([]dao.NotifyToken, error) {
	var rows []dao.NotifyToken
	if err := s.db.WithContext(ctx).Model(&dao.NotifyToken{}).Order("created_at ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	if botID == "" {
		return rows, nil
	}
	out := rows[:0]
	for i := range rows {
		if TokenAllows(&rows[i], botID) {
			out = append(out, rows[i])
		}
	}
	return out, nil
}

// Revoke 吊销 token。botID 非空时要求 token 作用域覆盖该 bot。返回是否命中。
func (s *TokenStore) Revoke(ctx context.Context, botID, id string) (bool, error) {
	if botID != "" {
		var row dao.NotifyToken
		err := s.db.WithContext(ctx).Where("id = ? AND revoked_at IS NULL", id).Take(&row).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if !TokenAllows(&row, botID) {
			return false, nil
		}
	}
	now := s.now().UTC()
	res := s.db.WithContext(ctx).Model(&dao.NotifyToken{}).Where("id = ? AND revoked_at IS NULL", id).Update("revoked_at", &now)
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}

// Authenticate 校验明文 token（常量时间比较），返回 ErrTokenMissing / ErrTokenInvalid（→401）。
// 作用域另由 TokenAllows 判定（→403），因为 /api/notify 要读完请求体才知道目标 bot。
func (s *TokenStore) Authenticate(ctx context.Context, plain string) (*dao.NotifyToken, error) {
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
	return &row, nil
}

// Touch 记录 token 最近一次成功使用时间（鉴权 + 作用域都通过后调用）。
func (s *TokenStore) Touch(ctx context.Context, id string) {
	now := s.now().UTC()
	_ = s.db.WithContext(ctx).Model(&dao.NotifyToken{}).Where("id = ?", id).Update("last_used_at", &now).Error
}
