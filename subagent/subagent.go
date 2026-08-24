package subagent

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/kasuganosora/thinkbot/llm"
	"github.com/kasuganosora/thinkbot/util/errs"
)

// ============================================================================
// SubAgent — 轻量级隔离 Agent
//
// SubAgent 继承主 Agent 的 LLM Provider 和模型设置，
// 但维护完全独立的对话上下文。
//
// 核心特性：
//   - 上下文隔离：每个 SubAgent 的对话历史互不影响
//   - 无记忆：不持久化任何数据，Close 后上下文丢弃
//   - 滑动窗口：自动管理上下文长度，防止 token 爆炸
//   - 只能被主 Agent 程序调用，不监听任何 Channel
//
// 典型用法：
//
//	bundle := bot.CreateLLMBundle(...)  // 主 Agent 的 LLM
//	sub := subagent.New(bundle.Main, "glm-5.2",
//	    subagent.WithSystemPrompt("你是一个代码审查专家"),
//	    subagent.WithMaxTurns(5),
//	)
//	defer sub.Close()
//
//	reply, err := sub.Chat(ctx, "审查这段代码: ...")
// ============================================================================

// defaultSubagentToolSteps 是子 Agent 带工具执行回路时的默认最大 LLM 步数预算。
// 仅当通过 WithTools 注入了可执行工具、且 toolSteps != 0 时启用多步编排回路。
const defaultSubagentToolSteps = 20

// defaultChatHardTimeout 是持久 subagent（Spawn/Chat）单次交互的墙钟硬上限。
// 防止工具挂死/LLM 假活时 Chat 无限阻塞调用方 goroutine（该路径此前无任何超时/看门狗）。
// 可用 WithChatTimeout 覆盖。
const defaultChatHardTimeout = 10 * time.Minute

// SubAgent 是一个上下文隔离的轻量 Agent。
// 它复用主 Agent 的 LLM Provider，但维护独立的对话历史。
//
// 工具能力：若通过 WithTools 注入了带 Execute 处理函数的工具（通常由
// SubAgentManager 从主 Agent 的 ToolManager 解析后注入），且 toolSteps != 0，
// 则 Chat/Stream 会走 llm.OrchestrateGenerate/Stream 多步回路——自动执行工具、
// 将结果喂回模型，直到模型停止请求工具。子 Agent 因此能像主 Agent 一样使用
// 工作空间（exec/读/写/列目录等），只是不能 spawn 子 Agent（由 spawn 工具的
// scope 排除，防止套娃）。
type SubAgent struct {
	mu sync.Mutex

	// LLM 配置（从主 Agent 继承）
	provider llm.Provider
	model    string
	system   string
	temp     float64
	// frequencyPenalty/presencePenalty 重复抑制（GLM-5.x 推荐 0.1/0.05），
	// 抑制 agent 长工具链下反复输出相同 token 的退化。
	frequencyPenalty float64
	presencePenalty  float64
	maxTokens        int

	// 上下文管理
	ctxMgr     *ContextManager
	totalTurns int

	// callTimeout 本次调用的超时覆盖（0 = 使用 SubAgentManager 的默认 delegateTimeout）。
	// 由 WithCallTimeout 设置，仅对 Delegate/DelegateMany 的单次调用生效。
	callTimeout time.Duration

	// stuckTimeout 卡死看门狗阈值（0 = 使用默认 defaultDelegateStuckTimeout）。
	// 由 WithStuckTimeout 设置，仅对 DelegateStream（流式委托）生效。
	stuckTimeout time.Duration

	// chatTimeout 持久 subagent（Spawn/Chat）单次交互的墙钟硬上限。
	// 0 = 使用默认 defaultChatHardTimeout。防止工具挂死/LLM 假活时
	// Chat 无限阻塞调用方 goroutine（此前该路径无任何超时/看门狗）。
	// 由 WithChatTimeout 覆盖。
	chatTimeout time.Duration

	closed bool

	// 元数据
	id   string
	name string

	// 额外生成参数
	extraTools     []llm.Tool
	responseFormat *llm.ResponseFormat

	// toolSteps 是带工具执行回路时的最大 LLM 步数预算（>0 启用 OrchestrateGenerate/Stream）。
	// 0 = 使用包默认 defaultSubagentToolSteps。仅当 extraTools 非空时生效。
	toolSteps int

	// skipTools 为 true 时，SubAgentManager.ResolveTools 即使解析到了工具也不注入。
	// 用于 Analyzer 等纯 LLM 任务（只需输出 JSON，不需要工具能力），
	// 避免被注入工具后误走 OrchestrateStream 多步编排循环导致卡死或延迟。
	skipTools bool

	// compactor 可选的语义压缩器（LLM 摘要）。非空时在每步编排前检测 token
	// 溢出并自动用 LLM 摘要旧消息，避免 context 爆炸把 subagent 养到硬上限看门狗
	// （默认 30min）仍无法收敛。注意：语义压缩按「对话轮次」切分旧头部，单轮委托
	// （如工作流「审查 X 目录」只有 1 个 user 轮）几乎触发不到——它主要服务多轮场景。
	// 单轮工具循环的 context 爆炸靠下方的 reducer（工具输出缩减）压住。
	// 建议通过 WithAutoCompact 让每个 subagent 各自持有一个独立实例——Compactor
	// 内部有 previousSummary / compactionCount 等跨调用状态，跨 subagent 共享会
	// 互相污染（DelegateMany 并发场景下尤其危险）。
	compactor *llm.Compactor

	// autoCompact 为 true 且未显式 WithCompactor 时，New 自动建一个独立
	// Compactor（DefaultCompactionConfig）。经 manager 的 defaultOpts 注入，
	// 让所有委托/派生的 subagent 默认开启压缩，而无需逐个显式传实例。
	autoCompact bool

	// reductionConfig 可选的「工具输出缩减」配置（in-loop 轻量压缩）。
	// 非空时，每步编排前把超大/过旧的工具结果截断或替换为占位符，按 step 而非
	// 轮次裁剪——因此单轮委托（工作流子 Agent 读大文件→跑命令的循环）也能压住
	// context 爆炸。这是根治「context 爆炸 → 30min 硬上限」失控流的核心机制
	// （主 Agent 的 llmroute 已用 NewReducePrepareStepCallback 接同一套）。
	// 零值 ReductionConfig 表示关闭（MaxOutputTokens/ClearThresholdTokens 均为 0）。
	reductionConfig llm.ReductionConfig

	// reducer 由 reductionConfig 预构建的 PrepareStep 回调（无 ctx 依赖，可在 New 时构建）。
	// 对应 Reduction 阶段 2：每步 LLM 调用前压缩历史中过旧的大工具结果。
	reducer func(*llm.GenerateParams) *llm.GenerateParams

	// reducerOnToolResults 由 reductionConfig 预构建的 OnToolResults 回调（无 ctx 依赖）。
	// 对应 Reduction 阶段 1：工具执行后、结果写入历史前，把超阈值单条工具结果截断为预览+摘要。
	// 与 reducer（阶段 2）配套，共同压住单轮工具循环的 context 爆炸。
	reducerOnToolResults func(int, []llm.ToolResultPart) []llm.ToolResultPart
}

// WithCompactor 显式注入一个语义压缩器。为空时可通过 WithAutoCompact 让
// New 自动创建独立实例。强烈建议每个 subagent 各自持有一个（Compactor 内部
// 有跨调用状态，共享会互相污染）。
func WithCompactor(c *llm.Compactor) Option {
	return func(sa *SubAgent) { sa.compactor = c }
}

// isZeroReductionConfig 判断缩减配置是否为零值（MaxOutputTokens /
// ClearThresholdTokens / RetainRecentSteps 均为 0 且无 ExcludeTools）。
// ReductionConfig 含 []string 字段，不能用 == 比较，故用此辅助函数。
func isZeroReductionConfig(rc llm.ReductionConfig) bool {
	return rc.MaxOutputTokens == 0 && rc.ClearThresholdTokens == 0 &&
		rc.RetainRecentSteps == 0 && len(rc.ExcludeTools) == 0
}

// WithAutoCompact 让 New 在 compactor 为空时自动创建一个独立 Compactor，
// 并默认开启 in-loop 缩减（WithReduction(DefaultReductionConfig)）——二者针对
// 不同失控模式（语义摘要服务多轮、缩减压住单轮工具循环），一并开启才是完整的
// 自动上下文管理。用户显式 WithReduction(rc) 会覆盖此默认缩减配置。
func WithAutoCompact() Option {
	return func(sa *SubAgent) {
		sa.autoCompact = true
		// 仅在用户未显式 WithReduction 时播种默认缩减配置，避免覆盖用户选择。
		if isZeroReductionConfig(sa.reductionConfig) {
			sa.reductionConfig = llm.DefaultReductionConfig()
		}
	}
}

// WithReduction 开启「工具输出缩减」in-loop 压缩（按 step 裁剪超大/过旧工具结果），
// 是单轮委托 context 爆炸的核心防线。rc 为零值时等同于关闭。
func WithReduction(rc llm.ReductionConfig) Option {
	return func(sa *SubAgent) { sa.reductionConfig = rc }
}

// New 创建一个 SubAgent。
//
// provider 和 model 通常来自主 Agent 的 LLMBundle（如 bundle.Main, bundle.MainDef.Model）。
// 可通过 opts 自定义系统提示词、温度、滑动窗口等参数。
func New(provider llm.Provider, model string, opts ...Option) *SubAgent {
	sa := &SubAgent{
		provider:         provider,
		model:            model,
		temp:             0.7,  // 默认与 BotConfig 一致
		frequencyPenalty: 0.1,  // GLM-5.x 官方推荐重复抑制
		presencePenalty:  0.05, // GLM-5.x 官方推荐存在惩罚
		maxTokens:        4096,
		ctxMgr:           NewContextManager(20), // 默认保留最近 20 条消息（10 轮）
		chatTimeout:      defaultChatHardTimeout,
	}
	for _, opt := range opts {
		opt(sa)
	}
	// autoCompact 且无显式实例时，为每个 subagent 创建独立 Compactor，
	// 避免并发 subagent 共享内部状态互相污染。
	if sa.autoCompact && sa.compactor == nil {
		sa.compactor = llm.NewCompactor(llm.DefaultCompactionConfig())
	}
	// 持久 subagent 多轮对话超滑窗时，用 LLM 摘要替代纯删除（压缩优先），
	// 避免早期上下文永久丢失。仅当 compactor+provider 齐备时启用；
	// summarizeHead 内部独立摘要传入的 head，不污染主 Agent 的增量锚定摘要状态。
	if sa.compactor != nil && sa.provider != nil {
		compactor := sa.compactor
		provider := sa.provider
		model := sa.model
		sa.ctxMgr.summarizeHead = func(ctx context.Context, head []llm.Message) (llm.Message, bool) {
			if len(head) < compactor.Config().MinMessagesToCompact {
				return llm.Message{}, false
			}
			summary, err := compactor.SummarizeHead(ctx, provider, model, head)
			if err != nil || summary == "" {
				return llm.Message{}, false
			}
			return llm.SystemMessage(fmt.Sprintf("[Conversation Summary]\n%s\n[End of Summary]", summary)), true
		}
	}
	// 工具输出缩减：零值配置视为关闭（NewReducePrepareStepCallback /
	// NewOnToolResultsCallback 内部对零阈值不做任何裁剪）。
	// 阶段 2（PrepareStep，历史压缩）+ 阶段 1（OnToolResults，单条结果截断）
	// 配套使用，与主 Agent 的 llmroute 接同一套机制。
	if !isZeroReductionConfig(sa.reductionConfig) {
		sa.reducer = llm.NewReducePrepareStepCallback(sa.reductionConfig)
		sa.reducerOnToolResults = llm.NewOnToolResultsCallback(sa.reductionConfig)
	}
	return sa
}

// Chat 发送一条消息并返回回复文本，同时更新内部上下文。
// 返回的回复会自动追加到上下文中。
func (sa *SubAgent) Chat(ctx context.Context, text string) (string, error) {
	result, err := sa.ChatWithResult(ctx, text)
	if err != nil {
		return "", err
	}
	return result.Text, nil
}

// ChatWithResult 发送消息并返回完整的 GenerateResult。
// 对话历史会自动更新。
func (sa *SubAgent) ChatWithResult(ctx context.Context, text string) (*llm.GenerateResult, error) {
	sa.mu.Lock()
	if sa.closed {
		sa.mu.Unlock()
		return nil, fmt.Errorf("subagent %q: already closed", sa.name)
	}

	// 构建消息序列：历史 + 当前用户消息
	history := sa.ctxMgr.Messages()
	sa.mu.Unlock()

	msgs := make([]llm.Message, 0, len(history)+1)
	msgs = append(msgs, history...)
	msgs = append(msgs, llm.UserMessage(text))

	// 带可执行工具时走多步编排回路：自动执行工具并把结果喂回模型，
	// 直到模型停止请求工具。子 Agent 因此能像主 Agent 一样使用工作空间。
	if len(sa.extraTools) > 0 {
		params := sa.buildParams(msgs)
		steps := sa.toolSteps
		if steps <= 0 {
			steps = defaultSubagentToolSteps
		}
		cfg := &llm.OrchestrateConfig{
			Params:       params,
			MaxSteps:     steps,
			HardMaxSteps: 0, // 0 = 自动 = MaxSteps * 3（看门狗兜底）
			// 延迟加载：子 Agent 与主 Agent 共用同一批工具（含可能延迟的
			// MCP 工具），按需经 tool_search / 直接引用加载完整 schema。
			ToolDeferral: llm.NewToolDeferral(true),
		}
		// 自动上下文防御（同主 Agent 的 llmroute 机制）：
		//   - PrepareStep：reducer 阶段 2（历史压缩）+ compactor 语义摘要（多轮场景）。
		//   - OnToolResults：reducer 阶段 1（单条工具结果截断，单轮循环核心防线）。
		// 二者按 step 工作，即使单轮委托（工作流子 Agent 读大文件→跑命令）也能压住
		// context 爆炸，避免养到 30min 硬上限仍无法收敛、纯浪费 token。
		if prepareStep, onToolResults := sa.buildOrchestrateHooks(ctx); prepareStep != nil || onToolResults != nil {
			if prepareStep != nil {
				cfg.PrepareStep = prepareStep
			}
			if onToolResults != nil {
				cfg.OnToolResults = onToolResults
			}
		}
		result, err := llm.OrchestrateGenerate(ctx, sa.provider, cfg)
		if err != nil {
			return nil, errs.Wrapf(err, "subagent %q: orchestrated generate failed", sa.name)
		}
		// 持久化完整输出消息（含工具调用/结果），保证多轮上下文连贯。
		// 注意：result.Messages 仅含本轮的 assistant/tool 消息，不含本轮 user 消息，
		// 必须先把 user 消息写回上下文，否则下一轮会丢失 user 导致对话错乱。
		// 用 AppendWithCtx：溢出滑窗时以 LLM 摘要保留早期上下文（压缩优先），
		// 而非纯删除。
		sa.mu.Lock()
		if !sa.closed {
			sa.ctxMgr.AppendWithCtx(ctx, llm.UserMessage(text))
			for _, m := range result.Messages {
				sa.ctxMgr.AppendWithCtx(ctx, m)
			}
			sa.totalTurns++
		}
		sa.mu.Unlock()
		return result, nil
	}

	params := sa.buildParams(msgs)

	result, err := sa.provider.DoGenerate(llm.WithStatsFeature(ctx, "subagent"), params)
	if err != nil {
		return nil, errs.Wrapf(err, "subagent %q: LLM generate failed", sa.name)
	}

	// 更新上下文
	sa.mu.Lock()
	if !sa.closed {
		sa.ctxMgr.AppendTurnWithCtx(ctx, text, result.Text)
		sa.totalTurns++
	}
	sa.mu.Unlock()

	return result, nil
}

// Stream 发送消息并以流式方式返回结果。
// 对话历史在流完成（调用 StreamResult.Text() 或 ToResult()）后更新。
func (sa *SubAgent) Stream(ctx context.Context, text string) (*llm.StreamResult, error) {
	sa.mu.Lock()
	if sa.closed {
		sa.mu.Unlock()
		return nil, fmt.Errorf("subagent %q: already closed", sa.name)
	}
	history := sa.ctxMgr.Messages()
	sa.mu.Unlock()

	msgs := make([]llm.Message, 0, len(history)+1)
	msgs = append(msgs, history...)
	msgs = append(msgs, llm.UserMessage(text))

	// 带可执行工具时走多步编排流式回路（自动执行工具）。
	if len(sa.extraTools) > 0 {
		params := sa.buildParams(msgs)
		steps := sa.toolSteps
		if steps <= 0 {
			steps = defaultSubagentToolSteps
		}
		cfg := &llm.OrchestrateConfig{
			Params:       params,
			MaxSteps:     steps,
			HardMaxSteps: 0, // 0 = 自动 = MaxSteps * 3
			// 延迟加载：子 Agent 与主 Agent 共用同一批工具（含可能延迟的
			// MCP 工具），按需经 tool_search / 直接引用加载完整 schema。
			// 与 ChatWithResult 路径保持一致，避免流式下延迟工具暴露完整 schema。
			ToolDeferral: llm.NewToolDeferral(true),
		}
		// 自动上下文防御（同 ChatWithResult 路径）：PrepareStep + OnToolResults 双钩子。
		if prepareStep, onToolResults := sa.buildOrchestrateHooks(ctx); prepareStep != nil || onToolResults != nil {
			if prepareStep != nil {
				cfg.PrepareStep = prepareStep
			}
			if onToolResults != nil {
				cfg.OnToolResults = onToolResults
			}
		}
		result, err := llm.OrchestrateStream(ctx, sa.provider, cfg)
		if err != nil {
			return nil, errs.Wrapf(err, "subagent %q: orchestrated stream failed", sa.name)
		}
		// 流结束后把本轮完整消息持久化到上下文：先写回 user 消息，
		// 再写入编排结果（含工具调用/结果等中间消息），保证多轮上下文连贯。
		// OrchestrateStream 在关闭 channel 前已填充 result.Messages，此处可安全读取。
		originalCh := result.Stream
		wrappedCh := make(chan llm.StreamPart, 256)
		go func() {
			defer close(wrappedCh)
			for part := range originalCh {
				select {
				case wrappedCh <- part:
				case <-ctx.Done():
					return
				}
			}
			// 流结束后持久化：AppendWithCtx 在溢出滑窗时以 LLM 摘要保留早期上下文。
			sa.mu.Lock()
			if !sa.closed {
				sa.ctxMgr.AppendWithCtx(ctx, llm.UserMessage(text))
				for _, m := range result.Messages {
					sa.ctxMgr.AppendWithCtx(ctx, m)
				}
				sa.totalTurns++
			}
			sa.mu.Unlock()
		}()
		result.Stream = wrappedCh
		return result, nil
	}

	params := sa.buildParams(msgs)

	result, err := sa.provider.DoStream(llm.WithStatsFeature(ctx, "subagent"), params)
	if err != nil {
		return nil, errs.Wrapf(err, "subagent %q: LLM stream failed", sa.name)
	}

	// 包装原始 channel，在流结束时更新上下文
	originalCh := result.Stream
	wrappedCh := make(chan llm.StreamPart, 64)

	go func() {
		defer close(wrappedCh)
		var textBuf string
		for part := range originalCh {
			select {
			case wrappedCh <- part:
				if tp, ok := part.(*llm.TextDeltaPart); ok {
					textBuf += tp.Text
				}
			case <-ctx.Done():
				return // context 取消，退出避免 goroutine 泄漏
			}
		}
		// 流结束后更新上下文：AppendTurnWithCtx 在溢出滑窗时以 LLM 摘要保留早期上下文。
		if textBuf != "" {
			sa.mu.Lock()
			if !sa.closed {
				sa.ctxMgr.AppendTurnWithCtx(ctx, text, textBuf)
				sa.totalTurns++
			}
			sa.mu.Unlock()
		}
	}()

	result.Stream = wrappedCh
	return result, nil
}

// Clear 重置对话上下文（保留系统提示词和配置，只清除历史消息）。
func (sa *SubAgent) Clear() {
	sa.mu.Lock()
	defer sa.mu.Unlock()
	sa.ctxMgr.Clear()
	sa.totalTurns = 0
}

// History 返回当前上下文消息的副本。
func (sa *SubAgent) History() []llm.Message {
	sa.mu.Lock()
	defer sa.mu.Unlock()
	msgs := sa.ctxMgr.Messages()
	out := make([]llm.Message, len(msgs))
	copy(out, msgs)
	return out
}

// TurnCount 返回总对话轮数（不受滑动窗口影响）。
func (sa *SubAgent) TurnCount() int {
	sa.mu.Lock()
	defer sa.mu.Unlock()
	return sa.totalTurns
}

// ID 返回 SubAgent 的标识符。
func (sa *SubAgent) ID() string {
	return sa.id
}

// Name 返回 SubAgent 的名称。
func (sa *SubAgent) Name() string {
	return sa.name
}

// SetSystem 动态修改系统提示词（影响后续所有调用）。
func (sa *SubAgent) SetSystem(prompt string) {
	sa.mu.Lock()
	defer sa.mu.Unlock()
	sa.system = prompt
}

// SeedMessages 用给定消息预填充上下文（在首次 Chat 之前调用）。
// 适用于从外部导入已有对话的场景。
func (sa *SubAgent) SeedMessages(msgs []llm.Message) {
	sa.mu.Lock()
	defer sa.mu.Unlock()
	for _, m := range msgs {
		sa.ctxMgr.Append(m)
	}
}

// Close 关闭 SubAgent，释放上下文。
// Close 后调用 Chat 会返回错误。
// 可以安全地多次调用。
func (sa *SubAgent) Close() {
	sa.mu.Lock()
	defer sa.mu.Unlock()
	sa.closed = true
	sa.ctxMgr = nil
}

// buildOrchestrateHooks 构建工具编排回路（OrchestrateGenerate/Stream）的上下文压缩钩子：
//
//   - PrepareStep：串联 reducer（阶段 2 历史压缩）与 compactor（LLM 语义摘要，多轮场景）。
//     二者都满足「func(*GenerateParams) *GenerateParams」签名，故合并为单步钩子：
//     先跑 reducer（原地压缩历史），再交给 compactor 判断是否需 LLM 摘要——无需摘要则返回
//     已缩减的 p，需摘要则返回 compactor 新生成的 params。
//   - OnToolResults：串联 reducer 阶段 1（工具执行后、结果入史前，把超阈值单条工具结果截断
//     为预览+摘要），这是单轮委托 context 爆炸的核心防线（语义压缩在单轮下几乎触发不到）。
//
// 任一钩子为 nil（未启用对应能力）时即不挂载，保证零开销。
func (sa *SubAgent) buildOrchestrateHooks(ctx context.Context) (
	prepareStep func(*llm.GenerateParams) *llm.GenerateParams,
	onToolResults func(int, []llm.ToolResultPart) []llm.ToolResultPart,
) {
	if sa.reducer != nil {
		prepareStep = sa.reducer
	}
	if sa.reducerOnToolResults != nil {
		onToolResults = sa.reducerOnToolResults
	}
	if sa.compactor != nil {
		compactHook := llm.CompactionPrepareStepWithProvider(sa.compactor, sa.provider)(ctx)
		basePrepare := prepareStep
		prepareStep = func(p *llm.GenerateParams) *llm.GenerateParams {
			if basePrepare != nil {
				basePrepare(p)
			}
			if cp := compactHook(p); cp != nil {
				return cp
			}
			return p
		}
	}
	return prepareStep, onToolResults
}

// buildParams 根据当前配置和消息构建 GenerateParams。
func (sa *SubAgent) buildParams(msgs []llm.Message) llm.GenerateParams {
	sa.mu.Lock()
	defer sa.mu.Unlock()

	temp := sa.temp
	maxTokens := sa.maxTokens
	freqPen := sa.frequencyPenalty
	presPen := sa.presencePenalty

	params := llm.GenerateParams{
		Model:            llm.ChatModel(sa.model),
		System:           sa.system,
		Messages:         msgs,
		Temperature:      &temp,
		FrequencyPenalty: &freqPen,
		PresencePenalty:  &presPen,
		MaxTokens:        &maxTokens,
	}

	if len(sa.extraTools) > 0 {
		params.Tools = sa.extraTools
	}
	if sa.responseFormat != nil {
		params.ResponseFormat = sa.responseFormat
	}

	return params
}
