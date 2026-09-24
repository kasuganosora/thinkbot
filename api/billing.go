package api

import (
	"sort"

	"github.com/gin-gonic/gin"

	"github.com/kasuganosora/thinkbot/agent/pipeline"
	"github.com/kasuganosora/thinkbot/config"
	"github.com/kasuganosora/thinkbot/dao"
	"github.com/kasuganosora/thinkbot/llm"
)

// effectiveRowCost 取一行的「生效花费」：优先落库的 cost_total，为 0 时按当前
// 单价回算兜底——与额度恢复的 rowCost（agent/pipeline/cost_quota.go）同源同口径。
//
// 为什么必须在这里也兜底：cost_total 是调用当时算出来的，单价没配置时写进去的
// 就是 0（本项目 9 个 GLM 模型的单价长期为空）。补上单价后，新调用会正常落库，
// 但**存量行仍是 0** —— 若看板直接 SUM(cost_total)，就会出现「今天 5.8 万 token、
// 花费 ¥0」而墙那边其实已经按 ¥0.55 在扣。显示与拦截口径不一致会直接误导排查。
func effectiveRowCost(cost float64, model string, input, output, cacheRead int, priceFor llm.ModelPriceResolver) float64 {
	if cost > 0 || priceFor == nil {
		return cost
	}
	price, ok := priceFor(model)
	if !ok || !price.HasPrice() {
		return cost
	}
	_, _, total := llm.ComputeCost(llm.Usage{
		InputTokens:       input,
		OutputTokens:      output,
		InputTokenDetails: llm.InputTokenDetail{CacheReadTokens: cacheRead},
	}, price)
	return total
}

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

	// 本周期各 (bot, feature) 花费。
	//
	// 同样要带上 model / token 用量：cost_total 为 0 的存量行需按当前单价回算
	// （见 effectiveRowCost），否则额度进度会显示 0 而墙那边其实已经在扣——
	// 用户看到「今天花了 0 / 300」会以为拦截没生效。
	priceFor := config.NewBuilder(s.store, s.logger).PriceResolver()
	type usageRow struct {
		BotID     string
		Feature   string
		Model     string
		Cost      float64
		Input     int
		Output    int
		CacheRead int
	}
	var rows []usageRow
	if err := s.db.WithContext(c.Request.Context()).Raw(`
		SELECT bot_id, feature, model, SUM(cost_total) AS cost,
			SUM(input_tokens) AS input, SUM(output_tokens) AS output,
			SUM(cache_read_tokens) AS cache_read
		FROM stats_usage_daily
		WHERE date >= ?
		GROUP BY bot_id, feature, model
	`, start).Scan(&rows).Error; err != nil {
		Fail(c, err)
		return
	}

	botTotal := map[string]float64{}
	botFeature := map[string]map[string]float64{}
	globalFeature := map[string]float64{}
	for _, r := range rows {
		cost := effectiveRowCost(r.Cost, r.Model, r.Input, r.Output, r.CacheRead, priceFor)
		botTotal[r.BotID] += cost
		if r.Feature != "" {
			if botFeature[r.BotID] == nil {
				botFeature[r.BotID] = map[string]float64{}
			}
			botFeature[r.BotID][r.Feature] += cost
			globalFeature[r.Feature] += cost
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

	// 单价解析器：用于把 cost_total=0 的存量行按当前单价回算（见 effectiveRowCost）。
	priceFor := config.NewBuilder(s.store, s.logger).PriceResolver()

	// 按功能（细分到 model，回算花费需要按模型取单价）
	var featureRows []struct {
		Feature   string
		Model     string
		Cost      float64
		Requests  int
		Input     int
		Output    int
		CacheRead int
	}
	db.Raw(`SELECT feature, model, SUM(cost_total) AS cost, SUM(total_requests) AS requests,
		SUM(input_tokens) AS input, SUM(output_tokens) AS output,
		SUM(cache_read_tokens) AS cache_read
		FROM stats_usage_daily WHERE `+where+` GROUP BY feature, model`, args...).
		Scan(&featureRows)

	// 按模型
	var modelRows []struct {
		Model     string
		Cost      float64
		Requests  int
		Input     int
		Output    int
		CacheRead int
	}
	db.Raw(`SELECT model, SUM(cost_total) AS cost, SUM(total_requests) AS requests,
		SUM(input_tokens) AS input, SUM(output_tokens) AS output,
		SUM(cache_read_tokens) AS cache_read
		FROM stats_usage_daily WHERE `+where+` GROUP BY model`, args...).
		Scan(&modelRows)
	for i := range modelRows {
		modelRows[i].Cost = effectiveRowCost(modelRows[i].Cost, modelRows[i].Model,
			modelRows[i].Input, modelRows[i].Output, modelRows[i].CacheRead, priceFor)
	}
	// 排序必须用回算后的 cost：存量行 cost_total=0，按原值排会把真正花钱的模型排到后面。
	sort.Slice(modelRows, func(i, j int) bool { return modelRows[i].Cost > modelRows[j].Cost })

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

	// 按 feature 归并（同一 feature 可能跨多个模型），并在此过程中回算存量行的花费。
	byFeature := map[string]*CostBreakdownItem{}
	var featureOrder []string
	for _, r := range featureRows {
		key := r.Feature
		if key == "" {
			key = costUnlabeledLabel
		}
		it, ok := byFeature[key]
		if !ok {
			it = &CostBreakdownItem{Key: key, LimitKey: r.Feature}
			byFeature[key] = it
			featureOrder = append(featureOrder, key)
		}
		it.Cost += effectiveRowCost(r.Cost, r.Model, r.Input, r.Output, r.CacheRead, priceFor)
		it.Requests += r.Requests
		it.InputTokens += r.Input
		it.OutputTokens += r.Output
	}

	// 细分花费索引：预算键可能是组名（dreaming），进度要按覆盖的全部阶段汇总，
	// 不能只用当前这一行的花费做分子。
	featureCost := make(map[string]float64, len(byFeature))
	for k, it := range byFeature {
		featureCost[k] = it.Cost
	}

	var totalCost float64
	for _, key := range featureOrder {
		it := byFeature[key]
		// 预算键：优先精确匹配，其次落到所属功能组的预算（配 "dreaming" 时
		// dream_extract 等阶段行都应显示该预算与整体进度）。
		limKey := it.LimitKey
		lim := featureLimit[limKey]
		if lim <= 0 {
			if g := pipeline.CostFeatureGroup(limKey); g != limKey {
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
		it.Limit = lim
		it.LimitKey = limKey
		it.Progress = prog
		view.ByFeature = append(view.ByFeature, *it)
	}
	// 按花费降序（SQL 原本 ORDER BY cost DESC，改在 Go 里归并后需自行排序，
	// 且必须用回算后的 cost 排，否则存量行会让排序失真）。
	sort.Slice(view.ByFeature, func(i, j int) bool {
		return view.ByFeature[i].Cost > view.ByFeature[j].Cost
	})
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
			BotID     string
			Model     string
			Cost      float64
			Requests  int
			Input     int
			Output    int
			CacheRead int
		}
		db.Raw(`SELECT bot_id, model, SUM(cost_total) AS cost, SUM(total_requests) AS requests,
			SUM(input_tokens) AS input, SUM(output_tokens) AS output,
			SUM(cache_read_tokens) AS cache_read
			FROM stats_usage_daily WHERE `+where+` GROUP BY bot_id, model`, args...).
			Scan(&botRows)
		byBot := map[string]*CostBreakdownItem{}
		for i := range botRows {
			r := &botRows[i]
			it, ok := byBot[r.BotID]
			if !ok {
				it = &CostBreakdownItem{Key: r.BotID}
				byBot[r.BotID] = it
			}
			it.Cost += effectiveRowCost(r.Cost, r.Model, r.Input, r.Output, r.CacheRead, priceFor)
			it.Requests += r.Requests
			it.InputTokens += r.Input
			it.OutputTokens += r.Output
		}
		for _, it := range byBot {
			view.ByBot = append(view.ByBot, *it)
		}
		sort.Slice(view.ByBot, func(i, j int) bool { return view.ByBot[i].Cost > view.ByBot[j].Cost })
	}

	OK(c, view)
}
