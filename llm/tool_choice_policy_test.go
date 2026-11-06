package llm

import (
	"reflect"
	"testing"
)

func TestNormalizeToolChoice(t *testing.T) {
	fn := map[string]any{"type": "function", "function": map[string]any{"name": "ping"}}
	tests := []struct {
		name   string
		model  string
		choice any
		want   any
	}{
		{"thinking model degrades required", "deepseek-flash", "required", "auto"},
		{"vendor prefix is stripped", "example/deepseek-v4-pro", "required", "auto"},
		{"model id is case insensitive", "DEEPSEEK-Flash", "required", "auto"},
		{"glm keeps required", "glm-5.3", "required", "required"},
		{"unknown model keeps required", "hy4", "required", "required"},
		{"empty model keeps required", "", "required", "required"},
		{"none stays none", "deepseek-flash", "none", "none"},
		{"auto stays auto", "deepseek-flash", "auto", "auto"},
		{"nil stays nil", "deepseek-flash", nil, nil},
		{"function object passes through", "deepseek-flash", fn, fn},
		{"non-canonical string passes through", "deepseek-flash", "Required", "Required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NormalizeToolChoice(tt.model, tt.choice)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("NormalizeToolChoice(%q, %v) = %v, want %v", tt.model, tt.choice, got, tt.want)
			}
		})
	}
}
