package tools

import (
	"context"
	"testing"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/agent/prompt"
	"github.com/kasuganosora/thinkbot/llm"
	"go.uber.org/zap"
)

// ============================================================================
// Mock Channel 实现 ChannelToolProvider
// ============================================================================

type mockChannel struct {
	name        string
	channelType string
	tools       []ToolDef
}

func newMockChannel(name, channelType string, tools []ToolDef) *mockChannel {
	return &mockChannel{
		name:        name,
		channelType: channelType,
		tools:       tools,
	}
}

func (m *mockChannel) ChannelTools(ctx context.Context) ([]ToolDef, error) {
	return m.tools, nil
}

func (m *mockChannel) Name() string  { return m.name }
func (m *mockChannel) Type() string  { return m.channelType }
func (m *mockChannel) BotID() string { return "test-bot" }

func makeMockTool(name, description string) ToolDef {
	return ToolDef{
		Tool: llm.Tool{
			Name:        name,
			Description: description,
			Parameters: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
			Execute: llm.ToolExecuteFunc(func(ctx *llm.ToolExecContext, input any) (any, error) {
				return map[string]any{"ok": true}, nil
			}),
		},
		Category: "mock",
	}
}

// ============================================================================
// 测试：ChannelToolProvider 注册后工具可被解析
// ============================================================================

func TestChannelTools_RegistrationAndResolution(t *testing.T) {
	promptReg := prompt.NewRegistry()
	mgr := NewToolManager(promptReg, nil, zap.NewNop().Sugar())

	// 注册 mock channel 工具
	mkTools := []ToolDef{
		makeMockTool("misskey_follow_user", "Follow a user on Misskey"),
		makeMockTool("misskey_create_note", "Create a note on Misskey"),
	}
	mkCh := newMockChannel("mk-bot", "misskey", mkTools)

	tgTools := []ToolDef{
		makeMockTool("telegram_ban_member", "Ban a member on Telegram"),
		makeMockTool("telegram_pin_message", "Pin a message on Telegram"),
	}
	tgCh := newMockChannel("tg-bot", "telegram", tgTools)

	// 模拟 StartBot 中的注册流程
	allChannels := []any{tgCh, mkCh}
	for _, ch := range allChannels {
		if ctp, ok := ch.(ChannelToolProvider); ok {
			defs, err := ctp.ChannelTools(context.Background())
			if err != nil {
				t.Fatalf("ChannelTools failed: %v", err)
			}
			for _, def := range defs {
				if err := mgr.Register(def); err != nil {
					t.Fatalf("Register failed for %s: %v", def.Name, err)
				}
			}
		}
	}

	// 验证：从 Telegram 来源请求时，所有工具（含 Misskey）都应可用
	tgEnv := &core.Envelope{
		Message: core.Message{
			BotID:    "test-bot",
			Channel:  "12345",
			ChatType: core.ChatPrivate,
			UserID:   "user1",
			Metadata: map[string]any{
				"channel_type": "telegram",
			},
		},
	}
	tools, err := mgr.ResolveForEnvelope(context.Background(), tgEnv)
	if err != nil {
		t.Fatalf("ResolveForEnvelope failed: %v", err)
	}

	// 所有 4 个工具都应该可用（跨 Channel）
	if len(tools) < 4 {
		t.Errorf("expected at least 4 tools, got %d: %v", len(tools), toolNames(tools))
	}

	hasMisskey := false
	hasTelegram := false
	for _, tool := range tools {
		switch tool.Name {
		case "misskey_follow_user", "misskey_create_note":
			hasMisskey = true
		case "telegram_ban_member", "telegram_pin_message":
			hasTelegram = true
		}
	}
	if !hasMisskey {
		t.Error("expected Misskey tools to be available from Telegram source (cross-channel)")
	}
	if !hasTelegram {
		t.Error("expected Telegram tools to be available from Telegram source")
	}
}

// ============================================================================
// 测试：ToolSessionContext.SourceChannelType 正确读取
// ============================================================================

func TestChannelTools_SourceChannelType(t *testing.T) {
	// Telegram 来源
	env := &core.Envelope{
		Message: core.Message{
			BotID:    "test-bot",
			Channel:  "12345",
			ChatType: core.ChatPrivate,
			UserID:   "user1",
			Metadata: map[string]any{
				"channel_type": "telegram",
			},
		},
	}
	sctx := envelopeToSessionContext(env)
	if sctx.SourceChannelType != "telegram" {
		t.Errorf("expected SourceChannelType=telegram, got %q", sctx.SourceChannelType)
	}

	// Misskey 来源
	env2 := &core.Envelope{
		Message: core.Message{
			BotID:   "test-bot",
			Channel: "timeline",
			UserID:  "user2",
			Metadata: map[string]any{
				"channel_type": "misskey",
			},
		},
	}
	sctx2 := envelopeToSessionContext(env2)
	if sctx2.SourceChannelType != "misskey" {
		t.Errorf("expected SourceChannelType=misskey, got %q", sctx2.SourceChannelType)
	}

	// 无 channel_type 的旧消息（向后兼容）
	env3 := &core.Envelope{
		Message: core.Message{
			BotID:  "test-bot",
			UserID: "user3",
		},
	}
	sctx3 := envelopeToSessionContext(env3)
	if sctx3.SourceChannelType != "" {
		t.Errorf("expected empty SourceChannelType for legacy message, got %q", sctx3.SourceChannelType)
	}
}

// ============================================================================
// 测试：跨 Channel 工具调用 — 从 Misskey 源调用 Telegram 工具
// ============================================================================

func TestChannelTools_CrossChannelFromMisskey(t *testing.T) {
	promptReg := prompt.NewRegistry()
	mgr := NewToolManager(promptReg, nil, zap.NewNop().Sugar())

	// 只注册一个 Telegram 工具
	tgTools := []ToolDef{
		makeMockTool("telegram_get_chat_info", "Get Telegram chat info"),
	}
	tgCh := newMockChannel("tg-bot", "telegram", tgTools)
	defs, _ := tgCh.ChannelTools(context.Background())
	for _, def := range defs {
		_ = mgr.Register(def)
	}

	// 从 Misskey 来源请求工具
	mkEnv := &core.Envelope{
		Message: core.Message{
			BotID:   "test-bot",
			Channel: "misskey:timeline",
			UserID:  "mk_user1",
			Metadata: map[string]any{
				"channel_type": "misskey",
			},
		},
	}
	tools, err := mgr.ResolveForEnvelope(context.Background(), mkEnv)
	if err != nil {
		t.Fatalf("ResolveForEnvelope failed: %v", err)
	}

	found := false
	for _, tool := range tools {
		if tool.Name == "telegram_get_chat_info" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected telegram_get_chat_info to be available from Misskey source (cross-channel)")
	}
}

// ============================================================================
// 辅助函数
// ============================================================================

func toolNames(tools []llm.Tool) []string {
	names := make([]string, len(tools))
	for i, t := range tools {
		names[i] = t.Name
	}
	return names
}
