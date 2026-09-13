package outreach

import (
	"context"
	"fmt"
	"strings"

	"github.com/kasuganosora/thinkbot/dao"
)

type outreachSessionCtxKey struct{}

// WithOutreachSession 标记当前 ctx 为人主动开口合成轮（反套娃）。
func WithOutreachSession(ctx context.Context) context.Context {
	return context.WithValue(ctx, outreachSessionCtxKey{}, true)
}

// IsOutreachSession 判断 ctx 是否来自 outreach 触发的一轮。
// fail-closed：ctx 异常时视为是，阻止嵌套创建。
func IsOutreachSession(ctx context.Context) (is bool) {
	defer func() {
		if recover() != nil {
			is = true
		}
	}()
	if ctx == nil {
		return true
	}
	v, ok := ctx.Value(outreachSessionCtxKey{}).(bool)
	return ok && v
}

func buildOutreachPrompt(c dao.OutreachCommitment) string {
	var b strings.Builder
	b.WriteString("This is a system-triggered proactive message, not a new user message.\n")
	if isHard(c.Kind) {
		b.WriteString("Reason: a reminder the user asked you to set is now due.\n")
	} else {
		b.WriteString("Reason: a watch the user asked you to keep is now due.\n")
	}
	fmt.Fprintf(&b, "Topic: %s\n", strings.TrimSpace(c.Topic))
	if ctx := strings.TrimSpace(c.Context); ctx != "" && ctx != c.Topic {
		fmt.Fprintf(&b, "User's original words: %s\n", ctx)
	}
	b.WriteString("Due at (UTC): ")
	b.WriteString(c.DueAt.UTC().Format(timeRFC3339))
	b.WriteString("\n\n")
	b.WriteString("You MUST speak to the user. Do not debate whether to send. Do not stay silent. Do not emit @@REPLY_CONTROL@@{\"send\":false}.\n")
	b.WriteString("You MAY call tools to get the wording right (weather, memory search, a quick look at related material). Tools are for wording, not for another vote on whether to send.\n")
	b.WriteString("Use your usual voice, in the user's language, short and natural. Output only the message body to send.\n")
	return b.String()
}

const timeRFC3339 = "2006-01-02T15:04:05Z07:00"
