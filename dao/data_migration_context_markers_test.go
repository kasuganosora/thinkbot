package dao

import (
	"strings"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func newContextMarkersTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=private"), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&TieredMemoryModel{}, &UserMessageEvent{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

// 真实样本取自本地 thinkbot.db（2026-09-29），结构未改写。#2 的被回复正文**含换行**
// （井字棋棋盘），是纯构造样本最容易漏掉的边界。
const (
	realSample1 = "[Timeline] @luna: [Reply to 栞娜: @luna 天色暗下来的时候就是这种感觉！" +
		"要是不方便出门，雨停之后说不定会有鸟类出来活动，正好拍鸟喵～ 出门前记得带伞呀]\n" +
		"那忘记带伞怎么办\n[note_id: aq0ngtc8di7a00m7]"
	realSample2 = "[Timeline] @luna: [Reply to 栞娜: @luna 来喵！我先下，占据中心 ✕： ``` 1 | 2 | 3\n" +
		"───┼───┼─── 4 │ ✕ │ 6\n" +
		"───┼───┼─── 7 | 8 | 9\n" +
		"``` 轮到你了，报个格子编号就行～（你是 ◯）]\n" +
		"4\n[note_id: aq0up57vdi7a00ms]"
)

func TestFixChannelContextMarkers(t *testing.T) {
	db := newContextMarkersTestDB(t)

	// 两条真实污染样本 + 一条干净样本（不应被动）
	mems := []TieredMemoryModel{
		{ID: "note-dirty-1", Tier: 0, ScopeKind: "channel", ScopeID: "misskey:timeline", Content: realSample1},
		{ID: "note-dirty-2", Tier: 0, ScopeKind: "channel", ScopeID: "misskey:timeline", Content: realSample2},
		{ID: "note-clean", Tier: 0, ScopeKind: "channel", ScopeID: "misskey:timeline", Content: "今天天气不错"},
	}
	if err := db.Create(&mems).Error; err != nil {
		t.Fatalf("seed tiered_memories: %v", err)
	}
	evs := []UserMessageEvent{
		{BotID: "bot-1", Channel: "misskey:timeline", UserID: "u1", MessageID: "m1", Content: realSample1},
		{BotID: "bot-1", Channel: "misskey:timeline", UserID: "u1", MessageID: "m2", Content: "干净的原文"},
	}
	if err := db.Create(&evs).Error; err != nil {
		t.Fatalf("seed user_message_events: %v", err)
	}

	detail, err := fixChannelContextMarkers(db)
	if err != nil {
		t.Fatalf("fixChannelContextMarkers: %v", err)
	}
	t.Logf("detail: %s", detail)

	// 真实样本 #1：应只剩用户正文，不再含 bot 的话与任何标记
	var got TieredMemoryModel
	if err := db.First(&got, "id = ?", "note-dirty-1").Error; err != nil {
		t.Fatalf("load: %v", err)
	}
	if want := "那忘记带伞怎么办"; got.Content != want {
		t.Errorf("tiered #1: got %q, want %q", got.Content, want)
	}
	// 真实样本 #2（多行被回复正文）：用户正文是落子 "4"
	// 注意：GORM 的 First 在 dest 主键非空时会把该主键也作为条件，必须换零值变量。
	got = TieredMemoryModel{}
	if err := db.First(&got, "id = ?", "note-dirty-2").Error; err != nil {
		t.Fatalf("load: %v", err)
	}
	if want := "4"; got.Content != want {
		t.Errorf("tiered #2: got %q, want %q", got.Content, want)
	}
	if strings.Contains(got.Content, "Reply to") || strings.Contains(got.Content, "note_id") {
		t.Errorf("tiered #2 still has markers: %q", got.Content)
	}
	// 干净样本不被改动
	got = TieredMemoryModel{}
	if err := db.First(&got, "id = ?", "note-clean").Error; err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.Content != "今天天气不错" {
		t.Errorf("clean sample modified: %q", got.Content)
	}

	// 事件流同样被清洗
	var ev UserMessageEvent
	if err := db.First(&ev, "message_id = ?", "m1").Error; err != nil {
		t.Fatalf("load ev: %v", err)
	}
	if want := "那忘记带伞怎么办"; ev.Content != want {
		t.Errorf("event #1: got %q, want %q", ev.Content, want)
	}

	// 只更新不删除：两表行数不变
	var memCount, evCount int64
	db.Model(&TieredMemoryModel{}).Count(&memCount)
	db.Model(&UserMessageEvent{}).Count(&evCount)
	if memCount != 3 || evCount != 2 {
		t.Errorf("rows changed: tiered=%d event=%d, want 3/2", memCount, evCount)
	}

	// 幂等：再跑一次不应报错，且内容不再变化
	if _, err := fixChannelContextMarkers(db); err != nil {
		t.Fatalf("second run: %v", err)
	}
	got = TieredMemoryModel{}
	if err := db.First(&got, "id = ?", "note-dirty-1").Error; err != nil {
		t.Fatalf("reload: %v", err)
	}
	if want := "那忘记带伞怎么办"; got.Content != want {
		t.Errorf("not idempotent: got %q, want %q", got.Content, want)
	}
}

// TestFixChannelContextMarkers_NoContentLoss 护栏：任何情况下清洗都不会把内容清空。
// 剥离后为空的行必须跳过（保留原样），绝不能写空串进库。
func TestFixChannelContextMarkers_NoContentLoss(t *testing.T) {
	db := newContextMarkersTestDB(t)
	const markerOnly = "[Reply to 栞娜: bot 的话]"
	if err := db.Create(&TieredMemoryModel{
		ID: "note-marker-only", Tier: 0, ScopeKind: "channel", ScopeID: "s", Content: markerOnly,
	}).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := fixChannelContextMarkers(db); err != nil {
		t.Fatalf("run: %v", err)
	}
	var got TieredMemoryModel
	if err := db.First(&got, "id = ?", "note-marker-only").Error; err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.Content == "" {
		t.Fatal("content wiped to empty — data loss")
	}
	if got.Content != markerOnly {
		t.Errorf("expected untouched, got %q", got.Content)
	}
}
