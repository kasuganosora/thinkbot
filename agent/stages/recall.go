package stages

import (
	"context"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/agent/memory"
	"go.uber.org/zap"
)

// RecallStage 在对话 LLM 生成前，从长期记忆（含潜水学到的经验）检索相关条目，
// 复用 memory.Snapshot 的渲染（威胁扫描 + 字符预算）拼成 "MEMORY" 块，
// 注入 KVMemoryRecall，供 LLMStage 拼入 system prompt。
//
// 这是「潜水学到的经验在真人交互里浮现」闭环的读侧。写入侧由 LLMStage 的潜水
// 观察者分支产出 ActionNote（bot 全局 scope），经 MultiStore 沉淀进 memory_entries
// 与 tiered_memories（L0→L1）；本 stage 在每个真人对话轮次前把相关记忆召回。
//
// 检索范围 [bot, channel, user] 三 scope：
//   - bot scope：潜水学到的跨渠道通用经验（关键，从 misskey 学到的在 web 对话也能用）；
//   - channel scope：当前渠道的对话流上下文；
//   - user scope：该用户的跨渠道长期偏好。
//
// retriever 为 nil 或非致命错误时静默跳过（不阻断对话）。
type RecallStage struct {
	retriever memory.Retriever
	logger    *zap.SugaredLogger
}

// NewRecallStage 创建记忆召回 stage。retriever 为 nil 时 stage 为空操作。
func NewRecallStage(name string, retriever memory.Retriever, logger *zap.SugaredLogger) *RecallStage {
	_ = name // 名称保留给 future 多实例场景；Process 使用固定 Name()
	return &RecallStage{retriever: retriever, logger: logger}
}

// Name 返回 stage 名称。
func (s *RecallStage) Name() string { return "memory-recall" }

// Process 检索并注入记忆。返回原 env（可能被注入 KVMemoryRecall）。
func (s *RecallStage) Process(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
	if s.retriever == nil {
		return env, nil
	}
	// 潜水（只读）消息：观察者自身产出的笔记不应被召回进自己的 prompt，
	// 避免记忆回环；同时省去一次无意义的检索 IO。
	if isLurkMode(env) {
		return env, nil
	}
	botID := env.Message.BotID
	channel := env.Message.Channel
	userID := env.Message.UserID

	scopes := make([]memory.Scope, 0, 3)
	if botID != "" {
		scopes = append(scopes, memory.BotScope(botID))
	}
	if channel != "" {
		scopes = append(scopes, memory.ChannelScope(channel))
	}
	if userID != "" {
		scopes = append(scopes, memory.UserScope(userID))
	}
	if len(scopes) == 0 {
		return env, nil
	}

	// ModeFrozen：每次对话新建快照并立即检索，不跨轮缓存（对话频次低，开销可控）。
	snap := memory.NewSnapshot(memory.SnapshotConfig{Mode: memory.ModeFrozen})
	if err := snap.Init(ctx, s.retriever, scopes); err != nil {
		if s.logger != nil {
			s.logger.Warnw("recall: snapshot init failed, skip memory injection",
				"bot_id", botID, "err", err)
		}
		return env, nil
	}
	text := snap.FullSnapshot()
	if text == "" {
		if s.logger != nil {
			s.logger.Debugw("recall: no long-term memory matched",
				"bot_id", botID, "scopes", len(scopes))
		}
		return env, nil
	}
	env.Set(core.KVMemoryRecall, text)
	if s.logger != nil {
		// INFO 级：运维需能直接观测「人味读侧」是否工作（默认 INFO 下可见）。
		s.logger.Infow("recall: injected long-term memory into prompt",
			"bot_id", botID, "scopes", len(scopes), "chars", len(text))
	}
	return env, nil
}
