package api

import (
	"github.com/gin-gonic/gin"

	"github.com/kasuganosora/thinkbot/agent/pipeline"
	"github.com/kasuganosora/thinkbot/config"
	"github.com/kasuganosora/thinkbot/dao"
)

// ============================================================================
// 计费 / 额度 API
// ============================================================================

// featureCoveredCost 计算某个「预算键」实际覆盖的花费。
//
// 预算键可能是细分标签（dream_extract），也可能是功能组名（dreaming）。
// 后者必须把组内所有阶段的花费汇总 —— 否则用户配了 "dreaming"，
// 墙那边（costFeatureGroup）按组累加会正常拦截，看板进度却恒显示 0，
// 表现为「明明超了却显示没花钱」。
func featureCoveredCost(costByFeature map[string]float64, key string) float64 {
	total := 0.0
	for f, c := range costByFeature {
		if f == key || pipeline.CostFeatureGroup(f) == key {
			total += c
		}
	}
	return total
}

// costUnlabeledLabel 是「未打标」维度的展示名。
//
// stats_usage_daily 里存在 feature / model 为空的行：早期数据没打功能标签，
// budget_warning 这类事件行本来就没有模型。若原样输出空字符串，看板会出现
// 一个无法理解的空白条目，且各维度之和与总花费对不上。
const costUnlabeledLabel = "(未分类)"

// handleBillingModels 返回 token↔金钱换算表（所有模型的单价）。
//
//	GET /api/billing/models
func (s *Server) handleBillingModels(c *gin.Context) {
	builder := config.NewBuilder(s.store, s.logger)
	OK(c, gin.H{"models": builder.ModelPriceTable()})
}

// BotQuotaView 单个 bot 的额度 + 本周期已花费视图。
type BotQuotaView struct {
	BotID         string             `json:"botId"`
	Name          string             `json:"name"`
	Enabled       bool               `json:"enabled"`
	Currency      string             `json:"currency"`
	Total         float64            `json:"total"`
	Period        string             `json:"period"`
	Features      map[string]float64 `json:"features"`
	UsedTotal     float64            `json:"usedTotal"`
	UsedByFeature map[string]float64 `json:"usedByFeature"`
	Progress      float64            `json:"progress"` // UsedTotal/Total（0=不限制）
}

// SystemQuotaView 全局额度 + 本周期已花费视图。
type SystemQuotaView struct {
	Period        string             `json:"period"`
	Currency      string             `json:"currency"`
	Total         float64            `json:"total"`
	Features      map[string]float64 `json:"features"`
	UsedTotal     float64            `json:"usedTotal"`
	UsedByFeature map[string]float64 `json:"usedByFeature"`
	Progress      float64            `json:"progress"`
}

// handleBillingQuotas 返回每个 bot 的额度配置 + 本周期进度，以及全局额度。
//
//	GET /api/billing/quotas
func (s *Server) handleBillingQuotas(c *gin.Context) {
	sys, sysOK := pipeline.SystemCostQuotaFromStore(s.store)
	period := sys.Period
	if period == "" {
		period = "monthly"
	}
	currency := sys.Currency
	if currency == "" {
		currency = "CNY"
	}
	start := pipeline.PeriodStartNow(period)

	// bot 定义列表
	var bots []dao.BotDefinition
	if err := s.db.Find(&bots).Error; err != nil {
		Fail(c, err)
		return
	}

	// 本周期各 (bot, feature) 花费
	type usageRow struct {
		BotID   string
		Feature string
		Cost    float64
	}
	var rows []usageRow
	if err := s.db.WithContext(c.Request.Context()).Raw(`
		SELECT bot_id, feature, SUM(cost_total) AS cost
		FROM stats_usage_daily
		WHERE date >= ?
		GROUP BY bot_id, feature
	`, start).Scan(&rows).Error; err != nil {
		Fail(c, err)
		return
	}

	botTotal := map[string]float64{}
	botFeature := map[string]map[string]float64{}
	globalFeature := map[string]float64{}
	for _, r := range rows {
		botTotal[r.BotID] += r.Cost
		if r.Feature != "" {
			if botFeature[r.BotID] == nil {
				botFeature[r.BotID] = map[string]float64{}
			}
			botFeature[r.BotID][r.Feature] += r.Cost
			globalFeature[r.Feature] += r.Cost
		}
	}

	views := make([]BotQuotaView, 0, len(bots))
	for _, b := range bots {
		cfg, _ := pipeline.ParseCostQuotaConfig(b.CostQuota)
		used := botTotal[b.ID]
		// 细分用度 + 各预算键的覆盖用度（组名也要能查到数）
		fv := map[string]float64{}
		for f, c := range botFeature[b.ID] {
			fv[f] = c
		}
		for k := range cfg.Features {
			if _, ok := fv[k]; !ok {
				fv[k] = featureCoveredCost(botFeature[b.ID], k)
			}
		}
		for k := range sys.Features {
			if _, ok := fv[k]; !ok {
				fv[k] = featureCoveredCost(botFeature[b.ID], k)
			}
		}
		progress := 0.0
		if cfg.Total > 0 {
			progress = used / cfg.Total
		}
		cur := cfg.Currency
		if cur == "" {
			cur = currency
		}
		views = append(views, BotQuotaView{
			BotID:         b.ID,
			Name:          b.Name,
			Enabled:       cfg.Enabled,
			Currency:      cur,
			Total:         cfg.Total,
			Period:        period,
			Features:      cfg.Features,
			UsedTotal:     used,
			UsedByFeature: fv,
			Progress:      progress,
		})
	}

	sysUsed := 0.0
	for _, v := range botTotal {
		sysUsed += v
	}
	// 全局侧同样补齐预算键（组名）的覆盖用度
	for k := range sys.Features {
		if _, ok := globalFeature[k]; !ok {
			globalFeature[k] = featureCoveredCost(globalFeature, k)
		}
	}
	sysProgress := 0.0
	if sysOK && sys.Total > 0 {
		sysProgress = sysUsed / sys.Total
	}
	sysView := SystemQuotaView{
		Period:        period,
		Currency:      currency,
		Total:         sys.Total,
		Features:      sys.Features,
		UsedTotal:     sysUsed,
		UsedByFeature: globalFeature,
		Progress:      sysProgress,
	}

	OK(c, gin.H{"period": period, "currency": currency, "bots": views, "system": sysView})
}

// CostBreakdownItem 单维度的花费分解项。
type CostBreakdownItem struct {
	Key          string  `json:"key"`          // feature 名 / model ID / bot ID
	Cost         float64 `json:"cost"`         // 本周期花费
	Requests     int     `json:"requests"`     // 请求数
	InputTokens  int     `json:"inputTokens"`  // 输入 token
	OutputTokens int     `json:"outputTokens"` // 输出 token
	Limit        float64 `json:"limit"`        // 该维度预算（0=不限制），仅 feature 维度可能非空
	Progress     float64 `json:"progress"`     // 该预算键覆盖的花费 / Limit
	LimitKey     string  `json:"limitKey"`     // 预算来自哪个键（可能等于 key，也可能是所属功能组名）
}

// BillingUsageView 计费用量拆解（看板数据源）。
type BillingUsageView struct {
	Period    string              `json:"period"`
	Currency  string              `json:"currency"`
	TotalCost float64             `json:"totalCost"`
	ByFeature []CostBreakdownItem `json:"byFeature"`
	ByModel   []CostBreakdownItem `json:"byModel"`
	ByBot     []CostBreakdownItem `json:"byBot"` // 仅全局视图（未指定 bot）返回
}

// handleBillingUsage 按功能 / 模型 / bot 拆解本周期花费，驱动计费看板。
//
//	GET /api/billing/usage?bot=<id>&period=<daily|weekly|monthly>
func (s *Server) handleBillingUsage(c *gin.Context) {
	bot := c.Query("bot")
	period := c.Query("period")
	sys, _ := pipeline.SystemCostQuotaFromStore(s.store)
	if period == "" {
		period = sys.Period
	}
	if period == "" {
		period = "monthly"
	}
	currency := sys.Currency
	if currency == "" {
		currency = "CNY"
	}
	start := pipeline.PeriodStartNow(period)

	db := s.db.WithContext(c.Request.Context())
	where := "date >= ?"
	args := []any{start}
	if bot != "" {
		where += " AND bot_id = ?"
		args = append(args, bot)
	}

	// 按功能
	var featureRows []struct {
		Feature  string
		Cost     float64
		Requests int
		Input    int
		Output   int
	}
	db.Raw(`SELECT feature, SUM(cost_total) AS cost, SUM(total_requests) AS requests,
		SUM(input_tokens) AS input, SUM(output_tokens) AS output
		FROM stats_usage_daily WHERE `+where+` GROUP BY feature ORDER BY cost DESC`, args...).
		Scan(&featureRows)

	// 按模型
	var modelRows []struct {
		Model    string
		Cost     float64
		Requests int
		Input    int
		Output   int
	}
	db.Raw(`SELECT model, SUM(cost_total) AS cost, SUM(total_requests) AS requests,
		SUM(input_tokens) AS input, SUM(output_tokens) AS output
		FROM stats_usage_daily WHERE `+where+` GROUP BY model ORDER BY cost DESC`, args...).
		Scan(&modelRows)

	view := BillingUsageView{Period: period, Currency: currency}

	// feature 预算（用于进度线）：优先 bot 级，其次全局级。
	//
	// 仅当查询周期与全局周期一致时才给出预算：预算是「每全局周期」定义的，
	// 拿日花费去除以月度预算会得到完全误导的进度（看板支持切换周期查看）。
	featureLimit := map[string]float64{}
	sysPeriod := sys.Period
	if sysPeriod == "" {
		sysPeriod = "monthly"
	}
	if period == sysPeriod {
		if bot != "" {
			var bd dao.BotDefinition
			if err := s.db.First(&bd, "id = ?", bot).Error; err == nil {
				if cfg, ok := pipeline.ParseCostQuotaConfig(bd.CostQuota); ok {
					for k, v := range cfg.Features {
						featureLimit[k] = v
					}
				}
			}
		}
		for k, v := range sys.Features {
			if _, ok := featureLimit[k]; !ok {
				featureLimit[k] = v
			}
		}
	}

	// 细分花费索引：预算键可能是组名（dreaming），进度要按覆盖的全部阶段汇总，
	// 不能只用当前这一行的花费做分子。
	featureCost := make(map[string]float64, len(featureRows))
	for _, r := range featureRows {
		featureCost[r.Feature] += r.Cost
	}

	var totalCost float64
	for _, r := range featureRows {
		// 空 feature（早期数据 / 未打标的调用）不丢弃，归到「未分类」。
		// 否则 ByFeature 之和会小于总花费，用户看到一笔对不上的差额却查不到去向。
		key := r.Feature
		if key == "" {
			key = costUnlabeledLabel
		}
		// 预算键：优先精确匹配，其次落到所属功能组的预算（配 "dreaming" 时
		// dream_extract 等阶段行都应显示该预算与整体进度）。
		limKey := r.Feature
		lim := featureLimit[r.Feature]
		if lim <= 0 {
			if g := pipeline.CostFeatureGroup(r.Feature); g != r.Feature {
				if gl, ok := featureLimit[g]; ok && gl > 0 {
					lim = gl
					limKey = g
				}
			}
		}
		prog := 0.0
		if lim > 0 {
			prog = featureCoveredCost(featureCost, limKey) / lim
		}
		view.ByFeature = append(view.ByFeature, CostBreakdownItem{
			Key: key, Cost: r.Cost, Requests: r.Requests,
			InputTokens: r.Input, OutputTokens: r.Output, Limit: lim, Progress: prog,
			LimitKey: limKey,
		})
	}
	// 总花费取「按模型」聚合之和（覆盖全部调用，不受 feature 标签缺失影响），
	// 比按 feature 求和更完整；feature 分解为下钻视图，不要求与总花费相等。
	for _, r := range modelRows {
		// 空 model（如 budget_warning 这类不带模型的事件行）显式标注，
		// 避免在「按模型」拆解里出现一个无法理解的空白条目。
		key := r.Model
		if key == "" {
			key = costUnlabeledLabel
		}
		view.ByModel = append(view.ByModel, CostBreakdownItem{
			Key: key, Cost: r.Cost, Requests: r.Requests,
			InputTokens: r.Input, OutputTokens: r.Output,
		})
		totalCost += r.Cost
	}
	view.TotalCost = totalCost

	// 全局视图：按 bot 拆解
	if bot == "" {
		var botRows []struct {
			BotID    string
			Cost     float64
			Requests int
			Input    int
			Output   int
		}
		db.Raw(`SELECT bot_id, SUM(cost_total) AS cost, SUM(total_requests) AS requests,
			SUM(input_tokens) AS input, SUM(output_tokens) AS output
			FROM stats_usage_daily WHERE `+where+` GROUP BY bot_id ORDER BY cost DESC`, args...).
			Scan(&botRows)
		for _, r := range botRows {
			view.ByBot = append(view.ByBot, CostBreakdownItem{
				Key: r.BotID, Cost: r.Cost, Requests: r.Requests,
				InputTokens: r.Input, OutputTokens: r.Output,
			})
		}
	}

	OK(c, view)
}
