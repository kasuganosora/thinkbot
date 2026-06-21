package workflow

import (
	"context"
	"fmt"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	noop_trace "go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/llm"
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
      "maxIterations": 3
    }
  ]
}`

// dagSpec 是分析器输出的 DAG 规范（从 LLM JSON 解析）。
type dagSpec struct {
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
	} `json:"nodes"`
}

// Analyze 分析需求并生成 DAG 节点列表。
func (a *Analyzer) Analyze(ctx context.Context, requirement string) ([]*DAGNode, error) {
	ctx, span := a.tracer.Start(ctx, "workflow.analyzer.analyze")
	defer span.End()

	logger := traceid.WithLoggerFrom(ctx, a.logger)
	logger.Infow("analyzing requirement", "requirement_len", len(requirement))

	// 构建分析任务
	task := fmt.Sprintf("请将以下需求分解为 DAG 子任务图：\n\n%s", requirement)

	// 调用 LLM（JSON 模式）
	raw, err := a.saMgr.Delegate(ctx, analyzerSystemPrompt, task,
		subagent.WithResponseFormat(&llm.ResponseFormat{Type: llm.ResponseFormatJSONObject}),
		subagent.WithTemperature(a.ec.AnalyzerTemperature),
		subagent.WithMaxTokens(a.ec.AnalyzerMaxTokens),
	)
	if err != nil {
		span.RecordError(err)
		return nil, errs.Wrap(err, "analyzer LLM call failed")
	}

	// 解析 JSON
	spec, err := parseDAGSpec(raw)
	if err != nil {
		span.RecordError(err)
		return nil, errs.Wrapf(err, "failed to parse analyzer output")
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
		})
	}

	// 校验 DAG
	if err := ValidateDAG(nodes); err != nil {
		span.RecordError(err)
		return nil, errs.Wrap(err, "generated DAG is invalid")
	}

	span.SetAttributes(attribute.Int("analyzer.node_count", len(nodes)))
	logger.Infow("requirement analyzed", "nodes", len(nodes))
	for _, n := range nodes {
		logger.Debugw("node", "id", n.ID, "name", n.Name,
			"deps", n.Dependencies, "review", n.Review)
	}

	return nodes, nil
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
