package telegram

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/inbound"
	"github.com/kasuganosora/thinkbot/internal/interaction"
)

func TestUtf16Extract(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		offset   int
		length   int
		expected string
	}{
		{
			name:     "ASCII only",
			text:     "hello @mybot world",
			offset:   6,
			length:   6,
			expected: "@mybot",
		},
		{
			name:     "Chinese text before mention",
			text:     "你好 @mybot 世界",
			offset:   3, // "你好 " = 3 UTF-16 code units (each CJK char = 1 unit)
			length:   6,
			expected: "@mybot",
		},
		{
			name:     "Emoji before mention (surrogate pair)",
			text:     "😀 @mybot hello",
			offset:   3, // 😀 = 2 UTF-16 units + space = 3
			length:   6,
			expected: "@mybot",
		},
		{
			name:     "Multiple emojis before mention",
			text:     "👍🎉 @bot end",
			offset:   5, // 👍=2, 🎉=2, space=1 → offset 5
			length:   4,
			expected: "@bot",
		},
		{
			name:     "Extract emoji itself",
			text:     "hello 😀 world",
			offset:   6, // "hello " = 6
			length:   2, // 😀 = 2 UTF-16 code units
			expected: "😀",
		},
		{
			name:     "Mixed CJK and emoji",
			text:     "你好😀@bot",
			offset:   4, // 你=1, 好=1, 😀=2 → offset 4
			length:   4,
			expected: "@bot",
		},
		{
			name:     "Out of bounds returns empty",
			text:     "hello",
			offset:   10,
			length:   5,
			expected: "",
		},
		{
			name:     "Negative offset returns empty",
			text:     "hello",
			offset:   -1,
			length:   3,
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := utf16Extract(tt.text, tt.offset, tt.length)
			if result != tt.expected {
				t.Errorf("utf16Extract(%q, %d, %d) = %q, want %q",
					tt.text, tt.offset, tt.length, result, tt.expected)
			}
		})
	}
}

func TestDetectMention(t *testing.T) {
	ch := &TelegramChannel{
		botUsername: "mybot",
		botUserID:   12345,
	}

	tests := []struct {
		name     string
		msg      *Message
		expected bool
	}{
		{
			name: "mention entity matches bot username",
			msg: &Message{
				Text: "hello @mybot how are you",
				Entities: []MessageEntity{
					{Type: "mention", Offset: 6, Length: 6},
				},
			},
			expected: true,
		},
		{
			name: "mention entity does not match",
			msg: &Message{
				Text: "hello @otherbot how are you",
				Entities: []MessageEntity{
					{Type: "mention", Offset: 6, Length: 9},
				},
			},
			expected: false,
		},
		{
			name: "text_mention with matching user ID",
			msg: &Message{
				Text: "hello Bot how are you",
				Entities: []MessageEntity{
					{Type: "text_mention", Offset: 6, Length: 3, User: &User{ID: 12345}},
				},
			},
			expected: true,
		},
		{
			name: "text_mention with different user ID",
			msg: &Message{
				Text: "hello Bot how are you",
				Entities: []MessageEntity{
					{Type: "text_mention", Offset: 6, Length: 3, User: &User{ID: 99999}},
				},
			},
			expected: false,
		},
		{
			name: "bot_command at offset 0",
			msg: &Message{
				Text: "/help please",
				Entities: []MessageEntity{
					{Type: "bot_command", Offset: 0, Length: 5},
				},
			},
			expected: true,
		},
		{
			name: "bot_command not at offset 0",
			msg: &Message{
				Text: "text /help please",
				Entities: []MessageEntity{
					{Type: "bot_command", Offset: 5, Length: 5},
				},
			},
			expected: false,
		},
		{
			name: "mention with Chinese text before (UTF-16 offset)",
			msg: &Message{
				Text: "你好 @mybot 世界",
				Entities: []MessageEntity{
					{Type: "mention", Offset: 3, Length: 6},
				},
			},
			expected: true,
		},
		{
			name: "mention with emoji before (UTF-16 surrogate pair)",
			msg: &Message{
				Text: "😀 @mybot hello",
				Entities: []MessageEntity{
					{Type: "mention", Offset: 3, Length: 6}, // 😀=2 units + space=1
				},
			},
			expected: true,
		},
		// 附件说明（caption）场景：实体在 CaptionEntities，必须与 Caption 配对取用。
		// 修复前 detectMention 固定读 msg.Text，这两类消息在群里会被完全漏判。
		{
			name: "caption mention matches bot username",
			msg: &Message{
				Caption: "@mybot 看看这个",
				CaptionEntities: []MessageEntity{
					{Type: "mention", Offset: 0, Length: 6},
				},
				Photo: []PhotoSize{{FileID: "p1"}},
			},
			expected: true,
		},
		{
			name: "caption bot_command for another bot is ignored",
			msg: &Message{
				Caption: "/start@otherbot",
				CaptionEntities: []MessageEntity{
					{Type: "bot_command", Offset: 0, Length: 15},
				},
				Document: &Document{FileName: "a.pdf"},
			},
			expected: false,
		},
		{
			name: "caption bot_command for this bot",
			msg: &Message{
				Caption: "/start@mybot",
				CaptionEntities: []MessageEntity{
					{Type: "bot_command", Offset: 0, Length: 12},
				},
				Photo: []PhotoSize{{FileID: "p2"}},
			},
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 与 handleUpdate 一致：文本与实体按来源配对
			text, entities := ch.mentionTextAndEntities(tt.msg)
			result := ch.detectMention(text, entities)
			if result != tt.expected {
				t.Errorf("detectMention() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestSplitMessage(t *testing.T) {
	tests := []struct {
		name   string
		text   string
		maxLen int
		chunks int
	}{
		{
			name:   "short message no split",
			text:   "hello world",
			maxLen: 100,
			chunks: 1,
		},
		{
			name:   "exact length no split",
			text:   "hello",
			maxLen: 5,
			chunks: 1,
		},
		{
			name:   "needs split at newline",
			text:   "line1\nline2\nline3",
			maxLen: 12,
			chunks: 2,
		},
		{
			name:   "needs split no newline",
			text:   "abcdefghij",
			maxLen: 5,
			chunks: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := splitMessage(tt.text, tt.maxLen)
			if len(result) != tt.chunks {
				t.Errorf("splitMessage(%q, %d) returned %d chunks, want %d",
					tt.text, tt.maxLen, len(result), tt.chunks)
			}
			// Verify all text is preserved
			joined := ""
			for _, chunk := range result {
				joined += chunk
			}
			if joined != tt.text {
				t.Errorf("splitMessage lost text: got %q, want %q", joined, tt.text)
			}
		})
	}
}

func TestMediaGroupAggregation(t *testing.T) {
	ch := &TelegramChannel{}

	// 非相册消息（groupID 为空）永不跳过
	if ch.mediaGroupAlreadySeen("") {
		t.Error("empty media group id must never be skipped")
	}

	// 相册首条必须放行，后续条全部跳过
	if ch.mediaGroupAlreadySeen("album-1") {
		t.Error("first message of an album must not be skipped")
	}
	for i := 0; i < 4; i++ {
		if !ch.mediaGroupAlreadySeen("album-1") {
			t.Fatalf("subsequent album message #%d must be skipped", i+2)
		}
	}

	// 不同相册互不干扰
	if ch.mediaGroupAlreadySeen("album-2") {
		t.Error("a different album must not be skipped")
	}

	// 窗口过期后，同 ID 按新组处理（滑动窗口不应永久吞掉该 ID）
	ch.mediaGroupMu.Lock()
	ch.mediaGroupSeen["album-3"] = time.Now().Add(-mediaGroupWindow - time.Second)
	ch.mediaGroupMu.Unlock()
	if ch.mediaGroupAlreadySeen("album-3") {
		t.Error("expired album entry must not skip the message")
	}
}

// TestPollTimeouts 固化 long polling 的超时关系与单位正确性。
//
// 背景：httpTimeout 曾是 int 秒数，改为 time.Duration 后 WithTimeout 处仍
// 多乘了一次 time.Second，使 45s 变成约 1425 年——编译通过但超时静默失效。
// 本测试同时钉住「context 必须晚于客户端超时到期」这一不变量。
func TestPollTimeouts(t *testing.T) {
	const pollTimeout = 30

	clientTimeout := time.Duration(pollTimeout)*time.Second + pollTimeoutBuffer
	if clientTimeout != 45*time.Second {
		t.Errorf("client timeout = %v, want 45s (unit bug?)", clientTimeout)
	}

	ctxTimeout := apiTimeoutMultiplier(pollTimeout)
	if ctxTimeout != 50*time.Second {
		t.Errorf("context timeout = %v, want 50s", ctxTimeout)
	}

	// 核心不变量：context 必须比客户端超时更晚到期，否则客户端超时形同虚设
	if ctxTimeout <= clientTimeout {
		t.Errorf("context timeout (%v) must outlive client timeout (%v)", ctxTimeout, clientTimeout)
	}

	// 合理量级保护：任何超过 1 小时的轮询超时都说明单位算错了
	if ctxTimeout > time.Hour {
		t.Errorf("context timeout %v is implausibly large — unit error", ctxTimeout)
	}
}

func TestParseChoiceCallback(t *testing.T) {
	qid := "uc-0123456789abcdef012345"
	data := encodeChoiceCallback(qid, "o0")
	if len(data) > 64 {
		t.Fatalf("callback_data too long: %d %q", len(data), data)
	}
	gotQ, gotO, ok := parseChoiceCallback(data)
	if !ok || gotQ != qid || gotO != "o0" {
		t.Fatalf("parse %q → %q %q %v", data, gotQ, gotO, ok)
	}
	done := encodeChoiceCallback(qid, choiceDoneToken)
	if len(done) > 64 {
		t.Fatalf("done callback_data too long: %d", len(done))
	}
	_, tok, ok := parseChoiceCallback(done)
	if !ok || tok != choiceDoneToken {
		t.Fatalf("done token = %q", tok)
	}
	if _, _, ok := parseChoiceCallback("nope"); ok {
		t.Fatal("unknown prefix must fail")
	}
	if _, _, ok := parseChoiceCallback("c|only"); ok {
		t.Fatal("missing token must fail")
	}
}

func TestBuildChoiceKeyboard(t *testing.T) {
	opts := []interaction.Option{{ID: "o0", Label: "甲"}, {ID: "o1", Label: "乙"}}
	kb := buildChoiceKeyboard("uc-abc", opts, false, nil)
	if len(kb.InlineKeyboard) != 2 {
		t.Fatalf("single rows = %d", len(kb.InlineKeyboard))
	}
	if kb.InlineKeyboard[0][0].CallbackData != "c|uc-abc|o0" {
		t.Fatalf("data = %q", kb.InlineKeyboard[0][0].CallbackData)
	}
	kbM := buildChoiceKeyboard("uc-abc", opts, true, map[string]bool{"o1": true})
	if len(kbM.InlineKeyboard) != 3 {
		t.Fatalf("multi rows = %d (want options + 确认)", len(kbM.InlineKeyboard))
	}
	if kbM.InlineKeyboard[1][0].Text != "✓ 乙" {
		t.Fatalf("selected label = %q", kbM.InlineKeyboard[1][0].Text)
	}
	if kbM.InlineKeyboard[2][0].CallbackData != "c|uc-abc|~" {
		t.Fatalf("done data = %q", kbM.InlineKeyboard[2][0].CallbackData)
	}
	long := truncButtonText(string(make([]rune, 80)), 64)
	if len([]rune(long)) != 64 {
		t.Fatalf("trunc len = %d", len([]rune(long)))
	}
}

func TestDefaultAllowedUpdatesIncludesCallbackQuery(t *testing.T) {
	d := defaultAllowedUpdates()
	need := map[string]bool{"callback_query": false, "message_reaction": false}
	for _, u := range d {
		if _, ok := need[u]; ok {
			need[u] = true
		}
	}
	for k, ok := range need {
		if !ok {
			t.Fatalf("default allowed updates missing %s: %v", k, d)
		}
	}
	merged := mergeCallbackQueryUpdate([]string{"message"})
	need = map[string]bool{"callback_query": false, "message_reaction": false}
	for _, u := range merged {
		if _, ok := need[u]; ok {
			need[u] = true
		}
	}
	for k, ok := range need {
		if !ok {
			t.Fatalf("merge did not add %s: %v", k, merged)
		}
	}
	custom := []string{"message", "callback_query", "message_reaction"}
	got := mergeCallbackQueryUpdate(custom)
	if len(got) != 3 {
		t.Fatalf("should not duplicate: %v", got)
	}
}

func TestHandleUpdateCallbackQueryDoesNotIngress(t *testing.T) {
	ing := inbound.NewIngress(inbound.IngressConfig{BufferSize: 8}, zap.NewNop().Sugar(), noop.NewTracerProvider())
	defer ing.Close()
	ch := &TelegramChannel{name: "tg", botID: "b", ingress: ing}
	ch.handleUpdate(context.Background(), Update{
		CallbackQuery: &CallbackQuery{
			ID:   "cb1",
			Data: encodeChoiceCallback("uc-ghost", "o0"),
			From: &User{ID: 1},
			Message: &Message{
				MessageID: 10,
				Chat:      Chat{ID: 99},
			},
		},
	})
	if ing.Len() != 0 {
		t.Fatalf("callback_query must not be injected as chat message, len=%d", ing.Len())
	}
}

func TestHandleCallbackQuerySingleResolves(t *testing.T) {
	qid := "uc-tg-" + t.Name()
	q := interaction.Question{
		ID: qid, BotID: "b", ChatID: "99",
		Question: "选一个",
		Options:  []interaction.Option{{ID: "o0", Label: "甲"}, {ID: "o1", Label: "乙"}},
		Mode:     interaction.ModeSingle, TimeoutSecs: 30,
	}
	if _, err := interaction.Default().RegisterQuestion(q); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { interaction.Default().AbortPending(qid) })

	ch := &TelegramChannel{name: "tg", botID: "b"}
	ch.handleCallbackQuery(context.Background(), &CallbackQuery{
		ID:   "cb2",
		Data: encodeChoiceCallback(qid, "o1"),
		From: &User{ID: 7},
		Message: &Message{
			MessageID: 10,
			Chat:      Chat{ID: 99},
		},
	})
	snap, err := interaction.Default().Lookup(qid)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Status != interaction.StatusAnswered {
		t.Fatalf("status = %s", snap.Status)
	}
}

func TestHandleCallbackQueryWrongChatIgnored(t *testing.T) {
	qid := "uc-tg-wrong-" + t.Name()
	q := interaction.Question{
		ID: qid, BotID: "b", ChatID: "99",
		Question: "选一个",
		Options:  []interaction.Option{{ID: "o0", Label: "甲"}, {ID: "o1", Label: "乙"}},
		Mode:     interaction.ModeSingle, TimeoutSecs: 30,
	}
	if _, err := interaction.Default().RegisterQuestion(q); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { interaction.Default().AbortPending(qid) })

	ch := &TelegramChannel{name: "tg", botID: "b"}
	ch.handleCallbackQuery(context.Background(), &CallbackQuery{
		ID:   "cb3",
		Data: encodeChoiceCallback(qid, "o0"),
		From: &User{ID: 7},
		Message: &Message{
			MessageID: 10,
			Chat:      Chat{ID: 10001}, // different chat
		},
	})
	snap, _ := interaction.Default().Lookup(qid)
	if snap.Status != interaction.StatusPending {
		t.Fatalf("wrong chat must not resolve, status = %s", snap.Status)
	}
}

func TestHandleCallbackQueryMultiToggleThenDone(t *testing.T) {
	qid := "uc-tg-multi-" + t.Name()
	q := interaction.Question{
		ID: qid, BotID: "b", ChatID: "99",
		Question: "多选",
		Options:  []interaction.Option{{ID: "o0", Label: "甲"}, {ID: "o1", Label: "乙"}, {ID: "o2", Label: "丙"}},
		Mode:     interaction.ModeMulti, TimeoutSecs: 30,
	}
	if _, err := interaction.Default().RegisterQuestion(q); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { interaction.Default().AbortPending(qid) })

	ch := &TelegramChannel{name: "tg", botID: "b"}
	cq := func(tok string) *CallbackQuery {
		return &CallbackQuery{
			ID: "cb-" + tok, Data: encodeChoiceCallback(qid, tok),
			From:    &User{ID: 7},
			Message: &Message{MessageID: 10, Chat: Chat{ID: 99}},
		}
	}
	ch.handleCallbackQuery(context.Background(), cq("o0"))
	ch.handleCallbackQuery(context.Background(), cq("o2"))
	snap, _ := interaction.Default().Lookup(qid)
	if snap.Status != interaction.StatusPending {
		t.Fatalf("toggle must not resolve, status = %s", snap.Status)
	}
	ch.handleCallbackQuery(context.Background(), cq(choiceDoneToken))
	snap, err := interaction.Default().Lookup(qid)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Status != interaction.StatusAnswered {
		t.Fatalf("done should resolve, status = %s", snap.Status)
	}
}

func TestHandleCallbackQueryMultiDoneEmptyHint(t *testing.T) {
	qid := "uc-tg-empty-" + t.Name()
	q := interaction.Question{
		ID: qid, BotID: "b", ChatID: "99",
		Question: "多选",
		Options:  []interaction.Option{{ID: "o0", Label: "甲"}, {ID: "o1", Label: "乙"}},
		Mode:     interaction.ModeMulti, TimeoutSecs: 30,
	}
	if _, err := interaction.Default().RegisterQuestion(q); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { interaction.Default().AbortPending(qid) })

	ch := &TelegramChannel{name: "tg", botID: "b"}
	ch.handleCallbackQuery(context.Background(), &CallbackQuery{
		ID: "cb-empty", Data: encodeChoiceCallback(qid, choiceDoneToken),
		From:    &User{ID: 7},
		Message: &Message{MessageID: 10, Chat: Chat{ID: 99}},
	})
	snap, _ := interaction.Default().Lookup(qid)
	if snap.Status != interaction.StatusPending {
		t.Fatalf("empty done must not resolve, status = %s", snap.Status)
	}
}

func TestChoiceTimeoutScheduled(t *testing.T) {
	ch := &TelegramChannel{name: "tg", botID: "b", choicePending: map[string]*choicePending{
		"q-timeout": {ChatID: 1, MessageID: 2},
	}}
	ch.armChoiceTimeout("q-timeout", 600)
	ch.choiceMu.Lock()
	p := ch.choicePending["q-timeout"]
	ch.choiceMu.Unlock()
	if p == nil || p.timer == nil {
		t.Fatal("timeout timer should be set")
	}
	ch.dropChoicePending("q-timeout")
	if p.timer != nil {
		t.Fatal("dropChoicePending should Stop and clear timer")
	}
	if _, ok := ch.choicePending["q-timeout"]; ok {
		t.Fatal("pending should be dropped")
	}
}

func TestChoiceTimeoutZeroUsesDefault(t *testing.T) {
	ch := &TelegramChannel{name: "tg", botID: "b", choicePending: map[string]*choicePending{
		"q-zero": {ChatID: 1, MessageID: 2},
	}}
	ch.armChoiceTimeout("q-zero", 0)
	ch.choiceMu.Lock()
	p := ch.choicePending["q-zero"]
	ch.choiceMu.Unlock()
	if p == nil || p.timer == nil {
		t.Fatal("timeoutSecs<=0 should still schedule DefaultTimeoutSecs")
	}
	ch.dropChoicePending("q-zero")
}

func TestExpireChoiceDropsPending(t *testing.T) {
	ch := &TelegramChannel{name: "tg", botID: "b", choicePending: map[string]*choicePending{
		"q-exp": {ChatID: 1, MessageID: 2},
	}}
	ch.expireChoice("q-exp")
	if _, ok := ch.choicePending["q-exp"]; ok {
		t.Fatal("expireChoice should drop pending")
	}
}

func TestDropChoicePendingStopsTimer(t *testing.T) {
	ch := &TelegramChannel{name: "tg", botID: "b", choicePending: map[string]*choicePending{
		"q-stop": {ChatID: 1, MessageID: 2},
	}}
	fired := make(chan struct{}, 1)
	ch.choiceMu.Lock()
	ch.choicePending["q-stop"].timer = time.AfterFunc(40*time.Millisecond, func() {
		fired <- struct{}{}
		ch.expireChoice("q-stop")
	})
	ch.choiceMu.Unlock()
	ch.dropChoicePending("q-stop")
	time.Sleep(80 * time.Millisecond)
	select {
	case <-fired:
		t.Fatal("timer should have been stopped so expire does not fire")
	default:
	}
}
