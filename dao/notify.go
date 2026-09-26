package dao

import "time"

// ============================================================================
// Notify — 外部程序（mdadm / smartd / cron 脚本等）经 HTTP 让 bot 给主人发通知
// 详见 docs/notify.md。
// ============================================================================

// NotifyToken 是 notify 接口的调用凭据。只存 SHA-256 哈希，明文仅在创建时展示一次。
// 明文格式 "tbn_<ID>_<secret>"：ID 用于定位行，secret 为 32 字节随机数（base64url）。
//
// Scope 决定 token 能替哪些 bot 发通知：逗号分隔的 bot ID 列表，或 "*"（全部 bot）。
// 旧版 token（Scope 为空）等价于只含 BotID 一个 bot；启动 / CLI 时会回填。
// BotID 保留为「主归属 bot」（单 bot token 即该 bot；多 bot 为第一个；全部 bot 为 "*"）。
type NotifyToken struct {
	ID         string     `gorm:"primaryKey;size:32" json:"id"`
	BotID      string     `gorm:"size:64;not null;index" json:"botId"`
	Scope      string     `gorm:"type:text;not null;default:''" json:"-"`
	Name       string     `gorm:"size:128;not null;default:''" json:"name"`
	Hash       string     `gorm:"size:64;not null" json:"-"`
	CreatedAt  time.Time  `gorm:"not null" json:"createdAt"`
	LastUsedAt *time.Time `json:"lastUsedAt,omitempty"`
	RevokedAt  *time.Time `json:"revokedAt,omitempty"`
}

// TableName 指定 GORM 表名。
func (NotifyToken) TableName() string { return "notify_tokens" }

// NotifyEvent 是 notify 接口的审计记录：每个通过鉴权的请求一行（含去重 / 限流 / 失败）。
type NotifyEvent struct {
	ID          string    `gorm:"primaryKey;size:64" json:"id"`
	BotID       string    `gorm:"size:64;not null;index:idx_notify_dedup,priority:1;index:idx_notify_bot_time,priority:1" json:"botId"`
	CreatedAt   time.Time `gorm:"not null;index:idx_notify_dedup,priority:3;index:idx_notify_bot_time,priority:2" json:"createdAt"`
	Source      string    `gorm:"size:64;not null;default:''" json:"source"`
	Level       string    `gorm:"size:16;not null;default:''" json:"level"`
	Title       string    `gorm:"size:512;not null;default:''" json:"title"`
	DedupKey    string    `gorm:"size:256;not null;default:''" json:"dedupKey"`
	DedupHash   string    `gorm:"size:64;not null;default:'';index:idx_notify_dedup,priority:2" json:"-"`
	TokenID     string    `gorm:"size:32;not null;default:''" json:"tokenId"`
	CallerIP    string    `gorm:"size:64;not null;default:''" json:"callerIp"`
	Mode        string    `gorm:"size:16;not null;default:''" json:"mode"`
	ChannelName string    `gorm:"size:128;not null;default:''" json:"channel"`
	Target      string    `gorm:"size:128;not null;default:''" json:"target"`
	// Status: pending | delivered | deduplicated | rate_limited | failed | rejected
	Status      string `gorm:"size:16;not null;index" json:"status"`
	Error       string `gorm:"type:text" json:"error,omitempty"`
	DuplicateOf string `gorm:"size:64;not null;default:''" json:"duplicateOf,omitempty"`
	RepeatCount int    `gorm:"not null;default:1" json:"repeatCount"`
	// PersonaUsed 仅 bot 模式（旧名 persona）下模型输出被采用时为 true；false＝raw / 回落 raw。
	// 列名沿用首版（persona_used）以免迁移，JSON 同时给出 botUsed。
	PersonaUsed bool `gorm:"not null;default:false" json:"botUsed"`
}

// TableName 指定 GORM 表名。
func (NotifyEvent) TableName() string { return "notify_events" }
