package stages

import (
	"testing"

	"github.com/kasuganosora/thinkbot/llm"
)

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// 对应真实事故形态：长任务里凭空给出一个产出物路径，但本轮工具只做了检索。
func TestDetectUnbackedCompletionClaim_HighForMissingArtifact(t *testing.T) {
	text := "这次真做完了，结果在 `/tmp/report_logs.tar.gz`，可以直接下载。"
	results := []llm.ToolResult{
		{
			ToolName: "exec",
			Output:   map[string]any{"stdout": "log scan complete, no file written", "exitCode": 0},
		},
	}

	f := DetectUnbackedCompletionClaim(text, results)
	if f.Level != "high" {
		t.Fatalf("level = %q, want high (finding=%+v)", f.Level, f)
	}
	if !containsString(f.UnsupportedArtifacts, "report_logs.tar.gz") {
		t.Fatalf("unsupported artifacts = %v, want report_logs.tar.gz", f.UnsupportedArtifacts)
	}
	if f.ToolResultCount != 1 {
		t.Fatalf("tool_result_count = %d, want 1", f.ToolResultCount)
	}
}

func TestDetectUnbackedCompletionClaim_SuppressedWhenToolOutputMentionsArtifact(t *testing.T) {
	text := "已生成 `/tmp/report_logs.tar.gz`。"
	results := []llm.ToolResult{
		{
			ToolName: "exec",
			Output:   map[string]any{"stdout": "wrote /tmp/report_logs.tar.gz", "exitCode": 0},
		},
	}
	if f := DetectUnbackedCompletionClaim(text, results); f.Level != "" {
		t.Fatalf("tool output is valid evidence, got finding %+v", f)
	}
}

func TestDetectUnbackedCompletionClaim_MediumWithZeroToolResults(t *testing.T) {
	f := DetectUnbackedCompletionClaim("已生成压缩包，请查收。", nil)
	if f.Level != "medium" {
		t.Fatalf("level = %q, want medium (finding=%+v)", f.Level, f)
	}
}

func TestDetectUnbackedCompletionClaim_NoFalsePositiveOnOrdinaryReplies(t *testing.T) {
	cases := []string{
		"好的，收到。",
		"结果在下面，我列给你看。",
		"我没有发文件/附件的能力。",
		"还没全好，如实说下进度：检索完成，但上传这一步没做。",
		"Here is what I found in the logs.",
	}
	results := []llm.ToolResult{{ToolName: "exec", Output: "some log lines"}}
	for _, text := range cases {
		if f := DetectUnbackedCompletionClaim(text, results); f.Level != "" {
			t.Errorf("false positive for %q: %+v", text, f)
		}
	}
}

// 复杂工具输出必须按 JSON 摊平后再检索；早先压缩预算曾因把 map 输出
// stringify 成空串而漏算，这里锁住同一类问题不再复发。
func TestDetectUnbackedCompletionClaim_ComplexToolOutputIsSearchable(t *testing.T) {
	text := "已生成 `artifact-42.zip`。"
	results := []llm.ToolResult{
		{
			ToolName: "exec",
			Output: map[string]any{
				"stdout":   "created artifact-42.zip",
				"exitCode": 0,
				"files":    []any{"artifact-42.zip", "index.json"},
			},
		},
	}
	if f := DetectUnbackedCompletionClaim(text, results); f.Level != "" {
		t.Fatalf("complex tool output must count as evidence, got %+v", f)
	}
}

func TestCollectToolResults_MergesSummaryAndSteps(t *testing.T) {
	r := &llm.GenerateResult{
		ToolResults: []llm.ToolResult{{ToolName: "summary-level"}},
		Steps: []llm.StepResult{
			{ToolResults: []llm.ToolResult{{ToolName: "step-1"}}},
			{ToolResults: []llm.ToolResult{{ToolName: "step-2"}}},
		},
	}
	got := collectToolResults(r)
	if len(got) != 3 {
		t.Fatalf("collected %d results, want 3", len(got))
	}
	if got[0].ToolName != "summary-level" || got[2].ToolName != "step-2" {
		t.Fatalf("unexpected order: %+v", got)
	}
	if len(collectToolResults(nil)) != 0 {
		t.Fatal("nil result must yield no tool results")
	}
}

// 回复里顺带引用的代码库文件不该被当成本轮产出：完成措辞与文件名不在
// 同一行时，不产生 high 级发现。真实日志里这类引用相当常见。
func TestDetectUnbackedCompletionClaim_ReferencedFileOnOtherLineIsNotAProduct(t *testing.T) {
	text := "已推送到远端。\n\n相关配置见 pnpm-workspace.yaml（仓库里原有的文件）。"
	results := []llm.ToolResult{{ToolName: "exec", Output: "push ok"}}

	f := DetectUnbackedCompletionClaim(text, results)
	if f.Level != "" {
		t.Fatalf("a file merely referenced on another line must not be treated as this turn's artifact, got %+v", f)
	}
	if len(f.Claims) == 0 {
		t.Fatal("the completion phrase should still be detected (guard against a trivially passing test)")
	}
}
