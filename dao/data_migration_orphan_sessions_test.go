package dao

import (
	"strconv"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func newOrphanSessionTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=private"), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&ChatSession{}, &ChatMessage{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// 补上 S0 的 partial 唯一索引（AutoMigrate 不认 WHERE 子句，真实库由 ensureIndexes 建）。
	// ResolveSession 的「并发撞索引后重查」回退依赖它。
	if err := db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_chat_sessions_external_key
		ON chat_sessions(bot_id, external_key) WHERE external_key <> ''`).Error; err != nil {
		t.Fatalf("create unique index: %v", err)
	}
	return db
}

// TestBackfillOrphanSessions 覆盖四类孤儿：TG 私聊 / TG 群组（负 chatID）/ 空 session_id /
// web 测试残留。断言建行、改 session_id、回填计数，以及**已归属会话不受影响**。
func TestBackfillOrphanSessions(t *testing.T) {
	db := newOrphanSessionTestDB(t)
	const bot = "bot-2d8f9b087270da0bcfe177a5"
	base := time.Date(2026, 9, 7, 13, 0, 0, 0, time.UTC)

	// 已归属会话：迁移不得动它（曾经 handleListSessions 的兜底只在无会话时触发，
	// 有会话就永远收不进去，本迁移正是为了修这个）。
	owned := ChatSession{BotID: bot, Title: "选项测试", Status: SessionStatusActive, MessageCount: 2,
		CreatedAt: base, UpdatedAt: base}
	if err := db.Create(&owned).Error; err != nil {
		t.Fatalf("seed session: %v", err)
	}
	ownedID := strconv.FormatUint(owned.ID, 10)

	msgs := []ChatMessage{
		// 已归属
		{BotID: bot, UserID: "1", SessionID: ownedID, Role: ChatRoleUser, Content: "老会话", CreatedAt: base},
		{BotID: bot, UserID: "1", SessionID: ownedID, Role: ChatRoleAssistant, Content: "收到", CreatedAt: base},
		// 孤儿 1：TG 私聊，首条带渠道装饰前缀（验证标题剥壳）
		{BotID: bot, UserID: "tg:76017910", SessionID: "tg:76017910", Role: ChatRoleUser,
			Content: "[Timeline] @luna: [Reply to 栞娜: bot 的话]\n真实用户第一句", CreatedAt: base},
		{BotID: bot, UserID: "tg:76017910", SessionID: "tg:76017910", Role: ChatRoleAssistant,
			Content: "好的", CreatedAt: base.Add(time.Minute)},
		{BotID: bot, UserID: "tg:76017910", SessionID: "tg:76017910", Role: ChatRoleUser,
			Content: "第二句", CreatedAt: base.Add(2 * time.Minute)},
		// 孤儿 2：TG 群组（负 chatID）
		{BotID: bot, UserID: "tg:-914633707", SessionID: "tg:-914633707", Role: ChatRoleUser,
			Content: "群里的第一句", CreatedAt: base.Add(3 * time.Minute)},
		{BotID: bot, UserID: "tg:-914633707", SessionID: "tg:-914633707", Role: ChatRoleAssistant,
			Content: "在", CreatedAt: base.Add(4 * time.Minute)},
		// 孤儿 3：空 session_id（历史遗留）
		{BotID: bot, UserID: "1", SessionID: "", Role: ChatRoleUser, Content: "孤儿一", CreatedAt: base.Add(5 * time.Minute)},
		{BotID: bot, UserID: "1", SessionID: "", Role: ChatRoleAssistant, Content: "嗯", CreatedAt: base.Add(6 * time.Minute)},
		// 孤儿 4：web 测试残留
		{BotID: bot, UserID: "1", SessionID: "verify-compact-1", Role: ChatRoleUser, Content: "压缩测试", CreatedAt: base.Add(7 * time.Minute)},
	}
	if err := db.Create(&msgs).Error; err != nil {
		t.Fatalf("seed messages: %v", err)
	}

	detail, err := backfillOrphanSessions(db)
	if err != nil {
		t.Fatalf("backfillOrphanSessions: %v", err)
	}
	t.Logf("detail: %s", detail)

	// 1) 会话行：已有 1 + 新建 4
	var sessCount int64
	db.Model(&ChatSession{}).Count(&sessCount)
	if sessCount != 5 {
		t.Errorf("session rows = %d, want 5", sessCount)
	}

	// 2) 每条消息的 session_id 都变成数字且指向存在的会话行
	var left []ChatMessage
	if err := db.Find(&left).Error; err != nil {
		t.Fatalf("reload messages: %v", err)
	}
	ids := map[uint64]bool{}
	var all []ChatSession
	db.Find(&all)
	for _, s := range all {
		ids[s.ID] = true
	}
	for _, m := range left {
		id, err := strconv.ParseUint(m.SessionID, 10, 64)
		if err != nil {
			t.Fatalf("message %d session_id %q is not numeric after migration", m.ID, m.SessionID)
		}
		if !ids[id] {
			t.Errorf("message %d points to missing session %d", m.ID, id)
		}
	}

	// 3) 已归属会话及其消息未被牵动
	var afterOwned ChatSession
	if err := db.First(&afterOwned, owned.ID).Error; err != nil {
		t.Fatalf("reload owned session: %v", err)
	}
	if afterOwned.MessageCount != 2 || afterOwned.Title != "选项测试" {
		t.Errorf("owned session mutated: count=%d title=%q", afterOwned.MessageCount, afterOwned.Title)
	}

	// 4) TG 私聊：external_key / kind / 标题剥壳
	var dm ChatSession
	if err := db.First(&dm, "external_key = ?", TelegramChatKey("76017910")).Error; err != nil {
		t.Fatalf("find tg dm session: %v", err)
	}
	if dm.SessionKind != KindDirect {
		t.Errorf("tg dm kind = %q, want %q", dm.SessionKind, KindDirect)
	}
	if want := "真实用户第一句"; dm.Title != want {
		t.Errorf("tg dm title = %q, want %q", dm.Title, want)
	}
	if dm.MessageCount != 3 {
		t.Errorf("tg dm message_count = %d, want 3", dm.MessageCount)
	}
	if dm.LastMsgAt == nil || !dm.LastMsgAt.Equal(base.Add(2*time.Minute)) {
		t.Errorf("tg dm last_msg_at = %v, want %v", dm.LastMsgAt, base.Add(2*time.Minute))
	}

	// 5) TG 群组：负 chatID 判 group，且 key 三段解析不丢负号
	var grp ChatSession
	if err := db.First(&grp, "external_key = ?", TelegramChatKey("-914633707")).Error; err != nil {
		t.Fatalf("find tg group session: %v", err)
	}
	if grp.SessionKind != KindGroup {
		t.Errorf("tg group kind = %q, want %q", grp.SessionKind, KindGroup)
	}
	if _, _, id, ok := ParseExternalKey(grp.ExternalKey); !ok || id != "-914633707" {
		t.Errorf("group external_key parse = (%q, %v)", id, ok)
	}
	if grp.MessageCount != 2 {
		t.Errorf("tg group message_count = %d, want 2", grp.MessageCount)
	}

	// 6) 空 session_id 归到「默认会话」，且不占 external_key
	var dflt ChatSession
	if err := db.First(&dflt, "title = ? AND external_key = ''", "默认会话").Error; err != nil {
		t.Fatalf("find default session: %v", err)
	}
	if dflt.MessageCount != 2 {
		t.Errorf("default session message_count = %d, want 2", dflt.MessageCount)
	}

	// 7) web 测试残留：可读标题 + 不占 external_key
	var web ChatSession
	if err := db.First(&web, "title = ? AND external_key = ''", "压缩测试").Error; err != nil {
		t.Fatalf("find web session: %v", err)
	}
	if web.MessageCount != 1 {
		t.Errorf("web session message_count = %d, want 1", web.MessageCount)
	}

	// 8) 幂等：再跑一次不新增会话、不改 session_id
	if _, err := backfillOrphanSessions(db); err != nil {
		t.Fatalf("second run: %v", err)
	}
	var again int64
	db.Model(&ChatSession{}).Count(&again)
	if again != 5 {
		t.Errorf("not idempotent: session rows = %d after second run, want 5", again)
	}
}

// TestBackfillOrphanSessions_NoDataLoss 护栏：迁移只改 session_id，绝不删消息。
func TestBackfillOrphanSessions_NoDataLoss(t *testing.T) {
	db := newOrphanSessionTestDB(t)
	const bot = "bot-x"
	now := time.Now()
	msgs := []ChatMessage{
		{BotID: bot, UserID: "tg:1", SessionID: "tg:1", Role: ChatRoleUser, Content: "a", CreatedAt: now},
		{BotID: bot, UserID: "tg:1", SessionID: "tg:1", Role: ChatRoleAssistant, Content: "b", CreatedAt: now},
		{BotID: bot, UserID: "tg:1", SessionID: "tg:1", Role: ChatRoleUser, Content: "c", CreatedAt: now},
	}
	if err := db.Create(&msgs).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := backfillOrphanSessions(db); err != nil {
		t.Fatalf("run: %v", err)
	}
	var n int64
	db.Model(&ChatMessage{}).Where("bot_id = ?", bot).Count(&n)
	if n != 3 {
		t.Fatalf("messages lost: got %d, want 3", n)
	}
	// 正文与角色保持原样
	var got []ChatMessage
	db.Where("bot_id = ?", bot).Order("id ASC").Find(&got)
	want := []string{"a", "b", "c"}
	for i, m := range got {
		if m.Content != want[i] {
			t.Errorf("message %d content = %q, want %q", i, m.Content, want[i])
		}
	}
}
