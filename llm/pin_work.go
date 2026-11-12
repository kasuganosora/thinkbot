package llm

import "context"

// workPinner 由跟随当前模型快照的 provider 实现。
// 一次子代理或后台多步调用在入口钉住快照，步与步之间不换模型。
type workPinner interface {
	PinWork(context.Context) context.Context
}

// PinWork 在 provider 支持时钉住这一次工作的模型快照。
func PinWork(ctx context.Context, p Provider) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if w, ok := p.(workPinner); ok && w != nil {
		return w.PinWork(ctx)
	}
	return ctx
}
