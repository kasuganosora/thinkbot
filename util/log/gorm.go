package log

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	gormlogger "gorm.io/gorm/logger"
)

// GormLogLevel 映射字符串到 gorm 日志级别，方便配置使用。
type GormLogLevel string

const (
	GormSilent GormLogLevel = "silent"
	GormError  GormLogLevel = "error"
	GormWarn   GormLogLevel = "warn"
	GormInfo   GormLogLevel = "info"
)

// GormConfig GORM 日志配置。
type GormConfig struct {
	// Level 日志级别：silent / error / warn / info，默认 warn。
	Level GormLogLevel
	// SlowThreshold 慢查询阈值，超过此值的查询将以 Warn 级别记录，默认 200ms。
	SlowThreshold time.Duration
	// IgnoreRecordNotFoundError 是否忽略 ErrRecordNotFound，默认 true（避免噪音）。
	IgnoreRecordNotFoundError bool
	// ParameterizedQueries 是否在日志中展示参数值，默认 false（安全考虑）。
	ParameterizedQueries bool
}

// DefaultGormConfig 返回合理的 GORM 日志默认配置。
func DefaultGormConfig() GormConfig {
	return GormConfig{
		Level:                     GormWarn,
		SlowThreshold:             200 * time.Millisecond,
		IgnoreRecordNotFoundError: true,
	}
}

// gormLogger 实现 gorm.io/gorm/logger.Interface，将日志转发到 zap。
type gormLogger struct {
	zl                       *zap.Logger
	level                    gormlogger.LogLevel
	slowThreshold            time.Duration
	ignoreRecordNotFoundError bool
}

// NewGormLogger 基于全局 Logger 创建一个 GORM logger.Interface 实现。
// 需要在 Init / InitWithConfig 之后调用。
func NewGormLogger(cfg GormConfig) gormlogger.Interface {
	zl := zap.L() // fallback
	if Logger != nil {
		zl = Logger.Desugar().With(zap.String("module", "gorm"))
	}

	level := gormlogger.Warn
	switch cfg.Level {
	case GormSilent:
		level = gormlogger.Silent
	case GormError:
		level = gormlogger.Error
	case GormWarn:
		level = gormlogger.Warn
	case GormInfo:
		level = gormlogger.Info
	}

	slowThreshold := cfg.SlowThreshold
	if slowThreshold <= 0 {
		slowThreshold = 200 * time.Millisecond
	}

	return &gormLogger{
		zl:                       zl,
		level:                    level,
		slowThreshold:            slowThreshold,
		ignoreRecordNotFoundError: cfg.IgnoreRecordNotFoundError,
	}
}

// NewGormLoggerWithZap 使用指定的 zap.Logger 创建 GORM logger（不依赖全局变量）。
func NewGormLoggerWithZap(zl *zap.Logger, cfg GormConfig) gormlogger.Interface {
	level := gormlogger.Warn
	switch cfg.Level {
	case GormSilent:
		level = gormlogger.Silent
	case GormError:
		level = gormlogger.Error
	case GormWarn:
		level = gormlogger.Warn
	case GormInfo:
		level = gormlogger.Info
	}

	slowThreshold := cfg.SlowThreshold
	if slowThreshold <= 0 {
		slowThreshold = 200 * time.Millisecond
	}

	return &gormLogger{
		zl:                       zl.With(zap.String("module", "gorm")),
		level:                    level,
		slowThreshold:            slowThreshold,
		ignoreRecordNotFoundError: cfg.IgnoreRecordNotFoundError,
	}
}

// LogMode 实现接口：返回一个使用新级别的副本。
func (l *gormLogger) LogMode(level gormlogger.LogLevel) gormlogger.Interface {
	newLogger := *l
	newLogger.level = level
	return &newLogger
}

// Info 实现接口。
func (l *gormLogger) Info(ctx context.Context, msg string, data ...any) {
	if l.level >= gormlogger.Info {
		l.zl.Info(fmt.Sprintf(msg, data...))
	}
}

// Warn 实现接口。
func (l *gormLogger) Warn(ctx context.Context, msg string, data ...any) {
	if l.level >= gormlogger.Warn {
		l.zl.Warn(fmt.Sprintf(msg, data...))
	}
}

// Error 实现接口。
func (l *gormLogger) Error(ctx context.Context, msg string, data ...any) {
	if l.level >= gormlogger.Error {
		l.zl.Error(fmt.Sprintf(msg, data...))
	}
}

// Trace 实现接口：记录 SQL 执行详情。
func (l *gormLogger) Trace(ctx context.Context, begin time.Time, fc func() (sql string, rowsAffected int64), err error) {
	if l.level <= gormlogger.Silent {
		return
	}

	elapsed := time.Since(begin)

	// 捕获业务调用方信息（跳过 gorm 内部帧）
	caller := captureGormCaller()

	check := func(err error) zap.Field {
		switch {
		case err != nil && l.level >= gormlogger.Error &&
			(!l.ignoreRecordNotFoundError || !errors.Is(err, gormlogger.ErrRecordNotFound)):
			sql, rows := fc()
			l.zl.Error("gorm query error",
				caller,
				zap.Duration("elapsed", elapsed),
				zap.String("sql", sql),
				zap.Int64("rows", rows),
				zap.Error(err),
			)

		case l.slowThreshold > 0 && elapsed > l.slowThreshold && l.level >= gormlogger.Warn:
			sql, rows := fc()
			l.zl.Warn("gorm slow query",
				caller,
				zap.Duration("elapsed", elapsed),
				zap.Duration("threshold", l.slowThreshold),
				zap.String("sql", sql),
				zap.Int64("rows", rows),
			)

		case l.level >= gormlogger.Info:
			sql, rows := fc()
			l.zl.Debug("gorm query",
				caller,
				zap.Duration("elapsed", elapsed),
				zap.String("sql", sql),
				zap.Int64("rows", rows),
			)
		}
		return zap.Skip()
	}
	_ = check(err)
}

// captureGormCaller 遍历调用栈，跳过 gorm.io 和本包内部帧，
// 返回第一个业务调用方的 caller field。
func captureGormCaller() zap.Field {
	pcs := make([]uintptr, 64)
	n := runtime.Callers(4, pcs) // 跳过 Callers / captureGormCaller / Trace / gorm 内部
	if n == 0 {
		return zap.String("caller", "unknown")
	}

	frames := runtime.CallersFrames(pcs[:n])
	for {
		f, more := frames.Next()
		fn := f.Function

		// 跳过 gorm.io 包和本包（log）
		if !strings.Contains(fn, "gorm.io") &&
			!strings.Contains(fn, "/util/log") &&
			fn != "" && f.File != "" {
			return zap.Field{
				Key:    "caller",
				Type:   zapcore.StringType,
				String: fmt.Sprintf("%s:%d", shortFile(f.File), f.Line),
			}
		}
		if !more {
			break
		}
	}
	return zap.String("caller", "unknown")
}

// shortFile 缩短文件路径，只保留最后两级目录。
func shortFile(path string) string {
	parts := strings.Split(path, "/")
	if len(parts) <= 2 {
		return path
	}
	return strings.Join(parts[len(parts)-2:], "/")
}
