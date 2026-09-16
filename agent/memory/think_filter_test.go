package memory

import (
	"context"
	"strings"
	"testing"
)

// ============================================================================
// StripThinkTags
// ============================================================================

func TestStripThinkTags_Complete(t *testing.T) {
	input := "<think>I need to analyze this carefully</think>Hello world"
	want := "Hello world"
	got := StripThinkTags(input)
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestStripThinkTags_Thinking(t *testing.T) {
	input := "<thinking>Let me think about this</thinking>Final answer"
	want := "Final answer"
	got := StripThinkTags(input)
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestStripThinkTags_Multiline(t *testing.T) {
	input := "<think>\nStep 1: Read input\nStep 2: Process\n</think>\nThe result is 42"
	want := "The result is 42"
	got := StripThinkTags(input)
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestStripThinkTags_MultipleBlocks(t *testing.T) {
	input := "<think>reasoning 1</think>Answer A<think>more reasoning</think>Answer B"
	want := "Answer AAnswer B"
	got := StripThinkTags(input)
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestStripThinkTags_CaseInsensitive(t *testing.T) {
	cases := []string{
		"<Think>secret</Think>visible",
		"<THINKING>secret</THINKING>visible",
		"<ThInK>secret</ThInK>visible",
	}
	for _, input := range cases {
		got := StripThinkTags(input)
		if got != "visible" {
			t.Errorf("case-insensitive: got %q, want %q", got, "visible")
		}
	}
}

func TestStripThinkTags_OnlyThink(t *testing.T) {
	input := "<think>This is all reasoning, no actual answer</think>"
	got := StripThinkTags(input)
	if got != "" {
		t.Errorf("expected empty string, got %q", got)
	}
}

func TestStripThinkTags_UnclosedTag(t *testing.T) {
	input := "Hello<think>this was cut off"
	want := "Hello"
	got := StripThinkTags(input)
	if got != want {
		t.Errorf("unclosed tag: got %q, want %q", got, want)
	}
}

func TestStripThinkTags_NoTags(t *testing.T) {
	input := "Just normal text without any tags"
	got := StripThinkTags(input)
	if got != input {
		t.Errorf("untagged text should be unchanged: got %q, want %q", got, input)
	}
}

func TestStripThinkTags_EmptyString(t *testing.T) {
	got := StripThinkTags("")
	if got != "" {
		t.Errorf("empty input should return empty, got %q", got)
	}
}

func TestStripThinkTags_PreservesContentBetweenTags(t *testing.T) {
	input := "Before<think>reasoning</think>Middle<think>more</think>After"
	want := "BeforeMiddleAfter"
	got := StripThinkTags(input)
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// ============================================================================
// StripReasoningArray
// ============================================================================

func TestStripReasoningArray_ValidArray(t *testing.T) {
	input := `[{"text":"internal reasoning here","type":"reasoning"},{"text":"final answer","type":"text"}]`
	want := "final answer"
	got := StripReasoningArray(input)
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestStripReasoningArray_MultipleTextParts(t *testing.T) {
	input := `[{"text":"thinking...","type":"reasoning"},{"text":"part1","type":"text"},{"text":"part2","type":"text"}]`
	want := "part1\npart2"
	got := StripReasoningArray(input)
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestStripReasoningArray_NotAnArray(t *testing.T) {
	input := "Just a normal message"
	got := StripReasoningArray(input)
	if got != input {
		t.Errorf("non-array input should be unchanged: got %q", got)
	}
}

func TestStripReasoningArray_OnlyReasoning(t *testing.T) {
	input := `[{"text":"only reasoning","type":"reasoning"}]`
	want := ""
	got := StripReasoningArray(input)
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestStripReasoningArray_NoReasoningType(t *testing.T) {
	// Array without any reasoning type → return as-is
	input := `[{"text":"just text","type":"text"}]`
	got := StripReasoningArray(input)
	if got != input {
		t.Errorf("array without reasoning type should be unchanged: got %q", got)
	}
}

func TestStripReasoningArray_InvalidJSON(t *testing.T) {
	input := "[{invalid json}]"
	got := StripReasoningArray(input)
	if got != input {
		t.Errorf("invalid JSON should be unchanged: got %q", got)
	}
}

// ============================================================================
// StripThinking (组合)
// ============================================================================

func TestStripThinking_Combined(t *testing.T) {
	input := `[{"text":"reasoning","type":"reasoning"},{"text":"<think>still thinking</think>actual content","type":"text"}]`
	want := "actual content"
	got := StripThinking(input)
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestStripThinking_PlainText(t *testing.T) {
	input := "Hello, this is a normal response."
	got := StripThinking(input)
	if got != input {
		t.Errorf("plain text should be unchanged: got %q", got)
	}
}

// ============================================================================
// ThinkFilterStore
// ============================================================================

func TestThinkFilterStore_StripsOnAppend(t *testing.T) {
	repo := NewMemoryRepository()
	store := NewThinkFilterStore(repo)

	entry := Entry{
		Scope:   ChannelScope("test"),
		Content: "<think>internal reasoning</think>actual memory content",
	}

	if err := store.Append(context.Background(), entry); err != nil {
		t.Fatal(err)
	}

	results, err := repo.Recent(context.Background(), ChannelScope("test"), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(results))
	}
	if results[0].Content != "actual memory content" {
		t.Errorf("content not filtered: got %q", results[0].Content)
	}
}

// ============================================================================
// IsTrivialMemoryContent
// ============================================================================

func TestIsTrivialMemoryContent_CJK(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"好的", true},       // 2 字符，过短
		{"哈哈哈", true},      // 3 字符，过短
		{"今天天气不错", false},  // 6 字符，CJK 逐字成词=6，保留
		{"用户喜欢喝咖啡", false}, // 7 字符，保留
		{" 嗯  ", true},     // 去空白后 1 字符，过短
	}
	for _, c := range cases {
		if got := IsTrivialMemoryContent(c.in); got != c.want {
			t.Errorf("IsTrivialMemoryContent(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestIsTrivialMemoryContent_English(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"ok", true},                      // 1 词，过短
		{"thanks", true},                  // 单长单词仍按词数<5 丢弃
		{"thanks a lot", true},            // 2 词，过短
		{"this is a good idea", false},    // 5 词，保留
		{"the cat sat on the mat", false}, // 6 词，保留
	}
	for _, c := range cases {
		if got := IsTrivialMemoryContent(c.in); got != c.want {
			t.Errorf("IsTrivialMemoryContent(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestIsTrivialMemoryContent_MixedAndWhitespace(t *testing.T) {
	// 中英混排：CJK 逐字成词补足词数，"今天天气不错"(6 词)+"hello"(1 词)=7 词，应保留。
	if IsTrivialMemoryContent("今天天气不错 hello") {
		t.Errorf("mixed CJK+English with enough CJK should be kept")
	}
	// 纯空白应判定为琐碎。
	if !IsTrivialMemoryContent("   ") {
		t.Errorf("whitespace-only content should be trivial")
	}
}

// TestIsTrivialMemoryContent_BotSpam 覆盖真实库内噪声检测器：
// 投票 bot 刷屏、表情短码主导、短内容重复符号主导，以及长内容保护。
func TestIsTrivialMemoryContent_BotSpam(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		// Misskey 投票 bot 提问模板 → 琐碎
		{"みなさんは、どれが体に良さそうだと思いますか？", true},
		// 投票 bot 无投票回声 → 琐碎
		{"[Renote from 藍: みなさんは、どれが体に良さそうだと思いますか？]\n投票はありませんでした", true},
		// 表情短码主导（实义字 <=2）→ 琐碎
		{":panpan::panpan:…", true},
		{":usuhoso::bia_doya:☝️☝️", true},
		// 短内容重复符号主导（纯情绪）→ 琐碎
		{"ふえええええ！？", true},
		{"哈哈哈哈", true},
		{"。。。。。", true},
		// 长内容即使含重复符号也保留（保护带情绪合法长文）
		{"うぉ〜〜！！！気まぐれにSEKIROやったら弦一郎直前まで進められたぞ！！！！\n居合の達人雑魚すぎぃ！！！", false},
		// 正常中等长度记忆保留
		{"@blogtalk 分享关于梁羽生武侠小说的详细阅读指南和历史背景", false},
		{"这个用户玩刀剑乱舞，对高强度的肝活动感到非常疲惫", false},
		// 表情短码但仍有实义正文 → 保留
		{"@nukui 发布了新作 :art: 一幅很棒的风景画", false},
	}
	for _, c := range cases {
		if got := IsTrivialMemoryContent(c.in); got != c.want {
			t.Errorf("IsTrivialMemoryContent(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestThinkFilterStore_StripsReasoningArray(t *testing.T) {
	repo := NewMemoryRepository()
	store := NewThinkFilterStore(repo)

	entry := Entry{
		Scope:   ChannelScope("test"),
		Content: `[{"text":"reasoning","type":"reasoning"},{"text":"clean content","type":"text"}]`,
	}

	_ = store.Append(context.Background(), entry)

	results, _ := repo.Recent(context.Background(), ChannelScope("test"), 1)
	if len(results) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(results))
	}
	if results[0].Content != "clean content" {
		t.Errorf("content not filtered: got %q", results[0].Content)
	}
}

func TestThinkFilterStore_PassthroughDelete(t *testing.T) {
	repo := NewMemoryRepository()
	store := NewThinkFilterStore(repo)

	_ = store.Append(context.Background(), Entry{
		Scope:   ChannelScope("test"),
		Content: "hello",
	})

	results, _ := repo.Recent(context.Background(), ChannelScope("test"), 1)
	if len(results) != 1 {
		t.Fatalf("expected 1 entry before delete, got %d", len(results))
	}

	_ = store.Delete(context.Background(), ChannelScope("test"), results[0].ID)

	results, _ = repo.Recent(context.Background(), ChannelScope("test"), 1)
	if len(results) != 0 {
		t.Errorf("expected 0 entries after delete, got %d", len(results))
	}
}

func TestThinkFilterStore_PassthroughClear(t *testing.T) {
	repo := NewMemoryRepository()
	store := NewThinkFilterStore(repo)

	_ = store.Append(context.Background(), Entry{Scope: ChannelScope("test"), Content: "a"})
	_ = store.Append(context.Background(), Entry{Scope: ChannelScope("test"), Content: "b"})

	_ = store.Clear(context.Background(), ChannelScope("test"))

	results, _ := repo.Recent(context.Background(), ChannelScope("test"), 10)
	if len(results) != 0 {
		t.Errorf("expected 0 entries after clear, got %d", len(results))
	}
}

func TestThinkFilterStore_EmptyAfterStrip(t *testing.T) {
	repo := NewMemoryRepository()
	store := NewThinkFilterStore(repo)

	// Content is entirely think tags → after stripping it's empty
	_ = store.Append(context.Background(), Entry{
		Scope:   ChannelScope("test"),
		Content: "<think>only reasoning, nothing useful</think>",
	})

	results, _ := repo.Recent(context.Background(), ChannelScope("test"), 1)
	if len(results) != 1 {
		t.Fatalf("expected 1 entry (empty content still stored), got %d", len(results))
	}
	if results[0].Content != "" {
		t.Errorf("expected empty content after strip, got %q", results[0].Content)
	}
}

// 验证大段中文 think 标签被正确处理
func TestStripThinkTags_ChineseContent(t *testing.T) {
	input := `<think>用户问的是Go语言的并发模型，我需要解释goroutine和channel</think>Go语言的并发主要通过goroutine和channel实现。`
	want := "Go语言的并发主要通过goroutine和channel实现。"
	got := StripThinkTags(input)
	if !strings.Contains(got, want) {
		t.Errorf("got %q, want to contain %q", got, want)
	}
}
