package heimdall

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// IngestResult reports what happened during session ingestion.
type IngestResult struct {
	ChunksProcessed int      `json:"chunksProcessed"`
	MemoriesCreated int      `json:"memoriesCreated"`
	MemoriesUpdated int      `json:"memoriesUpdated"`
	Duplicates      int      `json:"duplicates"`
	Tags            []string `json:"tagsExtracted"`
}

// IngestSession chunks a session summary, classifies and tags each chunk,
// deduplicates against existing memories, and stores new/updated memories.
func IngestSession(ctx context.Context, summary string, project string, embedder Embedder, store *MemoryStore) (*IngestResult, error) {
	chunks := ChunkSummary(summary, 500)
	if len(chunks) == 0 {
		return &IngestResult{}, nil
	}

	result := &IngestResult{}
	allTags := make(map[string]bool)

	// SEC-4: Load all vectors once for batch dedup
	var vectorCache []memoryVectorEntry
	if store.MemoryCount() <= MaxMemoriesForSemanticDedup {
		var err error
		vectorCache, err = store.LoadAllMemoryVectors()
		if err != nil {
			return nil, fmt.Errorf("loading memory vectors: %w", err)
		}
	}

	for _, chunk := range chunks {
		result.ChunksProcessed++

		hash := ContentHash(chunk)
		tags := ExtractTags(chunk)
		for _, t := range tags {
			allTags[t] = true
		}
		memType := ClassifyMemoryType(chunk)

		// Tier 1: Exact content hash dedup
		existing, err := store.GetMemoryByHash(hash)
		if err != nil {
			return nil, fmt.Errorf("hash lookup: %w", err)
		}
		if existing != nil {
			// For session ingestion, skip exact duplicates entirely
			result.Duplicates++
			continue
		}

		// Embed the chunk
		vec, err := embedder.Embed(ctx, chunk)
		if err != nil {
			return nil, fmt.Errorf("embedding chunk: %w", err)
		}

		// Tier 2: Semantic dedup
		if vectorCache != nil {
			matchID, sim := store.FindSimilarMemoryFromCache(vec, 0.92, vectorCache)
			if matchID != "" {
				// Near-duplicate found — update timestamp only for session source
				existingMem, err := store.GetMemoryByID(matchID)
				if err != nil {
					return nil, fmt.Errorf("getting memory %s: %w", matchID, err)
				}
				if existingMem != nil {
					_ = sim // used for threshold check above
					if existingMem.Source == MemorySourceExplicit {
						// Explicit wins — only bump updated_at
						existingMem.UpdatedAt = time.Now().Unix()
					} else {
						// Session + session: update content if new, merge tags
						existingMem.Content = chunk
						existingMem.Tags = MergeTags(existingMem.Tags, tags)
						existingMem.Vector = vec
						existingMem.UpdatedAt = time.Now().Unix()
						existingMem.ContentHash = hash
					}
					if err := store.UpsertMemory(*existingMem); err != nil {
						return nil, fmt.Errorf("updating memory: %w", err)
					}
					result.MemoriesUpdated++
					continue
				}
			}
		}

		// No duplicate — create new memory
		id := fmt.Sprintf("mem:session:%s", hash)
		now := time.Now().Unix()
		m := Memory{
			ID:          id,
			Content:     chunk,
			Type:        memType,
			Tags:        tags,
			Project:     project,
			Vector:      vec,
			CreatedAt:   now,
			UpdatedAt:   now,
			Source:       MemorySourceSession,
			ContentHash: hash,
		}
		if err := store.UpsertMemory(m); err != nil {
			return nil, fmt.Errorf("storing memory: %w", err)
		}
		result.MemoriesCreated++

		// Add to vector cache for subsequent chunks in this session
		if vectorCache != nil {
			vectorCache = append(vectorCache, memoryVectorEntry{ID: id, Vector: vec})
		}
	}

	for t := range allTags {
		result.Tags = append(result.Tags, t)
	}

	return result, nil
}

// ChunkSummary splits a summary into sentence-based chunks.
func ChunkSummary(summary string, maxSize int) []string {
	if maxSize <= 0 {
		maxSize = 500
	}

	summary = strings.TrimSpace(summary)
	if summary == "" {
		return nil
	}

	if len(summary) <= maxSize {
		return []string{summary}
	}

	sentences := splitSentences(summary)
	var chunks []string
	var buf strings.Builder

	for _, sentence := range sentences {
		sentence = strings.TrimSpace(sentence)
		if sentence == "" {
			continue
		}
		if buf.Len()+len(sentence)+1 > maxSize && buf.Len() > 0 {
			chunks = append(chunks, strings.TrimSpace(buf.String()))
			buf.Reset()
		}
		if buf.Len() > 0 {
			buf.WriteByte(' ')
		}
		buf.WriteString(sentence)
	}

	if buf.Len() > 0 {
		chunks = append(chunks, strings.TrimSpace(buf.String()))
	}

	return chunks
}

// splitSentences splits text on sentence boundaries.
var sentenceEnd = regexp.MustCompile(`([.!?])\s+`)

func splitSentences(text string) []string {
	// Split on sentence-ending punctuation followed by whitespace
	parts := sentenceEnd.Split(text, -1)
	delims := sentenceEnd.FindAllStringSubmatch(text, -1)

	var sentences []string
	for i, part := range parts {
		s := strings.TrimSpace(part)
		if s == "" {
			continue
		}
		// Re-attach the punctuation to the preceding sentence
		if i > 0 && i-1 < len(delims) {
			// This part already lost its leading punctuation
		}
		if i < len(delims) {
			s += delims[i][1] // re-attach the punctuation
		}
		sentences = append(sentences, s)
	}
	return sentences
}

// ExtractTags auto-extracts tags from memory content using keyword patterns.
func ExtractTags(content string) []string {
	var tags []string

	for tag, pattern := range techPatterns {
		if pattern.MatchString(content) {
			tags = append(tags, tag)
		}
	}

	// CamelCase words
	for _, match := range camelCasePattern.FindAllString(content, 5) {
		tags = append(tags, strings.ToLower(match))
	}

	return uniqueStrings(tags)
}

var techPatterns = map[string]*regexp.Regexp{
	"database":     regexp.MustCompile(`(?i)\b(sql|sqlite|postgres|mysql|mongodb|redis|database|db)\b`),
	"testing":      regexp.MustCompile(`(?i)\b(test|tdd|unit test|integration test|e2e)\b`),
	"api":          regexp.MustCompile(`(?i)\b(api|rest|grpc|graphql|endpoint|webhook)\b`),
	"deployment":   regexp.MustCompile(`(?i)\b(deploy|kubernetes|docker|ci/cd|pipeline|flux)\b`),
	"security":     regexp.MustCompile(`(?i)\b(auth|security|token|credential|encrypt|ssl|tls)\b`),
	"performance":  regexp.MustCompile(`(?i)\b(performance|latency|throughput|cache|optimization)\b`),
	"workflow":     regexp.MustCompile(`(?i)\b(workflow|process|pr|review|branch|commit|git)\b`),
	"architecture": regexp.MustCompile(`(?i)\b(architecture|design|pattern|refactor|migration)\b`),
	"frontend":     regexp.MustCompile(`(?i)\b(frontend|react|vue|angular|css|html|ui|ux)\b`),
	"backend":      regexp.MustCompile(`(?i)\b(backend|server|microservice|queue|worker)\b`),
}

var camelCasePattern = regexp.MustCompile(`\b([A-Z][a-z]+(?:[A-Z][a-z]+)+)\b`)

// ClassifyMemoryType uses heuristics to classify a memory's type.
func ClassifyMemoryType(content string) MemoryType {
	lower := strings.ToLower(content)

	if containsAny(lower, []string{"decided", "decision", "chose", "chosen", "we will", "agreed", "instead of"}) {
		return MemoryTypeDecision
	}
	if containsAny(lower, []string{"prefers", "preference", "likes", "always use", "never use", "rather", "favorite"}) {
		return MemoryTypePreference
	}
	if containsAny(lower, []string{"currently", "working on", "in progress", "context", "background", "situation"}) {
		return MemoryTypeContext
	}
	return MemoryTypeFact
}

// ContentHash returns the SHA-256 hash of normalized content.
func ContentHash(content string) string {
	normalized := strings.ToLower(strings.TrimSpace(content))
	normalized = collapseWhitespace(normalized)
	h := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(h[:])
}

func collapseWhitespace(s string) string {
	return whitespacePattern.ReplaceAllString(s, " ")
}

var whitespacePattern = regexp.MustCompile(`\s+`)

func containsAny(s string, substrs []string) bool {
	for _, sub := range substrs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func uniqueStrings(ss []string) []string {
	seen := make(map[string]bool, len(ss))
	var result []string
	for _, s := range ss {
		if !seen[s] {
			seen[s] = true
			result = append(result, s)
		}
	}
	return result
}

// MergeTags merges two tag slices, deduplicating and capping at 20.
func MergeTags(existing, newTags []string) []string {
	seen := make(map[string]bool, len(existing)+len(newTags))
	var result []string
	for _, t := range existing {
		if !seen[t] {
			seen[t] = true
			result = append(result, t)
		}
	}
	for _, t := range newTags {
		if !seen[t] && len(result) < 20 {
			seen[t] = true
			result = append(result, t)
		}
	}
	return result
}
