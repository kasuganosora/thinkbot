package stats

import (
	"context"

	"github.com/kasuganosora/thinkbot/agent/engagement"
	"github.com/kasuganosora/thinkbot/dao"
)

// ============================================================================
// JudgeSink — engagement 判定结果的落库实现
//
// JudgeRecordSink 接口由 engagement 包定义（消费方定义接口，Go 惯例），
// 本文件提供实现。方向是 stats → engagement 的单向依赖：
// engagement 只依赖 agent/core、agent/outbound、config 与 util 包，
// 均不依赖 stats，故不形成循环。
// ============================================================================

// 编译期断言：确保 JudgeSink 满足 engagement.JudgeRecordSink。
var _ engagement.JudgeRecordSink = (*JudgeSink)(nil)

// JudgeSink 把 LLM 快判结果写入 judge_records 表。
type JudgeSink struct {
	rec *JudgeRecorder
}

// NewJudgeSink 创建判定结果落库适配器。
func NewJudgeSink(rec *JudgeRecorder) *JudgeSink {
	return &JudgeSink{rec: rec}
}

// RecordJudge 实现 engagement.JudgeRecordSink。
//
// 非阻塞：底层走 JudgeRecorder 的 channel，满则丢弃。
// 落库是旁路观测，绝不能阻塞或影响「是否参与」这个主决策。
func (s *JudgeSink) RecordJudge(ctx context.Context, rec engagement.JudgeRecord) {
	if s == nil || s.rec == nil {
		return
	}
	s.rec.Record(ctx, dao.JudgeRecord{
		BotID:     rec.BotID,
		Channel:   rec.Channel,
		Feature:   "engagement",
		Model:     rec.Model,
		Engage:    rec.Engage,
		Score:     rec.Score,
		Reason:    truncateReason(rec.Reason),
		Tier:      rec.Tier,
		LatencyMS: rec.LatencyMS,
	})
}

// judgeReasonMaxLen 落库时理由字段的截断长度。
//
// 理由由 LLM 生成，长度不可控。列宽 512，超长会被 DB 截断或报错；
// 与其让它随机失败，不如在写入前确定性地截断。
const judgeReasonMaxLen = 480

// truncateReason 截断理由文本，保留开头（结论性内容通常在前面）。
func truncateReason(s string) string {
	if len(s) <= judgeReasonMaxLen {
		return s
	}
	return s[:judgeReasonMaxLen] + "…"
}
