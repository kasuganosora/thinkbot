// Package notify 实现「外部程序 → bot → 主人」的通知通道：
// mdadm / smartd / cron 脚本经 POST /api/bots/{id}/notify（独立 token 鉴权）
// 让 bot 把告警推到主人的私聊（默认 Telegram），绕过对话 pipeline 的
// reply_control / 低信息抑制等闸门，并做去重、限流、审计与会话历史回写。
//
// 外部内容一律视为不可信数据：raw 模式按纯文本发送（不启用任何 parse_mode）；
// persona 模式以 JSON 数据块喂给模型、不给任何工具，且 critical 始终附带原文。
// 详见 docs/notify.md。
package notify

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/kasuganosora/thinkbot/config"
)

// 级别。
const (
	LevelInfo     = "info"
	LevelWarn     = "warn"
	LevelCritical = "critical"
)

// 模式。
const (
	ModeRaw     = "raw"
	ModePersona = "persona"
)

// 审计状态。
const (
	StatusPending      = "pending"
	StatusDelivered    = "delivered"
	StatusDeduplicated = "deduplicated"
	StatusRateLimited  = "rate_limited"
	StatusFailed       = "failed"
	StatusRejected     = "rejected"
)

// Rate 是「窗口内最多 N 次」的令牌桶参数。N<=0 表示不限流。
type Rate struct {
	N      int
	Window time.Duration
}

// ParseRate 解析 "20/1h"、"5/10m"、"100/24h"。非法输入返回 def。
func ParseRate(s string, def Rate) Rate {
	s = strings.TrimSpace(s)
	if s == "" {
		return def
	}
	parts := strings.SplitN(s, "/", 2)
	if len(parts) != 2 {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return def
	}
	w, err := time.ParseDuration(strings.TrimSpace(parts[1]))
	if err != nil || w <= 0 {
		return def
	}
	return Rate{N: n, Window: w}
}

func (r Rate) String() string { return fmt.Sprintf("%d/%s", r.N, r.Window) }

// Config 是一次请求时刻的有效配置（全局 + per-bot 覆盖）。
type Config struct {
	Enabled             bool
	DefaultChannel      string
	OwnerTarget         string
	DefaultMode         string
	AllowTargetOverride bool
	MaxRequestBytes     int64
	MaxTitleChars       int
	MaxBodyChars        int
	RateLimit           Rate
	RateLimitCritical   Rate
	DedupWindow         time.Duration
	PersonaTimeout      time.Duration
	PersonaMaxChars     int
	PersonaMaxTokens    int
	RecordHistory       bool
	Location            *time.Location
}

// 默认值（与 config.DefaultMap 保持一致）。
var (
	DefaultRate         = Rate{N: 20, Window: time.Hour}
	DefaultCriticalRate = Rate{N: 60, Window: time.Hour}
)

// DefaultConfig 返回内置默认配置。
func DefaultConfig() Config {
	return Config{
		Enabled:           true,
		DefaultChannel:    "telegram",
		DefaultMode:       ModeRaw,
		MaxRequestBytes:   16384,
		MaxTitleChars:     200,
		MaxBodyChars:      3000,
		RateLimit:         DefaultRate,
		RateLimitCritical: DefaultCriticalRate,
		DedupWindow:       30 * time.Minute,
		PersonaTimeout:    45 * time.Second,
		PersonaMaxChars:   1000,
		RecordHistory:     true,
		Location:          time.Local,
	}
}

// LoadConfig 从 config.Store 读取 botID 的有效配置。store 为 nil 时返回默认值。
func LoadConfig(store *config.Store, botID string) Config {
	c := DefaultConfig()
	if store == nil {
		return c
	}
	perBot := func(field, globalKey, def string) string {
		if botID != "" {
			if v, ok := store.Get(config.NotifyBotKey(botID, field)); ok && strings.TrimSpace(v) != "" {
				return strings.TrimSpace(v)
			}
		}
		return strings.TrimSpace(store.GetString(globalKey, def))
	}
	c.Enabled = store.GetBool(config.KeyNotifyEnabled, c.Enabled)
	c.DefaultChannel = perBot("channel", config.KeyNotifyDefaultChannel, c.DefaultChannel)
	c.OwnerTarget = perBot("target", config.KeyNotifyOwnerTarget, "")
	c.DefaultMode = NormalizeMode(perBot("mode", config.KeyNotifyDefaultMode, c.DefaultMode))
	if c.DefaultMode == "" {
		c.DefaultMode = ModeRaw
	}
	c.AllowTargetOverride = store.GetBool(config.KeyNotifyAllowTargetOverride, false)
	c.MaxRequestBytes = clamp64(store.GetInt64(config.KeyNotifyMaxRequestBytes, c.MaxRequestBytes), 1024, 1<<20)
	c.MaxTitleChars = clampInt(store.GetInt(config.KeyNotifyMaxTitleChars, c.MaxTitleChars), 16, 1000)
	c.MaxBodyChars = clampInt(store.GetInt(config.KeyNotifyMaxBodyChars, c.MaxBodyChars), 64, 3500)
	c.RateLimit = ParseRate(store.GetString(config.KeyNotifyRateLimit, ""), DefaultRate)
	c.RateLimitCritical = ParseRate(store.GetString(config.KeyNotifyRateLimitCritical, ""), DefaultCriticalRate)
	c.DedupWindow = store.GetDuration(config.KeyNotifyDedupWindow, c.DedupWindow)
	if c.DedupWindow < 0 {
		c.DedupWindow = 0
	}
	c.PersonaTimeout = store.GetDuration(config.KeyNotifyPersonaTimeout, c.PersonaTimeout)
	if c.PersonaTimeout <= 0 {
		c.PersonaTimeout = 45 * time.Second
	}
	c.PersonaMaxChars = clampInt(store.GetInt(config.KeyNotifyPersonaMaxChars, c.PersonaMaxChars), 100, 3000)
	c.PersonaMaxTokens = store.GetInt(config.KeyNotifyPersonaMaxTokens, 0)
	if c.PersonaMaxTokens < 0 {
		c.PersonaMaxTokens = 0
	}
	c.RecordHistory = store.GetBool(config.KeyNotifyRecordHistory, true)
	c.Location = config.NewBuilder(store, nil).GetBotTimezoneLocation(botID)
	if c.Location == nil {
		c.Location = time.Local
	}
	return c
}

// NormalizeLevel 归一级别；未知返回 ""。
func NormalizeLevel(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "info", "notice", "":
		return LevelInfo
	case "warn", "warning":
		return LevelWarn
	case "critical", "crit", "error", "err", "alert", "emerg", "emergency":
		return LevelCritical
	default:
		return ""
	}
}

// NormalizeMode 归一模式；空串原样返回（表示用默认），未知返回 "invalid"。
func NormalizeMode(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "":
		return ""
	case ModeRaw:
		return ModeRaw
	case ModePersona:
		return ModePersona
	default:
		return "invalid"
	}
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func clamp64(v, lo, hi int64) int64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
