package llm

import (
	"context"
	"sync"
	"testing"
)

// ============================================================================
// mockProvider — 模拟 LLM provider
// ============================================================================

type mockProvider struct {
	name       string
	genResult  *GenerateResult
	genErr     error
	streamErr  error
	streamPart StreamPart // sent as the single stream part
}

func (p *mockProvider) Name() string { return p.name }
func (p *mockProvider) DoGenerate(ctx context.Context, params GenerateParams) (*GenerateResult, error) {
	return p.genResult, p.genErr
}
func (p *mockProvider) DoStream(ctx context.Context, params GenerateParams) (*StreamResult, error) {
	if p.streamErr != nil {
		return nil, p.streamErr
	}
	ch := make(chan StreamPart, 1)
	if p.streamPart != nil {
		ch <- p.streamPart
	}
	close(ch)
	return &StreamResult{Stream: ch}, nil
}

// ============================================================================
// tests
// ============================================================================

func TestQuotaRecordingProvider_DoGenerate_Records(t *testing.T) {
	var (
		mu       sync.Mutex
		recorded []recordedCall
	)
	recorder := func(dim string, tokens int64) int64 {
		mu.Lock()
		defer mu.Unlock()
		recorded = append(recorded, recordedCall{dim, tokens})
		return 0
	}

	inner := &mockProvider{
		name: "test",
		genResult: &GenerateResult{
			Usage: Usage{TotalTokens: 150},
		},
	}
	wp := NewQuotaRecordingProvider(inner, recorder)

	ctx := WithQuotaDimension(context.Background(), "bot:test:channel:telegram")
	result, err := wp.DoGenerate(ctx, GenerateParams{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(recorded) != 1 {
		t.Fatalf("expected 1 recorded call, got %d", len(recorded))
	}
	if recorded[0].dimension != "bot:test:channel:telegram" {
		t.Errorf("expected dimension 'bot:test:channel:telegram', got %q", recorded[0].dimension)
	}
	if recorded[0].tokens != 150 {
		t.Errorf("expected 150 tokens, got %d", recorded[0].tokens)
	}
}

func TestQuotaRecordingProvider_DoGenerate_NoDimension_NoRecord(t *testing.T) {
	var recorded int
	recorder := func(dim string, tokens int64) int64 {
		recorded++
		return 0
	}

	inner := &mockProvider{
		name: "test",
		genResult: &GenerateResult{
			Usage: Usage{TotalTokens: 100},
		},
	}
	wp := NewQuotaRecordingProvider(inner, recorder)

	// No dimension in context → should not record
	_, err := wp.DoGenerate(context.Background(), GenerateParams{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if recorded != 0 {
		t.Errorf("expected 0 recordings, got %d", recorded)
	}
}

func TestQuotaRecordingProvider_DoGenerate_Error_NoRecord(t *testing.T) {
	var recorded int
	recorder := func(dim string, tokens int64) int64 {
		recorded++
		return 0
	}

	inner := &mockProvider{
		name:   "test",
		genErr: assertAnError,
	}
	wp := NewQuotaRecordingProvider(inner, recorder)

	ctx := WithQuotaDimension(context.Background(), "bot:test")
	_, err := wp.DoGenerate(ctx, GenerateParams{})
	if err == nil {
		t.Fatal("expected error")
	}
	if recorded != 0 {
		t.Errorf("expected 0 recordings on error, got %d", recorded)
	}
}

func TestQuotaRecordingProvider_DoGenerate_ZeroTokens_Skipped(t *testing.T) {
	var recorded int
	recorder := func(dim string, tokens int64) int64 {
		recorded++
		return 0
	}

	inner := &mockProvider{
		name:      "test",
		genResult: &GenerateResult{Usage: Usage{TotalTokens: 0}},
	}
	wp := NewQuotaRecordingProvider(inner, recorder)

	ctx := WithQuotaDimension(context.Background(), "bot:test")
	_, err := wp.DoGenerate(ctx, GenerateParams{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if recorded != 0 {
		t.Errorf("expected 0 recordings for 0 tokens, got %d", recorded)
	}
}

func TestQuotaRecordingProvider_DoStream_RecordsOnFinishPart(t *testing.T) {
	var (
		mu       sync.Mutex
		recorded []recordedCall
	)
	recorder := func(dim string, tokens int64) int64 {
		mu.Lock()
		defer mu.Unlock()
		recorded = append(recorded, recordedCall{dim, tokens})
		return 0
	}

	inner := &mockProvider{
		name: "test",
		streamPart: &FinishPart{
			TotalUsage: Usage{TotalTokens: 250},
		},
	}
	wp := NewQuotaRecordingProvider(inner, recorder)

	ctx := WithQuotaDimension(context.Background(), "bot:test:chat:telegram:-789")
	result, err := wp.DoStream(ctx, GenerateParams{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}

	// Consume the stream
	for range result.Stream {
	}

	mu.Lock()
	defer mu.Unlock()
	if len(recorded) != 1 {
		t.Fatalf("expected 1 recorded call, got %d", len(recorded))
	}
	if recorded[0].dimension != "bot:test:chat:telegram:-789" {
		t.Errorf("unexpected dimension: %q", recorded[0].dimension)
	}
	if recorded[0].tokens != 250 {
		t.Errorf("expected 250 tokens, got %d", recorded[0].tokens)
	}
}

func TestQuotaRecordingProvider_DoStream_NoDimension_NoRecord(t *testing.T) {
	var recorded int
	recorder := func(dim string, tokens int64) int64 {
		recorded++
		return 0
	}

	inner := &mockProvider{
		name: "test",
		streamPart: &FinishPart{
			TotalUsage: Usage{TotalTokens: 50},
		},
	}
	wp := NewQuotaRecordingProvider(inner, recorder)

	result, err := wp.DoStream(context.Background(), GenerateParams{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for range result.Stream {
	}
	if recorded != 0 {
		t.Errorf("expected 0 recordings without dimension, got %d", recorded)
	}
}

func TestQuotaRecordingProvider_DoStream_Error_NoRecord(t *testing.T) {
	var recorded int
	recorder := func(dim string, tokens int64) int64 {
		recorded++
		return 0
	}

	inner := &mockProvider{
		name:      "test",
		streamErr: assertAnError,
	}
	wp := NewQuotaRecordingProvider(inner, recorder)

	ctx := WithQuotaDimension(context.Background(), "bot:test")
	_, err := wp.DoStream(ctx, GenerateParams{})
	if err == nil {
		t.Fatal("expected error")
	}
	if recorded != 0 {
		t.Errorf("expected 0 recordings on error, got %d", recorded)
	}
}

func TestQuotaRecordingProvider_Name(t *testing.T) {
	inner := &mockProvider{name: "openai"}
	wp := NewQuotaRecordingProvider(inner, nil)
	if wp.Name() != "openai" {
		t.Errorf("expected name 'openai', got %q", wp.Name())
	}
}

// ============================================================================
// Context helpers tests
// ============================================================================

func TestQuotaDimensionContext_RoundTrip(t *testing.T) {
	ctx := context.Background()
	dim := QuotaDimensionFromContext(ctx)
	if dim != "" {
		t.Errorf("expected empty from bare context, got %q", dim)
	}

	ctx = WithQuotaDimension(ctx, "bot:bot1:chat:telegram:-123")
	dim = QuotaDimensionFromContext(ctx)
	if dim != "bot:bot1:chat:telegram:-123" {
		t.Errorf("expected 'bot:bot1:chat:telegram:-123', got %q", dim)
	}
}

func TestQuotaDimensionContext_Independent(t *testing.T) {
	// Two contexts should not interfere
	ctx1 := WithQuotaDimension(context.Background(), "dim_a")
	ctx2 := WithQuotaDimension(context.Background(), "dim_b")

	if QuotaDimensionFromContext(ctx1) != "dim_a" {
		t.Error("ctx1 corrupted")
	}
	if QuotaDimensionFromContext(ctx2) != "dim_b" {
		t.Error("ctx2 corrupted")
	}
}

// ============================================================================
// helpers
// ============================================================================

type recordedCall struct {
	dimension string
	tokens    int64
}

var assertAnError = &testError{}

type testError struct{}

func (e *testError) Error() string { return "test error" }
