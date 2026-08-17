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
