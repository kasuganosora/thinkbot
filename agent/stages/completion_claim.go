package stages

import (
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/kasuganosora/thinkbot/llm"
)

// ============================================================================
// 完成断言校验（出站前的程序化检查）
//
// agent_honesty_rules 只是 system prompt 里的一段约束，模型在长任务里仍会
// 把「打算做 / 以为做了」写成「已经做完」，甚至凭空给出一个产出物路径。
// 这类幻觉用纯文本规则无法 100% 判定，但有一个误报极低、可精确验证的切面：
// 回复里声称产出了**具体文件**，而本轮所有工具执行的输出里从未出现过该文件名。
//
// 设计取舍：
//   - 默认只告警（WARN 日志 + span + envelope 标记），**不改写、不拦截**出站文本。
//     先用真实对话统计误报率，再决定是否收紧；否则一条过于激进的规则会把
//     正常回复也改坏。
//   - 判据只依赖「本轮工具结果」这一运行时证据，不采信模型自己的历史发言。
//   - 复杂工具输出（map / slice / struct）按 JSON 文本检索；此前压缩预算曾因
//     把这类输出 stringify 成空串而漏算，这里不能重蹈覆辙。
// ============================================================================

// CompletionClaimFinding 是一次出站回复的校验结果。Level 为空表示未发现问题。
//   - high：回复声称产出了具体文件，但本轮工具输出里找不到该文件名。
//   - medium：回复含有完成动作断言，但本轮没有任何工具执行结果。
type CompletionClaimFinding struct {
	Level                string   `json:"level"`
	Claims               []string `json:"claims,omitempty"`
	Artifacts            []string `json:"artifacts,omitempty"`
	UnsupportedArtifacts []string `json:"unsupportedArtifacts,omitempty"`
	ToolResultCount      int      `json:"toolResultCount"`
}

var (
	// completionActionRe 命中「动作已完成」类断言。刻意不收录「结果在 / 文件在」
	// 这类指向性措辞：它们常出现在正常回复里（"结果在下面"），单独命中会误报。
	completionActionRe = regexp.MustCompile(`(?i)(已经?生成|已经?保存|已经?上传|已经?打包|已经?压缩|已经?写入|已经?创建|已经?提交|已经?推送|已经?部署|生成好了|做完了|搞定了|已生成|已保存|已上传|已打包|已压缩|已写入|已创建|已提交|已推送|已部署|(?:has been )?(?:generated|saved|uploaded|created|written|packaged))`)

	// artifactPointerRe 是「产出物指向」措辞，本身不足以构成完成断言，
	// 但可以用来定位同一行里被点名的文件。
	artifactPointerRe = regexp.MustCompile(`结果在|文件在|保存在|输出到|生成到|写到|结果[:：]|文件[:：]`)

	// artifactRe 只认带常见产出扩展名的路径/文件名，避免把普通英文单词当产出物。
	artifactRe = regexp.MustCompile(`(?i)(?:[A-Za-z0-9_./-]*/)?[A-Za-z0-9_.-]+\.(?:tar\.gz|tgz|zip|json|md|txt|csv|log|pdf|xlsx|docx|png|jpe?g|yaml|yml|sql|patch|diff|bin)`)
)

// DetectUnbackedCompletionClaim 检查最终出站文本里的完成断言是否有本轮工具证据。
// results 应包含本轮所有步骤的工具结果（见 collectToolResults）。
func DetectUnbackedCompletionClaim(text string, results []llm.ToolResult) CompletionClaimFinding {
	f := CompletionClaimFinding{ToolResultCount: len(results)}
	if strings.TrimSpace(text) == "" {
		return f
	}
	claims := completionActionRe.FindAllString(text, -1)
	if len(claims) == 0 {
		return f
	}
	f.Claims = uniqueStrings(claims)

	// 只在「同一行内既有完成措辞、又点名了文件」时把该文件当成本轮产出断言。
	// 回复里顺带引用的代码库文件（"我读了 pnpm-workspace.yaml"）不算，否则
	// 正常的技术回复会被大量误报。
	seen := make(map[string]bool)
	for _, line := range strings.Split(text, "\n") {
		if !completionActionRe.MatchString(line) && !artifactPointerRe.MatchString(line) {
			continue
		}
		for _, m := range artifactRe.FindAllString(line, -1) {
			base := path.Base(m)
			if base == "" || base == "." || seen[base] {
				continue
			}
			seen[base] = true
			f.Artifacts = append(f.Artifacts, base)
		}
	}

	// 只有当工具输出确实提到过这个文件名，才算「有凭据」。用 basename 匹配，
	// 兼容工具返回绝对路径而回复里只写文件名（或反过来）的情况。
	evidence := toolEvidenceText(results)
	for _, a := range f.Artifacts {
		if !strings.Contains(evidence, a) {
			f.UnsupportedArtifacts = append(f.UnsupportedArtifacts, a)
		}
	}

	switch {
	case len(f.UnsupportedArtifacts) > 0:
		f.Level = "high"
	case len(results) == 0:
		f.Level = "medium"
	}
	return f
}

// collectToolResults 汇总本轮全部工具结果。GenerateResult.ToolResults 是汇总字段，
// Steps[].ToolResults 是逐步明细；两者都要收，避免某条路径只填了其中一个。
func collectToolResults(result *llm.GenerateResult) []llm.ToolResult {
	if result == nil {
		return nil
	}
	out := make([]llm.ToolResult, 0, len(result.ToolResults)+len(result.Steps))
	out = append(out, result.ToolResults...)
	for _, st := range result.Steps {
		out = append(out, st.ToolResults...)
	}
	return out
}

// toolEvidenceText 把工具输出摊平成可检索文本。复杂类型必须 JSON 序列化，
// 不能用 fmt.Sprintf("%v") 之外的空串实现（见文件头说明）。
func toolEvidenceText(results []llm.ToolResult) string {
	var b strings.Builder
	for _, r := range results {
		b.WriteString(r.ToolName)
		b.WriteByte('\n')
		switch v := r.Output.(type) {
		case nil:
		case string:
			b.WriteString(v)
		default:
			if data, err := json.Marshal(v); err == nil {
				b.Write(data)
			} else {
				fmt.Fprintf(&b, "%v", v)
			}
		}
		b.WriteByte('\n')
	}
	return b.String()
}

func uniqueStrings(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
