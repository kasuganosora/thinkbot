package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/kasuganosora/thinkbot/agent/outbound"
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
const healDiagnoseSystemPrompt = `你是一个工作流失败根因诊断专家。给定一个执行失败的工作流节点，你要判断它"为什么失败"，而不是"失败了没有"。

你拥有 read / grep / glob 工具，可以：
- 读取失败节点覆盖的代码目录/文件，确认真实文件数与行数（不要轻信任务文案，要亲自探查）；
- 读取运行日志（默认 /tmp/thinkbot.log），按节点 id 找它的 tool call summary（total_calls / steps）、GLM 生成延迟、是否出现 context deadline exceeded / 截断。

请基于真实证据输出 JSON，判定以下四类根因之一：

1. granularity（颗粒度太大）：节点任务覆盖的代码规模过大（例如整模块组数千行），需要 100+ 次工具调用才能在硬上限内完成，但硬上限内根本跑不完。证据：真实行数巨大、total_calls 很高（如 >80）、且 GLM 延迟正常。
   → suggested_action = "refine"，并在 refine_hint 中给出具体拆分方案（按子目录/单文件拆成 2~4 个更小节点）。

2. endpoint（端点变慢/瞬断）：GLM 整体变慢或短暂不可用导致同样的真实工作量耗时翻倍。证据：日志里该节点附近大量 context deadline exceeded、同工作流其它不重节点也变慢或失败、真实代码规模其实不大。
   → suggested_action = "backoff_retry"（不应细化，应退避后重试）。

3. context_bloat（上下文膨胀）：长任务上下文累积导致每步决策变慢、token 消耗飙升。证据：total_calls 不算极端但单次 GLM 调用耗时持续攀升、日志出现截断。
   → suggested_action = "refine"（拆细节点可降低单节点上下文），refine_hint 给出拆分方案。

4. quota（额度墙）：日志出现 429 / 配额耗尽类错误。
   → suggested_action = "wait_quota"。

5. capability（工具档位不足）：节点被授予的工具档位不足以完成它的任务。判定依据见下方「工具档位」一节——典型证据是日志里出现工具不可用/未找到，或任务明确需要执行、写入能力而当前档位不含。
   → suggested_action = "escalate"，并在 suggested_profile 里给出你认为需要的最低档位。
   **不要**因为档位不足而判成 granularity：拆细节点解决不了缺工具，只会白白多跑一轮。

6. other（不确定）：证据不足或多种原因混合无法判定。
   → suggested_action = "escalate"。

## 工具档位

节点可能被限定在以下档位之一（失败节点的当前档位见任务描述）：

- readonly —— 只能列目录、读文件、搜内容
- analysis —— readonly + 执行命令（跑测试/lint/构建），但不能写文件
- edit     —— analysis + 新建文件、局部替换，但**不能删除或移动**
- full     —— 全部工具

判定要点：
- 先确认任务本身需要什么能力（读？执行？写？删除？），再看当前档位是否覆盖。
- 任务要跑测试/构建而档位是 readonly → capability
- 任务要改文件而档位是 analysis → capability
- 代码规模巨大但档位够用 → 那是 granularity，不是 capability

把最终 JSON 放在 <result>...</result> 之间，格式：
{"category":"granularity|endpoint|context_bloat|quota|capability|other","confidence":0.0~1.0,"reason":"基于真实证据的一句话说明","suggested_action":"refine|backoff_retry|wait_quota|escalate","refine_hint":"仅 granularity/context_bloat 时给出","suggested_profile":"仅 capability 时给出：readonly|analysis|edit|full"}

confidence 是你对判定的把握（0~1）。证据不足时不要虚高。`

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

	task := fmt.Sprintf(`失败节点诊断任务：

节点 id: %s
节点名: %s
当前工具档位: %s
节点任务描述:
%s

失败错误: %s
已重试次数: %d

原始工作流需求:
%s

请先 read/grep/glob 探查该节点任务覆盖的真实代码规模（文件数、总行数），再读 /tmp/thinkbot.log 按节点 id 提取它的 tool call summary 与 GLM 延迟证据，然后判定根因。
特别注意：若失败源于「当前工具档位不足以完成任务」，请判为 capability 并给出建议档位，**不要**判成 granularity。
输出格式见 system prompt，放在 <result>...</result> 之间。`,
		node.ID, node.Name, profile, node.Task, node.Error, node.RetryCount, wf.Requirement)

	raw, err := a.saMgr.DelegateStream(ctx, healDiagnoseSystemPrompt, task,
		subagent.WithTemperature(0),
		subagent.WithMaxTokens(2000),
		subagent.WithStuckTimeout(healDiagnoseStuckTimeout),
	)
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

	task := fmt.Sprintf(`把一个执行超时的评审节点拆成更小、可独立完成的子节点。

失败节点：
- 名称: %s
- 任务描述:
%s
- 诊断建议（为什么超时、怎么拆）:
%s

原始工作流需求（供你理解上下文）:
%s

要求：
1. 把该节点拆成 2~4 个更小、彼此有依赖关系的子节点，每个子节点任务范围足够小，能在 120 分钟硬上限内完成（按子目录/单文件拆分，不要整模块组）。
2. 子图内部用相对 id（如 "a"、"b"、"c"），dependencies 引用子图内部相对 id。
3. 子图第一个节点应继承原节点的上游依赖：在其 dependencies 里写哨兵 "__UP__"（不要写真实上游 id）。
4. 子图最后一个节点的下游会自动接回原节点的下游，你无需处理——但子图内部必须形成从入口到出口的链/树。
5. 每个子节点若需审查，设 review=true 并给简短 reviewPrompt。

把最终 JSON 放在 <result>...</result> 之间，格式：
{"nodes":[{"id":"a","name":"...","task":"...","dependencies":["__UP__"],"review":false,"reviewPrompt":""}, ...]}`,
		failedNode.Name, failedNode.Task, hint, wf.Requirement)

	raw, err := a.saMgr.DelegateStream(ctx, analyzerSystemPrompt, task,
		subagent.WithTemperature(a.ec.AnalyzerTemperature),
		subagent.WithMaxTokens(a.ec.AnalyzerMaxTokens),
		// 细化是「轻量拆解 DAG」，不是执行代码，必须用短超时——否则"诊断一个卡死节点又卡 30 分钟"本末倒置。
		subagent.WithStuckTimeout(3*time.Minute),
	)
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
