package llm

import "testing"

func TestComputeCost(t *testing.T) {
	price := ModelPrice{
		InputPer1M:     2.0,
		OutputPer1M:    4.0,
		CacheReadPer1M: 0.5,
		Currency:       "CNY",
	}
	// InputTokens 含缓存：1M 输入里 200K 命中缓存。
	usage := Usage{
		InputTokens:  1_000_000,
		OutputTokens: 500_000,
		InputTokenDetails: InputTokenDetail{
			CacheReadTokens: 200_000,
		},
	}
	in, out, total := ComputeCost(usage, price)
	// input  = 800000*2/1e6 + 200000*0.5/1e6 = 1.6 + 0.1 = 1.7
	// output = 500000*4/1e6 = 2.0
	// total  = 3.7
	if !approxEq(in, 1.7) {
		t.Errorf("input cost = %v, want 1.7", in)
	}
	if !approxEq(out, 2.0) {
		t.Errorf("output cost = %v, want 2.0", out)
	}
	if !approxEq(total, 3.7) {
		t.Errorf("total cost = %v, want 3.7", total)
	}
}

// TestComputeCostNoDoubleCountCache 锁定缓存命中不被双计的不变量：
// 旧公式 in*Pin + cacheRead*Pcache 把命中部分按全价算一遍后又按缓存价加一遍。
func TestComputeCostNoDoubleCountCache(t *testing.T) {
	glm := ModelPrice{InputPer1M: 8, OutputPer1M: 28, CacheReadPer1M: 2}

	// 全部命中缓存：只按缓存价计，绝不能再叠加全价。
	allCached := Usage{InputTokens: 1_000_000, InputTokenDetails: InputTokenDetail{CacheReadTokens: 1_000_000}}
	if _, _, total := ComputeCost(allCached, glm); !approxEq(total, 2) {
		t.Fatalf("all-cached cost = %v, want 2 (1M × ¥2 cache price), old formula gave 10", total)
	}

	// 无缓存：与全价一致。
	noCache := Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000}
	if _, _, total := ComputeCost(noCache, glm); !approxEq(total, 36) {
		t.Fatalf("no-cache cost = %v, want 36", total)
	}

	// 实测 glm-5.3 九月形态：86% 命中率。
	real := Usage{
		InputTokens:       195_406_290,
		OutputTokens:      20_696_939,
		InputTokenDetails: InputTokenDetail{CacheReadTokens: 168_373_312},
	}
	want := (float64(195_406_290-168_373_312)*8 + 168_373_312*2 + 20_696_939*28) / 1e6
	if _, _, total := ComputeCost(real, glm); !approxEq(total, want) {
		t.Fatalf("real-shape cost = %v, want %v", total, want)
	}
}

// TestComputeCostOpenAIStyleUsage：OpenAI 兼容协议（智谱 GLM / xAI / OpenAI）
// prompt_tokens 已包含 cached_tokens，adapter 原样存入 InputTokens。
func TestComputeCostOpenAIStyleUsage(t *testing.T) {
	// prompt_tokens=1200, cached_tokens=800, completion_tokens=300（智谱文档示例）
	u := Usage{
		InputTokens:       1200,
		OutputTokens:      300,
		CachedInputTokens: 800,
		InputTokenDetails: InputTokenDetail{CacheReadTokens: 800, NoCacheTokens: 400},
	}
	_, _, total := ComputeCost(u, ModelPrice{InputPer1M: 8, OutputPer1M: 28, CacheReadPer1M: 2})
	want := (400*8 + 800*2 + 300*28) / 1e6
	if !approxEq(total, want) {
		t.Fatalf("openai-style cost = %v, want %v", total, want)
	}
}

// TestComputeCostAnthropicStyleUsage：Anthropic 原生 input_tokens 不含缓存，
// llm/anthropic 解析时折算为 InputTokens = nonCached + cacheRead + cacheWrite。
// 计费结果必须是 nonCached(+cacheWrite)*Pin + cacheRead*Pcache，不能少算也不能双计。
func TestComputeCostAnthropicStyleUsage(t *testing.T) {
	nonCached, cacheRead, cacheWrite := 100, 5000, 300
	u := Usage{
		InputTokens:  nonCached + cacheRead + cacheWrite,
		OutputTokens: 50,
		InputTokenDetails: InputTokenDetail{
			NoCacheTokens:    nonCached,
			CacheReadTokens:  cacheRead,
			CacheWriteTokens: cacheWrite,
		},
	}
	p := ModelPrice{InputPer1M: 3, OutputPer1M: 15, CacheReadPer1M: 0.3}
	_, _, total := ComputeCost(u, p)
	want := (float64(nonCached+cacheWrite)*3 + float64(cacheRead)*0.3 + 50*15) / 1e6
	if !approxEq(total, want) {
		t.Fatalf("anthropic-style cost = %v, want %v", total, want)
	}
}

// 未配置缓存单价：命中部分按输入全价计（不知道折扣就不打折），不能变成免费。
func TestComputeCostNoCachePriceFallsBackToInputPrice(t *testing.T) {
	u := Usage{InputTokens: 1_000_000, InputTokenDetails: InputTokenDetail{CacheReadTokens: 600_000}}
	_, _, total := ComputeCost(u, ModelPrice{InputPer1M: 4, OutputPer1M: 12})
	if !approxEq(total, 4) {
		t.Fatalf("no cache price → %v, want 4 (all input at full price)", total)
	}
}

// 异常数据（cacheRead > input）不产生负数。
func TestComputeCostCacheExceedsInputNoNegative(t *testing.T) {
	u := Usage{InputTokens: 100, InputTokenDetails: InputTokenDetail{CacheReadTokens: 1_000_000}}
	in, _, total := ComputeCost(u, ModelPrice{InputPer1M: 8, CacheReadPer1M: 2})
	if in < 0 || !approxEq(total, 2) {
		t.Fatalf("cache>input → in=%v total=%v, want total 2 and no negatives", in, total)
	}
}

func approxEq(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d <= 1e-9
}

func TestHasPrice(t *testing.T) {
	if (ModelPrice{}).HasPrice() {
		t.Error("all-zero price should HasPrice=false")
	}
	if !(ModelPrice{InputPer1M: 0.1}).HasPrice() {
		t.Error("any positive dimension should HasPrice=true")
	}
	if !(ModelPrice{CacheReadPer1M: 1}).HasPrice() {
		t.Error("cache-only price should HasPrice=true")
	}
}

func TestComputeCostNoPriceZero(t *testing.T) {
	// 未配置单价 → 全部 0，调用方可据此跳过计费
	usage := Usage{InputTokens: 1e9, OutputTokens: 1e9}
	in, out, total := ComputeCost(usage, ModelPrice{})
	if in != 0 || out != 0 || total != 0 {
		t.Errorf("no price → (in,out,total)=(%v,%v,%v), want (0,0,0)", in, out, total)
	}
}
