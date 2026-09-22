package storage

import (
	"context"
	"testing"
	"time"

	"github.com/kasuganosora/thinkbot/agent/memory"
)

// ============================================================================
// 时间轴检索 & 统计（回归防线）
//
// 背景事故：用户问 bot「你最早的记忆是什么时候」，bot 答「9 月」，而真实最早
// 记忆是 2026-08-12。根因不是模型笨，而是**检索只能按时间倒序取最近 N 条**：
// 模型无论问什么，看到的都是最近那批，于是把「最近 N 条里最早的」当成
// 「全部最早」。这组用例锁住 order=asc / since / until / MemoryStats 四条能力。
// ============================================================================

// seedTimeSpanRepo 写入跨两个 scope、跨三个月的时间轴样本。
// 返回各条目的创建时间，供断言复用。
func seedTimeSpanRepo(t *testing.T, repo *SQLiteRepository) (jul, aug1, aug15, sep time.Time) {
	t.Helper()
	ctx := context.Background()

	jul = time.Date(2026, 7, 1, 10, 0, 0, 0, time.Local)
	aug1 = time.Date(2026, 8, 1, 10, 0, 0, 0, time.Local)
	aug15 = time.Date(2026, 8, 15, 10, 0, 0, 0, time.Local)
	sep = time.Date(2026, 9, 1, 10, 0, 0, 0, time.Local)

	// bot scope 里藏着一条更早的：只有跨 scope 检索才能看到它。
	entries := []memory.Entry{
		{Scope: memory.BotScope("bot-1"), Content: "july bot memory", CreatedAt: jul},
		{Scope: memory.ChannelScope("ch1"), Content: "august first", CreatedAt: aug1},
		{Scope: memory.ChannelScope("ch1"), Content: "august fifteenth", CreatedAt: aug15},
		{Scope: memory.ChannelScope("ch1"), Content: "september first", CreatedAt: sep},
	}
	for _, e := range entries {
		if err := repo.Append(ctx, e); err != nil {
			t.Fatalf("append %q: %v", e.Content, err)
		}
	}
	return jul, aug1, aug15, sep
}

func TestSQLiteRepository_RetrieveOrderAsc(t *testing.T) {
	db := testDB(t)
	repo := NewSQLiteRepository(db)
	ctx := context.Background()
	jul, aug1, _, sep := seedTimeSpanRepo(t, repo)

	ch := memory.ChannelScope("ch1")

	// 默认（不传 Order）必须仍是倒序 —— 既有调用方行为不得改变。
	got, err := repo.Retrieve(ctx, memory.Query{Scopes: []memory.Scope{ch}, Limit: 10})
	if err != nil {
		t.Fatalf("retrieve desc: %v", err)
	}
	if len(got) != 3 || got[0].Content != "september first" {
		t.Fatalf("expected newest-first by default, got %+v", got)
	}

	// order=asc 才能走到时间轴另一端。
	got, err = repo.Retrieve(ctx, memory.Query{
		Scopes: []memory.Scope{ch}, Limit: 10, Order: memory.OrderAsc,
	})
	if err != nil {
		t.Fatalf("retrieve asc: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(got))
	}
	if got[0].Content != "august first" {
		t.Errorf("order=asc should return oldest first, got %q", got[0].Content)
	}
	if !got[0].CreatedAt.Equal(aug1) {
		t.Errorf("oldest created_at: got %v, want %v", got[0].CreatedAt, aug1)
	}

	// 跨 scope（Scopes=nil）+ 升序 → 命中 bot scope 里那条最早的。
	got, err = repo.Retrieve(ctx, memory.Query{Limit: 10, Order: memory.OrderAsc})
	if err != nil {
		t.Fatalf("retrieve asc all scopes: %v", err)
	}
	if len(got) == 0 || got[0].Content != "july bot memory" {
		t.Fatalf("expected cross-scope oldest 'july bot memory', got %+v", got)
	}
	if !got[0].CreatedAt.Equal(jul) {
		t.Errorf("cross-scope oldest created_at: got %v, want %v", got[0].CreatedAt, jul)
	}
	_ = sep
}

func TestSQLiteRepository_RetrieveTimeRange(t *testing.T) {
	db := testDB(t)
	repo := NewSQLiteRepository(db)
	ctx := context.Background()
	_, _, _, _ = seedTimeSpanRepo(t, repo)

	// 紧贴区间上界的一条：若 since/until 参数被按 UTC 序列化，而库里存的是
	// 带 +08:00 的文本，边界附近 8 小时内的样本会被静默排除。这条用例专门
	// 盯住这个时区口径（差 8 小时才暴露，普通样本察觉不到）。
	lateAug := time.Date(2026, 8, 31, 23, 0, 0, 0, time.Local)
	if err := repo.Append(ctx, memory.Entry{
		Scope:     memory.ChannelScope("ch1"),
		Content:   "august thirty-first late",
		CreatedAt: lateAug,
	}); err != nil {
		t.Fatalf("append: %v", err)
	}

	got, err := repo.Retrieve(ctx, memory.Query{
		Scopes: []memory.Scope{memory.ChannelScope("ch1")},
		Limit:  10,
		Order:  memory.OrderAsc,
		Since:  time.Date(2026, 8, 1, 0, 0, 0, 0, time.Local),
		Until:  time.Date(2026, 8, 31, 23, 59, 59, 0, time.Local),
	})
	if err != nil {
		t.Fatalf("retrieve range: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected the 3 August entries, got %d: %+v", len(got), got)
	}
	if got[0].Content != "august first" || got[1].Content != "august fifteenth" {
		t.Errorf("unexpected August ordering: %+v", got)
	}
	if got[2].Content != "august thirty-first late" {
		t.Errorf("entry near the upper bound must survive the range filter, got %+v", got)
	}
}

func TestSQLiteRepository_MemoryStats(t *testing.T) {
	db := testDB(t)
	repo := NewSQLiteRepository(db)
	ctx := context.Background()
	jul, aug1, _, sep := seedTimeSpanRepo(t, repo)

	// 全库统计：这是「我最早的记忆是什么时候」的权威答案来源。
	var provider memory.MemoryStatsProvider = repo
	info, err := provider.MemoryStats(ctx, nil)
	if err != nil {
		t.Fatalf("memory stats (all): %v", err)
	}
	if info.Total != 4 {
		t.Errorf("total: got %d, want 4", info.Total)
	}
	if !sameMoment(info.Oldest, jul) {
		t.Errorf("oldest: got %v, want %v", info.Oldest, jul)
	}
	if !sameMoment(info.Newest, sep) {
		t.Errorf("newest: got %v, want %v", info.Newest, sep)
	}

	// 单 scope 统计：只算该频道的条数和时间跨度。
	info, err = provider.MemoryStats(ctx, []memory.Scope{memory.ChannelScope("ch1")})
	if err != nil {
		t.Fatalf("memory stats (channel): %v", err)
	}
	if info.Total != 3 {
		t.Errorf("channel total: got %d, want 3", info.Total)
	}
	if !sameMoment(info.Oldest, aug1) {
		t.Errorf("channel oldest: got %v, want %v", info.Oldest, aug1)
	}

	// 空 scope：不能因为没数据就报错或编造时间。
	empty, err := provider.MemoryStats(ctx, []memory.Scope{memory.ChannelScope("nonexistent")})
	if err != nil {
		t.Fatalf("memory stats (empty): %v", err)
	}
	if empty.Total != 0 || !empty.Oldest.IsZero() || !empty.Newest.IsZero() {
		t.Errorf("empty scope stats should be zero, got %+v", empty)
	}
}

// sameMoment 比较两个时间点是否同一时刻（容忍 SQLite 往返带来的亚秒/时区误差）。
func sameMoment(got, want time.Time) bool {
	if got.IsZero() || want.IsZero() {
		return got.IsZero() && want.IsZero()
	}
	return got.Sub(want).Abs() < time.Second
}
