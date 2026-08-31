package inbound

import (
	"go.uber.org/fx"
)

// ============================================================================
// fx Module
// ============================================================================

// Module 是 inbound 子系统的 fx 模块。
// 它提供 Ingress（消息入口网关）作为 Pipeline 的输入端。
//
// 各 channel 输入端（webhook、websocket 等）通过 fx 注入 *Ingress，
// 然后调用 ingress.Receive(ctx, msg) 注入消息。
var Module = fx.Module("inbound",
	// 提供默认 IngressConfig
	fx.Provide(func() IngressConfig {
		return DefaultIngressConfig()
	}),

	// 提供 Ingress
	fx.Provide(NewIngress),
)
