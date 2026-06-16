package telegram

import (
	"testing"
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
		botUserID:  12345,
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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ch.detectMention(tt.msg)
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
