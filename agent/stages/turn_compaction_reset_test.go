package stages

import (
	"context"
	"strings"
	"testing"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/llm"
)

// 回归背景：会话级 Compactor 跨轮复用，doom-loop 计数却是单轮保护。命中后
// ShouldCompactIn 直接短路，压缩内部清零不再执行 —— 修复前上一轮锁死会让
// 同一会话后续所有轮次永久禁用压缩。Process 每回合开头必须重置。

func lockedTurnCompactor(t *testing.T) (*llm.Compactor, llm.GenerateParams) {
	t.Helper()
	cfg := llm.DefaultCompactionConfig()
	cfg.ContextLength = 1_048_576
	cfg.TriggerCap = 10_000
	cfg.MinMessagesToCompact = 2
	c := llm.NewCompactor(cfg)

	// 纯文本消息：不触发 PruneToolOutputs，保证「压缩后仍溢出」从而累计计数。
	msgs := make([]llm.Message, 0, 8)
	for i := 0; i < 8; i++ {
		msgs = append(msgs, llm.Message{
			Role:    llm.MessageRoleAssistant,
			Content: []llm.MessagePart{llm.TextPart{Text: strings.Repeat("payload ", 900)}},
		})
	}
	params := llm.GenerateParams{System: "system prompt", Messages: msgs}

	hook := llm.CompactionPrepareStepWithTarget(c, nil, nil)(context.Background())
	for i := 0; i < llm.DoomLoopThreshold; i++ {
		p := params
		if hook(&p) == nil {
			t.Fatalf("fixture stopped at iteration %d before reaching the doom-loop threshold", i)
		}
	}
	if c.ShouldCompactIn(context.Background(), params) {
		t.Fatal("fixture must be locked by the doom-loop guard")
	}
	return c, params
}

func TestResetTurnCompaction_UnlocksSessionCompactorAcrossTurns(t *testing.T) {
	env := tgEnv("turn-reset")
	key := conversationKey(env)
	s := &LLMStage{}
	main, params := lockedTurnCompactor(t)
	s.compactors.Store(key, main)

	s.resetTurnCompaction(env)

	if !main.ShouldCompactIn(context.Background(), params) {
		t.Fatal("new turn must unlock compaction for the session compactor")
	}
}

func TestResetTurnCompaction_CoversSelfCompactorAndKeepsOtherSessions(t *testing.T) {
	env := tgEnv("turn-reset-multi")
	key := conversationKey(env)
	otherKey := conversationKey(tgEnv("other-session"))
	s := &LLMStage{}
	main, params := lockedTurnCompactor(t)
	self, _ := lockedTurnCompactor(t)
	untouched, otherParams := lockedTurnCompactor(t)
	s.compactors.Store(key, main)
	s.selfCompactors.Store(key, self)
	s.compactors.Store(otherKey, untouched)

	s.resetTurnCompaction(env)

	if !main.ShouldCompactIn(context.Background(), params) {
		t.Fatal("main compaction compactor must be reset")
	}
	if !self.ShouldCompactIn(context.Background(), params) {
		t.Fatal("compact_context compactor must be reset too")
	}
	if untouched.ShouldCompactIn(context.Background(), otherParams) {
		t.Fatal("reset must not touch a different conversation's compactor")
	}
}

func TestResetTurnCompaction_EmptyKeyIsNoop(t *testing.T) {
	s := &LLMStage{}
	// key 为空时 compactor 是一次性临时实例、不入表，重置应安全跳过。
	s.resetTurnCompaction(nil)
	s.resetTurnCompaction(core.NewEnvelope(core.Message{BotID: "bot-1"}))
}
