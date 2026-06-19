package bot

import (
	"fmt"

	"github.com/kasuganosora/thinkbot/config"
	"github.com/kasuganosora/thinkbot/llm"
	"github.com/kasuganosora/thinkbot/llm/anthropic"
	"github.com/kasuganosora/thinkbot/llm/google"
	"github.com/kasuganosora/thinkbot/llm/grok"
	"github.com/kasuganosora/thinkbot/llm/openai"
	"github.com/kasuganosora/thinkbot/util/errs"
)

// ============================================================================
// LLM Factory — 从 config.ModelDef 构建实际的 llm.Provider 实例
//
// 依赖方向：bot → config + bot → llm
// config 只存纯数据（ModelDef），不导入 llm。
// ============================================================================

// CreateProvider 根据 ModelDef 创建对应的 llm.Provider 实例。
func CreateProvider(def config.ModelDef) (llm.Provider, error) {
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
		return openai.New(opts...), nil

	case "anthropic":
		opts := []anthropic.Option{anthropic.WithAPIKey(def.APIKey)}
		if def.BaseURL != "" {
			opts = append(opts, anthropic.WithBaseURL(def.BaseURL))
		}
		return anthropic.New(opts...), nil

	case "google":
		opts := []google.Option{google.WithAPIKey(def.APIKey)}
		if def.BaseURL != "" {
			opts = append(opts, google.WithBaseURL(def.BaseURL))
		}
		return google.New(opts...), nil

	case "grok":
		opts := []grok.Option{grok.WithAPIKey(def.APIKey)}
		if def.BaseURL != "" {
			opts = append(opts, grok.WithBaseURL(def.BaseURL))
		}
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

	// 解析主力 LLM
	mainDef, ok := b.GetLLMModel(assignment.Main)
	if !ok {
		return nil, fmt.Errorf("bot %q: LLM %q not found in config", botID, assignment.Main)
	}
	mainProvider, err := CreateProvider(mainDef)
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
			lightProvider, err := CreateProvider(lightDef)
			if err != nil {
				return nil, errs.Wrapf(err, "bot %q: create light LLM", botID)
			}
			bundle.Light = lightProvider
			bundle.LightDef = lightDef
			return bundle.withVision(b, botID, assignment), nil
		}
	}

	// Light 回退到 Main
	bundle.LightDef = mainDef
	return bundle.withVision(b, botID, assignment), nil
}

// withVision 尝试创建多模态辅助 Provider。
// 如果未配置 Vision 或配置无效，返回原 bundle 不变。
func (b *LLMBundle) withVision(builder *config.Builder, botID string, assignment config.BotLLMAssignment) *LLMBundle {
	if assignment.Vision == "" {
		return b
	}
	visionDef, ok := builder.GetLLMModel(assignment.Vision)
	if !ok {
		return b
	}
	visionProvider, err := CreateProvider(visionDef)
	if err != nil {
		return b
	}
	b.Vision = visionProvider
	b.VisionDef = visionDef
	return b
}
