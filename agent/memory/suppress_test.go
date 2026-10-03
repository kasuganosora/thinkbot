package memory

import (
	"strings"
	"testing"
)

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

func TestFilterUnrelatedDropsScar(t *testing.T) {
	entries := []Entry{{Content: "科目二挂了"}, {Content: "thinkbot 是多层记忆"}}
	got := FilterUnrelated(entries, "我们继续看记忆系统怎么改")
	if len(got) != 1 || !strings.Contains(got[0].Content, "记忆") {
		t.Fatalf("got %#v", got)
	}
	kept := FilterUnrelated(entries, "科目二后来怎么样")
	if len(kept) != 1 || kept[0].Content != "科目二挂了" {
		t.Fatalf("asked topic should stay: %#v", kept)
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
