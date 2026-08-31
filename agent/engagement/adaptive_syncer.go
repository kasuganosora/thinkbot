package engagement

import (
	"context"
	"sync"
	"time"

	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
)

// ============================================================================
// AdaptiveEngagementSyncer — 画像 → Engagement 参数动态映射器
//
// 职责：
//   1. 订阅 BotProfileTraits 的更新（由 Dreaming 管线的 Bot 画像提取阶段触发）
//   2. 将量化画像映射为具体的 TimingGateConfig 参数调整
//   3. 支持层级继承的 per-channel 配置
//   4. 提供 DynamicConfigFunc 回调供 TimingGate 实时读取
//
// 层级配置继承（从粗到细）：
//   bot.<id>.engagement.*                         → bot 级
//   bot.<id>.channel.<type>.engagement.*          → channel 类型级（如 telegram）
//   bot.<id>.channel.<type>.<chat_id>.engagement.* → 具体群/单聊级
//
// 读取优先级：最细粒度优先，未配置时向上 fallback。
// ============================================================================

// AdaptiveEngagementSyncer 是 Adaptive Engagement 的核心同步器。
type AdaptiveEngagementSyncer struct {
	mu sync.RWMutex

	// traits 当前 Bot 画像（线程安全）。
	traits BotProfileTraits

	// perChannelOverrides per-channel 的手动覆盖配置。
	// key = channel type or "channel_type:chat_id"
	perChannelOverrides map[string]*channelEngagementOverride

	// enabledChannels 启用了动态调整的 channel 集合。
	// key = channel type or "channel_type:chat_id"
	enabledChannels map[string]bool

	// globalEnabled 全局开关（总闸）。
	globalEnabled bool

	// lastSync 上次同步时间。
	lastSync time.Time

	tracer trace.Tracer
	logger *zap.SugaredLogger
}

// channelEngagementOverride per-channel 覆盖配置。
type channelEngagementOverride struct {
	ReplyProbability    *float64 `json:"reply_probability,omitempty"`
	BackoffBaseSeconds  *float64 `json:"backoff_base_seconds,omitempty"`
	BackoffStartCount   *int     `json:"backoff_start_count,omitempty"`
	RateLimitCapacity   *int     `json:"rate_limit_capacity,omitempty"`
	Keywords            []string `json:"keywords,omitempty"`
	MinLength           *int     `json:"min_length,omitempty"`
	MaxLength           *int     `json:"max_length,omitempty"`
	EngagementThreshold *int     `json:"engagement_threshold,omitempty"`
}

// SyncerConfig 配置 AdaptiveEngagementSyncer。
type SyncerConfig struct {
	// BotID Bot 标识符。
	BotID string
	// InitialTraits 初始画像（从 SOUL.md 解析）。
	InitialTraits BotProfileTraits
	// GlobalEnabled 全局开关。
	GlobalEnabled bool
	// EnabledChannels 启用了动态调整的 channel key 集合。
	EnabledChannels []string
}

// NewAdaptiveEngagementSyncer 创建自适应同步器。
func NewAdaptiveEngagementSyncer(cfg SyncerConfig, tp trace.TracerProvider, logger *zap.SugaredLogger) *AdaptiveEngagementSyncer {
	syncer := &AdaptiveEngagementSyncer{
		traits:              cfg.InitialTraits,
		perChannelOverrides: make(map[string]*channelEngagementOverride),
		enabledChannels:     make(map[string]bool),
		globalEnabled:       cfg.GlobalEnabled,
		tracer:              tp.Tracer("github.com/kasuganosora/thinkbot/agent/engagement/adaptive_syncer"),
		logger:              logger.With("component", "adaptive_syncer"),
	}
	for _, ch := range cfg.EnabledChannels {
		syncer.enabledChannels[ch] = true
	}
	return syncer
}

// ============================================================================
// 画像更新
// ============================================================================

// UpdateTraits 更新 Bot 画像（由 Dreaming 管线调用）。
func (s *AdaptiveEngagementSyncer) UpdateTraits(traits BotProfileTraits) {
	s.mu.Lock()
	defer s.mu.Unlock()

	old := s.traits
	s.traits = traits
	s.lastSync = time.Now()

	// 检测画像漂移
	energyDrift := traits.EnergyLevel - old.EnergyLevel
	if energyDrift < 0 {
		energyDrift = -energyDrift
	}

	s.logger.Infow("bot profile updated",
		"energy_level", traits.EnergyLevel,
		"patience", traits.Patience,
		"verbosity", traits.Verbosity,
		"personality", traits.Personality,
		"confidence", traits.Confidence,
		"was_energy", old.EnergyLevel,
		"was_patience", old.Patience,
		"energy_drift", energyDrift,
		"topics_count", len(traits.PreferredTopics),
	)
}

// GetTraits 返回当前画像（线程安全）。
func (s *AdaptiveEngagementSyncer) GetTraits() BotProfileTraits {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.traits
}

// ============================================================================
// Per-channel 开关管理
// ============================================================================

// SetChannelEnabled 启用/禁用指定 channel 的动态调整。
// channelKey 格式：
//   - "telegram"              → 整个 telegram channel
//   - "telegram:-123456"      → 特定群聊
//   - "telegram:user_123"     → 特定单聊
func (s *AdaptiveEngagementSyncer) SetChannelEnabled(channelKey string, enabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if enabled {
		s.enabledChannels[channelKey] = true
	} else {
		delete(s.enabledChannels, channelKey)
	}
	s.logger.Infow("channel adaptive engagement toggled",
		"channel_key", channelKey, "enabled", enabled)
}

// IsChannelEnabled 检查指定 channel 的动态调整是否启用。
// 支持层级继承：先查最细粒度，未找到时逐级向上回退。
func (s *AdaptiveEngagementSyncer) IsChannelEnabled(channelType, chatID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if !s.globalEnabled {
		return false
	}

	// 优先级：type:chatID > type > global
	if chatID != "" {
		if enabled, ok := s.enabledChannels[channelType+":"+chatID]; ok {
			return enabled
		}
	}
	if enabled, ok := s.enabledChannels[channelType]; ok {
		return enabled
	}
	// 未明确配置的 channel 默认不启用动态调整
	return false
}

// SetGlobalEnabled 设置全局开关。
func (s *AdaptiveEngagementSyncer) SetGlobalEnabled(enabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.globalEnabled = enabled
}

// ============================================================================
// 层级配置覆盖
// ============================================================================

// SetChannelOverride 为指定 channel 设置手动覆盖参数。
func (s *AdaptiveEngagementSyncer) SetChannelOverride(channelKey string, override channelEngagementOverride) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.perChannelOverrides[channelKey] = &override
}

// RemoveChannelOverride 删除指定 channel 的手动覆盖。
func (s *AdaptiveEngagementSyncer) RemoveChannelOverride(channelKey string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.perChannelOverrides, channelKey)
}

// ============================================================================
// DynamicConfigFunc — 核心回调
// ============================================================================

// GetTimingConfigOverride 返回指定 channel 的 TimingGateConfig 调整。
// 这是注入 TimingGate 的 DynamicConfigFunc 的方法。
//
// 合并顺序：
//  1. 从 BotProfileTraits 映射出基础调整
//  2. 叠加 per-channel 手动覆盖（最细粒度优先）
//
// 参数：
//   - channelType: 渠道类型（如 "telegram"）
//   - chatID: 聊天 ID（群 ID 或用户 ID），为空时表示不区分
//
// 返回 TimingGateConfig 的差异字段（nil 表示不修改该字段）。
func (s *AdaptiveEngagementSyncer) GetTimingConfigOverride(channelType, chatID string) *channelEngagementOverride {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if !s.globalEnabled {
		return nil
	}

	// 个性化画像映射
	profileMap := MapProfileToEngagement(s.traits)
	result := &channelEngagementOverride{
		ReplyProbability:    profileMap.ReplyProbability,
		BackoffBaseSeconds:  profileMap.BackoffBaseSeconds,
		BackoffStartCount:   profileMap.BackoffStartCount,
		RateLimitCapacity:   profileMap.RateLimitCapacity,
		Keywords:            profileMap.Keywords,
		MinLength:           profileMap.MinLength,
		MaxLength:           profileMap.MaxLength,
		EngagementThreshold: profileMap.EngagementThreshold,
	}

	// 叠加 per-channel 手动覆盖（层级继承）
	s.applyChannelOverrides(channelType, chatID, result)

	// 如果当前 channel 未启用动态调整，返回 nil
	if !s.isChannelEnabledLocked(channelType, chatID) {
		return nil
	}

	// 记录关键决策参数（仅 Debug 级别，避免高频日志）
	s.logger.Debugw("adaptive engagement override computed",
		"channel_type", channelType,
		"chat_id", chatID,
		"energy_level", s.traits.EnergyLevel,
		"patience", s.traits.Patience,
		"reply_prob", ptrOrZero(result.ReplyProbability),
		"backoff_start", ptrOrZeroInt(result.BackoffStartCount),
	)

	return result
}

// isChannelEnabledLocked 在持有读锁时检查 channel 开关。
func (s *AdaptiveEngagementSyncer) isChannelEnabledLocked(channelType, chatID string) bool {
	if chatID != "" {
		if enabled, ok := s.enabledChannels[channelType+":"+chatID]; ok {
			return enabled
		}
	}
	if enabled, ok := s.enabledChannels[channelType]; ok {
		return enabled
	}
	// 未明确配置：channel type 级没有显式打开 → 不启用
	return false
}

// applyChannelOverrides 按层级继承优先级叠加覆盖配置。
func (s *AdaptiveEngagementSyncer) applyChannelOverrides(channelType, chatID string, result *channelEngagementOverride) {
	// 优先级从低到高应用：
	// 1. channel type 级
	if ov, ok := s.perChannelOverrides[channelType]; ok {
		mergeOverride(result, ov)
	}
	// 2. channel type + chatID 级（最高优先级）
	if chatID != "" {
		if ov, ok := s.perChannelOverrides[channelType+":"+chatID]; ok {
			mergeOverride(result, ov)
		}
	}
}

// ApplyToTimingConfig 将 override 应用到 TimingGateConfig。
// 返回修改后的配置副本。
func (s *AdaptiveEngagementSyncer) ApplyToTimingConfig(base TimingGateConfig, override *channelEngagementOverride) TimingGateConfig {
	if override == nil {
		return base
	}
	if override.ReplyProbability != nil {
		base.ReplyProbability = *override.ReplyProbability
	}
	if override.BackoffBaseSeconds != nil {
		base.BackoffBaseSeconds = *override.BackoffBaseSeconds
	}
	if override.BackoffStartCount != nil {
		base.BackoffStartCount = *override.BackoffStartCount
	}
	return base
}

// ============================================================================
// 画像写入（供 Dreaming 管线完成后调用）
// ============================================================================

// WriteProfileToManager 将当前画像写入 TieredManager 的 L3（BotScope）。
// 应在每次 Dreaming 管线完成 Bot 画像提取后调用。
func (s *AdaptiveEngagementSyncer) WriteProfileToManager(_ context.Context) {
	s.mu.RLock()
	traits := s.traits
	s.mu.RUnlock()

	// traits 将通过 TieredManager.WriteProfile 写入
	// 此处仅记录日志，实际写入由调用方通过 TieredManager 完成
	s.logger.Debugw("would write bot profile to L3",
		"energy_level", traits.EnergyLevel,
		"patience", traits.Patience,
		"personality", traits.Personality,
	)
}

// ============================================================================
// ChannelKey helpers
// ============================================================================

// ChannelKeyForType 构建 channel type 级别的 key。
func ChannelKeyForType(channelType string) string {
	return channelType
}

// ChannelKeyForChat 构建 channel type + chat ID 级别的 key。
func ChannelKeyForChat(channelType, chatID string) string {
	if chatID == "" {
		return channelType
	}
	return channelType + ":" + chatID
}

// ============================================================================
// 内部工具
// ============================================================================

func mergeOverride(dst, src *channelEngagementOverride) {
	if src.ReplyProbability != nil {
		dst.ReplyProbability = src.ReplyProbability
	}
	if src.BackoffBaseSeconds != nil {
		dst.BackoffBaseSeconds = src.BackoffBaseSeconds
	}
	if src.BackoffStartCount != nil {
		dst.BackoffStartCount = src.BackoffStartCount
	}
	if src.RateLimitCapacity != nil {
		dst.RateLimitCapacity = src.RateLimitCapacity
	}
	if len(src.Keywords) > 0 {
		dst.Keywords = append(dst.Keywords, src.Keywords...)
	}
	if src.MinLength != nil {
		dst.MinLength = src.MinLength
	}
	if src.MaxLength != nil {
		dst.MaxLength = src.MaxLength
	}
	if src.EngagementThreshold != nil {
		dst.EngagementThreshold = src.EngagementThreshold
	}
}

func ptrOrZero(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}

func ptrOrZeroInt(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}
