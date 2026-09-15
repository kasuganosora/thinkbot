package bot

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/kasuganosora/thinkbot/agent/memory"
)

// ============================================================================
// DreamRunStore — 梦境运行记录的 JSON 文件持久化
//
// 为什么单独存一份，而不是直接用 cron Job 的 RunCount/LastRunAt：
//  1. 手动触发（POST /dreaming/trigger）直接调用 DreamManager.Run()，绕过调度器，
//     cron Job 的统计永远不会被更新；
//  2. NewDreamingBundle 每次进程启动都会删除并重建 dreaming Job（幂等去重），
//     即使定时跑过，统计也会随重启归零；
//  3. DreamManager.report 只在内存，浏览器刷新能读到、服务重启就丢。
//
// 因此运行状态页统一以本记录为准：手动/定时两条路径都经由 DreamManager.Run()
// 的完成回调写入，包含累计次数、上次运行时间、一行摘要与完整报告。
// ============================================================================

// DreamReportView 是 DreamReport 面向 API 的投影。
// DreamReport 自身的 json tag 是 snake_case，而 trigger 接口返回的是 camelCase，
// 这里统一成 camelCase，前端只需读一种字段风格。
type DreamReportView struct {
	LightIngested   int `json:"lightIngested"`
	LightDeduped    int `json:"lightDeduped"`
	LightDropped    int `json:"lightDropped"`
	REMThemes       int `json:"remThemes"`
	REMCandidates   int `json:"remCandidates"`
	DeepScored      int `json:"deepScored"`
	DeepPassed      int `json:"deepPassed"`
	DeepPromoted    int `json:"deepPromoted"`
	SkippedInactive int `json:"skippedInactive"`
	UserProfiles    int `json:"userProfiles"`
	BotProfiles     int `json:"botProfiles"`
}

// DreamRunRecord 是一次梦境运行的持久化快照，供运行状态页展示。
type DreamRunRecord struct {
	RunAt      time.Time       `json:"runAt"`      // 本次运行开始时间
	FinishedAt time.Time       `json:"finishedAt"` // 本次运行结束时间
	Duration   string          `json:"duration"`   // 耗时（人类可读）
	RunCount   int             `json:"runCount"`   // 累计运行次数
	Phase      string          `json:"phase"`      // 结束所处阶段
	Summary    string          `json:"summary"`    // 一行摘要
	Error      string          `json:"error,omitempty"`
	Report     DreamReportView `json:"report"` // 完整报告（详情卡片用）
}

// DreamRunStore 以 JSON 文件持久化单个 bot 的最近一次运行记录。
// 单 bot 单进程写入，互斥锁 + 临时文件 rename 原子替换即可。
type DreamRunStore struct {
	mu       sync.Mutex
	filePath string
}

// NewDreamRunStore 创建运行记录存储。filePath 为记录文件的完整路径。
func NewDreamRunStore(filePath string) *DreamRunStore {
	return &DreamRunStore{filePath: filePath}
}

// Load 读取最近一次运行记录。文件不存在或内容损坏时返回 nil（不视为错误）。
func (s *DreamRunStore) Load() *DreamRunRecord {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked()
}

// Record 记录一次运行完成：累计次数 +1，覆盖报告快照，返回落盘后的记录。
// 传入 report 为本次运行的报告；nil 时不做任何事。
func (s *DreamRunStore) Record(report *memory.DreamReport) *DreamRunRecord {
	if s == nil || report == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	runCount := 1
	if prev := s.loadLocked(); prev != nil {
		runCount = prev.RunCount + 1
	}

	rec := &DreamRunRecord{
		RunAt:      report.StartedAt,
		FinishedAt: report.FinishedAt,
		Duration:   report.Duration().Round(time.Millisecond).String(),
		RunCount:   runCount,
		Phase:      string(report.Phase),
		Summary:    dreamRunSummary(report),
		Error:      report.Error,
		Report:     dreamReportView(report),
	}
	s.saveLocked(rec)
	return rec
}

func (s *DreamRunStore) loadLocked() *DreamRunRecord {
	data, err := os.ReadFile(s.filePath)
	if err != nil {
		return nil
	}
	var rec DreamRunRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil
	}
	return &rec
}

// saveLocked 原子写入：先写临时文件再 rename，避免读到半截 JSON。
func (s *DreamRunStore) saveLocked(rec *DreamRunRecord) {
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return
	}
	if dir := filepath.Dir(s.filePath); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return
		}
	}
	tmp := s.filePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, s.filePath)
}

func dreamReportView(r *memory.DreamReport) DreamReportView {
	if r == nil {
		return DreamReportView{}
	}
	return DreamReportView{
		LightIngested:   r.LightIngested,
		LightDeduped:    r.LightDeduped,
		LightDropped:    r.LightDropped,
		REMThemes:       r.REMThemes,
		REMCandidates:   r.REMCandidates,
		DeepScored:      r.DeepScored,
		DeepPassed:      r.DeepPassed,
		DeepPromoted:    r.DeepPromoted,
		SkippedInactive: r.SkippedInactive,
		UserProfiles:    r.UserProfiles,
		BotProfiles:     r.BotProfiles,
	}
}

// dreamRunSummary 生成一行摘要，与 DreamExecutor 写进 cron LastResult 的格式保持一致。
func dreamRunSummary(r *memory.DreamReport) string {
	if r == nil {
		return ""
	}
	if r.Error != "" {
		return "failed: " + r.Error
	}
	return fmt.Sprintf("ingested=%d deduped=%d dropped=%d themes=%d scored=%d promoted=%d profiles=%d/%d",
		r.LightIngested, r.LightDeduped, r.LightDropped, r.REMThemes,
		r.DeepScored, r.DeepPromoted, r.UserProfiles, r.BotProfiles)
}
