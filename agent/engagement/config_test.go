package engagement

import (
	"testing"
	"time"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/config"
)

func TestBuildWritableChecker_Disabled(t *testing.T) {
	cfg := config.EngagementConfig{Enabled: false}
	checker := BuildWritableChecker(cfg)
	if _, ok := checker.(DenyAll); !ok {
		t.Fatalf("expected DenyAll, got %T", checker)
	}
}

func TestBuildWritableChecker_AllowAll(t *testing.T) {
	cfg := config.EngagementConfig{Enabled: true}
	checker := BuildWritableChecker(cfg)
	if _, ok := checker.(AllowAll); !ok {
		t.Fatalf("expected AllowAll, got %T", checker)
	}
}

func TestBuildWritableChecker_SourceAllowlist(t *testing.T) {
	cfg := config.EngagementConfig{
		Enabled:  true,
		Channels: []string{"misskey", "telegram"},
	}
	checker := BuildWritableChecker(cfg)
	al, ok := checker.(*SourceAllowlist)
	if !ok {
		t.Fatalf("expected *SourceAllowlist, got %T", checker)
	}
	msg := &core.Message{Source: "misskey"}
	if !al.IsWritable(msg) {
		t.Error("misskey should be writable")
	}
	msg2 := &core.Message{Source: "rss"}
	if al.IsWritable(msg2) {
		t.Error("rss should NOT be writable")
	}
}

func TestBuildRuleEngine(t *testing.T) {
	cfg := config.EngagementConfig{
		Keywords:           []string{"猫", "cat"},
		BlockedUsers:       []string{"spammer"},
		BlockedSources:     []string{"rss"},
		MinLength:          2,
		MaxLength:          500,
		Cooldown:           10 * time.Minute,
		RateLimitCapacity:  3,
		RateLimitInterval:  1 * time.Hour,
	}

	engine, rateLimit := BuildRuleEngine(cfg, "bot-user-id")
	if engine == nil {
		t.Fatal("expected non-nil engine")
	}
	if rateLimit == nil {
		t.Fatal("expected non-nil rateLimit")
	}

	// Bot 自己的消息应该被排除
	selfMsg := &core.Message{UserID: "bot-user-id", Text: "hello", Source: "misskey"}
	if engine.Allow(selfMsg) {
		t.Error("self message should be excluded")
	}

	// 黑名单用户应该被排除
	blockedMsg := &core.Message{UserID: "spammer", Text: "猫真可爱", Source: "misskey"}
	if engine.Allow(blockedMsg) {
		t.Error("blocked user should be rejected")
	}

	// 黑名单来源应该被排除
	blockedSrc := &core.Message{UserID: "user1", Text: "猫真可爱", Source: "rss"}
	if engine.Allow(blockedSrc) {
		t.Error("blocked source should be rejected")
	}

	// 太短的消息应该被排除
	shortMsg := &core.Message{UserID: "user1", Text: "a", Source: "misskey"}
	if engine.Allow(shortMsg) {
		t.Error("short message should be rejected")
	}

	// 不含关键词的消息应该被排除
	noKwMsg := &core.Message{UserID: "user1", Text: "hello world from somewhere", Source: "misskey"}
	if engine.Allow(noKwMsg) {
		t.Error("message without keyword should be rejected")
	}

	// 符合条件的消息应该通过
	goodMsg := &core.Message{UserID: "user1", Text: "这只猫真可爱啊", Source: "misskey"}
	if !engine.Allow(goodMsg) {
		t.Errorf("good message should pass, rejected: %s", engine.LastReason())
	}
}

func TestBuildTimingGateConfig(t *testing.T) {
	cfg := config.EngagementConfig{
		ReplyProbability:          0.3,
		BackoffBaseSeconds:        20.0,
		BackoffCapSeconds:         600.0,
		BackoffStartCount:         5,
		BurstIntervalSeconds:      8.0,
		WaitTimeoutSeconds:        60.0,
		BackoffBypassPendingCount: 10,
	}

	tgCfg := BuildTimingGateConfig(cfg)
	if tgCfg.ReplyProbability != 0.3 {
		t.Errorf("ReplyProbability=%v, want 0.3", tgCfg.ReplyProbability)
	}
	if tgCfg.BackoffBaseSeconds != 20.0 {
		t.Errorf("BackoffBaseSeconds=%v, want 20.0", tgCfg.BackoffBaseSeconds)
	}
	if tgCfg.BackoffCapSeconds != 600.0 {
		t.Errorf("BackoffCapSeconds=%v, want 600.0", tgCfg.BackoffCapSeconds)
	}
	if tgCfg.BackoffStartCount != 5 {
		t.Errorf("BackoffStartCount=%v, want 5", tgCfg.BackoffStartCount)
	}
	if tgCfg.BurstIntervalSeconds != 8.0 {
		t.Errorf("BurstIntervalSeconds=%v, want 8.0", tgCfg.BurstIntervalSeconds)
	}
	if tgCfg.WaitTimeoutSeconds != 60.0 {
		t.Errorf("WaitTimeoutSeconds=%v, want 60.0", tgCfg.WaitTimeoutSeconds)
	}
	if tgCfg.BackoffBypassPendingCount != 10 {
		t.Errorf("BackoffBypassPendingCount=%v, want 10", tgCfg.BackoffBypassPendingCount)
	}
	// Defaults that should come from DefaultTimingGateConfig
	if tgCfg.EngagedResetDecline != true {
		t.Error("EngagedResetDecline should default to true")
	}
}

func TestBuildFromConfig_FullPipeline(t *testing.T) {
	cfg := config.EngagementConfig{
		Enabled:               true,
		Channels:              []string{"misskey"},
		Keywords:              []string{"AI"},
		RateLimitCapacity:     5,
		RateLimitInterval:     30 * time.Minute,
		ReplyProbability:      0.5,
	}

	result := BuildFromConfig(cfg, "bot-123", nil)
	if result.Policy == nil {
		t.Fatal("expected non-nil policy")
	}
	if result.Gate == nil {
		t.Fatal("expected non-nil gate (ReplyProbability > 0)")
	}
	if result.RateLimit == nil {
		t.Fatal("expected non-nil rateLimit")
	}

	// 禁用模式 → 无 gate
	cfg2 := config.EngagementConfig{
		Enabled:          true,
		Channels:         []string{"misskey"},
		ReplyProbability: 0,
	}
	result2 := BuildFromConfig(cfg2, "bot-123", nil)
	if result2.Gate != nil {
		t.Error("expected nil gate when ReplyProbability=0")
	}
}

func TestBuildFromConfig_Disabled(t *testing.T) {
	cfg := config.DefaultEngagementConfig() // Enabled=false
	result := BuildFromConfig(cfg, "", nil)

	// DenyAll → 所有消息被拒绝
	msg := &core.Message{Source: "misskey", Text: "hello"}
	decision := result.Policy.Evaluate(nil, msg)
	if decision.Engage {
		t.Error("disabled config should reject all messages")
	}
}
