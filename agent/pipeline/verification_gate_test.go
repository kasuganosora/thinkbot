package pipeline

import (
	"context"
	"testing"

	"github.com/kasuganosora/thinkbot/agent/core"
)

// TestRequiresVerification 验证环境类问题的确定性分类。
func TestRequiresVerification(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		expected bool
	}{
		// 应判定为需要核实（true）
		{"安装类", "这个环境里有没有安装 git？", true},
		{"是否存在", "检查一下 /usr/bin/git 是否存在", true},
		{"运行吗", "redis 服务在运行吗", true},
		{"shell 惯用法 which", "which git", true},
		{"command -v", "command -v python3", true},
		{"版本查询", "python 版本是多少", true},
		{"内核版本", "当前内核版本是什么", true},
		{"系统信息", "帮我看一下系统信息", true},
		{"磁盘空间", "磁盘空间还够吗", true},
		{"端口占用", "8080 端口被占用了吗", true},
		{"环境变量", "环境变量 PATH 里有哪些", true},
		{"包管理器", "apt 还能用吗", true},
		{"docker 镜像", "sandbox 里有没有这个 docker 镜像", true},

		// 不应判定（false）——避免误伤普通对话/知识问答
		{"知识问答", "git 怎么用？", false},
		{"概念解释", "什么是版本控制？", false},
		{"闲聊", "你好，今天天气不错", false},
		{"代码问题无实体", "这个函数的版本号在哪里定义？", false},
		{"空字符串", "", false},
		{"纯学习", "python 和 go 有什么区别", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := requiresVerification(tc.text)
			if got != tc.expected {
				t.Errorf("requiresVerification(%q) = %v, want %v", tc.text, got, tc.expected)
			}
		})
	}
}

// TestVerificationGateMiddleware_SetsFlag 验证命中时打标记。
func TestVerificationGateMiddleware_SetsFlag(t *testing.T) {
	mw := VerificationGateMiddleware(NewVerificationGateConfig())
	dummy := &core.StageFunc{
		StageName: "llm",
		Fn: func(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
			return env, nil
		},
	}
	wrapped := mw(dummy)

	env := core.NewEnvelope(core.Message{Channel: "ch", ID: "1", Text: "这个环境有没有安装 git？"})
	result, err := wrapped.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v, ok := result.Get("verify.required"); !ok || v != true {
		t.Errorf("expected verify.required=true, got %v (ok=%v)", v, ok)
	}
}

// TestVerificationGateMiddleware_SkipsNormal 验证普通问题不打标记。
func TestVerificationGateMiddleware_SkipsNormal(t *testing.T) {
	mw := VerificationGateMiddleware(NewVerificationGateConfig())
	dummy := &core.StageFunc{
		StageName: "llm",
		Fn: func(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
			return env, nil
		},
	}
	wrapped := mw(dummy)

	env := core.NewEnvelope(core.Message{Channel: "ch", ID: "2", Text: "git 怎么用？"})
	result, err := wrapped.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v, ok := result.Get("verify.required"); ok && v == true {
		t.Error("normal question should not set verify.required")
	}
}

// TestVerificationGateMiddleware_Disabled 验证禁用时为 no-op。
func TestVerificationGateMiddleware_Disabled(t *testing.T) {
	mw := VerificationGateMiddleware(VerificationGateConfig{Enabled: false})
	dummy := &core.StageFunc{StageName: "dummy", Fn: func(ctx context.Context, env *core.Envelope) (*core.Envelope, error) { return env, nil }}
	if wrapped := mw(dummy); wrapped.Name() != dummy.Name() {
		t.Error("disabled middleware should return original stage")
	}
}
