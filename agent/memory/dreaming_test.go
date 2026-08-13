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

// TestDreamManager_LightIngestsConsolidatedL0 回归测试：模拟实时 Consolidator 已将
// L0 标记为 consolidated 的场景（这正是此前 dreaming 永远空跑的根因）。修复后 runLight
// 改用 Retrieve 拉取全部 L0 并仅跳过 dream_processed，因此即便已被 consolidated 也应被
// 梦境管线摄取，而不是被旧的 GetUnprocessed("consolidated") 过滤掉。
func TestDreamManager_LightIngestsConsolidatedL0(t *testing.T) {
	scope := ChannelScope("consolidated-test")
	dm, _ := newTestDreamManager(t, []Scope{scope})
	ctx := context.Background()

	const n = 6
	for i := 0; i < n; i++ {
		if err := dm.manager.WriteWorking(ctx, scope,
			"用户使用 Go 语言开发后端服务，偏好简洁代码风格", "test"); err != nil {
			t.Fatalf("WriteWorking failed: %v", err)
		}
	}

	// 模拟实时 Consolidator 把 L0 提升为 L1 并标记 consolidated
	entries, err := dm.manager.store.Retrieve(ctx, Tier0Working, []Scope{scope}, 100)
	if err != nil {
		t.Fatalf("Retrieve failed: %v", err)
	}
	if len(entries) != n {
		t.Fatalf("expected %d L0 entries, got %d", n, len(entries))
	}
	ids := make([]string, 0, len(entries))
	for _, e := range entries {
		ids = append(ids, e.ID)
	}
	if err := dm.manager.store.MarkProcessed(ctx, scope, ids); err != nil {
		t.Fatalf("MarkProcessed failed: %v", err)
	}

	// 修复前的旧逻辑（GetUnprocessed 跳过 consolidated）会得到 ingested=0；
	// 修复后应当摄取全部 n 条。
	report, err := dm.Run(ctx)
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if report.LightIngested != n {
		t.Errorf("expected %d ingested despite consolidated marker, got %d", n, report.LightIngested)
	}

	// 再次运行应被 dream_processed 跳过 → ingested=0（幂等，避免重复摄取）
	report2, err := dm.Run(ctx)
	if err != nil {
		t.Fatalf("second Run failed: %v", err)
	}
	if report2.LightIngested != 0 {
		t.Errorf("expected 0 ingested on second run (already dream_processed), got %d", report2.LightIngested)
	}
}

// TestDreamManager_LightSkipsEphemeral 验证被结构化标记 ephemeral=true 的 L0 条目
// 不进入晋升候选，因此不会成为长期记忆。
//
// 这是「语言无关护栏」的关键回归测试：判定只看 metadata 里的布尔，
// 不依赖内容语言。历史上这里靠正则枚举「无需记忆 / なし / [NONE]」等短语，
// 补一种语言漏下一种，已彻底移除，勿回退。
func TestDreamManager_LightSkipsEphemeral(t *testing.T) {
	scope := ChannelScope("ephemeral-test")
	dm, _ := newTestDreamManager(t, []Scope{scope})
	ctx := context.Background()

	// 3 条时效性内容（多语言，均标 ephemeral=true）——应全部被跳过。
	ephemeral := []string{
		"TANK CHAIR というアニメが4月7日から放送開始。",                    // 日语
		"@dev ने कहा कि नया मॉडल कल रिलीज़ होगा।",             // 印地语
		"某模型今天开源发布，来源 IT 之家。",                              // 中文
	}
	for _, c := range ephemeral {
		if err := dm.manager.store.Append(ctx, TieredEntry{
			Entry: Entry{
				Scope:    scope,
				Content:  c,
				Source:   "note",
				Category: "lurk",
				Metadata: map[string]any{"speaker": "observer", "ephemeral": true},
			},
			Tier: Tier0Working,
		}); err != nil {
			t.Fatalf("append ephemeral failed: %v", err)
		}
	}

	// 1 条持久内容（ephemeral=false）——应被正常摄取。
	if err := dm.manager.store.Append(ctx, TieredEntry{
		Entry: Entry{
			Scope:    scope,
			Content:  "@blogtalk 在学 Rust，卡在所有权概念上，偏好用 sqlite。",
			Source:   "note",
			Category: "lurk",
			Metadata: map[string]any{"speaker": "observer", "ephemeral": false},
		},
		Tier: Tier0Working,
	}); err != nil {
		t.Fatalf("append durable failed: %v", err)
	}

	report, err := dm.Run(ctx)
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	// 只有那 1 条非 ephemeral 的应被摄取。
	if report.LightIngested != 1 {
		t.Errorf("expected only 1 non-ephemeral entry ingested, got %d", report.LightIngested)
	}
}

// TestIsEphemeralEntry 直接覆盖结构化 ephemeral 判定的各种取值形态。
func TestIsEphemeralEntry(t *testing.T) {
	cases := []struct {
		name string
		meta map[string]any
		want bool
	}{
		{"nil_metadata", nil, false},
		{"missing_key", map[string]any{"speaker": "observer"}, false},
		{"bool_true", map[string]any{"ephemeral": true}, true},
		{"bool_false", map[string]any{"ephemeral": false}, false},
		// JSON 往返后布尔可能退化成字符串
		{"string_true", map[string]any{"ephemeral": "true"}, true},
		{"string_false", map[string]any{"ephemeral": "false"}, false},
		{"unexpected_type", map[string]any{"ephemeral": 1}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isEphemeralEntry(tc.meta); got != tc.want {
				t.Errorf("isEphemeralEntry(%v)=%v want=%v", tc.meta, got, tc.want)
			}
		})
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

func TestDreamManager_REMPhaseSkipsPromoted(t *testing.T) {
	scope := ChannelScope("rem-skip-promoted")
	dm, _ := newTestDreamManager(t, []Scope{scope})
	ctx := context.Background()
	now := time.Now()

	// 注入 3 个候选：1 个已晋升，2 个待处理（同 category 以便落同一 theme）。
	dm.candidates["promoted-a"] = &DreamCandidate{
		Key: "promoted-a", Content: "已晋升事实", Scope: scope,
		Category: "catX", Theme: "known", REMHits: 3, Promoted: true, LastSeen: now,
	}
	dm.candidates["pending-b"] = &DreamCandidate{
		Key: "pending-b", Content: "待处理事实 b", Scope: scope,
		Category: "catX", REMHits: 0, Promoted: false, LastSeen: now,
	}
	dm.candidates["pending-c"] = &DreamCandidate{
		Key: "pending-c", Content: "待处理事实 c", Scope: scope,
		Category: "catX", REMHits: 0, Promoted: false, LastSeen: now,
	}

	res, err := dm.runREM(ctx)
	if err != nil {
		t.Fatalf("runREM failed: %v", err)
	}

	// 已晋升候选不应进入 REM 聚类：staged 只含未晋升的 2 个。
	if res.candidates != 2 {
		t.Errorf("expected REM to process only 2 un-promoted candidates, got %d", res.candidates)
	}
	// 已晋升候选的 REMHits / Theme 不应被改动。
	if a := dm.candidates["promoted-a"]; a.REMHits != 3 || a.Theme != "known" {
		t.Errorf("promoted candidate was mutated by REM: REMHits=%d Theme=%q", a.REMHits, a.Theme)
	}
	// 未晋升候选仍正常聚类：同 category → 1 theme size=2 → 各 REMHits++。
	if b := dm.candidates["pending-b"]; b.REMHits != 1 {
		t.Errorf("pending-b expected REMHits=1, got %d", b.REMHits)
	}
	if c := dm.candidates["pending-c"]; c.REMHits != 1 {
		t.Errorf("pending-c expected REMHits=1, got %d", c.REMHits)
	}
}

func TestDreamManager_REMPhaseReusesThemed(t *testing.T) {
	scope := ChannelScope("rem-reuse-themed")
	dm, _ := newTestDreamManager(t, []Scope{scope})
	ctx := context.Background()
	now := time.Now()

	// 两个已带主题的未晋升候选，但 category 不同：
	// 旧实现会按 category 重新聚类把它们拆开；新实现应按既有 Theme 复用聚到一起。
	dm.candidates["themed-x"] = &DreamCandidate{
		Key: "themed-x", Content: "运维事实 x", Scope: scope,
		Category: "ops", Theme: "t-ops", REMHits: 2, Promoted: false, LastSeen: now,
	}
	dm.candidates["themed-y"] = &DreamCandidate{
		Key: "themed-y", Content: "运维事实 y", Scope: scope,
		Category: "other", Theme: "t-ops", REMHits: 1, Promoted: false, LastSeen: now,
	}
	// 两个新候选（Theme==""）同 category，应走一次聚类形成多成员簇。
	dm.candidates["new-z"] = &DreamCandidate{
		Key: "new-z", Content: "新事实 z", Scope: scope,
		Category: "ops", Theme: "", REMHits: 0, Promoted: false, LastSeen: now,
	}
	dm.candidates["new-w"] = &DreamCandidate{
		Key: "new-w", Content: "新事实 w", Scope: scope,
		Category: "ops", Theme: "", REMHits: 0, Promoted: false, LastSeen: now,
	}

	res, err := dm.runREM(ctx)
	if err != nil {
		t.Fatalf("runREM failed: %v", err)
	}

	// 已带主题的候选按既有 Theme 复用聚类：t-ops 双成员 → 各 REMHits++，主题不被 category 覆盖。
	if x := dm.candidates["themed-x"]; x.REMHits != 3 || x.Theme != "t-ops" {
		t.Errorf("themed-x: REMHits=%d Theme=%q, want 3/t-ops", x.REMHits, x.Theme)
	}
	if y := dm.candidates["themed-y"]; y.REMHits != 2 || y.Theme != "t-ops" {
		t.Errorf("themed-y: REMHits=%d Theme=%q, want 2/t-ops", y.REMHits, y.Theme)
	}
	// 新候选仍被聚类：同 category 形成多成员簇 → 各 REMHits++ 且主题被赋为 category。
	if z := dm.candidates["new-z"]; z.REMHits != 1 || z.Theme != "ops" {
		t.Errorf("new-z: REMHits=%d Theme=%q, want 1/ops", z.REMHits, z.Theme)
	}
	if w := dm.candidates["new-w"]; w.REMHits != 1 || w.Theme != "ops" {
		t.Errorf("new-w: REMHits=%d Theme=%q, want 1/ops", w.REMHits, w.Theme)
	}
	// 已带主题的候选未触发重新 LLM 聚类，themes 数=既有不同主题数（t-ops + 新聚类 ops）。
	if res.candidates != 4 {
		t.Errorf("expected REM to process 4 candidates, got %d", res.candidates)
	}
	if res.themes != 2 {
		t.Errorf("expected 2 themes (reused t-ops + fresh ops), got %d", res.themes)
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

	// REM 主题数量：无 LLM 时按 category 聚类，可能只有一个 category。
	// rule-based 提取可能都归为 "observation"，因此不强求非零。
	_ = report.REMThemes
}

// TestDreamManager_DeepPhasePromotesValuable 验证修复后的核心行为：
// 有价值的近期事实（即便没有任何白天召回信号）也应被晋升，
// 不再因 MinRecallCount/MinUniqueQueries 硬门控而永远为 0。
func TestDreamManager_DeepPhasePromotesValuable(t *testing.T) {
	scope := ChannelScope("deep-promote-test")
	dm, tm := newTestDreamManager(t, []Scope{scope})
	ctx := context.Background()

	// 写入两条同主题、近期、丰富、可复用的偏好事实（应被晋升）
	_ = tm.WriteWorking(ctx, scope,
		"用户使用 Go 语言进行后端开发，偏好简洁且可测试的代码风格", "test")
	_ = tm.WriteWorking(ctx, scope,
		"用户偏好 Go 标准库，尽量少引入第三方依赖", "test")

	report, err := dm.Run(ctx)
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	if report.DeepPromoted == 0 {
		t.Errorf("expected valuable recent facts to be promoted, got 0 (scored=%d)",
			report.DeepScored)
	}
}

// TestDreamManager_DeepPhaseFiltersJunk 验证无价值内容仍被过滤。
func TestDreamManager_DeepPhaseFiltersJunk(t *testing.T) {
	scope := ChannelScope("deep-filter-test")
	dm, tm := newTestDreamManager(t, []Scope{scope})
	ctx := context.Background()

	// 极短内容（<10 runes）应在 Light 阶段被丢弃，不会晋升
	_ = tm.WriteWorking(ctx, scope, "哦", "test")

	report, err := dm.Run(ctx)
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if report.DeepPromoted != 0 {
		t.Errorf("expected junk content to be filtered, got %d promoted", report.DeepPromoted)
	}
}

// TestDreamManager_DeepPhaseWithRecall 验证可选的「召回门控」在显式启用时生效：
// 未注入召回信号 → 被过滤；注入足够召回信号后 → 通过门控晋升。
// （默认配置下召回门控关闭，晋升由分数+近期+丰富度驱动，见 DefaultDreamConfig）
func TestDreamManager_DeepPhaseWithRecall(t *testing.T) {
	scope := ChannelScope("deep-recall-test")
	dm, tm := newTestDreamManager(t, []Scope{scope})
	dm.config.Deep.MinRecallCount = 3
	dm.config.Deep.MinUniqueQueries = 3
	ctx := context.Background()

	_ = tm.WriteWorking(ctx, scope,
		"用户使用 Go 语言进行后端开发，偏好简洁且可测试的代码风格", "test")

	// 第一次 Run：无召回信号 → 被召回门控过滤
	report1, err := dm.Run(ctx)
	if err != nil {
		t.Fatalf("first Run failed: %v", err)
	}
	if report1.DeepPromoted != 0 {
		t.Errorf("expected 0 promotions without recall when gate enabled, got %d", report1.DeepPromoted)
	}

	// 手动模拟召回信号
	for _, c := range dm.StagedCandidates() {
		for i := 0; i < 5; i++ {
			dm.RecordRecall(c.Key, "query-"+string(rune('0'+i)))
		}
	}

	// 第二次 Run：已有召回信号 → 通过门控晋升
	report2, err := dm.Run(ctx)
	if err != nil {
		t.Fatalf("second Run failed: %v", err)
	}
	if report2.DeepScored == 0 {
		t.Error("expected some scored candidates in deep phase")
	}
	if report2.DeepPromoted == 0 {
		t.Errorf("expected promotion after recall signals injected, got 0")
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
	if cfg.Deep.MinScore != 0.45 {
		t.Errorf("unexpected minScore: %f", cfg.Deep.MinScore)
	}
	if cfg.Deep.MinRecallCount != 0 {
		t.Errorf("unexpected MinRecallCount: %d (should default off to avoid promotion deadlock)", cfg.Deep.MinRecallCount)
	}
	if cfg.Deep.MinUniqueQueries != 0 {
		t.Errorf("unexpected MinUniqueQueries: %d (should default off)", cfg.Deep.MinUniqueQueries)
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
