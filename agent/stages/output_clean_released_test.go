package stages

import (
	"strings"
	"testing"
)

// 本文件钉死「已提前放行的 <public> 区块」在流关闭时的归属（2026-10-12 复查 8d54404）。
//
// 流式为了实时性，在当前区块归一化前缀满 publicDupPrefixRunes 字且与已推送块不相似时
// 就先推给用户（released），此时它还没记入 kept。闭合时必须无条件记入——内容已经推出去了，
// 再判一次重复只会让「用户在 Web 上看到的」与「落库 / 渠道发出的」对不上。

// longPublicBlock 造一个归一化后远超 publicDupPrefixRunes(48) 字的区块，
// 使流式能在闭合前就做出「不是重复」的判定并提前放行。
func longPublicBlock(seed string) string {
	var b strings.Builder
	for i := 0; i < 6; i++ {
		b.WriteString(seed)
		b.WriteString("这是第")
		b.WriteString(string(rune('一' + i)))
		b.WriteString("段需要足够长才能触发提前放行的公开正文。")
	}
	return b.String()
}

func TestStreamReleasedBlockIsKeptAndLaterDuplicateDropped(t *testing.T) {
	block := longPublicBlock("栞娜先说明一下背景，")
	raw := "<public>" + block + "</public><public>" + block + "</public>"

	// 流式（Web 实时推送）
	got, _ := streamClean([]string{raw}, true)
	if strings.Count(got, block) != 1 {
		t.Fatalf("identical blocks must be streamed once, got %d occurrences:\n%s", strings.Count(got, block), got)
	}
	// 门控（落库 / 渠道发送）必须与之完全一致
	if want := sharedClean(raw); want != got {
		t.Fatalf("stream and gate disagree:\nstream=%q\ngate  =%q", got, want)
	}
}

func TestStreamReleasedBlockParityAcrossChunkings(t *testing.T) {
	block := longPublicBlock("先记一下要点，")
	raw := "<public>" + block + "</public>"
	want := sharedClean(raw)
	if want != block {
		t.Fatalf("gate output = %q, want the single block", want)
	}
	// 无论怎么切分，流式最终文本必须与门控一致（逐字切片是最极端的切法）。
	for _, chunks := range runeSplits(raw) {
		if got, _ := streamClean(chunks, true); got != want {
			t.Fatalf("chunking %d splits: stream=%q want=%q", len(chunks), got, want)
		}
	}
	if got, _ := streamClean(perRune(raw), true); got != want {
		t.Fatalf("per-rune stream = %q, want %q", got, want)
	}
}
