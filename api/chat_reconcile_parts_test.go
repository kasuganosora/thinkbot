package api

import (
	"reflect"
	"testing"
)

func textPart(s string) map[string]any { return map[string]any{"type": "text", "content": s} }
func toolPart(id string) map[string]any {
	return map[string]any{"type": "tool", "id": id, "name": "search", "status": "success"}
}

// TestReconcileTextParts_KeepsInterleavingWhenStreamMatches：流式文本与最终出站内容一致时，
// 保留文本 / 工具交错顺序，只清掉空白片段与片段首尾空白。
func TestReconcileTextParts_KeepsInterleavingWhenStreamMatches(t *testing.T) {
	parts := []map[string]any{textPart("我查一下"), toolPart("c1"), textPart("\n\n"), toolPart("c2"), textPart("\n结果是 42。")}
	got := reconcileTextParts(parts, "我查一下\n结果是 42。")
	want := []map[string]any{textPart("我查一下"), toolPart("c1"), toolPart("c2"), textPart("结果是 42。")}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}
}

// TestReconcileTextParts_ReplacesDivergingText：流式预览与最终内容有出入
// （如 <public> 之前已推送的裸文本、被剥离的内部指标）时，文本以 canonical 为准，工具片段保留。
func TestReconcileTextParts_ReplacesDivergingText(t *testing.T) {
	parts := []map[string]any{textPart("前言"), toolPart("c1"), textPart("公开回复")}
	got := reconcileTextParts(parts, "公开回复")
	want := []map[string]any{toolPart("c1"), textPart("公开回复")}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}

	// 流式文本不完整（缺尾部）也按 canonical 兜底。
	got = reconcileTextParts([]map[string]any{textPart("你好")}, "你好，世界")
	if !reflect.DeepEqual(got, []map[string]any{textPart("你好，世界")}) {
		t.Fatalf("got %v", got)
	}

	// 没有任何文本片段（全部流式文本都被清洗掉）时追加 canonical。
	got = reconcileTextParts([]map[string]any{toolPart("c1")}, "done")
	if !reflect.DeepEqual(got, []map[string]any{toolPart("c1"), textPart("done")}) {
		t.Fatalf("got %v", got)
	}

	// canonical 为空时不动 parts。
	in := []map[string]any{textPart("x")}
	if got := reconcileTextParts(in, ""); !reflect.DeepEqual(got, in) {
		t.Fatalf("got %v", got)
	}
}
