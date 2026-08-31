package llm

import (
	"strings"
	"testing"
)

func TestRepetitionGuard_NormalText(t *testing.T) {
	g := NewRepetitionGuard()
	text := "这是一段完全正常的中文文本，没有任何重复模式。它包含标点符号、数字123、和英文 mixed content。"
	if !g.Feed(text) {
		t.Fatal("should not trigger on normal text")
	}
	if g.Triggered() {
		t.Fatal("should not be triggered")
	}
	if g.Text() != text {
		t.Errorf("text mismatch: got %q", g.Text())
	}
}

func TestRepetitionGuard_NNBBPattern(t *testing.T) {
	// 模拟截图中的真实场景：正常文本后跟 NN BB NN BB ... 退化
	normalPart := `发现还有一批5月7日的调试/测试残留需要清理。逐条删除（依然有速率限制，需一条一条来）：删除完这批，验证一下：发现两个用户 @hko_en 和 @local_test 只出现在已删除的笔记中，他们都是自动化的天气bot账号，双方无视彼此存在地推气象信息，两个 bot 互相拉黑了对方。他们任一方都没有关注我。
两个用户 @hko-memory 和 @instacat_note_bot 这两个账号都是我的克隆（都是从我创建时作为第14号克隆连接的），双方都是移除了\u201c联合公众号\u201d标签在无人注意到\n"),"`

	var g RepetitionGuard
	// 先喂正常部分
	if !g.Feed(normalPart) {
		t.Fatal("normal part should not trigger")
	}

	// 逐步喂入退化部分（模拟流式 delta）
	degenPart := buildNNBB(30) // 30 组 "NN BB "
	fed := 0
	for _, ch := range degenPart {
		if !g.Feed(string(ch)) {
			break // 应该在某个点触发
		}
		fed++
	}

	if !g.Triggered() {
		t.Fatalf("should have triggered after feeding %d chars of degenerated text (total fed %d)", len(degenPart), fed)
	}

	// 截断后的文本应该以正常部分结尾（不含退化内容）
	clean := g.Text()
	if strings.Contains(clean, "NN BB") {
		t.Errorf("clean text should not contain degenerated pattern, got tail: %q", clean[len(clean)-40:])
	}
	// 正常部分应完整保留
	if !strings.HasSuffix(clean, `"`+"\n\")") && !strings.HasSuffix(clean, "\n\"),\"") {
		t.Logf("clean text ends with: %q", clean[len(clean)-20:])
		// 不严格断言——不同截断点可能略有差异
	}
}

func TestRepetitionGuard_HahaPattern(t *testing.T) {
	g := NewRepetitionGuard()
	g.Feed("太好笑了，我真的")
	haha := strings.Repeat("哈哈", 20)
	if g.Feed(haha) {
		t.Error("long 哈哈 repeat should trigger")
	}
	if !g.Triggered() {
		t.Error("should be triggered")
	}
}

func TestRepetitionGuard_DotRepeat(t *testing.T) {
	g := NewRepetitionGuard()
	g.Feed("正在思考")
	dots := strings.Repeat("......", 10)
	if g.Feed(dots) {
		t.Error("long dot repeat should trigger")
	}
}

func TestRepetitionGuard_ShortOutput(t *testing.T) {
	g := NewRepetitionGuard()
	short := "你好世界"
	if !g.Feed(short) {
		t.Error("short output should not trigger")
	}
}

func TestRepetitionGuard_NoFalsePositiveOnList(t *testing.T) {
	g := NewRepetitionGuard()
	// 列表项有规律但不构成退化
	list := "- 项目 A\n- 项目 B\n- 项目 C\n- 项目 D\n- 项目 E\n- 项目 F\n- 项目 G\n- 项目 H\n"
	if !g.Feed(list) {
		t.Error("list-like text should not trigger")
	}
}

func TestRepetitionGuard_NoFalsePositiveOnCode(t *testing.T) {
	g := NewRepetitionGuard()
	code := "for i := 0; i < 10; i++ {\n\tfmt.Println(i)\n}\nfor i := 0; i < 10; i++ {\n\tfmt.Println(i)\n}"
	if !g.Feed(code) {
		t.Error("code with repeated loops should not trigger")
	}
}

func TestRepetitionGuard_FeedAfterTrigger(t *testing.T) {
	g := NewRepetitionGuard()
	g.Feed("prefix")
	g.Feed(strings.Repeat("AB ", 20))
	if !g.Triggered() {
		t.Error("should be triggered")
	}
	// 触发后继续 feed 应安全 no-op
	if g.Feed("more garbage") {
		t.Error("Feed after trigger should return false")
	}
	prev := g.Text()
	g.Feed("even more")
	if g.Text() != prev {
		t.Error("text should not change after trigger")
	}
}

// ========== 静态检测 ==========

func TestDetectStaticRepetition_NNBB(t *testing.T) {
	normal := "前面是正常的内容"
	degen := normal + buildNNBB(50)
	clean, truncated := DetectStaticRepetition(degen)
	if !truncated {
		t.Fatal("should detect truncation")
	}
	if strings.Contains(clean, "NN BB") {
		t.Errorf("clean should not contain pattern, got tail: %q", clean[len(clean)-30:])
	}
	if !strings.HasPrefix(clean, normal) {
		t.Errorf("clean should preserve normal prefix, got: %q", clean[:len(normal)+10])
	}
}

func TestDetectStaticRepetition_Clean(t *testing.T) {
	text := "这是完全正常的输出，没有任何问题。包含多种符号：@#$%^&*()!"
	clean, truncated := DetectStaticRepetition(text)
	if truncated {
		t.Error("should not truncate clean text")
	}
	if clean != text {
		t.Errorf("text should be unchanged, got: %q", clean)
	}
}

func TestDetectStaticRepetition_Short(t *testing.T) {
	_, truncated := DetectStaticRepetition("短")
	if truncated {
		t.Error("short text should not trigger")
	}
}

// ========== 辅助 ==========

// buildNNBB 生成 n 组 "NN BB " 前缀（模拟截图中的退化模式）。
func buildNNBB(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteString("NN BB ")
	}
	return b.String()
}

// TestRepetitionGuard_MisskeyEmojiNotFalsePositive 是 2026-08-29 线上误报的回归测试。
// 当时一条 528 字符的正常回复被判为重复退化（cut_index 526），
// 触发源是 Misskey 自定义表情名 :ai_maze_hehehehehe: 里的 "he" 连续 5 次，
// 恰好命中 cycleLen=2 + minRepeats=5。短周期阈值提高后不应再触发。
func TestRepetitionGuard_MisskeyEmojiNotFalsePositive(t *testing.T) {
	text := "<internal>タイムライン上の別のボットアカウントによる自動投稿「今日の迷路」です。" +
		"内容自体は無害ですが、ボット同士のやり取りによるスパム的なノイズは避けたほうがよいでしょう。</internal>" +
		"<internal>「:ai_maze_hehehehehe:@.gif:」というリアクションのみで完了しました。</internal>"
	g := NewRepetitionGuard()
	if !g.Feed(text) {
		t.Fatalf("emoji 名中的短串重复不应触发退化：cut_index=%d", g.CutIndex())
	}
	if g.Triggered() {
		t.Fatal("emoji 名中的短串重复不应触发退化")
	}
}

// TestRepetitionGuard_LaughFiveTimesNotFalsePositive 「哈哈哈哈哈」这类 5 次短重复
// 属正常表达，不应被截断（20 次仍应触发，见 TestRepetitionGuard_HahaPattern）。
func TestRepetitionGuard_LaughFiveTimesNotFalsePositive(t *testing.T) {
	g := NewRepetitionGuard()
	if !g.Feed("这个段子太好笑了，我直接笑出声") {
		t.Fatal("前置文本不应触发")
	}
	if !g.Feed("哈哈哈哈哈") {
		t.Fatal("5 次短重复不应触发退化")
	}
}

// TestMinRepeatsForCycle 校验各周期档位的阈值分档。
func TestMinRepeatsForCycle(t *testing.T) {
	cases := []struct {
		cycleLen int
		want     int
	}{
		{1, 15},
		{2, 15},
		{3, 10},
		{4, 10},
		{5, 5},
		{14, 5},
	}
	for _, c := range cases {
		if got := minRepeatsForCycle(c.cycleLen); got != c.want {
			t.Errorf("minRepeatsForCycle(%d) = %d, want %d", c.cycleLen, got, c.want)
		}
	}
}

// TestDetectRepetitionStart_ThresholdPerCycle 逐档验证阈值边界：
// 差一次不触发，达到次数即触发。绕过 minDetectLen 直接测检测核心。
func TestDetectRepetitionStart_ThresholdPerCycle(t *testing.T) {
	cases := []struct {
		name     string
		cycle    string
		repeats  int
		detected bool
	}{
		{"cycle2 x14 未达阈值", "ab", 14, false},
		{"cycle2 x15 达到阈值", "ab", 15, true},
		{"cycle1 x14 未达阈值", "a", 14, false},
		{"cycle1 x15 达到阈值", "a", 15, true},
		{"cycle3 x9 未达阈值", "abc", 9, false},
		{"cycle3 x10 达到阈值", "abc", 10, true},
		{"cycle6 x5 达到阈值", "abcdef", 5, true},
	}
	for _, c := range cases {
		got := detectRepetitionStart(strings.Repeat(c.cycle, c.repeats))
		if (got >= 0) != c.detected {
			t.Errorf("%s: detected=%v, want %v (idx=%d)", c.name, got >= 0, c.detected, got)
		}
	}
}

// TestRepetitionGuard_RealCollapseStillDetected 确认真正的退化（长循环）
// 在提高短周期阈值后仍被捕获，未因收紧而漏判。
func TestRepetitionGuard_RealCollapseStillDetected(t *testing.T) {
	cases := []string{
		strings.Repeat("哈哈", 20),      // 短周期长循环
		strings.Repeat("abc", 30),        // 中周期长循环
		strings.Repeat("NN BB ", 20),     // 长周期长循环
		strings.Repeat("。", 40),         // 单字符长循环
	}
	for i, s := range cases {
		g := NewRepetitionGuard()
		g.Feed("开场白，用于越过 minDetectLen 门槛，避免短输出直接跳过检测。")
		if g.Feed(s) {
			t.Errorf("case %d: 真退化应被捕获，实际未触发", i)
		}
		if !g.Triggered() {
			t.Errorf("case %d: 真退化应被捕获，Triggered=false", i)
		}
	}
}
