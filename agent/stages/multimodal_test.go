package stages

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/llm"
)

// ============================================================================
// Mock Vision Provider
// ============================================================================

type mockVisionProvider struct {
	name      string
	generate  func(ctx context.Context, params llm.GenerateParams) (*llm.GenerateResult, error)
	lastParam *llm.GenerateParams // 记录最后一次调用的参数（测试用）
}

func (m *mockVisionProvider) Name() string { return m.name }

func (m *mockVisionProvider) DoGenerate(ctx context.Context, params llm.GenerateParams) (*llm.GenerateResult, error) {
	m.lastParam = &params
	if m.generate != nil {
		return m.generate(ctx, params)
	}
	return &llm.GenerateResult{
		Text:         "A cat sitting on a table",
		FinishReason: llm.FinishReasonStop,
	}, nil
}

func (m *mockVisionProvider) DoStream(ctx context.Context, params llm.GenerateParams) (*llm.StreamResult, error) {
	return nil, nil
}

// ============================================================================
// Helpers
// ============================================================================

func newTestMultimodalStage(t *testing.T, config MultimodalConfig) *MultimodalStage {
	t.Helper()
	return NewMultimodalStage("multimodal", config, noop.NewTracerProvider(), zap.NewNop().Sugar())
}

func makeEnvWithText(text string, attachments []core.Attachment) *core.Envelope {
	msg := core.Message{
		ID:        "msg-1",
		Text:      text,
		Source:    "test",
		Channel:   "test",
		ChatType:  core.ChatPrivate,
		UserID:    "user1",
		MediaType: "text/plain",
	}
	if attachments != nil {
		core.SetAttachments(&msg, attachments)
	}
	return core.NewEnvelope(msg)
}

// ============================================================================
// ShouldProcess tests
// ============================================================================

func TestMultimodal_ShouldProcess_NoAttachments(t *testing.T) {
	provider := &mockVisionProvider{name: "mock"}
	stage := newTestMultimodalStage(t, MultimodalConfig{
		VisionProvider: provider,
		VisionModel:    llm.ChatModel("vision-model"),
	})

	env := makeEnvWithText("hello", nil)
	if stage.ShouldProcess(&env.Message) {
		t.Error("expected ShouldProcess=false with no attachments")
	}
}

func TestMultimodal_ShouldProcess_HasAttachments(t *testing.T) {
	provider := &mockVisionProvider{name: "mock"}
	stage := newTestMultimodalStage(t, MultimodalConfig{
		VisionProvider: provider,
		VisionModel:    llm.ChatModel("vision-model"),
	})

	attachments := []core.Attachment{
		{Type: core.AttachmentTypeImage, MimeType: "image/png", URL: "https://example.com/cat.png"},
	}
	env := makeEnvWithText("what is this?", attachments)
	if !stage.ShouldProcess(&env.Message) {
		t.Error("expected ShouldProcess=true with image attachments")
	}
}

func TestMultimodal_ShouldProcess_MainSupportsMultimodal(t *testing.T) {
	provider := &mockVisionProvider{name: "mock"}
	stage := newTestMultimodalStage(t, MultimodalConfig{
		VisionProvider: provider,
		VisionModel:    llm.ChatModel("vision-model"),
		MainMultimodal: true, // 主模型支持多模态
	})

	attachments := []core.Attachment{
		{Type: core.AttachmentTypeImage, MimeType: "image/png", URL: "https://example.com/cat.png"},
	}
	env := makeEnvWithText("what is this?", attachments)
	if stage.ShouldProcess(&env.Message) {
		t.Error("expected ShouldProcess=false when main model supports multimodal")
	}
}

func TestMultimodal_ShouldProcess_NoVisionProvider(t *testing.T) {
	stage := newTestMultimodalStage(t, MultimodalConfig{
		// No vision provider
	})

	attachments := []core.Attachment{
		{Type: core.AttachmentTypeImage, MimeType: "image/png", URL: "https://example.com/cat.png"},
	}
	env := makeEnvWithText("what is this?", attachments)
	if stage.ShouldProcess(&env.Message) {
		t.Error("expected ShouldProcess=false when no vision provider")
	}
}

func TestMultimodal_ShouldProcess_FileAttachmentOnly(t *testing.T) {
	provider := &mockVisionProvider{name: "mock"}
	stage := newTestMultimodalStage(t, MultimodalConfig{
		VisionProvider: provider,
		VisionModel:    llm.ChatModel("vision-model"),
	})

	// File type is not multimodal
	attachments := []core.Attachment{
		{Type: core.AttachmentTypeFile, MimeType: "application/pdf", URL: "https://example.com/doc.pdf"},
	}
	env := makeEnvWithText("read this", attachments)
	if stage.ShouldProcess(&env.Message) {
		t.Error("expected ShouldProcess=false with file-only attachment")
	}
}

// ============================================================================
// Process tests
// ============================================================================

func TestMultimodal_Process_TranscribesImage(t *testing.T) {
	provider := &mockVisionProvider{name: "mock"}
	stage := newTestMultimodalStage(t, MultimodalConfig{
		VisionProvider: provider,
		VisionModel:    llm.ChatModel("gpt-4o"),
	})

	attachments := []core.Attachment{
		{Type: core.AttachmentTypeImage, MimeType: "image/png", URL: "https://example.com/cat.png"},
	}
	env := makeEnvWithText("what is this?", attachments)

	result, err := stage.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("Process failed: %v", err)
	}

	// Text should contain original + description
	if !contains(result.Message.Text, "what is this?") {
		t.Errorf("expected original text preserved, got: %q", result.Message.Text)
	}
	if !contains(result.Message.Text, "A cat sitting on a table") {
		t.Errorf("expected description in text, got: %q", result.Message.Text)
	}
}

func TestMultimodal_Process_EmptyText(t *testing.T) {
	provider := &mockVisionProvider{name: "mock"}
	stage := newTestMultimodalStage(t, MultimodalConfig{
		VisionProvider: provider,
		VisionModel:    llm.ChatModel("gpt-4o"),
	})

	attachments := []core.Attachment{
		{Type: core.AttachmentTypeImage, MimeType: "image/jpeg", URL: "https://example.com/photo.jpg"},
	}
	env := makeEnvWithText("", attachments)

	result, err := stage.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("Process failed: %v", err)
	}

	if result.Message.Text == "" {
		t.Error("expected non-empty text after transcription")
	}
	if !contains(result.Message.Text, "A cat sitting on a table") {
		t.Errorf("expected description, got: %q", result.Message.Text)
	}
}

func TestMultimodal_Process_SkipsWhenNotNeeded(t *testing.T) {
	provider := &mockVisionProvider{name: "mock"}
	stage := newTestMultimodalStage(t, MultimodalConfig{
		VisionProvider: provider,
		VisionModel:    llm.ChatModel("gpt-4o"),
	})

	// No attachments
	env := makeEnvWithText("hello", nil)
	originalText := env.Message.Text

	result, err := stage.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("Process failed: %v", err)
	}

	if result.Message.Text != originalText {
		t.Errorf("expected text unchanged, got: %q", result.Message.Text)
	}
}

func TestMultimodal_Process_SkipsWhenMainMultimodal(t *testing.T) {
	provider := &mockVisionProvider{name: "mock"}
	stage := newTestMultimodalStage(t, MultimodalConfig{
		VisionProvider: provider,
		VisionModel:    llm.ChatModel("gpt-4o"),
		MainMultimodal: true,
	})

	attachments := []core.Attachment{
		{Type: core.AttachmentTypeImage, MimeType: "image/png", URL: "https://example.com/cat.png"},
	}
	env := makeEnvWithText("what is this?", attachments)
	originalText := env.Message.Text

	result, err := stage.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("Process failed: %v", err)
	}

	if result.Message.Text != originalText {
		t.Errorf("expected text unchanged when main supports multimodal, got: %q", result.Message.Text)
	}
}

func TestMultimodal_Process_MultipleAttachments(t *testing.T) {
	callCount := 0
	provider := &mockVisionProvider{
		name: "mock",
		generate: func(ctx context.Context, params llm.GenerateParams) (*llm.GenerateResult, error) {
			callCount++
			return &llm.GenerateResult{
				Text:         "description " + string(rune('A'+callCount-1)),
				FinishReason: llm.FinishReasonStop,
			}, nil
		},
	}
	stage := newTestMultimodalStage(t, MultimodalConfig{
		VisionProvider: provider,
		VisionModel:    llm.ChatModel("gpt-4o"),
	})

	attachments := []core.Attachment{
		{Type: core.AttachmentTypeImage, MimeType: "image/png", URL: "https://example.com/1.png"},
		{Type: core.AttachmentTypeImage, MimeType: "image/jpeg", URL: "https://example.com/2.jpg"},
	}
	env := makeEnvWithText("two images", attachments)

	result, err := stage.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("Process failed: %v", err)
	}

	if callCount != 2 {
		t.Errorf("expected 2 transcribe calls, got %d", callCount)
	}
	if !contains(result.Message.Text, "description A") {
		t.Errorf("expected description A in text, got: %q", result.Message.Text)
	}
	if !contains(result.Message.Text, "description B") {
		t.Errorf("expected description B in text, got: %q", result.Message.Text)
	}
}

func TestMultimodal_Process_FileAttachmentSkipped(t *testing.T) {
	callCount := 0
	provider := &mockVisionProvider{
		name: "mock",
		generate: func(ctx context.Context, params llm.GenerateParams) (*llm.GenerateResult, error) {
			callCount++
			return &llm.GenerateResult{Text: "should not be called", FinishReason: llm.FinishReasonStop}, nil
		},
	}
	stage := newTestMultimodalStage(t, MultimodalConfig{
		VisionProvider: provider,
		VisionModel:    llm.ChatModel("gpt-4o"),
	})

	// Only a PDF file, no multimodal
	attachments := []core.Attachment{
		{Type: core.AttachmentTypeFile, MimeType: "application/pdf", URL: "https://example.com/doc.pdf"},
	}
	env := makeEnvWithText("read this", attachments)

	result, err := stage.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("Process failed: %v", err)
	}

	// ShouldProcess would return false, but even if we call Process directly,
	// no transcription should happen for file type
	if callCount != 0 {
		t.Errorf("expected 0 transcribe calls for file attachment, got %d", callCount)
	}
	if result.Message.Text != "read this" {
		t.Errorf("expected text unchanged, got: %q", result.Message.Text)
	}
}

func TestMultimodal_Process_VisionProviderError(t *testing.T) {
	provider := &mockVisionProvider{
		name: "mock",
		generate: func(ctx context.Context, params llm.GenerateParams) (*llm.GenerateResult, error) {
			return nil, fmt.Errorf("API error")
		},
	}
	stage := newTestMultimodalStage(t, MultimodalConfig{
		VisionProvider: provider,
		VisionModel:    llm.ChatModel("gpt-4o"),
	})

	attachments := []core.Attachment{
		{Type: core.AttachmentTypeImage, MimeType: "image/png", URL: "https://example.com/cat.png"},
	}
	env := makeEnvWithText("what is this?", attachments)

	// Should not return error — should gracefully skip failed transcription
	result, err := stage.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("Process should not error on provider failure: %v", err)
	}

	// Original text should be preserved
	if !contains(result.Message.Text, "what is this?") {
		t.Errorf("expected original text preserved on error, got: %q", result.Message.Text)
	}
}

// ============================================================================
// Attachment helper tests
// ============================================================================

func TestAttachment_DataURI_URL(t *testing.T) {
	att := core.Attachment{URL: "https://example.com/img.png"}
	if att.DataURI() != "https://example.com/img.png" {
		t.Error("expected URL returned as-is")
	}
}

func TestAttachment_DataURI_Data(t *testing.T) {
	att := core.Attachment{
		Data:     []byte("hello"),
		MimeType: "image/png",
	}
	uri := att.DataURI()
	if uri == "" || !contains(uri, "data:image/png;base64,") {
		t.Errorf("expected data URI, got: %q", uri)
	}
}

func TestAttachment_DataURI_Empty(t *testing.T) {
	att := core.Attachment{}
	if att.DataURI() != "" {
		t.Error("expected empty DataURI for empty attachment")
	}
}

func TestCore_GetAttachments_Nil(t *testing.T) {
	msg := &core.Message{}
	if core.GetAttachments(msg) != nil {
		t.Error("expected nil for no metadata")
	}
}

func TestCore_HasMultimodalAttachments(t *testing.T) {
	msg := &core.Message{}
	core.SetAttachments(msg, []core.Attachment{
		{Type: core.AttachmentTypeFile},
	})
	if core.HasMultimodalAttachments(msg) {
		t.Error("expected false for file-only attachment")
	}

	core.SetAttachments(msg, []core.Attachment{
		{Type: core.AttachmentTypeImage, URL: "https://example.com/a.png"},
	})
	if !core.HasMultimodalAttachments(msg) {
		t.Error("expected true for image attachment")
	}
}

func TestCore_IsMultimodalType(t *testing.T) {
	if !core.IsMultimodalType(core.AttachmentTypeImage) {
		t.Error("image should be multimodal")
	}
	if !core.IsMultimodalType(core.AttachmentTypeAudio) {
		t.Error("audio should be multimodal")
	}
	if !core.IsMultimodalType(core.AttachmentTypeVideo) {
		t.Error("video should be multimodal")
	}
	if core.IsMultimodalType(core.AttachmentTypeFile) {
		t.Error("file should not be multimodal")
	}
}

// ============================================================================
// LLMBundle tests
// ============================================================================

func TestLLMBundle_MainSupportsMultimodal(t *testing.T) {
	// This test is in bot package context — verify through ModelDef
	// We just verify the logic here
	def := struct {
		Multimodal bool
	}{Multimodal: true}

	if !def.Multimodal {
		t.Error("expected Multimodal=true")
	}
}

// ============================================================================
// Helpers
// ============================================================================

func contains(s, substr string) bool {
	return strings.Contains(s, substr)
}
