package bot

import (
	"os"
	"testing"

	"github.com/kasuganosora/thinkbot/llm/openai"
)

// ============================================================================
// 集成测试共享辅助函数
//
// 需要设置以下环境变量：
//   INTEG_LLM_API_KEY  — LLM Provider API Key
//   INTEG_LLM_BASE_URL — LLM Provider Base URL（可选）
//   INTEG_LLM_MODEL    — 测试用模型 ID（默认 gpt-4o-mini）
//   INTEG_BOT_ID       — 测试 Bot ID（默认 test-bot）
// ============================================================================

// integConfig 集成测试配置。
type integConfig struct {
	Model     string
	MaxTokens int
	APIKey    string
	BaseURL   string
}

var integCfg = integConfig{
	Model:     envOr("INTEG_LLM_MODEL", "gpt-4o-mini"),
	MaxTokens: 4096,
	APIKey:    os.Getenv("INTEG_LLM_API_KEY"),
	BaseURL:   os.Getenv("INTEG_LLM_BASE_URL"),
}

var integBotID = envOr("INTEG_BOT_ID", "test-bot")

// skipIfShort 在 -short 模式或缺少 API Key 时跳过测试。
func skipIfShort(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	if integCfg.APIKey == "" {
		t.Skip("skipping integration test: INTEG_LLM_API_KEY not set")
	}
}

// setupIntegLLMBundle 创建集成测试用的 LLMBundle。
func setupIntegLLMBundle(t *testing.T) *LLMBundle {
	t.Helper()

	provider := openai.New(
		openai.WithAPIKey(integCfg.APIKey),
		openai.WithBaseURL(integCfg.BaseURL),
	)

	return &LLMBundle{
		Main:  provider,
		Light: provider,
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// truncate 截断字符串到指定最大长度。
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	if maxLen <= 3 {
		return s[:maxLen]
	}
	return s[:maxLen-3] + "..."
}

// intPtr 返回 int 指针（测试辅助）。
func intPtr(n int) *int { return &n }

// floatPtr 返回 float64 指针（测试辅助）。
func floatPtr(f float64) *float64 { return &f }
