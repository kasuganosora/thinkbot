package tools

import "testing"

func TestClassifyTaskIntent(t *testing.T) {
	cases := []struct {
		text string
		want string
	}{
		// 代码 / 文件任务：必须不命中（隐藏社交写工具，从架构上根绝跑偏）
		{"给cfblog加上无障碍树支持 完成后推送到远端", ""},
		{"帮我修一下这个bug", ""},
		{"重构这段逻辑", ""},
		{"查看 layout.js 的可访问性", ""},
		{"帮我看下代码有什么问题", ""},
		// 模糊 / 日常：默认隐藏（不误杀以 social 关键词命中的正常社交）
		{"在吗", ""},
		{"谢谢", ""},
		{"", ""},
		// 明确社交操作：命中关键词，暴露社交写工具
		{"关注一下 kanna", TaskIntentSocial},
		{"帮我取关某人", TaskIntentSocial},
		{"发条动态说今天天气真好", TaskIntentSocial},
		{"发个帖子介绍一下项目", TaskIntentSocial},
		{"给这条点赞", TaskIntentSocial},
		{"renote 这条", TaskIntentSocial},
		{"转发这条消息", TaskIntentSocial},
		{"私信告诉她", TaskIntentSocial},
		{"@kanna 在吗", TaskIntentSocial},
		{"follow alice on misskey", TaskIntentSocial},
		{"帮我 publish 一条", TaskIntentSocial},
	}
	for _, c := range cases {
		if got := ClassifyTaskIntent(c.text); got != c.want {
			t.Errorf("ClassifyTaskIntent(%q) = %q, want %q", c.text, got, c.want)
		}
	}
}

func TestToolDef_appliesTo_SocialScope(t *testing.T) {
	socialTool := ToolDef{Scopes: []string{"social"}}

	cases := []struct {
		name string
		sctx *ToolSessionContext
		want bool
	}{
		{
			name: "社交意图的用户会话 → 暴露",
			sctx: &ToolSessionContext{TaskIntent: TaskIntentSocial, SourceChannelType: "misskey"},
			want: true,
		},
		{
			name: "代码任务（未知意图）的用户会话 → 隐藏",
			sctx: &ToolSessionContext{TaskIntent: "", SourceChannelType: "misskey"},
			want: false,
		},
		{
			name: "非社交意图 → 隐藏",
			sctx: &ToolSessionContext{TaskIntent: TaskIntentNonsocial},
			want: false,
		},
		{
			name: "系统会话（心跳/cron 主动发帖）兜底放行",
			sctx: &ToolSessionContext{TaskIntent: "", IsSystem: true},
			want: true,
		},
		{
			name: "子代理（workflow 内部执行）隐藏社交写工具",
			sctx: &ToolSessionContext{TaskIntent: "", IsSubagent: true},
			want: false,
		},
		{
			name: "子代理 + 社交意图仍隐藏（防止套娃/跑偏）",
			sctx: &ToolSessionContext{TaskIntent: TaskIntentSocial, IsSubagent: true},
			want: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := socialTool.appliesTo(c.sctx); got != c.want {
				t.Errorf("appliesTo = %v, want %v", got, c.want)
			}
		})
	}
}

func TestToolDef_appliesTo_SocialScopeDoesNotAffectOtherScopes(t *testing.T) {
	// 回归保护：social scope 的引入不能破坏现有空 scope / private / group 逻辑。
	emptyTool := ToolDef{Scopes: []string{}}
	if !emptyTool.appliesTo(&ToolSessionContext{}) {
		t.Error("empty-scope tool should be visible everywhere")
	}
	privateTool := ToolDef{Scopes: []string{"private"}}
	if !privateTool.appliesTo(&ToolSessionContext{ChatType: "private"}) {
		t.Error("private tool should be visible in private chat")
	}
	if privateTool.appliesTo(&ToolSessionContext{ChatType: "group"}) {
		t.Error("private tool should be hidden in group chat")
	}
}
