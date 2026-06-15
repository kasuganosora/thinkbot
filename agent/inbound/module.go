package inbound

import (
	"go.uber.org/fx"
)

// ============================================================================
// fx Module
// ============================================================================

// Module 是 inbound 子系统的 fx 模块。
// 它将所有注册的 Source 收集到 "inbound_sources" 分组中。
var Module = fx.Module("inbound",
	// 默认提供 MemorySource（开发/测试用）
	fx.Provide(
		fx.Annotate(
			func() *MemorySource { return NewMemorySource("memory", 64) },
			fx.ResultTags(`group:"inbound_sources"`),
			fx.As(new(Source)),
		),
	),
)

// ProvideSource 将 Source 构造器注册到 fx 的 "inbound_sources" 分组。
func ProvideSource(constructor any) fx.Option {
	return fx.Provide(
		fx.Annotate(
			constructor,
			fx.ResultTags(`group:"inbound_sources"`),
			fx.As(new(Source)),
		),
	)
}
