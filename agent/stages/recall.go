package stages

import (
	"context"
	"os"
	"strconv"
	"strings"

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
// 检索当前 bot 的统一 scope：同一机器人跨渠道、用户和群私聊共享记忆。
// 不读取未记录机器人归属的旧 channel/user scope，避免不同机器人串用同名会话。
//
// retriever 为 nil 或非致命错误时静默跳过（不阻断对话）。
type RecallStage struct {
	retriever memory.Retriever
	window    *memory.Window
	logger    *zap.SugaredLogger
	// recall 额外的快照配置（目前用于相关性召回开关与配额）。
	recall memory.SnapshotConfig
}

// NewRecallStage 创建记忆召回 stage。retriever 为 nil 时 stage 为空操作。
// window 可选：注入后记忆块字符上限由 Window.MemoryBudget()*3 派生，
// 取代硬编码的 2200，与 context.go 使用 window 模块的口径一致。
// recall 可选：传入记忆快照的额外配置（如相关性召回）；不传则全部走默认值。
func NewRecallStage(name string, retriever memory.Retriever, window *memory.Window, logger *zap.SugaredLogger, recall ...memory.SnapshotConfig) *RecallStage {
	_ = name // 名称保留给 future 多实例场景；Process 使用固定 Name()
	var extra memory.SnapshotConfig
	if len(recall) > 0 {
		extra = recall[0]
	}
	return &RecallStage{retriever: retriever, window: window, logger: logger, recall: extra}
}

// MemoryRelevanceEnabled 报告是否启用记忆相关性召回（默认开启）。
//
// 启用前提：目标 scope 的记忆量已远超主通道窗口（memory.recentPerScope）。
// 若 scope 记忆量本身就在窗口内，相关性通道没有候选可补，纯属浪费 IO。
//
// 环境变量只作为「紧急关闭」通道（设 0/false/off 关闭），不再是灰度开关。
// 关掉时，窗口外的旧事实不会被补回；打开时，与当前问句相关的旧事实可以排到前面。
//
// 开启后的两个副作用已同步修掉（否则不能放量）：
//  1. 补充通道独占预算 → 新增 RecalledBudgetChars 总配额（默认 1200）；
//  2. 约束类记忆（发言频率、回复边界这类行为约定）被相关性排序挤出 →
//     新增约束类置顶（PinnedCategories / MaxPinnedEntries）。
func MemoryRelevanceEnabled() bool {
	raw := strings.TrimSpace(os.Getenv("THINKBOT_MEMORY_RELEVANCE_RECALL"))
	if raw == "" {
		return true
	}
	if v, err := strconv.ParseBool(raw); err == nil {
		return v
	}
	return raw == "on" || raw == "yes"
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
	if botID == "" {
		return env, nil
	}
	scopes := []memory.Scope{memory.BotScope(botID)}

	// ModeFrozen：每次对话新建快照并立即检索，不跨轮缓存（对话频次低，开销可控）。
	// 注入 Window 使记忆块字符上限随模型上下文窗口自适应（window 模块）。
	snapCfg := memory.SnapshotConfig{Mode: memory.ModeFrozen}
	if s.window != nil {
		snapCfg.Window = s.window
	}
	// 当前输入始终作为相关性 query。窗口内加权不看 RelevanceRecall；
	// 若只在开关打开时赋值，关掉窗口外召回会把窗口内加权一起关掉。
	if q := strings.TrimSpace(env.Message.Text); q != "" {
		snapCfg.Query = q
	}
	// 窗口外召回另受开关控制。配额为 0 时由 Snapshot 回落默认值。
	// 单条长度封顶与近重复折叠不随本开关变化，它们修的是主通道预算，
	// 已在 SnapshotConfig 默认值里常开。
	if s.recall.RelevanceRecall {
		snapCfg.RelevanceRecall = true
		snapCfg.RelevanceCandidates = s.recall.RelevanceCandidates
		snapCfg.RelevanceTopK = s.recall.RelevanceTopK
		// 补充通道专用检索源（可选）：主链路不含 L0，而高价值老记忆常是 L0。
		snapCfg.BeyondWindowRetriever = s.recall.BeyondWindowRetriever
	}
	snap := memory.NewSnapshot(snapCfg)
	// 必须显式注入 logger：Snapshot 内部的「丢弃超长 / 折叠近重复」计数只在
	// logger 非 nil 时输出，缺了它运维无法判断记忆块到底被谁吃掉了预算
	// （一条上万字的档案就能把 2200 字符预算吃满到只剩 1 条）。
	snap.SetLogger(s.logger)
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
			"bot_id", botID, "scopes", len(scopes), "chars", len(text),
			"entries", snap.RenderedMemoryCount())
	}
	return env, nil
}
