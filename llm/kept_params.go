package llm

import "context"

// KeptParams 标记这次请求里哪些字段是调用方按当前快照算好的，跟随层不要再覆盖。
//
// Temperature 和 TopP 不再被跟随层保留。采样只来自钉住的模型配置：
// 未配置则省略，-1 由编码器省略。调用方写上的温度、重试抬温或分析器配置
// 都不能盖过模型。MaxTokens 和 ReasoningEffort 仍可保留，因为它们是在模型
// 上限之上按用途算过的封顶和词表映射。
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
