package workflow

import (
	"errors"
	"strings"
)

// ============================================================================
// 确定性失败识别（节点层重试熔断）
//
// 解决的问题（真实事故 wf-75adc58e5e08d704411a3fd0，2026-08-21）：
//
//	节点执行错误经 retry.Do 重试，但 ShouldRetry 只排除了「额度耗尽」，
//	对以下两类「重试必然再次失败」的确定性错误无脑重试 max_retries=2：
//	  · GLM 400 code 1214「messages 参数非法」—— 同一任务上下文必然再次越界；
//	  · GLM 400 code 1210「API 调用参数有误」—— 工具 schema 缺 properties 等
//	    请求体结构问题，相同请求必然再次被拒（真实根因见 llm/openai/chat.go）；
//	  · subagent 30m 硬上限被强制终止 —— 同模型同预算重试必再次跑满 30m 被硬杀。
//	→ 单节点最坏 3×30m=90min 卡在 running，前端一直轮询转圈。
//
// 新行为：isNonRetryable 在 ShouldRetry 中一并排除，节点立即 fail 而非雪崩重试。
//
// 与 isQuotaExhausted 同样的「Loose」哲学：错误链跨 节点重试 → SubAgent →
// LLM 流式多层包装后已退化为纯文本，errors.As 拿不到结构化类型，必须用字符串宽松匹配。
// ============================================================================

// nonRetryablePatterns 是「重试必然再次失败」的确定性错误特征（小写匹配）。
//
// 收录标准：该特征只可能出现在**确定性**失败中，即给定相同任务与预算，
// 重试必然产出相同结果。任何可能表达「瞬时限流 / 可恢复抖动」的措辞都**不得**加入，
// 否则会把可恢复的故障误判为终态而放弃重试。
var nonRetryablePatterns = []string{
	// —— GLM 上下文超长 / 请求体非法（HTTP 400 code 1214） ——
	// 同一任务上下文必然再次越界，重试无意义。
	`"code":"1214"`,
	`"code": "1214"`,
	"messages 参数非法",
	// —— GLM 工具/请求参数非法（HTTP 400 code 1210 "API 调用参数有误"） ——
	// 真实事故 wf-75adc58e5e08d704411a3fd0：延迟加载工具被剥离 Parameters 后
	// 发出缺 properties 的 {"type":"object"}，GLM 整请求拒收。该错误对相同请求
	// 必然复现，重试无意义；修复见 llm/openai/chat.go normalizeToolParameters。
	`"code":"1210"`,
	`"code": "1210"`,
	"api 调用参数有误",
	// —— GLM 内容安全审核（HTTP 400 code 1301）——
	// 输出触发平台内容安全策略被拒。该判定对相同 prompt 必然复现（同模型同预算
	// 重试必再次被过滤），重试纯属浪费、还会把单次调用拖到数分钟（GLM 对审核类
	// 请求常挂起到超时而非即时 400）。与 1210/1214 同属「确定性客户端错误」，
	// 不应经 workflow 节点层无脑重试。
	`"code":"1301"`,
	`"code": "1301"`,
	"内容安全审核",
	"触发平台内容",
	"prompt is too long",
	"prompt too long",
	"input is too long for requested model",
	"exceeded context length",
	"exceeded the context length",
	// —— subagent 硬上限被强制终止（看门狗兜底） ——
	// 过重任务跑不满预算，同模型同预算重试必再次跑满硬上限被强杀。
	"超过硬上限",
	"被强制终止",
	"exceeded hard timeout",
	"exceeded hard ceiling",
	// —— 主 Agent 墙钟硬上限（llmroute HardTimeout）触发 ——
	// 编排总时长超限被强制终止，重试同一节点必再次撞同一墙钟。
	"context deadline exceeded",
}

// isNonRetryable 判断错误是否为「重试必然再次失败」的确定性失败，
// 节点层不应无脑重试（否则放大雪崩、把工作流卡在 running 数十分钟）。
func isNonRetryable(err error) bool {
	if err == nil {
		return false
	}
	// 额度耗尽已由专用判定覆盖（其判定更严格，仅限 429/403 + 明确额度特征）。
	if isQuotaExhausted(err) {
		return true
	}
	// 工具缺失是**环境事实**而非随机故障：只要节点档位不变，重跑多少次
	// 都拿不到工具。与额度耗尽同理，走快速失败，别把预算烧在注定失败的重试上。
	//
	// 用 errors.Is 而非文本匹配——这是类型化的哨兵错误，文本匹配会把
	// 恰好包含同字样式的无关错误一起吞掉。
	//
	// ⚠️ 关于生效范围（2026-09-01 核查确认）：本函数只被 retry.Do 的
	// ShouldRetry 调用，而 retry.Do 的执行体**只包裹 Execute /
	// ExecuteWithFeedback**，不包含 reviewLoop（见 scheduler.go 的 runNode）。
	// 因此当前 errMissingTool 实际由 reviewLoop 产生、也不会进入重试——
	// 「不重试」是靠它压根不在重试范围内达成的，不是靠本分支。
	//
	// 保留本分支的理由：一旦执行阶段自身也能产出这类错误（例如 subagent
	// 在编排中检测到工具缺失），或 reviewLoop 日后被移进 retry.Do，
	// 这里就是唯一能拦住无效重试的地方。删掉它会留下一个静默的退化路径。
	if errors.Is(err, errMissingTool) {
		return true
	}
	// 上游数据缺失：问题在上游，重跑本节点拿不到数据。
	if errors.Is(err, errMissingData) {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, p := range nonRetryablePatterns {
		if strings.Contains(msg, p) {
			return true
		}
	}
	return false
}
