package stages

import (
	"context"

	"github.com/kasuganosora/thinkbot/agent/core"
)

// NoteCaptureMiddleware 在 LLM 生成回复后，将回复文本作为 L0 工作记忆笔记
// （category 默认 "exchange"）自动捕获，供梦境巩固（dreaming）管线后续分析。
//
// 背景：生产 pipeline 使用 LLMStage（而非 ReplyStage）产出 ActionReply，
// 而 ReplyStage 里的自动记笔记分支不会被走到，导致分层记忆库 L0 长期为空、
// dreaming 永远空跑（日志表现为每夜 ingested=0）。本中间件在真实回复路径上
// 补齐这一捕获：LLMStage 把回复写入 ActionReply 后，本中间件据此补一个
// ActionNote，经由已注册的 NoteHandler 落入 TieredStore 的 L0 层。
//
// 仅当存在非空 ActionReply 时才写笔记；不修改任何回复行为，对下游透明。
func NoteCaptureMiddleware(category string) func(next core.Stage) core.Stage {
	if category == "" {
		category = "exchange"
	}
	return func(next core.Stage) core.Stage {
		return &core.StageFunc{
			StageName: "note-capture",
			Fn: func(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
				out, err := next.Process(ctx, env)
				if err != nil {
					return out, err
				}
				if out == nil {
					return out, err
				}
				for _, a := range out.Actions() {
					if a.Type != core.ActionNote && a.Type != core.ActionReply {
						continue
					}
					// 已经显式记过笔记（DecisionReplyWithNote / DecisionNoteOnly）
					// 的，不再重复捕获，避免重复写入。
					if a.Type == core.ActionNote {
						continue
					}
					if a.Payload == "" {
						continue
					}
					out.AddAction(core.Action{
						Type:    core.ActionNote,
						Channel: env.Message.Channel, // 会话空间标识（记忆关联）
						UserID:  env.Message.UserID,
						Payload: a.Payload,
						Metadata: map[string]any{
							"source_channel": env.Message.Source,
							"bot_id":         env.Message.BotID,
							"message_id":     env.Message.ID,
							"category":       category,
						},
					})
				}
				return out, nil
			},
		}
	}
}
