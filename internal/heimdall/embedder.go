package heimdall

import "context"

// Embedder generates embedding vectors for text.
type Embedder interface {
	// Embed returns a float32 vector for the given text.
	Embed(ctx context.Context, text string) ([]float32, error)
}

// OllamaEmbedder implements Embedder using a local Ollama instance.
type OllamaEmbedder struct {
	client *OllamaClient
	model  string
}

// NewOllamaEmbedder creates an embedder that uses the given Ollama client and model.
func NewOllamaEmbedder(client *OllamaClient, model string) *OllamaEmbedder {
	return &OllamaEmbedder{client: client, model: model}
}

// Embed generates an embedding vector for the given text via Ollama.
func (e *OllamaEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	return e.client.Embed(ctx, e.model, text)
}

// MockEmbedder returns fixed embeddings for testing.
// Map keys are the input text; if not found, returns a zero vector.
type MockEmbedder struct {
	Vectors   map[string][]float32
	Dimension int
	CallCount int
}

// Embed returns a stored vector for the text, or a zero vector if not found.
func (m *MockEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	m.CallCount++
	if v, ok := m.Vectors[text]; ok {
		return v, nil
	}
	return make([]float32, m.Dimension), nil
}
