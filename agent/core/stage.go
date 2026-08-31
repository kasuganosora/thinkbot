package core

import "context"

// ============================================================================
// Stage — Pipeline 处理单元
// ============================================================================

// Stage 是 Pipeline 的基本处理单元。
// 每个 Stage 接收一个 Envelope，进行处理后返回（可能是修改后的）Envelope。
//
// 约定：
//   - 返回 nil Envelope 表示消息被丢弃（Pipeline 中止后续 Stage）。
//   - 返回 error 时，Pipeline 根据错误类型决定是中止还是跳过。
//   - Stage 应该是无状态的或自行保证并发安全。
type Stage interface {
	// Name 返回 Stage 的唯一标识名称。
	// 用于 tracing span name、metrics label 和日志标签。
	Name() string

	// Process 处理消息信封。
	// ctx 携带 tracing span 和超时信息。
	Process(ctx context.Context, env *Envelope) (*Envelope, error)
}

// StageFunc 将普通函数适配为 Stage 接口。
type StageFunc struct {
	StageName string
	Fn        func(ctx context.Context, env *Envelope) (*Envelope, error)
}

// Name 返回 Stage 名称。
func (f *StageFunc) Name() string { return f.StageName }

// Process 执行处理函数。
func (f *StageFunc) Process(ctx context.Context, env *Envelope) (*Envelope, error) {
	return f.Fn(ctx, env)
}

// ============================================================================
// StageInfo — Stage 元数据（用于 fx 注册和排序）
// ============================================================================

// StageInfo 描述一个 Stage 及其在 Pipeline 中的元信息。
type StageInfo struct {
	// Stage 处理单元实例。
	Stage Stage
	// Order 排序权重，越小越靠前执行。
	Order int
	// Enabled 是否启用。false 时 Pipeline 跳过此 Stage。
	Enabled bool
}
