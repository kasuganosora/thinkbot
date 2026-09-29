package dao

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"
)

// ============================================================================
// SessionResolver — 会话身份的唯一入口（S1 / D6）
//
// 背景：渠道（Telegram / Misskey）的会话此前只以字符串 session_id（如 "tg:76017910"）
// 存在于 chat_messages 里，**chat_sessions 中根本没有对应行**（实测 364 条全是孤儿），
// 于是 Web 列表列不出、touchSessionAfterSave 不更新、DELETE 删不干净。
//
// 统一为数字会话身份后，本文件成为「按 external_key 查/建会话行」的**唯一入口**，
// 被两处共用：
//   - 增量：inbound 落库前解析出数字 session id
//   - 存量：backfill 迁移把孤儿消息收编进会话行
//
// ⚠️ 两者必须以 **external_key** 而不是字符串 session_id 作为唯一键 —— 这样即使迁移
//    因故没跑完，增量路径也会收敛到**同一行**，不会出现「新消息在新会话、老消息在
//    旧字符串 id」的上下文断裂。
// ============================================================================

// external_key 的渠道取值（三段式 `<channel>:<kind>:<id...>` 的首段）。
const (
	ChannelTelegram = "telegram"
	ChannelMisskey  = "misskey"
)

// session_kind 取值。空串表示未分类（历史 web 会话），由后续回填。
const (
	KindDirect   = "direct"
	KindGroup    = "group"
	KindTopic    = "topic"
	KindThread   = "thread"
	KindTimeline = "timeline"
	KindWeb      = "web"
	KindNotify   = "notify"
)

// TelegramChatKey 构造 Telegram 会话的 external_key：`telegram:chat:<chatID>`。
func TelegramChatKey(chatID string) string { return ChannelTelegram + ":chat:" + chatID }

// TelegramSessionKind 依据 chatID 形态判定会话类型：Telegram 的群组/频道 chatID 为负数，
// 私聊为正数。未解析 MessageThreadID 之前不区分 topic。
func TelegramSessionKind(chatID string) string {
	if strings.HasPrefix(chatID, "-") {
		return KindGroup
	}
	return KindDirect
}

// MisskeyThreadKey 构造 Misskey thread 会话的 external_key：`misskey:thread:<rootNoteID>`。
func MisskeyThreadKey(rootNoteID string) string { return ChannelMisskey + ":thread:" + rootNoteID }

// MisskeyDMKey 构造 Misskey DM 会话的 external_key：`misskey:dm:<userID>`。
func MisskeyDMKey(userID string) string { return ChannelMisskey + ":dm:" + userID }

// ParseExternalKey 把 external_key 拆成 (channel, kind, id)。
// 形如 "telegram:chat:76017910" → ("telegram", "chat", "76017910")。
// 段数不足或含空段时 ok=false —— 调用方据此回退，不要猜。
func ParseExternalKey(key string) (channel, kind, id string, ok bool) {
	if key == "" {
		return "", "", "", false
	}
	parts := strings.SplitN(key, ":", 3)
	if len(parts) != 3 {
		return "", "", "", false
	}
	for _, p := range parts {
		if p == "" {
			return "", "", "", false
		}
	}
	return parts[0], parts[1], parts[2], true
}

// ResolveSession 按 (botID, externalKey) 查找会话行；不存在则创建。
//
// 返回的行带数字 ID，可直接用作 chat_messages.session_id。
//
// ⚠️ externalKey 为空时**拒绝** upsert：空值不进 partial 唯一索引（见 migrate.go），
// 无法唯一定位，强行创建每次都会新增一行（曾因此让孤儿消息永远收不进去）。
// 这类会话（历史 web 会话）请由调用方自行管理生命周期。
//
// 并发安全：两个 goroutine 同时首次解析同一 key 时，后一个 Create 会撞唯一索引；
// 此时重新查一次而不是把错误抛给调用方——并发下这不是异常，是正常竞争。
func ResolveSession(db *gorm.DB, botID, externalKey, kind, title string) (ChatSession, error) {
	if db == nil {
		return ChatSession{}, fmt.Errorf("dao: resolve session: nil db")
	}
	if botID == "" || externalKey == "" {
		return ChatSession{}, fmt.Errorf("dao: resolve session: botID and externalKey are required (got botID=%q key=%q)", botID, externalKey)
	}
	if _, _, _, ok := ParseExternalKey(externalKey); !ok {
		return ChatSession{}, fmt.Errorf("dao: resolve session: malformed external_key %q", externalKey)
	}

	var sess ChatSession
	err := db.Where("bot_id = ? AND external_key = ?", botID, externalKey).First(&sess).Error
	if err == nil {
		return sess, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return ChatSession{}, fmt.Errorf("dao: resolve session lookup: %w", err)
	}

	now := time.Now()
	sess = ChatSession{
		BotID:       botID,
		Title:       title,
		Status:      SessionStatusActive,
		ExternalKey: externalKey,
		SessionKind: kind,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if sess.Title == "" {
		sess.Title = "新会话"
	}
	if err := db.Create(&sess).Error; err != nil {
		var again ChatSession
		if e2 := db.Where("bot_id = ? AND external_key = ?", botID, externalKey).First(&again).Error; e2 == nil {
			return again, nil
		}
		return ChatSession{}, fmt.Errorf("dao: resolve session create: %w", err)
	}
	return sess, nil
}

// LookupSessionByExternalKey 只查不建。用于只读路径（如续跑路由解析 target），
// 避免为了「看看有没有」而凭空建出一个空会话。
func LookupSessionByExternalKey(db *gorm.DB, botID, externalKey string) (ChatSession, bool, error) {
	var sess ChatSession
	err := db.Where("bot_id = ? AND external_key = ?", botID, externalKey).First(&sess).Error
	if err == nil {
		return sess, true, nil
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ChatSession{}, false, nil
	}
	return ChatSession{}, false, fmt.Errorf("dao: lookup session by external key: %w", err)
}

// parseSessionID 把会话 ID 字符串解析为数字主键。
// ⚠️ 单独抽出来是因为「是不是数字会话 ID」这个判断在多处重复（touchSessionAfterSave、
// 续跑路由、本文件），各写各的会漂移。空串与 0 一律视为非法：历史消息里有
// session_id=” 的孤儿行，strconv 对空串报错但有些路径可能漏判 0。
func parseSessionID(sessionID string) (uint64, error) {
	if sessionID == "" {
		return 0, fmt.Errorf("dao: empty session id")
	}
	id, err := strconv.ParseUint(sessionID, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("dao: session id %q is not numeric: %w", sessionID, err)
	}
	if id == 0 {
		return 0, fmt.Errorf("dao: session id must be positive")
	}
	return id, nil
}

// LookupSessionByID 按数字会话 ID 查找。sessionID 非数字时返回 found=false（不报错）——
// 旧式字符串 session_id 的调用方据此走兼容分支。
func LookupSessionByID(db *gorm.DB, sessionID string) (ChatSession, bool, error) {
	id, err := parseSessionID(sessionID)
	if err != nil {
		return ChatSession{}, false, nil
	}
	var sess ChatSession
	if err := db.First(&sess, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ChatSession{}, false, nil
		}
		return ChatSession{}, false, fmt.Errorf("dao: lookup session by id: %w", err)
	}
	return sess, true, nil
}
