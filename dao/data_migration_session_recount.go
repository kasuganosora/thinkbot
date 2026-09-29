package dao

import (
	"fmt"

	"gorm.io/gorm"
)

// sessionRecountFixName 是本次迁移的登记名（见 data_migration.go）。
const sessionRecountFixName = "2026-09-29-session-recount"

// recountSessionStats 按 chat_messages 的真实行数回填 chat_sessions 的统计列。
//
// 背景（B34）：`message_count` / `last_msg_at` 只由 `touchSessionAfterSave` 在每次
// 落库时 +1 / 刷新，**从不与真实行数对账**。任何绕过该函数的写入或删除都会让它漂移
// —— 实测会话 10 记 53 条而实际 61 条，会话 7 记 0 条。Web 列表的排序与「多少条」
// 都读这两列，漂移直接表现成排序错乱。
//
// ⚠️ 只 UPDATE、绝不 DELETE：本迁移不判断「哪条消息该留」，那是 trim / 保留策略的事。
//
// ⚠️ 一次性而非每次启动：全表对账在有大量会话时是 O(会话数 × 消息数) 的相关子查询，
// 放到启动路径上会拖慢启动。漂移速度很慢（只在删除/迁移时发生），跑一次足够；
// 后续若需要，应由 S4 的启动告警发现而不是无条件重算。
func recountSessionStats(db *gorm.DB) (string, error) {
	type row struct {
		ID           uint64
		MessageCount int
	}
	var before []row
	if err := db.Model(&ChatSession{}).Select("id", "message_count").Find(&before).Error; err != nil {
		return "", fmt.Errorf("recount: load sessions: %w", err)
	}

	// 相关子查询按会话 id 精确对账。session_id 是 TEXT 而会话 id 是整数，
	// 必须 CAST —— 否则 1 ≠ '1'，所有会话都会被算成 0 条。
	//
	// last_msg_at 用 COALESCE 保留原值：没有消息的会话不应被写成 NULL
	// （NULL 在 ORDER BY last_msg_at DESC 里排最后，会把空会话沉底、掩盖问题）。
	if err := db.Exec(`
		UPDATE chat_sessions SET
			message_count = (
				SELECT COUNT(*) FROM chat_messages
				WHERE chat_messages.session_id = CAST(chat_sessions.id AS TEXT)
			),
			last_msg_at = COALESCE((
				SELECT MAX(chat_messages.created_at) FROM chat_messages
				WHERE chat_messages.session_id = CAST(chat_sessions.id AS TEXT)
			), chat_sessions.last_msg_at)
	`).Error; err != nil {
		return "", fmt.Errorf("recount: update: %w", err)
	}

	var after []row
	if err := db.Model(&ChatSession{}).Select("id", "message_count").Find(&after).Error; err != nil {
		return "", fmt.Errorf("recount: reload sessions: %w", err)
	}

	oldByID := make(map[uint64]int, len(before))
	for _, r := range before {
		oldByID[r.ID] = r.MessageCount
	}
	corrected := 0
	examples := make([]string, 0, 3)
	for _, r := range after {
		if old, ok := oldByID[r.ID]; ok && old != r.MessageCount {
			corrected++
			if len(examples) < 3 {
				examples = append(examples, fmt.Sprintf("%d:%d->%d", r.ID, old, r.MessageCount))
			}
		}
	}

	return fmt.Sprintf("sessions=%d corrected=%d %v", len(after), corrected, examples), nil
}
