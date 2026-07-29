package strutil

import (
	"encoding/json"
	"testing"
)

type sampleSpec struct {
	Nodes []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Task string `json:"task"`
	} `json:"nodes"`
}

func TestExtractJSON_Robustness(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want int // expected node count
	}{
		{
			name: "plain object",
			raw:  `{"nodes":[{"id":"n1","name":"a","task":"t"}]}`,
			want: 1,
		},
		{
			name: "wrapped in prose",
			raw:  `好的，这是分解结果：{"nodes":[{"id":"n1","name":"a","task":"t"}]} 以上。`,
			want: 1,
		},
		{
			name: "markdown fenced",
			raw:  "```json\n{\"nodes\":[{\"id\":\"n1\",\"name\":\"a\",\"task\":\"t\"}]}\n```",
			want: 1,
		},
		{
			name: "raw newline inside string value",
			raw:  "{\"nodes\":[{\"id\":\"n1\",\"name\":\"a\",\"task\":\"line1\nline2\tend\"}]}",
			want: 1,
		},
		{
			name: "trailing comma",
			raw:  `{"nodes":[{"id":"n1","name":"a","task":"t"},]}`,
			want: 1,
		},
		{
			name: "BOM prefix",
			raw:  "\ufeff{\"nodes\":[{\"id\":\"n1\",\"name\":\"a\",\"task\":\"t\"}]}",
			want: 1,
		},
		{
			name: "array top-level",
			raw:  `["a","b","c"]`,
			want: 3, // interpreted as nodes length? no — use a different struct
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "array top-level" {
				var arr []string
				if err := ExtractJSON(tc.raw, &arr); err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if len(arr) != tc.want {
					t.Fatalf("got %d items, want %d", len(arr), tc.want)
				}
				return
			}
			var spec sampleSpec
			if err := ExtractJSON(tc.raw, &spec); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(spec.Nodes) != tc.want {
				t.Fatalf("got %d nodes, want %d", len(spec.Nodes), tc.want)
			}
		})
	}
}

func TestExtractJSON_PreservesValidJSON(t *testing.T) {
	raw := `{"nodes":[{"id":"n1","name":"héllo","task":"任务\n含换行"}]}`
	var spec sampleSpec
	if err := json.Unmarshal([]byte(raw), &spec); err != nil {
		t.Fatalf("baseline valid JSON failed: %v", err)
	}
	var got sampleSpec
	if err := ExtractJSON(raw, &got); err != nil {
		t.Fatalf("ExtractJSON failed on valid JSON: %v", err)
	}
	if got.Nodes[0].Task != "任务\n含换行" {
		t.Fatalf("task not preserved: %q", got.Nodes[0].Task)
	}
}

func TestSanitizeJSONStrings_NoOpOnValid(t *testing.T) {
	in := `{"a":"b"}`
	if out := sanitizeJSONStrings(in); out != in {
		t.Fatalf("changed valid json: %q -> %q", in, out)
	}
}
