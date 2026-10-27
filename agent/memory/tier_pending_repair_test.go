package memory

import (
	"context"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/kasuganosora/thinkbot/dao"
	"github.com/kasuganosora/thinkbot/util/idgen"
)

func TestHasUserSource(t *testing.T) {
	// 内存态：刚构造的 []DreamSourceEntry
	if !HasUserSource([]DreamSourceEntry{
		{Speaker: SpeakerAssistant}, {Speaker: SpeakerUser},
	}) {
		t.Fatal("assistant+user should count as user-sourced")
	}
	if HasUserSource([]DreamSourceEntry{{Speaker: SpeakerAssistant}, {Speaker: SpeakerObserver}}) {
		t.Fatal("assistant+observer must not count as user-sourced")
	}
	if HasUserSource([]DreamSourceEntry{}) {
		t.Fatal("empty snapshot must not count as user-sourced")
	}

	// 落库态：从 SQLite JSON 读回的 []any
	decoded := []any{
		map[string]any{"speaker": "assistant", "content": "x"},
		map[string]any{"speaker": "user", "content": "y"},
	}
	if !HasUserSource(decoded) {
		t.Fatal("decoded []any with a user entry should count as user-sourced")
	}
	if HasUserSource([]any{map[string]any{"content": "no speaker"}}) {
		t.Fatal("decoded []any without speaker must not count as user-sourced")
	}
	if HasUserSource(nil) || HasUserSource("junk") {
		t.Fatal("nil / unexpected type must not count as user-sourced")
	}
}

// TestDreamPromotionDoesNotParkUserStatedFacts 覆盖生产者侧（dreaming 深睡相位）：
// 用户原话支撑的蒸馏事实晋升后必须是 active；只有 bot 猜测支撑的才标 pending。
func TestDreamPromotionDoesNotParkUserStatedFacts(t *testing.T) {
	ctx := context.Background()

	run := func(t *testing.T, scopeID, speaker string) (promoted int, statuses []string) {
		t.Helper()
		scope := ChannelScope(scopeID)
		dm, tm := newTestDreamManager(t, []Scope{scope})
		for _, c := range []string{
			"用户使用 Go 语言进行后端开发，偏好简洁且可测试的代码风格",
			"用户偏好 Go 标准库，尽量少引入第三方依赖",
		} {
			if err := tm.store.Append(ctx, TieredEntry{
				Entry: Entry{
					ID:        idgen.New("l0"),
					Scope:     scope,
					Content:   c,
					Category:  "preference",
					Source:    "note_capture",
					Metadata:  map[string]any{"speaker": speaker},
					CreatedAt: time.Now(),
				},
				Tier: Tier0Working,
			}); err != nil {
				t.Fatal(err)
			}
		}
		report, err := dm.Run(ctx)
		if err != nil {
			t.Fatal(err)
		}
		l1, err := tm.store.GetAll(ctx, Tier1LongTerm, scope)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range l1 {
			statuses = append(statuses, StatusOf(e.Entry))
		}
		return report.DeepPromoted, statuses
	}

	promoted, statuses := run(t, "dream-src-user", SpeakerUser)
	if promoted == 0 {
		t.Fatal("expected user-stated facts to be promoted; test would be vacuous otherwise")
	}
	for _, s := range statuses {
		if s != StatusActive {
			t.Fatalf("user-stated promotion status = %q, want %q (pending ones are invisible to recall)", s, StatusActive)
		}
	}

	// observer（公开时间线观察，非本会话用户所说）不是用户陈述，推断结果应继续
	// 标 pending 等待确认。注意不能用 assistant：规则提取阶段本就整体排除
	// bot 自身回复（dreaming_phases.go extractCandidatesRuleBased），走不到晋升。
	promoted2, statuses2 := run(t, "dream-src-observer", SpeakerObserver)
	if promoted2 == 0 {
		t.Fatal("expected observer-sourced facts to be promoted; test would be vacuous otherwise")
	}
	for _, s := range statuses2 {
		if s != StatusPending {
			t.Fatalf("non-user-sourced promotion status = %q, want %q (pure guess must stay pending)", s, StatusPending)
		}
	}
}

// dreaming 无条件 MarkInferred 后，全部 L1 蒸馏事实被标成 pending，
// RecallVisible 一律拒绝，真实召回里 L1 返回 0 条。
// 本测试断言启动改判只放行「来源含用户原话」的那部分。
func TestStartPendingRepairFlipsUserStatedFacts(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&dao.TieredMemoryModel{}); err != nil {
		t.Fatal(err)
	}

	scope := ChannelScope("wecom:dm:T1")
	now := time.Now()
	mk := func(id, content, meta string) dao.TieredMemoryModel {
		return dao.TieredMemoryModel{
			ID: id, Tier: int(Tier1LongTerm),
			ScopeKind: string(scope.Kind), ScopeID: scope.ID,
			Content: content, Category: "fact", Source: "dreaming",
			Importance: 0.6, MetadataJSON: meta, CreatedAt: now,
		}
	}
	rows := []dao.TieredMemoryModel{
		// 用户原话支撑的 pending：应被改判为 active
		mk("e-user", "正式环境日志主题为 yak_log_json",
			`{"status":"pending","inferred":true,"source_entries":[{"speaker":"user","content":"正式环境日志主题是 yak_log_json"}]}`),
		// 只有 bot 回复支撑的 pending：保持 pending（确实是系统猜测）
		mk("e-assistant", "bot 自己猜的事实",
			`{"status":"pending","inferred":true,"source_entries":[{"speaker":"assistant","content":"猜的"}]}`),
		// 没有来源快照的 pending：无从判断，保持 pending
		mk("e-nosrc", "无来源快照的事实", `{"status":"pending","inferred":true}`),
		// 已经是 active 的：不受影响
		mk("e-active", "已确认事实", `{"status":"active","source_entries":[{"speaker":"user"}]}`),
	}
	if err := db.Create(&rows).Error; err != nil {
		t.Fatal(err)
	}

	store := NewTieredStoreWithDB(nil, db)
	if store.pendingRepairDone == nil {
		t.Fatal("pending repair did not start")
	}
	select {
	case <-store.pendingRepairDone:
	case <-time.After(5 * time.Second):
		t.Fatal("pending repair did not finish")
	}

	// 内存态
	got, err := store.GetAll(context.Background(), Tier1LongTerm, scope)
	if err != nil {
		t.Fatal(err)
	}
	status := map[string]string{}
	for _, e := range got {
		status[e.ID] = StatusOf(e.Entry)
	}
	want := map[string]string{
		"e-user":      StatusActive,
		"e-assistant": StatusPending,
		"e-nosrc":     StatusPending,
		"e-active":    StatusActive,
	}
	for id, w := range want {
		if status[id] != w {
			t.Fatalf("%s status = %q, want %q", id, status[id], w)
		}
	}

	// 召回可见性：只有 e-user / e-active 能进入上下文
	visible := map[string]bool{}
	for _, e := range got {
		visible[e.ID] = RecallVisible(e.Entry)
	}
	if !visible["e-user"] || !visible["e-active"] {
		t.Fatal("user-stated and already-active entries must stay recallable")
	}
	if visible["e-assistant"] || visible["e-nosrc"] {
		t.Fatal("pure-guess entries must not be recallable")
	}

	// 落库态：改判必须持久化，不能只改内存
	for id, w := range want {
		var m dao.TieredMemoryModel
		if err := db.Where("id = ?", id).First(&m).Error; err != nil {
			t.Fatal(err)
		}
		if got := StatusOf(modelToTiedEntry(m).Entry); got != w {
			t.Fatalf("persisted %s status = %q, want %q", id, got, w)
		}
	}

	// 幂等：再跑一次不应报错也不应改变结果
	store.repairUserStatedPending(context.Background())
	got2, _ := store.GetAll(context.Background(), Tier1LongTerm, scope)
	for _, e := range got2 {
		if StatusOf(e.Entry) != want[e.ID] {
			t.Fatalf("after rerun %s status = %q, want %q", e.ID, StatusOf(e.Entry), want[e.ID])
		}
	}
}
