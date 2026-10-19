package strutil

import "testing"

// 真实样本取自本地 thinkbot.db 的 user_message_events（2026-09-29），未改写结构。
// 其中 #2/#3 是井字棋对局：被回复帖正文（Bot 的话）**含换行**——这是必须覆盖的
// 真实边界，纯构造样本很容易漏掉。
//
// 纪律（本项目吃过亏）：构造样本只能证明「机制可能发生」，必须用真实数据复核。

func TestStripChannelContextMarkers_RealSamples(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "真实#1 单行被回复正文",
			in: "[Timeline] @luna: [Reply to 栞娜: @luna 天色暗下来的时候就是这种感觉！" +
				"要是不方便出门，雨停之后说不定会有鸟类出来活动，正好拍鸟喵～ 出门前记得带伞呀]\n" +
				"那忘记带伞怎么办\n[note_id: aq0ngtc8di7a00m7]",
			want: "那忘记带伞怎么办",
		},
		{
			name: "真实#2 多行被回复正文（井字棋）",
			in: "[Timeline] @luna: [Reply to 栞娜: @luna 来喵！我先下，占据中心 ✕： ``` 1 | 2 | 3\n" +
				"───┼───┼─── 4 │ ✕ │ 6\n" +
				"───┼───┼─── 7 | 8 | 9\n" +
				"``` 轮到你了，报个格子编号就行～（你是 ◯）]\n" +
				"4\n[note_id: aq0up57vdi7a00ms]",
			want: "4",
		},
		{
			name: "真实#3 多行被回复正文（对局续）",
			in: "[Timeline] @luna: [Reply to 栞娜: @luna 收到！你下 4 了喵～ ``` 1 | 2 | 3\n" +
				"───┼───┼─── ◯ │ ✕ │ 6\n" +
				"───┼───┼─── 7 | 8 | ✕\n" +
				"``` 那我下 9，占个对角喵！轮到你咯 ◯]\n" +
				"1\n[note_id: aq0upmyqdi7a00mw]",
			want: "1",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := StripChannelContextMarkers(tc.in)
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
			// 幂等：二次清洗结果不变
			if again := StripChannelContextMarkers(got); again != got {
				t.Fatalf("not idempotent: %q -> %q", got, again)
			}
		})
	}
}

func TestStripChannelContextMarkers_Combinations(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "无装饰原样返回",
			in:   "今天天气不错",
			want: "今天天气不错",
		},
		{
			name: "空串",
			in:   "",
			want: "",
		},
		{
			name: "仅 timeline 前缀",
			in:   "[Timeline] @alice: 大家好",
			want: "大家好",
		},
		{
			name: "Bot 账号前缀在最外层",
			in:   "[对方是 Bot 账号 @x] [Timeline] @alice: 大家好\n[note_id: n1]",
			want: "大家好",
		},
		{
			name: "DM 前缀",
			in:   "[DM] @bob: 在吗",
			want: "在吗",
		},
		{
			name: "Renote 包裹 Reply（Renote 在外）",
			in:   "[Renote from @c: 转发的原文]\n[Reply to 栞娜: bot 的话]\n用户正文",
			want: "用户正文",
		},
		{
			name: "纯引用转发且正文为空",
			in:   "[Renote from @c: 转发的原文]",
			want: "",
		},
		{
			name: "仅 Reply 且正文为空",
			in:   "[Reply to 栞娜: bot 的话]",
			want: "",
		},
		{
			name: "用户正文含方括号不被误删",
			in:   "[Reply to 栞娜: bot 的话]\n我觉得 [这个] 更好",
			want: "我觉得 [这个] 更好",
		},
		{
			name: "裸 note_id 剥离",
			in:   "用户正文\n[note_id: abc123]",
			want: "用户正文",
		},
		{
			name: "CRLF 换行",
			in:   "[Reply to 栞娜: bot 的话]\r\n用户正文",
			want: "用户正文",
		},
		{
			name: "被回复正文以 ] 收尾（边界）",
			in:   "[Reply to 栞娜: 前面 [1] ]\n用户正文",
			want: "用户正文",
		},
		{
			name: "未闭合前缀不匹配（保守保留）",
			in:   "[Reply to 栞娜: 缺个右括号\n用户正文",
			want: "[Reply to 栞娜: 缺个右括号\n用户正文",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := StripChannelContextMarkers(tc.in); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestStripChannelContextMarkersMastodon 覆盖 Mastodon 渠道注入的标记。
//
// Mastodon 的转发上下文叫 [Boost from ...]、帖子 ID 后缀叫 [status_id: ...]，
// 与 Misskey 的 [Renote from ...] / [note_id: ...] 命名不同。不覆盖的话
// status_id 会被当成用户原话写进 L0 记忆（见 context_markers.go 头部说明）。
func TestStripChannelContextMarkersMastodon(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "status_id 后缀被剥离",
			in:   "用户正文\n[status_id: 10987654321]",
			want: "用户正文",
		},
		{
			name: "Boost 上下文被剥离",
			in:   "[Boost from alice: 被转发的原文]\n用户正文",
			want: "用户正文",
		},
		{
			name: "完整 Mastodon 组合",
			in:   "[Timeline] @alice: [Boost from bob: x]\n[Reply to carol: y]\n用户正文\n[status_id: 1]",
			want: "用户正文",
		},
		{
			name: "用户正文自带的方括号保留",
			in:   "我觉得 [1] 是对的\n[status_id: 9]",
			want: "我觉得 [1] 是对的",
		},
		{
			name: "对方是 Bot 账号前缀 + status_id 后缀",
			in:   "[对方是 Bot 账号 @alice] 用户正文\n[status_id: 5]",
			want: "用户正文",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := StripChannelContextMarkers(tc.in); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}
