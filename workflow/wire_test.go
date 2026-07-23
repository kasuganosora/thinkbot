package workflow

import (
	"testing"

	"github.com/kasuganosora/thinkbot/config"
)

// TestAnalyzerMaxTokensResolution 锁定需求分析器 max_tokens 的推导优先级：
// 1) 显式配置 workflow.analyzer_max_tokens（>0）→ 直接用；
// 2) 否则回退到当前模型 ModelDef.MaxTokens；
// 3) 连模型定义都缺失 → 代码兜底 8192。
// 验证「不写死固定值，跟随当时使用的模型配置」这一诉求。
func TestAnalyzerMaxTokensResolution(t *testing.T) {
	// 核心推导函数：显式 > 模型 > 兜底
	if got := analyzerMaxTokens(0, &config.ModelDef{MaxTokens: 4096}); got != 4096 {
		t.Fatalf("model-driven expected 4096, got %d", got)
	}
	if got := analyzerMaxTokens(2048, &config.ModelDef{MaxTokens: 4096}); got != 2048 {
		t.Fatalf("explicit override expected 2048, got %d", got)
	}
	if got := analyzerMaxTokens(0, nil); got != 8192 {
		t.Fatalf("code fallback expected 8192, got %d", got)
	}
}

// TestEngineConfig_ModelDrivenMaxTokens 验证 nil Store（无运营配置）时，
// resolveEngineConfig 从 ModelDef 取 max_tokens 而非写死 8192。
func TestEngineConfig_ModelDrivenMaxTokens(t *testing.T) {
	// 基线：nil Store + 无 modelDef → 兜底 8192
	ec := resolveEngineConfig(nil, 0, nil)
	if ec.AnalyzerMaxTokens != 8192 {
		t.Fatalf("expected code fallback 8192, got %d", ec.AnalyzerMaxTokens)
	}

	// 模型 glm-5.2 在 provider 配置里 maxTokens=4096 → 应取到 4096
	ec = resolveEngineConfig(nil, 0, &config.ModelDef{Model: "glm-5.2", MaxTokens: 4096})
	if ec.AnalyzerMaxTokens != 4096 {
		t.Fatalf("expected model-driven 4096, got %d", ec.AnalyzerMaxTokens)
	}
}
