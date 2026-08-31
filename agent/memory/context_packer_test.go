package memory

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestContextPacker_BasicPacking(t *testing.T) {
	packer := NewContextPacker(ContextPackerConfig{
		MaxTotalChars:  200,
		MinItemChars:   20,
		MaxItemChars:   100,
		TargetItems:    3,
		OverfetchRatio: 2,
		EnableReorder:  false, // 关闭重排序便于断言
	})

	entries := []Entry{
		{ID: "1", Content: "用户使用 Go 语言", Category: "fact", Importance: 0.9, CreatedAt: time.Now()},
		{ID: "2", Content: "用户偏好简洁回复", Category: "preference", Importance: 0.8, CreatedAt: time.Now()},
		{ID: "3", Content: "用户完成了项目部署", Category: "event", Importance: 0.7, CreatedAt: time.Now()},
		{ID: "4", Content: "用户对 Rust 感兴趣", Category: "observation", Importance: 0.5, CreatedAt: time.Now()},
	}

	result := packer.Pack(context.TODO(), entries, "Go 语言")

	if len(result) == 0 {
		t.Fatal("expected non-empty result")
	}
	if len(result) > 3 {
		t.Errorf("expected at most 3 items, got %d", len(result))
	}

	// 第一个应该是最高分的（含 "Go" 关键词匹配 + 高 importance）
	if result[0].Entry.ID != "1" {
		t.Errorf("expected first item ID=1, got %s", result[0].Entry.ID)
	}
}

func TestContextPacker_Truncation(t *testing.T) {
	packer := NewContextPacker(ContextPackerConfig{
		MaxTotalChars:  300,
		MinItemChars:   20,
		MaxItemChars:   50, // 很小的限制
		TargetItems:    5,
		OverfetchRatio: 2,
		EnableReorder:  false,
	})

	longContent := "这是一段非常非常长的记忆内容" + repeatStr("x", 200)

	entries := []Entry{
		{ID: "1", Content: longContent, Importance: 0.9, CreatedAt: time.Now()},
		{ID: "2", Content: "short", Importance: 0.8, CreatedAt: time.Now()},
	}

	result := packer.Pack(context.TODO(), entries, "")

	if len(result) == 0 {
		t.Fatal("expected non-empty result")
	}

	// 第一条应该被截断
	if !result[0].Truncated {
		t.Error("expected first entry to be truncated")
	}
	// 截断后不应超过 MaxItemChars + "..."
	if len([]rune(result[0].Entry.Content)) > 53 {
		t.Errorf("truncated content too long: %d runes", len([]rune(result[0].Entry.Content)))
	}
}

func TestContextPacker_AntiLostMiddleReorder(t *testing.T) {
	packer := NewContextPacker(ContextPackerConfig{
		MaxTotalChars:  1000,
		MinItemChars:   10,
		MaxItemChars:   200,
		TargetItems:    6,
		OverfetchRatio: 1,
		EnableReorder:  true,
	})

	entries := []Entry{
		{ID: "s1", Content: "最高分条目", Importance: 1.0, CreatedAt: time.Now()},
		{ID: "s2", Content: "次高分条目", Importance: 0.9, CreatedAt: time.Now()},
		{ID: "s3", Content: "中等分条目", Importance: 0.7, CreatedAt: time.Now()},
		{ID: "s4", Content: "中低分条目", Importance: 0.5, CreatedAt: time.Now()},
		{ID: "s5", Content: "低分条目", Importance: 0.3, CreatedAt: time.Now()},
		{ID: "s6", Content: "最低分条目", Importance: 0.1, CreatedAt: time.Now()},
	}

	result := packer.Pack(context.TODO(), entries, "")

	if len(result) != 6 {
		t.Fatalf("expected 6 items, got %d", len(result))
	}

	// 最高分在头部（position 0）
	if result[0].Entry.ID != "s1" {
		t.Errorf("expected position 0 to be s1 (highest), got %s", result[0].Entry.ID)
	}

	// 次高分在尾部（position n-1）
	if result[5].Entry.ID != "s2" {
		t.Errorf("expected position 5 (tail) to be s2 (second highest), got %s", result[5].Entry.ID)
	}

	// 最低分应该在中间区域（position 2 或 3）
	middleIDs := []string{result[2].Entry.ID, result[3].Entry.ID}
	found := false
	for _, id := range middleIDs {
		if id == "s6" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected s6 (lowest) in middle positions [2,3], got %s, %s", result[2].Entry.ID, result[3].Entry.ID)
	}
}

func TestContextPacker_EmptyInput(t *testing.T) {
	packer := NewContextPacker()

	result := packer.Pack(context.TODO(), nil, "test")
	if result != nil {
		t.Error("expected nil for empty input")
	}

	text := packer.PackToText(context.TODO(), nil, "test")
	if text != "" {
		t.Error("expected empty text for empty input")
	}
}

func TestContextPacker_Scoring(t *testing.T) {
	packer := NewContextPacker()

	queryLower := "go 语言"
	entry := Entry{
		Content:    "用户使用 Go 语言",
		Category:   "fact",
		Importance: 0.8,
		CreatedAt:  time.Now(),
	}

	score := packer.scoreEntry(entry, queryLower)
	if score <= 0 {
		t.Errorf("expected positive score, got %f", score)
	}

	// 应该比没有匹配的高
	noMatch := Entry{
		Content:    "用户喜欢猫",
		Category:   "fact",
		Importance: 0.8,
		CreatedAt:  time.Now(),
	}
	noMatchScore := packer.scoreEntry(noMatch, queryLower)
	if score <= noMatchScore {
		t.Errorf("matching entry should score higher: %f vs %f", score, noMatchScore)
	}
}

// repeatStr 重复字符串 n 次。
func repeatStr(s string, n int) string {
	var sb strings.Builder
	for range n {
		sb.WriteString(s)
	}
	return sb.String()
}
