package llm

import "context"

// KeptParams 标记这次请求里哪些字段是调用方按当前快照算好的，跟随层不要再覆盖。
//
// 模型温度、top_p、最大输出是模型配置。轻量档上限、潜水低温、分析器全局温度、
// 操作者封顶是在模型值之上另加的策略。前者跟随热切换；后者由调用方标成保留。
type KeptParams struct {
	Temperature     bool
	TopP            bool
	MaxTokens       bool
	ReasoningEffort bool
}

type keptParamsKey struct{}

// WithKeptParams 把保留标记放进这次调用的 context。
func WithKeptParams(ctx context.Context, k KeptParams) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, keptParamsKey{}, k)
}

// KeptParamsFrom 读取保留标记。没有标记时零值表示全部跟随当前快照。
func KeptParamsFrom(ctx context.Context) KeptParams {
	if ctx == nil {
		return KeptParams{}
	}
	k, _ := ctx.Value(keptParamsKey{}).(KeptParams)
	return k
}
