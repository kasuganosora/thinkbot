package pipeline

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/kasuganosora/thinkbot/llm"
)

// ============================================================================
// Completion-claim ↔ evidence reconciliation
//
// The lazy guard used to only look for "environment state assertions" and only
// on turns with zero tool calls. Production incidents showed a different and
// more damaging failure mode: the bot states that an action finished ("go build
// passed", "already pushed", "all parameters removed") when that action was
// never executed in this turn. The model cannot tell "I said it last turn" from
// "I did it", because cross-turn history only replays assistant text.
//
// This file adds a deterministic reconciliation pass: a small set of explicit,
// verifiable completion claims is mapped to the runtime evidence that must be
// present. It is deliberately narrow. The acceptance bar is a near-zero false
// positive rate when replayed against real assistant turns that called tools.
//
// Two false-positive sources were found in that replay and are handled here:
//   - Domain language ("已提交报价", "文件记录已推送完成") is not a git action.
//     Commit/push claims therefore require an explicit git context.
//   - Reports frequently summarise work done in EARLIER turns ("已合 master、
//     编译通过、推送远端"), which is a truthful retrospective, not a fabrication.
//     A retrospective marker near the claim suppresses the challenge, and the
//     previous turn's persisted commands are accepted as evidence.
//
// Evidence sources:
//   - Commands / ToolNames: this turn's Steps (llm.GenerateResult).
//   - PriorCommands: the previous assistant turn's persisted tool calls.
// ============================================================================

// ClaimEvidence is the runtime evidence available for one turn.
type ClaimEvidence struct {
	// ToolNames are the lower-cased tool names called this turn.
	ToolNames []string
	// Commands are the lower-cased exec/shell inputs of this turn.
	Commands []string
	// PriorCommands are the lower-cased exec/shell inputs of the previous
	// assistant turn, reconstructed from persisted tool calls.
	PriorCommands []string
}

// CompletionClaim describes a claim that has no supporting evidence.
type CompletionClaim struct {
	// Kind identifies the claim class (go_build, make_proto, ...).
	Kind string
	// Required is the command/tool that would have satisfied it.
	Required string
}

type claimRule struct {
	kind     string
	claim    *regexp.Regexp
	required string
}

// commandClaimRules are intentionally specific. A bare "done" is never enough
// to trigger reconciliation; the reply must name a concrete verifiable action.
var commandClaimRules = []claimRule{
	{
		kind:     "go_build",
		claim:    regexp.MustCompile(`(?i)(go\s+build[^\n|]{0,60}?(?:通过|成功|EXIT\s*=\s*0|BUILD_EXIT=0|0\s*错误)|(?:编译|构建)\s*(?:已\s*)?通过|BUILD_EXIT=0)`),
		required: "go build",
	},
	{
		kind: "make_proto",
		// Past tense only: "重新跑了 make proto，生成成功" / "make proto EXIT=0".
		// "再跑一次 make proto（能跑成功）" is a plan and must not match.
		claim:    regexp.MustCompile(`(?i)((?:重新)?跑了[^\n|]{0,24}?make\s+proto[^\n|]{0,40}?(?:生成成功|成功|通过)|make\s+proto[^\n|]{0,20}?(?:生成成功|EXIT\s*=\s*0|BUILD_EXIT=0))`),
		required: "make proto",
	},
	{
		kind:     "go_test",
		claim:    regexp.MustCompile(`(?i)(go\s+test[^\n|]{0,60}?(?:通过|成功|PASS|EXIT\s*=\s*0)|(?:单元|全部)?测试\s*(?:全部\s*)?通过)`),
		required: "go test",
	},
	{
		kind: "git_push",
		// Requires an explicit git/remote context; "记录已推送完成" is domain data.
		// A bare "推送成功" is not enough: replays showed it appears as a stale
		// fragment of a previous turn with no git command in the current one.
		claim:    regexp.MustCompile(`(?i)(git\s+push|(?:已|已经|刚)\s*push(?:\s*成功|\s*完成)|(?:已|已经)(?:完成)?(?:推送|push)(?:\s*(?:到|至))?\s*(?:远端|origin|remote|仓库|分支|代码)|推送远端|push-check)`),
		required: "git push",
	},
	{
		kind: "git_commit",
		// Requires an explicit git/change context; "已提交报价/提交状态" is domain data.
		claim:    regexp.MustCompile(`(?i)(git\s+commit|(?:已|已经|刚)\s*commit(?:\s*完成|\s*成功)?|(?:已|已经)(?:完成)?(?:提交|commit)(?:\s*(?:代码|改动|修复|变更|到远端|到仓库|到分支))|(?:提交|commit)\s*(?:并|然后|再|\+)\s*(?:推送|push)|提交推送)`),
		required: "git commit",
	},
}

// retrospectiveContextRe marks a claim as a truthful report of earlier work
// (or as a plan), not an assertion that this turn executed the action.
// It is applied to a window around the matched claim, not to the whole reply.
var retrospectiveContextRe = regexp.MustCompile(`(?i)(上一轮|之前|本次分支|已合|master|验证记录|小结|状态|待上线|验收|清单|计划|准备|如果|要我|说一声|再跑一次|能跑|会跑|改完我会|将按|按\s*B\s*来|发布与验收|要提交推送|提交推送吗|确认后|没完成|未完成|没跑|也没|本轮没|如实说明)`)

// changeClaimRe matches a bare "the change is done" statement. It is used only
// by HasCompletionClaim (light tier, which has no file/shell tools at all);
// reconciliation deliberately does not use it because historical summaries
// routinely say "已修复/已删除" about earlier turns.
var changeClaimRe = regexp.MustCompile(`(?i)(改完了|已改好|已经改好|(?:已|已经)(?:经)?(?:修改|删除|修复|重构|去掉|移除|新增)(?:了|完成|完毕)?|处理完了)`)

// claimContextWindow is the number of bytes kept on each side of a match when
// looking for retrospective markers. UTF-8 boundaries do not matter here:
// the markers are only used with strings.Contains-style matching.
const claimContextWindow = 90

func retrospectiveClaim(text string, loc []int) bool {
	from := loc[0] - claimContextWindow
	if from < 0 {
		from = 0
	}
	to := loc[1] + claimContextWindow
	if to > len(text) {
		to = len(text)
	}
	return retrospectiveContextRe.MatchString(text[from:to])
}

// internalBlocksRe removes private blocks before reconciliation. A model may
// write "I will claim the build passed" inside <think>/<internal>; that is not
// a claim made to the user and must not trigger a loop-back.
var internalBlocksRe = regexp.MustCompile(`(?is)<think>.*?</think>|<thinking>.*?</thinking>|<internal>.*?</internal>|<execution_record>.*?</execution_record>`)

// ClaimVisibleText returns the part of a reply that a user can actually see.
func ClaimVisibleText(text string) string {
	return internalBlocksRe.ReplaceAllString(text, "")
}

// commandRequires reports whether required is satisfied by one of commands.
func commandRequires(required string, commands []string) bool {
	for _, c := range commands {
		if strings.Contains(c, required) {
			return true
		}
		switch required {
		case "make proto":
			if strings.Contains(c, "make") && strings.Contains(c, "proto") {
				return true
			}
		case "git commit", "git push":
			// `git -C <path> commit ...` / `git push origin <branch>` both
			// contain the verb but not the literal "git commit" / "git push".
			verb := strings.TrimPrefix(required, "git ")
			if strings.Contains(c, "git") && strings.Contains(c, verb) {
				return true
			}
		}
	}
	return false
}

// CheckCompletionClaims returns the first completion claim that lacks runtime
// evidence, or nil when every claim is backed by an actual tool call.
//
// The check is deterministic and side-effect free so it can be replayed against
// persisted turns to measure its false positive rate before rollout.
func CheckCompletionClaims(reply string, ev ClaimEvidence) *CompletionClaim {
	if strings.TrimSpace(reply) == "" {
		return nil
	}
	for _, rule := range commandClaimRules {
		loc := rule.claim.FindStringIndex(reply)
		if loc == nil || retrospectiveClaim(reply, loc) {
			continue
		}
		if commandRequires(rule.required, ev.Commands) ||
			commandRequires(rule.required, ev.PriorCommands) {
			continue
		}
		return &CompletionClaim{Kind: rule.kind, Required: rule.required}
	}
	return nil
}

// HasCompletionClaim reports whether the text contains any explicit action
// completion claim. Used by the light-tier guard: a light turn has no exec or
// file tools, so any such claim is unbacked by construction.
func HasCompletionClaim(text string) bool {
	if strings.TrimSpace(text) == "" {
		return false
	}
	for _, rule := range commandClaimRules {
		if rule.claim.MatchString(text) {
			return true
		}
	}
	return changeClaimRe.MatchString(text)
}

// EvidenceFromResult extracts this turn's tool evidence from an orchestration
// result. Tool inputs are stringified with fmt.Sprint on purpose: exec inputs
// arrive both as JSON objects and as map-like strings, and we only need to
// look for command substrings.
func EvidenceFromResult(result *llm.GenerateResult) ClaimEvidence {
	var ev ClaimEvidence
	if result == nil {
		return ev
	}
	seen := make(map[string]bool)
	for _, step := range result.Steps {
		for _, call := range step.ToolCalls {
			name := strings.ToLower(call.ToolName)
			if name == "" {
				continue
			}
			if !seen[name] {
				seen[name] = true
				ev.ToolNames = append(ev.ToolNames, name)
			}
			if !isShellTool(name) {
				continue
			}
			if s := strings.ToLower(fmt.Sprint(call.Input)); s != "" && s != "<nil>" {
				ev.Commands = append(ev.Commands, s)
			}
		}
	}
	return ev
}

func isShellTool(name string) bool {
	switch name {
	case "exec", "shell", "bash", "run_command", "terminal":
		return true
	}
	return strings.Contains(name, "exec")
}
