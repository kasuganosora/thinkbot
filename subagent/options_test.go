package subagent

import (
	"testing"

	"github.com/kasuganosora/thinkbot/llm"
)

// ============================================================================
// 工具白名单（WithToolAllowlist）— 纯逻辑单元测试
//
// 这是工具级最小权限的基础能力：调用方声明白名单，SubAgent 只看到名单内的工具。
// ============================================================================

func makeTools(names ...string) []llm.Tool {
	tools := make([]llm.Tool, 0, len(names))
	for _, n := range names {
		tools = append(tools, llm.Tool{Name: n})
	}
	return tools
}

func TestFilterToolsByName(t *testing.T) {
	all := makeTools("sandbox_exec", "sandbox_read_file", "sandbox_write_file", "sandbox_delete_file")

	tests := []struct {
		name  string
		tools []llm.Tool
		allow []string
		want  []string
	}{
		{
			name:  "空白名单不过滤",
			tools: all,
			allow: nil,
			want:  []string{"sandbox_exec", "sandbox_read_file", "sandbox_write_file", "sandbox_delete_file"},
		},
		{
			name:  "空切片同样不过滤",
			tools: all,
			allow: []string{},
			want:  []string{"sandbox_exec", "sandbox_read_file", "sandbox_write_file", "sandbox_delete_file"},
		},
		{
			name:  "白名单生效",
			tools: all,
			allow: []string{"sandbox_read_file"},
			want:  []string{"sandbox_read_file"},
		},
		{
			name:  "多个白名单按原顺序保留",
			tools: all,
			allow: []string{"sandbox_write_file", "sandbox_read_file"},
			want:  []string{"sandbox_read_file", "sandbox_write_file"},
		},
		{
			name:  "未知名字被静默忽略",
			tools: all,
			allow: []string{"sandbox_read_file", "no_such_tool"},
			want:  []string{"sandbox_read_file"},
		},
		{
			name:  "全不匹配返回空集",
			tools: all,
			allow: []string{"gone_tool"},
			want:  []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := filterToolsByName(tt.tools, tt.allow)
			gotNames := make([]string, 0, len(got))
			for _, g := range got {
				gotNames = append(gotNames, g.Name)
			}
			if !equalSlices(gotNames, tt.want) {
				t.Errorf("got %v, want %v", gotNames, tt.want)
			}
		})
	}
}

// TestApplyToolAllowlist_EmptiedFlag 过滤后为空必须上报。
//
// 这是重要的可观测性保证：白名单与可用工具严重不匹配时（工具改名、
// 档位映射表过时），若不告警，现象就是「节点突然没有工具」且日志毫无线索。
func TestApplyToolAllowlist_EmptiedFlag(t *testing.T) {
	all := makeTools("sandbox_exec", "sandbox_read_file")

	t.Run("全被过滤时上报", func(t *testing.T) {
		_, emptied := applyToolAllowlist(all, []Option{WithToolAllowlist("gone")})
		if !emptied {
			t.Error("emptied should be true when everything was filtered out")
		}
	})

	t.Run("未设置白名单不上报", func(t *testing.T) {
		_, emptied := applyToolAllowlist(all, nil)
		if emptied {
			t.Error("emptied should be false when no allowlist is set")
		}
	})

	t.Run("正常过滤不上报", func(t *testing.T) {
		_, emptied := applyToolAllowlist(all, []Option{WithToolAllowlist("sandbox_read_file")})
		if emptied {
			t.Error("emptied should be false for a partial filter")
		}
	})

	t.Run("原工具集为空时不上报", func(t *testing.T) {
		_, emptied := applyToolAllowlist(nil, []Option{WithToolAllowlist("sandbox_read_file")})
		if emptied {
			t.Error("emptied should be false when input was already empty")
		}
	})
}

// TestWithToolAllowlist_SkipToolsPriority skipTools 优先于白名单。
//
// 语义必须明确：skipTools 表示「完全不注入工具」，此时白名单无从生效。
// 若两者冲突时行为不确定，调用方无法推理权限。
func TestWithToolAllowlist_SkipToolsPriority(t *testing.T) {
	// hasSkipTools 为 true 时，manager 根本不会走到过滤分支，
	// 这里验证的是选项本身能被正确读取（过滤逻辑的入口前提）。
	allow := toolAllowlistFrom(WithToolAllowlist("a", "b"), WithSkipTools())
	if !equalSlices(allow, []string{"a", "b"}) {
		t.Errorf("allowlist should still be readable, got %v", allow)
	}

	// 未设置时返回 nil
	if got := toolAllowlistFrom(); got != nil {
		t.Errorf("unset allowlist should be nil, got %v", got)
	}
}

// TestWithToolAllowlist_CopiesSlice 白名单应被拷贝，避免调用方后续修改影响已创建的 SubAgent。
func TestWithToolAllowlist_CopiesSlice(t *testing.T) {
	names := []string{"a", "b"}
	var sa SubAgent
	WithToolAllowlist(names...)(&sa)

	// 调用方修改原切片
	names[0] = "mutated"

	if sa.toolAllowlist[0] != "a" {
		t.Errorf("allowlist should be copied, got %v", sa.toolAllowlist)
	}
}

func equalSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
