package workflow

import (
	"testing"
	"time"

	"github.com/kasuganosora/thinkbot/config"
)

// TestAnalyzerMaxTokensResolution 锁定「单一来源」：分析器 max_tokens 只跟随
// bot 所选模型的 MaxTokens，仅在拿不到模型定义时才走代码兜底。
// 曾存在的 workflow.analyzer_max_tokens 独立旋钮优先级高于模型能力，
// 被播种成 8192 后把 glm-5.2（128K）压死并截断 DAG JSON——不得回归。
func TestAnalyzerMaxTokensResolution(t *testing.T) {
	// 换模型即自动跟随
	if got := analyzerMaxTokens(&config.ModelDef{MaxTokens: 4096}); got != 4096 {
		t.Fatalf("model-driven expected 4096, got %d", got)
	}
	if got := analyzerMaxTokens(&config.ModelDef{Model: "glm-5.2", MaxTokens: 128000}); got != 128000 {
		t.Fatalf("model-driven expected 128000, got %d", got)
	}
	// 模型定义缺失/未填 maxTokens → 兜底
	if got := analyzerMaxTokens(nil); got != analyzerMaxTokensFallback {
		t.Fatalf("code fallback expected %d, got %d", analyzerMaxTokensFallback, got)
	}
	if got := analyzerMaxTokens(&config.ModelDef{Model: "x"}); got != analyzerMaxTokensFallback {
		t.Fatalf("code fallback expected %d, got %d", analyzerMaxTokensFallback, got)
	}
}

// TestAnalyzerMaxTokens_NoSeparateKnob 防回归：配置层不得再出现与模型能力
// 竞争的分析器预算旋钮。若将来重新引入，此断言会失败以提醒收敛到单一来源。
func TestAnalyzerMaxTokens_NoSeparateKnob(t *testing.T) {
	for _, spec := range config.WorkflowMetaSpecs() {
		if spec.Key == "workflow.analyzer_max_tokens" {
			t.Fatalf("analyzer budget must follow the model's maxTokens, not a separate setting key %q", spec.Key)
		}
	}
}

// TestAnalyzerMaxTokensFallback_FitsThinkingPlusJSON 锁定兜底预算不得退回到
// 曾造成线上故障的 8192：该预算由「思考 + JSON 正文」共享，思考型模型
// （GLM-4.6+ 默认开启 thinking）先吃掉大半后会把 DAG JSON 硬截断，
// 导致 analyzer 连续解析失败、workflow 直接 failed。
func TestAnalyzerMaxTokensFallback_FitsThinkingPlusJSON(t *testing.T) {
	if analyzerMaxTokensFallback <= 8192 {
		t.Fatalf("fallback %d too small: must leave room for reasoning tokens plus the DAG JSON body",
			analyzerMaxTokensFallback)
	}
}

// TestEngineConfig_ModelDrivenMaxTokens 验证 nil Store（无运营配置）时，
// resolveEngineConfig 从 ModelDef 取 max_tokens 而非写死 8192。
func TestEngineConfig_ModelDrivenMaxTokens(t *testing.T) {
	// 基线：nil Store + 无 modelDef → 代码兜底
	ec := resolveEngineConfig(nil, 0, nil)
	if ec.AnalyzerMaxTokens != analyzerMaxTokensFallback {
		t.Fatalf("expected code fallback %d, got %d", analyzerMaxTokensFallback, ec.AnalyzerMaxTokens)
	}

	// 模型 glm-5.2 在 provider 配置里 maxTokens=4096 → 应取到 4096
	ec = resolveEngineConfig(nil, 0, &config.ModelDef{Model: "glm-5.2", MaxTokens: 4096})
	if ec.AnalyzerMaxTokens != 4096 {
		t.Fatalf("expected model-driven 4096, got %d", ec.AnalyzerMaxTokens)
	}
}

// TestSetup_DelegateTimeoutIndependentOfToolMgr 防回归：委托超时与工具步数
// 必须与 ToolMgr 是否存在**解耦**。
//
// 线上事故（2026-08-04）：SetDelegateTimeout(10min) / SetDefaultToolSteps(25)
// 被写在 `if cfg.ToolMgr != nil` 分支内，导致 workflow_service 实例（ToolMgr=nil，
// 服务于启动 Recover / Sweeper / UI 重试）保持 subagent 默认的 120s。
// 同一条工作流被 Recover 接管后超时从 10min 骤降到 120s，
// 线上出现 30 次 elapsed=120.000s 的硬超时。超时预算与「有没有工具」无关。
func TestSetup_DelegateTimeoutIndependentOfToolMgr(t *testing.T) {
	// ToolMgr 为 nil（即 workflow_service 的装配方式）
	_, saMgr := Setup(WireConfig{Model: "test-model"})
	if saMgr == nil {
		t.Fatal("Setup returned nil SubAgentManager")
	}
	defer saMgr.CloseAll()

	timeout := saMgr.DelegateTimeout()
	if timeout != 10*time.Minute {
		t.Errorf("delegate timeout = %v, want 10m even when ToolMgr is nil "+
			"(timeout budget must not depend on tool availability)", timeout)
	}
}

// TestSetup_NilLoggerDoesNotPanic 防回归：WireConfig 文档承诺 Logger 可为 nil，
// 但 Setup 内多处直接调用 cfg.Logger.Infow/Warnw——此前缺少兜底会 panic。
func TestSetup_NilLoggerDoesNotPanic(t *testing.T) {
	mgr, saMgr := Setup(WireConfig{Model: "test-model"}) // Logger 未设置
	if mgr == nil || saMgr == nil {
		t.Fatal("Setup must succeed with a nil logger")
	}
	saMgr.CloseAll()
}
