package interaction

import (
	"errors"
	"testing"
	"time"
)

func secretQuestion(id, name string) Question {
	return Question{
		ID:          id,
		BotID:       "bot-1",
		ChatID:      "chat-1",
		Question:    "请输入用于示例服务的令牌",
		Kind:        KindSecret,
		SecretName:  name,
		TimeoutSecs: 300,
	}
}

func TestConsumeSecret_HitAndOnceOnly(t *testing.T) {
	r := NewRegistry()
	if _, err := r.RegisterQuestion(secretQuestion("si-1", "svc.token")); err != nil {
		t.Fatalf("RegisterQuestion(secret) rejected: %v", err)
	}

	name, ok := r.ConsumeSecret("bot-1", "chat-1", "abcdefghijklmnop")
	if !ok || name != "svc.token" {
		t.Fatalf("ConsumeSecret = (%q,%v), want (svc.token,true)", name, ok)
	}
	// 一次性：会话索引已移除，第二次不再命中。
	if _, ok := r.ConsumeSecret("bot-1", "chat-1", "another-value-123"); ok {
		t.Fatal("secret request consumed twice")
	}
}

func TestConsumeSecret_SessionScoped(t *testing.T) {
	r := NewRegistry()
	if _, err := r.RegisterQuestion(secretQuestion("si-1", "svc.token")); err != nil {
		t.Fatal(err)
	}
	// 别的会话不能消费。
	if _, ok := r.ConsumeSecret("bot-1", "chat-OTHER", "abcdefghijklmnop"); ok {
		t.Fatal("cross-session secret consumption should not happen")
	}
	if _, ok := r.ConsumeSecret("bot-1", "chat-1", "abcdefghijklmnop"); !ok {
		t.Fatal("own session should still consume")
	}
}

func TestConsumeSecret_IgnoresChoiceQuestions(t *testing.T) {
	r := NewRegistry()
	if _, err := r.RegisterQuestion(validQuestion("uc-1")); err != nil {
		t.Fatal(err)
	}
	// user_choice 的 pending 问题不能被当作 secret 消费。
	if _, ok := r.ConsumeSecret("bot-1", "chat-1", "some-reply-value"); ok {
		t.Fatal("choice question should not be consumed as secret")
	}
}

func TestConsumeSecret_ExpiredSkipped(t *testing.T) {
	r := NewRegistry()
	q := secretQuestion("si-1", "svc.token")
	q.TimeoutSecs = 1
	if _, err := r.RegisterQuestion(q); err != nil {
		t.Fatal(err)
	}
	// 把注册时刻回拨到超时之前。
	r.mu.Lock()
	if e, ok := r.entries["si-1"]; ok {
		e.question.RegisteredAt = time.Now().Add(-5 * time.Second).Unix()
	}
	r.mu.Unlock()
	if _, ok := r.ConsumeSecret("bot-1", "chat-1", "abcdefghijklmnop"); ok {
		t.Fatal("expired secret request should not consume")
	}
}

func TestRegisterQuestion_ChoiceStillValidatesOptions(t *testing.T) {
	r := NewRegistry()
	q := validQuestion("uc-1")
	q.Options = nil
	if _, err := r.RegisterQuestion(q); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("choice question with no options: got %v, want ErrInvalidOptions", err)
	}
}

func TestRegisterQuestion_SecretNeedsName(t *testing.T) {
	r := NewRegistry()
	q := secretQuestion("si-1", "")
	if _, err := r.RegisterQuestion(q); !errors.Is(err, ErrInvalidQuestion) {
		t.Fatalf("secret question without name: got %v, want ErrInvalidQuestion", err)
	}
}

func TestSecretRequestsAreUnambiguousAndCancellable(t *testing.T) {
	r := NewRegistry()
	if _, err := r.RegisterQuestion(secretQuestion("first", "one")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.RegisterQuestion(secretQuestion("second", "two")); !errors.Is(err, ErrPendingSecret) {
		t.Fatalf("second request accepted: %v", err)
	}
	if _, ok := r.CancelSecret("bot-1", "other-chat"); ok {
		t.Fatal("cross-chat cancel")
	}
	if name, ok := r.CancelSecret("bot-1", "chat-1"); !ok || name != "one" {
		t.Fatal("cancel failed")
	}
	if _, err := r.RegisterQuestion(secretQuestion("second", "two")); err != nil {
		t.Fatal(err)
	}
	if name, _, ok := r.TakeSecretReply("bot-1", "chat-1", "fictional-credential"); !ok || name != "two" {
		t.Fatal("reply matched wrong request")
	}
}

func TestLateSecretReplyIsClaimedForDisposal(t *testing.T) {
	r := NewRegistry()
	q := secretQuestion("expired", "one")
	q.TimeoutSecs = 1
	if _, err := r.RegisterQuestion(q); err != nil {
		t.Fatal(err)
	}
	r.entries[q.ID].question.RegisteredAt = time.Now().Add(-time.Minute).Unix()
	if name, expired, ok := r.TakeSecretReply("bot-1", "chat-1", "fictional-late-credential"); !ok || !expired || name != "one" {
		t.Fatal("late reply would pass through as chat")
	}
	if _, _, ok := r.TakeSecretReply("bot-1", "chat-1", "normal chat"); ok {
		t.Fatal("expired request consumed twice")
	}
}
