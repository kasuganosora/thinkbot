package engagement

import (
	"context"
	"sync"
	"testing"

	"github.com/kasuganosora/thinkbot/agent/core"
)

// ============================================================================
// 判定结果落库（JudgeRecordSink）
//
// 判定质量此前完全不可观测：结果只用于派生「参不参与」的决策，用完即弃。
// 本文件锁死「判定后必定落库」以及落库字段的正确性。
// ============================================================================

// captureJudgeSink 捕获落库记录，用于断言。
type captureJudgeSink struct {
	mu   sync.Mutex
	recs []JudgeRecord
}

func (s *captureJudgeSink) RecordJudge(_ context.Context, rec JudgeRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recs = append(s.recs, rec)
}

func (s *captureJudgeSink) all() []JudgeRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]JudgeRecord, len(s.recs))
	copy(out, s.recs)
	return out
}

func TestSimpleJudge_RecordsToSink(t *testing.T) {
	client := &mockLLMClient{response: "YES this is interesting"}
	sink := &captureJudgeSink{}

	judge := NewSimpleJudge(client, PromptConfig{BotName: "bot"},
		WithJudgeModel("glm-4.6"),
		WithJudgeSink(sink),
	)

	msg := &core.Message{BotID: "bot-1", Channel: "telegram:42"}
	res, err := judge.Judge(context.Background(), msg)
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}

	recs := sink.all()
	if len(recs) != 1 {
		t.Fatalf("expected 1 recorded judgement, got %d", len(recs))
	}
	rec := recs[0]

	if rec.BotID != "bot-1" {
		t.Errorf("BotID: got %q, want %q", rec.BotID, "bot-1")
	}
	if rec.Channel != "telegram:42" {
		t.Errorf("Channel: got %q, want %q", rec.Channel, "telegram:42")
	}
	if rec.Model != "glm-4.6" {
		t.Errorf("Model: got %q, want %q", rec.Model, "glm-4.6")
	}
	if rec.Engage != res.Engage {
		t.Errorf("Engage mismatch: recorded %v, judged %v", rec.Engage, res.Engage)
	}
	if rec.Tier != string(TierLLM) {
		t.Errorf("Tier: got %q, want %q", rec.Tier, TierLLM)
	}
}

// TestSimpleJudge_RecordsRawVerdict 落库的是**原始判定**，不含阈值判断。
//
// 关键：被阈值拦掉的判定同样要记录。阈值定得合不合理，
// 全靠被拦样本的分数分布来判断——只记通过的样本等于没有数据。
func TestSimpleJudge_RecordsRawVerdict(t *testing.T) {
	// 评分很低（20），配合阈值 75 会被拦掉
	client := &mockLLMClient{response: "SCORE: 20\n理由：不太相关"}
	sink := &captureJudgeSink{}

	judge := NewScoredSimpleJudge(client, PromptConfig{BotName: "bot"}, WithJudgeSink(sink))

	policy := NewCompositePolicy(
		AllowAll{},
		WithJudge(judge),
		WithEngagementThreshold(75),
	)

	decision := policy.Evaluate(context.Background(), &core.Message{BotID: "b", Channel: "c"})
	if decision.Engage {
		t.Error("score 20 < threshold 75 should NOT engage")
	}

	// 即便被拦掉，判定也必须已落库
	recs := sink.all()
	if len(recs) != 1 {
		t.Fatalf("rejected judgements must still be recorded, got %d records", len(recs))
	}
	if recs[0].Score != 20 {
		t.Errorf("recorded Score: got %d, want 20 (raw verdict, not thresholded)", recs[0].Score)
	}
}

// TestSimpleJudge_NoSinkDoesNotPanic 未配置 sink 时不落库、不 panic（改动前行为）。
func TestSimpleJudge_NoSinkDoesNotPanic(t *testing.T) {
	client := &mockLLMClient{response: "YES"}
	judge := NewSimpleJudge(client, PromptConfig{BotName: "bot"})

	if _, err := judge.Judge(context.Background(), &core.Message{BotID: "b"}); err != nil {
		t.Fatalf("Judge without sink: %v", err)
	}
}

// TestSimpleJudge_LatencyRecorded 耗时必须被记录——快判在主链路关键路径上。
func TestSimpleJudge_LatencyRecorded(t *testing.T) {
	client := &mockLLMClient{response: "YES"}
	judge := NewSimpleJudge(client, PromptConfig{BotName: "bot"})

	res, err := judge.Judge(context.Background(), &core.Message{BotID: "b"})
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}
	if res.LatencyMS < 0 {
		t.Errorf("LatencyMS should be non-negative, got %d", res.LatencyMS)
	}
	// 模型未显式设置时留空，不应伪造
	if res.Model != "" {
		t.Errorf("Model should be empty when not configured, got %q", res.Model)
	}
}

// TestSimpleJudge_ErrorNoRecord 判定失败（LLM 报错）时不落库——
// 记一条「判定失败」没有意义，且会被误当成 engage=false 的真实判定。
func TestSimpleJudge_ErrorNoRecord(t *testing.T) {
	client := &mockLLMClient{err: context.DeadlineExceeded}
	sink := &captureJudgeSink{}
	judge := NewSimpleJudge(client, PromptConfig{BotName: "bot"}, WithJudgeSink(sink))

	if _, err := judge.Judge(context.Background(), &core.Message{BotID: "b"}); err == nil {
		t.Fatal("expected error from judge")
	}
	if recs := sink.all(); len(recs) != 0 {
		t.Errorf("no record should be written when judgement failed, got %d", len(recs))
	}
}
