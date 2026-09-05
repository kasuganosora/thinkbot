package stages

import "testing"

// TestExtractPublicReplyTakesFirstBlock 验证：多个 <public> 块（模型不合法输出）
// 只保留第一个有效块，丢弃其后所有块——无论后续块是否为宽度变体重复。
func TestExtractPublicReplyTakesFirstBlock(t *testing.T) {
	in := "<public>喵~6/6全对!💯 这是给谁复习呀🤔</public>" +
		"<public>喵~6/6全对！💯 这是给谁复习呀🤔</public>"
	got := extractPublicReply(in)
	want := "喵~6/6全对!💯 这是给谁复习呀🤔"
	if got != want {
		t.Fatalf("多块应只取第一个，得到 %q，期望 %q", got, want)
	}
}

// TestExtractPublicReplySingleBlock 验证：单个 <public> 块行为不变。
func TestExtractPublicReplySingleBlock(t *testing.T) {
	in := "<public>只有一段回复</public>"
	if got := extractPublicReply(in); got != "只有一段回复" {
		t.Fatalf("单块应原样返回，得到 %q", got)
	}
}

// TestExtractPublicReplySkipsEmptyBlock 验证：首个块为空时跳过，取下一个非空块，
// 避免空块吞掉真实回复。
func TestExtractPublicReplySkipsEmptyBlock(t *testing.T) {
	in := "<public>   </public><public>真正的回复</public>"
	if got := extractPublicReply(in); got != "真正的回复" {
		t.Fatalf("应跳过空块取下一个，得到 %q", got)
	}
}

// TestExtractPublicReplyStripsInternal 验证：<public> 块内夹带的 <internal> 心里话不外发。
func TestExtractPublicReplyStripsInternal(t *testing.T) {
	in := "<public>公开内容<internal>私密想法</internal>收尾</public>"
	if got := extractPublicReply(in); got != "公开内容收尾" {
		t.Fatalf("internal 应被剥离，得到 %q", got)
	}
}
