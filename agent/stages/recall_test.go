package stages

import (
	"context"
	"strings"
	"testing"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/agent/memory"
	"go.uber.org/zap"
)

// TestRecallStage_InjectsBotScopeMemory 验证「潜水学到的经验在真人交互里浮现」读侧：
// 召回 [bot, channel, user] 三 scope 的记忆时，bot 全局 scope 的潜水笔记应被召回
// （从 misskey 学到的在 web 对话也能用），且其他 bot 的笔记不串入。
func TestRecallStage_InjectsBotScopeMemory(t *testing.T) {
	repo := memory.NewMemoryRepository()
	botID := "bot-recall"
	if err := repo.Append(context.Background(), memory.Entry{
		Scope:    memory.BotScope(botID),
		Content:  "luna is building a Matrix homeserver with postgres",
		Category: "lurk",
	}); err != nil {
		t.Fatal(err)
	}
	// 其他 bot 的笔记不应被召回
	if err := repo.Append(context.Background(), memory.Entry{
		Scope:    memory.BotScope("other-bot"),
		Content:  "unrelated noise that must not leak",
		Category: "lurk",
	}); err != nil {
		t.Fatal(err)
	}

	stage := NewRecallStage("memory-recall", repo, nil, zap.NewNop().Sugar())
	env := core.NewEnvelope(core.Message{ID: "r1", BotID: botID, Channel: "web", UserID: "u1"})
	if _, err := stage.Process(context.Background(), env); err != nil {
		t.Fatalf("process err: %v", err)
	}
	v, ok := env.Get(core.KVMemoryRecall)
	if !ok {
		t.Fatal("KVMemoryRecall not set")
	}
	text, _ := v.(string)
	if text == "" {
		t.Fatal("expected non-empty recall text")
	}
	if !strings.Contains(text, "Matrix homeserver") {
		t.Errorf("recall should include bot-scope lurk note, got: %q", text)
	}
	if strings.Contains(text, "unrelated noise") {
		t.Errorf("recall should NOT include other bot's note, got: %q", text)
	}
}

// TestRecallStage_SkipsWhenNilRetriever 验证 retriever 为 nil 时安全空操作。
func TestRecallStage_SkipsWhenNilRetriever(t *testing.T) {
	stage := NewRecallStage("memory-recall", nil, nil, zap.NewNop().Sugar())
	env := core.NewEnvelope(core.Message{ID: "r2", BotID: "b"})
	if _, err := stage.Process(context.Background(), env); err != nil {
		t.Fatalf("process err: %v", err)
	}
	if _, ok := env.Get(core.KVMemoryRecall); ok {
		t.Error("nil retriever should not set KVMemoryRecall")
	}
}

// TestRecallStage_SkipsWhenNoScopes 验证无 bot/channel/user 标识时安全跳过。
func TestRecallStage_SkipsWhenNoScopes(t *testing.T) {
	repo := memory.NewMemoryRepository()
	stage := NewRecallStage("memory-recall", repo, nil, zap.NewNop().Sugar())
	env := core.NewEnvelope(core.Message{ID: "r3"}) // 全部为空
	if _, err := stage.Process(context.Background(), env); err != nil {
		t.Fatalf("process err: %v", err)
	}
	if _, ok := env.Get(core.KVMemoryRecall); ok {
		t.Error("empty scopes should not set KVMemoryRecall")
	}
}
