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

type countingRetriever struct {
	fakeRetriever
	calls int
}

func (c *countingRetriever) Recent(ctx context.Context, scope Scope, limit int) ([]Entry, error) {
	c.calls++
	return c.fakeRetriever.Recent(ctx, scope, limit)
}
