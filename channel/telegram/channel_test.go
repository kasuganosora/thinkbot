package telegram

import (
	"testing"
	"time"
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
