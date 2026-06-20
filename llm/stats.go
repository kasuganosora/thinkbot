package llm

import "context"

// UsageMetric 描述一次 LLM 调用的使用统计维度。
// 由各 Stage（reply / chat / vision / memory 等）在 LLM 调用完成后构建，
// 传递给 UsageRecorder 进行按日聚合记录。
type UsageMetric struct {
	// BotID 标识哪个 Bot 发起的调用（从 Envelope bot.id 提取）。
	BotID string

	// Model 模型标识符（如 claude-sonnet-4-20250514）。
	Model string

	// Feature 功能维度，标记调用来源（如 "reply"、"chat"、"vision"、"memory_compress"）。
	Feature string

	// Usage 本次调用的 token 用量（含缓存明细）。
	Usage Usage

	// ToolCalls 本次编排过程中工具调用的总次数。
	ToolCalls int

	// Steps 编排步数。
	Steps int
}

// UsageRecorder 是使用统计记录器的抽象接口。
// 实现方（如 stats.Recorder）负责将指标按日聚合并持久化。
type UsageRecorder interface {
	// RecordUsage 异步记录一次使用指标。
	// 实现应确保非阻塞——即使录制失败也不影响主流程。
	RecordUsage(ctx context.Context, metric UsageMetric)
}
