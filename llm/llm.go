package llm

import "context"

// ============================================================================
// Core text-generation interface
// ============================================================================

// Provider is the interface that all text-generation backends must implement.
//
// A Provider translates unified GenerateParams into provider-specific wire
// formats and converts the responses back into unified GenerateResult /
// StreamResult.
type Provider interface {
	// Name returns a human-readable provider identifier, e.g. "openai" or
	// "anthropic".
	Name() string

	// DoGenerate performs a single (non-streaming) text-generation call.
	DoGenerate(ctx context.Context, params GenerateParams) (*GenerateResult, error)

	// DoStream performs a streaming text-generation call, returning a
	// StreamResult whose channel yields StreamPart chunks.
	DoStream(ctx context.Context, params GenerateParams) (*StreamResult, error)
}

// ============================================================================
// Optional capability interfaces
// ============================================================================

// ModelLister is implemented by providers that expose a model-listing API.
type ModelLister interface {
	ListModels(ctx context.Context) ([]Model, error)
}

// TestableProvider is implemented by providers that support health checks.
// Providers that do not support health checks simply omit these methods.
type TestableProvider interface {
	// Test performs a minimal health check against the provider backend.
	Test(ctx context.Context) *ProviderTestResult
	// TestModel checks whether a specific model ID is supported.
	TestModel(ctx context.Context, modelID string) (*ModelTestResult, error)
}

// ProviderStatus represents the health status of a provider.
type ProviderStatus string

const (
	ProviderStatusOK          ProviderStatus = "ok"
	ProviderStatusUnhealthy   ProviderStatus = "unhealthy"
	ProviderStatusUnreachable ProviderStatus = "unreachable"
)

// ProviderTestResult holds the result of a provider health check.
type ProviderTestResult struct {
	Status  ProviderStatus
	Message string
	Error   error
}

// ModelTestResult holds the result of a model support check.
type ModelTestResult struct {
	Supported bool
	Message   string
}

// EmbeddingProvider is the interface for embedding backends.
type EmbeddingProvider interface {
	DoEmbed(ctx context.Context, params EmbedParams) (*EmbedResult, error)
}

// EmbedParams holds parameters for an embedding request.
type EmbedParams struct {
	Model      *Model
	Values     []string
	Dimensions *int
}

// EmbedResult holds the result of an embedding request.
type EmbedResult struct {
	Embeddings [][]float64
	Tokens     int
}

// SpeechProvider is the interface for text-to-speech backends.
type SpeechProvider interface {
	DoSpeech(ctx context.Context, params SpeechParams) (*SpeechResult, error)
}

// SpeechParams holds parameters for a speech synthesis request.
type SpeechParams struct {
	Model        string
	Text         string
	Voice        string
	Format       string
	Speed        *float64
	Instructions string
	Extra        map[string]any
}

// SpeechResult holds synthesized audio.
type SpeechResult struct {
	Audio       []byte
	ContentType string
}

// TranscriptionProvider is the interface for speech-to-text backends.
type TranscriptionProvider interface {
	DoTranscribe(ctx context.Context, params TranscriptionParams) (*TranscriptionResult, error)
}

// TranscriptionParams holds parameters for a transcription request.
type TranscriptionParams struct {
	Model       string
	Audio       []byte
	Filename    string
	ContentType string
	Language    string
	Prompt      string
	Extra       map[string]any
}

// TranscriptionWord represents a word-level alignment item.
type TranscriptionWord struct {
	Text      string
	Start     float64
	End       float64
	SpeakerID string
}

// TranscriptionResult holds the result of a transcription request.
type TranscriptionResult struct {
	Text             string
	Language         string
	DurationSeconds  float64
	Words            []TranscriptionWord
	ProviderMetadata map[string]any
}
