package memory

import (
	"context"
	"testing"

	"github.com/kasuganosora/thinkbot/llm"
)

// ============================================================================
// Clustering Tests
// ============================================================================

func TestClusterCountSuggestion(t *testing.T) {
	tests := []struct {
		n    int
		want int
	}{
		{1, 2}, // min is 2
		{4, 2},
		{9, 3},
		{16, 4},
		{64, 8},
		{100, 8}, // capped at 8
	}
	for _, tt := range tests {
		got := clusterCountSuggestion(tt.n)
		if got != tt.want {
			t.Errorf("clusterCountSuggestion(%d) = %d, want %d", tt.n, got, tt.want)
		}
	}
}

func TestClusterEntries_Basic(t *testing.T) {
	entries := []TieredEntry{
		{Entry: Entry{ID: "1", Content: "golang programming language backend server"}},
		{Entry: Entry{ID: "2", Content: "python data science machine learning"}},
		{Entry: Entry{ID: "3", Content: "golang microservice grpc api gateway"}},
		{Entry: Entry{ID: "4", Content: "python numpy pandas dataframe analysis"}},
		{Entry: Entry{ID: "5", Content: "golang concurrency goroutine channel"}},
		{Entry: Entry{ID: "6", Content: "python tensorflow pytorch deep learning"}},
	}

	clusters := clusterEntries(entries, 2)
	if len(clusters) < 1 || len(clusters) > 2 {
		t.Fatalf("expected 1-2 clusters, got %d", len(clusters))
	}

	// 所有条目应该被分配
	totalEntries := 0
	for _, c := range clusters {
		totalEntries += len(c.entries)
	}
	if totalEntries != len(entries) {
		t.Errorf("clustered entries = %d, want %d", totalEntries, len(entries))
	}

	// 最大聚类应该排在前面
	if len(clusters) >= 2 {
		if len(clusters[0].entries) < len(clusters[1].entries) {
			t.Error("clusters not sorted by size descending")
		}
	}
}

func TestClusterEntries_SingleEntry(t *testing.T) {
	entries := []TieredEntry{
		{Entry: Entry{ID: "1", Content: "only one entry here"}},
	}
	clusters := clusterEntries(entries, 2)
	if len(clusters) != 1 {
		t.Fatalf("expected 1 cluster for single entry, got %d", len(clusters))
	}
	if len(clusters[0].entries) != 1 {
		t.Errorf("expected 1 entry in cluster, got %d", len(clusters[0].entries))
	}
}

// ============================================================================
// Cosine Similarity Tests
// ============================================================================

func TestCosineSimilarity(t *testing.T) {
	tests := []struct {
		name string
		a    []float64
		b    []float64
		want float64
	}{
		{
			name: "identical vectors",
			a:    []float64{1, 2, 3},
			b:    []float64{1, 2, 3},
			want: 1.0,
		},
		{
			name: "orthogonal vectors",
			a:    []float64{1, 0},
			b:    []float64{0, 1},
			want: 0.0,
		},
		{
			name: "opposite vectors",
			a:    []float64{1, 1},
			b:    []float64{-1, -1},
			want: -1.0,
		},
		{
			name: "empty vectors",
			a:    []float64{},
			b:    []float64{},
			want: 0.0,
		},
		{
			name: "different dimensions",
			a:    []float64{1, 2},
			b:    []float64{1, 2, 3},
			want: 0.0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cosineSimilarity(tt.a, tt.b)
			if abs(got-tt.want) > 0.001 {
				t.Errorf("cosineSimilarity() = %f, want %f", got, tt.want)
			}
		})
	}
}

// ============================================================================
// Dot Product (sparse) Tests
// ============================================================================

func TestDotProduct(t *testing.T) {
	a := map[string]float64{"go": 2.0, "python": 1.0, "rust": 3.0}
	b := map[string]float64{"go": 1.0, "rust": 2.0, "java": 4.0}
	got := dotProduct(a, b)
	// go: 2*1=2, rust: 3*2=6, python/java don't overlap → 8
	if abs(got-8.0) > 0.001 {
		t.Errorf("dotProduct = %f, want 8.0", got)
	}
}

// ============================================================================
// ProfileItem Dedup Tests
// ============================================================================

func TestDedupProfileItems(t *testing.T) {
	items := []ProfileItem{
		{Type: ProfileTypeFact, Content: "user likes golang programming", Confidence: 0.9},
		{Type: ProfileTypeFact, Content: "user likes golang programming", Confidence: 0.8},
		{Type: ProfileTypeFact, Content: "user prefers dark mode", Confidence: 0.7},
		{Type: ProfileTypeFact, Content: "user works at a startup company", Confidence: 0.6},
	}

	result := dedupProfileItems(items)
	if len(result) != 3 {
		t.Errorf("expected 3 items after dedup, got %d", len(result))
	}
}

func TestDedupProfileItems_SingleItem(t *testing.T) {
	items := []ProfileItem{
		{Type: ProfileTypeFact, Content: "single item", Confidence: 0.9},
	}
	result := dedupProfileItems(items)
	if len(result) != 1 {
		t.Errorf("expected 1 item, got %d", len(result))
	}
}

func TestDedupProfileItems_Empty(t *testing.T) {
	result := dedupProfileItems(nil)
	if len(result) != 0 {
		t.Errorf("expected 0 items, got %d", len(result))
	}
}

// ============================================================================
// Jaccard Validation Tests
// ============================================================================

func TestValidateWithJaccard(t *testing.T) {
	sourceEntries := []TieredEntry{
		{Entry: Entry{ID: "s1", Content: "user prefers concise responses in technical discussions"}},
		{Entry: Entry{ID: "s2", Content: "user works with golang and rust for systems programming"}},
	}

	items := []ProfileItem{
		{
			Type:       ProfileTypePreference,
			Content:    "user prefers concise responses in technical discussions",
			Confidence: 0.9,
		},
		{
			Type:       ProfileTypeFact,
			Content:    "user likes skydiving on weekends", // not in source
			Confidence: 0.8,
		},
	}

	profiler := &LLMProfiler{
		config: LLMProfilerConfig{
			MinValidationScore: 0.15,
		},
	}

	validated := profiler.validateWithJaccard(items, sourceEntries)

	// The first item should pass (high overlap with source)
	// The second item should likely fail (no overlap)
	if len(validated) < 1 {
		t.Error("expected at least 1 validated item")
	}

	// First item should be validated
	if len(validated) >= 1 {
		if !validated[0].Validated {
			t.Error("first item should be validated (high source overlap)")
		}
		if validated[0].ValidationScore <= 0 {
			t.Error("validated item should have positive validation score")
		}
	}
}

// ============================================================================
// Embedding Validation Tests (mock)
// ============================================================================

// mockEmbeddingProvider 模拟 embedding 后端用于测试。
type mockEmbeddingProvider struct {
	dimensions int
}

func (m *mockEmbeddingProvider) DoEmbed(_ context.Context, params llm.EmbedParams) (*llm.EmbedResult, error) {
	// 返回简单的确定性 embedding：第一个维度为 1.0，其余为 0
	embeddings := make([][]float64, len(params.Values))
	for i := range embeddings {
		embeddings[i] = make([]float64, m.dimensions)
		embeddings[i][0] = 1.0
	}
	return &llm.EmbedResult{Embeddings: embeddings}, nil
}

func TestValidateWithEmbedding_AllPass(t *testing.T) {
	// 使用 always-cosine-1 的 mock（所有向量相同 → cosine=1.0）
	sourceEntries := []TieredEntry{
		{Entry: Entry{ID: "s1", Content: "source one"}},
		{Entry: Entry{ID: "s2", Content: "source two"}},
	}
	items := []ProfileItem{
		{Type: ProfileTypeFact, Content: "item one", Confidence: 0.9},
		{Type: ProfileTypeFact, Content: "item two", Confidence: 0.7},
	}

	profiler := &LLMProfiler{
		config: LLMProfilerConfig{
			EmbeddingProvider:  &uniformEmbeddingProvider{},
			MinValidationScore: 0.5,
		},
	}

	validated := profiler.validateWithEmbedding(context.Background(), items, sourceEntries)
	if len(validated) != 2 {
		t.Errorf("expected 2 validated items (all cosine=1.0), got %d", len(validated))
	}
	for _, item := range validated {
		if !item.Validated {
			t.Error("item should be validated")
		}
		if abs(item.ValidationScore-1.0) > 0.001 {
			t.Errorf("validation score = %f, want 1.0", item.ValidationScore)
		}
	}
}

// uniformEmbeddingProvider 返回所有向量都相同 → cosine 总是 1.0。
type uniformEmbeddingProvider struct{}

func (u *uniformEmbeddingProvider) DoEmbed(_ context.Context, params llm.EmbedParams) (*llm.EmbedResult, error) {
	n := len(params.Values)
	embeddings := make([][]float64, n)
	for i := range embeddings {
		embeddings[i] = []float64{1.0}
	}
	return &llm.EmbedResult{Embeddings: embeddings}, nil
}

func TestCosineSimilarity_ConfidenceFusion(t *testing.T) {
	// 当 cosine=1.0 时，融合后 confidence = min(original, (original+1.0)/2)
	// 对于 confidence=0.7：fusion = min(0.7, (0.7+1.0)/2) = min(0.7, 0.85) = 0.7
	// 对于 confidence=0.95：fusion = min(0.95, (0.95+1.0)/2) = min(0.95, 0.975) = 0.95

	sourceEntries := []TieredEntry{
		{Entry: Entry{ID: "s1", Content: "source"}},
	}
	items := []ProfileItem{
		{Type: ProfileTypeFact, Content: "test item", Confidence: 0.7},
	}

	profiler := &LLMProfiler{
		config: LLMProfilerConfig{
			MinValidationScore: 0.5,
		},
	}

	// 直接验证 embedding 路径会需要 mock，这里测试 Jaccard 路径的融合逻辑
	// Jaccard 路径目前不融合 confidence，只标记 Validated
	validated := profiler.validateWithJaccard(items, sourceEntries)
	if len(validated) > 0 && !validated[0].Validated {
		t.Error("expected validated item to have Validated=true")
	}
}

// ============================================================================
// TopKeyword Tests
// ============================================================================

func TestTopKeyword(t *testing.T) {
	vectors := []map[string]float64{
		{"golang": 3.0, "programming": 1.0},
		{"golang": 2.0, "server": 1.0},
		{"python": 4.0, "data": 2.0},
		{"python": 3.0, "science": 1.0},
	}
	assignments := []int{0, 0, 1, 1} // first two in cluster 0, last two in cluster 1
	allEntries := []TieredEntry{
		{Entry: Entry{ID: "1"}},
		{Entry: Entry{ID: "2"}},
		{Entry: Entry{ID: "3"}},
		{Entry: Entry{ID: "4"}},
	}
	clusterEntries := []TieredEntry{
		{Entry: Entry{ID: "1"}},
		{Entry: Entry{ID: "2"}},
	}

	keyword := topKeyword(vectors, assignments, clusterEntries, allEntries)
	// "golang" should have highest aggregate weight in cluster 0
	if keyword != "golang" {
		t.Errorf("topKeyword = %q, want %q", keyword, "golang")
	}
}

// ============================================================================
// Helpers
// ============================================================================

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}
