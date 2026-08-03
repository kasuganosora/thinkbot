package workflow

// ============================================================================
// Review 阶段的错误分类
//
// 背景（真实事故 wf-b068495f484ef31e4b22e031）：
//   目标模式跑到第 2 轮审查时，Review SubAgent 的 LLM 调用超时：
//     review error at iteration 2: node "m1" review failed: subagent "":
//     LLM generate failed: openai: transport: context deadline exceeded
//   旧 reviewLoop 对 Review 返回的**任何** error 都直接 return 失败，于是这个纯粹的
//   基础设施抖动被当成「审查不通过」，把整个 12 节点 workflow 判 failed、下游 11 个
//   节点全部 skipped。而节点本身的产物是好的，只是没人来审。
//
// 判定原则：
//   「模型没能给出审查结论」≠「审查结论是不通过」。
//   前者是传输层问题，应当重试；后者才是业务判定，应当触发修复迭代。
//
// 为什么不直接用 llm.IsRetryableLLMError：
//   它依赖结构化 *LLMError（errors.As）。而 Review 的错误跨了 subagent 层被反复
//   fmt.Errorf 包装成纯字符串，结构体信息已丢失，errors.As 拿不到。这里做基于
//   errors.Is + 字符串特征的兜底判定。
// ============================================================================

import (
	"context"
	"errors"
	"net"
	"strings"

	"github.com/kasuganosora/thinkbot/llm"
)

// reviewInfraErrorPatterns 是基础设施类错误的字符串特征。
//
// 仅收录「明确与传输/服务可用性相关」的模式，不含任何可能表达业务判定的词，
// 避免把真正的审查失败误判为基础设施抖动而无限重试。
var reviewInfraErrorPatterns = []string{
	"context deadline exceeded",
	"timeout",
	"timed out",
	"connection refused",
	"connection reset",
	"broken pipe",
	"eof",
	"no such host",
	"tls handshake",
	"too many requests", // 429 限流
	"rate limit",
	"bad gateway",           // 502
	"service unavailable",   // 503
	"gateway timeout",       // 504
	"internal server error", // 500
	"overloaded",
	"try again",
	"temporarily unavailable",
	"stream error",
	"unexpected end of json input", // 响应被截断
}

// isReviewInfraError 判断 Review 的错误是否属于「模型/网络没能给出结论」这类可重试故障。
//
// 返回 true 表示应当重试审查本身（而非把它当成审查不通过去触发修复迭代）。
//
// 注意：主动取消（context.Canceled）不算基础设施抖动——那是用户/上层终止意图，
// 必须原样上抛，否则会把「终止」变成「重试」。
func isReviewInfraError(err error) bool {
	if err == nil {
		return false
	}

	// 主动取消：不可重试，交由调用方按终止语义处理。
	if errors.Is(err, context.Canceled) {
		return false
	}

	// 超时：最典型的可重试基础设施故障。
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}

	// 结构化 LLM 错误若已标注可重试，直接采信。
	if llm.IsRetryableLLMError(err) {
		return true
	}

	// 网络层错误：超时或临时性故障。
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}

	// 兜底：字符串特征匹配（错误已被跨层包装成纯文本时的唯一手段）。
	msg := strings.ToLower(err.Error())
	for _, p := range reviewInfraErrorPatterns {
		if strings.Contains(msg, p) {
			return true
		}
	}
	return false
}
