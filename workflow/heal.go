package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/kasuganosora/thinkbot/agent/outbound"
	"github.com/kasuganosora/thinkbot/llm"
	"github.com/kasuganosora/thinkbot/subagent"
	"github.com/kasuganosora/thinkbot/util/errs"
	"github.com/kasuganosora/thinkbot/util/traceid"
	"go.uber.org/zap"
)

// ============================================================================
// 工作流自愈：失败节点根因诊断 + 局部重分析
//
// 背景：评审类节点反复触顶 30~120min 硬上限而失败时，失败原因有本质区别——
//   - 颗粒度太大：单节点覆盖整个模块组（数千行），单次跑不完；
//   - 端点变慢/瞬断累积：GLM 整体变慢，同样工作量耗时翻倍；
//   - 上下文膨胀：长任务上下文累积导致每步决策变慢；
//   - 额度墙：配额耗尽（已有独立自愈，不在此处理）。
// 固定正则只能说"这节点重试必死"，说不了"为什么死"。因此根因判定交给一个
// 能 read/grep/glob 探查真实代码规模、能读该节点 trace 日志的【诊断 subagent】，
// 而非硬编码规则。
//
// 闭环：节点 failed（仅"硬上限强杀"这类歧义失败才触发）→ 诊断 subagent 判根因
//   → granularity 高置信 → 打回分析器局部重分析（RefineNode）产出更细子图
//   → ReplaceNodeWithSubgraph 动态插入 DAG → 原失败节点下游重连子图叶子 → 续跑。
// 整个过程只动那个失败节点，不影响其它正常节点。
// ============================================================================

// Diagnosis 是诊断 subagent 给出的根因判定。
type Diagnosis struct {
	// Category 根因类别：
	//   granularity   —— 颗粒度太大，需细化（触发 RefineNode）
	//   endpoint      —— LLM 端点变慢/瞬断累积（应退避重试，非细化）
	//   context_bloat —— 上下文膨胀导致每步变慢（细化可降低上下文，也可归 granularity）
	//   quota         —— 额度墙（已有独立自愈，理论上不会到此）
	//   capability    —— 工具档位不足：任务需要的能力超出节点被授予的档位
	//   other         —— 其它/不确定（走现有 failed 路径，交由外部决策）
	//
	// 注意 trySelfHeal 只对 granularity / context_bloat 做自动细化，
	// capability **刻意不自动扩权**——见 SuggestedProfile 的说明。
	Category        string  `json:"category"`
	Confidence      float64 `json:"confidence"`
	Reason          string  `json:"reason"`
	SuggestedAction string  `json:"suggested_action"` // refine | backoff_retry | wait_quota | escalate
	RefineHint      string  `json:"refine_hint"`      // 仅 granularity/context_bloat 时：给分析器的细化建议

	// SuggestedProfile 仅在 category=capability 时给出：诊断认为该节点需要的最低档位。
	//
	// **只记录，绝不自动应用**。自动扩权会与工具档位的整个设计目标相悖——
	// 档位存在的意义就是让节点只拿到任务必需的能力，让自愈有能力自行放宽它，
	// 等于给这道防线开了一个自动化后门。这里只把建议写进诊断结果，
	// 是否提档由人决定。
	SuggestedProfile string `json:"suggested_profile,omitempty"`
}

// healDiagnoseSystemPrompt 根因诊断专家的 system prompt。
// 关键约束：强制 JSON 输出、明确四类根因的判定依据、要求它必须亲自探查真实代码规模
// （而非轻信任务文案里的目录名），避免"凭感觉"错判颗粒度。
const healDiagnoseSystemPrompt = `You are a workflow failure root-cause diagnostician. Given a failed workflow node, your job is to determine WHY it failed, not merely whether it failed.

You have read / grep / glob tools so you can:
- Inspect the code directories and files covered by the failed node to verify the actual file count and line count (do not trust the task wording blindly — inspect it yourself);
- Read the runtime log (default /tmp/thinkbot.log) and look up this node's ID to find its tool call summary (total_calls / steps), LLM generation latency, and whether context deadline exceeded or output truncation occurred.

Base your judgment on real evidence and classify the failure into one of the following categories:

1. granularity (scope too large): the node's task covers too much code (e.g. an entire group of modules totaling thousands of lines), requiring 100+ tool calls that cannot finish within the hard cap. Evidence: huge actual line count, high total_calls (e.g. >80), and normal LLM latency.
   -> suggested_action = "refine", and provide a concrete splitting plan in refine_hint (split by sub-directory or individual files into 2-4 smaller nodes).

2. endpoint (slow or flaky model endpoint): the LLM endpoint slowed down globally or had transient outages, doubling the wall-clock time for an otherwise modest workload. Evidence: repeated context deadline exceeded near this node in the log, other lightweight nodes in the same workflow also slowed down or failed, and the actual code size is small.
   -> suggested_action = "backoff_retry" (do NOT refine; back off and retry instead).

3. context_bloat (context growth): accumulated context in a long-running task slowed down each step and inflated token usage. Evidence: total_calls is moderate, per-call LLM latency climbs steadily, or truncation appears in the log.
   -> suggested_action = "refine" (splitting into smaller nodes shrinks per-node context), with a concrete splitting plan in refine_hint.

4. quota (rate limit / quota wall): 429 or quota-exhaustion errors appear in the log.
   -> suggested_action = "wait_quota".

5. capability (insufficient tool profile): the node's granted toolProfile lacks a capability its task requires. See "Tool Profiles" below — typical evidence is tool-unavailable / tool-not-found errors in the log, or a task that clearly requires command execution or file writes when the current profile omits them.
   -> suggested_action = "escalate", and set suggested_profile to the narrowest profile that satisfies the task.
   NEVER misclassify an insufficient tool profile as granularity: splitting the node will not grant missing tools and only wastes another round.

6. other (uncertain): insufficient evidence or mixed causes that cannot be determined reliably.
   -> suggested_action = "escalate".

## Tool Profiles

A node may be restricted to one of the following tiers (the failed node's current profile is shown in the task prompt):

- readonly — list dir, read file, search content only
- analysis — readonly + run commands / code (tests / lint / build), no file writes
- edit     — analysis (including command / code execution) + create file, replace in file, **no delete or move**
- full     — all workspace tools

Judgment rules:
- First check what capabilities the task itself requires (read? execute? write? delete/move?), then check whether the current profile covers them.
- Task needs to run tests/builds/commands while profile is readonly -> capability
- Task needs to create/edit files while profile is analysis -> capability
- Task needs to delete/move files while profile is edit -> capability
- Huge code scope while the profile already grants the required tools -> granularity, NOT capability

Wrap the final JSON strictly between <result> and </result> in this exact schema:
{"category":"granularity|endpoint|context_bloat|quota|capability|other","confidence":0.0,"reason":"<one-sentence evidence-based explanation>","suggested_action":"refine|backoff_retry|wait_quota|escalate","refine_hint":"<only when granularity or context_bloat>","suggested_profile":"<only when capability: readonly|analysis|edit|full>"}

Write "reason" and "refine_hint" in Chinese (中文). "confidence" is your certainty in [0.0, 1.0]; do not inflate it when evidence is thin.`

// healDiagnoseStuckTimeout 诊断 subagent 的墙钟上限：它必须短，否则"诊断一个卡死节点又卡 30 分钟"本末倒置。
const healDiagnoseStuckTimeout = 5 * time.Minute

// healMinConfidence 触发自动细化的最小置信度。低于此值不自动改 DAG，交外部决策。
const healMinConfidence = 0.6

// maxHealRefinements 单个节点允许被自动细化的最大次数，防止"拆了还超→再拆→爆炸"。
const maxHealRefinements = 2

// DiagnoseNode 运行诊断 subagent 判定失败根因。
// 入参 node 是当前失败的节点；返回诊断结论。任何错误都返回 (zero, err)，由调用方退回到现有 failed 路径。
func (a *Analyzer) DiagnoseNode(ctx context.Context, node *DAGNode, wf *Workflow) (Diagnosis, error) {
	ctx, span := a.tracer.Start(ctx, "workflow.analyzer.diagnose")
	defer span.End()

	logger := traceid.WithLoggerFrom(ctx, a.logger)
	logger.Infow("healing: diagnosing failed node",
		"node_id", node.ID, "node_name", node.Name, "tool_profile", node.ToolProfile, "error", node.Error)

	// 档位必须告诉诊断 agent：否则它看不到「节点被限制了工具」这个事实，
	// 会把档位不足误判成 granularity，然后白白细化一次——拆细节点解决不了缺工具。
	profile := node.ToolProfile
	if profile == "" {
		profile = ProfileFull
	}

	task := fmt.Sprintf(`Failed node diagnosis task:

Node ID: %s
Node Name: %s
Current Tool Profile: %s
Node Task Description:
%s

Failure Error: %s
Retry Count So Far: %d

Original Workflow Requirement:
%s

First use read/grep/glob to inspect the actual code size covered by this node (file count and total lines), then inspect /tmp/thinkbot.log by node ID to extract its tool call summary and LLM latency evidence, and finally determine the root cause.
IMPORTANT: if the failure was caused by an insufficient tool profile for the task, classify it as "capability" and provide "suggested_profile" — do NOT misclassify it as "granularity".
Follow the output schema in your system prompt and wrap the JSON in <result>...</result>.`,
		node.ID, node.Name, profile, node.Task, node.Error, node.RetryCount, wf.Requirement)

	ctx, modelMax := a.liveCall(ctx)
	modelName := a.ec.AnalyzerModel
	if a.ec.BindLiveModel != nil {
		var liveMax int
		ctx, modelName, liveMax = a.ec.BindLiveModel(ctx)
		if liveMax > 0 {
			modelMax = liveMax
		}
	}
	healMax := modelMax
	if a.ec.InternalPolicy != nil {
		healMax = a.ec.InternalPolicy.MaxTokens(llm.PurposeWorkflowHeal, modelMax, 0)
	}
	var healEffort string
	if a.ec.InternalPolicy != nil {
		healEffort = a.ec.InternalPolicy.ReasoningEffort(llm.PurposeWorkflowHeal, modelName)
	}
	// 诊断不改温度。采样跟随模型配置；输出上限只在模型给了正数时发送。
	if modelMax <= 0 {
		healMax = 0
	}
	callCtx, callOpts := a.modelSamplingCall(ctx, healMax,
		subagent.WithReasoningEffort(healEffort),
		subagent.WithStuckTimeout(healDiagnoseStuckTimeout),
	)
	raw, err := a.saMgr.DelegateStream(callCtx, healDiagnoseSystemPrompt, task, callOpts...)
	if err != nil {
		return Diagnosis{}, errs.Wrap(err, "healing diagnosis LLM call failed")
	}

	diag, ok := parseDiagnosis(raw)
	if !ok {
		return Diagnosis{}, errs.New("healing diagnosis output unparsable")
	}
	logger.Infow("healing: diagnosis result",
		"node_id", node.ID, "category", diag.Category, "confidence", diag.Confidence, "action", diag.SuggestedAction)
	return diag, nil
}

// RefineNode 针对失败节点做局部重分析，产出更细的子图节点。
// 产出节点的 id 为相对名（如 a/b/c），dependencies 内部引用；子图第一个节点若依赖
// 原节点的上游，用哨兵 "__UP__" 标记（由 ReplaceNodeWithSubgraph 替换为真实上游依赖）。
// 任何错误都返回 (nil, err)，由调用方退回到现有 failed 路径。
func (a *Analyzer) RefineNode(ctx context.Context, wf *Workflow, failedNode *DAGNode, hint string) ([]*DAGNode, error) {
	ctx, span := a.tracer.Start(ctx, "workflow.analyzer.refine")
	defer span.End()

	logger := traceid.WithLoggerFrom(ctx, a.logger)
	logger.Infow("healing: refining failed node", "node_id", failedNode.ID, "hint", hint)

	task := fmt.Sprintf(`Split a timed-out / oversized workflow node into smaller, independently completable sub-nodes.

Failed Node:
- Name: %s
- Task Description:
%s
- Diagnosis & Splitting Hint (why it timed out and how to split it):
%s

Original Workflow Requirement (for context):
%s

Requirements:
1. Split this node into 2-4 smaller sub-nodes with clear dependencies so each sub-node has a bounded scope small enough to finish well within the 120-minute hard cap (split by sub-directory or individual files, not whole module groups).
2. Use short relative IDs inside the subgraph (e.g. "a", "b", "c"), and reference those relative IDs in internal "dependencies".
3. Entry sub-node(s) that should inherit the failed node's upstream dependencies MUST list the sentinel "__UP__" in their "dependencies" (do NOT write actual upstream node IDs).
4. Exit sub-node(s) are rewired to the failed node's downstream dependents automatically — you do not need to touch downstream nodes, but the subgraph itself must form a valid DAG from entry to exit.
5. Set review=true with a concise reviewPrompt on any sub-node whose output requires quality review. Write "name", "task", "systemPrompt", and "reviewPrompt" values in Chinese (中文).

Wrap the final JSON strictly between <result> and </result> in this format:
{"nodes":[{"id":"a","name":"...","task":"...","dependencies":["__UP__"],"review":false,"reviewPrompt":""}, ...]}`,
		failedNode.Name, failedNode.Task, hint, wf.Requirement)

	ctx, maxTok := a.liveCall(ctx)
	if maxTok <= 0 {
		maxTok = a.ec.AnalyzerMaxTokens
	}
	callCtx, callOpts := a.modelSamplingCall(ctx, maxTok,
		// 细化是「轻量拆解 DAG」，不是执行代码，必须用短超时——否则"诊断一个卡死节点又卡 30 分钟"本末倒置。
		subagent.WithStuckTimeout(3*time.Minute),
	)
	raw, err := a.saMgr.DelegateStream(callCtx, analyzerSystemPrompt, task, callOpts...)
	if err != nil {
		return nil, errs.Wrap(err, "healing refine LLM call failed")
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errs.New("healing refine returned empty response")
	}
	spec, perr := parseDAGSpec(raw)
	if perr != nil {
		return nil, errs.Wrap(perr, "healing refine parse failed")
	}
	if len(spec.Nodes) == 0 {
		return nil, errs.New("healing refine produced 0 nodes")
	}

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
	return nodes, nil
}

// parseDiagnosis 从诊断 subagent 自由文本输出中提取 JSON 诊断结论。
// 复用分析器的 <result> 包裹提取 + 容错 JSON 解析。
func parseDiagnosis(raw string) (Diagnosis, bool) {
	body, ok := extractResultTag(raw)
	if !ok {
		// 退化：尝试直接 json.Unmarshal 整段
		body = raw
	}
	body = strings.TrimSpace(body)
	// 去掉可能的 markdown 代码块包裹
	if strings.HasPrefix(body, "```") {
		if idx := strings.Index(body, "\n"); idx >= 0 {
			body = body[idx+1:]
		}
		if idx := strings.LastIndex(body, "```"); idx >= 0 {
			body = body[:idx]
		}
	}
	var d Diagnosis
	if err := json.Unmarshal([]byte(body), &d); err != nil {
		return Diagnosis{}, false
	}
	if d.Category == "" {
		return Diagnosis{}, false
	}
	if d.Confidence < 0 {
		d.Confidence = 0
	}
	if d.Confidence > 1 {
		d.Confidence = 1
	}
	return d, true
}

// healLogger 是自愈日志的辅助（避免每次构造）。
func (s *Scheduler) healLog(ctx context.Context, msg string, fields ...zap.Field) {
	traceid.WithLoggerFrom(ctx, s.logger).Desugar().With(fields...).Sugar().Infow("healing: " + msg)
}

// healCacheEntry 缓存单节点的诊断结论与已细化次数（防重复诊断 / 无限细化）。
type healCacheEntry struct {
	diagnosis   Diagnosis
	refineCount int
}

// trySelfHeal 在节点执行失败（重试耗尽）后尝试自愈。仅对「硬上限强杀」这类
// 确定性失败（重试必再撞同一墙钟）触发根因诊断，避免无谓诊断普通错误。
//
// 返回 true 表示自愈已接管（节点已被细化子图替换、下游自然续跑），调用方应直接返回，
// 不再标记 failed、不再级联跳过；返回 false 表示无法自愈，走现有 failed 路径。
//
// 线程安全：替换 DAG 时持有 s.mu。调用方（runNode 执行失败分支）此刻尚未加锁，安全。
func (s *Scheduler) trySelfHeal(ctx context.Context, node *DAGNode, lastErr error) bool {
	if s.analyzer == nil {
		return false // 未配置 analyzer → 关闭自愈
	}
	// 配额墙：已由 runNode 的 isQuotaExhausted 分支单独熔断处理，不在此诊断。
	if isQuotaExhausted(lastErr) {
		return false
	}
	if isAuthoritativeNonRetryableLLMError(lastErr) {
		return false
	}
	// 仅「硬上限强杀」这类确定性失败才值得诊断根因（普通错误重试已耗尽，诊断无意义）。
	if !isNonRetryable(lastErr) {
		return false
	}

	// 最大细化深度护栏：已达上限 → 不再细化，交外部决策（监控/用户）。
	if v, ok := s.healCache.Load(node.ID); ok {
		if e, ok2 := v.(healCacheEntry); ok2 && e.refineCount >= maxHealRefinements {
			s.healLog(ctx, "refine depth exhausted, escalate to human", zap.String("node_id", node.ID))
			return false
		}
	}

	// 诊断（结论按 nodeID 缓存，避免每次 fail 都烧一次 LLM）。
	var diag Diagnosis
	if v, ok := s.healCache.Load(node.ID); ok {
		if e, ok2 := v.(healCacheEntry); ok2 {
			diag = e.diagnosis
		}
	}
	if diag.Category == "" {
		d, err := s.analyzer.DiagnoseNode(ctx, node, s.wf)
		if err != nil {
			s.healLog(ctx, "diagnosis failed, fallback", zap.String("node_id", node.ID), zap.Error(err))
			return false
		}
		diag = d
		s.healCache.Store(node.ID, healCacheEntry{diagnosis: diag})
	}

	// capability（工具档位不足）**刻意不自动扩权**，只把建议留痕。
	//
	// 自动放宽档位会让整道最小权限防线形同虚设：档位的意义就在于
	// 「只给任务必需的能力」，若自愈能自行放宽，等于给这道防线开了后门——
	// 一次注入或一次误判就能让节点拿到它本不该有的 exec / 删除能力。
	// 因此这里只记录建议，是否提档由人决定。
	//
	// 注意它也不会落到下面的 refine 分支（那里只认 granularity / context_bloat），
	// 于是自然走 escalate 交外部决策——正是期望的行为。
	if diag.Category == "capability" {
		current := node.ToolProfile
		if current == "" {
			current = ProfileFull
		}
		s.healLog(ctx, "diagnosed as capability gap, NOT auto-escalating",
			zap.String("node_id", node.ID),
			zap.String("current_profile", string(current)),
			zap.String("suggested_profile", diag.SuggestedProfile),
			zap.Float64("confidence", diag.Confidence),
			zap.String("reason", diag.Reason))
	}

	// 仅 granularity / context_bloat 且高置信 → 局部重分析 + 动态替换 DAG。
	if (diag.Category == "granularity" || diag.Category == "context_bloat") && diag.Confidence >= healMinConfidence {
		sub, err := s.analyzer.RefineNode(ctx, s.wf, node, diag.RefineHint)
		if err != nil || len(sub) == 0 {
			s.healLog(ctx, "refine failed, fallback", zap.String("node_id", node.ID), zap.Error(err))
			return false
		}
		s.mu.Lock()
		if err := s.wf.ReplaceNodeWithSubgraph(node.ID, sub); err != nil {
			s.mu.Unlock()
			s.healLog(ctx, "replace subgraph failed, fallback", zap.String("node_id", node.ID), zap.Error(err))
			return false
		}
		s.mu.Unlock()

		// 更新细化计数（含本次）。
		e := healCacheEntry{diagnosis: diag, refineCount: 1}
		if v, ok := s.healCache.Load(node.ID); ok {
			if prev, ok2 := v.(healCacheEntry); ok2 {
				e.refineCount = prev.refineCount + 1
			}
		}
		s.healCache.Store(node.ID, e)
		s.persist()
		s.emitNodeEvent(ctx, outbound.EventWorkflowNodeHealed, map[string]any{
			"node_id":      node.ID,
			"refined_into": len(sub),
			"category":     diag.Category,
			"confidence":   diag.Confidence,
			"reason":       diag.Reason,
		})
		s.healLog(ctx, "node refined and replaced, continuing",
			zap.String("node_id", node.ID), zap.Int("sub_nodes", len(sub)),
			zap.String("category", diag.Category), zap.Float64("confidence", diag.Confidence))
		return true
	}

	// 其它类别（endpoint/quota/other/低置信）→ 不自动改 DAG，走现有 failed 路径。
	s.healLog(ctx, "diagnosis not actionable, fallback to failed",
		zap.String("node_id", node.ID), zap.String("category", diag.Category),
		zap.Float64("confidence", diag.Confidence))
	return false
}
