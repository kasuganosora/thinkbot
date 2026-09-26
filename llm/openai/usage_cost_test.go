package openai

import (
	"encoding/json"
	"testing"

	"github.com/kasuganosora/thinkbot/llm"
)

// TestConvertChatUsage_InclusiveCachedTokens 锁定 OpenAI 兼容协议（含智谱 GLM）的用量口径：
// usage.prompt_tokens 已包含 prompt_tokens_details.cached_tokens，InputTokens 原样保存
// 含缓存总量，CacheReadTokens 为命中部分，NoCacheTokens = prompt - cached。
// 计费（llm.ComputeCost）依赖这一口径，命中部分只能按缓存价计一次。
func TestConvertChatUsage_InclusiveCachedTokens(t *testing.T) {
	// 智谱文档示例：prompt 1200 / cached 800 / completion 300 / total 1500
	raw := `{"prompt_tokens":1200,"completion_tokens":300,"total_tokens":1500,
		"prompt_tokens_details":{"cached_tokens":800}}`
	var cu ChatUsage
	if err := json.Unmarshal([]byte(raw), &cu); err != nil {
		t.Fatal(err)
	}
	u := convertChatUsage(&cu)
	if u.InputTokens != 1200 || u.InputTokenDetails.CacheReadTokens != 800 || u.InputTokenDetails.NoCacheTokens != 400 {
		t.Fatalf("usage = %+v, want input 1200 (inclusive), cacheRead 800, noCache 400", u)
	}
	if u.TotalTokens != 1500 {
		t.Fatalf("total = %d, want 1500 (= prompt + completion, i.e. cached is inside prompt)", u.TotalTokens)
	}
	_, _, cost := llm.ComputeCost(u, llm.ModelPrice{InputPer1M: 8, OutputPer1M: 28, CacheReadPer1M: 2})
	want := (400*8.0 + 800*2.0 + 300*28.0) / 1e6
	if d := cost - want; d > 1e-12 || d < -1e-12 {
		t.Fatalf("cost = %v, want %v", cost, want)
	}
}
