package cron

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ============================================================================
// Job — 定时任务定义
//
// 支持 4 种调度格式（参照同类 Agent 框架设计）：
//   1. Cron 表达式：   "0 9 * * 1-5"     → 标准 5 段 cron（工作日 9:00）
//   2. 间隔循环：      "every 30m"        → 每 30 分钟
//   3. 相对延迟：      "2h" / "30m" / "1d" → 延迟后执行一次
//   4. ISO 时间戳：    "2026-03-20T14:00" → 在指定时刻执行一次
// ============================================================================

// ScheduleKind 表示调度的类型。
type ScheduleKind string

const (
	ScheduleCron     ScheduleKind = "cron"     // cron 表达式
	ScheduleInterval ScheduleKind = "interval" // 固定间隔循环
	ScheduleOnce     ScheduleKind = "once"     // 一次性（相对延迟或 ISO 时间戳）
)

// JobState 表示任务的运行时状态。
type JobState string

const (
	StateActive   JobState = "active"   // 已启用，等待触发
	StatePaused   JobState = "paused"   // 已暂停
	StateDone     JobState = "done"     // 一次性任务已完成
	StateFailed   JobState = "failed"   // 执行失败且不重试
	StateDisabled JobState = "disabled" // 被禁用（enabled=false）
)

// Job 是一个定时任务定义。
type Job struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Prompt  string   `json:"prompt"`            // 自包含提示词（触发后发送给 bot）
	Model   string   `json:"model,omitempty"`   // 可选：覆盖 bot 默认模型
	Channel string   `json:"channel,omitempty"` // 可选：指定输出渠道（空=bot 默认）
	Skills  []string `json:"skills,omitempty"`  // 可选：执行时激活的技能

	// Feature 统计维度标签（默认 "cron"）。
	// 用于在 stats 模块中区分 cron 产生的 token 消耗。
	Feature string `json:"feature,omitempty"`

	// 调度
	Schedule        string       `json:"schedule"`         // 原始调度字符串
	ScheduleKind    ScheduleKind `json:"schedule_kind"`    // 解析后的类型
	ScheduleDisplay string       `json:"schedule_display"` // 可读显示

	// 重复控制
	MaxRuns  int `json:"max_runs,omitempty"` // 0=无限
	RunCount int `json:"run_count"`          // 已执行次数

	// 运行时状态
	Enabled    bool       `json:"enabled"`
	State      JobState   `json:"state"`
	NextRunAt  *time.Time `json:"next_run_at,omitempty"` // 下次执行时间（UTC）
	LastRunAt  *time.Time `json:"last_run_at,omitempty"` // 上次执行时间（UTC）
	LastResult string     `json:"last_result,omitempty"` // 上次执行结果摘要
	LastError  string     `json:"last_error,omitempty"`  // 上次执行错误

	// 元数据
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Tags      []string  `json:"tags,omitempty"`

	// 解析后的 cron 表达式（内部使用，不序列化）
	cronExpr *cronExpr `json:"-"`
}

// parseSchedule 解析调度字符串，确定类型并预计算 NextRunAt。
// loc 用于 cron/once 类型的时区计算。
func parseSchedule(raw string, loc *time.Location) (kind ScheduleKind, display string, cronE *cronExpr, nextRun *time.Time, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", nil, nil, fmt.Errorf("empty schedule")
	}

	display = raw

	// 1. 间隔循环："every 30m"
	if strings.HasPrefix(strings.ToLower(raw), "every ") {
		dur, dErr := parseDuration(raw[6:])
		if dErr != nil {
			err = fmt.Errorf("invalid interval: %w", dErr)
			return
		}
		kind = ScheduleInterval
		now := time.Now().In(loc)
		nr := now.Add(dur)
		nextRun = &nr
		return
	}

	// 2. Cron 表达式：5 段空格分隔，每段仅含 [\d*\-,/]
	parts := strings.Fields(raw)
	if len(parts) == 5 && allCronFields(parts) {
		cronE, err = parseCronExpr(raw)
		if err != nil {
			return
		}
		kind = ScheduleCron
		nr := cronE.Next(time.Now().In(loc), loc)
		if nr.IsZero() {
			err = fmt.Errorf("cron expression never matches")
			return
		}
		nextRun = &nr
		return
	}

	// 3. ISO 时间戳："2026-03-20T14:00"
	if isISOTimestamp(raw) {
		t, pErr := parseISOTimestamp(raw, loc)
		if pErr != nil {
			err = fmt.Errorf("invalid ISO timestamp: %w", pErr)
			return
		}
		kind = ScheduleOnce
		nextRun = &t
		return
	}

	// 4. 相对延迟："30m" / "2h" / "1d"
	if dur, dErr := parseDuration(raw); dErr == nil {
		kind = ScheduleOnce
		now := time.Now().In(loc)
		nr := now.Add(dur)
		nextRun = &nr
		return
	}

	err = fmt.Errorf("unrecognized schedule format: %q", raw)
	return
}

// allCronFields 检查所有字段是否仅包含 cron 允许的字符。
func allCronFields(fields []string) bool {
	re := regexp.MustCompile(`^[\d*\-,/]+$`)
	for _, f := range fields {
		if !re.MatchString(f) {
			return false
		}
	}
	return true
}

// isISOTimestamp 检查是否为 ISO 时间戳格式。
func isISOTimestamp(s string) bool {
	return regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}`).MatchString(s)
}

// parseISOTimestamp 解析 ISO 时间戳。
// 支持带或不带时区偏移的格式：
//
//	"2026-03-20T14:00"
//	"2026-03-20T14:00:00+08:00"
//	"2026-03-20T14:00Z"
func parseISOTimestamp(s string, loc *time.Location) (time.Time, error) {
	// 尝试多种格式
	formats := []string{
		"2006-01-02T15:04:05Z07:00",
		"2006-01-02T15:04:05",
		"2006-01-02T15:04Z07:00",
		"2006-01-02T15:04",
	}
	for _, f := range formats {
		if t, err := time.ParseInLocation(f, s, loc); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("cannot parse %q as ISO timestamp", s)
}

// parseDuration 解析持续时间字符串。
// 支持格式：30s / 5m / 2h / 1d / 30m / 1d2h / 3h30m
var durationRe = regexp.MustCompile(`(\d+)([smhd])`)

func parseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	matches := durationRe.FindAllStringSubmatch(s, -1)
	if matches == nil {
		return 0, fmt.Errorf("invalid duration %q", s)
	}

	// 确保整个字符串被消费
	consumed := 0
	for _, m := range matches {
		consumed += len(m[0])
	}
	// 允许空格
	cleaned := strings.ReplaceAll(s, " ", "")
	if consumed != len(cleaned) {
		return 0, fmt.Errorf("invalid duration %q (unparsed characters)", s)
	}

	var total time.Duration
	for _, m := range matches {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			return 0, err
		}
		switch m[2] {
		case "s":
			total += time.Duration(n) * time.Second
		case "m":
			total += time.Duration(n) * time.Minute
		case "h":
			total += time.Duration(n) * time.Hour
		case "d":
			total += time.Duration(n) * 24 * time.Hour
		}
	}
	if total <= 0 {
		return 0, fmt.Errorf("duration must be positive")
	}
	return total, nil
}
