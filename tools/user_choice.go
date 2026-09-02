package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	agenttools "github.com/kasuganosora/thinkbot/agent/tools"
	"github.com/kasuganosora/thinkbot/internal/interaction"
	"github.com/kasuganosora/thinkbot/llm"
	"github.com/kasuganosora/thinkbot/util/idgen"
)

// ============================================================================
// user_choice 工具 — 阻塞式向用户提问并等待选择
//
// 平台支持现状（**以实现为准**）：
//   - web：已支持。工具进度事件携带结构化 payload → 前端 ChoiceCard 内联卡片，
//     点选/输入后经 POST /api/user-choice/{questionId}/answer 回填。
//   - misskey：已支持。通过 Misskey 原生 poll 实现；至少 2 个选项。
//     用户在 Misskey UI 上点选 → WS pollVoted → interaction.ResolveFrom。
//   - telegram：已支持。channel 发送 InlineKeyboardMarkup，callback_query
//     经 interaction.ResolveFrom 回填（单选一点即决；多选需点「确认」）。
// ============================================================================

// userChoiceKeepaliveInterval 是等待用户作答期间重发卡片事件的间隔。
// 必须明显小于 SSE 的空闲超时（api/handler_chat.go idleTimeout=120s），
// 取 30s 留足余量（丢一两次事件也不至于触发空闲判定）。
const userChoiceKeepaliveInterval = 30 * time.Second

// userChoiceMinTimeoutSecs 是把 TimeoutSecs 钳到 ctx deadline 时的下限。
// 剩 0~1 秒会被当成「立刻超时」，看起来像取消；留几秒让 Wait 能以 timeout 终态返回。
const userChoiceMinTimeoutSecs = 5

// userChoiceInput 是工具入参（LLM 生成）。
type userChoiceInput struct {
	Question    string          `json:"question"`
	Options     []userChoiceOpt `json:"options"`
	Mode        string          `json:"mode"`
	InputHint   string          `json:"input_hint,omitempty"`
	TimeoutSecs int             `json:"timeout_secs,omitempty"`
}

type userChoiceOpt struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// UserChoiceEventPayload 是随 tool_progress 事件发给前端的卡片渲染数据。
// 前端（web）与 channel 层（telegram/misskey 的发送路径）共用这个结构。
//
// 字段命名即线上契约，改动必须同步 web/src/stores/bot.js 的 extractChoicePayload
// 与 web/src/components/ChoiceCard.vue 的 props.payload：
//   - Type 是前端识别「这条进度事件是选择卡」的判别键；
//   - Options 必须带 id（由 interaction.RegisterQuestion 补齐后回读），
//     前端以 id 为选中标识并原样回填；
//   - TimeoutAt 是绝对到期时间（unix 毫秒），刷新后前端据此估算剩余时间；
//     Timeout（秒）保留给 channel 层文案使用。
type UserChoiceEventPayload struct {
	Type       string               `json:"type"` // 固定 "user_choice"
	QuestionID string               `json:"questionId"`
	Question   string               `json:"question"`
	Options    []interaction.Option `json:"options"`
	Mode       string               `json:"mode"`
	InputHint  string               `json:"inputHint,omitempty"`
	Timeout    int                  `json:"timeout"`
	TimeoutAt  int64                `json:"timeoutAt"`
	Via        string               `json:"via"` // 本次提问来源平台（决定回填来源）
	// BotID / ChatID / ReplyTarget 供 channel 层原生渲染时定位发送目标，
	// web 前端不使用。
	BotID       string `json:"botId,omitempty"`
	ChatID      string `json:"chatId,omitempty"`
	ReplyTarget string `json:"replyTarget,omitempty"`
}

// capTimeoutSecs 若 ctx 带 deadline 且 timeoutSecs 超过剩余时间，则钳到剩余秒数
// （至少 userChoiceMinTimeoutSecs）。无 deadline 时原样返回（0/负值留给 RegisterQuestion 默认化）。
func capTimeoutSecs(ctx context.Context, timeoutSecs int) int {
	if timeoutSecs <= 0 {
		timeoutSecs = interaction.DefaultTimeoutSecs
	}
	if ctx == nil {
		return timeoutSecs
	}
	dl, ok := ctx.Deadline()
	if !ok {
		return timeoutSecs
	}
	remain := int(time.Until(dl) / time.Second)
	if remain < userChoiceMinTimeoutSecs {
		remain = userChoiceMinTimeoutSecs
	}
	if timeoutSecs > remain {
		return remain
	}
	return timeoutSecs
}

// userChoiceToolDef 创建 user_choice 工具定义。
func userChoiceToolDef() agenttools.ToolDef {
	return agenttools.ToolDef{
		Tool: llm.Tool{
			Name: "user_choice",
			Description: "Present a question with selectable options to the user and BLOCK until they answer. " +
				"Rendered natively per platform (web: interactive card, misskey: native poll, telegram: inline buttons). " +
				"Returns {status:\"answered\", selected:[...], custom_input:\"...\", via:\"...\"} or {status:\"timeout\"}. " +
				"On timeout, gracefully continue without the user's input (make a sensible default decision or ask again later) — never loop waiting.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"question": map[string]any{
						"type":        "string",
						"description": "Question text shown to the user",
					},
					"options": map[string]any{
						"type": "array",
						"items": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"label":       map[string]any{"type": "string"},
								"description": map[string]any{"type": "string"},
							},
							"required": []string{"label"},
						},
						"description": "1-8 selectable options. The last option for free input is NOT needed here — the input box is always rendered automatically.",
					},
					"mode": map[string]any{
						"type":        "string",
						"enum":        []string{"single", "multi"},
						"description": "single = pick exactly one; multi = pick any number",
					},
					"input_hint": map[string]any{
						"type":        "string",
						"description": "Placeholder text for the free-input box, e.g. \"or type your own answer\"",
					},
					"timeout_secs": map[string]any{
						"type":        "integer",
						"description": "How long to wait for the user, default 600",
					},
				},
				"required": []string{"question", "options", "mode"},
			},
			Execute: llm.ToolExecuteFunc(execUserChoice),
		},
		Category: "utility",
		Scopes:   []string{"private"}, // 只在有明确对话对象的场景可用；群聊/子代理提问无人应答
	}
}

// execUserChoice 是 user_choice 的执行体：注册问题 → 发 progress 事件（携带
// 渲染 payload）→ 阻塞等待用户应答 → 返回统一 JSON。
func execUserChoice(ctx *llm.ToolExecContext, input any) (any, error) {
	// ---- 解析入参 ----
	raw, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("user_choice: 无法序列化入参: %w", err)
	}
	var in userChoiceInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("user_choice: 入参格式错误: %w", err)
	}
	if strings.TrimSpace(in.Question) == "" {
		return nil, fmt.Errorf("user_choice: question 不能为空")
	}
	if len(in.Options) < interaction.MinOptions || len(in.Options) > interaction.MaxOptions {
		return nil, fmt.Errorf("user_choice: options 须为 %d~%d 个，实际 %d 个",
			interaction.MinOptions, interaction.MaxOptions, len(in.Options))
	}
	if in.Mode != string(interaction.ModeSingle) && in.Mode != string(interaction.ModeMulti) {
		return nil, fmt.Errorf("user_choice: mode 须为 single 或 multi")
	}
	in.TimeoutSecs = capTimeoutSecs(ctx.Context, in.TimeoutSecs)

	// ---- 来源平台：从 CallOrigin / context 推断 ----
	// interaction 包不依赖 api 层，这里用轻量 context key 读取本轮消息元信息
	// （由 LLMStage 注入，见 agent/stages/llmroute.go 的 WithMessageMeta）。
	meta := agenttools.MessageMetaFromContext(ctx.Context)
	via := string(interaction.ViaWeb)
	switch meta.ChannelType {
	case "", "web":
		// web，或无 channel 上下文的内部调用（子代理 / 单测）：走完整交互流程。
	default:
		// Misskey 原生 poll 至少 2 项；1 项在 CreatePollNote 也会失败，这里提前降级
		// 避免先 RegisterQuestion 再踩雷留下 pending。
		if meta.ChannelType == "misskey" && len(in.Options) < 2 {
			return map[string]any{
				"status":   "unsupported",
				"platform": meta.ChannelType,
				"message":  "Misskey 投票至少需要 2 个选项。请增加选项，或改用普通文本把问题列出来让用户直接回复；不要重试本工具。",
			}, nil
		}
		// 非 web 平台：尝试使用平台原生的投票创建器（Misskey poll / Telegram inline keyboard）。
		// 若该平台已注册 PollCreator，创建原生控件 + 注册问题 + 阻塞等待结果。
		// 未注册的平台仍降级为 unsupported（让模型用纯文本提问）。
		pollFn := interaction.GetPollCreator(meta.ChannelType)
		if pollFn == nil {
			return map[string]any{
				"status":   "unsupported",
				"platform": meta.ChannelType,
				"message": "当前平台暂不支持交互式选择卡。请改用普通文本消息把问题和候选项列出来（例如编号列表），" +
					"让用户直接回复；不要重试本工具。",
			}, nil
		}

		// ---- 非 web 平台：走原生投票路径 ----
		// 1. 注册问题（与 web 路径相同，用于超时/取消等终态管理）
		opts := make([]interaction.Option, len(in.Options))
		for i, o := range in.Options {
			opts[i] = interaction.Option{Label: o.Label, Description: o.Description}
		}
		questionID := idgen.New("uc")
		q := interaction.Question{
			ID:          questionID,
			BotID:       meta.BotID,
			ChatID:      meta.ChatID,
			Question:    in.Question,
			Options:     opts,
			Mode:        interaction.Mode(in.Mode),
			InputHint:   in.InputHint,
			TimeoutSecs: in.TimeoutSecs,
		}
		reg, err := interaction.Default().RegisterQuestion(q)
		if err != nil {
			return nil, fmt.Errorf("user_choice: 注册问题失败: %w", err)
		}
		defer interaction.Default().CleanupFinal(questionID)

		// 2. 提取选项文本列表（PollCreator 需要 []string，不是 []Option）
		optLabels := make([]string, len(in.Options))
		for i, o := range in.Options {
			optLabels[i] = o.Label
		}

		// 3. 通过平台 PollCreator 创建原生投票帖
		multiple := in.Mode == string(interaction.ModeMulti)
		noteID, err := pollFn(ctx.Context, in.Question, meta.ReplyTarget, optLabels, multiple, reg.TimeoutSecs, questionID)
		if err != nil {
			// CleanupFinal 对 pending 是空操作；必须先 Cancel 再清理，否则问题泄漏。
			interaction.Default().AbortPending(questionID)
			return map[string]any{
				"status":   "unsupported",
				"platform": meta.ChannelType,
				"message":  fmt.Sprintf("投票帖创建失败: %v。请改用纯文本提问。", err),
			}, nil
		}

		// 4. 阻塞等待（与 web 路径相同的 Wait 机制）
		//    唤醒来源：channel 层的 handlePollVoted → interaction.Resolve
		snap, ans, err := interaction.Default().Wait(ctx.Context, questionID)
		if err != nil {
			if err == interaction.ErrTimeout {
				return map[string]any{
					"status":     "timeout",
					"questionId": questionID,
					"noteId":     noteID,
					"message":    "等待用户投票超时。请基于已有信息给出合理的默认处理，或稍后再问；不要反复重试本工具。",
				}, nil
			}
			return nil, fmt.Errorf("user_choice: 等待中断: %w", err)
		}
		_ = snap

		// 5. 组装返回（与 web 路径格式一致）
		renderOpts := reg.Options
		selectedIDs := make([]string, 0, len(ans.Selected))
		labels := make([]string, 0, len(ans.Selected))
		for _, idx := range ans.Selected {
			if idx >= 0 && idx < len(renderOpts) {
				selectedIDs = append(selectedIDs, renderOpts[idx].ID)
				labels = append(labels, renderOpts[idx].Label)
			}
		}
		selected := make([]int, len(ans.Selected))
		copy(selected, ans.Selected)

		return map[string]any{
			"status":          "answered",
			"questionId":      questionID,
			"noteId":          noteID,
			"via":             string(ans.Via),
			"custom_input":    ans.CustomInput,
			"freeText":        ans.CustomInput,
			"selected":        selected,
			"selected_labels": labels,
			"selectedIds":     selectedIDs,
		}, nil
	}

	// ---- 注册问题 ----
	opts := make([]interaction.Option, len(in.Options))
	for i, o := range in.Options {
		opts[i] = interaction.Option{Label: o.Label, Description: o.Description}
	}
	questionID := idgen.New("uc")
	q := interaction.Question{
		ID:          questionID,
		BotID:       meta.BotID,
		ChatID:      meta.ChatID,
		Question:    in.Question,
		Options:     opts,
		Mode:        interaction.Mode(in.Mode),
		InputHint:   in.InputHint,
		TimeoutSecs: in.TimeoutSecs, // 0 → 包内取默认 600
	}
	reg, err := interaction.Default().RegisterQuestion(q)
	if err != nil {
		return nil, fmt.Errorf("user_choice: 注册问题失败: %w", err)
	}
	defer interaction.Default().CleanupFinal(questionID)

	// 回读注册后的选项快照：选项 id 是注册时补齐的（见 interaction.Option.ID），
	// 必须用 reg.Options 而不是本地 opts，否则下发给前端的选项没有 id，
	// 前端按 id 过滤后得到空选项列表——卡片显示出来但一个都点不了。
	renderOpts := reg.Options

	// ---- 通知渲染层：progress 事件携带结构化 payload ----
	// web SSE → 前端 ChoiceCard 内联卡片。
	//
	// payload 只构造一次：等待期心跳要重发**完全相同**的这份数据，
	// timeoutAt 每次重算会让前端倒计时不断被推后（永远倒不完）。
	ev := UserChoiceEventPayload{
		Type:        "user_choice",
		QuestionID:  questionID,
		Question:    in.Question,
		Options:     renderOpts,
		Mode:        in.Mode,
		InputHint:   in.InputHint,
		Timeout:     reg.TimeoutSecs,
		TimeoutAt:   time.Now().Add(time.Duration(reg.TimeoutSecs) * time.Second).UnixMilli(),
		Via:         via,
		BotID:       meta.BotID,
		ChatID:      meta.ChatID,
		ReplyTarget: meta.ReplyTarget,
	}
	if ctx.SendProgress != nil {
		ctx.SendProgress(ev)

		// 等待期心跳：SSE 写端有 120s 空闲超时（api/handler_chat.go 的 idleTimeout，
		// 任意事件都会重置），而本工具默认阻塞 600s。等待期间一条事件都不发的话，
		// 用户刚看到卡片、还在思考，2 分钟后流就被判空闲切断（前端收到 error:
		// idle timeout，回复转入后台落库，不刷新页面看不到结果）。
		//
		// 与 task 工具阻塞等待时推进度是同一手法。重发同一份 payload 而不是发
		// 「还在等」文本：既保活，又让中途重连/漏事件的客户端能补到卡片，
		// 且前端 registerChoice 按 questionId 合并，重复事件天然幂等。
		done := make(chan struct{})
		defer close(done)
		go func() {
			tk := time.NewTicker(userChoiceKeepaliveInterval)
			defer tk.Stop()
			for {
				select {
				case <-done:
					return
				case <-ctx.Context.Done():
					return
				case <-tk.C:
					ctx.SendProgress(ev)
				}
			}
		}()
	}

	// ---- 阻塞等待（问题超时 / ctx 取消，谁先到算谁）----
	snap, ans, err := interaction.Default().Wait(ctx.Context, questionID)
	if err != nil {
		if err == interaction.ErrTimeout {
			// questionId 必须带上：前端靠它把对应卡片切到 timeout 终态，
			// 否则刷新前那张卡会永远停在「等待作答」。
			return map[string]any{
				"status":     "timeout",
				"questionId": questionID,
				"message":    "等待用户应答超时。请基于已有信息给出合理的默认处理，或稍后再问；不要反复重试本工具。",
			}, nil
		}
		return nil, fmt.Errorf("user_choice: 等待中断: %w", err)
	}
	_ = snap

	// ---- 组装返回 ----
	// 这份 output 有两个消费者，字段要同时喂饱：
	//   - LLM：status / selected / selected_labels / custom_input（下标+文案）；
	//   - 前端（tool_result 事件与落库历史恢复）：questionId / selectedIds /
	//     freeText —— 缺 questionId 就无法把终态锚回卡片。
	selectedIDs := make([]string, 0, len(ans.Selected))
	labels := make([]string, 0, len(ans.Selected))
	for _, idx := range ans.Selected {
		if idx >= 0 && idx < len(renderOpts) {
			selectedIDs = append(selectedIDs, renderOpts[idx].ID)
			labels = append(labels, renderOpts[idx].Label)
		}
	}
	selected := make([]int, len(ans.Selected))
	copy(selected, ans.Selected)

	return map[string]any{
		"status":          "answered",
		"questionId":      questionID,
		"via":             string(ans.Via),
		"custom_input":    ans.CustomInput,
		"freeText":        ans.CustomInput,
		"selected":        selected,
		"selected_labels": labels,
		"selectedIds":     selectedIDs,
	}, nil
}
