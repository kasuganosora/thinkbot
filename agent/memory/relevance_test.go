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

	got := SelectImportant(context.Background(), &fakeRetriever{entries: entries},
		[]Scope{scope}, nil, 0.7, 3)

	if len(got) == 0 {
		t.Fatal("expected high-value entries to be selected")
	}
	if got[0].Entry.ID != "top-value" {
		t.Fatalf("highest importance entry must win regardless of creation order, got %q", got[0].Entry.ID)
	}
	// 保底通道与当前输入无关，Score 应恒为 0（见 SelectImportant 注释）。
	if got[0].Score != 0 {
		t.Fatalf("important channel score must be 0, got %v", got[0].Score)
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

type countingRetriever struct {
	fakeRetriever
	calls int
}

func (c *countingRetriever) Recent(ctx context.Context, scope Scope, limit int) ([]Entry, error) {
	c.calls++
	return c.fakeRetriever.Recent(ctx, scope, limit)
}
