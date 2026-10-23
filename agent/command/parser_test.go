package command

import "testing"

// TestParseSkipsLeadingMention 群聊里命令必须写成 "@bot /cmd"，
// 而剥 @ 前缀依赖 channel 配置里的机器人显示名与实际群昵称一致
// （见 channel/wecom.stripMention）。一旦不一致就漏剥，命令识别会**静默**失效：
// 消息被当成普通聊天喂给 LLM，bot 回一句"我不认识"。
// 实测 2026-10-23 线上就是这个现象：配置 botName=maid，群里昵称是"打工仔"。
// 这里让 Parse 自己兜住，不再依赖上游剥得干不干净。
func TestParseSkipsLeadingMention(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		wantNil  bool
		wantName string
		wantArgs string
	}{
		{name: "mention then command", text: "@打工仔 /chatid", wantName: "chatid"},
		{name: "mention glued to command", text: "@打工仔/chatid", wantName: "chatid"},
		{name: "mention extra spaces", text: "@打工仔   /chatid", wantName: "chatid"},
		{name: "mention with args", text: "@bot /compact 5", wantName: "compact", wantArgs: "5"},
		{name: "fullwidth at", text: "＠打工仔 /chatid", wantName: "chatid"},
		{name: "two mentions", text: "@a @bot /chatid", wantName: "chatid"},
		{name: "leading space before mention", text: "  @bot /chatid", wantName: "chatid"},

		// 剥完 @ 不是命令 → 必须仍然放行给 LLM，不能吞掉普通群聊。
		{name: "mention then plain text", text: "@打工仔 你是啥工具弄得", wantNil: true},
		{name: "mention only", text: "@打工仔", wantNil: true},
		{name: "mention only trailing space", text: "@打工仔 ", wantNil: true},

		// @ 在句中不动：那不是提及前缀。
		{name: "at mid sentence", text: "群聊带 @ 的模式 /chatid", wantNil: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := Parse(tt.text)
			if tt.wantNil {
				if result != nil {
					t.Errorf("Parse(%q) = %+v, want nil", tt.text, result)
				}
				return
			}
			if result == nil {
				t.Fatalf("Parse(%q) = nil, want non-nil", tt.text)
			}
			if result.Name != tt.wantName {
				t.Errorf("Name = %q, want %q", result.Name, tt.wantName)
			}
			if result.Args != tt.wantArgs {
				t.Errorf("Args = %q, want %q", result.Args, tt.wantArgs)
			}
		})
	}
}
