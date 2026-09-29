package dao

import "time"

// SessionThreadIndex 渠道 reply 链的反向索引。
//
// 存在的理由：Misskey 的 `note.Reply` 只嵌一层，两级以上的回复（A→B→C）在本地算不出
// root，必须靠这条索引按「根帖」枚举整棵子树：
//   - reclaim（fork 时把已落 timeline 的分支迁进 thread）需要 `WHERE root_note_id = ?`；
//   - 入站归属判定需要 `WHERE note_id = ?` 反查 root。
//
// ⚠️ 两个维度必须分开存，不能合并：
//   - `root_note_id` 表达**链关系**（这条 note 属于哪条 reply 链），入站即确定；
//   - `session_id`   表达**会话归属**（这条链是否已被 fork 成 session），fork 之后才有值。
//
// `session_id` 必须可空：Bot 尚未参与的链（案例 2 的 C1/E1）入站时链已存在但没有会话，
// 此时若不写索引，`root_note_id` 就丢了，后续 fork 无从枚举子树做 reclaim（缺陷 B11）。
type SessionThreadIndex struct {
	// BotID Bot 标识（复合主键第 1 列）。
	BotID string `gorm:"primaryKey;size:64;index:idx_sti_root,priority:1" json:"botId"`

	// Channel 渠道标识（复合主键第 2 列），如 "misskey" / "telegram"。
	Channel string `gorm:"primaryKey;size:32;index:idx_sti_root,priority:2" json:"channel"`

	// NoteID 渠道内的消息/帖子 ID（复合主键第 3 列）。
	NoteID string `gorm:"primaryKey;size:128" json:"noteId"`

	// RootNoteID 所属 reply 链的根帖 ID。原创帖填自身；未解析出时为 NULL。
	RootNoteID string `gorm:"size:128;index:idx_sti_root,priority:3" json:"rootNoteId"`

	// SessionID 归属的 thread 会话（chat_sessions.id）。
	// NULL = 该链尚未 fork（Bot 未参与）。
	SessionID *uint64 `json:"sessionId,omitempty"`

	// Degraded root 是降级结果（拿不到真 root，用了直接父帖），可被后续自愈覆盖。
	Degraded bool `gorm:"not null;default:false" json:"degraded"`

	// CreatedAt 入站时间。
	CreatedAt time.Time `gorm:"not null" json:"createdAt"`
}

// TableName 指定 GORM 表名。
func (SessionThreadIndex) TableName() string { return "session_thread_index" }
