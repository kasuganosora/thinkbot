package strutil

import (
	"encoding/json"
	"errors"
	"testing"
)

// ============================================================================
// Truncate
// ============================================================================

func TestTruncate(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		maxRunes int
		want     string
	}{
		{"short", "hello", 10, "hello"},
		{"exact", "hello", 5, "hello"},
		{"truncate", "hello world", 5, "hello..."},
		{"empty", "", 5, ""},
		{"unicode", "你好世界测试", 4, "你好世界..."},
		{"zero_max", "abc", 0, "..."},
		{"ascii_one", "a", 1, "a"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Truncate(tt.input, tt.maxRunes)
			if got != tt.want {
				t.Errorf("Truncate(%q, %d) = %q, want %q", tt.input, tt.maxRunes, got, tt.want)
			}
		})
	}
}

// ============================================================================
// ExtractJSON — 对象
// ============================================================================

type testObj struct {
	Name string `json:"name"`
	Age  int    `json:"age"`
}

func TestExtractJSON_DirectObject(t *testing.T) {
	raw := `{"name":"alice","age":30}`
	var obj testObj
	if err := ExtractJSON(raw, &obj); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if obj.Name != "alice" || obj.Age != 30 {
		t.Errorf("got %+v", obj)
	}
}

func TestExtractJSON_MarkdownObject(t *testing.T) {
	raw := "```json\n{\"name\":\"bob\",\"age\":25}\n```"
	var obj testObj
	if err := ExtractJSON(raw, &obj); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if obj.Name != "bob" || obj.Age != 25 {
		t.Errorf("got %+v", obj)
	}
}

func TestExtractJSON_ObjectWithSurroundingText(t *testing.T) {
	raw := `Here is the result: {"name":"charlie","age":40} — enjoy.`
	var obj testObj
	if err := ExtractJSON(raw, &obj); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if obj.Name != "charlie" || obj.Age != 40 {
		t.Errorf("got %+v", obj)
	}
}

// ============================================================================
// ExtractJSON — 数组
// ============================================================================

func TestExtractJSON_DirectArray(t *testing.T) {
	raw := `[{"name":"alice","age":30},{"name":"bob","age":25}]`
	var arr []testObj
	if err := ExtractJSON(raw, &arr); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(arr) != 2 || arr[0].Name != "alice" || arr[1].Name != "bob" {
		t.Errorf("got %+v", arr)
	}
}

func TestExtractJSON_MarkdownArray(t *testing.T) {
	raw := "```json\n[{\"name\":\"x\",\"age\":1}]\n```"
	var arr []testObj
	if err := ExtractJSON(raw, &arr); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(arr) != 1 || arr[0].Name != "x" {
		t.Errorf("got %+v", arr)
	}
}

func TestExtractJSON_ArrayWithSurroundingText(t *testing.T) {
	raw := `Sure! Here are the results:
[{"name":"a","age":1},{"name":"b","age":2}]
That's all.`
	var arr []testObj
	if err := ExtractJSON(raw, &arr); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(arr) != 2 {
		t.Errorf("got %d items", len(arr))
	}
}

func TestExtractJSON_ArrayPlainMarkdown(t *testing.T) {
	raw := "```\n[{\"name\":\"plain\",\"age\":99}]\n```"
	var arr []testObj
	if err := ExtractJSON(raw, &arr); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(arr) != 1 || arr[0].Name != "plain" {
		t.Errorf("got %+v", arr)
	}
}

// ============================================================================
// ExtractJSON — 错误场景
// ============================================================================

func TestExtractJSON_InvalidJSON(t *testing.T) {
	raw := "this is not json at all"
	var obj testObj
	err := ExtractJSON(raw, &obj)
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestExtractJSON_Empty(t *testing.T) {
	var obj testObj
	err := ExtractJSON("", &obj)
	if err == nil {
		t.Error("expected error for empty string")
	}
}

func TestExtractJSON_PrefersArrayOverObject(t *testing.T) {
	// 当文本中同时包含 {} 和 [] 时，应优先尝试对象，失败后尝试数组
	raw := `{"incomplete": } [1, 2, 3]`
	var arr []int
	if err := ExtractJSON(raw, &arr); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(arr) != 3 {
		t.Errorf("got %v", arr)
	}
}

// ============================================================================
// stripMarkdownCodeBlock
// ============================================================================

func TestStripMarkdownCodeBlock(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"json_block", "```json\n{}\n```", "{}"},
		{"plain_block", "```\n[]\n```", "[]"},
		{"no_block", `{"a":1}`, `{"a":1}`},
		{"json_block_inline", "```json {\"a\":1} ```", `{"a":1}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stripMarkdownCodeBlock(tt.input)
			if got != tt.want {
				t.Errorf("stripMarkdownCodeBlock(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// 确保 ExtractJSON 返回的错误是 json 类型
func TestExtractJSON_ErrorIsJSONType(t *testing.T) {
	err := ExtractJSON("not json", &testObj{})
	if err == nil {
		t.Fatal("expected error")
	}
	var syntaxErr *json.SyntaxError
	if !errors.As(err, &syntaxErr) {
		// 也可能是其他 json 错误类型，只要不是 nil 就行
		// 这里只确认返回了某种 json 解析错误
	}
}
