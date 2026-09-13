package core

import "context"

type interruptCtxKey struct{}

// WithInterruptChannel 将一条消息生命周期内的「用户中途追加」通道绑定到 ctx。
func WithInterruptChannel(ctx context.Context, ch chan string) context.Context {
	return context.WithValue(ctx, interruptCtxKey{}, ch)
}

// InterruptChannelFromContext 取回绑定到 ctx 的中途追加通道；无则返回 nil。
func InterruptChannelFromContext(ctx context.Context) chan string {
	if v := ctx.Value(interruptCtxKey{}); v != nil {
		if ch, ok := v.(chan string); ok {
			return ch
		}
	}
	return nil
}
