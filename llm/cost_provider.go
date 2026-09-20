package llm

import (
	"context"
)

// CostWallChecker 在每次 LLM 调用前检查四堵额度墙（bot 总预算 / 全局总预算 /
// bot 功能预算 / 全局功能预算）。任意一堵已耗尽时返回 *CostQuotaExceededError，
// 调用方据此跳过本次调用（不实际请求模型），与 token quota「允许最后一次超额」一致。
//
// 实现位于 pipeline 包（CostQuotaResolver），此处仅定义回调契约，避免 llm→pipeline 循环依赖。
type CostWallChecker func(botID, feature string) *CostQuotaExceededError

// CostRecorder 在一次 LLM 调用成功后把花费计入各维度计数器。
// 回调实现负责把 cost 累加到四个维度（bot 总 / 全局总 / bot 功能 / 全局功能）。
type CostRecorder func(botID, feature string, cost float64)

// ModelPriceResolver 按模型 ID 解析单价（token↔金钱换算表的一行）。
// 查不到或该模型未配置单价时返回 ok=false，调用方据此跳过计费。
type ModelPriceResolver func(modelID string) (ModelPrice, bool)

// CostRecordingProvider 包裹一个 llm.Provider，在每次 DoGenerate / DoStream 前后：
//  1. pre-check：调用前检查四堵额度墙，任意墙已耗尽则直接返回 *CostQuotaExceededError
//     （不请求模型）。这保证了 cron / heartbeat / dreaming 等非用户路径也能被覆盖。
//  2. post：调用成功后按模型单价换算花费，并记入各维度计数器。
//
// 嵌套调用（subagent / workflow / memory / dreaming）只要走了被包裹的 provider，
// 就一律被记账 + 拦截，天然覆盖所有入口。
type CostRecordingProvider struct {
	inner    Provider
	botID    string
	priceFor ModelPriceResolver
	preCheck CostWallChecker
	record   CostRecorder
}

// NewCostRecordingProvider 创建计费记录 + 拦截 provider。
// botID 在构建时固定（bot 生命周期内不变）。
func NewCostRecordingProvider(inner Provider, botID string, priceFor ModelPriceResolver, preCheck CostWallChecker, record CostRecorder) *CostRecordingProvider {
	return &CostRecordingProvider{
		inner:    inner,
		botID:    botID,
		priceFor: priceFor,
		preCheck: preCheck,
		record:   record,
	}
}

// Name 委托给内层 provider。
func (p *CostRecordingProvider) Name() string { return p.inner.Name() }

// DoGenerate 调用前检查额度墙；通过后调用内层并记账花费。
func (p *CostRecordingProvider) DoGenerate(ctx context.Context, params GenerateParams) (*GenerateResult, error) {
	feature := StatsFeatureFromContext(ctx)
	if err := p.preCheck(p.botID, feature); err != nil {
		return nil, err
	}
	result, err := p.inner.DoGenerate(ctx, params)
	if err == nil && result != nil {
		p.recordCost(params, result, feature)
	}
	return result, err
}

// DoStream 调用前检查额度墙；通过后调用内层并包裹流，在流完成时记账花费。
func (p *CostRecordingProvider) DoStream(ctx context.Context, params GenerateParams) (*StreamResult, error) {
	feature := StatsFeatureFromContext(ctx)
	if err := p.preCheck(p.botID, feature); err != nil {
		return nil, err
	}
	result, err := p.inner.DoStream(ctx, params)
	if err == nil && result != nil {
		result = p.wrapStream(ctx, params, feature, result)
	}
	return result, err
}

// recordCost 按模型单价换算花费并记入各维度计数器。
// 未配置单价（priceFor ok=false 或 HasPrice=false）的模型不计费、不限额。
func (p *CostRecordingProvider) recordCost(params GenerateParams, result *GenerateResult, feature string) {
	if p.record == nil {
		return
	}
	modelID := ""
	if params.Model != nil {
		modelID = params.Model.ID
	}
	if modelID == "" {
		modelID = p.inner.Name()
	}
	price, ok := p.priceFor(modelID)
	if !ok || !price.HasPrice() {
		return
	}
	_, _, total := ComputeCost(result.Usage, price)
	if total <= 0 {
		return
	}
	p.record(p.botID, feature, total)
}

// wrapStream 包裹流通道，在 FinishPart 到达时记账花费。
func (p *CostRecordingProvider) wrapStream(ctx context.Context, params GenerateParams, feature string, sr *StreamResult) *StreamResult {
	orig := sr.Stream
	wrapped := make(chan StreamPart, 16)

	go func() {
		defer close(wrapped)
		var totalUsage Usage
		for part := range orig {
			if fp, ok := part.(*FinishPart); ok {
				totalUsage = fp.TotalUsage
			}
			wrapped <- part
		}
		if totalUsage.TotalTokens > 0 {
			result := &GenerateResult{Usage: totalUsage}
			p.recordCost(params, result, feature)
		}
	}()

	sr.Stream = wrapped
	return sr
}
