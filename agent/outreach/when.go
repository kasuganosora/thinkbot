package outreach

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// ParseWhen 在 loc 时区把 when 解析为绝对 UTC 时刻。
// 支持：ISO 时间戳、相对延迟（2h / 1d / 30m）、tomorrow / 明天 [HH:MM]。
func ParseWhen(raw string, loc *time.Location, now time.Time) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, fmt.Errorf("when is required")
	}
	if loc == nil {
		loc = time.Local
	}
	now = now.In(loc)

	// 相对延迟：纯 duration（不含空格的 2h / 1d / 30m / 1d2h）
	if !strings.ContainsAny(raw, " T:") && durationLooks(raw) {
		d, err := parseDuration(raw)
		if err != nil {
			return time.Time{}, err
		}
		return now.Add(d).UTC(), nil
	}

	if rest, ok := stripTomorrow(raw); ok {
		hh, mm, err := parseClock(rest)
		if err != nil {
			return time.Time{}, err
		}
		t := time.Date(now.Year(), now.Month(), now.Day(), hh, mm, 0, 0, loc).Add(24 * time.Hour)
		return t.UTC(), nil
	}

	// ISO：2006-01-02T15:04[:05][Z|offset] 或 2006-01-02 15:04
	layouts := []string{
		time.RFC3339,
		"2006-01-02T15:04:05",
		"2006-01-02T15:04",
		"2006-01-02 15:04:05",
		"2006-01-02 15:04",
		"2006-01-02",
	}
	for _, layout := range layouts {
		if t, err := time.ParseInLocation(layout, raw, loc); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognized when %q (want ISO timestamp, relative like 2h/1d, or tomorrow 09:00)", raw)
}

func stripTomorrow(raw string) (rest string, ok bool) {
	trimmed := strings.TrimSpace(raw)
	if strings.HasPrefix(trimmed, "明天") {
		return strings.TrimSpace(strings.TrimPrefix(trimmed, "明天")), true
	}
	lower := strings.ToLower(trimmed)
	const prefix = "tomorrow"
	if strings.HasPrefix(lower, prefix) {
		return strings.TrimSpace(trimmed[len(prefix):]), true
	}
	return "", false
}

func parseClock(rest string) (hh, mm int, err error) {
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return 9, 0, nil
	}
	rest = strings.ReplaceAll(rest, "：", ":")
	rest = strings.TrimSuffix(rest, "点")
	if strings.Contains(rest, ":") {
		parts := strings.SplitN(rest, ":", 2)
		hh, err = strconv.Atoi(strings.TrimSpace(parts[0]))
		if err != nil {
			return 0, 0, fmt.Errorf("invalid hour in %q", rest)
		}
		mm, err = strconv.Atoi(strings.TrimSpace(parts[1]))
		if err != nil {
			return 0, 0, fmt.Errorf("invalid minute in %q", rest)
		}
	} else {
		hh, err = strconv.Atoi(rest)
		if err != nil {
			return 0, 0, fmt.Errorf("invalid time of day %q", rest)
		}
	}
	if hh < 0 || hh > 23 || mm < 0 || mm > 59 {
		return 0, 0, fmt.Errorf("time of day out of range: %02d:%02d", hh, mm)
	}
	return hh, mm, nil
}

var durationRe = regexp.MustCompile(`(\d+)([smhd])`)

func durationLooks(s string) bool {
	s = strings.ToLower(strings.ReplaceAll(s, " ", ""))
	if s == "" {
		return false
	}
	for _, r := range s {
		if unicode.IsDigit(r) || r == 's' || r == 'm' || r == 'h' || r == 'd' {
			continue
		}
		return false
	}
	return durationRe.MatchString(s)
}

func parseDuration(s string) (time.Duration, error) {
	s = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(s), " ", ""))
	matches := durationRe.FindAllStringSubmatch(s, -1)
	if matches == nil {
		return 0, fmt.Errorf("invalid duration %q", s)
	}
	consumed := 0
	for _, m := range matches {
		consumed += len(m[0])
	}
	if consumed != len(s) {
		return 0, fmt.Errorf("invalid duration %q", s)
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
