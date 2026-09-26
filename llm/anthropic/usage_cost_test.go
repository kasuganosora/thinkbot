package anthropic

import (
	"testing"

	"github.com/kasuganosora/thinkbot/llm"
)

// TestAnthropicUsage_NormalizedToInclusive 锁定 Anthropic 用量口径的归一化：
// 原生 input_tokens 不含缓存（cache_read_input_tokens / cache_creation_input_tokens 另计），
// adapter 必须折算为「含缓存」的 InputTokens，与 OpenAI 兼容协议同口径，
// 这样 llm.ComputeCost 的 (in - cacheRead)*Pin + cacheRead*Pcache 才对两种协议都成立。
func TestAnthropicUsage_NormalizedToInclusive(t *testing.T) {
	resp := &MessageResponse{
		ID:    "msg_1",
		Model: "claude-x",
		Usage: Usage{
			InputTokens:         100,  // 非缓存
			CacheReadTokens:     5000, // 命中
			CacheCreationTokens: 300,  // 写入
			OutputTokens:        50,
		},
	}
	u := anthropicResponseToResult(resp).Usage
	if u.InputTokens != 5400 {
		t.Fatalf("InputTokens = %d, want 5400 (100 + 5000 + 300)", u.InputTokens)
	}
	if u.InputTokenDetails.CacheReadTokens != 5000 || u.InputTokenDetails.NoCacheTokens != 100 {
		t.Fatalf("details = %+v", u.InputTokenDetails)
	}
	p := llm.ModelPrice{InputPer1M: 3, OutputPer1M: 15, CacheReadPer1M: 0.3}
	_, _, cost := llm.ComputeCost(u, p)
	// 非缓存 100 + 写入 300 按输入价；命中 5000 按缓存价；绝不双计命中部分。
	want := (400*3.0 + 5000*0.3 + 50*15.0) / 1e6
	if d := cost - want; d > 1e-12 || d < -1e-12 {
		t.Fatalf("cost = %v, want %v", cost, want)
	}
}
