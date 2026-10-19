package dao

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"
)

// NotifyLegacySessionMoveName 是本次迁移的登记名（见 data_migration.go）。
const NotifyLegacySessionMoveName = "2026-10-19-notify-legacy-session-move"

// MoveLegacyNotifyRows 把写错会话的 notify 历史收回到该 chat 的数字会话。
//
// 背景：D6（0d71897，2026-09-29）把 Telegram 入站历史切到 chat_sessions 的数字 ID，
// 但 notify 仍按旧字符串键 "tg:<chat>" 落库（api/handler_notify.go notifySessionID）。
// 于是 smartd / mdadm 等告警（role=notify 的系统备注 + bot 转述的 assistant 行，
// trace_id 均为事件 ID "ntf-…"）进了一个模型永远不加载的孤儿会话，主人随后回话时
// bot 看不到自己刚发过的告警。代码侧已修（notify 改按 ResolveSession 写入），本迁移
// 收编存量。
//
// 规则：
//   - 只动 session_id LIKE 'tg:%' 且 trace_id LIKE 'ntf-%' 的行；旧会话里其它行
//     （若有）一律不碰。chat id 必须是整数（Telegram chat id），否则跳过该组。
//   - 目标会话按 (bot_id, TelegramChatKey(chat)) 经 ResolveSession 查/建 —— 与入站、
//     与修复后的 notify 共用同一唯一键，三方收敛到同一行。
//   - **只 UPDATE session_id 一列**：保留原 id 与 created_at。历史窗口按
//     created_at DESC, id DESC 取最近 N 条，旧告警保留原时间就只会落在它们真实
//     发生的位置，不会冒充「最近 20 条」挤掉近期对话；绝不 INSERT 新行（新 id /
//     新时间会把旧告警顶到窗口末尾）。
//   - 压缩边界：context_checkpoints 以「id <= boundary_message_id」表示被摘要覆盖。
//     迁入一条 id <= 目标会话边界的行，等于把摘要从未见过的内容静默划进摘要范围，
//     还可能让本已因「覆盖行滑出窗口」而失效的检查点重新生效。因此只迁 id 大于目标
//     会话全部检查点（含已停用的）最大边界的行；更旧的留在旧会话不动，登记里计数。
//   - 迁完按真实行数重算目标会话的 message_count / last_msg_at（与
//     2026-09-29-session-recount 同一口径）。旧字符串会话没有 chat_sessions 行，无计数可改。
//
// 幂等：runDataMigrations 保证恰好执行一次；函数本身也可安全重跑——迁过的行已不在
// tg: 会话里，第二次只会找到被边界留下的行并再次跳过，计数重算结果不变。
func MoveLegacyNotifyRows(tx *gorm.DB) (string, error) {
	type group struct {
		BotID     string `gorm:"column:bot_id"`
		SessionID string `gorm:"column:session_id"`
	}
	var groups []group
	if err := tx.Raw(`SELECT bot_id, session_id FROM chat_messages
		WHERE session_id LIKE 'tg:%' AND trace_id LIKE 'ntf-%'
		GROUP BY bot_id, session_id ORDER BY bot_id, session_id`).Scan(&groups).Error; err != nil {
		return "", fmt.Errorf("scan legacy notify sessions: %w", err)
	}

	var moved, kept, skippedGroups int
	details := make([]string, 0, len(groups))
	for _, g := range groups {
		chat := strings.TrimPrefix(g.SessionID, "tg:")
		if _, err := strconv.ParseInt(chat, 10, 64); err != nil {
			skippedGroups++
			details = append(details, fmt.Sprintf("%s:skip(non-numeric chat)", g.SessionID))
			continue
		}
		sess, err := ResolveSession(tx, g.BotID, TelegramChatKey(chat), TelegramSessionKind(chat), "")
		if err != nil {
			return "", fmt.Errorf("resolve session for %s/%s: %w", g.BotID, g.SessionID, err)
		}
		target := strconv.FormatUint(sess.ID, 10)

		var boundary uint64
		if err := tx.Raw(`SELECT COALESCE(MAX(boundary_message_id), 0) FROM context_checkpoints
			WHERE bot_id = ? AND session_id = ?`, g.BotID, target).Scan(&boundary).Error; err != nil {
			return "", fmt.Errorf("checkpoint boundary of session %s: %w", target, err)
		}

		res := tx.Exec(`UPDATE chat_messages SET session_id = ?
			WHERE bot_id = ? AND session_id = ? AND trace_id LIKE 'ntf-%' AND id > ?`,
			target, g.BotID, g.SessionID, boundary)
		if res.Error != nil {
			return "", fmt.Errorf("move notify rows %s -> %s: %w", g.SessionID, target, res.Error)
		}

		var left int64
		if err := tx.Model(&ChatMessage{}).
			Where("bot_id = ? AND session_id = ? AND trace_id LIKE 'ntf-%'", g.BotID, g.SessionID).
			Count(&left).Error; err != nil {
			return "", fmt.Errorf("count kept notify rows of %s: %w", g.SessionID, err)
		}

		if err := tx.Exec(`UPDATE chat_sessions SET
				message_count = (SELECT COUNT(*) FROM chat_messages WHERE session_id = ?),
				last_msg_at = COALESCE((SELECT MAX(created_at) FROM chat_messages WHERE session_id = ?), last_msg_at),
				updated_at = ?
			WHERE id = ?`, target, target, time.Now(), sess.ID).Error; err != nil {
			return "", fmt.Errorf("recount session %d: %w", sess.ID, err)
		}

		moved += int(res.RowsAffected)
		kept += int(left)
		details = append(details, fmt.Sprintf("%s/%s->%s(moved=%d,kept=%d,boundary=%d)",
			g.BotID, g.SessionID, target, res.RowsAffected, left, boundary))
	}
	return fmt.Sprintf("groups=%d moved=%d kept_below_boundary=%d skipped_groups=%d %s",
		len(groups), moved, kept, skippedGroups, strings.Join(details, " ")), nil
}
