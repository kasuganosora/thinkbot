package engagement

import (
	"encoding/json"
	"strings"
)

// ============================================================================
// BotProfileTraits — Bot 自我画像（从 BotScope L3 演化而来）
//
// 画像维度设计：
//   - energy_level: 精力值（0.0~1.0），影响参与概率和限流容量
//   - patience: 耐心值（0.0~1.0），影响冷却时间和退避行为
//   - preferred_topics: 兴趣主题列表，自动追加到 keywords
//   - verbosity: 话痨度（0.0~1.0），影响消息长度过滤
//   - personality: 人格描述标签（如 "热情的技术布道师"）
//
// SOUL.md 作为初始化种子，随时间通过 Dreaming 巩固演化。
// ============================================================================

// BotProfileTraits 是 Bot 的量化人格画像。
type BotProfileTraits struct {
	// EnergyLevel 精力值 0.0~1.0。
	// 0.0 = 精疲力竭，几乎不参与
	// 1.0 = 精力充沛，积极参与
	// 默认 0.5。
	EnergyLevel float64 `json:"energy_level"`

	// Patience 耐心值 0.0~1.0。
	// 0.0 = 极度不耐烦，频繁冷却
	// 1.0 = 极具耐心，无限容忍
	// 默认 0.6。
	Patience float64 `json:"patience"`

	// PreferredTopics Bot 感兴趣的话题关键词列表。
	// 默认空。
	PreferredTopics []string `json:"preferred_topics"`

	// Verbosity 话痨度 0.0~1.0。
	// 0.0 = 惜字如金，只回长文
	// 1.0 = 喋喋不休，短消息也回
	// 默认 0.9。
	Verbosity float64 `json:"verbosity"`

	// Personality 人格描述标签（如 "热情的技术布道师"）。
	Personality string `json:"personality"`

	// Confidence 画像可信度 0.0~1.0（由 SOUL.md = 0.3，经多次梦境提升）。
	Confidence float64 `json:"confidence"`
}

// DefaultBotProfileTraits 返回默认画像（中性人格）。
func DefaultBotProfileTraits() BotProfileTraits {
	return BotProfileTraits{
		EnergyLevel:     0.5,
		Patience:        0.6,
		PreferredTopics: nil,
		Verbosity:       0.9,
		Personality:     "helpful AI assistant",
		Confidence:      0.0,
	}
}

// ============================================================================
// SOUL.md 种子解析
// ============================================================================

// ParseSoulProfile 从 SOUL.md 内容中解析初始 BotProfileTraits。
//
// SOUL.md 中的 Personality 段落被解析为以下映射：
//   - "friendly" → energy_level=0.6, patience=0.8
//   - "concise" / "direct" → verbosity=0.5
//   - "helpful" → energy_level=0.7
//   - "knowledgeable" → energy_level=0.6
//   - "patient" → patience=0.9
//   - "enthusiastic" → energy_level=0.9
//   - "cautious" → energy_level=0.3, verbosity=0.4
//   - "proactive" → energy_level=0.8
//   - "reactive" / "reserved" → energy_level=0.3
//
// 优先级：front matter 中的 profile JSON > 文本中的 Personality 描述符 > 默认值。
func ParseSoulProfile(soulContent string) BotProfileTraits {
	traits := DefaultBotProfileTraits()
	traits.Confidence = 0.3 // SOUL.md 初始化可信度为 0.3

	// 尝试从 front matter 中提取 profile JSON
	if jsonStr := extractFrontMatterProfile(soulContent); jsonStr != "" {
		var fm BotProfileTraits
		if err := json.Unmarshal([]byte(jsonStr), &fm); err == nil {
			mergeTraits(&traits, fm)
			return traits
		}
	}

	// 从文本中提取 Personality 描述符
	persona := extractPersonaSection(soulContent)
	if persona != "" {
		traits.Personality = persona
	}

	// 关键词映射
	lowerContent := strings.ToLower(soulContent)

	// 精力映射
	// 注意：先检查包含否定语境的关键词（reactive/reserved），再检查正向关键词（active）。
	// 避免 "reactive" 被 "active" 子串误匹配。
	if containsAny(lowerContent, "cautious", "reactive", "reserved", "quiet", "observer") {
		traits.EnergyLevel = 0.3
	} else if containsAny(lowerContent, "enthusiastic", "energetic", "passionate", "proactive", "active") {
		traits.EnergyLevel = 0.9
	} else if containsAny(lowerContent, "helpful", "friendly", "knowledgeable") {
		traits.EnergyLevel = 0.7
	} else if containsAny(lowerContent, "lazy", "tired", "reluctant") {
		traits.EnergyLevel = 0.2
	}

	// 耐心映射
	// 先检查否定语境（impatient），再检查正向（patient），避免 "impatient" 被 "patient" 子串误匹配。
	if containsAny(lowerContent, "impatient", "short-tempered") {
		traits.Patience = 0.3
	} else if containsAny(lowerContent, "patient", "tolerant") {
		traits.Patience = 0.9
	}

	// 话痨度映射
	if containsAny(lowerContent, "concise", "brief", "short", "direct") {
		traits.Verbosity = 0.4
	} else if containsAny(lowerContent, "verbose", "detailed", "long", "elaborate") {
		traits.Verbosity = 1.0
	} else if containsAny(lowerContent, "quiet", "silent", "taciturn") {
		traits.Verbosity = 0.2
	}

	// 兴趣主题提取
	traits.PreferredTopics = extractTopicKeywords(soulContent)

	return traits
}

// ============================================================================
// 画像 → Engagement 参数映射（由 AdaptiveEngagementSyncer 使用）
// ============================================================================

// ProfileToEngagementMap 定义从一个 BotProfileTraits 到一个 TimingGateConfig 差异的映射。
// 返回的字段为 nil 表示不修改该字段。
type ProfileToEngagementMap struct {
	ReplyProbability    *float64 `json:"reply_probability,omitempty"`
	BackoffBaseSeconds  *float64 `json:"backoff_base_seconds,omitempty"`
	BackoffStartCount   *int     `json:"backoff_start_count,omitempty"`
	RateLimitCapacity   *int     `json:"rate_limit_capacity,omitempty"`
	Keywords            []string `json:"keywords,omitempty"`
	MinLength           *int     `json:"min_length,omitempty"`
	MaxLength           *int     `json:"max_length,omitempty"`
	EngagementThreshold *int     `json:"engagement_threshold,omitempty"`
}

// MapProfileToEngagement 将 BotProfileTraits 映射为 Engagement 参数调整。
//
// 映射规则：
//   - energy_level → ReplyProbability, RateLimitCapacity
//   - patience → BackoffBaseSeconds, BackoffStartCount
//   - preferred_topics → Keywords
//   - verbosity → MinLength, MaxLength
func MapProfileToEngagement(traits BotProfileTraits) ProfileToEngagementMap {
	m := ProfileToEngagementMap{}

	// energy_level → ReplyProbability（0.05 ~ 0.30）
	rp := 0.05 + traits.EnergyLevel*0.25
	m.ReplyProbability = &rp

	// energy_level → RateLimitCapacity（1 ~ 10）
	rc := int(1 + traits.EnergyLevel*9)
	m.RateLimitCapacity = &rc

	// patience → BackoffBaseSeconds（倒置：耐心低=退避长）
	// 耐心 1.0 → 退避 5s，耐心 0.0 → 退避 60s
	bbs := 5.0 + (1.0-traits.Patience)*55.0
	m.BackoffBaseSeconds = &bbs

	// patience → BackoffStartCount
	// 耐心 1.0 → 5 次不参与后才退避，耐心 0.0 → 1 次就退避
	bsc := int(1 + traits.Patience*4)
	m.BackoffStartCount = &bsc

	// preferred_topics → Keywords
	if len(traits.PreferredTopics) > 0 {
		m.Keywords = make([]string, len(traits.PreferredTopics))
		copy(m.Keywords, traits.PreferredTopics)
	}

	// verbosity → MinLength
	// 话痨度高 → 短消息也回（MinLength=0）
	// 话痨度低 → 只回长文（MinLength=50）
	if traits.Verbosity < 0.3 {
		minLen := 50
		m.MinLength = &minLen
	} else if traits.Verbosity < 0.6 {
		minLen := 10
		m.MinLength = &minLen
	}

	// verbosity → MaxLength（话痨度极高的忽略超长消息）
	if traits.Verbosity < 0.2 {
		maxLen := 200
		m.MaxLength = &maxLen
	}

	return m
}

// ============================================================================
// 内部工具函数
// ============================================================================

// extractFrontMatterProfile 从 SOUL.md front matter 中提取 profile 字段。
func extractFrontMatterProfile(content string) string {
	// 查找 --- 分隔的 front matter
	start := strings.Index(content, "---")
	if start != 0 {
		return ""
	}
	end := strings.Index(content[3:], "---")
	if end < 0 {
		return ""
	}
	fm := strings.TrimSpace(content[3 : 3+end])

	// 查找 profile: 行（可能缩进）
	for _, line := range strings.Split(fm, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "profile:") {
			val := strings.TrimSpace(strings.TrimPrefix(line, "profile:"))
			// 支持 profile: | 多行
			if val == "|" {
				return "" // 暂不支持多行 YAML
			}
			return val
		}
	}
	return ""
}

// extractPersonaSection 从 SOUL.md 提取 Personality 描述。
func extractPersonaSection(content string) string {
	lower := strings.ToLower(content)
	// 查找 ## Personality 或 ## 人格
	idx := strings.Index(lower, "## personality")
	if idx < 0 {
		idx = strings.Index(lower, "## 人格")
	}
	if idx < 0 {
		// 回退到搜索 "personality:" 行
		for _, line := range strings.Split(content, "\n") {
			if strings.HasPrefix(strings.ToLower(strings.TrimSpace(line)), "personality:") {
				return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "personality:"))
			}
		}
		return ""
	}

	// 提取 ## Personality 之后的文本直到下一个 ## 或文件结束
	section := content[idx:]
	rest := strings.Index(section[3:], "##")
	if rest > 0 {
		section = section[:3+rest]
	}

	// 清理：取第一行非 markdown 列表的实质内容
	lines := strings.Split(section, "\n")
	var desc []string
	for _, line := range lines[1:] {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// 去掉 markdown 列表符号
		line = strings.TrimPrefix(line, "- ")
		line = strings.TrimPrefix(line, "* ")
		if line != "" {
			desc = append(desc, line)
		}
	}
	return strings.Join(desc, "; ")
}

// extractTopicKeywords 从 SOUL.md 中提取兴趣主题关键词。
func extractTopicKeywords(content string) []string {
	lower := strings.ToLower(content)
	var keywords []string

	// 查找 "interests:" 或 "topics:" 或 "skills:"
	for _, prefix := range []string{"interests:", "topics:", "skills:", "expertise:"} {
		idx := strings.Index(lower, prefix)
		if idx >= 0 {
			line := strings.TrimSpace(content[idx+len(prefix):])
			if newline := strings.Index(line, "\n"); newline > 0 {
				line = line[:newline]
			}
			for _, kw := range strings.Split(line, ",") {
				kw = strings.TrimSpace(kw)
				if kw != "" {
					keywords = append(keywords, kw)
				}
			}
			break
		}
	}

	// 从 markdown list 中提取（- Go, - Rust 风格）
	if len(keywords) == 0 {
		inList := false
		for _, line := range strings.Split(content, "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "- ") || strings.HasPrefix(trimmed, "* ") {
				inList = true
				item := strings.TrimPrefix(trimmed, "- ")
				item = strings.TrimPrefix(item, "* ")
				if !strings.HasPrefix(item, "#") && len(item) > 1 && len(item) < 50 {
					keywords = append(keywords, item)
				}
			} else if inList && trimmed == "" {
				break
			}
		}
	}

	return keywords
}

// containsAny 检查 s 中是否包含任一关键词。
func containsAny(s string, keywords ...string) bool {
	for _, kw := range keywords {
		if strings.Contains(s, kw) {
			return true
		}
	}
	return false
}

// mergeTraits 将 src 中的非零值覆盖到 dst。
func mergeTraits(dst *BotProfileTraits, src BotProfileTraits) {
	if src.EnergyLevel > 0 {
		dst.EnergyLevel = src.EnergyLevel
	}
	if src.Patience > 0 {
		dst.Patience = src.Patience
	}
	if src.Verbosity > 0 {
		dst.Verbosity = src.Verbosity
	}
	if src.Personality != "" {
		dst.Personality = src.Personality
	}
	if len(src.PreferredTopics) > 0 {
		dst.PreferredTopics = src.PreferredTopics
	}
	if src.Confidence > 0 {
		dst.Confidence = src.Confidence
	}
}
