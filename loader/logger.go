package loader

import (
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// NewLogger 构造 loader 自身使用的控制台 logger（带 [loader] 前缀风格的时间戳）。
// 输出到 stdout，不写文件——避免与受监管子进程的 ./logs 混淆。
func NewLogger() *zap.SugaredLogger {
	cfg := zap.NewProductionConfig()
	cfg.Encoding = "console"
	cfg.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
	cfg.EncoderConfig.EncodeLevel = zapcore.CapitalLevelEncoder
	cfg.DisableCaller = true
	cfg.DisableStacktrace = true
	l, err := cfg.Build()
	if err != nil {
		// 极端兜底：直接用无操作 logger，不影响主流程
		return zap.NewNop().Sugar()
	}
	return l.Sugar()
}
