package memory

import (
	"context"
	"testing"
)

// TestWriteProfile_DedupesBotPersonality 验证：连续多次写入同一 scope 的
// bot_personality 画像时，WriteProfile 会在写入前去重，最终只保留最新一条；
// 其他 category 的画像不受影响。
func TestWriteProfile_DedupesBotPersonality(t *testing.T) {
	store := NewTieredStore(nil)
	mgr := NewTieredManager(TieredManagerConfig{Store: store}, testTracerProvider(), testLogger())
	ctx := context.Background()
	scope := BotScope("bot-dedupe-test")

	// 模拟多次固化各写入一份 bot_personality（每天 03:00 一次）
	for i := 0; i < 3; i++ {
		if err := mgr.WriteProfile(ctx, Entry{
			Scope: scope, Category: "bot_personality", Source: "bot_profiler", Content: "personality",
		}); err != nil {
			t.Fatal(err)
		}
	}

	// 另一个 category 不应被误删
	if err := mgr.WriteProfile(ctx, Entry{
		Scope: scope, Category: "fact", Source: "profiler", Content: "fact",
	}); err != nil {
		t.Fatal(err)
	}

	got, err := store.Retrieve(ctx, Tier3Profile, []Scope{scope}, 100)
	if err != nil {
		t.Fatal(err)
	}
	var botP, fact int
	for _, e := range got {
		switch e.Category {
		case "bot_personality":
			botP++
		case "fact":
			fact++
		}
	}
	if botP != 1 {
		t.Fatalf("expected exactly 1 bot_personality after dedupe, got %d", botP)
	}
	if fact != 1 {
		t.Fatalf("expected 1 fact profile preserved, got %d", fact)
	}
}
