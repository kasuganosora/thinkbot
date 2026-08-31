package engagement

import (
	"github.com/kasuganosora/thinkbot/config"
)

// ============================================================================
// EngagementProfile — 参与预设角色
//
// 参考 Houde et al. (2025) "Controlling AI Agent Participation in Group
// Conversations" 研究二发现：
//   - "角色选择"是用户最欢迎的控制方式（排名第一）
//   - 用户偏好"选一个角色 → 自动调好所有参数"而非逐项调整
//   - 不同任务阶段需要不同的参与行为配置
//
// 预设角色覆盖以下参数：
//   - ReplyProbability（参与概率）
//   - EngagementThreshold（评分阈值）
//   - BackoffStartCount（退避起始计数）
//   - RateLimitCapacity（限流容量）
// ============================================================================

// EngagementProfile 描述一组预设的参与行为参数。
type EngagementProfile struct {
	// Name 角色名称。
	Name string `json:"name"`
	// Description 角色描述（中文）。
	Description string `json:"description"`
	// ReplyProbability 主动参与概率。
	ReplyProbability float64 `json:"replyProbability"`
	// EngagementThreshold LLM 评分阈值（0-100）。
	EngagementThreshold int `json:"engagementThreshold"`
	// BackoffStartCount 从第几次连续不参与开始退避。
	BackoffStartCount int `json:"backoffStartCount"`
	// RateLimitCapacity 令牌桶容量（单位时间最大参与次数）。
	RateLimitCapacity int `json:"rateLimitCapacity"`
}

// BuiltinProfiles 内置预设角色。
//
// 设计参考论文发现：
//   - 72.2% 用户偏好 Reactive（仅@时响应）而非 Proactive
//   - Proactive 变体被批评为"dominated the conversation"、"distracting"
//   - 改进后（降低侵入性 + 可控阈值），用户才接受 Proactive
//   - 用户反感过度热情（"that's a great idea!"），希望更 neutral
var BuiltinProfiles = map[string]EngagementProfile{
	// observer: 极少主动参与，只在高度相关时回复。
	// 适合：正式群组、用户明确反馈"太吵了"的场景
	"observer": {
		Name:                "observer",
		Description:         "极少主动参与，只在非常相关时才回复",
		ReplyProbability:    0.05,
		EngagementThreshold: 85,
		BackoffStartCount:   2,
		RateLimitCapacity:   2,
	},

	// lurker: 偶尔参与，保持低调。
	// 适合：默认行为，大部分社交媒体场景
	"lurker": {
		Name:                "lurker",
		Description:         "偶尔参与，保持低调（推荐默认）",
		ReplyProbability:    0.10,
		EngagementThreshold: 75,
		BackoffStartCount:   3,
		RateLimitCapacity:   3,
	},

	// moderator: 适度参与，引导和总结讨论。
	// 适合：需要 Bot 承担一定主持角色的社区
	"moderator": {
		Name:                "moderator",
		Description:         "适度参与，引导和总结讨论",
		ReplyProbability:    0.15,
		EngagementThreshold: 60,
		BackoffStartCount:   3,
		RateLimitCapacity:   5,
	},

	// active: 积极活跃，经常参与讨论。
	// 适合：小型活跃群组，用户明确希望 Bot 更积极
	"active": {
		Name:                "active",
		Description:         "积极活跃，经常参与讨论（注意可能过于频繁）",
		ReplyProbability:    0.30,
		EngagementThreshold: 50,
		BackoffStartCount:   5,
		RateLimitCapacity:   10,
	},
}

// ApplyProfile 将预设角色的参数覆盖到 EngagementConfig。
// 如果 profileName 未找到，返回 false 且不修改 cfg。
// 覆盖的字段：ReplyProbability、EngagementThreshold、BackoffStartCount、RateLimitCapacity。
func ApplyProfile(cfg *config.EngagementConfig, profileName string) bool {
	p, ok := BuiltinProfiles[profileName]
	if !ok {
		return false
	}
	cfg.ReplyProbability = p.ReplyProbability
	cfg.EngagementThreshold = p.EngagementThreshold
	cfg.BackoffStartCount = p.BackoffStartCount
	cfg.RateLimitCapacity = p.RateLimitCapacity
	return true
}

// GetProfile 按名称查找预设角色。未找到返回 nil。
func GetProfile(name string) *EngagementProfile {
	if p, ok := BuiltinProfiles[name]; ok {
		return &p
	}
	return nil
}

// ProfileNames 返回所有内置角色名称。
func ProfileNames() []string {
	names := make([]string, 0, len(BuiltinProfiles))
	for name := range BuiltinProfiles {
		names = append(names, name)
	}
	return names
}
