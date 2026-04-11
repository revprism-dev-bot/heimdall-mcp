package heimdall

import (
	"context"
	"fmt"
	"strings"
)

// ContextBlock is a piece of project context ready for injection.
type ContextBlock struct {
	FilePath   string  // relative path
	StartLine  int
	EndLine    int
	Content    string
	Kind       string  // "function", "type", "paragraph", etc.
	Identifier string  // function/type name if applicable
	Score      float64 // similarity score [0, 1]
}

// Retriever finds relevant project context for a given query.
type Retriever struct {
	embedder  Embedder
	store     *VectorStore
	topK      int
	maxTokens int
}

// NewRetriever creates a context retriever.
func NewRetriever(embedder Embedder, store *VectorStore, topK int, maxTokens int) *Retriever {
	return &Retriever{
		embedder:  embedder,
		store:     store,
		topK:      topK,
		maxTokens: maxTokens,
	}
}

// Retrieve embeds the query and searches the store for relevant context.
// Returns up to topK blocks, respecting maxTokens total.
func (r *Retriever) Retrieve(ctx context.Context, query string) ([]ContextBlock, error) {
	// 1. Embed the query
	queryVec, err := r.embedder.Embed(ctx, query)
	if err != nil {
		return nil, err
	}

	// 2. Search the store
	results := r.store.Search(queryVec, r.topK)

	// 3. Convert to ContextBlocks, respecting token budget
	var blocks []ContextBlock
	tokenCount := 0
	for _, result := range results {
		// Rough token estimate: len/4 (good enough for English/code)
		chunkTokens := estimateTokens(result.Record.Content)
		if tokenCount+chunkTokens > r.maxTokens && len(blocks) > 0 {
			break // stop before exceeding budget (but always include at least 1)
		}
		blocks = append(blocks, ContextBlock{
			FilePath:   result.Record.FilePath,
			StartLine:  result.Record.StartLine,
			EndLine:    result.Record.EndLine,
			Content:    result.Record.Content,
			Kind:       result.Record.Kind,
			Identifier: result.Record.Identifier,
			Score:      result.Similarity,
		})
		tokenCount += chunkTokens
	}

	return blocks, nil
}

// FormatContextBlocks formats context blocks for injection into a prompt.
func FormatContextBlocks(blocks []ContextBlock) string {
	if len(blocks) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("[Relevant project context]\n\n")
	for i, b := range blocks {
		sb.WriteString(fmt.Sprintf("--- %s (L%d-%d", b.FilePath, b.StartLine, b.EndLine))
		if b.Identifier != "" {
			sb.WriteString(fmt.Sprintf(", %s %s", b.Kind, b.Identifier))
		}
		sb.WriteString(fmt.Sprintf(", score: %.2f) ---\n", b.Score))
		sb.WriteString(b.Content)
		if i < len(blocks)-1 {
			sb.WriteString("\n\n")
		}
	}
	return sb.String()
}

// estimateTokens gives a rough token count for a piece of text.
// Uses the standard heuristic of ~4 characters per token.
func estimateTokens(text string) int {
	return len(text) / 4
}
