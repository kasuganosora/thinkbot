package skill

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kasuganosora/thinkbot/llm"
	"github.com/kasuganosora/thinkbot/llm/openai"
	"github.com/kasuganosora/thinkbot/util/log"
	"go.uber.org/zap"
)

func init() {
	// 初始化全局 logger（util/http 包依赖它）
	if log.Logger == nil {
		log.Logger = zap.NewNop().Sugar()
	}
}

// ============================================================================
// 真实 LLM 集成测试 — bangumi skill
//
// 环境变量（不设置则 skip）：
//
//	THINKBOT_TEST_LLM_API_KEY  — API Key（必填）
//	THINKBOT_TEST_LLM_BASE_URL — Base URL（可选，默认 https://api.openai.com）
//	THINKBOT_TEST_LLM_MODEL    — 模型（可选，默认 gpt-4o-mini）
//	THINKBOT_TEST_LLM_CHAT_MODE — 设为 "1" 启用 Chat Completions 模式（BigModel/DeepSeek 等）
//
// 运行：
//
//	THINKBOT_TEST_LLM_API_KEY=sk-xxx go test -v -run TestIntegration -timeout 120s ./skill/
// ============================================================================

// findBangumiSkillDir 查找 bangumi SKILL.md 所在目录。
// bangumi.skill 仓库的结构是 repo/skill/SKILL.md（不是 repo/SKILL.md）。
func findBangumiSkillDir(t *testing.T) string {
	t.Helper()
	candidates := []string{
		"../skills/bangumi/skill", // 从 skill/ 测试运行时的相对路径
		"./skills/bangumi/skill",  // 从项目根运行时的相对路径
	}
	for _, c := range candidates {
		if _, err := os.Stat(filepath.Join(c, "SKILL.md")); err == nil {
			abs, _ := filepath.Abs(c)
			return abs
		}
	}
	t.Skip("bangumi skill not found, skipping integration test")
	return ""
}

// createTestLLMProvider 从环境变量创建 LLM Provider。
func createTestLLMProvider(t *testing.T) llm.Provider {
	t.Helper()

	apiKey := os.Getenv("THINKBOT_TEST_LLM_API_KEY")
	if apiKey == "" {
		t.Skip("THINKBOT_TEST_LLM_API_KEY not set, skipping integration test")
	}

	baseURL := os.Getenv("THINKBOT_TEST_LLM_BASE_URL")
	model := os.Getenv("THINKBOT_TEST_LLM_MODEL")
	if model == "" {
		model = "gpt-4o-mini"
	}

	opts := []openai.Option{
		openai.WithAPIKey(apiKey),
		openai.WithTimeout(60 * time.Second),
	}
	if baseURL != "" {
		opts = append(opts, openai.WithBaseURL(baseURL))
	}
	if os.Getenv("THINKBOT_TEST_LLM_CHAT_MODE") == "1" {
		opts = append(opts, openai.WithChatMode())
	}
	// 自定义 chat completions 端点路径（BigModel 等供应商与默认 /v1/chat/completions 不同）
	if chatPath := os.Getenv("THINKBOT_TEST_LLM_CHAT_PATH"); chatPath != "" {
		opts = append(opts, openai.WithChatPath(chatPath))
	}

	provider := openai.New(opts...)
	t.Logf("LLM provider: openai, model=%s, baseURL=%s, chatMode=%s",
		model, defaultStr(baseURL, "https://api.openai.com"),
		os.Getenv("THINKBOT_TEST_LLM_CHAT_MODE"))
	return provider
}

func defaultStr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// TestIntegration_LoadBangumiSkill 测试从文件系统加载 bangumi SKILL.md。
func TestIntegration_LoadBangumiSkill(t *testing.T) {
	skillDir := findBangumiSkillDir(t)

	loader := NewLoader(skillDir, nil)
	s, err := loader.LoadSkill(skillDir)
	if err != nil {
		t.Fatalf("LoadSkill: %v", err)
	}

	if s.Name != "bangumi" {
		t.Errorf("name: got %q, want \"bangumi\"", s.Name)
	}
	if s.Description == "" {
		t.Error("description is empty")
	}
	if s.Content == "" {
		t.Error("content is empty")
	}
	if !s.Enabled {
		t.Error("should be enabled by default")
	}

	t.Logf("✓ Loaded skill: name=%s, description length=%d, content length=%d",
		s.Name, len(s.Description), len(s.Content))
	t.Logf("  Description preview: %s", truncate(s.Description, 80))
}

// TestIntegration_TriggerPrompt 验证 BuildTriggerPrompt 包含 bangumi。
func TestIntegration_TriggerPrompt(t *testing.T) {
	skillDir := findBangumiSkillDir(t)

	loader := NewLoader(skillDir, nil)
	s, err := loader.LoadSkill(skillDir)
	if err != nil {
		t.Fatalf("LoadSkill: %v", err)
	}

	mgr := NewSkillManager(nil, nil, nil)
	mgr.Register(s)

	trigger := mgr.BuildTriggerPrompt()
	t.Logf("Trigger prompt:\n%s", trigger)

	if !strings.Contains(trigger, "bangumi") {
		t.Error("trigger prompt should contain 'bangumi'")
	}
	if !strings.Contains(trigger, "use_skill") {
		t.Error("trigger prompt should contain use_skill instruction")
	}
}

// TestIntegration_LLM_TriggerDetection 发送应该触发 bangumi skill 的用户消息，
// 验证 LLM 输出 <use_skill: bangumi>。
func TestIntegration_LLM_TriggerDetection(t *testing.T) {
	skillDir := findBangumiSkillDir(t)
	provider := createTestLLMProvider(t)
	model := os.Getenv("THINKBOT_TEST_LLM_MODEL")
	if model == "" {
		model = "gpt-4o-mini"
	}

	// 1. 加载 skill 并构建 trigger prompt
	loader := NewLoader(skillDir, nil)
	s, err := loader.LoadSkill(skillDir)
	if err != nil {
		t.Fatalf("LoadSkill: %v", err)
	}

	mgr := NewSkillManager(nil, nil, nil)
	mgr.Register(s)

	systemPrompt := mgr.BuildTriggerPrompt()

	// 2. 发送应触发 skill 的用户消息
	testCases := []struct {
		name    string
		message string
	}{
		{"daily_calendar", "今天有什么新番放送？"},
		{"search_anime", "帮我搜一下《葬送的芙莉莲》这部动画"},
		{"mark_watching", "我想把《鬼灭之刃》标记为在看"},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := provider.DoGenerate(ctx, llm.GenerateParams{
				Model:  llm.ChatModel(model),
				System: systemPrompt,
				Messages: []llm.Message{
					llm.UserMessage(tc.message),
				},
			})
			if err != nil {
				t.Fatalf("DoGenerate: %v", err)
			}

			t.Logf("=== %s ===", tc.name)
			t.Logf("User: %s", tc.message)
			t.Logf("LLM: %s", result.Text)
			t.Logf("Usage: input=%d output=%d", result.Usage.InputTokens, result.Usage.OutputTokens)

			// 验证 LLM 是否输出了 <use_skill: bangumi>
			triggered := mgr.TriggerIfNeeded(result.Text)
			if triggered == "" {
				t.Errorf("LLM did not trigger any skill. Output:\n%s", result.Text)
				return
			}
			if triggered != "bangumi" {
				t.Errorf("triggered skill: got %q, want \"bangumi\"", triggered)
				return
			}
			t.Logf("✓ Skill triggered: %s", triggered)
		})
	}
}

// TestIntegration_LLM_SkillInjection 测试完整的 skill 注入流程：
// 1. 第一轮：LLM 输出 <use_skill: bangumi>
// 2. 注入 SKILL.md 正文到 system prompt
// 3. 第二轮：LLM 根据注入的指令生成正确的 CLI 命令
func TestIntegration_LLM_SkillInjection(t *testing.T) {
	skillDir := findBangumiSkillDir(t)
	provider := createTestLLMProvider(t)
	model := os.Getenv("THINKBOT_TEST_LLM_MODEL")
	if model == "" {
		model = "gpt-4o-mini"
	}

	// 加载 skill
	loader := NewLoader(skillDir, nil)
	s, err := loader.LoadSkill(skillDir)
	if err != nil {
		t.Fatalf("LoadSkill: %v", err)
	}

	mgr := NewSkillManager(nil, nil, nil)
	mgr.Register(s)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	userMsg := "帮我搜一下《葬送的芙莉莲》这部动画"

	// === 第一轮：trigger detection ===
	triggerPrompt := mgr.BuildTriggerPrompt()
	t.Logf("=== Round 1: Trigger Detection ===")

	round1, err := provider.DoGenerate(ctx, llm.GenerateParams{
		Model:  llm.ChatModel(model),
		System: triggerPrompt,
		Messages: []llm.Message{
			llm.UserMessage(userMsg),
		},
	})
	if err != nil {
		t.Fatalf("Round 1 DoGenerate: %v", err)
	}

	t.Logf("User: %s", userMsg)
	t.Logf("LLM Response: %s", round1.Text)

	triggered := mgr.TriggerIfNeeded(round1.Text)
	if triggered != "bangumi" {
		t.Fatalf("Round 1: expected skill 'bangumi' to be triggered, got %q. Full output:\n%s",
			triggered, round1.Text)
	}
	t.Logf("✓ Round 1: skill 'bangumi' triggered")

	// === 第二轮：注入 skill content，验证 LLM 能生成正确的 CLI 命令 ===
	t.Logf("\n=== Round 2: Skill Content Injection ===")

	injector := NewDirectInjector()
	fullSystemPrompt := injector.Inject(triggerPrompt, s)

	t.Logf("System prompt length: %d (trigger=%d + skill=%d)",
		len(fullSystemPrompt), len(triggerPrompt), len(s.Content))

	round2, err := provider.DoGenerate(ctx, llm.GenerateParams{
		Model:  llm.ChatModel(model),
		System: fullSystemPrompt,
		Messages: []llm.Message{
			llm.UserMessage(userMsg),
		},
	})
	if err != nil {
		t.Fatalf("Round 2 DoGenerate: %v", err)
	}

	t.Logf("LLM Response: %s", round2.Text)
	t.Logf("Usage: input=%d output=%d", round2.Usage.InputTokens, round2.Usage.OutputTokens)

	// 验证 LLM 生成了正确的 bangumi CLI 命令
	responseLower := strings.ToLower(round2.Text)
	hasCommand := strings.Contains(responseLower, "bangumi") &&
		(strings.Contains(responseLower, "search") ||
			strings.Contains(responseLower, "calendar") ||
			strings.Contains(responseLower, "collection") ||
			strings.Contains(responseLower, "subject"))

	if !hasCommand {
		t.Errorf("Round 2: LLM response should contain bangumi CLI command. Output:\n%s", round2.Text)
	} else {
		t.Logf("✓ Round 2: LLM generated bangumi command reference")
	}

	// 检查 LLM 是否用了正确的命令格式（skill/bangumi ...）
	if strings.Contains(round2.Text, "skill/bangumi") || strings.Contains(round2.Text, "bangumi ") {
		t.Logf("✓ Round 2: correct command format detected")
	} else {
		t.Logf("⚠ Round 2: command format may need review (no 'skill/bangumi' prefix found)")
	}
}

// TestIntegration_LLM_NegativeCase 测试不相关的用户消息不会触发 skill。
func TestIntegration_LLM_NegativeCase(t *testing.T) {
	skillDir := findBangumiSkillDir(t)
	provider := createTestLLMProvider(t)
	model := os.Getenv("THINKBOT_TEST_LLM_MODEL")
	if model == "" {
		model = "gpt-4o-mini"
	}

	loader := NewLoader(skillDir, nil)
	s, err := loader.LoadSkill(skillDir)
	if err != nil {
		t.Fatalf("LoadSkill: %v", err)
	}

	mgr := NewSkillManager(nil, nil, nil)
	mgr.Register(s)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	userMsg := "今天天气怎么样？"

	result, err := provider.DoGenerate(ctx, llm.GenerateParams{
		Model:  llm.ChatModel(model),
		System: mgr.BuildTriggerPrompt(),
		Messages: []llm.Message{
			llm.UserMessage(userMsg),
		},
	})
	if err != nil {
		t.Fatalf("DoGenerate: %v", err)
	}

	t.Logf("User: %s", userMsg)
	t.Logf("LLM: %s", result.Text)

	triggered := mgr.TriggerIfNeeded(result.Text)
	if triggered == "bangumi" {
		t.Errorf("bangumi skill should NOT be triggered for weather question")
	} else {
		t.Logf("✓ Correctly not triggered for unrelated question")
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// TestIntegration_Summary 打印集成测试的运行说明。
func TestIntegration_Summary(t *testing.T) {
	t.Log(`
========================================
  Skill 集成测试运行说明
========================================

  本测试套件使用真实 LLM 验证 skill 系统的端到端流程：
    1. 从文件系统加载 bangumi SKILL.md
    2. 构建 trigger prompt 注入 system prompt
    3. 发送用户消息，验证 LLM 自主触发 <use_skill: bangumi>
    4. 注入 SKILL.md 正文，验证 LLM 生成正确 CLI 命令
    5. 负面测试：不相关问题不触发 skill

  运行方式：
    THINKBOT_TEST_LLM_API_KEY=sk-xxx \
    THINKBOT_TEST_LLM_BASE_URL=https://api.openai.com \
    THINKBOT_TEST_LLM_MODEL=gpt-4o-mini \
    go test -v -run TestIntegration -timeout 180s ./skill/

  BigModel（智谱）示例：
    THINKBOT_TEST_LLM_API_KEY=xxx \
    THINKBOT_TEST_LLM_BASE_URL=https://open.bigmodel.cn/api/paas/v4 \
    THINKBOT_TEST_LLM_MODEL=glm-4-flash \
    THINKBOT_TEST_LLM_CHAT_MODE=1 \
    go test -v -run TestIntegration -timeout 180s ./skill/

========================================
`)
	fmt.Println("Skill integration test suite ready.")
}
