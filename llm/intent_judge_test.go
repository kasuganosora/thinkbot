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
		ok       bool
	}{
		// 合规 JSON
		{`{"verdict":"YES","reason":"用户明确要求发帖"}`, true, true},
		{`{"verdict":"NO","reason":"只是查询"}`, false, true},
		{`{"verdict":"yes","reason":"小写也接受"}`, true, true},
		{`{"verdict":" NO ","reason":"容忍空白"}`, false, true},
		{`  {"verdict":"YES","reason":"首尾空白"}  `, true, true},
		// markdown 围栏包裹的合规 JSON
		{"```json\n{\"verdict\":\"YES\",\"reason\":\"围栏包裹\"}\n```", true, true},
		{"```\n{\"verdict\":\"NO\",\"reason\":\"无语言标注围栏\"}\n```", false, true},
		// verdict 非法 → 不合规
		{`{"verdict":"MAYBE","reason":"歧义"}`, false, false},
		{`{"verdict":"","reason":"空 verdict"}`, false, false},
		{`{"verdict":123,"reason":"类型错误"}`, false, false},
		// 非 JSON / 破损 JSON → 不合规
		{`YES 用户明确授权`, false, false},
		{"无法回答", false, false},
		{"", false, false},
		{`{"verdict":"YES","reason":`, false, false},
		// JSON 数组/字符串而非对象
		{`["YES"]`, false, false},
		{`"YES"`, false, false},
	}
	for _, c := range cases {
		got, ok := parseIntentJudgeResponse(c.in)
		if ok != c.ok {
			t.Errorf("parse(%q) ok=%v, want %v", c.in, ok, c.ok)
			continue
		}
		if ok && got.grounded != c.grounded {
			t.Errorf("parse(%q) grounded=%v, want %v (reason=%q)", c.in, got.grounded, c.grounded, got.reason)
		}
	}
}

func TestIntentJudge_Grounded(t *testing.T) {
	ctx := context.Background()

	// 合规 YES：口语授权被翻案
	mock := &mockIntentJudgeClient{resps: []string{`{"verdict":"YES","reason":"用户明确要求发帖"}`}}
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
		t.Errorf("tool name missing from judge judge prompt: %q", mock.gotUsr)
	}

	// 合规 NO：只读查询不放行
	mock = &mockIntentJudgeClient{resps: []string{`{"verdict":"NO","reason":"只是查询"}`}}
	j = NewIntentJudge(mock, time.Second)
	v = j.Grounded(ctx, "misskey_create_note", "帮我查下 misskey 上的用户", nil)
	if v.grounded {
		t.Error("judge NO must not ground")
	}

	// 第一次不合规、重试后合规 → 放行
	mock = &mockIntentJudgeClient{resps: []string{
		"YES 直接文本不算",
		`{"verdict":"YES","reason":"重试后输出合规 JSON"}`,
	}}
	j = NewIntentJudge(mock, time.Second)
	v = j.Grounded(ctx, "misskey_create_note", "那你就发呗", nil)
	if !v.grounded {
		t.Errorf("retry with valid JSON should ground, reason=%q", v.reason)
	}
	if mock.calls != 2 {
		t.Errorf("expected 2 LLM calls (1 invalid + 1 valid), got %d", mock.calls)
	}

	// 重试耗尽仍不合规 → fail-closed
	mock = &mockIntentJudgeClient{resps: []string{
		"随心所欲的自由文本",
		`{"verdict":"MAYBE","reason":"verdict 非法"}`,
	}}
	j = NewIntentJudge(mock, time.Second)
	v = j.Grounded(ctx, "misskey_create_note", "那你就发呗", nil)
	if v.grounded {
		t.Error("invalid responses exhausted must fail closed")
	}
	if mock.calls != 2 {
		t.Errorf("expected 2 attempts, got %d", mock.calls)
	}

	// 客户端报错 → fail-closed，不重试
	mock = &mockIntentJudgeClient{err: errors.New("boom")}
	j = NewIntentJudge(mock, time.Second)
	v = j.Grounded(ctx, "misskey_create_note", "发条 misskey", nil)
	if v.grounded {
		t.Error("judge error must fail closed")
	}
	if mock.calls != 1 {
		t.Errorf("client error should not retry, got %d calls", mock.calls)
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
		return `{"verdict":"YES","reason":"慢但合规"}`, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func TestIntentJudge_TimeoutDefault(t *testing.T) {
	// timeout<=0 → 默认 10s
	j := NewIntentJudge(&mockIntentJudgeClient{resps: []string{`{"verdict":"YES"}`}}, 0)
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
