package pipeline

import (
	"strings"
	"testing"
)

func TestCheckCompletionClaims(t *testing.T) {
	cases := []struct {
		name string
		text string
		ev   ClaimEvidence
		want string
	}{
		{
			name: "fabricated build claim without tools",
			text: "改完了，接口已去掉全部参数，`go build ./...` 通过（EXIT=0）。重新跑了 `make proto`，生成成功。",
			want: "go_build",
		},
		{
			name: "build claim backed by this turn",
			text: "改完了，`go build ./...` 通过（EXIT=0）。",
			ev:   ClaimEvidence{Commands: []string{"{'command': 'go build ./...'}"}},
		},
		{
			name: "build claim backed by previous turn",
			text: "改完了，`go build ./...` 通过（EXIT=0）。",
			ev:   ClaimEvidence{PriorCommands: []string{"{'command': 'go build ./...'}"}},
		},
		{
			name: "retrospective report is not a fabrication",
			text: "本分支实现，已合 master、编译通过、推送远端（1edbb036f）。",
			ev:   ClaimEvidence{Commands: []string{"cat doc.md"}},
		},
		{
			name: "domain language is not a git action",
			text: "报价轮 R1 已提交报价，文件记录已推送完成，提交状态为 done。",
			ev:   ClaimEvidence{Commands: []string{"select * from t"}},
		},
		{
			name: "question about a future action",
			text: "当前未提交改动：后端 3 文件。要提交推送吗？",
		},
		{
			name: "explicitly unfinished work",
			text: "如实说明：补 lint + commit + push 这件事本轮没完成。",
		},
		{
			name: "plan rather than result",
			text: "先加 proto 再跑一次 `make proto`（环境现在能跑成功），然后加实现。",
		},
		{
			name: "git -C variant satisfies commit claim",
			text: "提交并推送：修复完成并已推送 ✅",
			ev:   ClaimEvidence{Commands: []string{"git -C repo commit -m fix; git -C repo push origin branch"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := CheckCompletionClaims(tc.text, tc.ev)
			kind := ""
			if got != nil {
				kind = got.Kind
			}
			if kind != tc.want {
				t.Fatalf("claim = %q, want %q", kind, tc.want)
			}
		})
	}
}

func TestClaimVisibleText(t *testing.T) {
	in := "<think>我会说 go build 通过</think>结论：<internal>假装已推送</internal>还没有编译。"
	got := ClaimVisibleText(in)
	if strings.Contains(got, "go build 通过") || strings.Contains(got, "已推送") {
		t.Fatalf("private blocks leaked into claim text: %q", got)
	}
	if CheckCompletionClaims(got, ClaimEvidence{}) != nil {
		t.Fatal("honest text must not be challenged after private blocks are removed")
	}
}

func TestHasCompletionClaim(t *testing.T) {
	if !HasCompletionClaim("改完了，参数已去掉") {
		t.Fatal("bare completion claim must be detected for the light-tier guard")
	}
	if HasCompletionClaim("这是一个概念解释，没有执行任何操作。") {
		t.Fatal("plain explanation must not be treated as a completion claim")
	}
}

func TestEvidenceFromResult(t *testing.T) {
	ev := EvidenceFromResult(nil)
	if len(ev.Commands) != 0 || len(ev.PriorCommands) != 0 {
		t.Fatal("nil result must produce empty evidence")
	}
}
