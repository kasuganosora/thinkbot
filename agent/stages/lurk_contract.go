package stages

import (
	"context"
	"encoding/json"
	"strings"

	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/agent/memory"
	"github.com/kasuganosora/thinkbot/llm"
)

// ============================================================================
// 潜水观察者结构化输出契约（语言无关）
// ============================================================================
//
// 设计背景（重要，勿回退）：
// 早期实现让模型用自然语言表达「没什么值得记的」，代码侧再去枚举匹配
// [NONE] / [无] / [なし] / 「无需记忆」…… 这是打地鼠：补掉日语，下次印地语
// (कुछ नहीं) 又漏，泄漏进 L0 污染长期记忆。根因不是枚举得不够全，而是
// 「跳过」这个信号由模型自由选词，而我们又不敢信它。
//
// 现在改为：跳过与否是一个由我们定义的 JSON 布尔 remember，与模型的
// 思考语言完全无关。日语/印地语/韩语帖子都只影响 note 正文，不影响判定。
// 因此本文件中 **不得** 出现任何「无/なし/nothing」之类的自然语言枚举。
//
// glm-5.2 / bigmodel 实测结论（2026-08-13 探活）：
//   - response_format=json_object：content 为纯净可解析 JSON，无 markdown 围栏；
//   - json_schema strict：请求被接受但 **不强制** required（少返字段照样 200），
//     因此不可依赖，字段校验一律在 Go 侧做（见 validate）；
//   - reasoning_content 与 content 天然分离，思考不会污染 content；
//   - max_tokens 过小时思考吃光预算 → content 为空 + finish_reason=length，
//     此时重试必须「先加预算」而非原样重推（否则必然复现，白烧 token）。

// lurkObserverInstruction 是潜水观察者模式的系统提示词后缀（英文，遵循 LLM 提示词约定）。
// 它把 LLM 的产出从「回复」重新导向「从帖子里学到什么」，并强制结构化 JSON 输出。
//
// 注意：文案中必须出现 "json" 字样 —— OpenAI 兼容的 json_object 模式要求提示词
// 内提及 json，否则部分供应商会直接报错。已由 TestLurkPromptMentionsJSON 钉住。
const lurkObserverInstruction = `[OBSERVER MODE — LURK / READ-ONLY]
You are in lurk mode. You are silently observing a public social timeline and you will NOT send any reply to anyone. No message leaves this session.
Your job is to learn, not to respond. Analyze the post you just read through the lens of your own identity and values (defined above). Decide whether it contains anything worth remembering for future interaction with this person or community, for example:
- the speaker's technical preferences, stack, or current projects
- explicit needs, questions, or requests
- mood, relationship, or how they expect you to help
- anything that would make you more useful next time

OUTPUT FORMAT — respond with STRICTLY ONE json object and nothing else:
{"remember": true, "note": "<concise first-person internal note>", "ephemeral": false, "speaker_handle": "@handle", "importance": 3}
or, when there is nothing worth remembering:
{"remember": false}

Rules:
- If there is nothing worth remembering, output exactly {"remember": false} and NOTHING else. Do NOT write a note explaining why you decided not to remember it.
- Never express "nothing to remember" as prose or as a placeholder token in any language. The boolean field is the only channel for that decision.
- "note" is written in first person, for your own future reference, and is never sent to anyone.
- "ephemeral": true when the content is time-sensitive and loses value quickly (broadcast schedules, release dates, one-off news, live-stream times). Such notes are kept short-term only and are never promoted to long-term memory.
- "speaker_handle": the author's handle exactly as it appears (e.g. "@blogtalk"). Use "" if genuinely unknown. Never replace it with a generic word like "the user".
- "importance": integer 1-5 (1 = trivia, 5 = essential to how I help this person).
- Output raw json only. No markdown code fences, no commentary before or after. Do not translate or rename the keys.`

// lurkOutput 是潜水观察者的结构化产出。
//
// 字段用指针/显式校验区分「模型没给」与「模型给了零值」：remember 是判定核心，
// 缺失即视为格式违约（进重试），绝不静默当 false —— 否则模型漏字段就等于悄悄丢笔记。
type lurkOutput struct {
	Remember      *bool  `json:"remember"`
	Note          string `json:"note"`
	Ephemeral     bool   `json:"ephemeral"`
	SpeakerHandle string `json:"speaker_handle"`
	Importance    int    `json:"importance"`
}

// lurkParseOutcome 描述一次潜水产出的解析结果，供调用方决定「存 / 跳过 / 重试」。
type lurkParseOutcome int

const (
	// lurkParseOK 解析成功且模型判定值得记忆，note 可用。
	lurkParseOK lurkParseOutcome = iota
	// lurkParseSkip 解析成功且模型判定无需记忆（remember=false）。正常路径，不写库。
	lurkParseSkip
	// lurkParseInvalid 输出不符合契约（非法 json / 缺 remember / remember=true 但 note 空）。
	// 调用方应重试；重试耗尽则放弃，绝不落库半成品。
	lurkParseInvalid
)

// parseLurkOutput 解析模型的潜水产出。
//
// 严格模式：只认契约 JSON，不做任何自然语言兜底 —— 这是「语言无关」的前提。
// 解析失败一律走重试而非猜测，猜测就是重新引入语言枚举。
//
// 容错仅限于与语言无关的格式噪音：思考残留、markdown 围栏、JSON 前后的多余文本。
func parseLurkOutput(raw string) (out lurkOutput, outcome lurkParseOutcome) {
	// 防御性剥离：实测 bigmodel 的思考在独立字段，content 本就干净；
	// 但换供应商/模型时思考可能内联进 content，这里统一先剥。
	s := memory.StripThinking(raw)
	s = stripCodeFence(s)
	s = extractFirstJSONObject(s)
	if s == "" {
		return out, lurkParseInvalid
	}
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return out, lurkParseInvalid
	}
	// remember 缺失 = 契约违约。不可默认 false，否则模型漏字段会静默吞掉真实观察。
	if out.Remember == nil {
		return out, lurkParseInvalid
	}
	if !*out.Remember {
		return out, lurkParseSkip
	}
	// remember=true 却没给 note：同样是违约，交给重试而不是当 skip 丢掉。
	out.Note = strings.TrimSpace(out.Note)
	if out.Note == "" {
		return out, lurkParseInvalid
	}
	// importance 越界时收敛到合法区间，避免脏值流入 dreaming 打分。
	if out.Importance < 1 {
		out.Importance = 1
	} else if out.Importance > 5 {
		out.Importance = 5
	}
	out.SpeakerHandle = strings.TrimSpace(out.SpeakerHandle)
	return out, lurkParseOK
}

// stripCodeFence 去掉 ```json ... ``` 围栏。与语言无关的纯格式处理。
func stripCodeFence(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	// 去掉起始围栏行（可能带 json 语言标注）
	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		s = s[idx+1:]
	} else {
		return s
	}
	if idx := strings.LastIndex(s, "```"); idx >= 0 {
		s = s[:idx]
	}
	return strings.TrimSpace(s)
}

// extractFirstJSONObject 从文本中抽取第一个平衡的 {...} 片段。
// 用于容忍模型在 JSON 前后附加说明文字（格式噪音，与语言无关）。
// 会正确跳过字符串字面量内的花括号与转义。
func extractFirstJSONObject(s string) string {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return ""
	}
	depth := 0
	inStr := false
	escaped := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inStr {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return ""
}

// lurkRetryNudge 构造重试时追加的纠正指令。把模型上一次的非法输出回贴给它，
// 比单纯重发原 prompt 的纠正效果好得多（模型能看到自己错在哪）。
// 指令本身语言无关：只谈 json 结构，不谈「无/なし」等具体词。
func lurkRetryNudge(badOutput string) string {
	bad := strings.TrimSpace(badOutput)
	const maxEcho = 400 // 回贴截断：避免把超长垃圾输出灌回上下文
	if len(bad) > maxEcho {
		bad = bad[:maxEcho] + "…"
	}
	var b strings.Builder
	b.WriteString("\n\n[FORMAT VIOLATION — RETRY]\nYour previous output did not satisfy the required json contract.\n")
	if bad != "" {
		b.WriteString("Your previous output was:\n")
		b.WriteString(bad)
		b.WriteString("\n")
	}
	b.WriteString(`You MUST now output exactly ONE raw json object and nothing else:
{"remember": true, "note": "...", "ephemeral": false, "speaker_handle": "@handle", "importance": 3}
or
{"remember": false}
The "remember" field is REQUIRED and must be a json boolean. When remember is true, "note" must be a non-empty string. No markdown fences, no explanation, no text outside the json object.`)
	return b.String()
}

// ============================================================================
// 重试
// ============================================================================

// lurkMaxRetries 是契约违约后的最大重试次数（不含首次调用）。
// 取 2：实测低温下模型首次即合规，重试主要用于兜偶发抖动；再多只是线性烧 token。
const lurkMaxRetries = 2

// lurkTruncationBudgetFactor 是截断重试时放大输出预算的倍数。
// 截断的根因是思考吃光了预算（max_tokens 是思考+正文共享），
// 原样重推必然复现 —— 必须先加预算，这是「盲重试打不死」的关键。
const lurkTruncationBudgetFactor = 3

// lurkRetryTemperature 是重试时的采样温度：比首次更低，尽量压制格式发散。
const lurkRetryTemperature = 0.0

// lurkFirstTemperature 是潜水首次调用的采样温度。结构化输出不需要创造性。
const lurkFirstTemperature = 0.1

// lurkTemperature 返回潜水首次调用的温度指针。
func lurkTemperature() *float64 {
	t := lurkFirstTemperature
	return &t
}

// lurkImportanceToScore 把模型给的 1~5 整数重要度转成记忆层的 0.0~1.0 量纲。
//
// 必须转换，不可直接透传整数：
//   - Entry.Importance 语义是 0.0~1.0（参与召回打分 context_packer 的 score*0.3）；
//   - note_handler 读 metadata["importance"] 时**只接受 float64**，
//     传 int 会被静默忽略并落回默认 0.5 —— 属于「看起来生效其实没生效」的坑。
//
// 映射：1→0.2, 2→0.4, 3→0.6, 4→0.8, 5→1.0
func lurkImportanceToScore(importance int) float64 {
	if importance < 1 {
		importance = 1
	} else if importance > 5 {
		importance = 5
	}
	return float64(importance) / 5.0
}

// retryLurkUntilValid 在潜水产出不满足契约时重试，返回最终结果。
//
// 关键设计：按失败类型分流，而不是盲目原样重推 ——
//   - finish_reason=length（思考吃光预算、content 为空）：放大 max_tokens 并降低
//     reasoning effort 后重试。盲重试对截断完全无效，只会复现并白烧 3 倍 token。
//   - 格式违约：把上次非法输出回贴 + 硬化指令 + 降温后重试。
//
// 重试全部失败时返回最后一次结果，由 emitLurkNote 判定为 invalid 并放弃落库
// （安全失败：宁可丢一条观察，也不污染记忆）。
func (s *LLMStage) retryLurkUntilValid(
	ctx context.Context,
	cfg *llm.OrchestrateConfig,
	result *llm.GenerateResult,
	logger *zap.SugaredLogger,
	env *core.Envelope,
) *llm.GenerateResult {
	baseSystem := cfg.Params.System

	for attempt := 1; attempt <= lurkMaxRetries; attempt++ {
		if _, outcome := parseLurkOutput(result.Text); outcome != lurkParseInvalid {
			return result
		}

		truncated := result.FinishReason == llm.FinishReasonLength
		retryCfg := *cfg
		retryCfg.Params = cfg.Params

		if truncated {
			// 加预算 + 降思考深度，给 content 留出空间。
			if cfg.Params.MaxTokens != nil {
				boosted := *cfg.Params.MaxTokens * lurkTruncationBudgetFactor
				retryCfg.Params.MaxTokens = &boosted
			}
			low := "low"
			retryCfg.Params.ReasoningEffort = &low
			retryCfg.Params.System = baseSystem
		} else {
			retryCfg.Params.System = baseSystem + lurkRetryNudge(result.Text)
		}
		temp := lurkRetryTemperature
		retryCfg.Params.Temperature = &temp

		logger.Warnw("lurk: json contract violated, retrying",
			"message_id", env.Message.ID,
			"attempt", attempt,
			"max_retries", lurkMaxRetries,
			"reason", map[bool]string{true: "truncated", false: "format"}[truncated],
			"finish_reason", result.FinishReason,
			"raw_len", len(result.Text))

		retried, err := llm.OrchestrateGenerate(ctx, s.provider, &retryCfg)
		if err != nil {
			// 重试调用本身失败：保留上一次结果，由调用方判定放弃。
			logger.Warnw("lurk: retry generate failed, keeping previous result",
				"message_id", env.Message.ID, "attempt", attempt, "err", err)
			return result
		}
		result = retried
	}
	return result
}

