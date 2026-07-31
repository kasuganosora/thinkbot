package workflow

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	noop_trace "go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/subagent"
	"github.com/kasuganosora/thinkbot/util/errs"
	"github.com/kasuganosora/thinkbot/util/idgen"
	"github.com/kasuganosora/thinkbot/util/strutil"
	"github.com/kasuganosora/thinkbot/util/traceid"
)

// ============================================================================
// Analyzer — 需求分析器
//
// 使用 LLM 将用户需求分解为 DAG 节点图。
// LLM 以 JSON 模式输出节点列表，Analyzer 解析后构建 Workflow 领域对象。
// ============================================================================

// Analyzer 分析用户需求并生成 DAG。
type Analyzer struct {
	saMgr  *subagent.SubAgentManager
	tracer trace.Tracer
	ec     EngineConfig
	logger *zap.SugaredLogger
}

// NewAnalyzer 创建分析器。
func NewAnalyzer(saMgr *subagent.SubAgentManager, tp trace.TracerProvider, ec EngineConfig, logger *zap.SugaredLogger) *Analyzer {
	if logger == nil {
		logger = zap.NewNop().Sugar()
	}
	if tp == nil {
		tp = noop_trace.NewTracerProvider()
	}
	return &Analyzer{
		saMgr:  saMgr,
		tracer: tp.Tracer("github.com/kasuganosora/thinkbot/workflow/analyzer"),
		ec:     ec,
		logger: logger.With("component", "workflow_analyzer"),
	}
}

// maxNodeRetries / maxNodeIterations 限制 LLM 输出的重试/迭代上限，
// 防止恶意或异常输入导致节点无限重试。
const (
	maxNodeRetries    = 10
	maxNodeIterations = 10
)

// analyzerSystemPrompt 是分析器 SubAgent 的系统提示词。
const analyzerSystemPrompt = `你是一个任务分解专家。你的职责是将复杂需求分解为可执行的子任务 DAG 图。

## 分解原则
1. 将需求拆解为独立的、可执行的子任务
2. 识别任务间的依赖关系：哪些需要串行（前序完成才能执行），哪些可以并行
3. 标记关键任务（review=true）：结果质量直接影响后续流程的任务需要审查
4. 为每个任务设计合适的 SubAgent 角色（systemPrompt）

## 重要约束
- 每个子任务的 SubAgent 是隔离执行环境，不具备工具调用能力
- 子任务中不能依赖 workflow 工具或其他外部工具
- 任务描述应自包含，不需要额外资源就能由 SubAgent 独立完成

## 依赖关系规则
- dependencies 列出该任务依赖的前置节点 ID（AND 依赖：全部完成后才能执行）
- 无依赖的节点（空数组）将并行执行
- 例如：A→B, A→C, B→D, C→D 的依赖关系：
  A: [], B: ["A"], C: ["A"], D: ["B","C"]
  执行顺序：A → (B∥C) → D

## 节点字段说明
- id: 唯一标识，如 "n1", "n2"...
- name: 简短任务名称
- task: 详细任务描述（SubAgent 要执行的具体内容）
- systemPrompt: SubAgent 的角色定义（可为空）
- dependencies: 依赖的节点 ID 数组（空数组表示无依赖）
- review: 是否需要结果审查（关键/高风险任务设为 true）
- reviewPrompt: 审查 prompt（可选，为空则使用默认审查规则）
- maxRetries: 执行失败最大重试次数（默认 2）
- maxIterations: Review 迭代上限（默认 3，仅 review=true 时生效）
- feedback: 【可选】"目标模式"专用的回退边数组——当本节点 review 不通过时，
  回退到这些上游节点重新执行（形成"工作→审查→修复→审查"的闭环）。
  仅当整个任务开启了目标模式时才生效；若你不填，系统会自动把最终节点的
  feedback 接线到它的直接上游工作节点。

## 输出格式
必须返回 JSON，结构如下：
{
  "nodes": [
    {
      "id": "n1",
      "name": "任务名称",
      "task": "详细任务描述...",
      "systemPrompt": "你是一个...",
      "dependencies": [],
      "review": false,
      "maxRetries": 2,
      "maxIterations": 3,
      "feedback": []
    }
  ]
}

## 严格输出纪律（务必遵守，否则会导致解析失败并重试）
- **只输出 JSON**，不要输出任何解释、前言或总结文字。
- **不要用 markdown 代码块（即三个反引号包裹）去包裹输出**，也不要在 JSON 之外附加任何说明。
- 直接以左花括号开始、以右花括号结束，保证输出是一段可被 JSON 解析器直接解析的纯文本。`

// goalModeAnalyzerHint 在目标模式下追加到分析任务末尾，告知模型本次需要
// 一个可闭环的验收节点。system prompt 是静态的，模型无法从中判断当前请求
// 是否开启了目标模式，必须在任务侧显式说明。
const goalModeAnalyzerHint = `## 本次为「目标模式」

本任务开启了目标模式：最终产物必须迭代到达标才算完成。请遵守：

1. 在 DAG 末尾安排一个**独立的验收节点**（如"运行测试确认全部通过""通读全文核对是否满足全部要求"），不要把验收混在产出节点里。
2. 该验收节点必须 "review": true，其 reviewPrompt 写明**具体可判定的合格标准**（通过/不通过，而非"质量良好"）。
3. 该验收节点的 "feedback" 填审查不通过时应回退重做的工作节点 ID（回退边不参与依赖、不会构成环，可放心填多个）。
4. **多模块 / 多阶段场景**（如"逐个审查 N 个模块，每个审查到没有新问题才进行下一个，最后整体审查"）：
   - 把每个模块/阶段拆成**独立节点**，并**按顺序串联依赖**（m1 → m2 → ... → 整体审查），前一个收敛后下一个才会开始；
   - 每个模块节点设 "review": true 并给出具体 reviewPrompt（例如"逐一核查本模块，仅当确认无遗留问题时 pass"）；目标模式会自动让每个节点反复审查自身直到通过，模块间互不干扰；
   - 这类中间节点的 "feedback" **无需手动填写**（会自动自环），只需保证终点验收节点的 feedback 正确即可。`

// dagSpec 是分析器输出的 DAG 规范（从 LLM JSON 解析）。
type 	dagSpec struct {
	Nodes []struct {
		ID            string   `json:"id"`
		Name          string   `json:"name"`
		Task          string   `json:"task"`
		SystemPrompt  string   `json:"systemPrompt"`
		Dependencies  []string `json:"dependencies"`
		Review        bool     `json:"review"`
		ReviewPrompt  string   `json:"reviewPrompt"`
		MaxRetries    int      `json:"maxRetries"`
		MaxIterations int      `json:"maxIterations"`
		Feedback      []string `json:"feedback"`
	} `json:"nodes"`
}

// Analyze 分析需求并生成 DAG 节点列表。
//
// goalMode 为 true 时进入「目标模式」：分析完成后会自动把最终（无下游）节点的
// review 打开并将其 feedback 回退边接线到直接上游工作节点，从而实现
// 「工作→审查→修复→审查」的全局闭环（详见 wireGoalMode）。
//
// 注意：不强制使用 LLM 的 JSON 响应模式。经验表明 GLM 在
// response_format=json_object 且输入需求较长时，经常返回空 content（HTTP 200 但
// body 为空），导致解析直接失败、整个工作流在分析（准备）阶段就 failed 且无法恢复。
// 改为仅靠 system prompt 约束输出 JSON，并依赖 parseDAGSpec/ExtractJSON 的 markdown
// 容错提取。同时加入重试，偶发的空响应或截断可被自动恢复。
func (a *Analyzer) Analyze(ctx context.Context, requirement string, goalMode bool, onProgress ...func(attempt int, phase string)) ([]*DAGNode, error) {
	ctx, span := a.tracer.Start(ctx, "workflow.analyzer.analyze")
	defer span.End()

	logger := traceid.WithLoggerFrom(ctx, a.logger)
	logger.Infow("analyzing requirement", "requirement_len", len(requirement))

	// onProgress 在每次尝试前后上报进度（用于前端展示分析阶段进展，避免「分析中…」长期无变化）。
	fireProgress := func(attempt int, phase string) {
		if len(onProgress) > 0 && onProgress[0] != nil {
			onProgress[0](attempt, phase)
		}
	}

	// 构建分析任务。
	//
	// 目标模式下必须显式告知分析器：system prompt 里 feedback 字段的说明是
	// 「仅当整个任务开启了目标模式时才生效」，但 system prompt 是静态的，
	// 模型无从判断本次请求是否开启。不告知的话它只会留空 feedback、退化为
	// 依赖 wireGoalMode 兜底接线（能跑，但拿不到一个专门的验收节点）。
	task := fmt.Sprintf("请将以下需求分解为 DAG 子任务图：\n\n%s", requirement)
	if goalMode {
		task += "\n\n" + goalModeAnalyzerHint
	}

	// GLM 的空响应退化通常不是单次抖动，而是持续数十秒到数分钟的窗口：
	// 一旦进入该窗口，紧挨着的连续重试会全部撞在同一窗口里失败（实测 90s 内 3 连败）。
	// 因此重试之间采用指数退避，把多次尝试拉开到更宽的时间跨度上，
	// 显著提高"跨过退化窗口后自动恢复"的概率；退避可被 ctx 取消打断。
	const maxAttempts = 5
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		// 分析阶段被显式终止（如外部调用 Control(terminate) 取消了 bgCtx）：
		// 立刻返回，不重试、不污染错误信息。上层会据此标记为 terminated 而非 failed。
		if cerr := ctx.Err(); cerr != nil {
			return nil, errs.Wrap(cerr, "analysis canceled")
		}

		// 首次尝试不等待；后续尝试指数退避（2s, 4s, 8s, 16s，上限 30s）。
		if attempt > 1 {
			backoff := time.Duration(1<<(attempt-1)) * time.Second
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
			logger.Infow("backing off before analyzer retry",
				"attempt", attempt, "max_attempts", maxAttempts, "backoff", backoff.String())
			select {
			case <-ctx.Done():
				if lastErr == nil {
					lastErr = ctx.Err()
				}
				return nil, lastErr
			case <-time.After(backoff):
			}
		}

		// 重试时提高温度，打破 GLM 在长输入下"稳定返回空"的退化行为
		temp := a.ec.AnalyzerTemperature
		if attempt > 1 {
			temp = 0.7
		}

		fireProgress(attempt, fmt.Sprintf("正在调用模型分析需求（第 %d/%d 次尝试）", attempt, maxAttempts))

		raw, err := a.saMgr.DelegateStream(ctx, analyzerSystemPrompt, task,
			subagent.WithTemperature(temp),
			subagent.WithMaxTokens(a.ec.AnalyzerMaxTokens),
			subagent.WithStuckTimeout(a.ec.AnalyzerStuckTimeout),
		)
		if err != nil {
			// 上下文被取消（分析被终止）：不再重试，直接返回清晰错误。
			if errors.Is(err, context.Canceled) {
				return nil, errs.Wrap(err, "analysis canceled")
			}
			lastErr = errs.Wrap(err, "analyzer LLM call failed")
			logger.Warnw("analyzer LLM call failed, will retry",
				"attempt", attempt, "max_attempts", maxAttempts, "error", err)
			continue
		}

		raw = strings.TrimSpace(raw)
		if raw == "" {
			lastErr = errs.New("analyzer returned empty response")
			logger.Warnw("analyzer returned empty response, will retry",
				"attempt", attempt, "max_attempts", maxAttempts)
			continue
		}

		// 解析 JSON（支持 markdown 包裹与混合文本容错提取）
		spec, perr := parseDAGSpec(raw)
		if perr != nil {
			lastErr = errs.Wrapf(perr, "failed to parse analyzer output")
			logger.Warnw("analyzer parse failed, will retry",
				"attempt", attempt, "max_attempts", maxAttempts, "error", perr)
			continue
		}

		// 转换为领域对象
		nodes := make([]*DAGNode, 0, len(spec.Nodes))
		for _, sn := range spec.Nodes {
			maxRetries := sn.MaxRetries
			if maxRetries <= 0 {
				maxRetries = 2
			} else if maxRetries > maxNodeRetries {
				maxRetries = maxNodeRetries
			}
			maxIter := sn.MaxIterations
			if maxIter <= 0 {
				maxIter = 3
			} else if maxIter > maxNodeIterations {
				maxIter = maxNodeIterations
			}
			nodes = append(nodes, &DAGNode{
				ID:            sn.ID,
				Name:          sn.Name,
				Task:          sn.Task,
				SystemPrompt:  sn.SystemPrompt,
				Dependencies:  sn.Dependencies,
				Review:        sn.Review,
				ReviewPrompt:  sn.ReviewPrompt,
				MaxRetries:    maxRetries,
				MaxIterations: maxIter,
				Feedback:      sn.Feedback,
			})
		}

		// 目标模式：自动接线最终节点的 review + feedback 回退边（在校验前完成）。
		if goalMode {
			wireGoalMode(nodes)
		}

		// 校验 DAG（feedback 边不计入 Dependencies，故不影响无环性校验）
		if err := ValidateDAG(nodes); err != nil {
			lastErr = errs.Wrap(err, "generated DAG is invalid")
			logger.Warnw("generated DAG invalid, will retry",
				"attempt", attempt, "max_attempts", maxAttempts, "error", err)
			continue
		}

		span.SetAttributes(attribute.Int("analyzer.node_count", len(nodes)))
		logger.Infow("requirement analyzed", "nodes", len(nodes), "attempt", attempt)
		for _, n := range nodes {
			logger.Debugw("node", "id", n.ID, "name", n.Name,
				"deps", n.Dependencies, "review", n.Review)
		}
		return nodes, nil
	}

	span.RecordError(lastErr)
	return nil, lastErr
}

// parseDAGSpec 解析 LLM 返回的 JSON 为 dagSpec。
// 支持容错：提取 JSON 块、清理 markdown 包裹。
func parseDAGSpec(raw string) (*dagSpec, error) {
	raw = strings.TrimSpace(raw)

	// 清理 markdown 代码块包裹
	if strings.HasPrefix(raw, "```") {
		lines := strings.Split(raw, "\n")
		var sb strings.Builder
		inBlock := false
		for _, line := range lines {
			if strings.HasPrefix(line, "```") {
				inBlock = !inBlock
				continue
			}
			if inBlock {
				sb.WriteString(line)
				sb.WriteString("\n")
			}
		}
		raw = sb.String()
		if raw == "" {
			// 没有代码块结束符，直接去除开头的 ```json
			raw = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(raw, "```json"), "```"))
		}
	}

	var spec dagSpec
	if err := strutil.ExtractJSON(raw, &spec); err != nil {
		return nil, errs.Wrapf(err, "invalid JSON: %s", strutil.Truncate(raw, 200))
	}

	if len(spec.Nodes) == 0 {
		return nil, errs.New("analyzer returned 0 nodes")
	}

	return &spec, nil
}

// GenerateWorkflowID 生成工作流 ID。
func GenerateWorkflowID() string {
	return idgen.New("wf")
}

// maxFeedbackEdges 单个目标模式节点允许的最大回退边数（防御性上限）。
const maxFeedbackEdges = 8

// wireGoalMode 在目标模式下自动接线「目标模式闭环」：
//   - 强制所有节点 review=true：目标模式下每个模块/阶段都应迭代到审查通过才算完成，
//     而非一次产出即终态。这同时支持「单任务反复打磨」与「多模块逐模块门禁」——
//     模块间由依赖链保证顺序（前一个收敛后下一个才开始），每个模块自带收敛循环。
//   - 回退边（feedback）兜底：LLM 已显式指定的保留；否则按节点角色自动接线：
//     · 非终点节点（有下游依赖它）→ 自环，闭环时仅重跑自身，不波及下游，
//       从而支持「逐模块收敛后才进入下一步」的串联门禁；
//     · 终点节点（无人依赖）→ 回退到直接上游工作节点（Dependencies），
//       形成「工作→审查→修复→审查」的全局闭环；
//     · 单节点工作流（终点即起点、无依赖）→ 回退到自身。
//
// 注意：feedback 边不写入 Dependencies，因此不影响拓扑排序与无环性校验。
func wireGoalMode(nodes []*DAGNode) {
	if len(nodes) == 0 {
		return
	}

	// 找出终点节点：没有任何其他节点以它为依赖。
	isDependency := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		for _, dep := range n.Dependencies {
			isDependency[dep] = true
		}
	}

	for _, n := range nodes {
		// 目标模式下所有节点进入「收敛循环」：强制开启 review。
		if !n.Review {
			n.Review = true
		}

		// 回退边：LLM 已指定则保留；否则按节点角色兜底。
		if len(n.Feedback) > 0 {
			continue
		}
		if isDependency[n.ID] {
			// 非终点（有下游依赖它）：自环。闭环时只重跑自身，不波及下游，
			// 支持「逐模块收敛后才进入下一步」的串联门禁。
			n.Feedback = []string{n.ID}
			continue
		}
		// 终点节点。
		if len(n.Dependencies) == 0 {
			// 单节点工作流：回退到自身（带审查意见重跑）。
			n.Feedback = []string{n.ID}
			continue
		}
		// 复制直接上游（去重 + 上限防御），形成「工作→审查→修复→审查」闭环。
		seen := make(map[string]bool, len(n.Dependencies))
		fb := make([]string, 0, len(n.Dependencies))
		for _, dep := range n.Dependencies {
			if seen[dep] {
				continue
			}
			seen[dep] = true
			fb = append(fb, dep)
			if len(fb) >= maxFeedbackEdges {
				break
			}
		}
		n.Feedback = fb
	}
}
