package cron

import (
	"testing"
	"time"

	"go.uber.org/zap"
)

// TestScheduler_CheckMissedRuns 验证漏跑检测：活跃循环任务若「上次成功运行过旧」
// （错过 ≥2 个周期）被标记告警；一次性 / 未运行过 / 停用 / 近期运行过的任务不告警。
func TestScheduler_CheckMissedRuns(t *testing.T) {
	now := time.Now().UTC()
	s := &Scheduler{
		config:         SchedulerConfig{},
		logger:         zap.NewNop().Sugar(),
		missedWarnedAt: make(map[string]time.Time),
	}

	old := now.Add(-3 * 24 * time.Hour)  // 3 天前（daily 任务，应告警）
	recent := now.Add(-1 * time.Hour)    // 1 小时前（不应告警）
	ptr := func(t time.Time) *time.Time { return &t }

	jobs := []*Job{
		{ID: "daily-missed", Name: "dreaming", ScheduleKind: ScheduleCron, Schedule: "0 3 * * *",
			ExpectedInterval: 24 * time.Hour, Enabled: true, State: StateActive, LastRunAt: ptr(old)},
		{ID: "daily-ok", Name: "heartbeat", ScheduleKind: ScheduleCron, Schedule: "0 * * * *",
			ExpectedInterval: time.Hour, Enabled: true, State: StateActive, LastRunAt: ptr(recent)},
		{ID: "never-run", Name: "new", ScheduleKind: ScheduleCron, Schedule: "0 3 * * *",
			ExpectedInterval: 24 * time.Hour, Enabled: true, State: StateActive, LastRunAt: nil},
		{ID: "once-old", Name: "one-shot", ScheduleKind: ScheduleOnce,
			ExpectedInterval: 0, Enabled: true, State: StateActive, LastRunAt: ptr(old)},
		{ID: "disabled-old", Name: "off", ScheduleKind: ScheduleCron, Schedule: "0 3 * * *",
			ExpectedInterval: 24 * time.Hour, Enabled: false, State: StateActive, LastRunAt: ptr(old)},
	}

	s.checkMissedRuns(now, jobs)

	if _, ok := s.missedWarnedAt["daily-missed"]; !ok {
		t.Fatal("expected daily-missed (3d stale) to be flagged as missed")
	}
	for _, id := range []string{"daily-ok", "never-run", "once-old", "disabled-old"} {
		if _, ok := s.missedWarnedAt[id]; ok {
			t.Errorf("job %q should NOT be flagged as missed", id)
		}
	}

	// 冷却：同一 now 再次调用不应改变判定（仍只在集合里，不重复触发逻辑分支）
	s.checkMissedRuns(now, jobs)
	if _, ok := s.missedWarnedAt["daily-missed"]; !ok {
		t.Fatal("cooldown re-check should keep daily-missed flagged")
	}
}

// TestParseSchedule_ExpectedInterval 验证调度解析能正确推导标称间隔，供漏跑检测使用。
func TestParseSchedule_ExpectedInterval(t *testing.T) {
	cases := []struct {
		raw   string
		want  time.Duration
		isCron bool
	}{
		{"every 30m", 30 * time.Minute, false},
		{"every 6h", 6 * time.Hour, false},
		{"0 3 * * *", 24 * time.Hour, true}, // 每天一次 → 间隔 24h
		{"0 * * * *", time.Hour, true},       // 每小时 → 间隔 1h
	}
	for _, c := range cases {
		kind, _, _, _, exp, err := parseSchedule(c.raw, time.Local)
		if err != nil {
			t.Fatalf("parseSchedule(%q) error: %v", c.raw, err)
		}
		if c.isCron && kind != ScheduleCron {
			t.Errorf("parseSchedule(%q): kind=%v, want cron", c.raw, kind)
		}
		if exp != c.want {
			t.Errorf("parseSchedule(%q): ExpectedInterval=%v, want %v", c.raw, exp, c.want)
		}
	}
}
