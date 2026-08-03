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

// llmClientTimeout 是 LLM Provider 底层 HTTP 客户端的超时。
//
// 全局默认（util/http）仅 30s，对 bigmodel/智谱 等首字节较慢的模型会导致
// "Client.Timeout exceeded while awaiting headers" 而回复失败。
//
// 为什么需要给到 20 分钟：
//   - 该超时是**整个请求**的上限（含等待首字节）。**非流式**请求
//     （`llm.OrchestrateGenerate`，workflow 的 SubAgent 走这条路）必须等模型把整段回复
//     生成完才返回响应头，所以「生成大量代码 / 长篇审查报告」这类调用天然耗时很久。
//   - 实测 5 分钟明显不够：workflow 节点执行曾连续 3 次尝试全部精准卡在 5 分钟超时
//     （17:01 / 17:06 / 17:12），把本可完成的任务判为失败，并级联 skip 掉全部下游节点。
//   - 注意：SSE 看门狗只保护**流式**路径，对非流式的 OrchestrateGenerate 不生效，
//     因此这里不能依赖「看门狗会兜底」而把超时压短。
//
// 过长的整体超时不会掩盖真实 stall：流式路径由 SSE 看门狗按「无输出时长」判卡死，
// 而非流式路径本就没有比「整体超时」更细的可用信号。
const llmClientTimeout = 20 * time.Minute

// llmRetryMaxRetries 是 LLM Provider 遇到可恢复错误（429 限流 / 5xx）时的重试次数。
// 非流式请求由底层 httputil 自动重试；流式（SSE）请求在连接建立阶段（首字节前）
// 遇到 429/5xx 时重试整条流是安全的。退避策略为指数退避 + 解析 Retry-After 头。
const llmRetryMaxRetries = 4

// llmRetryConfig 返回各 Provider 共用的重试配置。
// GLM/智谱经常在高负载时返回 429（访问量过大），若不重试会直接中断对话/工作流。
func llmRetryConfig() retry.Config {
	return retry.LLMRetryConfig(llmRetryMaxRetries)
}

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
		opts = append(opts, openai.WithTimeout(llmClientTimeout))
		opts = append(opts, openai.WithRetry(llmRetryConfig()))
		return openai.New(opts...), nil

	case "anthropic":
		opts := []anthropic.Option{anthropic.WithAPIKey(def.APIKey)}
		if def.BaseURL != "" {
			opts = append(opts, anthropic.WithBaseURL(def.BaseURL))
		}
		opts = append(opts, anthropic.WithTimeout(llmClientTimeout))
		opts = append(opts, anthropic.WithRetry(llmRetryConfig()))
		return anthropic.New(opts...), nil

	case "google":
		opts := []google.Option{google.WithAPIKey(def.APIKey)}
		if def.BaseURL != "" {
			opts = append(opts, google.WithBaseURL(def.BaseURL))
		}
		opts = append(opts, google.WithTimeout(llmClientTimeout))
		opts = append(opts, google.WithRetry(llmRetryConfig()))
		return google.New(opts...), nil

	case "grok":
		opts := []grok.Option{grok.WithAPIKey(def.APIKey)}
		if def.BaseURL != "" {
			opts = append(opts, grok.WithBaseURL(def.BaseURL))
		}
		opts = append(opts, grok.WithTimeout(llmClientTimeout))
		opts = append(opts, grok.WithRetry(llmRetryConfig()))
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
			return bundle.withVision(b, botID, assignment)
		}
	}

	// Light 回退到 Main
	bundle.LightDef = mainDef
	return bundle.withVision(b, botID, assignment)
}

// withVision 尝试创建多模态辅助 Provider。
// 如果未配置 Vision，返回原 bundle 不变（nil error）。
// 如果配置了 Vision 但创建失败，返回错误。
func (b *LLMBundle) withVision(builder *config.Builder, botID string, assignment config.BotLLMAssignment) (*LLMBundle, error) {
	if assignment.Vision == "" {
		return b, nil
	}
	visionDef, ok := builder.GetLLMModel(assignment.Vision)
	if !ok {
		return b, nil
	}
	visionProvider, err := CreateProvider(visionDef)
	if err != nil {
		return nil, errs.Wrapf(err, "bot %q: create vision LLM", botID)
	}
	b.Vision = visionProvider
	b.VisionDef = visionDef
	return b, nil
}
