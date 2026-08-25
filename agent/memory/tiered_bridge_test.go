package memory

import (
	"context"
	"sync"
	"testing"
)

// fakeStore 记录 Append 调用次数，用于验证 MultiStore 去重行为。
type fakeStore struct {
	mu     sync.Mutex
	appends int
}

func (f *fakeStore) Append(ctx context.Context, entry Entry) error {
	f.mu.Lock()
	f.appends++
	f.mu.Unlock()
	return nil
}
func (f *fakeStore) Delete(ctx context.Context, scope Scope, entryID string) error { return nil }
func (f *fakeStore) Clear(ctx context.Context, scope Scope) error                  { return nil }

// TestMultiStore_DedupIdenticalContent 验证同一 (scope, content) 在窗口内的重复
// Append 被去重（只落一次），不同 content 或不同 scope 不受影响。
func TestMultiStore_DedupIdenticalContent(t *testing.T) {
	a := &fakeStore{}
	b := &fakeStore{}
	ms := NewMultiStore(a, b)
	scope := BotScope("bot-x")

	// 完全相同的两条 → 一次落库（两个后端各 1 次）。
	if err := ms.Append(context.Background(), Entry{Scope: scope, Content: "same note"}); err != nil {
		t.Fatal(err)
	}
	if err := ms.Append(context.Background(), Entry{Scope: scope, Content: "same note"}); err != nil {
		t.Fatal(err)
	}
	// 不同 content → 应再落一次。
	if err := ms.Append(context.Background(), Entry{Scope: scope, Content: "different note"}); err != nil {
		t.Fatal(err)
	}
	// 不同 scope 的相同 content → 应再落一次（去重键含 scope）。
	if err := ms.Append(context.Background(), Entry{Scope: ChannelScope("other"), Content: "same note"}); err != nil {
		t.Fatal(err)
	}

	if a.appends != 3 || b.appends != 3 {
		t.Fatalf("expected each backend appended 3 times, got a=%d b=%d", a.appends, b.appends)
	}
}

// TestTier2Episodic_IsOptInExtensionPoint 守卫「L2 场景层」作为可选扩展点的设计决策
// （2026-08-25 收敛）：L2 在默认配置中保留定义（不可误删），但生产无 Aggregator
// 实现 → 恒为空；有效管线为 L0→L1→L3。此测试防止未来有人把 L2 当死代码移除。
func TestTier2Episodic_IsOptInExtensionPoint(t *testing.T) {
	cfgs := DefaultTierConfigs()
	l2, ok := cfgs[Tier2Episodic]
	if !ok {
		t.Fatal("Tier2Episodic must remain defined as an opt-in extension layer")
	}
	if l2.MaxEntries <= 0 {
		t.Fatal("L2 config should specify MaxEntries to bound its storage when enabled")
	}
}
