package api

import (
	"strconv"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/dao"
)

func newSessionTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=private"), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := dao.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

// TestInboundSessionID D6 之后入站会话 ID 不再是 "tg:<chatID>" 字符串，而是 chat_sessions
// 的数字 ID —— 字符串 id 下会话表里没有行，Web 列不出、touch/delete 全失效（B24-A/B31）。
// 这里锁住三条：① 返回数字；② 会话行带正确的 external_key / session_kind；
// ③ 同一 chat 反复解析返回**同一个** id（不重复建行）。
func TestInboundSessionID(t *testing.T) {
	db := newSessionTestDB(t)
	s := &BotService{db: db}

	cases := []struct {
		name       string
		msg        core.Message
		wantKey    string
		wantKind   string
		wantStatus string
		wantOK     bool
	}{
		{
			name:     "telegram private chat",
			msg:      core.Message{BotID: "bot-a", Channel: "12345", Metadata: map[string]any{"channel_type": "telegram"}},
			wantKey:  "telegram:chat:12345",
			wantKind: dao.KindDirect,
			wantOK:   true,
		},
		{
			name:     "telegram group chat (negative chatID) shares one session by chatID",
			msg:      core.Message{BotID: "bot-a", Channel: "-98765", Metadata: map[string]any{"channel_type": "telegram"}},
			wantKey:  "telegram:chat:-98765",
			wantKind: dao.KindGroup,
			wantOK:   true,
		},
		{
			name:     "misskey mention/reply -> per-user dm session",
			msg:      core.Message{BotID: "bot-a", Channel: "9a1b2c", Metadata: map[string]any{"channel_type": "misskey"}},
			wantKey:  "misskey:dm:9a1b2c",
			wantKind: dao.KindDirect,
			wantOK:   true,
		},
		{
			// timeline 是旁听型：全局唯一且 archived，否则它会以最新 last_msg_at
			// 永久占据列表首位（B17）。
			name:       "misskey timeline broadcast -> single archived session",
			msg:        core.Message{BotID: "bot-a", Channel: "misskey:timeline", Metadata: map[string]any{"channel_type": "misskey"}},
			wantKey:    "misskey:timeline:global",
			wantKind:   dao.KindTimeline,
			wantStatus: dao.SessionStatusArchived,
			wantOK:     true,
		},
		{
			name:   "telegram empty channel -> skip",
			msg:    core.Message{BotID: "bot-a", Channel: "", Metadata: map[string]any{"channel_type": "telegram"}},
			wantOK: false,
		},
		{
			name:   "telegram without botID -> skip (external_key must be scoped by bot)",
			msg:    core.Message{Channel: "12345", Metadata: map[string]any{"channel_type": "telegram"}},
			wantOK: false,
		},
		{
			name:   "web source already handled -> skip",
			msg:    core.Message{BotID: "bot-a", Channel: "u1", Metadata: map[string]any{"channel_type": "web"}},
			wantOK: false,
		},
		{
			name:   "no channel_type -> skip (heartbeat/cron/etc)",
			msg:    core.Message{BotID: "bot-a", Channel: "x"},
			wantOK: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			msg := c.msg
			sid, kind, ok := s.inboundSessionID(&msg)
			if ok != c.wantOK {
				t.Fatalf("ok=%v want %v (sid=%q)", ok, c.wantOK, sid)
			}
			if !c.wantOK {
				if sid != "" {
					t.Fatalf("sid should be empty on skip, got %q", sid)
				}
				return
			}
			id, err := strconv.ParseUint(sid, 10, 64)
			if err != nil {
				t.Fatalf("sid=%q is not numeric (D6 regression): %v", sid, err)
			}
			var sess dao.ChatSession
			if err := db.First(&sess, id).Error; err != nil {
				t.Fatalf("session row %d missing: %v", id, err)
			}
			if sess.ExternalKey != c.wantKey {
				t.Errorf("external_key = %q, want %q", sess.ExternalKey, c.wantKey)
			}
			if c.wantKind != "" && sess.SessionKind != c.wantKind {
				t.Errorf("session_kind = %q, want %q", sess.SessionKind, c.wantKind)
			}
			// kind 返回值供 enricher 决定是否注入 chat_history，必须与会话行一致
			if c.wantKind != "" && kind != c.wantKind {
				t.Errorf("returned kind = %q, want %q", kind, c.wantKind)
			}
			wantStatus := c.wantStatus
			if wantStatus == "" {
				wantStatus = dao.SessionStatusActive
			}
			if sess.Status != wantStatus {
				t.Errorf("status = %q, want %q", sess.Status, wantStatus)
			}
			// 重复解析必须收敛到同一行
			again, _, _ := s.inboundSessionID(&msg)
			if again != sid {
				t.Errorf("not idempotent: %q then %q", sid, again)
			}
			var n int64
			db.Model(&dao.ChatSession{}).Where("external_key = ?", c.wantKey).Count(&n)
			if n != 1 {
				t.Errorf("session rows for %s = %d, want 1", c.wantKey, n)
			}
		})
	}
}

// TestResolveSessionRoute 锁住续跑路由：D6 之后 sessionID 是数字，渠道信息只在会话行的
// external_key 里。若仍按字符串切分，kind 永远不是 "tg"，TG 续跑会**静默退回 web 兜底**
// ——表现为「工作流跑完了但 Telegram 里什么都没有」。
func TestResolveSessionRoute(t *testing.T) {
	db := newSessionTestDB(t)
	s := &BotService{db: db}

	cases := []struct {
		name       string
		sessionID  string
		wantKind   string
		wantTarget string
	}{
		{"numeric tg dm session", mustResolve(t, db, "bot-a", dao.TelegramChatKey("76017910"), dao.KindDirect), "tg", "76017910"},
		{"numeric tg group session (negative chatID)", mustResolve(t, db, "bot-a", dao.TelegramChatKey("-914633707"), dao.KindGroup), "tg", "-914633707"},
		{"numeric misskey session", mustResolve(t, db, "bot-a", dao.MisskeyDMKey("u9"), dao.KindDirect), "mk", "u9"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			kind, target := s.resolveSessionRoute(c.sessionID)
			if kind != c.wantKind || target != c.wantTarget {
				t.Errorf("resolveSessionRoute(%q) = (%q,%q), want (%q,%q)", c.sessionID, kind, target, c.wantKind, c.wantTarget)
			}
		})
	}

	// 历史 web 会话：有会话行但没有 external_key → 无渠道可路由
	web := dao.ChatSession{BotID: "bot-a", Title: "web 会话", Status: dao.SessionStatusActive}
	if err := db.Create(&web).Error; err != nil {
		t.Fatalf("seed web session: %v", err)
	}
	if kind, _ := s.resolveSessionRoute(strconv.FormatUint(web.ID, 10)); kind != "" {
		t.Errorf("web session kind = %q, want empty", kind)
	}

	// 旧式字符串 sessionID 兜底（迁移前的历史数据 / 测试）
	if kind, target := s.resolveSessionRoute("tg:123"); kind != "tg" || target != "123" {
		t.Errorf("legacy string id: got (%q,%q), want (tg,123)", kind, target)
	}

	// 数字但会话行不存在（会话被删）→ 兜底返回原值，不 panic
	if kind, target := s.resolveSessionRoute("999999"); kind != "" || target != "999999" {
		t.Errorf("missing session: got (%q,%q), want (\"\",999999)", kind, target)
	}
}

func mustResolve(t *testing.T, db *gorm.DB, botID, key, kind string) string {
	t.Helper()
	sess, err := dao.ResolveSession(db, botID, key, kind, "")
	if err != nil {
		t.Fatalf("ResolveSession(%s): %v", key, err)
	}
	return strconv.FormatUint(sess.ID, 10)
}

func TestBuildQuoteBlock(t *testing.T) {
	cases := []struct {
		name string
		meta map[string]any
		want string
	}{
		{
			name: "with author",
			meta: map[string]any{
				"reply_to_text": "明天开会",
				"reply_to_from": "露娜 (@luna)",
			},
			want: "[引用 露娜 (@luna) 的消息]\n明天开会",
		},
		{
			name: "without author",
			meta: map[string]any{"reply_to_text": "hello"},
			want: "[引用消息]\nhello",
		},
		{
			name: "empty quoted text -> no block",
			meta: map[string]any{"reply_to_text": "  ", "reply_to_from": "x"},
			want: "",
		},
		{
			name: "no quoted text -> no block",
			meta: map[string]any{"foo": "bar"},
			want: "",
		},
		{
			name: "nil meta -> no block",
			meta: nil,
			want: "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := buildQuoteBlock(c.meta)
			if got != c.want {
				t.Fatalf("buildQuoteBlock=%q want %q", got, c.want)
			}
		})
	}
}
