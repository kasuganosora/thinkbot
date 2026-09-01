package workflow

import (
	"errors"
	"strings"
)

// ============================================================================
// 节点结果类别（Outcome）
//
// 节点失败时只有一个 error 字符串，无法区分：
//   - 做完了但质量不行（该走重试 / Review 迭代）
//   - **没工具所以没做**（重试一百次也没用）
//   - 无事可做（不是失败）
//   - 上游数据缺失（问题在上游，不在本节点）
//
// 这几种情况的处置完全不同，混为一谈的代价是：缺工具的节点被反复重跑到
// 迭代耗尽，白白烧掉整条工作流的预算，最后还是失败。
//
// Outcome 由 Review SubAgent 自报（LLM 自己最清楚它做没做成），
// 与 Review 的 passed 判定**正交**：一个节点可能 passed=true（产物合格）
// 但 outcome=partial（只覆盖了一部分），这两件事都要能表达。
// ============================================================================

// NodeOutcome 节点自报的结果类别。
type NodeOutcome string

const (
	// OutcomeOK 正常完成。零值等价于此，保持存量数据兼容。
	OutcomeOK NodeOutcome = "ok"
	// OutcomeNoop 无事可做（如匹配范围内没有需要处理的变更）。
	// 这是**正常结果**，不是失败——但工作流级别需要区分「全部 noop」与「真做了事」，
	// 否则整条工作流会显示 completed 而实际什么都没做。
	OutcomeNoop NodeOutcome = "noop"
	// OutcomePartial 只完成了任务的一部分。
	// 有产物但完整度不足，与 failed 不同——产物可能仍有价值。
	OutcomePartial NodeOutcome = "partial"
	// OutcomeMissingTool 缺少完成任务所必需的工具（档位不足或工具不可用）。
	// 重跑不会改善：工具缺失是环境事实，不是随机故障。
	OutcomeMissingTool NodeOutcome = "missing_tool"
	// OutcomeMissingData 缺少必需的输入数据（通常意味着上游产物不可用）。
	// 问题在上游，本节点无力解决。
	OutcomeMissingData NodeOutcome = "missing_data"
)

// ParseNodeOutcome 解析结果类别。空串返回 OutcomeOK 且 ok=true（向后兼容）。
// 非法非空值返回 ok=false——调用方应记录告警而非静默忽略。
func ParseNodeOutcome(s string) (NodeOutcome, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case "":
		return OutcomeOK, true
	case "ok", "noop", "partial", "missing_tool", "missing_data":
		return NodeOutcome(s), true
	default:
		return OutcomeOK, false
	}
}

// IsBlocked 是否属于「硬失败」——没有产出可用产物。
//
// 这类结果既不该重试（重跑无用），也不该消耗 Review 迭代额度
// （迭代同样解决不了缺工具/缺数据）。
func (o NodeOutcome) IsBlocked() bool {
	return o == OutcomeMissingTool || o == OutcomeMissingData
}

// IsDegraded 是否属于「降级完成」——有产物但不完整。
// 工作流终态需要区分这类与完全成功，避免用户误以为全部达成。
func (o NodeOutcome) IsDegraded() bool {
	return o == OutcomePartial || o == OutcomeNoop
}

// String 便于日志输出。
func (o NodeOutcome) String() string {
	if o == "" {
		return string(OutcomeOK)
	}
	return string(o)
}

// errMissingTool 节点因缺少必需工具而无法完成。
//
// 作为哨兵错误接入 isNonRetryable（retry_classify.go），
// 使「缺工具」走与「额度耗尽」相同的快速失败路径：
// 不重试、不消耗迭代额度、不级联空转。
//
// 刻意**不**自动放宽工具档位来"自愈"这类失败——那等于给最小权限防线
// 开一个自动化后门。是否提档由人决定，诊断只给建议（见 heal.go 的
// capability 类别与 SuggestedProfile）。
var errMissingTool = errors.New("node cannot finish: required tool is unavailable under its tool profile")

// errMissingData 节点因缺少必需的输入数据而无法完成。
//
// 与 errMissingTool 同理走快速失败：上游没产出可用数据时，
// 重跑本节点只是重复一次注定失败的尝试。
var errMissingData = errors.New("node cannot finish: required input data is unavailable")

// ErrMissingTool 暴露哨兵错误供调用方判定（errors.Is）。
func ErrMissingTool() error { return errMissingTool }

// ErrMissingData 暴露哨兵错误供调用方判定（errors.Is）。
func ErrMissingData() error { return errMissingData }

// errForBlocked 把 blocked 类的结果类别映射为对应的哨兵错误。
// 非 blocked 类别返回 nil。
func errForBlocked(o NodeOutcome) error {
	switch o {
	case OutcomeMissingTool:
		return errMissingTool
	case OutcomeMissingData:
		return errMissingData
	default:
		return nil
	}
}
