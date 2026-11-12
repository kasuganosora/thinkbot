package llm

// ModelType identifies the category of a model.
type ModelType string

const (
	ModelTypeChat      ModelType = "chat"
	ModelTypeEmbedding ModelType = "embedding"
)

// Model represents a model bound to a Provider.  The Provider field is used
// only at the SDK level to route requests; it is nil for models that are
// returned from ListModels (callers can still use the ID directly).
// OmitSamplingParam is the model-config sentinel for "do not send this sampling
// field". It is stored as temperature or top_p -1. Any other value is sent
// when the caller sets it. Empty temperature still defaults to 0.7 elsewhere.
const OmitSamplingParam = -1

type Model struct {
	ID          string
	DisplayName string
	Type        ModelType
	// Temperature is the configured sampling temperature, when known.
	// OmitSamplingParam means a caller such as L1 must not replace it with 0.
	Temperature *float64
}

// OmitsTemperature reports whether this model's configured temperature is the
// do-not-send sentinel.
func (m *Model) OmitsTemperature() bool {
	return m != nil && m.Temperature != nil && *m.Temperature == OmitSamplingParam
}

// ChatModel creates a chat-type Model with the given ID.
func ChatModel(id string) *Model {
	return &Model{ID: id, Type: ModelTypeChat}
}

// EmbeddingModel creates an embedding-type Model with the given ID.
func EmbeddingModel(id string) *Model {
	return &Model{ID: id, Type: ModelTypeEmbedding}
}
