package engagement

import (
	"context"
	"testing"

	"github.com/kasuganosora/thinkbot/agent/core"
)

// declineAlwaysPolicy 总是判定「不该参与」。
type declineAlwaysPolicy struct{}

func (declineAlwaysPolicy) Evaluate(_ context.Context, _ *core.Message) Decision {
	return Decision{Engage: false, Reason: "just chatter", Action: ActionNoAction}
}

// engageAlwaysPolicy 总是判定「应当参与」。
type engageAlwaysPolicy struct{}

func (engageAlwaysPolicy) Evaluate(_ context.Context, _ *core.Message) Decision {
	return Decision{Engage: true, Reason: "directly asked", Action: ActionContinue}
}

// TestEngagementStage_DeclineSetsSuppressReply 是本次修复的核心回归测试。
//
// 修复前的缺陷：判定「不该参与」时只写了 engagement.engage 这个 KV 就 return，
// 而下游 LLMStage 既不读它、也无条件产出 ActionReply —— 于是「不该说话」的判断
// 对实际发送毫无约束力，Bot 的内心独白会被原样投递到群聊。
//
// 修复后：显式设置 core.KVSuppressReply，由 LLMStage 读取并跳过回复产出。
// 注意仍然**不 Abort**：Bot 要继续「听到并思考」，记忆写入等下游 Stage 需要执行。
func TestEngagementStage_DeclineSetsSuppressReply(t *testing.T) {
	stage := newTestStage(declineAlwaysPolicy{})
	env := newEnvelope(core.Message{
		ID: "m1", Text: "群里的闲聊", Source: "misskey-ch",
		Channel: "room-1", UserID: "u1", Mentioned: false,
	})

	out, err := stage.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("process err: %v", err)
	}

	// 必须设置抑制标记
	v, ok := out.Get(core.KVSuppressReply)
	if !ok {
		t.Fatal("declined engagement must set KVSuppressReply")
	}
	if b, _ := v.(bool); !b {
		t.Fatalf("KVSuppressReply should be true, got %v", v)
	}

	// 必须带原因（静默降级要可解释，否则会被误判为 Bot 故障）
	rv, ok := out.Get(core.KVSuppressReplyReason)
	if !ok {
		t.Fatal("suppress reason must be recorded for observability")
	}
	if s, _ := rv.(string); s == "" {
		t.Fatal("suppress reason must not be empty")
	}

	// 关键：不能 Abort —— Bot 仍要处理这条消息（写记忆、更新画像）
	if out.Aborted() {
		t.Error("envelope must NOT be aborted: bot should still listen and remember")
	}
}

// TestEngagementStage_EngageDoesNotSuppress 验证正常参与时不会误设抑制标记
// （避免修复引入「该说话却不说」的反向故障）。
func TestEngagementStage_EngageDoesNotSuppress(t *testing.T) {
	stage := newTestStage(engageAlwaysPolicy{})
	env := newEnvelope(core.Message{
		ID: "m2", Text: "@bot 帮我看看", Source: "misskey-ch",
		Channel: "room-1", UserID: "u1", Mentioned: false,
	})

	out, err := stage.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("process err: %v", err)
	}
	if _, ok := out.Get(core.KVSuppressReply); ok {
		t.Error("engaged message must not be suppressed")
	}
	if !out.Message.Mentioned {
		t.Error("engaged message should be upgraded to Mentioned")
	}
}

// TestEngagementStage_EngageWithNilMetadata 覆盖一个真实panic：
//
// engage 分支会写 env.Message.Metadata["reply_target"]，而 Metadata 是可选字段，
// 并非所有 Channel 都会初始化。**写入 nil map 会 panic**（读取不会），
// 于是任何 Metadata 为 nil 的消息一旦被判定「主动参与」就打挂整条 pipeline。
func TestEngagementStage_EngageWithNilMetadata(t *testing.T) {
	stage := newTestStage(engageAlwaysPolicy{})
	env := newEnvelope(core.Message{
		ID: "m5", Text: "hi", Source: "ch", Channel: "room-1", UserID: "u1",
		Metadata: nil, // 关键：显式为 nil
	})

	out, err := stage.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("process err: %v", err)
	}
	if got := out.Message.Metadata["reply_target"]; got != "m5" {
		t.Errorf("reply_target = %v, want m5", got)
	}
}
