package llm

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestParseIntentJudgeResponse(t *testing.T) {
	cases := []struct {
		in       string
		grounded bool
	}{
		{"YES 用户明确授权", true},
		{"yes 口语授权", true},
		{"  YES  ", true},
		{"NO 只是查询", false},
		{"no 仅提到平台名", false},
		{"NO", false},
		{"无法回答", false},   // 无法解析 → fail-closed
		{"", false},        // 空 → fail-closed
		{"可能吧", false},     // 歧义 → fail-closed
	}
	for _, c := range cases {
		got := parseIntentJudgeResponse(c.in)
		if got.grounded != c.grounded {
			t.Errorf("parse(%q) grounded=%v, want %v (reason=%q)", c.in, got.grounded, c.grounded, got.reason)
		}
	}
}

func TestIntentJudge_Grounded(t *testing.T) {
	ctx := context.Background()

	// YES：口语授权被翻案
	mock := &mockIntentJudgeClient{resp: "YES 用户明确要求发帖"}
	j := NewIntentJudge(mock, time.Second)
	v := j.Grounded(ctx, "misskey_create_note", "发条 misskey，内容你定", []string{"那你尝试下发一个验证下"})
	if !v.grounded {
		t.Errorf("expected grounded, got reason=%q", v.reason)
	}
	// 近期消息必须进入 prompt
	if !strings.Contains(mock.gotUsr, "那你尝试下发一个验证下") {
		t.Errorf("recent msgs missing from judge prompt: %q", mock.gotUsr)
	}
	if !strings.Contains(mock.gotUsr, "misskey_create_note") {
		t.Errorf("tool name missing from judge prompt: %q", mock.gotUsr)
	}

	// NO：只读查询不放行
	mock = &mockIntentJudgeClient{resp: "NO 只是查询"}
	j = NewIntentJudge(mock, time.Second)
	v = j.Grounded(ctx, "misskey_create_note", "帮我查下 misskey 上的用户", nil)
	if v.grounded {
		t.Error("judge NO must not ground")
	}

	// 客户端报错 → fail-closed
	mock = &mockIntentJudgeClient{err: errors.New("boom")}
	j = NewIntentJudge(mock, time.Second)
	v = j.Grounded(ctx, "misskey_create_note", "发条 misskey", nil)
	if v.grounded {
		t.Error("judge error must fail closed")
	}

	// 超时 → fail-closed
	slow := &slowIntentJudgeClient{delay: 200 * time.Millisecond}
	j = NewIntentJudge(slow, 30*time.Millisecond)
	v = j.Grounded(ctx, "misskey_create_note", "发条 misskey", nil)
	if v.grounded {
		t.Error("judge timeout must fail closed")
	}
}

type slowIntentJudgeClient struct{ delay time.Duration }

func (s *slowIntentJudgeClient) Chat(ctx context.Context, system, user string) (string, error) {
	select {
	case <-time.After(s.delay):
		return "YES", nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func TestIntentJudge_TimeoutDefault(t *testing.T) {
	// timeout<=0 → 默认 10s
	j := NewIntentJudge(&mockIntentJudgeClient{resp: "YES"}, 0)
	if j.timeout != 10*time.Second {
		t.Errorf("expected default 10s timeout, got %v", j.timeout)
	}
}

func TestProviderIntentJudgeAdapter(t *testing.T) {
	// 适配器非 nil 即可满足接口；真实 provider 调用由集成环境覆盖。
	var client IntentJudgeClient = NewProviderIntentJudge(nil, nil)
	if client == nil {
		t.Error("adapter must be non-nil")
	}
}
