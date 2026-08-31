package log

import (
	"fmt"
	"os"
	"path/filepath"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"gopkg.in/natefinch/lumberjack.v2"
)

// Logger 全局 SugaredLogger 实例。
var Logger *zap.SugaredLogger

// ============================================================================
// 输出源
// ============================================================================

// OutputType 输出源类型。
type OutputType string

const (
	OutputStdout OutputType = "stdout" // 标准输出
	OutputStderr OutputType = "stderr" // 标准错误
	OutputFile   OutputType = "file"   // 文件（支持滚动）
)

// Format 日志格式。
type Format string

const (
	FormatConsole Format = "console" // 人类可读（默认用于 stdout/stderr）
	FormatJSON    Format = "json"    // JSONL（默认用于文件）
	FormatAuto    Format = "auto"    // 自动：stdout/stderr→console，file→json
)

// Output 单个输出源配置。
type Output struct {
	// Type 输出类型：stdout / stderr / file。
	Type OutputType
	// Level 该输出源的日志级别过滤，为空时继承全局 Level。
	Level string
	// Format 输出格式：auto / console / json，默认 auto。
	Format Format

	// --- 文件专用字段（Type == file 时生效） ---

	// FileDir 日志文件目录。
	FileDir string
	// FileName 文件名（不含扩展名），默认 thinkbot。
	FileName string
	// FileExt 文件扩展名，默认 .log。
	FileExt string
	// MaxSize 单文件最大体积（MB），默认 100。
	MaxSize int
	// MaxBackups 保留旧文件数量，默认 7。
	MaxBackups int
	// MaxAge 旧文件保留天数，默认 30。
	MaxAge int
	// Compress 是否 gzip 压缩旧文件。
	Compress bool
}

// Stdout 快捷构造 stdout 输出源。
func Stdout() Output {
	return Output{Type: OutputStdout, Format: FormatConsole}
}

// Stderr 快捷构造 stderr 输出源。
func Stderr() Output {
	return Output{Type: OutputStderr, Format: FormatConsole}
}

// File 快捷构造文件输出源。
func File(dir, name string) Output {
	return Output{
		Type:       OutputFile,
		Format:     FormatJSON,
		FileDir:    dir,
		FileName:   name,
		FileExt:    ".log",
		MaxSize:    100,
		MaxBackups: 7,
		MaxAge:     30,
		Compress:   true,
	}
}

// ConsoleFile 快捷构造「console 格式 + 文件滚动」输出源。
// 产出可读且自动轮换的纯文本日志（如 ./logs/thinkbot.console.log），
// 适合实时 tail；与 File（JSONL）写入不同文件、各自独立轮换，互不冲突。
// 注意：写文件时不带 ANSI 颜色（见 buildCore），以保证纯文本可读性。
func ConsoleFile(dir, name string) Output {
	return Output{
		Type:       OutputFile,
		Format:     FormatConsole,
		FileDir:    dir,
		FileName:   name,
		FileExt:    ".log",
		MaxSize:    100,
		MaxBackups: 7,
		MaxAge:     30,
		Compress:   true,
	}
}

// ============================================================================
// 全局配置
// ============================================================================

// Config 日志配置。
type Config struct {
	// Level 全局日志级别：debug / info / warn / error，默认 info。
	Level string

	// Outputs 输出源列表。为空时自动使用 legacy 字段构建（向后兼容）。
	Outputs []Output

	// --- Legacy 字段（Outputs 为空时使用，向后兼容） ---

	FileDir    string
	FileName   string
	FileExt    string
	MaxSize    int
	MaxBackups int
	MaxAge     int
	Compress   bool
	// AlsoStdout 输出到文件时是否同时输出到 stdout，默认 true。
	AlsoStdout bool
}

// DefaultConfig 返回一组合理的默认配置（仅 stdout）。
func DefaultConfig() Config {
	return Config{
		Level: "info",
	}
}

// Init 使用默认配置初始化全局 Logger（向后兼容）。
func Init() error {
	return InitWithConfig(DefaultConfig())
}

// InitWithConfig 使用自定义配置初始化全局 Logger。
//
// 每个输出源可以有自己的级别过滤和格式：
//   - stdout/stderr：默认 console 格式（彩色级别）
//   - file：默认 JSONL 格式（便于程序解析）
func InitWithConfig(cfg Config) error {
	// --- 确定全局日志级别 ---
	level := zapcore.InfoLevel
	if cfg.Level != "" {
		if l, err := zapcore.ParseLevel(cfg.Level); err == nil {
			level = l
		}
	}

	// --- 确定 Outputs（向后兼容） ---
	outputs := cfg.Outputs
	if len(outputs) == 0 {
		outputs = buildLegacyOutputs(cfg)
	}

	// --- 共用 EncoderConfig ---
	baseEncCfg := zapcore.EncoderConfig{
		TimeKey:        "ts",
		LevelKey:       "level",
		NameKey:        "logger",
		CallerKey:      "caller",
		MessageKey:     "msg",
		StacktraceKey:  "stacktrace",
		LineEnding:     zapcore.DefaultLineEnding,
		EncodeLevel:    zapcore.CapitalLevelEncoder,
		EncodeTime:     zapcore.ISO8601TimeEncoder,
		EncodeDuration: zapcore.SecondsDurationEncoder,
		EncodeCaller:   zapcore.ShortCallerEncoder,
	}

	// --- 为每个输出源构建 Core ---
	var cores []zapcore.Core

	// 文件路径 → lumberjack 实例 的复用表，避免多个输出源指向同一文件时
	// 各自创建独立的 rotator 互相破坏轮转。
	fileCache := map[string]zapcore.WriteSyncer{}

	for _, out := range outputs {
		core, err := buildCore(out, baseEncCfg, level, fileCache)
		if err != nil {
			return err
		}
		if core != nil {
			cores = append(cores, core)
		}
	}

	if len(cores) == 0 {
		// 兜底：至少输出到 stdout
		cores = append(cores, makeConsoleCore(baseEncCfg, os.Stdout, level))
	}

	// --- 组装 ---
	core := zapcore.NewTee(cores...)
	zl := zap.New(core, zap.AddCaller())
	Logger = zl.Sugar()
	zap.ReplaceGlobals(zl)

	return nil
}

// ============================================================================
// 内部：构建 Core
// ============================================================================

// buildCore 为单个输出源构建 zapcore.Core。
// fileCache 复用同一文件路径的 lumberjack 实例，避免重复轮转冲突。
func buildCore(out Output, baseEncCfg zapcore.EncoderConfig, globalLevel zapcore.Level, fileCache map[string]zapcore.WriteSyncer) (zapcore.Core, error) {
	// --- 确定该输出源的级别 ---
	outLevel := globalLevel
	if out.Level != "" {
		if l, err := zapcore.ParseLevel(out.Level); err == nil {
			outLevel = l
		}
	}

	// --- 确定格式 ---
	outFmt := out.Format
	if outFmt == "" || outFmt == FormatAuto {
		switch out.Type {
		case OutputStdout, OutputStderr:
			outFmt = FormatConsole
		default:
			outFmt = FormatJSON
		}
	}

	// --- 构建 encoder ---
	var encoder zapcore.Encoder
	switch outFmt {
	case FormatJSON:
		encoder = zapcore.NewJSONEncoder(baseEncCfg)
	default: // console
		consoleEncCfg := baseEncCfg
		if out.Type == OutputFile {
			// 写文件时不带 ANSI 颜色，保证纯文本可读（供 tail / 日志分析）
			consoleEncCfg.EncodeLevel = zapcore.CapitalLevelEncoder
		} else {
			consoleEncCfg.EncodeLevel = zapcore.CapitalColorLevelEncoder
		}
		encoder = zapcore.NewConsoleEncoder(consoleEncCfg)
	}

	// --- 构建 writer + core ---
	switch out.Type {
	case OutputStdout:
		// 尊重 out.Format（如显式 JSON），而非强制 console。
		return zapcore.NewCore(encoder, zapcore.Lock(os.Stdout), outLevel), nil

	case OutputStderr:
		return zapcore.NewCore(encoder, zapcore.Lock(os.Stderr), outLevel), nil

	case OutputFile:
		return buildFileCore(out, encoder, outLevel, fileCache)

	default:
		return nil, fmt.Errorf("unknown output type: %s", out.Type)
	}
}

// makeConsoleCore 创建 console 格式的 Core。
func makeConsoleCore(baseEncCfg zapcore.EncoderConfig, w zapcore.WriteSyncer, level zapcore.Level) zapcore.Core {
	encCfg := baseEncCfg
	encCfg.EncodeLevel = zapcore.CapitalColorLevelEncoder
	return zapcore.NewCore(
		zapcore.NewConsoleEncoder(encCfg),
		zapcore.Lock(w),
		level,
	)
}

// buildFileCore 创建文件输出 Core（JSONL，带滚动）。
// fileCache 按完整文件路径复用 lumberjack 实例，避免多个输出源指向同一文件时
// 各自独立轮转、互相破坏。
func buildFileCore(out Output, encoder zapcore.Encoder, level zapcore.Level, fileCache map[string]zapcore.WriteSyncer) (zapcore.Core, error) {
	dir := out.FileDir
	if dir == "" {
		dir = "."
	}

	logPath, err := ensureLogDir(dir)
	if err != nil {
		return nil, fmt.Errorf("create log dir %q: %w", dir, err)
	}

	name := out.FileName
	if name == "" {
		name = "thinkbot"
	}
	ext := out.FileExt
	if ext == "" {
		ext = ".log"
	}
	path := filepath.Join(logPath, name+ext)

	if ws, ok := fileCache[path]; ok {
		// 同一文件复用已有 rotator
		return zapcore.NewCore(encoder, ws, level), nil
	}

	lj := &lumberjack.Logger{
		Filename:   path,
		MaxSize:    orDefault(out.MaxSize, 100),
		MaxBackups: orDefault(out.MaxBackups, 7),
		MaxAge:     orDefault(out.MaxAge, 30),
		Compress:   out.Compress,
		LocalTime:  true,
	}
	ws := zapcore.AddSync(lj)
	fileCache[path] = ws

	return zapcore.NewCore(encoder, ws, level), nil
}

// ============================================================================
// 内部：向后兼容
// ============================================================================

// buildLegacyOutputs 从旧版 Config 字段推导 Outputs 列表。
func buildLegacyOutputs(cfg Config) []Output {
	if cfg.FileDir == "" {
		return []Output{Stdout()}
	}

	var outputs []Output

	// 文件（JSONL）
	outputs = append(outputs, File(cfg.FileDir, strOrDefault(cfg.FileName, "thinkbot")))

	// stdout（console）
	if cfg.AlsoStdout {
		outputs = append(outputs, Stdout())
	}

	return outputs
}

// ============================================================================
// 内部工具
// ============================================================================

// ensureLogDir 确保日志目录存在。
func ensureLogDir(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return "", err
	}
	return abs, nil
}

// orDefault 当 v <= 0 时返回 def。
func orDefault(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}

// strOrDefault 当 s 为空时返回 def。
func strOrDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
