package dao

import (
	"strconv"
	"time"

	"gorm.io/gorm"
)

// RecountSession derives chat_sessions.message_count / last_msg_at for one
// session from its actual chat_messages rows.
//
// message_count used to be a blind "+1" counter maintained only on some insert
// paths, while deletes (/compact, /clear) never decremented it and some inserts
// (/compact summary) never incremented it, so it drifted. Deriving it from rows
// on every write makes it self-healing and keeps it exact regardless of which
// path inserted, deleted or moved a row.
//
// Non-numeric session ids (legacy "tg:<chat>" keys) have no chat_sessions row;
// they are ignored.
func RecountSession(db *gorm.DB, sessionID string) error {
	id, err := strconv.ParseUint(sessionID, 10, 64)
	if err != nil || id == 0 {
		return nil
	}
	return db.Exec(`UPDATE chat_sessions SET
			message_count = (SELECT COUNT(*) FROM chat_messages WHERE session_id = ?),
			last_msg_at = COALESCE((SELECT MAX(created_at) FROM chat_messages WHERE session_id = ?), last_msg_at),
			updated_at = ?
		WHERE id = ?`, sessionID, sessionID, time.Now(), id).Error
}
