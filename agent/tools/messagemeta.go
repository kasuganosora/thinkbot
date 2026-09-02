package tools

import (
	"context"
)

// ============================================================================
// MessageMeta — 工具执行时可见的「本轮消息元信息」
//
// 与 CallOrigin 的分工：
//   - CallOrigin 回答「来自哪个 bot 的哪个前端会话」（botID + sessionID）；
//   - MessageMeta 回答「本轮消息来自哪个平台/哪个会话空间/回复目标是谁」
//     （channel_type / chat / reply_target）。user_choice 等交互类工具
//     需要它判断应答平台与渲染路径。
//
// 由 LLMStage 在编排前从 Envelope 提取注入（与 ContextWithCallOrigin 同点），
// 工具执行体经 MessageMetaFromContext 读取。
// ============================================================================

// MessageMeta 是注入工具执行 context 的本轮消息元信息快照。
type MessageMeta struct {
	// BotID 所属 bot。
	BotID string
	// ChatID 会话空间标识（telegram: chatID / misskey: channelID / web: sessionID）。
	ChatID string
	// ChannelType 来源平台类型（"web" / "telegram" / "misskey"）。
	ChannelType string
	// ReplyTarget outbound 回写目标（telegram: chatID / misskey: noteID）。
	ReplyTarget string
}

type messageMetaCtxKey struct{}

// ContextWithMessageMeta 把本轮消息元信息注入 context，供工具执行时读取。
func ContextWithMessageMeta(ctx context.Context, meta MessageMeta) context.Context {
	return context.WithValue(ctx, messageMetaCtxKey{}, meta)
}

// MessageMetaFromContext 从 context 取消息元信息（没有则返回零值，容忍非标准调用链）。
func MessageMetaFromContext(ctx context.Context) MessageMeta {
	if v, ok := ctx.Value(messageMetaCtxKey{}).(MessageMeta); ok {
		return v
	}
	return MessageMeta{}
}
