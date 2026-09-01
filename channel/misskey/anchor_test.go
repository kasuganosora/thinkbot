package misskey

import "testing"

// TestSetMentionAnchor_Monotonic 回归 2026-09-01 生产事故：
// timeline 路径处理的 mention 原先不推进锚点，且锚点允许被旧 ID 回退——
// 两次断连 backfill 以陈旧锚点 sinceId 拉取，把整晚 mention 重放两轮，
// 同一帖子被重复生成回复、重复外发、重复写记忆。
// 修复后：锚点单调递增（Misskey aid 定长、按时间字典序递增）。
func TestSetMentionAnchor_Monotonic(t *testing.T) {
	c := &MisskeyChannel{}

	// 空值不更新。
	c.setMentionAnchor("")
	if got := c.getMentionAnchor(); got != "" {
		t.Fatalf("empty noteID should not set anchor, got %q", got)
	}

	// 首次设置（种子锚点）。
	c.setMentionAnchor("aqggs2iwdi7a01hx")
	if got := c.getMentionAnchor(); got != "aqggs2iwdi7a01hx" {
		t.Fatalf("seed anchor = %q", got)
	}

	// 向更新的 ID 推进。
	c.setMentionAnchor("aqm92gvjdi7a01qy")
	if got := c.getMentionAnchor(); got != "aqm92gvjdi7a01qy" {
		t.Fatalf("advance to newer = %q", got)
	}

	// 旧 ID 不得回退锚点（乱序到达/backfill 并发防护）。
	c.setMentionAnchor("aqggs2iwdi7a01hx")
	if got := c.getMentionAnchor(); got != "aqm92gvjdi7a01qy" {
		t.Fatalf("older ID must not move anchor backwards, got %q", got)
	}

	// 相同 ID 幂等。
	c.setMentionAnchor("aqm92gvjdi7a01qy")
	if got := c.getMentionAnchor(); got != "aqm92gvjdi7a01qy" {
		t.Fatalf("same ID should be a no-op, got %q", got)
	}
}

// TestTimelineMentionAdvancesAnchor 验证 timeline 路径上「指向本 Bot 的帖子」
// 在成功处理后推进 mention 锚点的判定条件：timelineMentioned 为 true 的帖子
// 才会推进（此处直接锚定判定函数与锚点的联动语义，流处理分支调用顺序见
// channel.go timeline note 分支注释）。
func TestTimelineMentionAdvancesAnchor(t *testing.T) {
	c := &MisskeyChannel{botUsername: "kanna"}
	c.mentionRe = testMentionRe("kanna")
	c.botUserID = "bot-self"

	// 普通时间线帖（未指向 Bot）：不得推进锚点。
	plain := Note{ID: "aqm0000000000000aa", Text: "今天天气不错", User: User{ID: "u1", Username: "luna"}}
	if c.timelineMentioned(plain) {
		t.Fatalf("plain timeline note should not count as mention")
	}

	// 字面 @Bot 的时间线帖：判定为 mention，调用方据此推进锚点。
	mention := Note{ID: "aqm0000000000000bb", Text: "@kanna 在吗", User: User{ID: "u1", Username: "luna"}}
	if !c.timelineMentioned(mention) {
		t.Fatalf("@bot timeline note should count as mention")
	}
	// 模拟 channel.go timeline 分支的联动：mentioned → setMentionAnchor。
	if c.timelineMentioned(plain) {
		c.setMentionAnchor(plain.ID)
	}
	if got := c.getMentionAnchor(); got != "" {
		t.Fatalf("non-mention must not advance anchor, got %q", got)
	}
	if c.timelineMentioned(mention) {
		c.setMentionAnchor(mention.ID)
	}
	if got := c.getMentionAnchor(); got != mention.ID {
		t.Fatalf("mention via timeline must advance anchor, got %q", got)
	}
}
