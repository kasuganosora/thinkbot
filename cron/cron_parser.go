package cron

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ============================================================================
// Cron 表达式解析器（标准 5 段格式）
//
// 支持：minute hour day-of-month month day-of-week
// 语法：* / - ,
// 不支持：秒、年、L/W/# 等扩展语法（保持简洁）
// ============================================================================

// cronField 表示 cron 表达式的一个字段。
type cronField struct {
	values map[int]bool // 匹配的值集合
}

// cronExpr 是解析后的 5 段 cron 表达式。
type cronExpr struct {
	minute     cronField // 0-59
	hour       cronField // 0-23
	dayOfMonth cronField // 1-31
	month      cronField // 1-12
	dayOfWeek  cronField // 0-6 (0=Sunday)
}

// 各字段的范围
var cronRanges = []struct {
	min, max int
}{
	{0, 59}, // minute
	{0, 23}, // hour
	{1, 31}, // day-of-month
	{1, 12}, // month
	{0, 6},  // day-of-week
}

// parseCronExpr 解析标准 5 段 cron 表达式。
// 例如："0 9 * * 1-5" 表示工作日每天 9:00。
func parseCronExpr(expr string) (*cronExpr, error) {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return nil, fmt.Errorf("cron: expected 5 fields, got %d", len(fields))
	}

	ce := &cronExpr{}
	for i, raw := range fields {
		f, err := parseCronField(raw, cronRanges[i].min, cronRanges[i].max)
		if err != nil {
			return nil, fmt.Errorf("cron: field %d (%q): %w", i, raw, err)
		}
		switch i {
		case 0:
			ce.minute = f
		case 1:
			ce.hour = f
		case 2:
			ce.dayOfMonth = f
		case 3:
			ce.month = f
		case 4:
			ce.dayOfWeek = f
		}
	}
	return ce, nil
}

// parseCronField 解析单个 cron 字段。
// 支持：* / n / a-b / a,b / a-b/n 等组合。
func parseCronField(s string, min, max int) (cronField, error) {
	f := cronField{values: make(map[int]bool)}

	// 处理逗号分隔的列表
	for _, part := range strings.Split(s, ",") {
		if err := parseCronFieldPart(part, min, max, f.values); err != nil {
			return f, err
		}
	}
	return f, nil
}

// parseCronFieldPart 解析一个 cron 字段片段。
func parseCronFieldPart(s string, min, max int, values map[int]bool) error {
	// 处理 step（/n）
	step := 1
	rangePart := s

	if idx := strings.Index(s, "/"); idx >= 0 {
		rangePart = s[:idx]
		stepStr := s[idx+1:]
		var err error
		step, err = strconv.Atoi(stepStr)
		if err != nil || step < 1 {
			return fmt.Errorf("invalid step %q", stepStr)
		}
	}

	// 处理范围
	var start, end int
	if rangePart == "*" {
		start, end = min, max
	} else if idx := strings.Index(rangePart, "-"); idx >= 0 {
		var err error
		start, err = strconv.Atoi(rangePart[:idx])
		if err != nil {
			return fmt.Errorf("invalid range start %q", rangePart[:idx])
		}
		end, err = strconv.Atoi(rangePart[idx+1:])
		if err != nil {
			return fmt.Errorf("invalid range end %q", rangePart[idx+1:])
		}
	} else {
		var err error
		start, err = strconv.Atoi(rangePart)
		if err != nil {
			return fmt.Errorf("invalid value %q", rangePart)
		}
		end = start
	}

	if start < min || end > max || start > end {
		return fmt.Errorf("value out of range [%d-%d]: %d-%d", min, max, start, end)
	}

	for v := start; v <= end; v += step {
		values[v] = true
	}
	return nil
}

// Next 计算从 from 之后的下一次匹配时间。
// loc 指定时区用于计算。
func (ce *cronExpr) Next(from time.Time, loc *time.Location) time.Time {
	// 转换到目标时区
	t := from.In(loc)
	// 从下一分钟开始（秒归零）
	t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), 0, 0, loc)

	// 最多搜索 5 年（防止无效表达式导致死循环）
	limit := t.AddDate(5, 0, 0)

	for {
		// 前进一分钟
		t = t.Add(time.Minute)

		if t.After(limit) {
			return time.Time{} // 没有匹配
		}

		if ce.minute.values[t.Minute()] &&
			ce.hour.values[t.Hour()] &&
			ce.dayOfMonth.values[t.Day()] &&
			ce.month.values[int(t.Month())] &&
			ce.dayOfWeek.values[int(t.Weekday())] {
			return t
		}
	}
}
