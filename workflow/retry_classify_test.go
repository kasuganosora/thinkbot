package workflow

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// ============================================================================
// isNonRetryable 单元测试
// ============================================================================

func TestIsNonRetryable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		// —— GLM 1214 上下文超长 / 请求体非法 —— 确定性，不重试
		{
			name: "glm 1214 stream error",
			err:  errors.New(`subagent stream failed: openai: chat stream failed: stream HTTP error 400 on https://open.bigmodel.cn/api/coding/paas/v4/chat/completions: {"error":{"code":"1214","message":"messages 参数非法。请检查文档。"}}`),
			want: true,
		},
		{
			name: "glm 1214 with spaces in code",
			err:  errors.New(`stream HTTP error 400: {"error":{"code": "1214","message":"messages 参数非法"}}`),
			want: true,
		},
		{
			name: "messages 参数非法 without code",
			err:  errors.New("node execution failed: subagent stream failed: messages 参数非法。请检查文档。"),
			want: true,
		},
		// —— GLM 1301 内容安全审核 —— 确定性，不重试（与 1210/1214 同范式）
		{
			name: "glm 1301 content filter (no spaces)",
			err:  errors.New(`subagent stream failed: openai: chat stream failed: stream HTTP error 400 on https://open.bigmodel.cn/api/coding/paas/v4/chat/completions: {"error":{"code":"1301","message":"输出内容触发平台内容安全审核，请您更换话题或重新表达后再试。"}}`),
			want: true,
		},
		{
			name: "glm 1301 content filter (with spaces)",
			err:  errors.New(`stream HTTP error 400: {"error":{"code": "1301","message":"输出内容触发平台内容安全审核"}}`),
			want: true,
		},
		{
			name: "glm 1301 wrapped (multi-layer)",
			err: fmt.Errorf("workflow_node_n1: %w", errors.New(`subagent stream failed: openai: chat stream failed: stream HTTP error 400 on https://open.bigmodel.cn/...: {"error":{"code":"1301","message":"触发平台内容安全审核"}}`)),
			want: true,
		},
		{
			name: "prompt is too long",
			err:  errors.New("openai: prompt is too long for this model"),
			want: true,
		},
		// —— subagent 硬上限被强制终止 —— 确定性，不重试
		{
			name: "subagent hard cap killed",
			err:  errors.New("node \"n2\" execution failed: subagent 超过硬上限 30m0s 被强制终止（看门狗兜底）"),
			want: true,
		},
		{
			name: "exceeded hard timeout",
			err:  errors.New("LLM orchestrate exceeded hard timeout"),
			want: true,
		},
		// —— 主 Agent 墙钟硬上限 —— 确定性，不重试
		{
			name: "context deadline exceeded",
			err:  errors.New("context deadline exceeded"),
			want: true,
		},
		// —— 多层包装后仍应识别（errors.As 拿不到结构化类型，靠 loose 文本匹配）——
		{
			name: "wrapped 1214",
			err: fmt.Errorf("workflow_node_n1: %w", errors.New(`subagent stream failed: openai: chat stream failed: stream HTTP error 400 on https://open.bigmodel.cn/...: {"error":{"code":"1214","message":"messages 参数非法。请检查文档。"}}`)),
			want: true,
		},
		// —— 以下应为「可重试」（瞬时限流 / 网络抖动 / 普通错误）——
		{
			name: "transient 500",
			err:  errors.New("stream HTTP error 500: internal server error"),
			want: false,
		},
		{
			name: "rate limit 429 without quota exhaust",
			err:  errors.New("stream HTTP error 429: rate limit exceeded"),
			want: false,
		},
		{
			name: "generic execution error",
			err:  errors.New("boom"),
			want: false,
		},
		{
			name: "network error",
			err:  errors.New("connection refused"),
			want: false,
		},
		{
			name: "nil",
			err:  nil,
			want: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := isNonRetryable(c.err)
			if got != c.want {
				t.Errorf("isNonRetryable(%q) = %v, want %v", errText(c.err), got, c.want)
			}
		})
	}
}

func errText(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}

// ============================================================================
// 调度器集成测试：确定性失败只执行一次，不雪崩重试
// ============================================================================

// TestRunNode_ContextOverflowFailsFast 验证 GLM 1214 上下文超长错误：
// 即便 MaxRetries=2，节点也应立即 fail（只执行 1 次），而非重试 3 次。
func TestRunNode_ContextOverflowFailsFast(t *testing.T) {
	wf := NewWorkflow("wf", "req", []*DAGNode{
		{ID: "n1", Name: "task1", MaxRetries: 2},
	})
	wf.RebuildIndex()

	exec := &mockExecutor{
		execErr: errors.New(`subagent stream failed: openai: chat stream failed: stream HTTP error 400 on https://open.bigmodel.cn/api/coding/paas/v4/chat/completions: {"error":{"code":"1214","message":"messages 参数非法。请检查文档。"}}`),
	}
	s := newMockScheduler(wf, exec)

	node, _ := wf.GetNode("n1")
	node.Status = NodeReady

	s.runNode(context.Background(), node)

	if node.Status != NodeFailed {
		t.Errorf("expected failed, got %s", node.Status)
	}
	if exec.execCalls.Load() != 1 {
		t.Errorf("1214 must NOT be retried: expected 1 Execute call, got %d", exec.execCalls.Load())
	}
	if !strings.Contains(node.Error, "1214") {
		t.Errorf("expected error to mention 1214, got %s", node.Error)
	}
}

// TestRunNode_HardKillFailsFast 验证 subagent 30m 硬上限被强杀：
// 重试必然再次跑满 30m 被强杀，故应立即 fail（只执行 1 次）。
func TestRunNode_HardKillFailsFast(t *testing.T) {
	wf := NewWorkflow("wf", "req", []*DAGNode{
		{ID: "n1", Name: "task1", MaxRetries: 2},
	})
	wf.RebuildIndex()

	exec := &mockExecutor{
		execErr: errors.New("subagent 超过硬上限 30m0s 被强制终止（看门狗兜底）"),
	}
	s := newMockScheduler(wf, exec)

	node, _ := wf.GetNode("n1")
	node.Status = NodeReady

	s.runNode(context.Background(), node)

	if node.Status != NodeFailed {
		t.Errorf("expected failed, got %s", node.Status)
	}
	if exec.execCalls.Load() != 1 {
		t.Errorf("hard-kill must NOT be retried: expected 1 Execute call, got %d", exec.execCalls.Load())
	}
}

// TestRunNode_TransientErrorStillRetries 回归保护：
// 瞬时限流（429 非额度耗尽）仍应正常重试，不受确定性分类影响。
func TestRunNode_TransientErrorStillRetries(t *testing.T) {
	wf := NewWorkflow("wf", "req", []*DAGNode{
		{ID: "n1", Name: "task1", MaxRetries: 2},
	})
	wf.RebuildIndex()

	exec := &mockExecutor{
		execErrors:  []error{errors.New("stream HTTP error 429: rate limit exceeded")},
		execResults: []string{"ok"},
	}
	s := newMockScheduler(wf, exec)

	node, _ := wf.GetNode("n1")
	node.Status = NodeReady

	s.runNode(context.Background(), node)

	if node.Status != NodeCompleted {
		t.Errorf("expected completed after transient retry, got %s", node.Status)
	}
	if exec.execCalls.Load() < 2 {
		t.Errorf("transient error should be retried: expected >=2 calls, got %d", exec.execCalls.Load())
	}
}
