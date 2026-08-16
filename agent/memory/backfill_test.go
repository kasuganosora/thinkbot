package memory

import (
	"context"
	"sort"
	"testing"
	"time"

	"go.uber.org/zap"
)

// memUserMessageSource 内存版 UserMessageSource，供 backfill 单测（不依赖 gorm/sqlite）。
// 按 botID + id>sinceID 过滤，镜像生产 dbUserMessageSource 的查询语义。
type memUserMessageSource struct {
	msgs []struct {
		botID string
		msg   BackfillMessage
	}
}

func (s *memUserMessageSource) LoadSince(_ context.Context, botID string, sinceID uint64) ([]BackfillMessage, error) {
	var out []BackfillMessage
	for _, m := range s.msgs {
		if m.botID == botID && m.msg.ID > sinceID {
			out = append(out, m.msg)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func TestBackfillFromChatHistory(t *testing.T) {
	now := time.Now()
	// 事件流（user_message_events）只含「用户」消息；bot 回复不在此流中。
	src := &memUserMessageSource{msgs: []struct {
		botID string
		msg   BackfillMessage
	}{
		{botID: "bot-x", msg: BackfillMessage{ID: 1, Channel: "s1", UserID: "u1", MessageID: "m1", Content: "我喜欢用 Go 语言", CreatedAt: now.Add(-48 * time.Hour)}},
		{botID: "other", msg: BackfillMessage{ID: 2, Channel: "", UserID: "z", MessageID: "m2", Content: "噪声不应被灌入", CreatedAt: now}},
		{botID: "bot-x", msg: BackfillMessage{ID: 3, Channel: "", UserID: "u2", MessageID: "m3", Content: "用 Docker 部署应用", CreatedAt: now.Add(-10 * time.Hour)}},
	}}

	// 纯内存 store（不持久化），专注验证 L0 填充与 bot 过滤
	ts := NewTieredStore(nil)
	store := NewTieredStoreAdapter(ts)
	written, maxID, err := BackfillFromChatHistory(context.Background(), store, src, "bot-x", 0, zap.NewNop().Sugar())
	if err != nil {
		t.Fatal(err)
	}
	if written != 2 {
		t.Fatalf("expected 2 written (bot-x user messages only, empty skipped), got %d", written)
	}
	if maxID != 3 {
		t.Fatalf("expected maxID=3, got %d", maxID)
	}

	l0, err := ts.Retrieve(context.Background(), Tier0Working, nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(l0) != 2 {
		t.Fatalf("expected 2 L0 entries, got %d", len(l0))
	}
	for _, e := range l0 {
		if e.Source != "chat_history" {
			t.Fatalf("expected source chat_history, got %s", e.Source)
		}
		// 必须以当前时间写入，否则会被 dreaming 的 LookbackDays/ActiveThreshold 跳过
		if e.CreatedAt.Before(now.Add(-time.Minute)) {
			t.Fatalf("backfilled entry must use recent CreatedAt, got %v", e.CreatedAt)
		}
		if e.Metadata["event_id"] == nil {
			t.Fatalf("backfilled entry missing event_id metadata")
		}
	}

	// 空消息表的 bot 不应写入任何内容
	store2 := NewTieredStoreAdapter(NewTieredStore(nil))
	if n, _, err := BackfillFromChatHistory(context.Background(), store2, src, "nobody", 0, zap.NewNop().Sugar()); err != nil || n != 0 {
		t.Fatalf("expected 0 written for unknown bot, got n=%d err=%v", n, err)
	}

	// 增量幂等：以 maxID 为水位线再次回灌，不应重复写入任何内容
	store3 := NewTieredStoreAdapter(NewTieredStore(nil))
	if n, wm, err := BackfillFromChatHistory(context.Background(), store3, src, "bot-x", maxID, zap.NewNop().Sugar()); err != nil || n != 0 {
		t.Fatalf("incremental backfill above watermark should write 0, got n=%d wm=%d err=%v", n, wm, err)
	} else if wm != maxID {
		t.Fatalf("incremental backfill should preserve watermark, got %d want %d", wm, maxID)
	}
}
