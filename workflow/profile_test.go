package workflow

import (
	"testing"

	"github.com/kasuganosora/thinkbot/sandbox"
)

// ============================================================================
// 节点工具档位 — 纯逻辑单元测试
// ============================================================================

func TestParseToolProfile(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    ToolProfile
		wantErr bool
	}{
		{name: "空值默认full", input: "", want: ProfileFull},
		{name: "空白默认full", input: "   ", want: ProfileFull},
		{name: "readonly", input: "readonly", want: ProfileReadOnly},
		{name: "analysis", input: "analysis", want: ProfileAnalysis},
		{name: "edit", input: "edit", want: ProfileEdit},
		{name: "full", input: "full", want: ProfileFull},
		{name: "大小写不敏感", input: "ReadOnly", want: ProfileReadOnly},
		{name: "带空格", input: "  edit  ", want: ProfileEdit},

		// ↓ 非法值必须报错，不静默降级。
		// 若把拼写错误的 "radonly" 静默当作 full，作者会以为自己声明了只读
		// 限制而实际没有——这比不声明更危险。
		{name: "拼写错误报错", input: "radonly", wantErr: true},
		{name: "未知档位报错", input: "admin", wantErr: true},
		{name: "空字符串档位报错", input: `""`, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseToolProfile(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error for input %q, got profile %q", tt.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestToolsForProfile(t *testing.T) {
	tests := []struct {
		name        string
		profile     ToolProfile
		wantContain []string
		wantAbsent  []string
		wantNil     bool
	}{
		{
			name:        "readonly 只有读能力",
			profile:     ProfileReadOnly,
			wantContain: []string{toolListDir, toolReadFile, toolSearchContent, toolHealth},
			wantAbsent:  []string{toolExec, toolRunCode, toolWriteFile, toolDeleteFile, toolMoveFile},
		},
		{
			name:        "analysis 增加执行但不写",
			profile:     ProfileAnalysis,
			wantContain: []string{toolListDir, toolReadFile, toolExec, toolRunCode},
			wantAbsent:  []string{toolWriteFile, toolReplaceInFile, toolDeleteFile, toolMoveFile},
		},
		{
			name:        "edit 增加写入但不含删除移动",
			profile:     ProfileEdit,
			wantContain: []string{toolReadFile, toolExec, toolWriteFile, toolReplaceInFile},
			// 删除与移动是破坏性操作，只有显式 full 才有
			wantAbsent: []string{toolDeleteFile, toolMoveFile},
		},
		{
			name:    "full 不过滤（返回 nil）",
			profile: ProfileFull,
			wantNil: true,
		},
		{
			name:    "空档位不过滤（向后兼容）",
			profile: "",
			wantNil: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := toolsForProfile(tt.profile)
			if tt.wantNil {
				if got != nil {
					t.Errorf("expected nil (no filtering), got %v", got)
				}
				return
			}
			set := make(map[string]bool, len(got))
			for _, n := range got {
				set[n] = true
			}
			for _, want := range tt.wantContain {
				if !set[want] {
					t.Errorf("profile %q should include %q; got %v", tt.profile, want, got)
				}
			}
			for _, absent := range tt.wantAbsent {
				if set[absent] {
					t.Errorf("profile %q must NOT include %q; got %v", tt.profile, absent, got)
				}
			}
		})
	}
}

// TestProfileTools_NamesAreKnown 防「硬编码工具名静默失效」。
//
// profile.go 里的工具名是硬编码字符串（sandbox 包没有导出工具名常量）。
// 一旦 sandbox 改名或新增工具，这份清单就会过时——**而且失效是静默的**：
// 白名单里对不上的名字会被无声过滤，现象是「节点突然少了一些工具」，
// 没有任何报错或日志。
//
// 本测试用 sandbox.WorkspaceToolNames()（唯一真源）逐个校验，
// 工具改名会直接让测试失败，而不是悄悄丢能力。
//
// 注意：这里 import sandbox 只出现在**测试文件**中。生产代码刻意不依赖
// sandbox——workflow 通过 agent/tools 的 ToolResolver 间接获取工具，
// 不必知道工具来自哪里。测试代码 import 是安全的（sandbox 不依赖 workflow）。
func TestProfileTools_NamesAreKnown(t *testing.T) {
	known := make(map[string]bool)
	for _, n := range sandbox.WorkspaceToolNames() {
		known[n] = true
	}
	if len(known) == 0 {
		t.Fatal("sandbox.WorkspaceToolNames() returned nothing; check the toolbox wiring")
	}

	for profile, names := range profileTools {
		if names == nil {
			continue // full = 不过滤
		}
		for _, n := range names {
			if !known[n] {
				t.Errorf("profile %q references unknown workspace tool %q; "+
					"sandbox may have renamed/removed it (known: %v)",
					profile, n, sandbox.WorkspaceToolNames())
			}
		}
	}
}

// TestProfileTools_NoOverlapWithDestructive 破坏性工具不得出现在非 full 档位。
//
// 删除与移动一旦被滥用，损失不可逆（且与其他并行节点共享同一工作区）。
// 这里锁死「只有显式 full 才能拿到」这条约束，防止后续加档位时误开放。
func TestProfileTools_NoOverlapWithDestructive(t *testing.T) {
	destructive := []string{toolDeleteFile, toolMoveFile}
	for profile, names := range profileTools {
		if profile == ProfileFull {
			continue
		}
		set := make(map[string]bool, len(names))
		for _, n := range names {
			set[n] = true
		}
		for _, d := range destructive {
			if set[d] {
				t.Errorf("profile %q must not grant destructive tool %q", profile, d)
			}
		}
	}
}

// TestReplaceNodeWithSubgraph_InheritsToolProfile 防「自愈后档位放宽」。
//
// 这是**安全倒退**的回归测试：RefineNode 的输出格式（heal.go:165）不含
// toolProfile 字段，浅拷贝后该字段为空，而空档位语义是 full（不过滤）。
// 若不在替换时显式继承，一次失败修复就会让节点从 readonly 变回 full，
// 拿到它原本不该有的 exec / 删除能力。
func TestReplaceNodeWithSubgraph_InheritsToolProfile(t *testing.T) {
	tests := []struct {
		name     string
		original ToolProfile
	}{
		{name: "readonly 子图继承 readonly", original: ProfileReadOnly},
		{name: "analysis 子图继承 analysis", original: ProfileAnalysis},
		{name: "edit 子图继承 edit", original: ProfileEdit},
		{name: "full 子图继承 full", original: ProfileFull},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wf := NewWorkflow("wf", "req", []*DAGNode{
				{ID: "n1", Name: "work", ToolProfile: tt.original},
				{ID: "n2", Name: "review", Dependencies: []string{"n1"}},
			})
			if err := wf.Compile(); err != nil {
				t.Fatalf("Compile: %v", err)
			}

			// 模拟自愈产出的子图：节点不带 toolProfile（与 RefineNode 实际输出一致）
			sub := []*DAGNode{
				{ID: "a", Name: "sub-a"},
				{ID: "b", Name: "sub-b", Dependencies: []string{"a"}},
			}
			if err := wf.ReplaceNodeWithSubgraph("n1", sub); err != nil {
				t.Fatalf("ReplaceNodeWithSubgraph: %v", err)
			}

			for _, id := range []string{"n1-h1", "n1-h2"} {
				n, ok := wf.GetNode(id)
				if !ok {
					t.Fatalf("subgraph node %q not found", id)
				}
				if n.ToolProfile != tt.original {
					t.Errorf("node %q: ToolProfile got %q, want %q (inherited from replaced node)",
						id, n.ToolProfile, tt.original)
				}
			}
		})
	}
}
