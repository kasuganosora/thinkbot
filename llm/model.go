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
type Model struct {
	ID          string
	DisplayName string
	Type        ModelType
}

// ChatModel creates a chat-type Model with the given ID.
func ChatModel(id string) *Model {
	return &Model{ID: id, Type: ModelTypeChat}
}

// EmbeddingModel creates an embedding-type Model with the given ID.
func EmbeddingModel(id string) *Model {
	return &Model{ID: id, Type: ModelTypeEmbedding}
}
