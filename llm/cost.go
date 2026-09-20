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
// 公式：cost = (in*Pin + out*Pout + cacheRead*Pcache) / 1e6。
// 缺单价（Price=0）的维度不参与计费（返回 0），调用方据此判断「该模型未配置单价」。
func ComputeCost(usage Usage, p ModelPrice) (input, output, total float64) {
	input = float64(usage.InputTokens) * p.InputPer1M / 1e6
	output = float64(usage.OutputTokens) * p.OutputPer1M / 1e6
	cache := float64(usage.InputTokenDetails.CacheReadTokens) * p.CacheReadPer1M / 1e6
	total = input + output + cache
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
