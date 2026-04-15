package heimdall

import "context"

// Embedder generates embedding vectors for text.
type Embedder interface {
	// Embed returns a float32 vector for the given text.
	Embed(ctx context.Context, text string) ([]float32, error)
}

// BatchEmbedder extends Embedder with batch support.
type BatchEmbedder interface {
	Embedder
	// EmbedBatch generates embeddings for multiple texts in one call.
	EmbedBatch(ctx context.Context, texts []string) ([][]float32, error)
}

// OllamaEmbedder implements Embedder and BatchEmbedder using a local Ollama instance.
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

// EmbedBatch generates embeddings for multiple texts in one API call via Ollama.
// Automatically splits into sub-batches of EmbedBatchSize.
func (e *OllamaEmbedder) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	var all [][]float32
	for i := 0; i < len(texts); i += EmbedBatchSize {
		end := i + EmbedBatchSize
		if end > len(texts) {
			end = len(texts)
		}
		vecs, err := e.client.EmbedBatch(ctx, e.model, texts[i:end])
		if err != nil {
			return nil, err
		}
		all = append(all, vecs...)
	}
	return all, nil
}

// MockEmbedder returns fixed embeddings for testing.
// Map keys are the input text; if not found, returns a zero vector.
type MockEmbedder struct {
	Vectors        map[string][]float32
	Dimension      int
	CallCount      int
	BatchCallCount int
}

// Embed returns a stored vector for the text, or a zero vector if not found.
func (m *MockEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	m.CallCount++
	if v, ok := m.Vectors[text]; ok {
		return v, nil
	}
	return make([]float32, m.Dimension), nil
}

// EmbedBatch generates embeddings for multiple texts by calling Embed in a loop.
func (m *MockEmbedder) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	m.BatchCallCount++
	results := make([][]float32, 0, len(texts))
	for _, text := range texts {
		vec, err := m.Embed(ctx, text)
		if err != nil {
			return nil, err
		}
		results = append(results, vec)
	}
	return results, nil
}
