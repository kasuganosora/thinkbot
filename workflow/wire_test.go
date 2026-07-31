package workflow

import (
	"testing"

	"github.com/kasuganosora/thinkbot/config"
)

// TestAnalyzerMaxTokensResolution 锁定需求分析器 max_tokens 的推导优先级：
// 1) 显式配置 workflow.analyzer_max_tokens（>0）→ 直接用；
// 2) 否则回退到当前模型 ModelDef.MaxTokens；
// 3) 连模型定义都缺失 → 代码兜底 analyzerMaxTokensFallback。
// 验证「不写死固定值，跟随当时使用的模型配置」这一诉求。
func TestAnalyzerMaxTokensResolution(t *testing.T) {
	// 核心推导函数：显式 > 模型 > 兜底
	if got := analyzerMaxTokens(0, &config.ModelDef{MaxTokens: 4096}); got != 4096 {
		t.Fatalf("model-driven expected 4096, got %d", got)
	}
	if got := analyzerMaxTokens(2048, &config.ModelDef{MaxTokens: 4096}); got != 2048 {
		t.Fatalf("explicit override expected 2048, got %d", got)
	}
	if got := analyzerMaxTokens(0, nil); got != analyzerMaxTokensFallback {
		t.Fatalf("code fallback expected %d, got %d", analyzerMaxTokensFallback, got)
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
