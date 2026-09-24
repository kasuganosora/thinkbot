package memory

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// fakeRetriever 内存检索器：entries 按时间倒序存放（[0] 最新），
// Recent 返回前 limit 条，用于复现「窗口外的老记忆进不了候选集」的场景。
type fakeRetriever struct {
	entries []Entry
}

func (f *fakeRetriever) Retrieve(_ context.Context, q Query) ([]Entry, error) {
	out := make([]Entry, 0, len(f.entries))
	for _, e := range f.entries {
		if q.MinImportance > 0 && e.Importance < q.MinImportance {
			continue
		}
		out = append(out, e)
	}
	// 真实 SQL 的 OrderAsc 是「最早在前」。entries 按时间倒序存放（[0] 最新），
	// 因此升序要反转。必须模拟这一点，否则测不出 OrderAsc+Limit 的截断退化。
	if q.Order == OrderAsc {
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
	}
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out, nil
}

func (f *fakeRetriever) Recent(_ context.Context, _ Scope, limit int) ([]Entry, error) {
	if limit <= 0 {
		return nil, nil
	}
	if limit > len(f.entries) {
		limit = len(f.entries)
	}
	out := make([]Entry, limit)
	copy(out, f.entries)
	return out, nil
}

func (f *fakeRetriever) Count(_ context.Context, _ Scope) (int, error) {
	return len(f.entries), nil
}

func TestRelevanceScore_TopicalMatchBeatsNoise(t *testing.T) {
	related := RelevanceScore("鹦鹉", "用户养鹦鹉，最近空调低温又死一只")
	noise := RelevanceScore("鹦鹉", "今天天气不错，出门散步")

	if related <= noise {
		t.Fatalf("related(%f) should beat noise(%f)", related, noise)
	}
	if noise != 0 {
		t.Errorf("unrelated content should score 0, got %f", noise)
	}
}

func TestRelevanceScore_EmptyOrNoOverlap(t *testing.T) {
	if got := RelevanceScore("", "任意内容"); got != 0 {
		t.Errorf("empty query should be 0, got %f", got)
	}
	if got := RelevanceScore("查询", ""); got != 0 {
		t.Errorf("empty content should be 0, got %f", got)
	}
}

func TestSelectRelevant_TopKAndExclude(t *testing.T) {
	candidates := []Entry{
		{ID: "a", Content: "用户养鹦鹉"},
		{ID: "b", Content: "鹦鹉死了"},
		{ID: "c", Content: "鹦鹉饲料"},
		{ID: "d", Content: "今天天气不错"},
	}
	exclude := map[string]struct{}{"a": {}}

	picked := SelectRelevant("鹦鹉", candidates, exclude, 2)
	if len(picked) != 2 {
		t.Fatalf("expected 2 picked, got %d", len(picked))
	}
	for _, p := range picked {
		if p.Entry.ID == "a" {
			t.Error("excluded entry should not be picked")
		}
		if p.Entry.ID == "d" {
			t.Error("zero-score entry should not be picked")
		}
	}
	if picked[0].Score < picked[1].Score {
		t.Error("picked entries should be sorted by score desc")
	}
}

func TestRelevanceGateAndBoost(t *testing.T) {
	entries := []Entry{
		{ID: "1", Importance: 0.9},
		{ID: "2", Importance: 0.5},
		{ID: "3", Importance: 0.3},
	}
	// maxEntries=2 → 末位入选者的 importance 为 0.5
	if gate := relevanceGate(entries, 2); gate != 0.5 {
		t.Fatalf("gate should be 0.5, got %f", gate)
	}

	picked := []ScoredEntry{{Entry: Entry{ID: "old", Importance: 0.1}, Score: 0.5}}
	boostRelevance(picked, 0.5)
	if picked[0].Entry.Importance <= 0.5 {
		t.Fatalf("boosted importance must exceed gate, got %f", picked[0].Entry.Importance)
	}
	if picked[0].Entry.Importance > 1 {
		t.Fatalf("importance must stay <= 1, got %f", picked[0].Entry.Importance)
	}

	// 相关性更高的条目应获得更高的有效 importance
	higher := []ScoredEntry{{Entry: Entry{ID: "x"}, Score: 0.9}}
	lower := []ScoredEntry{{Entry: Entry{ID: "y"}, Score: 0.1}}
	boostRelevance(higher, 0.3)
	boostRelevance(lower, 0.3)
	if higher[0].Entry.Importance <= lower[0].Entry.Importance {
		t.Error("higher relevance should yield higher effective importance")
	}
}

// TestSnapshot_RelevanceRecallBringsBackOldMemory 是本次修改的核心回归：
//
// 复现 2026-09-24 的真实诊断——主 scope 记忆量远超 Recent 窗口（实测 2472 条 vs 50），
// importance 最高的老记忆全部落在窗口外，导致永不浮现。
// 关闭相关性通道时老记忆不出现；开启后必须被补回。
func TestSnapshot_RelevanceRecallBringsBackOldMemory(t *testing.T) {
	scope := ChannelScope("misskey:timeline")

	var entries []Entry
	for i := 0; i < 100; i++ {
		entries = append(entries, Entry{
			ID:         fmt.Sprintf("m%02d", i),
			Scope:      scope,
			Content:    fmt.Sprintf("今天天气不错 %03d", i),
			Importance: 0.30,
		})
	}
	// 索引 60 = 第 61 新，落在 Recent(50) 窗口之外；importance 最高但照样出不来。
	entries[60] = Entry{
		ID:         "old-parrot",
		Scope:      scope,
		Content:    "用户养鹦鹉，最近空调低温又死一只，另一只得疥廯",
		Importance: 0.80,
	}

	retriever := &fakeRetriever{entries: entries}
	query := "鹦鹉怎么了"

	// 关闭：老记忆不在候选窗口内，不应出现
	offCfg := DefaultSnapshotConfig()
	offCfg.Mode = ModeFrozen
	offCfg.RelevanceRecall = false
	offCfg.Query = query
	off := NewSnapshot(offCfg)
	if err := off.Init(context.Background(), retriever, []Scope{scope}); err != nil {
		t.Fatalf("init(off) failed: %v", err)
	}
	if strings.Contains(off.MemorySnapshot(), "鹦鹉") {
		t.Fatal("old memory should NOT be recalled when relevance recall is off")
	}

	// 开启：老记忆应被相关性通道补回
	onCfg := DefaultSnapshotConfig()
	onCfg.Mode = ModeFrozen
	onCfg.RelevanceRecall = true
	onCfg.Query = query
	on := NewSnapshot(onCfg)
	if err := on.Init(context.Background(), retriever, []Scope{scope}); err != nil {
		t.Fatalf("init(on) failed: %v", err)
	}
	if !strings.Contains(on.MemorySnapshot(), "鹦鹉") {
		t.Fatal("old memory should be recalled when relevance recall is on")
	}
}

// TestSnapshot_ImportantMemoryBeyondAnyWindow 复现「时间窗口再大也捞不到」的场景：
//
// 本机 misskey scope 里 importance 最高的 7 条排在 2400 位之后，1000 条窗口同样
// 覆盖不到——只有按 importance 直接取的保底通道能救回它们。
func TestSnapshot_ImportantMemoryBeyondAnyWindow(t *testing.T) {
	scope := ChannelScope("misskey:timeline")

	// 3000 条：索引越大越老。高价值记忆放在索引 2900（远超任何合理时间窗口）。
	var entries []Entry
	for i := 0; i < 3000; i++ {
		entries = append(entries, Entry{
			ID:         fmt.Sprintf("m%04d", i),
			Scope:      scope,
			Content:    fmt.Sprintf("今天天气不错 %04d", i),
			Importance: 0.30,
		})
	}
	entries[2900] = Entry{
		ID:         "core-preference",
		Scope:      scope,
		Content:    "@luna 讨厌反复回复的 bot",
		Importance: 0.80,
	}

	retriever := &fakeRetriever{entries: entries}
	cfg := DefaultSnapshotConfig()
	cfg.Mode = ModeFrozen
	cfg.RelevanceRecall = true
	cfg.Query = "今天天气" // 与高价值记忆无关，排除相关性通道的干扰

	snap := NewSnapshot(cfg)
	if err := snap.Init(context.Background(), retriever, []Scope{scope}); err != nil {
		t.Fatalf("init failed: %v", err)
	}
	if !strings.Contains(snap.MemorySnapshot(), "讨厌反复回复的 bot") {
		t.Fatal("high-importance memory beyond any time window should be recalled")
	}
}

// TestSnapshot_RelevanceRecallOffKeepsBaseline 保证默认关闭时行为完全不变：
// 不触发额外的宽窗口检索（只走主通道的 Recent）。
func TestSnapshot_RelevanceRecallOffKeepsBaseline(t *testing.T) {
	scope := ChannelScope("test")
	entries := []Entry{
		{ID: "1", Scope: scope, Content: "第一条记忆", Importance: 0.5},
		{ID: "2", Scope: scope, Content: "第二条记忆", Importance: 0.4},
	}
	counting := &countingRetriever{fakeRetriever: fakeRetriever{entries: entries}}

	cfg := DefaultSnapshotConfig()
	cfg.Mode = ModeFrozen
	snap := NewSnapshot(cfg)
	if err := snap.Init(context.Background(), counting, []Scope{scope}); err != nil {
		t.Fatalf("init failed: %v", err)
	}

	if counting.calls != 1 {
		t.Fatalf("relevance off should only call Recent once per scope, got %d", counting.calls)
	}
}

// TestSelectImportant_TopEntryNotLostToScanOrder 复现「OrderAsc + 小 Limit」的截断退化。
//
// 本机实测：importance>=0.7 共 641 条，其中 1.00 分条目按创建时间升序排在第
// 516/573 位。用 OrderAsc + Limit(200) 扫描时只有最早的 200 条进入选择范围，
// 这些最高分条目会被永久漏掉，抓到的反而是最早创建的人格设定条目。
// 正确行为：排序在内存完成，与创建顺序无关。
func TestSelectImportant_TopEntryNotLostToScanOrder(t *testing.T) {
	scope := ChannelScope("misskey:timeline")

	// 全部满足阈值，模拟本机 641 条的规模（> 旧的 importantScanLimit=200）。
	total := 300
	var entries []Entry
	for i := 0; i < total; i++ {
		entries = append(entries, Entry{
			ID:         fmt.Sprintf("m%03d", i),
			Scope:      scope,
			Content:    fmt.Sprintf("候选记忆 %03d", i),
			Importance: 0.70,
		})
	}
	// 最高分条目放在「时间较新」一侧（entries[0] 最新）。
	// 按时间升序排它落在第 total-5 位，旧的 Limit=200 扫不到。
	entries[5] = Entry{
		ID:         "top-value",
		Scope:      scope,
		Content:    "用户的核心偏好：讨厌反复回复的 bot",
		Importance: 1.00,
	}

	// 无 query：相关性恒为 0，应退化为纯 importance 排序。
	got := SelectImportant(context.Background(), &fakeRetriever{entries: entries},
		[]Scope{scope}, nil, "", DefaultImportantMinImportance, DefaultImportantRelevanceWeight, 3, DefaultRecalledMaxChars)

	if len(got) == 0 {
		t.Fatal("expected high-value entries to be selected")
	}
	if got[0].Entry.ID != "top-value" {
		t.Fatalf("highest importance entry must win regardless of creation order, got %q", got[0].Entry.ID)
	}
	// 无 query 时没有相关性信号，Score 应恒为 0（见 SelectImportant 注释）。
	if got[0].Score != 0 {
		t.Fatalf("important channel score must be 0 without a query, got %v", got[0].Score)
	}
}

// TestSelectImportant_RelevanceBeatsRawImportance 复现「名额被无关的高分条目占满」。
//
// 本机实测：保底通道按纯 importance 取时，名额恒定被同一批 0.95 条目占满
// （心理状态汇总等，与当轮话题无关），而 0.800 的长期事实（用户对 bot 行为的
// 偏好）一条都进不来。加入相关性后，话题相关的老记忆应能反超。
func TestSelectImportant_RelevanceBeatsRawImportance(t *testing.T) {
	scope := ChannelScope("misskey:timeline")
	entries := []Entry{
		{ID: "unrelated-095", Scope: scope, Content: "用户的心理状态汇总：最近情绪起伏较大，需要注意休息", Importance: 0.95},
		{ID: "unrelated-094", Scope: scope, Content: "今天讨论了一下晚饭吃什么，最后决定点外卖", Importance: 0.94},
		{ID: "target-080", Scope: scope, Content: "@luna gets annoyed by bots that reply repeatedly", Importance: 0.80},
	}

	query := "luna 讨厌什么样的 bot，反复回复会怎样"
	got := SelectImportant(context.Background(), &fakeRetriever{entries: entries},
		[]Scope{scope}, nil, query, 0.7, DefaultImportantRelevanceWeight, 1, DefaultRecalledMaxChars)

	if len(got) == 0 {
		t.Fatal("expected one entry to be selected")
	}
	if got[0].Entry.ID != "target-080" {
		t.Fatalf("query-relevant 0.80 memory must outrank irrelevant 0.95 entries, got %q", got[0].Entry.ID)
	}
	if got[0].Score <= 0 {
		t.Fatalf("selected entry should carry a positive relevance score, got %v", got[0].Score)
	}

	// 权重为 0 时应退回纯 importance 排序，证明权重是可控旋钮而非硬编码行为。
	plain := SelectImportant(context.Background(), &fakeRetriever{entries: entries},
		[]Scope{scope}, nil, query, 0.7, 0, 1, DefaultRecalledMaxChars)
	if len(plain) == 0 || plain[0].Entry.ID != "unrelated-095" {
		t.Fatalf("with weight 0 the channel must fall back to pure importance, got %+v", plain)
	}
}

// TestSelectImportant_FallsBackToImportanceWithoutQuery 无 query 时行为必须与
// 改动前一致（纯 importance 降序），即相关性只做增量、不改变无输入时的结果。
func TestSelectImportant_FallsBackToImportanceWithoutQuery(t *testing.T) {
	scope := ChannelScope("misskey:timeline")
	entries := []Entry{
		{ID: "a", Scope: scope, Content: "关于鹦鹉的饲养经验分享", Importance: 0.75},
		{ID: "b", Scope: scope, Content: "用户的核心偏好：讨厌反复回复的 bot", Importance: 0.90},
	}

	got := SelectImportant(context.Background(), &fakeRetriever{entries: entries},
		[]Scope{scope}, nil, "", 0.7, DefaultImportantRelevanceWeight, 2, DefaultRecalledMaxChars)
	if len(got) != 2 || got[0].Entry.ID != "b" {
		t.Fatalf("without query, ordering must be by importance, got %+v", got)
	}
}

// TestCapRecalled_SkipsOverlongInsteadOfTruncating 保证超长条目被丢弃而非截成残句。
//
// 本机实测会命中「【栞娜人格设定】」(701/951 字符) 与「luna 完整档案」(2287 字符)，
// 截断到 240 字符后注入 prompt 的是断头的人格定义，比不注入更糟。
func TestCapRecalled_SkipsOverlongInsteadOfTruncating(t *testing.T) {
	got := capRecalled([]Entry{
		{ID: "persona", Content: strings.Repeat("栞", 1000)},
		{ID: "normal", Content: "用户讨厌反复回复的 bot"},
		{ID: "mid", Content: strings.Repeat("记", 300)},
	}, 240)

	for _, e := range got {
		if e.ID == "persona" {
			t.Fatal("overlong entries must be dropped, not truncated into fragments")
		}
	}

	var normal, mid string
	for _, e := range got {
		switch e.ID {
		case "normal":
			normal = e.Content
		case "mid":
			mid = e.Content
		}
	}
	if normal != "用户讨厌反复回复的 bot" {
		t.Fatalf("short entries should pass through unchanged, got %q", normal)
	}
	if len([]rune(mid)) != 241 { // 240 + 省略号
		t.Fatalf("mid entry should be capped at 240 chars + ellipsis, got %d", len([]rune(mid)))
	}
}

// TestSnapshot_ImportantChannelSkipsPersonaSizedMemories 端到端：人格设定/档案量级的
// 高 importance 记忆不该被补进注入块，而正常长度的高价值记忆仍要能救回。
func TestSnapshot_ImportantChannelSkipsPersonaSizedMemories(t *testing.T) {
	scope := ChannelScope("misskey:timeline")
	persona := "【栞娜（Kanna）人格设定】" + strings.Repeat("身份：女仆Bot，服务于大小姐露娜。", 40)

	// 先用 60 条低分填充占满主通道的 Recent(50) 窗口，
	// 确保两条高价值记忆只能经由补充通道进入（否则测不到补充通道的行为）。
	var entries []Entry
	for i := 0; i < 60; i++ {
		entries = append(entries, Entry{
			ID:         fmt.Sprintf("fill%02d", i),
			Scope:      scope,
			Content:    fmt.Sprintf("今天天气不错 %02d", i),
			Importance: 0.30,
		})
	}
	entries = append(entries,
		Entry{ID: "persona-1", Scope: scope, Content: persona, Importance: 0.95},
		Entry{ID: "core-pref", Scope: scope, Content: "@luna 讨厌反复回复的 bot", Importance: 0.90},
	)

	cfg := DefaultSnapshotConfig()
	cfg.Mode = ModeFrozen
	cfg.RelevanceRecall = true
	cfg.Query = "今天天气" // 与两条高价值记忆都无关，排除相关性通道干扰

	snap := NewSnapshot(cfg)
	if err := snap.Init(context.Background(), &fakeRetriever{entries: entries}, []Scope{scope}); err != nil {
		t.Fatalf("init failed: %v", err)
	}

	text := snap.MemorySnapshot()
	if strings.Contains(text, "人格设定") {
		t.Fatal("persona-sized memory must not be injected as a truncated fragment")
	}
	if !strings.Contains(text, "讨厌反复回复的 bot") {
		t.Fatal("normal-sized high-value memory should still be recalled")
	}
}

// TestCapRendered_DropsArchiveSizedEntries 复现「单条巨型记忆吃满整个预算」。
//
// 本机实测：bot scope 有两条 importance=1.00、14808/15314 字符的完整档案，
// sort 后永远排第一，单条就把 2200 字符预算吃满，注入块是 "showing 1 by
// importance" ——bot 每轮只看得到一份档案的前 2200 字残片。
func TestCapRendered_DropsArchiveSizedEntries(t *testing.T) {
	entries := []Entry{
		{ID: "archive", Content: strings.Repeat("档", 15000), Importance: 1.0},
		// 600 字：在跳过门槛（240*4=960）之内，应被截断而非丢弃
		{ID: "mid", Content: strings.Repeat("记", 600), Importance: 0.9},
		{ID: "short", Content: "用户讨厌反复回复的 bot", Importance: 0.8},
	}

	kept, dropped := capRendered(entries, DefaultRenderedMaxChars)
	if dropped != 1 {
		t.Fatalf("the 15000-char archive must be dropped, dropped=%d", dropped)
	}
	for _, e := range kept {
		if e.ID == "archive" {
			t.Fatal("archive-sized entry must never reach the prompt")
		}
	}

	var short, mid string
	for _, e := range kept {
		switch e.ID {
		case "short":
			short = e.Content
		case "mid":
			mid = e.Content
		}
	}
	if short != "用户讨厌反复回复的 bot" {
		t.Fatalf("short entries should pass through unchanged, got %q", short)
	}
	if len([]rune(mid)) != DefaultRenderedMaxChars+1 {
		t.Fatalf("mid entry should be capped at %d chars + ellipsis, got %d",
			DefaultRenderedMaxChars, len([]rune(mid)))
	}

	// maxChars<=0 表示不启用，必须原样返回（保持现状）。
	unchanged, dropped2 := capRendered(entries, 0)
	if len(unchanged) != len(entries) || dropped2 != 0 {
		t.Fatal("capRendered must be a no-op when disabled")
	}
}

// TestSnapshot_RecalledSurvivesOversizedTopEntry 端到端：主通道存在巨型档案时，
// 补充召回的相关老记忆仍必须出现在注入块里。
//
// 这条覆盖了两个必须同时成立的条件，缺一个都测不出问题：
//  1. 巨型档案被丢弃（否则它单条吃满预算，后面一条都进不来）；
//  2. 补充条目的抬升基准是主通道的**最高** importance 而不是第 MaxEntries 名
//     （实测抬到第 20 名时，相关条目 rel=1.0 排第一却仍被字符预算切掉）。
func TestSnapshot_RecalledSurvivesOversizedTopEntry(t *testing.T) {
	scope := ChannelScope("misskey:timeline")

	// 主通道窗口：一条 1.00 分的巨型档案 + 60 条低分近期记忆。
	entries := []Entry{
		{ID: "archive", Scope: scope, Content: "【luna 完整档案】" + strings.Repeat("项目全记录。", 1200), Importance: 1.0},
	}
	for i := 0; i < 60; i++ {
		entries = append(entries, Entry{
			ID:         fmt.Sprintf("fill%02d", i),
			Scope:      scope,
			Content:    fmt.Sprintf("今天天气不错 %02d", i),
			Importance: 0.30,
		})
	}
	// 目标老记忆：排在 Recent(50) 窗口之外，只能靠补充通道（importance 达标）。
	entries = append(entries, Entry{
		ID:         "target-old",
		Scope:      scope,
		Content:    "@luna gets annoyed by bots that reply repeatedly",
		Importance: 0.80,
	})

	cfg := DefaultSnapshotConfig()
	cfg.Mode = ModeFrozen
	cfg.RelevanceRecall = true
	cfg.MaxRenderedEntryChars = DefaultRenderedMaxChars
	cfg.Query = "luna 讨厌反复回复的 bot 会怎样"

	snap := NewSnapshot(cfg)
	if err := snap.Init(context.Background(), &fakeRetriever{entries: entries}, []Scope{scope}); err != nil {
		t.Fatalf("init failed: %v", err)
	}

	text := snap.MemorySnapshot()
	if strings.Contains(text, "完整档案") {
		t.Fatal("archive-sized entry must not occupy the memory block")
	}
	if !strings.Contains(text, "annoyed by bots") {
		t.Fatalf("relevant old memory must survive both the entry cap and the char budget:\n%s", text)
	}
}

type countingRetriever struct {
	fakeRetriever
	calls int
}

func (c *countingRetriever) Recent(ctx context.Context, scope Scope, limit int) ([]Entry, error) {
	c.calls++
	return c.fakeRetriever.Recent(ctx, scope, limit)
}
