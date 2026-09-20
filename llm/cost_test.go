package llm

import "testing"

func TestComputeCost(t *testing.T) {
	price := ModelPrice{
		InputPer1M:     2.0,
		OutputPer1M:    4.0,
		CacheReadPer1M: 0.5,
		Currency:       "CNY",
	}
	usage := Usage{
		InputTokens:  1_000_000,
		OutputTokens: 500_000,
		InputTokenDetails: InputTokenDetail{
			CacheReadTokens: 200_000,
		},
	}
	in, out, total := ComputeCost(usage, price)
	// input  = 1e6 * 2 / 1e6 = 2.0
	// output = 500000 * 4 / 1e6 = 2.0
	// cache  = 200000 * 0.5 / 1e6 = 0.1
	// total  = 4.1
	if in != 2.0 {
		t.Errorf("input cost = %v, want 2.0", in)
	}
	if out != 2.0 {
		t.Errorf("output cost = %v, want 2.0", out)
	}
	if total != 4.1 {
		t.Errorf("total cost = %v, want 4.1", total)
	}
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
