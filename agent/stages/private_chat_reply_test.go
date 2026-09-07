package stages

import (
	"context"
	"testing"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/llm"
)

// newRCStage 构造开启回复控制门控（RequireReplyControl=true）的 LLMStage，
// 用传入的固定文本作为模型输出，便于确定性验证出站逻辑（不调用真实 LLM）。
func newRCStage(text string) *LLMStage {
	p := &suppressStubProvider{text: text}
	return NewLLMStage("llm", p, LLMConfig{
		RequireReplyControl: true,
		MessageBuilder: func(msg core.Message) []llm.Message {
			return []llm.Message{llm.UserMessage(msg.Text)}
		},
	}, nil, nil)
}

// TestIsPrivateChat 锁定单对单私聊判定（含 supergroup 归一化与系统源排除）。
func TestIsPrivateChat(t *testing.T) {
	cases := []struct {
		name    string
		chat    string
		source  string
		want    bool
	}{
		{"private", core.ChatPrivate, "", true},
		{"group", core.ChatGroup, "", false},
		{"supergroup normalizes to group -> not private", core.ChatSupergroup, "", false},
		{"empty chat type", "", "", false},
		{"heartbeat source even if private -> excluded", core.ChatPrivate, core.SourceHeartbeat, false},
		{"cron source even if private -> excluded", core.ChatPrivate, core.SourceCron, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := core.NewEnvelope(core.Message{ChatType: c.chat, Source: c.source})
			if got := isPrivateChat(env); got != c.want {
				t.Fatalf("isPrivateChat: got %v want %v (chat=%q source=%q)", got, c.want, c.chat, c.source)
			}
		})
	}
}

// TestLLMStage_PrivateChat_SendFalseReplies 锁定核心不变量：
// 单对单私聊里模型误判 send:false（如把正常提问当成「日常吐槽」），
// reply-control 必须 fail-open 把可发内容发出去，而非一言不发。
func TestLLMStage_PrivateChat_SendFalseReplies(t *testing.T) {
	stage := newRCStage("收到，这就去查一下缓存配置。\n@@REPLY_CONTROL@@{\"send\": false}")
	env := core.NewEnvelope(core.Message{
		ID: "priv-send-false", Text: "帮我看下 Go 里怎么控制超时", Source: "telegram",
		Channel: "telegram:123", UserID: "luna", ChatType: core.ChatPrivate,
	})

	out, err := stage.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	replies := replyActions(out)
	if len(replies) != 1 {
		t.Fatalf("private chat with send:false must still reply, got %d actions", len(replies))
	}
	if got := replies[0].Payload.(string); got != "收到，这就去查一下缓存配置。" {
		t.Fatalf("unexpected private reply payload: %q", got)
	}
}

// TestLLMStage_PrivateChat_SoftGateBypassed 锁定：
// 私聊里上游软门（engagement/rhythm 节流）不抑制，bot 仍应回复。
func TestLLMStage_PrivateChat_SoftGateBypassed(t *testing.T) {
	stage := newRCStage("好的，我来处理。\n@@REPLY_CONTROL@@{\"send\": false}")
	env := core.NewEnvelope(core.Message{
		ID: "priv-soft-gate", Text: "把这个 bug 修一下", Source: "telegram",
		Channel: "telegram:123", UserID: "luna", ChatType: core.ChatPrivate,
	})
	// 上游软门：engagement 判定「此刻不该说话」。
	env.Set(core.KVSuppressReply, true)
	env.Set(core.KVSuppressReplyReason, "engagement_declined")

	out, err := stage.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if got := replyActions(out); len(got) != 1 {
		t.Fatalf("private chat must bypass soft gate and reply, got %d actions", len(got))
	}
	// 私聊放行时不应误标「抑制但捕获记忆」（那是「想了不说」的语义，私聊是要说的）。
	if _, ok := out.Get(core.KVCaptureSuppressedExchange); ok {
		t.Fatalf("private chat reply must not set KVCaptureSuppressedExchange")
	}
}

// TestLLMStage_PrivateChat_InternalOnlyStaysSilent 锁定边界：
// 私聊里模型只写了 <internal> 私密心话、无任何可发公开内容（即使 send:false），
// 此时「无可发内容」而非「拒绝回复」，仍不发（避免泄露私密或发空消息）。
func TestLLMStage_PrivateChat_InternalOnlyStaysSilent(t *testing.T) {
	stage := newRCStage("<internal>这人好烦，不想理</internal>\n@@REPLY_CONTROL@@{\"send\": false}")
	env := core.NewEnvelope(core.Message{
		ID: "priv-internal-only", Text: "hi", Source: "telegram",
		Channel: "telegram:123", UserID: "luna", ChatType: core.ChatPrivate,
	})

	out, err := stage.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if got := replyActions(out); len(got) != 0 {
		t.Fatalf("private chat with internal-only output must stay silent, got %d actions: %+v", len(got), got)
	}
}

// TestLLMStage_Group_SendFalseStillSilent 回归：非私聊（群聊）的 send:false 必须仍静默，
// 私聊 fail-open 不影响公共场景（防把内心独白发到时间线）。
func TestLLMStage_Group_SendFalseStillSilent(t *testing.T) {
	stage := newRCStage("这条内容不适合扩散，保持沉默。\n@@REPLY_CONTROL@@{\"send\": false}")
	env := core.NewEnvelope(core.Message{
		ID: "group-send-false", Text: "水帖", Source: "misskey",
		Channel: "misskey:timeline", UserID: "someone", ChatType: core.ChatGroup,
	})

	out, err := stage.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if got := replyActions(out); len(got) != 0 {
		t.Fatalf("group send:false must stay silent, got %d actions: %+v", len(got), got)
	}
}

// TestLLMStage_PrivateChat_HardGateStillSuppresses 回归：
// 私聊里的硬门（如纯 Renote 物理不可回复）仍须抑制，fail-open 只放行软门。
func TestLLMStage_PrivateChat_HardGateStillSuppresses(t *testing.T) {
	stage := newRCStage("收到。\n@@REPLY_CONTROL@@{\"send\": true}")
	env := core.NewEnvelope(core.Message{
		ID: "priv-hard-gate", Text: "renote", Source: "misskey",
		Channel: "misskey:renote", UserID: "someone", ChatType: core.ChatPrivate,
	})
	// 纯 Renote 硬门（物理不可回复，Misskey 直接 400 拒绝）。
	env.Set(core.KVSuppressReply, true)
	env.Set(core.KVSuppressReplyReason, core.KVSuppressReasonPureRenote)

	out, err := stage.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if got := replyActions(out); len(got) != 0 {
		t.Fatalf("private chat with hard gate must stay silent, got %d actions: %+v", len(got), got)
	}
}
