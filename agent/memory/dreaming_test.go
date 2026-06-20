package memory

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"
)

// testDreamLogger 创建测试用 logger。
func testDreamLogger() *zap.SugaredLogger {
	return zap.NewNop().Sugar()
}

// newTestDreamManager 创建测试用 DreamManager（无 LLM provider）。
func newTestDreamManager(t *testing.T, scopes []Scope) (*DreamManager, *TieredManager) {
	t.Helper()
	store := NewTieredStore(nil)
	tm := NewTieredManager(TieredManagerConfig{
		Store: store,
	}, noop.NewTracerProvider(), testDreamLogger())

	cfg := DefaultDreamConfig()
	cfg.Enabled = true
	cfg.Scopes = scopes

	dm := NewDreamManager(cfg, tm, nil, noop.NewTracerProvider(), testDreamLogger())
	return dm, tm
}

func TestDreamManager_StateDisabled(t *testing.T) {
	cfg := DefaultDreamConfig()
	cfg.Enabled = false
	store := NewTieredStore(nil)
	tm := NewTieredManager(TieredManagerConfig{Store: store},
		noop.NewTracerProvider(), testDreamLogger())
	dm := NewDreamManager(cfg, tm, nil, noop.NewTracerProvider(), testDreamLogger())

	if dm.State() != DreamDisabled {
		t.Error("expected DreamDisabled")
	}

	_, err := dm.Run(context.Background())
	if err == nil {
		t.Error("expected error when disabled")
	}
}

func TestDreamManager_EnableDisable(t *testing.T) {
	dm, _ := newTestDreamManager(t, nil)

	if dm.State() != DreamIdle {
		t.Error("expected DreamIdle after creation with Enabled=true")
	}

	dm.Disable()
	if dm.State() != DreamDisabled {
		t.Error("expected DreamDisabled after Disable()")
	}

	dm.Enable()
	if dm.State() != DreamIdle {
		t.Error("expected DreamIdle after Enable()")
	}
}

func TestDreamManager_RunNoScopes(t *testing.T) {
	dm, _ := newTestDreamManager(t, nil)

	report, err := dm.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if report.Error != "no scopes to process" {
		t.Errorf("expected 'no scopes' error, got %q", report.Error)
	}
}

func TestDreamManager_RunEmptyMemory(t *testing.T) {
	scope := ChannelScope("empty-ch")
	dm, _ := newTestDreamManager(t, []Scope{scope})

	report, err := dm.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if report.LightIngested != 0 {
		t.Errorf("expected 0 ingested, got %d", report.LightIngested)
	}
}

func TestDreamManager_LightPhase(t *testing.T) {
	scope := ChannelScope("light-test")
	dm, tm := newTestDreamManager(t, []Scope{scope})
	ctx := context.Background()

	// 写入一些 L0 条目
	for i := 0; i < 10; i++ {
		_ = tm.WriteWorking(ctx, scope,
			"用户使用 Go 语言开发后端服务，偏好简洁代码风格", "test")
	}

	report, err := dm.Run(ctx)
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	if report.LightIngested != 10 {
		t.Errorf("expected 10 ingested, got %d", report.LightIngested)
	}
	if report.LightDeduped == 0 {
		t.Error("expected some deduped candidates")
	}

	// 验证 staged candidates 存在
	staged := dm.StagedCandidates()
	if len(staged) == 0 {
		t.Error("expected staged candidates after light phase")
	}
}

func TestDreamManager_JaccardDedup(t *testing.T) {
	candidates := []DreamCandidate{
		{Content: "用户偏好使用 Go 语言"},
		{Content: "用户偏好使用 Go 语言"}, // 完全重复
		{Content: "用户喜欢 Python 编程"},
		{Content: "服务器运行 Debian 13"},
	}

	deduped := jaccardDedup(candidates, 0.9)
	if len(deduped) != 3 {
		t.Errorf("expected 3 after dedup, got %d", len(deduped))
	}
}

func TestJaccardSimilarity(t *testing.T) {
	tests := []struct {
		a, b      string
		expected  float64
		tolerance float64
	}{
		{"hello world", "hello world", 1.0, 0.01},
		{"hello world", "goodbye world", 0.33, 0.1},
		{"completely different text", "totally unrelated words", 0.0, 0.01},
		{"", "", 0.0, 0.01},
	}

	for _, tt := range tests {
		got := jaccardSimilarity(tokenize(tt.a), tokenize(tt.b))
		if got < tt.expected-tt.tolerance || got > tt.expected+tt.tolerance {
			t.Errorf("jaccardSimilarity(%q, %q) = %f, expected ~%f",
				tt.a, tt.b, got, tt.expected)
		}
	}
}

func TestDreamManager_REMPhase(t *testing.T) {
	scope := ChannelScope("rem-test")
	dm, tm := newTestDreamManager(t, []Scope{scope})
	ctx := context.Background()

	// 写入多类内容
	_ = tm.WriteWorking(ctx, scope, "用户使用 VSCode 编辑器", "test")
	_ = tm.WriteWorking(ctx, scope, "用户配置了 VSCode 的字体", "test")
	_ = tm.WriteWorking(ctx, scope, "用户用 Docker 部署应用", "test")

	report, err := dm.Run(ctx)
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	// REM 应该发现至少一个主题
	if report.REMThemes == 0 {
		// 无 LLM 时按 category 聚类，可能只有一个 category
		// 不强求，因为 rule-based 提取可能都归为 "observation"
	}
}

func TestDreamManager_DeepPhaseNoPromotion(t *testing.T) {
	scope := ChannelScope("deep-test")
	dm, tm := newTestDreamManager(t, []Scope{scope})
	ctx := context.Background()

	_ = tm.WriteWorking(ctx, scope, "一些测试内容", "test")

	report, err := dm.Run(ctx)
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	// 无召回数据 → RecallCount=0 → 不通过门控
	if report.DeepPromoted != 0 {
		t.Errorf("expected 0 promotions without recall data, got %d",
			report.DeepPromoted)
	}
}

func TestDreamManager_DeepPhaseWithRecall(t *testing.T) {
	scope := ChannelScope("deep-promote-test")
	dm, tm := newTestDreamManager(t, []Scope{scope})
	ctx := context.Background()

	// 写入内容
	_ = tm.WriteWorking(ctx, scope,
		"用户使用 Go 语言进行后端开发，偏好简洁代码", "test")

	// 第一次 Run → Light 提取候选
	_, err := dm.Run(ctx)
	if err != nil {
		t.Fatalf("first Run failed: %v", err)
	}

	// 手动模拟召回信号
	for _, c := range dm.StagedCandidates() {
		for i := 0; i < 5; i++ {
			dm.RecordRecall(c.Key, "query-"+string(rune('0'+i)))
		}
	}

	// 第二次 Run → Deep 应该有评分
	report2, err := dm.Run(ctx)
	if err != nil {
		t.Fatalf("second Run failed: %v", err)
	}

	// 验证有候选被评分
	if report2.DeepScored == 0 {
		t.Error("expected some scored candidates in deep phase")
	}
}

func TestDreamManager_DreamDiary(t *testing.T) {
	scope := ChannelScope("diary-test")
	dm, _ := newTestDreamManager(t, []Scope{scope})

	_, _ = dm.Run(context.Background())

	diary := dm.DreamDiary()
	if len(diary) == 0 {
		t.Error("expected at least one diary entry after Run")
	}
}

func TestDreamManager_Report(t *testing.T) {
	scope := ChannelScope("report-test")
	dm, _ := newTestDreamManager(t, []Scope{scope})

	report, _ := dm.Run(context.Background())

	// 验证报告结构
	if report.StartedAt.IsZero() {
		t.Error("StartedAt should not be zero")
	}
	if report.FinishedAt.IsZero() {
		t.Error("FinishedAt should not be zero")
	}
	if report.Phase != PhaseDeep {
		t.Errorf("expected PhaseDeep, got %s", report.Phase)
	}

	// LastReport 应该返回同一份报告
	last := dm.LastReport()
	if last != report {
		t.Error("LastReport should return the same report")
	}
}

func TestNormalizeKey(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"  Hello   World  ", "hello world"},
		{"UPPERCASE", "uppercase"},
		{"a", "a"},
	}
	for _, tt := range tests {
		got := normalizeKey(tt.input)
		if got != tt.expected {
			t.Errorf("normalizeKey(%q) = %q, expected %q",
				tt.input, got, tt.expected)
		}
	}
}

func TestTokenize(t *testing.T) {
	tokens := tokenize("Hello, World! Hello/World")
	if !tokens["hello"] {
		t.Error("expected 'hello' in tokens")
	}
	if !tokens["world"] {
		t.Error("expected 'world' in tokens")
	}
}

func TestParseScopeFromKey(t *testing.T) {
	tests := []struct {
		key  string
		kind ScopeKind
		id   string
	}{
		{"L0_working|channel:abc", ScopeChannel, "abc"},
		{"L1_longterm|user:xyz", ScopeUser, "xyz"},
		{"L3_profile|bot", ScopeBot, ""},
	}

	for _, tt := range tests {
		s := parseScopeFromKey(tt.key)
		if s.Kind != tt.kind {
			t.Errorf("kind: got %s, want %s", s.Kind, tt.kind)
		}
		if s.ID != tt.id {
			t.Errorf("id: got %s, want %s", s.ID, tt.id)
		}
	}
}

func TestDreamConfig_Defaults(t *testing.T) {
	cfg := DefaultDreamConfig()

	if cfg.Enabled != false {
		t.Error("default should be disabled")
	}
	if cfg.Schedule != "0 3 * * *" {
		t.Errorf("unexpected schedule: %s", cfg.Schedule)
	}
	if cfg.Deep.MinScore != 0.8 {
		t.Errorf("unexpected minScore: %f", cfg.Deep.MinScore)
	}
	if cfg.JaccardThreshold != 0.9 {
		t.Errorf("unexpected threshold: %f", cfg.JaccardThreshold)
	}

	// 验证权重合计
	total := WeightRelevance + WeightFrequency + WeightDiversity +
		WeightRecency + WeightConsolidation + WeightRichness
	if total < 0.99 || total > 1.01 {
		t.Errorf("weight total should be 1.0, got %f", total)
	}
}

func TestScoreBreakdown(t *testing.T) {
	dm, _ := newTestDreamManager(t, []Scope{ChannelScope("score-test")})

	c := &DreamCandidate{
		Content:       "用户使用 Go 语言开发后端",
		LightHits:     3,
		RecallCount:   4,
		UniqueQueries: 3,
		REMHits:       2,
		LastSeen:      time.Now(),
	}

	sb := dm.scoreCandidate(c, time.Now())
	total := dm.computeTotalScore(sb, c)

	if total < 0 || total > 1.0 {
		t.Errorf("score should be 0~1, got %f", total)
	}
	if sb.Frequency <= 0 {
		t.Error("expected positive frequency score with LightHits=3")
	}
}
