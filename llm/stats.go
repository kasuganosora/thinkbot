package llm

import (
	"context"
	"time"
)

// UsageMetric 描述一次 LLM 调用的使用统计维度。
// 由各 Stage（reply / chat / vision / memory 等）在 LLM 调用完成后构建，
// 传递给 UsageRecorder 进行按日聚合记录。
type UsageMetric struct {
	// BotID 标识哪个 Bot 发起的调用（从 Envelope bot.id 提取）。
	BotID string

	// At 本次调用的发生时间（由构建方填充，默认零值将回退到 flush 时刻）。
	// flush 按此时间（而非 flush 时刻）归到对应自然日，避免跨日/延迟 flush 错日。
	At time.Time

	// Model 模型标识符（如 claude-sonnet-4-20250514）。
	Model string

	// Feature 功能维度，标记调用来源（如 "reply"、"chat"、"vision"、"memory_compress"）。
	Feature string

	// Channel 请求来源渠道（如 "telegram"、"web"、"misskey"）。
	// 非 pipeline 路径（dream、memory 等）为空。
	Channel string

	// Usage 本次调用的 token 用量（含缓存明细）。
	Usage Usage

	// ToolCalls 本次编排过程中工具调用的总次数。
	ToolCalls int

	// Steps 编排步数。
	Steps int

	// WorkflowID / NodeID 标记调用来自哪条工作流的哪个节点。
	//
	// 非工作流路径（reply / dream / memory_compress 等）两者均为空。
	//
	// **重要**：这两项刻意**不参与 UsageDaily 的聚合维度**（那是
	// bot/model/feature/channel/date 五维日聚合）。加入它们会把日聚合表
	// 撑成明细表并破坏唯一索引语义。此处仅用于旁路写入逐条明细表
	// （stats.WorkflowUsage），使「一条工作流花在哪」可回答。
	WorkflowID string
	NodeID     string
}

// UsageRecorder 是使用统计记录器的抽象接口。
// 实现方（如 stats.Recorder）负责将指标按日聚合并持久化。
type UsageRecorder interface {
	// RecordUsage 异步记录一次使用指标。
	// 实现应确保非阻塞——即使录制失败也不影响主流程。
	RecordUsage(ctx context.Context, metric UsageMetric)
}
