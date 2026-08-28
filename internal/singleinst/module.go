// Package singleinst 见 singleinst.go。本文件将其接入 fx 生命周期。
package singleinst

import (
	"context"
	"os"

	"go.uber.org/fx"
	"go.uber.org/zap"
)

// resolveAddr 取 API 监听地址：优先环境变量 API_ADDR，否则默认 :8080，
// 与 api/server.go 中 config.KeyAPIAddr 的默认值保持一致。
// 双实例只会在同机同端口部署时发生，因此用默认/环境变量地址探测即可。
func resolveAddr() string {
	if v := os.Getenv("API_ADDR"); v != "" {
		return v
	}
	return ":8080"
}

// Module 在应用依赖构造阶段执行单实例版本协商，必须排在 bot.Module 之前。
//
// 放在 Invoke（构造阶段、同步）而非 OnStart，是为了保证协商在 bot engine 启动
// 消费消息之前完成——fx 的 OnStart 是并发执行的，若放 OnStart 会存在「bot 已启动、
// 协商才判定让位」的重叠窗口。构造阶段按 module 注册顺序执行，singleinst 在
// bot.Module 之前，bot 的 OnStart 必然晚于协商结束。
//
// 协商不依赖 config.OnStart 的结果，仅用环境变量/默认端口探测，避免顺序耦合。
var Module = fx.Module("singleinst",
	fx.Invoke(func(logger *zap.SugaredLogger) {
		if err := Acquire(context.Background(), resolveAddr(), logger, func() { os.Exit(0) }); err != nil {
			if err == ErrYield {
				// selfExit 通常已 os.Exit，此处仅作保险。
				return
			}
			// 非让位错误（如等待旧实例释放端口超时）：宁可启动失败，也绝不留下双实例。
			logger.Errorw("singleinst: negotiation failed, aborting to avoid duplicate instances", "err", err)
			os.Exit(1)
		}
	}),
)
