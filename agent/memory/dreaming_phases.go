package memory

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/kasuganosora/thinkbot/llm"
	"github.com/kasuganosora/thinkbot/util/idgen"
	"github.com/kasuganosora/thinkbot/util/strutil"
)

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
}

// runLight 执行浅睡眠：从 L0 摄取 → LLM 提取候选 → Jaccard 去重 → 暂存。
func (d *DreamManager) runLight(ctx context.Context, scopes []Scope) (*lightResult, error) {
	ctx, span := d.tracer.Start(ctx, "memory.dreaming.light")
	defer span.End()

	d.logger.Debug("dreaming: light phase started")

	cutoff := time.Now().AddDate(0, 0, -d.config.Light.LookbackDays)
	var snippets []rawSnippet

	for _, scope := range scopes {
		l0Entries, err := d.manager.store.GetAll(ctx, Tier0Working, scope)
		if err != nil {
			d.logger.Warnw("dreaming light: failed to get L0",
				"scope", scope.Key(), "err", err)
			continue
		}
		for _, e := range l0Entries {
			if e.CreatedAt.Before(cutoff) {
				continue
			}
			content := strings.TrimSpace(StripThinking(e.Content))
			if content == "" {
				continue
			}
			snippets = append(snippets, rawSnippet{
				content: content, sourceID: e.ID, scope: scope,
			})
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
	sb.WriteString("以下是对话/观察记录。请提取 5-20 条简短、原子、可复用的候选事实。\n")
	sb.WriteString("每条不超过 100 字。过滤闲聊、问候、临时调试。\n")
	sb.WriteString("输出纯 JSON 数组: [{\"content\":\"...\",\"category\":\"fact|preference|observation\"}]\n\n")
	for i, s := range snippets {
		fmt.Fprintf(&sb, "--- 片段 %d ---\n%s\n\n", i+1, strutil.Truncate(s.content, 500))
	}

	maxTokens := d.config.MaxDreamTokens
	if maxTokens <= 0 {
		maxTokens = 4096
	}
	result, err := d.provider.DoGenerate(ctx, llm.GenerateParams{
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

const defaultLightExtractPrompt = `你是一个记忆提取助手。从对话和观察记录中提取原子、具体的候选事实和偏好。

规则：
1. 只提取有长期价值的信息
2. 过滤闲聊、问候、临时调试、路径/ID 噪音
3. 每条简短具体（≤100字）
4. 输出纯 JSON 数组`

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
	staged := make([]*DreamCandidate, 0, len(d.candidates))
	for _, c := range d.candidates {
		staged = append(staged, c)
	}
	d.mu.Unlock()

	if len(staged) == 0 {
		return &remResult{}, nil
	}

	themes := d.clusterByTheme(ctx, staged)

	now := time.Now()
	lookback := time.Duration(d.config.REM.LookbackDays) * 24 * time.Hour
	cutoff := now.Add(-lookback)

	for _, theme := range themes {
		if len(theme.items) < 2 {
			continue
		}
		for _, item := range theme.items {
			if item.LastSeen.After(cutoff) {
				item.REMHits++
				if item.Theme == "" {
					item.Theme = theme.tag
				}
			}
		}
	}

	d.logger.Debugw("dreaming: REM complete",
		"staged", len(staged), "themes", len(themes))

	return &remResult{
		themes:     len(themes),
		candidates: len(staged),
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

	var sb strings.Builder
	sb.WriteString("为以下候选记忆分配 1-3 个主题标签。输出 JSON: [{\"key\":\"候选key\",\"tags\":[\"标签\"]}]\n\n")
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
	result, err := d.provider.DoGenerate(ctx, llm.GenerateParams{
		System:    "你是记忆主题分类助手。",
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

	// 三重门控
	var passed []*DreamCandidate
	for _, sc := range scored {
		if sc.score < d.config.Deep.MinScore {
			continue
		}
		if sc.candidate.RecallCount < d.config.Deep.MinRecallCount {
			continue
		}
		if sc.candidate.UniqueQueries < d.config.Deep.MinUniqueQueries {
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
		sb.Recency = 0.5 * (1.0 - ageHours/halfLife)
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
