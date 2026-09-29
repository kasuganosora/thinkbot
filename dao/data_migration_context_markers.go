package dao

import (
	"fmt"
	"strings"

	"github.com/kasuganosora/thinkbot/util/strutil"
	"gorm.io/gorm"
)

// contextMarkersFixName 清洗存量记忆/事件流里残留的渠道装饰标记。
const contextMarkersFixName = "2026-09-29-strip-channel-context-markers"

// contextMarkerLikePatterns 装饰标记的 LIKE 模式（与 strutil.StripChannelContextMarkers
// 处理的格式一一对应）。SQLite 的 LIKE 只有 % 和 _ 是通配符，'[' 是普通字符，无需转义。
var contextMarkerLikePatterns = []string{
	"[Reply to %",
	"[Renote from %",
	"[Timeline]%",
	"[DM]%",
	"[对方是 Bot 账号%",
	"%[note_id:%",
}

// fixChannelContextMarkers 剥离存量文本里渠道注入的装饰前缀/后缀。
//
// 背景：Misskey 渠道把 "[Reply to <bot名>: <被回复帖正文>]"、"[Timeline] @u: "、
// "[note_id: xxx]" 等上下文一律拼进 core.Message.Text（channel/misskey/channel.go 的
// noteContext / handleNote），而 2026-09-29 之前 note_capture 的清洗正则**显式排除了
// "[Reply to ...]"**（旧注释误判它是帖子正文的一部分），导致：
//  1. 说话人归属错误——被回复帖正文（多数情况下就是 Bot 自己的话）被当作用户原话
//     写入 L0，再被 dreaming 巩固成「用户画像」；
//  2. 会话标题被前缀吃满（titleFromFirstMessage 只截前 30 rune）。
//
// 增量已由 strutil.StripChannelContextMarkers 在写入侧修复，此处只清洗存量。
// 实测（thinkbot.db，2026-09-29）：tiered_memories 244 条、user_message_events 265 条。
//
// 只 UPDATE 不 DELETE：
//   - 剥离后为空的行一律跳过（实测 0 条，但保留该护栏，绝不因清洗丢数据）；
//   - user_message_events 是 dreaming 回灌的权威源且以 max(id) 为水位线，删行会让
//     原文丢失，故一律更新；
//   - 剥离后的碎片（如井字棋落子 "4"）交由既有的 cleanup-trivial 接口处理——它带
//     dryRun 且需人工触发，自动迁移里不删除任何东西。
func fixChannelContextMarkers(tx *gorm.DB) (string, error) {
	memUpdated, memEmpty, err := stripMarkersInTable(tx, "tiered_memories")
	if err != nil {
		return "", err
	}
	evUpdated, evEmpty, err := stripMarkersInTable(tx, "user_message_events")
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(
		"tiered_memories: updated=%d skipped_empty=%d; user_message_events: updated=%d skipped_empty=%d",
		memUpdated, memEmpty, evUpdated, evEmpty), nil
}

// stripMarkersInTable 逐行剥离指定表 content 列里的装饰标记。
//
// 必须逐行：清洗依赖 Go 正则（含 (?s) 跨行匹配），SQL 的 REPLACE/LIKE 无法表达。
// 两表主键类型不同（tiered_memories 是 string、user_message_events 是 uint64），
// 故统一用 map 承载，避免为迁移引入额外模型约束。
//
// 返回更新条数与因「剥离后为空」而跳过的条数。
func stripMarkersInTable(tx *gorm.DB, table string) (updated, skipped int, err error) {
	var rows []map[string]any
	q := tx.Table(table).Select("id", "content")
	for i, p := range contextMarkerLikePatterns {
		if i == 0 {
			q = q.Where("content LIKE ?", p)
		} else {
			q = q.Or("content LIKE ?", p)
		}
	}
	if err := q.Scan(&rows).Error; err != nil {
		return 0, 0, err
	}
	for _, r := range rows {
		content, _ := r["content"].(string)
		cleaned := strutil.StripChannelContextMarkers(content)
		if cleaned == "" {
			skipped++
			continue
		}
		if cleaned == strings.TrimSpace(content) {
			continue // 无装饰或清洗无变化，不动
		}
		if err := tx.Table(table).Where("id = ?", r["id"]).Update("content", cleaned).Error; err != nil {
			return updated, skipped, err
		}
		updated++
	}
	return updated, skipped, nil
}
