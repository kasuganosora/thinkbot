package memory

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/kasuganosora/thinkbot/llm"
	"github.com/kasuganosora/thinkbot/util/idgen"
	"github.com/kasuganosora/thinkbot/util/strutil"
)

// volatileMetricRe 匹配易失的项目指标/自指进度汇报，规则降级路径据此跳过。
var volatileMetricRe = regexp.MustCompile(`\d+\s*(个|文件|测试|行|次|files?|tests?|builds?)|(created|passed|测试文件|编译通过|编译失败|build\s*(pass|fail)|CI)`)

// ============================================================================
// Phase 1: Light Sleep — 摄取 + 去重
// ============================================================================

type lightResult struct {
	ingested int
	deduped  int
	dropped  int
}

type rawSnippet struct {
	content  string
	sourceID string
	scope    Scope
	// speaker 标记该片段的说话人："user"=用户原话，"assistant"=bot 自身回复，
	// ""=未知。用于防止 dreaming 把 bot 自己的发言误归为用户事实。
	speaker string
}

// runLight 执行浅睡眠：从 L0 摄取 → LLM 提取候选 → Jaccard 去重 → 暂存。
func (d *DreamManager) runLight(ctx context.Context, scopes []Scope) (*lightResult, error) {
	ctx, span := d.tracer.Start(ctx, "memory.dreaming.light")
	defer span.End()

	d.logger.Debug("dreaming: light phase started")

	cutoff := time.Now().AddDate(0, 0, -d.config.Light.LookbackDays)
	var snippets []rawSnippet
	// 按 scope 追踪哪些 L0 条目被处理，用于标记
	type scopeWithIDs struct {
		scope Scope
		ids   []string
	}
	var processedScopes []scopeWithIDs

	for _, scope := range scopes {
		// 直接拉取该 scope 的全部 L0 条目（含已被实时 Consolidator 提升为 L1 的），
		// 而不用 GetUnprocessed（它会跳过 consolidated 标记）——否则实时 Consolidator
		// 把 L0 消费掉后，03:00 的 dreaming 会因无未处理 L0 而永远空跑。
		// limit=10000 模拟无限制（默认 50 不足以支持梦境全量提取）。
		l0Entries, err := d.manager.store.Retrieve(ctx, Tier0Working, []Scope{scope}, 10000)
		if err != nil {
			d.logger.Warnw("dreaming light: failed to get L0",
				"scope", scope.Key(), "err", err)
			continue
		}
		var ids []string
		for _, e := range l0Entries {
			if e.IsExpired(time.Now()) {
				continue
			}
			if e.CreatedAt.Before(cutoff) {
				continue
			}
			// 跳过本梦境管线已处理过的条目（独立标记 dream_processed，与 Consolidator 的
			// consolidated 解耦，互不影响去重）
			if e.Metadata != nil {
				if _, ok := e.Metadata["dream_processed"]; ok {
					continue
				}
			}
			content := strings.TrimSpace(StripThinking(e.Content))
			if content == "" {
				continue
			}
			// 读取说话人标签（note_capture 写入的 speaker 字段）。
			// 缺失时视为未知，交由提取器按规则保守处理。
			spk := ""
			if e.Metadata != nil {
				if v, ok := e.Metadata["speaker"].(string); ok {
					spk = v
				}
			}
			snippets = append(snippets, rawSnippet{
				content: content, sourceID: e.ID, scope: scope, speaker: spk,
			})
			ids = append(ids, e.ID)
		}
		if len(ids) > 0 {
			processedScopes = append(processedScopes, scopeWithIDs{scope: scope, ids: ids})
		}
	}

	if len(snippets) == 0 {
		return &lightResult{}, nil
	}

	// 按 scope 分组，避免跨 scope 混淆（channel vs user）
	candidates := d.extractCandidatesGrouped(ctx, snippets)
	deduped := jaccardDedup(candidates, d.config.JaccardThreshold)
	dropped := len(candidates) - len(deduped)

	if max := d.config.Light.MaxCandidates; max > 0 && len(deduped) > max {
		deduped = deduped[:max]
	}

	// 标记已处理的 L0 条目（避免下次 cron 重复提取）。
	// 使用独立的 MarkDreamProcessed（写入 metadata["dream_processed"]），
	// 与 Consolidator 的 consolidated 标记解耦，互不饿死。
	for _, swi := range processedScopes {
		if err := d.manager.store.MarkDreamProcessed(ctx, swi.scope, swi.ids); err != nil {
			d.logger.Warnw("dreaming light: mark dream processed failed",
				"scope", swi.scope.Key(), "err", err)
		}
	}

	// 合并到 staged candidates
	d.mu.Lock()
	now := time.Now()
	for i := range deduped {
		c := &deduped[i]
		c.LightHits++
		if existing, ok := d.candidates[c.Key]; ok {
			existing.LightHits++
			existing.LastSeen = now
			existing.SourceIDs = appendUnique(existing.SourceIDs, c.SourceIDs...)
		} else {
			if c.FirstSeen.IsZero() {
				c.FirstSeen = now
			}
			c.LastSeen = now
			d.candidates[c.Key] = c
		}
	}
	d.mu.Unlock()

	d.logger.Debugw("dreaming: light complete",
		"ingested", len(snippets), "candidates", len(candidates),
		"deduped", len(deduped), "dropped", dropped)

	return &lightResult{
		ingested: len(snippets),
		deduped:  len(deduped),
		dropped:  dropped,
	}, nil
}

// extractCandidatesGrouped 按 scope 分组后分别提取候选。
// 确保不同 scope 的记忆不会混淆归属（channel scope 事实不会归到 user scope）。
func (d *DreamManager) extractCandidatesGrouped(ctx context.Context, snippets []rawSnippet) []DreamCandidate {
	// 按 scope 分组
	groups := make(map[string][]rawSnippet)
	var scopeOrder []string // 保持稳定顺序
	for _, s := range snippets {
		key := s.scope.Key()
		if _, exists := groups[key]; !exists {
			scopeOrder = append(scopeOrder, key)
		}
		groups[key] = append(groups[key], s)
	}

	var allCandidates []DreamCandidate
	for _, scopeKey := range scopeOrder {
		group := groups[scopeKey]
		candidates := d.extractCandidates(ctx, group)
		allCandidates = append(allCandidates, candidates...)
	}
	return allCandidates
}

// extractCandidates 用 LLM（或降级规则）从原始片段提取候选事实。
func (d *DreamManager) extractCandidates(ctx context.Context, snippets []rawSnippet) []DreamCandidate {
	if d.provider == nil {
		return d.extractCandidatesRuleBased(snippets)
	}

	var sb strings.Builder
	sb.WriteString("Below are conversation and observation records. Extract 5-20 short, atomic, reusable candidate facts.\n")
	sb.WriteString("Keep each one under 100 characters. Filter out small talk, greetings and throwaway debugging.\n")
	sb.WriteString("Write each content value in Chinese (中文).\n")
	sb.WriteString("Output a raw JSON array: [{\"content\":\"...\",\"category\":\"fact|preference|observation\"}]\n\n")
	sb.WriteString("CRITICAL attribution rules (violating these creates false memories):\n")
	sb.WriteString("- Only extract facts the USER explicitly stated or clearly implied in THEIR OWN messages.\n")
	sb.WriteString("- Each snippet is labeled with its speaker. If speaker is \"assistant\", the text is the BOT's own reply.\n")
	sb.WriteString("  NEVER treat the assistant's statements, opinions, or knowledge as the user's. Do NOT conclude\n")
	sb.WriteString("  \"the user knows/likes/is familiar with X\" from an assistant snippet. Skip assistant snippets entirely.\n")
	sb.WriteString("- If speaker is \"user\" or unknown, extract only what that message literally conveys about the user.\n")
	sb.WriteString("- Do not infer the user's familiarity, taste, or evaluation of something merely because the assistant discussed it.\n")
	sb.WriteString("- Ephemeral / self-referential output from the assistant must NOT be promoted. Explicitly skip and never emit:\n")
	sb.WriteString("  * volatile metrics and transient project stats (file counts, test counts, line counts, version/build numbers,\n")
	sb.WriteString("    \"X files created/passed\", progress reports, CI status) — these go stale immediately and are not durable facts.\n")
	sb.WriteString("  * the assistant describing its own work product, plans, or status (\"I created/fixed/ran ...\").\n")
	sb.WriteString("  * trivia with no lasting value (counts, timestamps, one-off task results).\n")
	sb.WriteString("- Prefer durable, reusable context: stable user preferences, persistent project facts, recurring topics, known constraints.\n\n")
	for i, s := range snippets {
		spk := s.speaker
		if spk == "" {
			spk = "unknown"
		}
		fmt.Fprintf(&sb, "--- snippet %d (speaker: %s) ---\n%s\n\n", i+1, spk, strutil.Truncate(s.content, 500))
	}

	maxTokens := d.config.MaxDreamTokens
	if maxTokens <= 0 {
		maxTokens = 4096
	}
	model := d.model
	if model == "" {
		return d.extractCandidatesRuleBased(snippets)
	}
	result, err := d.provider.DoGenerate(llm.WithStatsFeature(ctx, "dream_extract"), llm.GenerateParams{
		Model:     llm.ChatModel(model),
		System:    defaultLightExtractPrompt,
		Messages:  []llm.Message{llm.UserMessage(sb.String())},
		MaxTokens: &maxTokens,
	})
	if err != nil {
		d.logger.Warnw("dreaming light: LLM failed, rule-based fallback", "err", err)
		return d.extractCandidatesRuleBased(snippets)
	}

	var extracted []struct {
		Content  string `json:"content"`
		Category string `json:"category"`
	}
	if err := strutil.ExtractJSON(result.Text, &extracted); err != nil {
		d.logger.Warnw("dreaming light: JSON parse failed, rule-based fallback", "err", err)
		return d.extractCandidatesRuleBased(snippets)
	}

	scope := Scope{}
	allSourceIDs := make([]string, 0, len(snippets))
	if len(snippets) > 0 {
		scope = snippets[0].scope
		for _, s := range snippets {
			allSourceIDs = append(allSourceIDs, s.sourceID)
		}
	}
	out := make([]DreamCandidate, 0, len(extracted))
	for _, e := range extracted {
		content := strings.TrimSpace(e.Content)
		if content == "" {
			continue
		}
		out = append(out, DreamCandidate{
			Key:       normalizeKey(content),
			Content:   content,
			Category:  e.Category,
			SourceIDs: allSourceIDs,
			Scope:     scope,
		})
	}
	return out
}

func (d *DreamManager) extractCandidatesRuleBased(snippets []rawSnippet) []DreamCandidate {
	out := make([]DreamCandidate, 0, len(snippets))
	for _, s := range snippets {
		// 规则降级路径同样排除 bot 自身的回复，避免把 bot 发言误存为用户事实。
		if s.speaker == "assistant" {
			continue
		}
		// 易失指标 / 瞬时进度汇报（如「153 个测试文件已创建」）不值得固化。
		if volatileMetricRe.MatchString(s.content) {
			continue
		}
		if len([]rune(s.content)) < 10 {
			continue
		}
		out = append(out, DreamCandidate{
			Key:       normalizeKey(s.content),
			Content:   s.content,
			Category:  "observation",
			SourceIDs: []string{s.sourceID},
			Scope:     s.scope,
		})
	}
	return out
}

const defaultLightExtractPrompt = `You are a memory extractor, an analysis component that pulls atomic, concrete candidate facts and preferences out of conversation and observation records.

Rules:
1. Extract only information with long-term value.
2. Filter out small talk, greetings, throwaway debugging, and path/ID noise.
3. Keep each item short and specific (≤100 characters).
4. Output a raw JSON array and nothing else.
5. Write each content value in Chinese (中文).`

// ============================================================================
// Phase 2: REM Sleep — 主题提取 + 模式识别
// ============================================================================

type remResult struct {
	themes     int
	candidates int
}

// runREM 执行 REM：主题聚类 → 标记反复出现的候选 → 增强 REM 信号。
func (d *DreamManager) runREM(ctx context.Context) (*remResult, error) {
	ctx, span := d.tracer.Start(ctx, "memory.dreaming.rem")
	defer span.End()

	d.logger.Debug("dreaming: REM phase started")

	d.mu.Lock()
	unthemed := make([]*DreamCandidate, 0, len(d.candidates))
	themed := make([]*DreamCandidate, 0)
	for _, c := range d.candidates {
		// 已晋升的候选不再参与 REM 聚类（与 runDeep 对称），
		// 避免每晚对已固化记忆重复调用 LLM 聚类、并无意义累加 REMHits。
		if c.Promoted {
			continue
		}
		if c.Theme == "" {
			unthemed = append(unthemed, c)
		} else {
			themed = append(themed, c)
		}
	}
	d.mu.Unlock()

	if len(unthemed) == 0 && len(themed) == 0 {
		return &remResult{}, nil
	}

	// 聚类只对新候选（Theme==""）调用一次 LLM；已聚类候选（Theme 已设）复用其既有主题，
	// 不再每晚重新聚类。旧实现每晚对全部未晋升候选重跑聚类，导致主题抖动（themes 3→10）、
	// co-occurrence 判定漂移、REMHits 不稳定，进而使同一批候选的晋升在多个夜晚间反复横跳
	//（"延迟晋升"现象）。稳定主题后，跨夜 co-occurrence 一致，REMHits 可靠累加，晋升可预期。
	themeMap := make(map[string][]*DreamCandidate, len(themed))
	for _, c := range themed {
		themeMap[c.Theme] = append(themeMap[c.Theme], c)
	}
	if len(unthemed) > 0 {
		for _, cl := range d.clusterByTheme(ctx, unthemed) {
			themeMap[cl.tag] = append(themeMap[cl.tag], cl.items...)
		}
	}

	now := time.Now()
	lookback := time.Duration(d.config.REM.LookbackDays) * 24 * time.Hour
	cutoff := now.Add(-lookback)

	for tag, items := range themeMap {
		if len(items) < 2 {
			continue
		}
		for _, item := range items {
			if item.LastSeen.After(cutoff) {
				item.REMHits++
				if item.Theme == "" {
					item.Theme = tag
				}
			}
		}
	}

	d.logger.Debugw("dreaming: REM complete",
		"staged", len(unthemed)+len(themed), "themes", len(themeMap))

	return &remResult{
		themes:     len(themeMap),
		candidates: len(unthemed) + len(themed),
	}, nil
}

type themeCluster struct {
	tag   string
	items []*DreamCandidate
}

func (d *DreamManager) clusterByTheme(ctx context.Context, candidates []*DreamCandidate) []themeCluster {
	if d.provider == nil {
		return d.clusterByCategory(candidates)
	}
	model := d.model
	if model == "" {
		return d.clusterByCategory(candidates)
	}

	var sb strings.Builder
	sb.WriteString("Assign 1-3 theme tags to each candidate memory below. Reuse the exact key you are given.\n")
	sb.WriteString("Output JSON: [{\"key\":\"candidate key\",\"tags\":[\"标签\"]}]\n\n")
	for i, c := range candidates {
		if i >= 50 {
			break
		}
		fmt.Fprintf(&sb, "[key:%s] %s\n", c.Key, strutil.Truncate(c.Content, 100))
	}

	maxTokens := 2048
	if d.config.MaxDreamTokens > 0 && d.config.MaxDreamTokens < 2048 {
		maxTokens = d.config.MaxDreamTokens
	}
	result, err := d.provider.DoGenerate(llm.WithStatsFeature(ctx, "dream_cluster"), llm.GenerateParams{
		Model:     llm.ChatModel(model),
		System:    "You are a memory theme classifier. You assign concise topical tags to memory entries. Write tags in Chinese (中文).",
		Messages:  []llm.Message{llm.UserMessage(sb.String())},
		MaxTokens: &maxTokens,
	})
	if err != nil {
		return d.clusterByCategory(candidates)
	}

	var tagged []struct {
		Key  string   `json:"key"`
		Tags []string `json:"tags"`
	}
	if err := strutil.ExtractJSON(result.Text, &tagged); err != nil {
		return d.clusterByCategory(candidates)
	}

	tagMap := make(map[string][]*DreamCandidate)
	for _, t := range tagged {
		for _, c := range candidates {
			if c.Key == t.Key {
				for _, tag := range t.Tags {
					tagMap[tag] = append(tagMap[tag], c)
				}
			}
		}
	}

	clusters := make([]themeCluster, 0, len(tagMap))
	for tag, items := range tagMap {
		clusters = append(clusters, themeCluster{tag: tag, items: items})
	}
	sort.Slice(clusters, func(i, j int) bool {
		return len(clusters[i].items) > len(clusters[j].items)
	})
	if max := d.config.REM.MaxThemes; max > 0 && len(clusters) > max {
		clusters = clusters[:max]
	}
	return clusters
}

func (d *DreamManager) clusterByCategory(candidates []*DreamCandidate) []themeCluster {
	catMap := make(map[string][]*DreamCandidate)
	for _, c := range candidates {
		cat := c.Category
		if cat == "" {
			cat = "uncategorized"
		}
		catMap[cat] = append(catMap[cat], c)
	}
	clusters := make([]themeCluster, 0, len(catMap))
	for cat, items := range catMap {
		clusters = append(clusters, themeCluster{tag: cat, items: items})
	}
	return clusters
}

// ============================================================================
// Phase 3: Deep Sleep — 评分 + 门控 + 晋升
// ============================================================================

type deepResult struct {
	scored   int
	passed   int
	promoted int
}

// runDeep 执行深睡眠：6 信号评分 → 3 门控筛选 → 写入 L1。
func (d *DreamManager) runDeep(ctx context.Context) (*deepResult, error) {
	ctx, span := d.tracer.Start(ctx, "memory.dreaming.deep")
	defer span.End()

	d.logger.Debug("dreaming: deep phase started")

	now := time.Now()
	maxAge := time.Duration(d.config.Deep.MaxAgeDays) * 24 * time.Hour

	d.mu.Lock()
	staged := make([]*DreamCandidate, 0, len(d.candidates))
	for _, c := range d.candidates {
		if c.Promoted {
			continue
		}
		if !c.FirstSeen.IsZero() && now.Sub(c.FirstSeen) > maxAge {
			continue
		}
		staged = append(staged, c)
	}
	d.mu.Unlock()

	type scoredItem struct {
		candidate *DreamCandidate
		score     float64
	}

	var scored []scoredItem
	for _, c := range staged {
		breakdown := d.scoreCandidate(c, now)
		total := d.computeTotalScore(breakdown, c)
		c.Score = total
		scored = append(scored, scoredItem{candidate: c, score: total})
	}

	sort.Slice(scored, func(i, j int) bool {
		return scored[i].score > scored[j].score
	})

	// 三重门控（召回/查询/主题门控仅当配置 >0 时生效；
	// 默认 0 = 不要求，仅依据分数 + 近期 + 丰富度晋升，
	// 避免白天召回信号缺失时「永不晋升」的死锁）。
	var passed []*DreamCandidate
	for _, sc := range scored {
		if sc.score < d.config.Deep.MinScore {
			continue
		}
		if d.config.Deep.MinRecallCount > 0 && sc.candidate.RecallCount < d.config.Deep.MinRecallCount {
			continue
		}
		if d.config.Deep.MinUniqueQueries > 0 && sc.candidate.UniqueQueries < d.config.Deep.MinUniqueQueries {
			continue
		}
		if d.config.Deep.MinREMHits > 0 && sc.candidate.REMHits < d.config.Deep.MinREMHits {
			continue
		}
		passed = append(passed, sc.candidate)
	}

	if max := d.config.Deep.MaxPromotions; max > 0 && len(passed) > max {
		passed = passed[:max]
	}

	// 写入 L1
	promoted := 0
	for _, c := range passed {
		entry := Entry{
			ID:         idgen.New("dream"),
			Scope:      c.Scope,
			Content:    c.Content,
			Category:   c.Category,
			Source:     "dreaming",
			Importance: c.Score,
			Metadata: map[string]any{
				"dream_score":       c.Score,
				"dream_theme":       c.Theme,
				"dream_light_hits":  c.LightHits,
				"dream_rem_hits":    c.REMHits,
				"dream_promoted_at": now,
			},
		}
		if err := d.manager.WriteLongTerm(ctx, entry, Tier0Working); err != nil {
			d.logger.Warnw("dreaming deep: promote failed",
				"key", c.Key, "err", err)
			continue
		}
		d.mu.Lock()
		c.Promoted = true
		d.mu.Unlock()
		promoted++
	}

	d.logger.Debugw("dreaming: deep complete",
		"scored", len(scored), "passed", len(passed), "promoted", promoted)

	return &deepResult{
		scored:   len(scored),
		passed:   len(passed),
		promoted: promoted,
	}, nil
}

// ============================================================================
// Scoring — 6 信号加权评分
// ============================================================================

// scoreCandidate 计算各信号子分数（0.0~1.0）。
func (d *DreamManager) scoreCandidate(c *DreamCandidate, now time.Time) ScoreBreakdown {
	var sb ScoreBreakdown

	// Relevance: 基于召回质量
	if c.RecallCount > 0 {
		sb.Relevance = minF(1.0, float64(c.RecallCount)/5.0)
	}

	// Frequency: 基于 Light 命中频次
	if c.LightHits > 0 {
		sb.Frequency = minF(1.0, float64(c.LightHits)/5.0)
	}

	// Diversity: 基于不同查询数
	if c.UniqueQueries > 0 {
		sb.Diversity = minF(1.0, float64(c.UniqueQueries)/5.0)
	}

	// Recency: 时间衰减（半衰期模型）
	if !c.LastSeen.IsZero() {
		halfLife := float64(d.config.Deep.RecencyHalfLifeDays) * 24 // hours
		if halfLife <= 0 {
			halfLife = 14 * 24
		}
		ageHours := now.Sub(c.LastSeen).Hours()
		sb.Recency = 1.0 - ageHours/halfLife
		if sb.Recency < 0 {
			sb.Recency = 0
		}
	}

	// Consolidation: 基于 REM 命中（跨多次梦境出现）
	if c.REMHits > 0 {
		sb.Consolidation = minF(1.0, float64(c.REMHits)/3.0)
	}

	// Richness: 基于内容长度和具体性
	contentLen := len([]rune(c.Content))
	if contentLen >= 10 && contentLen <= 100 {
		sb.Richness = 1.0
	} else if contentLen > 100 {
		sb.Richness = 0.7
	} else if contentLen >= 5 {
		sb.Richness = 0.4
	}

	return sb
}

// computeTotalScore 计算加权总分 + 相位增强。
func (d *DreamManager) computeTotalScore(sb ScoreBreakdown, c *DreamCandidate) float64 {
	total := sb.Relevance*WeightRelevance +
		sb.Frequency*WeightFrequency +
		sb.Diversity*WeightDiversity +
		sb.Recency*WeightRecency +
		sb.Consolidation*WeightConsolidation +
		sb.Richness*WeightRichness

	// 相位增强（衰减式）
	if c.LightHits > 0 {
		total += minF(LightEnhanceCap, LightEnhanceCap/float64(c.LightHits))
	}
	if c.REMHits > 0 {
		total += minF(REMEnhanceCap, REMEnhanceCap/float64(c.REMHits))
	}

	return minF(1.0, total)
}

// ============================================================================
// Helpers
// ============================================================================

// jaccardDedup 对候选列表进行 Jaccard 相似度去重。
func jaccardDedup(candidates []DreamCandidate, threshold float64) []DreamCandidate {
	if len(candidates) <= 1 {
		return candidates
	}
	// 预计算 token 集合
	tokenSets := make([]map[string]bool, len(candidates))
	for i, c := range candidates {
		tokenSets[i] = tokenize(c.Content)
	}

	keep := []int{0}
	for i := 1; i < len(candidates); i++ {
		dup := false
		for _, j := range keep {
			if jaccardSimilarity(tokenSets[i], tokenSets[j]) >= threshold {
				dup = true
				break
			}
		}
		if !dup {
			keep = append(keep, i)
		}
	}

	out := make([]DreamCandidate, len(keep))
	for idx, origIdx := range keep {
		out[idx] = candidates[origIdx]
	}
	return out
}

// jaccardSimilarity 计算两个 token 集合的 Jaccard 相似度。
func jaccardSimilarity(a, b map[string]bool) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	intersect := 0
	for k := range a {
		if b[k] {
			intersect++
		}
	}
	union := len(a) + len(b) - intersect
	if union == 0 {
		return 0
	}
	return float64(intersect) / float64(union)
}

// tokenize 将文本切分为 token 集合（用于 Jaccard 计算）。
func tokenize(text string) map[string]bool {
	text = strings.ToLower(text)
	tokens := strings.FieldsFunc(text, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n' || r == ',' ||
			r == '.' || r == '!' || r == '?' || r == ';' || r == ':' ||
			r == '/' || r == '\\' || r == '(' || r == ')'
	})
	set := make(map[string]bool, len(tokens))
	for _, t := range tokens {
		if len(t) >= 2 {
			set[t] = true
		}
	}
	return set
}

// normalizeKey 生成候选的去重键。
func normalizeKey(content string) string {
	content = strings.ToLower(strings.TrimSpace(content))
	content = strings.Join(strings.Fields(content), " ")
	if len(content) > 80 {
		content = content[:80]
	}
	return content
}

// appendUnique 向切片追加唯一元素。
func appendUnique(slice []string, values ...string) []string {
	seen := make(map[string]bool, len(slice))
	for _, s := range slice {
		seen[s] = true
	}
	for _, v := range values {
		if !seen[v] {
			slice = append(slice, v)
			seen[v] = true
		}
	}
	return slice
}

// minF 返回两个 float64 中的较小值。
// Go 1.21+ 有 builtin min()，但此处保留以确保 float64 语义一致。
func minF(a, b float64) float64 {
	return min(a, b)
}
