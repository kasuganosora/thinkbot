package workflow

import (
	"sort"
	"strings"

	"github.com/kasuganosora/thinkbot/util/errs"
)

// ============================================================================
// 节点工具档位（ToolProfile）— 工具级最小权限
//
// 背景：工作流节点 SubAgent **具备完整工作空间工具能力**，包括 sandbox_exec、
// run_code、write_file、replace_in_file、delete_file、move_file——详见
// wire.go:162-169（内部 SubAgent 继承工作空间工具）与 sandbox/tools.go:86-89
// （动态 provider 在 SubAgent 场景无条件返回全套工具）。而默认并发 3 个节点，
// 共享同一个 bot 工作区、无任何文件锁。
//
// 因此「每个节点都拿全套工具」意味着 N 个并行执行体各自持有 exec + 删除能力。
// 这里提供工具级最小权限：节点声明它需要什么档位，就只得到该档位的工具。
//
// 借鉴 gh-aw 的 safe-outputs 思路——**权限由「声明要做什么」推导，而非自由获取**。
// 差异：gh-aw 靠 job 边界隔离（agent job 只读、写操作下沉到独立 job），
// thinkbot 没有 job 边界（SubAgent 是 goroutine，共享工作区），故退到工具层收敛。
//
// 设计取舍：
//   - 档位做成**固定枚举**而非自由列举工具名：LLM 会写出无效名字，
//     且自由列举无法校验、无法审计。档位可枚举、可校验、可集中维护。
//   - 默认 ProfileFull，**不改现有行为**：并行节点改代码是工作流的核心能力，
//     一刀切降级会直接废掉它。先让 Analyzer 有能力表达更严的档位，
//     积累数据后再决定是否收紧默认值。
// ============================================================================

// ToolProfile 节点工具档位。
type ToolProfile string

const (
	// ProfileReadOnly 只读：探查目录、读文件、搜内容。不能执行命令、不能写入。
	ProfileReadOnly ToolProfile = "readonly"

	// ProfileAnalysis 只读 + 执行：在 readonly 之上增加 exec / run_code，
	// 用于跑测试、lint、构建等**不改文件**的验证类任务。
	ProfileAnalysis ToolProfile = "analysis"

	// ProfileEdit 在 analysis 之上增加写入能力（新建 / 局部替换），
	// 但**不含删除与移动**——破坏性操作需要显式声明 full。
	ProfileEdit ToolProfile = "edit"

	// ProfileFull 全部工具，等价现状行为。
	ProfileFull ToolProfile = "full"
)

// toolNames 工作空间工具名。
//
// 取值来自 sandbox/tools.go:111-124 的 botWorkspaceToolDefs（共 10 个）。
// 硬编码是无奈之举——sandbox 包没有导出工具名常量
// （grep `const.*sandbox_` 零命中），且 workflow 不能 import sandbox
// （wire.go 已依赖 sandbox 的工具解析，会形成循环依赖）。
//
// **失效风险与防线**：sandbox 一旦改名或新增工具，这张表就过时，
// 且失效是静默的（白名单里的名字匹配不上，工具被无声过滤掉）。
// 防线有两道：
//  1. TestProfileTools_NamesAreKnown（sandbox 包内）用 botWorkspaceToolDefs 的
//     实际返回值做静态断言，工具改名会直接让测试失败。
//  2. 运行时兜底：subagent.applyToolAllowlist 在「过滤后为空而原始非空」时
//     打 Warn 日志，不会无声退化。
const (
	toolExec          = "sandbox_exec"
	toolRunCode       = "run_code"
	toolReadFile      = "sandbox_read_file"
	toolWriteFile     = "sandbox_write_file"
	toolReplaceInFile = "sandbox_replace_in_file"
	toolDeleteFile    = "sandbox_delete_file"
	toolMoveFile      = "sandbox_move_file"
	toolListDir       = "sandbox_list_dir"
	toolSearchContent = "sandbox_search_content"
	toolHealth        = "sandbox_health"
)

// profileTools 档位 → 工具白名单。
//
// ProfileFull 对应 nil，表示「不过滤」——不是空列表（空列表会被解读为
// 「什么都不要」，与「全部都要」恰好相反，混淆二者是危险的）。
//
// 注意 sandbox_delete_file / sandbox_move_file **不在 readonly / analysis / edit
// 任何档位里**：删除与移动是破坏性操作，只有显式声明 full 才能拿到。
var profileTools = map[ToolProfile][]string{
	ProfileReadOnly: {
		toolListDir, toolReadFile, toolSearchContent, toolHealth,
	},
	ProfileAnalysis: {
		toolListDir, toolReadFile, toolSearchContent, toolHealth,
		toolExec, toolRunCode,
	},
	ProfileEdit: {
		toolListDir, toolReadFile, toolSearchContent, toolHealth,
		toolExec, toolRunCode,
		toolWriteFile, toolReplaceInFile,
	},
	ProfileFull: nil,
}

// AllToolProfiles 返回所有合法档位（供校验提示与文档生成）。
func AllToolProfiles() []ToolProfile {
	return []ToolProfile{ProfileReadOnly, ProfileAnalysis, ProfileEdit, ProfileFull}
}

// ParseToolProfile 解析并校验档位字符串。
//
// 空串 → ProfileFull。这是**向后兼容**的必需：存量工作流与未声明档位的
// Analyzer 输出都没有该字段，默认必须是「行为不变」而非「收紧」。
//
// 非法非空值 → 返回错误。**不静默降级**——借鉴 gh-aw CTR-023 的精神：
// 拒绝提供虚假安全感的配置。若把 "radonly"（拼写错误）静默当作 full，
// 作者会以为自己声明了只读限制而实际没有，这比不声明更危险。
func ParseToolProfile(s string) (ToolProfile, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return ProfileFull, nil
	}
	for _, p := range AllToolProfiles() {
		if string(p) == s {
			return p, nil
		}
	}
	return "", errs.Newf("invalid tool profile %q (valid: %s)", s, strings.Join(profileNames(), ", "))
}

// toolsForProfile 返回档位对应的工具白名单。
// 返回 nil 表示不过滤（ProfileFull / 空档位）。
func toolsForProfile(p ToolProfile) []string {
	if p == "" {
		return nil
	}
	return profileTools[p]
}

// profileNames 返回所有合法档位的字符串形式（已排序，便于错误提示稳定）。
func profileNames() []string {
	all := AllToolProfiles()
	names := make([]string, 0, len(all))
	for _, p := range all {
		names = append(names, string(p))
	}
	sort.Strings(names)
	return names
}
