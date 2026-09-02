package tools

import (
	"encoding/json"
	"fmt"
	"strings"

	agenttools "github.com/kasuganosora/thinkbot/agent/tools"
	"github.com/kasuganosora/thinkbot/internal/interaction"
	"github.com/kasuganosora/thinkbot/llm"
	"github.com/kasuganosora/thinkbot/util/idgen"
)

// ============================================================================
// user_choice 工具 — 阻塞式向用户提问并等待选择
//
// 三平台渲染：
//   - web：工具进度事件携带结构化 payload → 前端 ChoiceCard 内联卡片，
//     点选/输入后经 POST /api/chat/choice 回填；
//   - telegram：channel 发 InlineKeyboardMarkup（点按钮即回填）；
//   - misskey：纯文本编号列表（用户回复数字或文字）。
//
// 无论来源平台是什么，工具执行时都会发 progress 事件（web 前端据此渲染
// 卡片；其他平台 channel 在自己的发送路径里拿 payload 做原生渲染），
// 并把问题注册到 interaction 注册表等待对应平台回填。
// ============================================================================

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
type UserChoiceEventPayload struct {
	Type       string                  `json:"type"` // 固定 "user_choice"
	QuestionID string                  `json:"questionId"`
	Question   string                  `json:"question"`
	Options    []interaction.Option    `json:"options"`
	Mode       string                  `json:"mode"`
	InputHint  string                  `json:"inputHint,omitempty"`
	Timeout    int                     `json:"timeout"`
	Via        string                  `json:"via"` // 本次提问来源平台（决定回填来源）
	// BotID / ChatID / ReplyTarget 供 channel 层原生渲染时定位发送目标，
	// web 前端不使用。
	BotID       string `json:"botId,omitempty"`
	ChatID      string `json:"chatId,omitempty"`
	ReplyTarget string `json:"replyTarget,omitempty"`
}

// userChoiceToolDef 创建 user_choice 工具定义。
func userChoiceToolDef() agenttools.ToolDef {
	return agenttools.ToolDef{
		Tool: llm.Tool{
			Name: "user_choice",
			Description: "Present a question with selectable options to the user and BLOCK until they answer. " +
				"Rendered natively per platform (web: interactive card, telegram: inline buttons, misskey: numbered list). " +
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

	// ---- 来源平台：从 CallOrigin / context 推断 ----
	// interaction 包不依赖 api 层，这里用轻量 context key 读取本轮消息元信息
	// （由 LLMStage 注入，见 agent/stages/llmroute.go 的 WithMessageMeta）。
	meta := agenttools.MessageMetaFromContext(ctx.Context)
	via := string(interaction.ViaWeb)
	if meta.ChannelType == "telegram" {
		via = string(interaction.ViaTelegram)
	} else if meta.ChannelType == "misskey" {
		via = string(interaction.ViaMisskey)
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

	// ---- 通知渲染层：progress 事件携带结构化 payload ----
	// web SSE → 前端 ChoiceCard；telegram/misskey 的 outbound 发送路径也可
	// 从事件流拿到该 payload 做原生渲染（见 channel 实现的注入点）。
	if ctx.SendProgress != nil {
		ctx.SendProgress(UserChoiceEventPayload{
			Type:       "user_choice",
			QuestionID: questionID,
			Question:   in.Question,
			Options:    opts,
			Mode:       in.Mode,
			InputHint:  in.InputHint,
			Timeout:    reg.TimeoutSecs,
			Via:        via,
			BotID:      meta.BotID,
			ChatID:     meta.ChatID,
			ReplyTarget: meta.ReplyTarget,
		})
	}

	// ---- 阻塞等待（问题超时 / ctx 取消，谁先到算谁）----
	snap, ans, err := interaction.Default().Wait(ctx.Context, questionID)
	if err != nil {
		if err == interaction.ErrTimeout {
			return map[string]any{
				"status":  "timeout",
				"message": "等待用户应答超时。请基于已有信息给出合理的默认处理，或稍后再问；不要反复重试本工具。",
			}, nil
		}
		return nil, fmt.Errorf("user_choice: 等待中断: %w", err)
	}
	_ = snap

	// ---- 组装返回 ----
	result := map[string]any{
		"status":       "answered",
		"via":          string(ans.Via),
		"custom_input": ans.CustomInput,
	}
	if len(ans.Selected) > 0 {
		selected := make([]int, len(ans.Selected))
		copy(selected, ans.Selected)
		result["selected"] = selected
		// 附带选项文案，省 LLM 一次下标→文案的心算。
		labels := make([]string, 0, len(ans.Selected))
		for _, idx := range ans.Selected {
			if idx >= 0 && idx < len(opts) {
				labels = append(labels, opts[idx].Label)
			}
		}
		result["selected_labels"] = labels
	} else {
		result["selected"] = []int{}
	}
	return result, nil
}
