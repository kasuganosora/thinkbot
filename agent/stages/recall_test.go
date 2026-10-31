package stages

import (
	"context"
	"strings"
	"testing"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/agent/memory"
	"github.com/kasuganosora/thinkbot/agent/outbound"
	"go.uber.org/zap"
)

// TestRecallStage_InjectsBotScopeMemory 验证「潜水学到的经验在真人交互里浮现」读侧：
// 召回当前 bot scope 的记忆时，bot 全局 scope 的潜水笔记应被召回
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

// 实时写入与自动注入使用同一 bot 空间：跨平台、用户和群私聊共享，跨 bot 隔离。
func TestRecallStage_BotSharingBoundary(t *testing.T) {
	ctx := context.Background()
	repo := memory.NewMemoryRepository()
	writer := memory.NewNoteWriterAdapter(repo, "bot-a")
	if err := writer.WriteNote(ctx, outbound.NoteEntry{
		ScopeKind: "bot", ScopeID: "forged-bot",
		Content: "演示环境日志主题为 demo_events", Category: "fact",
		Metadata: map[string]any{"bot_id": "forged-bot", "user_id": "user-a"},
	}); err != nil {
		t.Fatal(err)
	}
	for _, entry := range []memory.Entry{
		{Scope: memory.BotScope("bot-b"), Content: "foreign bot sentinel"},
		{Scope: memory.ChannelScope("wecom:dm:user-a"), Content: "legacy channel sentinel"},
		{Scope: memory.UserScope("user-a"), Content: "legacy user sentinel"},
	} {
		if err := repo.Append(ctx, entry); err != nil {
			t.Fatal(err)
		}
	}
	stage := NewRecallStage("memory-recall", repo, nil, zap.NewNop().Sugar())
	for _, tc := range []struct {
		name, bot, channel, user string
		wantShared               bool
	}{
		{"original private chat", "bot-a", "wecom:dm:user-a", "user-a", true},
		{"different platform and user", "bot-a", "web:session", "user-b", true},
		{"group chat", "bot-a", "wecom:chat:group", "user-c", true},
		{"other bot same identifiers", "bot-b", "wecom:dm:user-a", "user-a", false},
		{"missing bot same identifiers", "", "wecom:dm:user-a", "user-a", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := core.NewEnvelope(core.Message{
				BotID: tc.bot, Channel: tc.channel, UserID: tc.user,
				Metadata: map[string]any{core.MetaMemoryLinkedChannels: []string{"wecom:dm:user-a"}},
			})
			if _, err := stage.Process(ctx, env); err != nil {
				t.Fatal(err)
			}
			value, _ := env.Get(core.KVMemoryRecall)
			text, _ := value.(string)
			if strings.Contains(text, "demo_events") != tc.wantShared {
				t.Fatalf("shared memory presence = %v, want %v: %s", strings.Contains(text, "demo_events"), tc.wantShared, text)
			}
			if strings.Contains(text, "legacy") {
				t.Fatalf("unowned legacy memory leaked: %s", text)
			}
			if tc.bot != "bot-b" && strings.Contains(text, "foreign bot sentinel") {
				t.Fatalf("other bot memory leaked: %s", text)
			}
		})
	}
}

// TestRecallStage_InWindowBoostWithoutBeyondWindowRecall 关掉窗口外召回时，
// 当前输入仍要作为 query，否则窗口内加权一起失效。
func TestRecallStage_InWindowBoostWithoutBeyondWindowRecall(t *testing.T) {
	ctx := context.Background()
	repo := memory.NewMemoryRepository()
	ch := "web-demo"
	topics := []string{"周会结论", "磁盘巡检", "发布窗口", "告警统计", "依赖升级", "容量评估", "值班安排",
		"代码评审", "压测结果", "灰度比例", "回滚预案", "监控覆盖", "权限梳理", "工单流转",
		"文档归档", "培训计划", "招聘进度", "预算核算", "合规检查", "资产盘点"}
	for i, topic := range topics {
		if err := repo.Append(ctx, memory.Entry{
			Scope:      memory.BotScope("bot-inwindow"),
			Content:    topic + "：" + strings.Repeat("无关细节", 40),
			Importance: 0.90,
		}); err != nil {
			t.Fatal(err)
		}
		_ = i
	}
	if err := repo.Append(ctx, memory.Entry{
		Scope:      memory.BotScope("bot-inwindow"),
		Content:    "演示环境日志主题为 demo_events，可用日志通道查询",
		Importance: 0.658,
	}); err != nil {
		t.Fatal(err)
	}

	stage := NewRecallStage("memory-recall", repo, nil, zap.NewNop().Sugar())
	env := core.NewEnvelope(core.Message{
		ID: "r-inwindow", BotID: "bot-inwindow", Channel: ch, Text: "演示环境 日志主题 在哪查",
	})
	if _, err := stage.Process(ctx, env); err != nil {
		t.Fatal(err)
	}
	text, _ := env.Get(core.KVMemoryRecall)
	s, _ := text.(string)
	if !strings.Contains(s, "demo_events") {
		t.Fatalf("in-window fact must survive without beyond-window recall, got %q", s)
	}
	if strings.Contains(s, "relevance") {
		t.Fatalf("injected block must not include relevance scores, got %q", s)
	}
}
