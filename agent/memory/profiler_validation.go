package memory

import (
	"context"
	"math"
	"sort"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/kasuganosora/thinkbot/llm"
)

// ============================================================================
// Profile Validation — 语义一致性验证
//
// 受 Persona Ecosystem Playground (arXiv:2603.03140) 的验证框架启发：
// 提取的画像必须在语义上显著接近其源记忆。
//
// 双重验证策略：
//   1. 如果配置了 EmbeddingProvider → 使用 cosine 相似度（精确）
//   2. 否则降级使用 Jaccard token 相似度（近似，无外部依赖）
//
// 验证逻辑：
//   - 计算画像 vs 所有源记忆的平均相似度（self-similarity）
//   - 画像的 self-similarity 必须达到 MinValidationScore 阈值
//   - 通过验证的画像标记 Validated=true 并记录 ValidationScore
// ============================================================================

// validateItems 对提取的画像做语义一致性验证。
// 过滤掉与源记忆不一致的低质量画像。
func (p *LLMProfiler) validateItems(ctx context.Context, items []ProfileItem, sourceEntries []TieredEntry) []ProfileItem {
	if len(items) == 0 || len(sourceEntries) == 0 {
		return items
	}

	_, span := p.tracer.Start(ctx, "memory.profile.validate",
		trace.WithAttributes(attribute.Int("items_input", len(items))))
	defer span.End()

	// 选择验证策略
	if p.config.EmbeddingProvider != nil {
		validated := p.validateWithEmbedding(ctx, items, sourceEntries)
		span.SetAttributes(attribute.String("strategy", "embedding"))
		span.SetAttributes(attribute.Int("items_validated", len(validated)))
		return validated
	}

	validated := p.validateWithJaccard(items, sourceEntries)
	span.SetAttributes(attribute.String("strategy", "jaccard"))
	span.SetAttributes(attribute.Int("items_validated", len(validated)))
	return validated
}

// validateWithEmbedding 使用 embedding cosine 相似度验证画像质量。
func (p *LLMProfiler) validateWithEmbedding(ctx context.Context, items []ProfileItem, sourceEntries []TieredEntry) []ProfileItem {
	// 批量获取 embedding
	texts := make([]string, 0, len(items)+len(sourceEntries))
	for _, item := range items {
		texts = append(texts, item.Content)
	}
	for _, e := range sourceEntries {
		texts = append(texts, e.Content)
	}

	embedResult, err := p.config.EmbeddingProvider.DoEmbed(ctx, llm.EmbedParams{
		Model:  p.config.EmbeddingModel,
		Values: texts,
	})
	if err != nil {
		p.logger.Warnw("profiler: embedding validation failed, falling back to jaccard", "err", err)
		return p.validateWithJaccard(items, sourceEntries)
	}

	if len(embedResult.Embeddings) != len(texts) {
		p.logger.Warnw("profiler: embedding count mismatch, falling back to jaccard",
			"expected", len(texts), "got", len(embedResult.Embeddings))
		return p.validateWithJaccard(items, sourceEntries)
	}

	// 分割 embedding
	itemEmbeds := embedResult.Embeddings[:len(items)]
	sourceEmbeds := embedResult.Embeddings[len(items):]

	// 计算每个画像与所有源记忆的最大相似度（取 max 而非 avg，因为画像可能只对应部分源记忆）
	var validated []ProfileItem
	for i, item := range items {
		maxSim := 0.0
		for _, srcEmb := range sourceEmbeds {
			sim := cosineSimilarity(itemEmbeds[i], srcEmb)
			if sim > maxSim {
				maxSim = sim
			}
		}

		// 验证通过条件：最大相似度 >= minScore
		// 同时用相似度调整置信度：validated confidence = min(original, maxSim)
		score := maxSim
		if score >= p.config.MinValidationScore {
			item.Validated = true
			item.ValidationScore = score
			// 融合 LLM 置信度和验证分数
			item.Confidence = math.Min(item.Confidence, (item.Confidence+score)/2)
			validated = append(validated, item)
		}
	}

	return validated
}

// validateWithJaccard 使用 Jaccard token 相似度验证画像质量（无 embedding 依赖的降级方案）。
func (p *LLMProfiler) validateWithJaccard(items []ProfileItem, sourceEntries []TieredEntry) []ProfileItem {
	// 预计算源记忆 token 集合
	sourceTokens := make([]map[string]bool, len(sourceEntries))
	for i, e := range sourceEntries {
		sourceTokens[i] = tokenize(strings.ToLower(e.Content))
	}

	var validated []ProfileItem
	for _, item := range items {
		itemTokens := tokenize(strings.ToLower(item.Content))
		if len(itemTokens) == 0 {
			continue
		}

		// 计算与所有源记忆的最大 Jaccard 相似度
		maxSim := 0.0
		for _, srcTokens := range sourceTokens {
			sim := jaccardSimilarity(itemTokens, srcTokens)
			if sim > maxSim {
				maxSim = sim
			}
		}

		// Jaccard 分数通常较低，使用更低的阈值
		threshold := p.config.MinValidationScore
		if threshold > 0.1 {
			threshold = 0.1 // Jaccard 稀疏，降阈值
		}
		if maxSim >= threshold {
			item.Validated = true
			item.ValidationScore = maxSim
			validated = append(validated, item)
		}
	}

	return validated
}

// ============================================================================
// Clustering — TF-IDF 聚类辅助画像提取
//
// 灵感来源：Persona Ecosystem Playground 的 k-means 聚类思路。
// 先发现记忆中的自然主题分组，然后按组提取画像，避免主题混叠。
//
// 实现策略：
//   - 构建 TF-IDF 向量（基于 token 频率）
//   - 使用 k-means 聚类
//   - 每个聚类取频率最高的 token 作为关键词
// ============================================================================

// profileCluster 表示一个记忆聚类。
type profileCluster struct {
	// keyword 该聚类的主题关键词（频率最高的 token）。
	keyword string
	// entries 属于该聚类的记忆条目。
	entries []TieredEntry
}

// clusterCountSuggestion 根据 L1 条目数建议聚类数。
// 规则：sqrt(N)，上限 8，最小 2。
func clusterCountSuggestion(n int) int {
	k := int(math.Sqrt(float64(n)))
	if k < 2 {
		k = 2
	}
	if k > 8 {
		k = 8
	}
	return k
}

// clusterEntries 对 L1 条目做 TF-IDF 聚类。
// 返回按大小降序排列的聚类列表。
func clusterEntries(entries []TieredEntry, k int) []profileCluster {
	if len(entries) <= 1 {
		return []profileCluster{{entries: entries}}
	}
	if k < 1 {
		k = 2
	}
	if k > len(entries) {
		k = len(entries)
	}

	// 1. 构建 TF-IDF 向量
	docs := make([]map[string]float64, len(entries))
	df := make(map[string]int) // document frequency

	for i, e := range entries {
		tokens := tokenize(strings.ToLower(e.Content))
		tf := make(map[string]float64)
		for tok := range tokens {
			tf[tok]++
		}
		// normalize TF
		total := float64(len(tokens))
		if total > 0 {
			for tok := range tf {
				tf[tok] /= total
			}
		}
		docs[i] = tf
		for tok := range tf {
			df[tok]++
		}
	}

	// IDF
	n := float64(len(entries))
	idf := make(map[string]float64)
	for tok, freq := range df {
		idf[tok] = math.Log(n / float64(freq))
	}

	// TF-IDF
	vectors := make([]map[string]float64, len(docs))
	for i, tf := range docs {
		vec := make(map[string]float64, len(tf))
		for tok, v := range tf {
			vec[tok] = v * idf[tok]
		}
		vectors[i] = vec
	}

	// 2. K-means 聚类
	assignments := kmeansTFIDF(vectors, k)

	// 3. 组装聚类结果
	clusterEntries := make([][]TieredEntry, k)
	for i, c := range assignments {
		clusterEntries[c] = append(clusterEntries[c], entries[i])
	}

	// 4. 为每个聚类提取关键词
	clusters := make([]profileCluster, 0, k)
	for _, ce := range clusterEntries {
		if len(ce) == 0 {
			continue
		}
		keyword := topKeyword(vectors, assignments, ce, entries)
		clusters = append(clusters, profileCluster{
			keyword: keyword,
			entries: ce,
		})
	}

	// 按大小降序
	sort.Slice(clusters, func(i, j int) bool {
		return len(clusters[i].entries) > len(clusters[j].entries)
	})

	return clusters
}

// kmeansTFIDF 在 TF-IDF 向量上执行 k-means 聚类。
// 返回每个文档的聚类分配索引。
func kmeansTFIDF(vectors []map[string]float64, k int) []int {
	n := len(vectors)
	if n == 0 {
		return nil
	}

	// 初始化：用前 k 个文档作为初始质心（简化版 k-means++）
	centroids := make([]map[string]float64, k)
	for i := 0; i < k && i < n; i++ {
		centroids[i] = copyVec(vectors[i])
	}
	// 如果文档数 < k，剩余质心用空向量
	for i := n; i < k; i++ {
		centroids[i] = make(map[string]float64)
	}

	assignments := make([]int, n)
	for iter := 0; iter < 10; iter++ {
		changed := false

		// 分配步骤
		for i, vec := range vectors {
			bestC := 0
			bestSim := -1.0
			for c, centroid := range centroids {
				sim := dotProduct(vec, centroid)
				if sim > bestSim {
					bestSim = sim
					bestC = c
				}
			}
			if assignments[i] != bestC {
				assignments[i] = bestC
				changed = true
			}
		}

		if !changed && iter > 0 {
			break
		}

		// 更新步骤：重新计算质心（平均向量）
		newCentroids := make([]map[string]float64, k)
		counts := make([]int, k)
		for i := range newCentroids {
			newCentroids[i] = make(map[string]float64)
		}
		for i, vec := range vectors {
			c := assignments[i]
			counts[c]++
			for tok, v := range vec {
				newCentroids[c][tok] += v
			}
		}
		for c := range newCentroids {
			if counts[c] > 0 {
				for tok := range newCentroids[c] {
					newCentroids[c][tok] /= float64(counts[c])
				}
			}
		}
		centroids = newCentroids
	}

	return assignments
}

// topKeyword 找出一个聚类中 TF-IDF 权重最高的 token。
func topKeyword(vectors []map[string]float64, assignments []int, clusterEntries []TieredEntry, allEntries []TieredEntry) string {
	// 构建该聚类的索引集合
	clusterIdx := make(map[int]bool)
	for _, ce := range clusterEntries {
		for i, e := range allEntries {
			if e.ID == ce.ID {
				clusterIdx[i] = true
				break
			}
		}
	}

	// 聚合该聚类的 TF-IDF 权重
	scores := make(map[string]float64)
	for i := range vectors {
		if !clusterIdx[i] {
			continue
		}
		for tok, v := range vectors[i] {
			scores[tok] += v
		}
	}

	// 找权重最高的 token
	bestTok := ""
	bestScore := 0.0
	for tok, score := range scores {
		if len(tok) < 3 {
			continue // 跳过太短的 token
		}
		if score > bestScore {
			bestScore = score
			bestTok = tok
		}
	}

	return bestTok
}

// ============================================================================
// Helpers
// ============================================================================

// cosineSimilarity 计算两个向量的余弦相似度。
func cosineSimilarity(a, b []float64) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, magA, magB float64
	for i := range a {
		dot += a[i] * b[i]
		magA += a[i] * a[i]
		magB += b[i] * b[i]
	}
	if magA == 0 || magB == 0 {
		return 0
	}
	return dot / (math.Sqrt(magA) * math.Sqrt(magB))
}

// dotProduct 计算稀疏向量（map）的点积。
func dotProduct(a, b map[string]float64) float64 {
	if len(a) > len(b) {
		a, b = b, a
	}
	var sum float64
	for tok, v := range a {
		if bv, ok := b[tok]; ok {
			sum += v * bv
		}
	}
	return sum
}

// copyVec 复制一个稀疏向量 map。
func copyVec(src map[string]float64) map[string]float64 {
	dst := make(map[string]float64, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

// dedupProfileItems 对画像条目做 Jaccard 去重。
func dedupProfileItems(items []ProfileItem) []ProfileItem {
	if len(items) <= 1 {
		return items
	}

	tokenSets := make([]map[string]bool, len(items))
	for i, item := range items {
		tokenSets[i] = tokenize(strings.ToLower(item.Content))
	}

	keep := []int{0}
	for i := 1; i < len(items); i++ {
		dup := false
		for _, j := range keep {
			if jaccardSimilarity(tokenSets[i], tokenSets[j]) >= 0.7 {
				dup = true
				break
			}
		}
		if !dup {
			keep = append(keep, i)
		}
	}

	out := make([]ProfileItem, len(keep))
	for idx, origIdx := range keep {
		out[idx] = items[origIdx]
	}
	return out
}
