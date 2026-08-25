package stages

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/kasuganosora/thinkbot/agent/core"
)

// exchangeSeen 按 message_id 对「同一条入站消息」做跨 Process 去重。
//
// 背景（2026-08-25 排查实证）：同一条 Misskey 帖可能经 mention 流 + timeline 流 +
// 重连重放被多次 ingest，每次 pipeline 都会写一条 exchange 记忆（memory_entries）；
// 渠道层 dedupSeen 因去重状态未跨连接/流持久，未能并掉这类重复。此处用闭包级
// （按 bot 持久，中间件每个 bot 只创建一次）seen 集兜底，确保同 message_id 只落
// 一条 exchange 记忆 + 一条事件流记录。
//
// TTL 仅为防止 map 无限增长，并允许极久之后同一消息被重新捕获（正常不会发生）。
type exchangeSeen struct {
	mu     sync.Mutex
	seenAt map[string]time.Time
	ttl    time.Duration
}

func newExchangeSeen(ttl time.Duration) *exchangeSeen {
	return &exchangeSeen{seenAt: make(map[string]time.Time), ttl: ttl}
}

// seen 报告 id 近期是否出现过，并将 id 标记为已见。返回 true 表示「已见过，应跳过」。
func (e *exchangeSeen) seen(id string) bool {
	if id == "" {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	now := time.Now()
	if ts, ok := e.seenAt[id]; ok && now.Sub(ts) < e.ttl {
		return true
	}
	if len(e.seenAt) > 8192 {
		for k, ts := range e.seenAt {
			if now.Sub(ts) >= e.ttl {
				delete(e.seenAt, k)
			}
		}
	}
	e.seenAt[id] = now
	return false
}

const exchangeCaptureDedupTTL = 10 * time.Minute

// CapturedUserMessage 是一条已摄取的入站用户消息（写入事件流的最小单元）。
// 与 dao.UserMessageEvent 解耦，避免 agent/stages 直接依赖存储层。
type CapturedUserMessage struct {
	BotID     string
	Channel   string
	UserID    string
	MessageID string
	Content   string
}

// UserMessageEventWriter 将摄取到的用户入站消息写入持久化事件流
// （user_message_events 表），供 dreaming 回灌（backfill）作为权威数据源消费。
// 由 Bot 装配时注入；nil 表示不写入（仅依赖实时 NoteCapture 落 L0）。
type UserMessageEventWriter interface {
	WriteUserMessageEvent(ctx context.Context, msg CapturedUserMessage) error
}

// NoteCaptureMiddleware 在 LLM 生成回复后，将回复文本作为 L0 工作记忆笔记
// （category 默认 "exchange"）自动捕获，供梦境巩固（dreaming）管线后续分析。
//
// 同时，在捕获「用户说了什么」时并行写入持久化事件流（writer 非 nil 时），
// 使 dreaming 回灌能从事件流消费、而非扫描原始 chat_messages（根治回灌陷阱）。
//
// 背景：生产 pipeline 使用 LLMStage（而非 ReplyStage）产出 ActionReply，
// 而 ReplyStage 里的自动记笔记分支不会被走到，导致分层记忆库 L0 长期为空、
// dreaming 永远空跑（日志表现为每夜 ingested=0）。本中间件在真实回复路径上
// 补齐这一捕获：LLMStage 把回复写入 ActionReply 后，本中间件据此补一个
// ActionNote，经由已注册的 NoteHandler 落入 TieredStore 的 L0 层。
//
// 仅当存在非空 ActionReply 时才写笔记；不修改任何回复行为，对下游透明。
func NoteCaptureMiddleware(category string, writer UserMessageEventWriter) func(next core.Stage) core.Stage {
	if category == "" {
		category = "exchange"
	}
	// 按 bot 持久的跨 Process 去重集（中间件每个 bot 仅创建一次，故 seen 集跨重连/重放持久）。
	seen := newExchangeSeen(exchangeCaptureDedupTTL)
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
				// 心跳自主唤醒的消息不写入 L0 记忆：其 InjectContext 不是用户原文，
				// 且「心跳唤醒」本身不应成为长期记忆的一部分（避免污染 dreaming 学习）。
				if out.Message.Source == core.SourceHeartbeat {
					return out, nil
				}
				// 跨 Process 去重：同一条入站消息（按 message_id）只捕获一次。
				// 见 exchangeSeen 注释——渠道层 dedupSeen 未跨连接/流持久，此处兜底，
				// 避免同帖被 mention/timeline/重连多次 ingest 时写多条 exchange 记忆。
				if seen.seen(env.Message.ID) {
					return out, nil
				}
			// 捕获「用户说了什么」作为 L0 对话记忆与事件流记录，供 dreaming 学习
			// 用户偏好/事实。注意：不要捕获 bot 自己的回复（env.Message.Text 才是用户
			// 入站原文）——否则 dreaming 会把 bot 的发言误当成用户的事实（说话人归属错误，
			// 见历史 bug：把 bot 对《零之使魔》的安利错记成「用户熟悉该作」）。
			// 一条用户消息只捕获一次：若一轮产出多个 ActionReply（如主回复 + ChannelPoster
			// 转发），原先会在循环里对每个回复各写一份，导致 L0 笔记与事件流记录重复。
			userText := strings.TrimSpace(env.Message.Text)
			if userText != "" {
				// 快照 actions，避免遍历过程中 AddAction 改变切片长度引发意外。
				actions := make([]core.Action, len(out.Actions()))
				copy(actions, out.Actions())
				captured := false
				for _, a := range actions {
					if a.Type != core.ActionNote && a.Type != core.ActionReply {
						continue
					}
					// 已经显式记过笔记（DecisionReplyWithNote / DecisionNoteOnly）的，
					// 视为已捕获，不再重复写入用户发言。
					if a.Type == core.ActionNote {
						captured = true
						continue
					}
					if a.Payload == "" {
						continue
					}
					if captured {
						// 本条消息已捕获过用户发言，跳过后续回复动作，避免重复。
						continue
					}
					out.AddAction(core.Action{
						Type:    core.ActionNote,
						Channel: env.Message.Channel, // 会话空间标识（记忆关联）
						UserID:  env.Message.UserID,
						Payload: userText,
						Metadata: map[string]any{
							"source_channel": env.Message.Source,
							"bot_id":         env.Message.BotID,
							"message_id":     env.Message.ID,
							"category":       category,
							"speaker":        "user",
						},
					})
					// 并行写入事件流（best-effort：writer 内部自行记日志，这里忽略错误，
					// 因为 backfill 有 chat_messages 一次性 seed 作为兜底）。
					if writer != nil {
						_ = writer.WriteUserMessageEvent(ctx, CapturedUserMessage{
							BotID:     env.Message.BotID,
							Channel:   env.Message.Channel,
							UserID:    env.Message.UserID,
							MessageID: env.Message.ID,
							Content:   userText,
						})
					}
					captured = true
				}
			}
				return out, nil
			},
		}
	}
}
