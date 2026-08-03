package workflow

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"
)

// TestIsReviewInfraError_Timeouts 验证各类超时被识别为可重试的基础设施错误。
//
// 这是事故 wf-b068495f484ef31e4b22e031 的直接回归：Review 的 LLM 调用超时
// 曾被当成「审查不通过」，把整个 workflow 判 failed、下游 11 节点全部 skipped。
func TestIsReviewInfraError_Timeouts(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"sentinel DeadlineExceeded", context.DeadlineExceeded},
		{"wrapped DeadlineExceeded", fmt.Errorf("review failed: %w", context.DeadlineExceeded)},
		{
			// 事故现场的真实错误串：跨 subagent 层包装后已丢失结构化类型，
			// 只能靠字符串特征识别。
			"real incident message",
			errors.New(`node "m1" review failed: subagent "": LLM generate failed: ` +
				`openai: transport: context deadline exceeded (last error: http request failed: ` +
				`Post "https://open.bigmodel.cn/api/coding/paas/v4/chat/completions": context deadline exceeded)`),
		},
		{"plain timeout wording", errors.New("request timed out after 60s")},
		{"429 rate limit", errors.New("openai: 429 Too Many Requests")},
		{"502 bad gateway", errors.New("upstream: 502 Bad Gateway")},
		{"503 unavailable", errors.New("Service Unavailable")},
		{"truncated json", errors.New("unexpected end of JSON input")},
		{"connection reset", errors.New("read tcp: connection reset by peer")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !isReviewInfraError(tc.err) {
				t.Fatalf("isReviewInfraError(%v) = false, want true", tc.err)
			}
		})
	}
}

// TestIsReviewInfraError_NotInfra 验证真正的业务/逻辑错误不会被误判为可重试。
//
// 误判的后果是把确定性失败反复重试 3 次，白等退避时间。
func TestIsReviewInfraError_NotInfra(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"nil", nil},
		// 主动取消是终止意图，必须原样上抛，不能变成「重试」。
		{"context canceled", context.Canceled},
		{"wrapped canceled", fmt.Errorf("stopped: %w", context.Canceled)},
		{"review verdict fail", errors.New("review failed: product does not satisfy requirement")},
		{"auth error", errors.New("invalid api key")},
		{"node not found", errors.New(`node "m1" not found`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if isReviewInfraError(tc.err) {
				t.Fatalf("isReviewInfraError(%v) = true, want false", tc.err)
			}
		})
	}
}

// timeoutNetErr 是一个 Timeout()==true 的 net.Error 实现。
type timeoutNetErr struct{}

func (timeoutNetErr) Error() string   { return "i/o deadline reached" }
func (timeoutNetErr) Timeout() bool   { return true }
func (timeoutNetErr) Temporary() bool { return true }

// TestIsReviewInfraError_NetError 验证走 net.Error 分支（错误串本身不含超时关键词）。
func TestIsReviewInfraError_NetError(t *testing.T) {
	var ne net.Error = timeoutNetErr{}
	if !isReviewInfraError(fmt.Errorf("dial: %w", ne)) {
		t.Fatal("net.Error with Timeout()==true must be treated as infra error")
	}
}

// TestReviewInfraRetryConstants 守住重试参数的合理区间。
//
// 次数为 1 等于关闭重试（回归事故）；过大则让确定性失败拖很久。
func TestReviewInfraRetryConstants(t *testing.T) {
	if reviewInfraMaxAttempts < 2 {
		t.Fatalf("reviewInfraMaxAttempts = %d, must be >= 2 or infra errors kill the whole workflow",
			reviewInfraMaxAttempts)
	}
	if reviewInfraMaxAttempts > 5 {
		t.Fatalf("reviewInfraMaxAttempts = %d, too many retries for a deterministic failure",
			reviewInfraMaxAttempts)
	}
	if reviewInfraRetryBaseDelay <= 0 || reviewInfraRetryBaseDelay > 10*time.Second {
		t.Fatalf("reviewInfraRetryBaseDelay = %v, out of sane range", reviewInfraRetryBaseDelay)
	}
}

// TestClearAnalyzeMessage 验证分析进度文案能被清空。
//
// 事故：workflow 已 failed 且 finishedAt 已写，AnalyzeMessage 仍残留
// 「正在调用模型分析需求（第 1/5 次尝试）」，前端把它当实时进度渲染 →
// 后端早已结束、UI 永远转圈的假卡死。
func TestClearAnalyzeMessage(t *testing.T) {
	wf := &Workflow{AnalyzeMessage: "正在调用模型分析需求（第 1/5 次尝试）"}
	wf.ClearAnalyzeMessage()
	if wf.AnalyzeMessage != "" {
		t.Fatalf("AnalyzeMessage = %q, want empty", wf.AnalyzeMessage)
	}
	// 幂等
	wf.ClearAnalyzeMessage()
	if wf.AnalyzeMessage != "" {
		t.Fatalf("AnalyzeMessage = %q after second call, want empty", wf.AnalyzeMessage)
	}
}
