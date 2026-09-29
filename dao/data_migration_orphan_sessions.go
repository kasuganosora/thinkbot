package dao

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/kasuganosora/thinkbot/util/strutil"
)

// orphanSessionBackfillName 孤儿会话收编（B24-A / B31 + D6）。
const orphanSessionBackfillName = "2026-09-29-orphan-session-backfill"

// backfillOrphanSessions 把 chat_messages 里「没有对应 chat_sessions 行」的 session_id
// 收编进会话表，并把存量消息的 session_id 改写为新的数字会话 ID。
//
// 实测（2026-09-29）：479 条消息里 417 条是孤儿，全部属于 bot-2d8f9b087270da0bcfe177a5：
//
//	tg:76017910 (364) / '' (35) / cfblog-a11y-retest* (12) / verify-compact-* (4)
//	tg:-914633707 (2) / web-4a70085616a7af13f9ef0460 (1)
//
// 后果是三层：**列表列不出**（会话表没行）、**点进去 0 条**（B24-B）、
// **touchSessionAfterSave 不生效**（它对非数字 sessionID 直接 return，于是渠道会话的
// last_msg_at / message_count / 自动标题永远不更新）。
//
// 只 UPDATE、绝不 DELETE —— 与既有数据迁移同一纪律。
//
// 幂等性由两层保证：① runDataMigrations 用「执行 + 登记同事务」保证恰好一次；
// ② 本函数自身跳过已归属的 session_id，即便重跑也不会建出重复会话。
func backfillOrphanSessions(tx *gorm.DB) (string, error) {
	type pair struct {
		BotID     string `gorm:"column:bot_id"`
		SessionID string `gorm:"column:session_id"`
	}
	var pairs []pair
	if err := tx.Raw("SELECT bot_id, session_id FROM chat_messages GROUP BY bot_id, session_id").Scan(&pairs).Error; err != nil {
		return "", fmt.Errorf("scan session pairs: %w", err)
	}

	// 已有会话行的数字 ID：session_id 命中即视为已归属，跳过。
	owned := map[string]struct{}{}
	var ids []uint64
	if err := tx.Model(&ChatSession{}).Pluck("id", &ids).Error; err != nil {
		return "", fmt.Errorf("pluck session ids: %w", err)
	}
	for _, id := range ids {
		owned[strconv.FormatUint(id, 10)] = struct{}{}
	}

	var created, migrated, orphanGroups int
	var details []string
	for _, p := range pairs {
		if _, ok := owned[p.SessionID]; ok {
			continue
		}
		orphanGroups++

		externalKey, kind, title := classifyOrphan(tx, p.BotID, p.SessionID)
		if title == "" {
			// 首条消息为空或是命令时给占位标题（isPlaceholderSessionTitle 认可的三种之一），
			// 后续新消息仍会自动覆盖成真实首条——不要留空串，DB default 能否生效取决于
			// GORM 对零值字段的处理，不可依赖。
			title = "默认会话"
		}

		var sess ChatSession
		if externalKey != "" {
			// 走 ResolveSession：与增量路径共用 (bot_id, external_key) 唯一键，
			// 保证迁移与增量收敛到同一行，不会出现「新老消息分属两个会话」的上下文断裂。
			got, err := ResolveSession(tx, p.BotID, externalKey, kind, title)
			if err != nil {
				return "", fmt.Errorf("resolve session for %q: %w", p.SessionID, err)
			}
			sess = got
		} else {
			// 无 external_key 的历史会话（空 session_id / web 测试残留）：空值不进
			// partial 唯一索引，无法按 key 去重，只能直接建行——靠「恰好一次」保证不重复。
			now := time.Now()
			sess = ChatSession{
				BotID:       p.BotID,
				Title:       title,
				Status:      SessionStatusActive,
				SessionKind: kind,
				CreatedAt:   now,
				UpdatedAt:   now,
			}
			if err := tx.Create(&sess).Error; err != nil {
				return "", fmt.Errorf("create session for %q: %w", p.SessionID, err)
			}
		}
		created++

		newID := strconv.FormatUint(sess.ID, 10)
		q := tx.Model(&ChatMessage{}).Where("bot_id = ?", p.BotID)
		if p.SessionID == "" {
			// 空串与 NULL 都要收编：GORM 对 NULL 列扫出来也是空串，但 SQL 里 NULL ≠ ''。
			q = q.Where("session_id = '' OR session_id IS NULL")
		} else {
			q = q.Where("session_id = ?", p.SessionID)
		}
		res := q.Update("session_id", newID)
		if res.Error != nil {
			return "", fmt.Errorf("migrate messages of %q: %w", p.SessionID, res.Error)
		}
		migrated += int(res.RowsAffected)

		// 回填计数与末条时间：让新会话在列表里排序正常，而不是堆在最后。
		if err := tx.Exec(
			`UPDATE chat_sessions SET message_count = (SELECT COUNT(*) FROM chat_messages WHERE session_id = ?),
				last_msg_at = (SELECT MAX(created_at) FROM chat_messages WHERE session_id = ?),
				updated_at = ?
			 WHERE id = ?`,
			newID, newID, time.Now(), sess.ID).Error; err != nil {
			return "", fmt.Errorf("refresh session %d: %w", sess.ID, err)
		}

		details = append(details, fmt.Sprintf("%s→%d(%s,%d)", displaySessionID(p.SessionID), sess.ID, kind, res.RowsAffected))
		owned[newID] = struct{}{}
	}

	return fmt.Sprintf("orphan_groups=%d sessions_created=%d messages_migrated=%d %s",
		orphanGroups, created, migrated, strings.Join(details, " ")), nil
}

// classifyOrphan 判定一个孤儿 session_id 的 external_key / session_kind / 标题。
//
// ⚠️ 只有能构造出稳定 external_key 的渠道会话才填 key（tg: 前缀）—— 它是迁移与增量
// 收敛到同一行的唯一依据。历史 web 会话与测试残留拿不到稳定 key，一律留空：
// 留空不进 partial 唯一索引，不会被未来的增量路径误匹配。
func classifyOrphan(tx *gorm.DB, botID, sessionID string) (externalKey, kind, title string) {
	if strings.HasPrefix(sessionID, "tg:") {
		chatID := strings.TrimPrefix(sessionID, "tg:")
		if chatID == "" {
			return "", KindWeb, "默认会话"
		}
		return TelegramChatKey(chatID), TelegramSessionKind(chatID), orphanTitle(tx, botID, sessionID)
	}
	if sessionID == "" {
		return "", KindWeb, "默认会话"
	}
	// web / notify / 测试残留（cfblog-a11y-retest*、verify-compact-*、web-*）：保留可读标题，不占 key。
	return "", KindWeb, orphanTitle(tx, botID, sessionID)
}

// orphanTitle 取该孤儿会话首条用户消息作为标题。
//
// 与 api 侧 titleFromFirstMessage 语义不同（增量可留空等下一条 user 消息补，迁移则
// 必须立刻给出标题），故不共用实现，只共用剥前缀这一步。
func orphanTitle(tx *gorm.DB, botID, sessionID string) string {
	var content string
	q := tx.Model(&ChatMessage{}).Where("bot_id = ? AND role = ?", botID, ChatRoleUser)
	if sessionID == "" {
		q = q.Where("session_id = '' OR session_id IS NULL")
	} else {
		q = q.Where("session_id = ?", sessionID)
	}
	if err := q.Order("id ASC").Limit(1).Pluck("content", &content).Error; err != nil {
		return ""
	}
	s := strings.Join(strings.Fields(strutil.StripChannelContextMarkers(content)), " ")
	if s == "" || strings.HasPrefix(s, "/") {
		return ""
	}
	runes := []rune(s)
	if len(runes) > 30 {
		return string(runes[:30]) + "…"
	}
	return s
}

// displaySessionID 给登记文本用的可读表示（空串在日志里看不出来）。
func displaySessionID(s string) string {
	if s == "" {
		return "''"
	}
	return s
}
