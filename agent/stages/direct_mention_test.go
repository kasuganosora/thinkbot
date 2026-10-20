package stages

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/llm"
)

// seqProvider returns its scripted outputs in order (last one repeats).
type seqProvider struct {
	outs   []string
	called int
	last   llm.GenerateParams
}

func (p *seqProvider) Name() string { return "seq" }
func (p *seqProvider) DoGenerate(_ context.Context, params llm.GenerateParams) (*llm.GenerateResult, error) {
	i := p.called
	if i >= len(p.outs) {
		i = len(p.outs) - 1
	}
	p.called++
	p.last = params
	return &llm.GenerateResult{Text: p.outs[i], FinishReason: llm.FinishReasonStop}, nil
}
func (p *seqProvider) DoStream(context.Context, llm.GenerateParams) (*llm.StreamResult, error) {
	return nil, errors.New("no stream")
}

func newSeqStage(outs ...string) (*LLMStage, *seqProvider) {
	p := &seqProvider{outs: outs}
	return NewLLMStage("llm", p, LLMConfig{
		RequireReplyControl: true,
		MessageBuilder: func(msg core.Message) []llm.Message {
			return []llm.Message{llm.UserMessage(msg.Text)}
		},
	}, nil, nil), p
}

// misskeyMentionEnv mirrors what the Misskey channel emits for a mention/reply
// event: Mentioned=true, ChatType=private, visibility of the original note.
func misskeyMentionEnv(explicit bool, vis string, fromBot bool) *core.Envelope {
	return core.NewEnvelope(core.Message{
		ID: "note-1", Text: "hey, what do you think?", Source: "misskey",
		Channel: "misskey:u1", UserID: "u1", ChatType: core.ChatPrivate,
		Mentioned: true, FromIsBot: fromBot,
		Metadata: map[string]any{
			"channel_type":           "misskey",
			"visibility":             vis,
			core.MetaExplicitMention: explicit,
		},
	})
}

const declinedInternalOnly = "<internal>it is someone else's thread, stay quiet</internal>\n@@REPLY_CONTROL@@{\"send\": false}"

func TestMisskeyExplicitMention_SendFalseForcesReply(t *testing.T) {
	stage, p := newSeqStage(declinedInternalOnly, "<public>Hi! Happy to help.</public>\n@@REPLY_CONTROL@@{\"send\": true}")
	out, err := stage.Process(context.Background(), misskeyMentionEnv(true, "public", false))
	if err != nil {
		t.Fatal(err)
	}
	r := replyActions(out)
	if len(r) != 1 || r[0].Payload.(string) != "Hi! Happy to help." {
		t.Fatalf("explicit mention must force a (regenerated) reply, got %v", r)
	}
	if p.called != 2 || !strings.Contains(p.last.System, "DIRECT MENTION") || len(p.last.Tools) != 0 {
		t.Fatalf("expected one tool-less re-ask, called=%d", p.called)
	}
}

func TestMisskeyExplicitMention_SendFalseWithTextSendsIt(t *testing.T) {
	stage, p := newSeqStage("<public>Sure thing.</public>\n@@REPLY_CONTROL@@{\"send\": false}")
	out, _ := stage.Process(context.Background(), misskeyMentionEnv(true, "home", false))
	if r := replyActions(out); len(r) != 1 || r[0].Payload.(string) != "Sure thing." || p.called != 1 {
		t.Fatalf("got %v called=%d", r, p.called)
	}
}

func TestMisskeyExplicitMention_MissingControlForcesReply(t *testing.T) {
	stage, _ := newSeqStage("<internal>hmm</internal>", "<public>Hello there.</public>\n@@REPLY_CONTROL@@{\"send\": true}")
	out, _ := stage.Process(context.Background(), misskeyMentionEnv(true, "public", false))
	if r := replyActions(out); len(r) != 1 || r[0].Payload.(string) != "Hello there." {
		t.Fatalf("got %v", r)
	}
}

func TestMisskeyInheritedMention_SendFalseRespected(t *testing.T) {
	stage, p := newSeqStage("<public>Sure thing.</public>\n@@REPLY_CONTROL@@{\"send\": false}")
	out, _ := stage.Process(context.Background(), misskeyMentionEnv(false, "public", false))
	if r := replyActions(out); len(r) != 0 || p.called != 1 {
		t.Fatalf("inherited-only mention must not force a reply, got %v called=%d", r, p.called)
	}
}

func TestMisskeyInheritedMention_DirectMessageStillFailOpen(t *testing.T) {
	stage, _ := newSeqStage("<public>Sure thing.</public>\n@@REPLY_CONTROL@@{\"send\": false}")
	out, _ := stage.Process(context.Background(), misskeyMentionEnv(false, "specified", false))
	if r := replyActions(out); len(r) != 1 {
		t.Fatalf("specified-visibility DM keeps private fail-open, got %v", r)
	}
}

func TestMisskeyExplicitMention_FromBotNotForced(t *testing.T) {
	if isExplicitlyAddressed(misskeyMentionEnv(true, "public", true)) {
		t.Fatal("bot accounts must never force a reply (anti-loop)")
	}
}

func TestIsExplicitlyAddressed_Telegram(t *testing.T) {
	env := core.NewEnvelope(core.Message{ChatType: core.ChatGroup, Mentioned: true,
		Metadata: map[string]any{"channel_type": "telegram"}})
	if isExplicitlyAddressed(env) {
		t.Fatal("telegram behaviour unchanged: Mentioned alone does not force")
	}
}

func TestChatContextLabel(t *testing.T) {
	cases := map[string]string{
		"public": "public post", "home": "home-timeline post",
		"followers": "followers-only post", "specified": "direct message",
	}
	for vis, want := range cases {
		if got := chatContextLabel(misskeyMentionEnv(true, vis, false)); got != want {
			t.Errorf("%s: got %q want %q", vis, got, want)
		}
	}
	if got := chatContextLabel(core.NewEnvelope(core.Message{ChatType: core.ChatPrivate})); got != "private chat" {
		t.Errorf("plain private: %q", got)
	}
	if got := chatContextLabel(core.NewEnvelope(core.Message{ChatType: core.ChatGroup})); got != "group chat" {
		t.Errorf("group: %q", got)
	}
}

func TestMisskeyExplicitMention_BotAuthorUsesModelDecision(t *testing.T) {
	stage, p := newSeqStage("<public>Sure thing.</public>\n@@REPLY_CONTROL@@{\"send\": false}")
	env := misskeyMentionEnv(true, "public", true)
	out, _ := stage.Process(context.Background(), env)
	if r := replyActions(out); len(r) != 0 || p.called != 1 {
		t.Fatalf("bot author must not be force-replied, got %v called=%d", r, p.called)
	}
	stage2, _ := newSeqStage("<public>Hi bot.</public>\n@@REPLY_CONTROL@@{\"send\": true}")
	out, _ = stage2.Process(context.Background(), misskeyMentionEnv(true, "public", true))
	if r := replyActions(out); len(r) != 1 {
		t.Fatalf("model may still reply to a bot via normal path, got %v", r)
	}
}

func TestMisskeyExplicitMention_PerThreadForcedCap(t *testing.T) {
	p := &seqProvider{outs: []string{"<public>ok</public>\n@@REPLY_CONTROL@@{\"send\": false}"}}
	stage := NewLLMStage("llm", p, LLMConfig{
		RequireReplyControl:     true,
		ForcedReplyMaxPerThread: 2,
		ForcedReplyWindow:       time.Minute,
		MessageBuilder: func(msg core.Message) []llm.Message {
			return []llm.Message{llm.UserMessage(msg.Text)}
		},
	}, nil, nil)
	now := time.Unix(1_000_000, 0)
	stage.forcedReplies.now = func() time.Time { return now }
	send := func(thread string) int {
		env := misskeyMentionEnv(true, "public", false)
		env.Message.Metadata["reply_id"] = thread
		out, _ := stage.Process(context.Background(), env)
		return len(replyActions(out))
	}
	if send("t1") != 1 || send("t1") != 1 {
		t.Fatal("first two forced replies in thread should go out")
	}
	if send("t1") != 0 {
		t.Fatal("third within window must fall back to model's send:false")
	}
	if send("t2") != 1 {
		t.Fatal("cap is per thread")
	}
	now = now.Add(2 * time.Minute)
	if send("t1") != 1 {
		t.Fatal("window expiry restores forcing")
	}
}
