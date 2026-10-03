package memory

import "testing"

func TestBanTopic(t *testing.T) {
	if got := BanTopic("你再提科目二 我就删掉你的记忆"); got != "科目二" {
		t.Fatalf("got %q", got)
	}
	if got := BanTopic("好了严谨你再提单身的事情 不然我要删库了"); got != "单身" {
		t.Fatalf("got %q", got)
	}
	if BanTopic("今天天气不错") != "" {
		t.Fatal("ordinary text should not ban")
	}
}

func TestFilterSuppressed(t *testing.T) {
	entries := []Entry{
		{Content: "科目二挂了", Category: "observation"},
		{Content: "喜欢黑白女仆装", Category: "observation"},
		{Content: "科目二", Category: suppressCategory},
	}
	got := FilterSuppressed(entries, []string{"科目二"})
	if len(got) != 1 || got[0].Content != "喜欢黑白女仆装" {
		t.Fatalf("got %#v", got)
	}
}
