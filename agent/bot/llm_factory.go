package bot

import (
	"fmt"
	"time"

	"github.com/kasuganosora/thinkbot/config"
	"github.com/kasuganosora/thinkbot/llm"
	"github.com/kasuganosora/thinkbot/llm/anthropic"
	"github.com/kasuganosora/thinkbot/llm/google"
	"github.com/kasuganosora/thinkbot/llm/grok"
	"github.com/kasuganosora/thinkbot/llm/openai"
	"github.com/kasuganosora/thinkbot/util/errs"
	"github.com/kasuganosora/thinkbot/util/retry"
)

// llmClientTimeout / 重试图由 config.LLMClientConfig 驱动（见 CreateProvider 的
// llmCfg 参数与 buildRetryConfig）。这些参数原硬编码为 const，现集中到配置模块
// （config.DefaultLLMClientConfig + /api/config 的 llm.* 键），用户可在前端「系统配置」
// 页修改并持久化，默认值保持生产现状（20 分钟超时 / 4 重试 / 指数退避）。

// buildRetryConfig 从 config.LLMClientConfig 构造各 Provider 共用的重试配置。
// GLM/智谱经常在高负载时返回 429（访问量过大），若不重试会直接中断对话/工作流。
// 退避策略为指数退避 + 解析 Retry-After 头。
func buildRetryConfig(cfg config.LLMClientConfig) retry.Config {
	return retry.Config{
		MaxRetries: cfg.MaxRetries,
		Backoff: &retry.Backoff{
			Strategy: retry.StrategyExponential,
			Initial:  time.Duration(cfg.RetryInitialMS) * time.Millisecond,
			Factor:   cfg.RetryFactor,
			Max:      time.Duration(cfg.RetryMaxMS) * time.Millisecond,
			Jitter:   cfg.RetryJitter,
		},
		ShouldRetry:   retry.HTTPShouldRetry,
		GetRetryDelay: retry.HTTPGetRetryDelay,
	}
}

// ============================================================================
// LLM Factory — 从 config.ModelDef 构建实际的 llm.Provider 实例
//
// 依赖方向：bot → config + bot → llm
// config 只存纯数据（ModelDef），不导入 llm。
// ============================================================================

// CreateProvider 根据 ModelDef 创建对应的 llm.Provider 实例。
// llmCfg 来自 config.Builder.GetLLMClientConfig()（超时 / 重试 / 退避），集中可配。
func CreateProvider(def config.ModelDef, llmCfg config.LLMClientConfig) (llm.Provider, error) {
	timeout := time.Duration(llmCfg.ClientTimeoutSeconds) * time.Second
	retryCfg := buildRetryConfig(llmCfg)
	switch def.Provider {
	case "openai", "bigmodel":
		opts := []openai.Option{openai.WithAPIKey(def.APIKey)}
		if def.BaseURL != "" {
			opts = append(opts, openai.WithBaseURL(def.BaseURL))
		}
		if def.Provider == "bigmodel" || def.ChatPath != "" {
			// BigModel / 智谱 GLM 等仅兼容 Chat Completions API 的供应商
			opts = append(opts, openai.WithChatMode())
			if def.ChatPath != "" {
				opts = append(opts, openai.WithChatPath(def.ChatPath))
			}
		}
		opts = append(opts, openai.WithTimeout(timeout))
		opts = append(opts, openai.WithRetry(retryCfg))
		return openai.New(opts...), nil

	case "anthropic":
		opts := []anthropic.Option{anthropic.WithAPIKey(def.APIKey)}
		if def.BaseURL != "" {
			opts = append(opts, anthropic.WithBaseURL(def.BaseURL))
		}
		opts = append(opts, anthropic.WithTimeout(timeout))
		opts = append(opts, anthropic.WithRetry(retryCfg))
		return anthropic.New(opts...), nil

	case "google":
		opts := []google.Option{google.WithAPIKey(def.APIKey)}
		if def.BaseURL != "" {
			opts = append(opts, google.WithBaseURL(def.BaseURL))
		}
		opts = append(opts, google.WithTimeout(timeout))
		opts = append(opts, google.WithRetry(retryCfg))
		return google.New(opts...), nil

	case "grok":
		opts := []grok.Option{grok.WithAPIKey(def.APIKey)}
		if def.BaseURL != "" {
			opts = append(opts, grok.WithBaseURL(def.BaseURL))
		}
		opts = append(opts, grok.WithTimeout(timeout))
		opts = append(opts, grok.WithRetry(retryCfg))
		return grok.New(opts...), nil

	default:
		return nil, fmt.Errorf("bot: unknown LLM provider %q", def.Provider)
	}
}

// LLMBundle 是一个 Bot 的完整 LLM 实例集合。
type LLMBundle struct {
	// Main 主力 Provider（深度对话、工具调用）。
	Main llm.Provider

	// Light 低成本 Provider（标题提取、简单分类等）。
	// 为 nil 时表示与 Main 相同，调用方应回退到 Main。
	Light llm.Provider

	// Vision 多模态辅助 Provider（图片/音频/视频转文字）。
	// 为 nil 时表示未配置多模态辅助。
	Vision llm.Provider

	// MainDef / LightDef / VisionDef 对应的 ModelDef。
	MainDef   config.ModelDef
	LightDef  config.ModelDef
	VisionDef config.ModelDef
}

// HasLight 返回是否有独立的低成本 LLM。
func (b *LLMBundle) HasLight() bool {
	return b.Light != nil
}

// HasVision 返回是否有独立的多模态辅助 LLM。
func (b *LLMBundle) HasVision() bool {
	return b.Vision != nil
}

// MainSupportsMultimodal 返回主力模型是否支持多模态输入。
func (b *LLMBundle) MainSupportsMultimodal() bool {
	return b.MainDef.Multimodal
}

// CreateLLMBundle 从 config Store 为指定 Bot 构建 LLM 实例集。
//
// 读取数据库中 bot.<botID>.main 和 bot.<botID>.light，
// 找到对应的 llm.<llm_id> JSON 配置，创建 Provider 实例。
func CreateLLMBundle(b *config.Builder, botID string) (*LLMBundle, error) {
	assignment := b.GetBotLLMAssignment(botID)

	if assignment.Main == "" {
		return nil, fmt.Errorf("bot %q: no main LLM assigned", botID)
	}

	// LLM 客户端可靠性配置（超时 / 重试 / 退避），集中可配。
	llmCfg := b.GetLLMClientConfig()

	// 解析主力 LLM
	mainDef, ok := b.GetLLMModel(assignment.Main)
	if !ok {
		return nil, fmt.Errorf("bot %q: LLM %q not found in config", botID, assignment.Main)
	}
	// max_tokens 跟随「模型」本身（ModelDef.MaxTokens），由 provider 模型配置页按每模型设置。
	// 主模型若无视觉能力，外挂的视觉模型（vision）使用其各自的 MaxTokens，互不覆盖。
	mainProvider, err := CreateProvider(mainDef, llmCfg)
	if err != nil {
		return nil, errs.Wrapf(err, "bot %q: create main LLM", botID)
	}

	bundle := &LLMBundle{
		Main:    mainProvider,
		MainDef: mainDef,
	}

	// 解析低成本 LLM
	if assignment.Light != assignment.Main {
		lightDef, ok := b.GetLLMModel(assignment.Light)
		if ok {
			lightProvider, err := CreateProvider(lightDef, llmCfg)
			if err != nil {
				return nil, errs.Wrapf(err, "bot %q: create light LLM", botID)
			}
			bundle.Light = lightProvider
			bundle.LightDef = lightDef
			return bundle.withVision(b, botID, assignment, llmCfg)
		}
	}

	// Light 回退到 Main
	bundle.LightDef = mainDef
	return bundle.withVision(b, botID, assignment, llmCfg)
}

// withVision 尝试创建多模态辅助 Provider。
// 如果未配置 Vision，返回原 bundle 不变（nil error）。
// 如果配置了 Vision 但创建失败，返回错误。
func (b *LLMBundle) withVision(builder *config.Builder, botID string, assignment config.BotLLMAssignment, llmCfg config.LLMClientConfig) (*LLMBundle, error) {
	if assignment.Vision == "" {
		return b, nil
	}
	visionDef, ok := builder.GetLLMModel(assignment.Vision)
	if !ok {
		return b, nil
	}
	// 视觉模型使用自身的 ModelDef.MaxTokens（如 Grok / OpenAI 各自上限），
	// 不继承主模型（GLM 等）的 max_tokens。
	visionProvider, err := CreateProvider(visionDef, llmCfg)
	if err != nil {
		return nil, errs.Wrapf(err, "bot %q: create vision LLM", botID)
	}
	b.Vision = visionProvider
	b.VisionDef = visionDef
	return b, nil
}
