package inbound

import (
	"fmt"
	"strings"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/internal/interaction"
	"github.com/kasuganosora/thinkbot/secret"
)

// ============================================================================
// 密钥回复拦截
//
// secret_input 工具登记 pending 请求后，用户的下一条回复会被当成密钥：明文写进
// 保险库，消息正文改写为脱敏提示再进 pipeline。所有「用户文本可能进入 LLM /
// 日志 / 聊天历史」的入口都必须**先**过这里：
//   - Ingress.Receive / TryReceive（所有渠道的常规入站）；
//   - 企微在入站日志、引用合并、append 注入**之前**（turn.go 的 append 不经 Ingress）；
//   - Web /api/chat/send 与 /api/chat/append 在 SaveMessage **之前**（先落库再 Inject）。
// 拦截是幂等的：命中后 pending 请求即被消费，后续环节再调用不会二次命中。
// ============================================================================

// InterceptSecretReply 若 msg 是对本会话 pending 密钥请求的回复，则把用户原文
// 登记进保险库（归属 msg.BotID），并把 msg.Text 改写为脱敏提示。
//
// 返回 (secretName, intercepted)。intercepted=false 时 msg 原样不动：
//   - 本会话没有 pending 密钥请求；
//   - 回复为空，请求继续保持 pending。
//
// 非空回复一律按密钥处理：自然语言、Unicode 和问号也可能是密码。
func InterceptSecretReply(msg *core.Message) (string, bool) {
	if msg == nil {
		return "", false
	}
	candidate := secretCandidate(msg)
	// 原文副本只供本函数使用，任何路径都不随 Metadata 流进 pipeline。
	if msg.Metadata != nil {
		delete(msg.Metadata, core.MetaSecretCandidate)
	}
	if msg.BotID == "" {
		return "", false
	}
	reg := interaction.Default()
	if candidate == "/cancel-secret" {
		if name, ok := reg.CancelSecret(msg.BotID, msg.Channel); ok {
			msg.Text = "[用户已取消密钥提供请求，未登记任何值。请继续普通对话；未经用户重新要求，不要再次索要该密钥。]"
			return name, true
		}
	}
	if candidate == "" {
		return "", false
	}
	name, expired, ok := reg.TakeSecretReply(msg.BotID, msg.Channel, candidate)
	if !ok {
		return "", false
	}
	if expired {
		msg.Text = "[密钥请求已过期，迟到的回复已按密钥处理并隐藏，未登记。请告知用户本次未保存；如仍需提供，请重新发起密钥请求。]"
		return name, true
	}
	err := secret.Default().Register(msg.BotID, name, candidate, "由用户在会话中直接提供")
	switch {
	case err == nil:
		msg.Text = fmt.Sprintf("[用户已提供密钥 %s。此后在工具参数中写占位符 %s 即可引用，系统会在执行前自动还原为真实值，你看不到也不应索要明文。请继续此前因缺少该密钥而暂停的任务。]", name, secret.Placeholder(name))
	case secret.IsTooShort(err):
		// 过短的回复不像密钥，但它也可能是用户的简短答复。原文不能进 LLM（它
		// 可能就是一个短口令），只告诉模型发生了什么，由模型决定是否重新发起请求。
		msg.Text = fmt.Sprintf("[用户对密钥 %s 的回复过短（少于 %d 个字符），未登记；原文已按密钥处理、不展示给你。如仍需要该密钥，请重新调用 secret_input 并请用户再发一次。]", name, secret.MinValueLen)
	default:
		msg.Text = fmt.Sprintf("[密钥 %s 登记失败（保险库不可用）；原文已按密钥处理、不展示给你。请告知用户稍后重试，暂不要使用该占位符。]", name)
	}
	return name, true
}

// secretCandidate 取用户亲手输入的原文：渠道装饰过正文时用 MetaSecretCandidate，
// 否则就是 msg.Text。
func secretCandidate(msg *core.Message) string {
	if msg.Metadata != nil {
		if raw, ok := msg.Metadata[core.MetaSecretCandidate].(string); ok && strings.TrimSpace(raw) != "" {
			return strings.TrimSpace(raw)
		}
	}
	return strings.TrimSpace(msg.Text)
}
