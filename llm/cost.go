package llm

import "strconv"

// ErrCostQuotaExceeded 表示一次 LLM 调用因触碰金钱额度墙（bot 总预算 / 全局总预算 /
// bot 功能预算 / 全局功能预算 任一维度）而被拦截。
//
// 该错误由 CostRecordingProvider 在调用前 pre-check 阶段返回（不实际调用模型，
// 与 token quota 一致「允许最后一次超额」）。调用方按自身语义处理：
//   - 用户回复路径（CostQuotaMiddleware）：转为友好提示，不致命。
//   - 嵌套调用（subagent / workflow / memory / dreaming 等）：调用方 skip 该步，
//     不影响主流程。
type CostQuotaExceededError struct {
	// Dimension 命中的额度墙维度（如 "bot:B" / "feature:dreaming"）。
	Dimension string
	// Feature 触发的功能（空表示总预算墙）。
	Feature string
	// Current 当前周期已花费。
	Current float64
	// Limit 该墙的预算上限（0 表示未设该墙，理论上不会触发）。
	Limit float64
	// Period 当前周期标识（如 "2026-09" / "2026-W38" / "2026-09-20"）。
	Period string
}

// Error 实现 error 接口。
func (e *CostQuotaExceededError) Error() string {
	return "cost quota exceeded: dimension=" + e.Dimension +
		" feature=" + e.Feature +
		" current=" + formatFloat(e.Current) +
		" limit=" + formatFloat(e.Limit) +
		" period=" + e.Period
}

// Is 支持 errors.Is(err, ErrCostQuotaExceeded) 快速判定。
func (e *CostQuotaExceededError) Is(target error) bool {
	_, ok := target.(*CostQuotaExceededError)
	return ok
}

// ErrCostQuotaExceeded 是 CostQuotaExceededError 的零值哨兵，供 errors.Is 使用。
var ErrCostQuotaExceeded = &CostQuotaExceededError{}

// ModelPrice 描述单个模型的单价（token↔金钱换算表的一行）。
type ModelPrice struct {
	// InputPer1M 每 1M 输入 token 单价。
	InputPer1M float64
	// OutputPer1M 每 1M 输出 token 单价。
	OutputPer1M float64
	// CacheReadPer1M 每 1M 缓存读 token 单价（命中提示缓存的 input）。
	CacheReadPer1M float64
	// Currency 货币代码（如 CNY、USD）。
	Currency string
}

// ComputeCost 按单价将一次调用的 token 用量换算为金钱。
//
// 用量口径（全项目统一，所有 adapter 在解析时已归一化）：
//   - usage.InputTokens 是**含缓存**的输入总量。OpenAI 兼容协议（含智谱 GLM、xAI）
//     的 prompt_tokens 本身就包含 prompt_tokens_details.cached_tokens；Gemini 的
//     promptTokenCount 同样包含缓存；Anthropic 原生 input_tokens 不含缓存，
//     llm/anthropic 在解析时已折算为 nonCached + cacheRead + cacheWrite。
//   - usage.InputTokenDetails.CacheReadTokens 是其中命中缓存的部分。
//
// 因此公式为（与智谱官方「未命中缓存的输入费用 + 缓存命中费用 + 输出费用」一致）：
//
//	cost = (in - cacheRead)*Pin + cacheRead*Pcache + out*Pout   （/1e6）
//
// 旧实现按 in*Pin + cacheRead*Pcache 计算，缓存命中部分既按全价算了一遍、又按缓存价
// 再加一遍（双计）。GLM 这类缓存命中率 80%+ 的负载会因此多算一倍以上。
//
// 细节：
//   - 未配置缓存单价（Pcache=0）时，缓存命中部分按输入全价计，即「不知道折扣就不打折」，
//     与旧版对这类模型的结果一致（旧版缓存部分已含在 in 里按全价算、缓存项为 0）。
//   - cacheRead > in 属于异常数据（调用方只填了缓存数），非缓存部分截断为 0，
//     缓存部分仍按缓存价计，不产生负数。
//   - Anthropic 的 cache write 仍包含在 (in - cacheRead) 中按输入全价计（官方为
//     1.25x/2x，此处略低估）；本项目当前没有单独的缓存写单价字段。
//
// 返回值：input 为输入侧花费（非缓存 + 缓存命中），output 为输出花费，total = input + output。
// 缺单价（Price=0）的维度不参与计费（返回 0），调用方据此判断「该模型未配置单价」。
func ComputeCost(usage Usage, p ModelPrice) (input, output, total float64) {
	cacheRead := usage.InputTokenDetails.CacheReadTokens
	if cacheRead < 0 {
		cacheRead = 0
	}
	nonCached := usage.InputTokens - cacheRead
	if nonCached < 0 {
		nonCached = 0
	}
	cacheRate := p.CacheReadPer1M
	if cacheRate <= 0 {
		cacheRate = p.InputPer1M
	}
	input = (float64(nonCached)*p.InputPer1M + float64(cacheRead)*cacheRate) / 1e6
	output = float64(usage.OutputTokens) * p.OutputPer1M / 1e6
	total = input + output
	return input, output, total
}

// HasPrice 判断该单价表是否包含有效计价（任一维度 > 0）。
func (p ModelPrice) HasPrice() bool {
	return p.InputPer1M > 0 || p.OutputPer1M > 0 || p.CacheReadPer1M > 0
}

func formatFloat(v float64) string {
	return strconv.FormatFloat(v, 'f', 4, 64)
}

// NewCostQuotaExceeded 构造一个带上下文的额度超限错误。
func NewCostQuotaExceeded(dim, feature string, current, limit float64, period string) *CostQuotaExceededError {
	return &CostQuotaExceededError{
		Dimension: dim,
		Feature:   feature,
		Current:   current,
		Limit:     limit,
		Period:    period,
	}
}
