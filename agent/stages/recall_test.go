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

// TestRecallStage_UnionsLinkedChannels 覆盖跨会话召回：私聊里说过的环境事实，
// 在群里提问时也要能被召回（记忆按 channel 分作用域，群 scope 里本来没有）。
// 关联会话由渠道层声明（core.MetaMemoryLinkedChannels），读侧只读不写。
func TestRecallStage_UnionsLinkedChannels(t *testing.T) {
	ctx := context.Background()
	repo := memory.NewMemoryRepository()
	botID := "bot-link"

	// 私聊里存过的环境事实。群里提问时，声明了关联会话就要能召回。
	if err := repo.Append(ctx, memory.Entry{
		Scope:      memory.ChannelScope("wecom:dm:demo-user"),
		Content:    "演示环境日志主题为 demo_events",
		Category:   "fact",
		Importance: 0.8,
	}); err != nil {
		t.Fatal(err)
	}
	// 群自己的记忆
	if err := repo.Append(ctx, memory.Entry{
		Scope:      memory.ChannelScope("wecom:chat:g1"),
		Content:    "群里讨论过的群内事项",
		Category:   "fact",
		Importance: 0.4,
	}); err != nil {
		t.Fatal(err)
	}

	stage := NewRecallStage("memory-recall", repo, nil, zap.NewNop().Sugar())

	// 未声明关联会话：调不到私聊内容（改动前的行为）
	plain := core.NewEnvelope(core.Message{
		ID: "r3", BotID: botID, Channel: "wecom:chat:g1", UserID: "demo-user",
		Text: "查一下演示环境的日志",
	})
	if _, err := stage.Process(ctx, plain); err != nil {
		t.Fatal(err)
	}
	before, _ := plain.Get(core.KVMemoryRecall)
	if s, _ := before.(string); strings.Contains(s, "demo_events") {
		t.Fatalf("without linked channels the DM fact must not appear, got %q", s)
	}

	// 声明关联会话后：私聊事实进入召回
	linked := core.NewEnvelope(core.Message{
		ID: "r4", BotID: botID, Channel: "wecom:chat:g1", UserID: "demo-user",
		Text: "查一下演示环境的日志",
		Metadata: map[string]any{
			core.MetaMemoryLinkedChannels: []string{"wecom:dm:demo-user"},
		},
	})
	if _, err := stage.Process(ctx, linked); err != nil {
		t.Fatal(err)
	}
	after, ok := linked.Get(core.KVMemoryRecall)
	if !ok {
		t.Fatal("KVMemoryRecall not set")
	}
	text, _ := after.(string)
	if !strings.Contains(text, "demo_events") {
		t.Fatalf("linked channel memory must be recalled, got %q", text)
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
			Scope:      memory.ChannelScope(ch),
			Content:    topic + "：" + strings.Repeat("无关细节", 40),
			Importance: 0.90,
		}); err != nil {
			t.Fatal(err)
		}
		_ = i
	}
	if err := repo.Append(ctx, memory.Entry{
		Scope:      memory.ChannelScope(ch),
		Content:    "演示环境日志主题为 demo_events，可用日志通道查询",
		Importance: 0.658,
	}); err != nil {
		t.Fatal(err)
	}

	stage := NewRecallStage("memory-recall", repo, nil, zap.NewNop().Sugar())
	env := core.NewEnvelope(core.Message{
		ID: "r-inwindow", Channel: ch, Text: "演示环境 日志主题 在哪查",
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

// TestAppendLinkedScopes_DedupesAndIgnoresBadType 覆盖去重与类型防御。
func TestAppendLinkedScopes_DedupesAndIgnoresBadType(t *testing.T) {
	stage := NewRecallStage("memory-recall", nil, nil, zap.NewNop().Sugar())
	base := []memory.Scope{
		memory.BotScope("b"),
		memory.ChannelScope("wecom:chat:g1"),
	}

	// 重复声明当前会话：不应出现两次
	env := core.NewEnvelope(core.Message{
		Channel:  "wecom:chat:g1",
		Metadata: map[string]any{core.MetaMemoryLinkedChannels: []string{"wecom:chat:g1", "wecom:dm:U1"}},
	})
	got := stage.appendLinkedScopes(base, env)
	if len(got) != 3 {
		t.Fatalf("want 3 scopes after dedupe, got %d: %+v", len(got), got)
	}
	if got[2].Key() != "channel:wecom:dm:U1" {
		t.Fatalf("linked scope = %q", got[2].Key())
	}

	// 类型不对：不应 panic，也不应新增
	bad := core.NewEnvelope(core.Message{
		Channel:  "wecom:chat:g1",
		Metadata: map[string]any{core.MetaMemoryLinkedChannels: "wecom:dm:U1"},
	})
	if n := len(stage.appendLinkedScopes(base, bad)); n != len(base) {
		t.Fatalf("non-[]string metadata must be ignored, got %d scopes", n)
	}
}
