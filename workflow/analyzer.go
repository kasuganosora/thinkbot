package workflow

import (
	"context"
	"encoding/json"
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
const analyzerSystemPrompt = `You are a task decomposition specialist, a planning agent that turns a complex requirement into an executable DAG of sub-tasks.

## Decomposition Principles

1. Split the requirement into independent, executable sub-tasks.
2. **Default to running sub-tasks in parallel**: add a dependency ONLY when the later task must read the earlier task's output (see "Maximize Parallelism").
3. Mark critical tasks with review=true: any task whose output quality directly affects downstream work must be reviewed.
4. Design a fitting SubAgent role (systemPrompt) for every task.

## Hard Constraints

- Every sub-task runs in an isolated SubAgent that has NO tool-calling ability.
- Sub-tasks MUST NOT depend on workflow tools or any other external tool.
- Task descriptions MUST be self-contained: a SubAgent must be able to complete them with no extra resources.

## Dependency Rules

- "dependencies" lists the prerequisite node IDs (AND semantics: all of them must finish before the node runs).
- Nodes with no dependencies (empty array) run in parallel.
- Example — the dependency graph A→B, A→C, B→D, C→D:
  A: [], B: ["A"], C: ["A"], D: ["B","C"]
  Execution order: A → (B∥C) → D

## Maximize Parallelism (IMPORTANT — this directly determines total runtime)

**Default to no dependency. Fill in "dependencies" ONLY when a real data dependency exists.**

Decision rule: **ask yourself "does B truly need to read A's output before it can start?"**
- Yes (B consumes files / conclusions / interfaces produced by A) → B depends on A.
- No (you are merely used to doing them in order, or they share one umbrella goal) → **do NOT add a dependency**.

The most common mistake: chaining **mutually independent work of the same kind** into a single chain. Such work MUST all run in parallel.

<example>
Correct — reviewing several modules; they are independent, so run them all in parallel and accept once at the end:
n1 review composables/         deps: []
n2 review shared/              deps: []
n3 review api/                 deps: []
n4 review stores/              deps: []
n5 full verification + overall re-review   deps: ["n1","n2","n3","n4"]
Execution: (n1∥n2∥n3∥n4) → n5
</example>

<example>
NEVER do this — chaining independent modules multiplies total runtime for no benefit:
n1 review composables/  deps: []
n2 review shared/       deps: ["n1"]   ← wrong: n2 does not need n1's output
n3 review api/          deps: ["n2"]   ← wrong: same reason
n4 review stores/       deps: ["n3"]   ← wrong: same reason
</example>

Cases that genuinely require sequencing (only these get dependencies):
- "Scaffold the project first" → "then implement features on top of the scaffold" (the latter consumes files produced by the former).
- "Design the database schema first" → "then write the query layer against that schema".
- "Final verification / aggregation / submission" → depends on every work node it verifies (like n5 above).

## Node Fields

- id: unique identifier, e.g. "n1", "n2", ...
- name: short task name
- task: detailed task description (exactly what the SubAgent must do)
- systemPrompt: role definition for the SubAgent (may be empty)
- dependencies: array of prerequisite node IDs (empty array means no dependency)
- review: whether the result needs review (set true for critical / high-risk tasks)
- reviewPrompt: review prompt (optional; empty means the default review rules are used)
- maxRetries: max retries when execution fails (default 2)
- maxIterations: max review iterations (default 3; only applies when review=true)
- feedback: [optional] array of fallback edges used ONLY by "goal mode" — when this node
  fails review, execution rolls back to these upstream nodes and reruns them, forming a
  "work → review → fix → review" loop. It only takes effect when goal mode is enabled for
  the whole task. If you leave it empty, the system automatically wires the final node's
  feedback to its direct upstream work nodes.

## Output Format

You MUST return JSON with exactly this structure:
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

IMPORTANT: this bot serves Chinese end users. Write the "name", "task", "systemPrompt" and
"reviewPrompt" values in Chinese (中文). Preserve all JSON keys and node IDs exactly as
specified above.

## Strict Output Discipline (violations cause a parse failure and a retry)

- Output **JSON only**. NEVER emit any explanation, preamble, or closing remarks.
- NEVER wrap the output in a markdown code fence (three backticks), and NEVER append any note outside the JSON.
- Start with the opening brace and end with the closing brace, so the output is plain text a JSON parser can consume directly.`

// goalModeAnalyzerHint 在目标模式下追加到分析任务末尾，告知模型本次需要
// 一个可闭环的验收节点。system prompt 是静态的，模型无法从中判断当前请求
// 是否开启了目标模式，必须在任务侧显式说明。
const goalModeAnalyzerHint = `## GOAL MODE IS ENABLED FOR THIS REQUEST

Goal mode is on: the final deliverable is not complete until it has iterated to an acceptable
standard. You MUST follow these rules:

1. Put a **dedicated acceptance node** at the end of the DAG (e.g. "run the tests and confirm they all pass", "read the whole document and verify every requirement is met"). NEVER fold acceptance into a production node.
2. That acceptance node MUST set "review": true, and its reviewPrompt MUST state **concrete, decidable pass criteria** (pass / fail — not "good quality").
3. That acceptance node's "feedback" MUST list the IDs of the work nodes to roll back and redo when review fails (fallback edges are not dependencies and can never form a cycle, so listing several is safe).
4. **Multi-module / multi-stage requests** (e.g. "review N modules one by one, only move on when a module has no new issues, then review everything"):
   - Split each module/stage into a **separate node** and **chain them with dependencies in order** (m1 → m2 → ... → overall review), so the next one starts only after the previous one converges;
   - Set "review": true on every module node with a concrete reviewPrompt (e.g. "check this module item by item; pass only when no issue remains"). Goal mode makes each node review itself repeatedly until it passes, and modules do not interfere with each other;
   - You do NOT need to fill "feedback" on these intermediate nodes (a self-loop is wired automatically) — you only need the final acceptance node's feedback to be correct.`

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
	task := fmt.Sprintf("Decompose the following requirement into a DAG of sub-tasks:\n\n%s", requirement)
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
			subagent.WithSkipTools(), // Analyzer 是纯 LLM 任务（输出 JSON DAG），不需要工具；
			// 避免被注入工具后误走 OrchestrateStream 多步编排循环导致卡死。
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
			// 输出被 max_tokens 硬截断时，报错只是笼统的 "unexpected end of JSON input"，
			// 极易被误判成「模型不听话」。这里显式点名预算，让日志直接指向可调参数。
			if looksTruncated(perr) {
				logger.Warnw("analyzer output looks TRUNCATED by the output budget, will retry",
					"attempt", attempt, "max_attempts", maxAttempts,
					"raw_len", len(raw),
					"max_tokens", a.ec.AnalyzerMaxTokens,
					"hint", "budget follows the bot's selected model maxTokens; it is shared by reasoning + body on thinking models. Raise maxTokens on that model in provider settings",
					"error", perr)
			} else {
				logger.Warnw("analyzer parse failed, will retry",
					"attempt", attempt, "max_attempts", maxAttempts,
					"raw_len", len(raw), "error", perr)
			}
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
// 支持容错：提取 JSON 块、清理 markdown 包裹、截断恢复（从被截断的
// 数组中提取所有完整节点对象，丢弃不完整的尾部）。
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
	if err := strutil.ExtractJSON(raw, &spec); err == nil {
		if len(spec.Nodes) == 0 {
			return nil, errs.New("analyzer returned 0 nodes")
		}
		return &spec, nil
	}

	// ---- 截断恢复：LLM 输出因 max_tokens 限制被截断时，JSON 数组可能不完整。
	// 策略：扫描原始输出，提取 nodes 数组中每个完整的 { } 对象（括号配平），
	// 丢弃末尾截断的不完整对象，用完整对象重新组装成合法 JSON 再解析。
	// 这能把「全量解析失败」降级为「少取最后一个节点」，显著提高成功率。
	if recovered, ok := recoverTruncatedDAGNodes(raw); ok && len(recovered.Nodes) > 0 {
		return recovered, nil
	}

	return nil, errs.Wrapf(strutil.ExtractJSON(raw, &spec), "invalid JSON: %s", strutil.Truncate(raw, 200))
}

// looksTruncated 判断解析错误是否源于「输出被硬截断」而非「模型格式不对」。
//
// 被 max_tokens 截断时 encoding/json 只会报 "unexpected end of JSON input"
// 一类的输入提前结束错误，和「模型返回了说明文字」在日志里长得几乎一样。
// 区分开来，才能把排查方向直接指向输出预算而不是提示词。
func looksTruncated(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unexpected end of json") ||
		strings.Contains(msg, "unexpected eof")
}

// recoverTruncatedDAGNodes 从被截断的 LLM 输出中恢复完整的 DAG 节点。
//
// 当模型输出因 max_tokens 限制被截断时，JSON 数组的末尾通常不完整（缺少闭合的
// } 或 ]）。本函数通过括号配平扫描提取 nodes 数组中每个结构完整的对象，
// 丢弃尾部截断的部分，用恢复出的完整节点重新组装 dagSpec。
//
// 返回 (recoveredSpec, true) 表示成功恢复了至少一个节点；
// 返回 (nil, false) 表示无法恢复（输出中不存在任何完整节点对象）。
func recoverTruncatedDAGNodes(raw string) (*dagSpec, bool) {
	// 先尝试用 ExtractJSON 提取最外层 { } 对象（可能 "nodes" 值被截断但外层完整）
	var outer map[string]any
	if err := strutil.ExtractJSON(raw, &outer); err == nil {
		if nodesRaw, ok := outer["nodes"]; ok {
			// nodes 可能是完整数组或截断数组
			if arr, ok := nodesRaw.([]any); ok {
				if recovered := extractCompleteObjects(arr); len(recovered) > 0 {
					if nodes := mapsToDagNodes(recovered); len(nodes) > 0 {
						return &dagSpec{Nodes: nodes}, true
					}
				}
			}
		}
	}

	// 外层对象也解析失败：直接在原始文本中扫描 [ ... ] 内的完整对象
	// 找到 "nodes": [ 之后的内容，逐个提取完整 { ... } 块
	arrStart := strings.Index(raw, `"nodes"`)
	if arrStart < 0 {
		return nil, false
	}
	bracketStart := strings.Index(raw[arrStart:], "[")
	if bracketStart < 0 {
		return nil, false
	}
	arrContent := raw[arrStart+bracketStart+1:]

	var completeObjs []map[string]any
	pos := 0
	for pos < len(arrContent) {
		// 跳过空白和逗号
		for pos < len(arrContent) && (arrContent[pos] == ' ' || arrContent[pos] == '\n' || arrContent[pos] == '\r' || arrContent[pos] == '\t' || arrContent[pos] == ',') {
			pos++
		}
		if pos >= len(arrContent) || arrContent[pos] != '{' {
			break // 遇到了非 { 字符（可能是 ] 或 EOF），说明后续无更多完整对象
		}

		objStr, balanced := extractBalancedGo(arrContent[pos:], '{', '}')
		if !balanced {
			break // 当前对象不完整，后续也不会有完整的了
		}

		var obj map[string]any
		if err := json.Unmarshal([]byte(objStr), &obj); err != nil {
			pos += len(objStr) + 1 // 跳过这个无法解析的对象
			continue
		}
		completeObjs = append(completeObjs, obj)
		pos += len(objStr)
	}

	if len(completeObjs) == 0 {
		return nil, false
	}
	if nodes := mapsToDagNodes(completeObjs); len(nodes) > 0 {
		return &dagSpec{Nodes: nodes}, true
	}
	return nil, false
}

// extractBalancedGo 是 Go 原生实现的括号配平扫描（与 strutil.extractBalanced 逻辑一致，
// 但返回 (string, bool) 以便在 analyzer 包内使用，避免循环依赖）。
func extractBalancedGo(s string, open, close byte) (string, bool) {
	if len(s) == 0 || s[0] != open {
		return "", false
	}
	depth := 0
	inStr := false
	esc := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			if esc {
				esc = false
			} else if c == '\\' {
				esc = true
			} else if c == '"' {
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case open:
			depth++
		case close:
			depth--
			if depth == 0 {
				return s[:i+1], true
			}
		}
	}
	return "", false
}

// extractCompleteObjects 从 []any 数组中提取所有可序列化为完整 JSON 的元素。
// 对于截断场景，末尾元素通常是 map[string]any 且包含不完整的字符串值，
// json.Marshal 会失败；这些元素将被丢弃。
func extractCompleteObjects(arr []any) []map[string]any {
	var result []map[string]any
	for _, item := range arr {
		obj, ok := item.(map[string]any)
		if !ok {
			continue
		}
		// 验证该对象可以合法序列化（字段值都是完整类型）
		if _, err := json.Marshal(obj); err != nil {
			continue // 截断导致的半成品对象，丢弃
		}
		result = append(result, obj)
	}
	return result
}

func coalesceString(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func coalesceStringSlice(m map[string]any, key string) []string {
	if v, ok := m[key].([]any); ok {
		s := make([]string, 0, len(v))
		for _, item := range v {
			if str, ok := item.(string); ok {
				s = append(s, str)
			}
		}
		return s
	}
	if v, ok := m[key].(string); ok && v != "" {
		return []string{v}
	}
	return nil
}

// mapsToDagNodes 将 []map[string]any 转换为 dagSpec.Nodes 的匿名结构体切片。
// 用于截断恢复场景：从 LLM 截断输出中提取的完整对象需要映射到强类型节点。
func mapsToDagNodes(objs []map[string]any) []struct {
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
} {
	nodes := make([]struct {
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
	}, 0, len(objs))
	for _, obj := range objs {
		node := struct {
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
		}{
			ID:           coalesceString(obj, "id"),
			Name:         coalesceString(obj, "name"),
			Task:         coalesceString(obj, "task"),
			SystemPrompt: coalesceString(obj, "systemPrompt"),
			ReviewPrompt: coalesceString(obj, "reviewPrompt"),
			Dependencies: coalesceStringSlice(obj, "dependencies"),
			Feedback:     coalesceStringSlice(obj, "feedback"),
		}
		if v, ok := obj["review"].(bool); ok {
			node.Review = v
		}
		if v, ok := obj["maxRetries"].(float64); ok {
			node.MaxRetries = int(v)
		}
		if v, ok := obj["maxIterations"].(float64); ok {
			node.MaxIterations = int(v)
		}
		nodes = append(nodes, node)
	}
	return nodes
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
//     从而支持「逐模块收敛后才进入下一步」的串联门禁；
//     · 终点节点（无人依赖）→ 回退到直接上游工作节点（Dependencies），
//     形成「工作→审查→修复→审查」的全局闭环；
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
