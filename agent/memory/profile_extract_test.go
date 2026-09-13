package memory

import (
	"context"
	"testing"
)

// stubUserProfiler 测试用 Profiler：返回预设条目并记录调用次数。
type stubUserProfiler struct {
	items []ProfileItem
	calls int
}

func (s *stubUserProfiler) ExtractProfile(_ context.Context, _, _, _ []TieredEntry) ([]ProfileItem, error) {
	s.calls++
	out := make([]ProfileItem, len(s.items))
	copy(out, s.items)
	return out, nil
}

func seedUserL1(t *testing.T, mgr *TieredManager, scope Scope) {
	t.Helper()
	if err := mgr.WriteLongTerm(context.Background(), Entry{
		Scope:    scope,
		Content:  "用户偏好简洁回复，常用 Go",
		Category: "preference",
		Source:   "test",
	}, Tier0Working); err != nil {
		t.Fatal(err)
	}
}

func TestExtractProfile_ReplacesProfilerL3NotStack(t *testing.T) {
	store := NewTieredStore(nil)
	stub := &stubUserProfiler{items: []ProfileItem{
		{Type: ProfileTypeTrait, Content: "理性", Confidence: 0.8},
		{Type: ProfileTypePreference, Content: "简洁", Confidence: 0.7},
	}}
	mgr := NewTieredManager(TieredManagerConfig{Store: store, Profiler: stub},
		testTracerProvider(), testLogger())
	ctx := context.Background()
	scope := UserScope("u-replace")
	seedUserL1(t, mgr, scope)

	if err := mgr.WriteProfile(ctx, Entry{
		Scope: scope, Category: "manual", Source: "admin", Content: "人工备注",
	}); err != nil {
		t.Fatal(err)
	}

	if n, err := mgr.ExtractProfile(ctx, scope); err != nil || n != 2 {
		t.Fatalf("first extract: n=%d err=%v", n, err)
	}
	stub.items = []ProfileItem{
		{Type: ProfileTypeTrait, Content: "更新后的特质", Confidence: 0.9},
	}
	if n, err := mgr.ExtractProfile(ctx, scope); err != nil || n != 1 {
		t.Fatalf("second extract: n=%d err=%v", n, err)
	}

	got, err := store.Retrieve(ctx, Tier3Profile, []Scope{scope}, 100)
	if err != nil {
		t.Fatal(err)
	}
	var profiler, other int
	var latest string
	for _, e := range got {
		if e.Source == "profiler" {
			profiler++
			latest = e.Content
		} else {
			other++
		}
	}
	if profiler != 1 {
		t.Fatalf("expected 1 source=profiler L3 after replace, got %d (total %d)", profiler, len(got))
	}
	if latest != "更新后的特质" {
		t.Fatalf("expected replaced content, got %q", latest)
	}
	if other != 1 {
		t.Fatalf("expected non-profiler L3 preserved, got %d", other)
	}
	if stub.calls != 2 {
		t.Fatalf("expected 2 profiler calls, got %d", stub.calls)
	}
}

func TestExtractProfile_SkipsLowConfidenceAndEmpty(t *testing.T) {
	store := NewTieredStore(nil)
	stub := &stubUserProfiler{items: []ProfileItem{
		{Type: ProfileTypeTrait, Content: "单次观察", Confidence: 0.3},
		{Type: ProfileTypeFact, Content: "   ", Confidence: 0.9},
		{Type: ProfileTypePreference, Content: "有效偏好", Confidence: 0.5},
	}}
	mgr := NewTieredManager(TieredManagerConfig{Store: store, Profiler: stub},
		testTracerProvider(), testLogger())
	ctx := context.Background()
	scope := UserScope("u-skip")
	seedUserL1(t, mgr, scope)

	n, err := mgr.ExtractProfile(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("expected 1 kept item, got %d", n)
	}
	got, err := store.Retrieve(ctx, Tier3Profile, []Scope{scope}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Content != "有效偏好" {
		t.Fatalf("expected only the high-confidence non-empty item, got %+v", got)
	}
}

func TestExtractProfile_AllLowConfidenceLeavesExisting(t *testing.T) {
	store := NewTieredStore(nil)
	stub := &stubUserProfiler{items: []ProfileItem{
		{Type: ProfileTypeTrait, Content: "噪声", Confidence: 0.1},
	}}
	mgr := NewTieredManager(TieredManagerConfig{Store: store, Profiler: stub},
		testTracerProvider(), testLogger())
	ctx := context.Background()
	scope := UserScope("u-keep")
	seedUserL1(t, mgr, scope)
	if err := mgr.WriteProfile(ctx, Entry{
		Scope: scope, Category: ProfileTypeTrait, Source: "profiler", Content: "已有画像",
	}); err != nil {
		t.Fatal(err)
	}

	n, err := mgr.ExtractProfile(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("expected 0 written, got %d", n)
	}
	got, err := store.Retrieve(ctx, Tier3Profile, []Scope{scope}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Content != "已有画像" {
		t.Fatalf("low-confidence extract must not wipe existing L3, got %+v", got)
	}
}
