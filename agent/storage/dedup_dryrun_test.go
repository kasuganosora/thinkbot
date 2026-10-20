package storage

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kasuganosora/thinkbot/agent/memory"
	"github.com/kasuganosora/thinkbot/dao"
)

func TestDryRunDedupGroups_NoWrites(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	repo := NewSQLiteRepository(db)
	now := time.Now()
	for _, m := range []dao.EntryModel{
		{ID: "big", ScopeKind: "channel", ScopeID: "dr", Content: strings.Repeat("很长的记忆内容", 120), Source: "note", CreatedAt: now},
		{ID: "a", ScopeKind: "channel", ScopeID: "dr", Content: "用户使用Go语言开发后端服务", Source: "note", CreatedAt: now},
		{ID: "b", ScopeKind: "channel", ScopeID: "dr", Content: "用户使用Go语言开发后端", Source: "note", CreatedAt: now.Add(time.Second)},
		{ID: "cmp", ScopeKind: "channel", ScopeID: "dr", Content: "旧的合并结果", Source: "compactor", CreatedAt: now},
	} {
		if err := db.Create(&m).Error; err != nil {
			t.Fatal(err)
		}
	}
	for _, all := range []bool{false, true} {
		res, err := repo.DryRunDedupGroups(ctx, DedupDryRunOptions{AllSeeds: all})
		if err != nil {
			t.Fatal(err)
		}
		if len(res) != 1 || res[0].Scope.Key() != "channel:dr" || res[0].Seeds != 2 || len(res[0].Oversized) != 1 {
			t.Fatalf("all=%v res=%+v", all, res)
		}
		if len(res[0].Groups) != 1 || res[0].Groups[0][0].Entry.ID != "a" || res[0].Groups[0][1].Similarity < DefaultDedupMinSimilarity && res[0].Mode == "vec" {
			t.Fatalf("all=%v groups=%+v", all, res[0].Groups)
		}
	}
	if n, _ := repo.CountUncheckedDedup(ctx, memory.ChannelScope("dr")); n != 3 {
		t.Fatalf("dry run must not mark entries checked, unchecked=%d", n)
	}
}
