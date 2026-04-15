package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/caio-silva/heimdall-mcp/internal/heimdall"
)

func (s *Server) toolRemember(args json.RawMessage) MCPToolResult {
	var input rememberInput
	if err := json.Unmarshal(args, &input); err != nil {
		return ErrResult("invalid arguments: " + err.Error())
	}

	// Validate inputs
	if err := validateContent(input.Content); err != nil {
		return ErrResult(err.Error())
	}
	if err := validateTags(input.Tags); err != nil {
		return ErrResult(err.Error())
	}

	// Sanitize: strip null bytes
	input.Content = strings.ReplaceAll(input.Content, "\x00", "")

	// Validate memory type
	memType := heimdall.MemoryTypeFact
	if input.Type != "" {
		mt := heimdall.MemoryType(input.Type)
		if !heimdall.ValidMemoryTypes[mt] {
			return ErrResult(fmt.Sprintf("invalid memory type %q: must be one of preference, decision, fact, context", input.Type))
		}
		memType = mt
	}

	if s.MemoryStore == nil {
		return ErrResult("memory store not initialized")
	}

	hash := heimdall.ContentHash(input.Content)

	// Tier 1: Check for exact content hash duplicate
	existing, err := s.MemoryStore.GetMemoryByHash(hash)
	if err != nil {
		return ErrResult("memory lookup error: " + err.Error())
	}
	if existing != nil {
		// Update existing: merge tags, update type and timestamp
		existing.Tags = heimdall.MergeTags(existing.Tags, input.Tags)
		existing.Type = memType
		existing.UpdatedAt = time.Now().Unix()
		if err := s.MemoryStore.UpsertMemory(*existing); err != nil {
			return ErrResult("memory update error: " + err.Error())
		}
		out, _ := json.MarshalIndent(map[string]any{
			"status":  "updated",
			"id":      existing.ID,
			"message": "Existing memory updated (exact match).",
		}, "", "  ")
		return TextResult(string(out))
	}

	// Embed the content
	ctx := context.Background()
	client := heimdall.NewOllamaClient(s.Cfg.OllamaEndpoint)
	if err := client.Ping(ctx); err != nil {
		return ollamaSetupError(s.Cfg.OllamaEndpoint, s.Cfg.Model, err)
	}
	embedder := heimdall.NewOllamaEmbedder(client, s.Cfg.Model)

	vec, err := embedder.Embed(ctx, input.Content)
	if err != nil {
		return ErrResult("embedding error: " + err.Error())
	}

	// Tier 2: Semantic dedup
	similar, sim, err := s.MemoryStore.FindSimilarMemory(vec, 0.92)
	if err != nil {
		return ErrResult("similarity check error: " + err.Error())
	}
	if similar != nil {
		// Near-duplicate: explicit always overwrites
		if similar.Source == heimdall.MemorySourceSession {
			// Session -> explicit: overwrite entirely
			similar.Content = input.Content
			similar.Vector = vec
			similar.ContentHash = hash
		}
		similar.Tags = heimdall.MergeTags(similar.Tags, input.Tags)
		similar.Type = memType
		similar.Source = heimdall.MemorySourceExplicit
		similar.UpdatedAt = time.Now().Unix()
		if err := s.MemoryStore.UpsertMemory(*similar); err != nil {
			return ErrResult("memory update error: " + err.Error())
		}
		out, _ := json.MarshalIndent(map[string]any{
			"status":     "updated",
			"id":         similar.ID,
			"similarity": fmt.Sprintf("%.2f", sim),
			"message":    "Similar memory updated (semantic match).",
		}, "", "  ")
		return TextResult(string(out))
	}

	// New memory
	id := fmt.Sprintf("mem:explicit:%s", hash)
	now := time.Now().Unix()
	m := heimdall.Memory{
		ID:          id,
		Content:     input.Content,
		Type:        memType,
		Tags:        input.Tags,
		Project:     input.Project,
		Vector:      vec,
		CreatedAt:   now,
		UpdatedAt:   now,
		Source:       heimdall.MemorySourceExplicit,
		ContentHash: hash,
	}
	if err := s.MemoryStore.UpsertMemory(m); err != nil {
		return ErrResult("memory store error: " + err.Error())
	}

	out, _ := json.MarshalIndent(map[string]any{
		"status":  "created",
		"id":      id,
		"type":    string(memType),
		"message": "Memory stored successfully.",
	}, "", "  ")
	return TextResult(string(out))
}

func (s *Server) toolRecall(args json.RawMessage) MCPToolResult {
	var input recallInput
	if err := json.Unmarshal(args, &input); err != nil {
		return ErrResult("invalid arguments: " + err.Error())
	}

	if err := validateQuery(input.Query); err != nil {
		return ErrResult(err.Error())
	}
	if err := validateTags(input.Tags); err != nil {
		return ErrResult(err.Error())
	}

	if s.MemoryStore == nil {
		return ErrResult("memory store not initialized")
	}

	ctx := context.Background()
	client := heimdall.NewOllamaClient(s.Cfg.OllamaEndpoint)
	if err := client.Ping(ctx); err != nil {
		return ollamaSetupError(s.Cfg.OllamaEndpoint, s.Cfg.Model, err)
	}
	embedder := heimdall.NewOllamaEmbedder(client, s.Cfg.Model)

	hits, err := heimdall.RunRecall(ctx, heimdall.RecallParams{
		Query:   input.Query,
		Type:    input.Type,
		Tags:    input.Tags,
		Project: input.Project,
		Limit:   input.Limit,
	}, embedder, s.MemoryStore)
	if err != nil {
		return ErrResult(err.Error())
	}

	if len(hits) == 0 {
		return TextResult("No memories found matching your query.")
	}

	data, _ := json.MarshalIndent(hits, "", "  ")
	return TextResult(string(data))
}

func (s *Server) toolIngestSession(args json.RawMessage) MCPToolResult {
	var input ingestSessionInput
	if err := json.Unmarshal(args, &input); err != nil {
		return ErrResult("invalid arguments: " + err.Error())
	}

	if err := validateSummary(input.Summary); err != nil {
		return ErrResult(err.Error())
	}

	if s.MemoryStore == nil {
		return ErrResult("memory store not initialized")
	}

	ctx := context.Background()
	client := heimdall.NewOllamaClient(s.Cfg.OllamaEndpoint)
	if err := client.Ping(ctx); err != nil {
		return ollamaSetupError(s.Cfg.OllamaEndpoint, s.Cfg.Model, err)
	}
	embedder := heimdall.NewOllamaEmbedder(client, s.Cfg.Model)

	result, err := heimdall.IngestSessionSummary(ctx, input.Summary, input.Project, embedder, s.MemoryStore)
	if err != nil {
		return ErrResult("ingestion error: " + err.Error())
	}

	// Include memory stats
	stats := s.MemoryStore.MemoryStats()

	out, _ := json.MarshalIndent(map[string]any{
		"chunksProcessed": result.ChunksProcessed,
		"memoriesCreated": result.MemoriesCreated,
		"memoriesUpdated": result.MemoriesUpdated,
		"duplicates":      result.Duplicates,
		"tagsExtracted":   result.Tags,
		"totalMemories":   stats.TotalMemories,
	}, "", "  ")
	return TextResult(string(out))
}

