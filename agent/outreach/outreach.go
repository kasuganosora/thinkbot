// Package outreach 实现对人主动开口：cron 轮询 + 显式规则 + 发送配额。
//
// 主动性来自 cron 和规则，不来自模型。闸门全开之后才走与 user_cron 相同的
// Engine.ProcessSync（工具可用），模型只负责把已经决定要说的事说清楚。
package outreach

import (
	"fmt"
	"strings"
	"time"
)

// 已知平台，与「渠道发言」对齐。未出现在 Config.Platforms 的平台按关闭处理。
var KnownPlatforms = []string{"web", "telegram", "misskey"}

// 记录状态。
const (
	StatusSilent                  = "silent"
	StatusSent                    = "sent"
	StatusSkippedQuota            = "skipped_quota"
	StatusSkippedQuiet            = "skipped_quiet"
	StatusSkippedPlatformDisabled = "skipped_platform_disabled"
	StatusSkippedSpeakMode        = "skipped_speak_mode"
	StatusSkippedNoTarget         = "skipped_no_target"
	StatusError                   = "error"
)

// 过期窗口：pending 且 due_at 早于 now-ExpireAfter 的承诺不再补发。
const ExpireAfter = 7 * 24 * time.Hour

// MaxAttempts 单条承诺连续失败上限。达到后标 failed，不再进 ListDue。
const MaxAttempts = 3

// PlatformConfig 单个平台的主动开口开关与配额。
type PlatformConfig struct {
	Enabled            bool    `json:"enabled"`
	MaxPerUserPerDay   int     `json:"max_per_user_per_day"`
	QuietHours         float64 `json:"quiet_hours"`
	HardBypassQuiet    bool    `json:"hard_bypass_quiet"`
	HardBypassDailyCap bool    `json:"hard_bypass_daily_cap"`
}

// Config 每 bot 一份。IntervalMin 是 bot 级轮询间隔；配额按平台独立。
type Config struct {
	Enabled     bool                      `json:"enabled"`
	IntervalMin int                       `json:"interval_min"`
	Platforms   map[string]PlatformConfig `json:"platforms"`
}

// DefaultPlatformConfig 返回宁紧勿松的平台默认值（平台本身默认关）。
func DefaultPlatformConfig() PlatformConfig {
	return PlatformConfig{
		Enabled:            false,
		MaxPerUserPerDay:   1,
		QuietHours:         6,
		HardBypassQuiet:    true,
		HardBypassDailyCap: true,
	}
}

// DefaultConfig 返回默认配置。总开关与各平台均默认关闭。
func DefaultConfig() Config {
	plats := make(map[string]PlatformConfig, len(KnownPlatforms))
	for _, p := range KnownPlatforms {
		plats[p] = DefaultPlatformConfig()
	}
	return Config{
		Enabled:     false,
		IntervalMin: 30,
		Platforms:   plats,
	}
}

// Normalize 补齐缺省平台与非法数值，避免前端拿到 0 值当真。
func (c *Config) Normalize() {
	if c == nil {
		return
	}
	def := DefaultConfig()
	if c.IntervalMin <= 0 {
		c.IntervalMin = def.IntervalMin
	}
	if c.IntervalMin > 1440 {
		c.IntervalMin = 1440
	}
	if c.Platforms == nil {
		c.Platforms = map[string]PlatformConfig{}
	}
	for _, name := range KnownPlatforms {
		p, ok := c.Platforms[name]
		if !ok {
			c.Platforms[name] = DefaultPlatformConfig()
			continue
		}
		if p.MaxPerUserPerDay <= 0 {
			p.MaxPerUserPerDay = 1
		}
		if p.QuietHours < 0 {
			p.QuietHours = 6
		}
		c.Platforms[name] = p
	}
}

// Platform 返回指定平台配置；未知平台视为关闭。
func (c Config) Platform(name string) PlatformConfig {
	name = strings.ToLower(strings.TrimSpace(name))
	if c.Platforms != nil {
		if p, ok := c.Platforms[name]; ok {
			return p
		}
	}
	p := DefaultPlatformConfig()
	p.Enabled = false
	return p
}

// ApplyPlatformPatch 把 patch 里出现的已知平台合并进 dst，未出现的平台保持原值。
func ApplyPlatformPatch(dst *Config, patch map[string]PlatformConfig) {
	if dst == nil || patch == nil {
		return
	}
	dst.Normalize()
	known := make(map[string]struct{}, len(KnownPlatforms))
	for _, n := range KnownPlatforms {
		known[n] = struct{}{}
	}
	for name, p := range patch {
		name = strings.ToLower(strings.TrimSpace(name))
		if _, ok := known[name]; !ok {
			continue
		}
		if p.MaxPerUserPerDay <= 0 {
			p.MaxPerUserPerDay = 1
		}
		if p.QuietHours < 0 {
			p.QuietHours = 6
		}
		dst.Platforms[name] = p
	}
}

// NormalizeChannelType 把 metadata / source 归一成 web|telegram|misskey。
// 入站若没带 channel_type，source 常是实例名（web-{botID} / telegram 实例名）。
func NormalizeChannelType(channelType, source string) string {
	t := strings.ToLower(strings.TrimSpace(channelType))
	for _, p := range KnownPlatforms {
		if t == p {
			return p
		}
	}
	s := strings.ToLower(strings.TrimSpace(source))
	for _, p := range KnownPlatforms {
		if s == p || strings.HasPrefix(s, p) {
			return p
		}
	}
	return t
}

// IdentityKey 生成配额与对账主键。
// web 的 userID 已是内部用户；其它平台若已绑定则用 user:{internalID}，否则 platform:platformUserID。
func IdentityKey(channelType, userID, boundInternalUserID string) string {
	channelType = strings.ToLower(strings.TrimSpace(channelType))
	userID = strings.TrimSpace(userID)
	boundInternalUserID = strings.TrimSpace(boundInternalUserID)
	if boundInternalUserID != "" {
		return "user:" + boundInternalUserID
	}
	if channelType == "web" && userID != "" {
		return "user:" + userID
	}
	if channelType == "" {
		channelType = "unknown"
	}
	return channelType + ":" + userID
}

// ReasonReminder 生成硬条件对账文案。
func ReasonReminder(topic, context string) string {
	topic = strings.TrimSpace(topic)
	context = strings.TrimSpace(context)
	if context != "" && context != topic {
		return fmt.Sprintf("预约到期：%s（用户原话：%s）", topic, context)
	}
	return "预约到期：" + topic
}

// ReasonWatch 生成软条件对账文案。
func ReasonWatch(topic, context string) string {
	topic = strings.TrimSpace(topic)
	context = strings.TrimSpace(context)
	if context != "" && context != topic {
		return fmt.Sprintf("盯梢到期：%s（%s）", topic, context)
	}
	return "盯梢到期：" + topic
}

// TemplateFallback 硬条件 pipeline 空输出时的模板正文。
func TemplateFallback(topic string) string {
	topic = strings.TrimSpace(topic)
	if topic == "" {
		return "提醒：你之前让我到点叫你。"
	}
	return "提醒：" + topic
}

// isHard 是否为硬条件（预约）。
func isHard(kind string) bool {
	return strings.EqualFold(kind, "reminder")
}

// nowOr 返回 nowFn()；nil 时用 time.Now。
func nowOr(nowFn func() time.Time) time.Time {
	if nowFn != nil {
		return nowFn()
	}
	return time.Now()
}
