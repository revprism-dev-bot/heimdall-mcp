package heimdall

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"strings"
)

// RecallParams bundles the arguments for a memory recall.
type RecallParams struct {
	Query   string
	Type    string
	Tags    []string
	Project string
	Limit   int
	// Scope is an optional prefix filter on Memory.ContextPath. When set,
	// only memories whose ContextPath starts with this prefix are returned.
	// Used by retrieval hooks to narrow context when the CWD is a subpath
	// of the repo root.
	Scope string
}

// RecallHit is a single result from a memory recall, shaped to match the
// MCP `memoryResult` array so CLI and MCP produce identical JSON output.
type RecallHit struct {
	ID      string   `json:"id"`
	Content string   `json:"content"`
	Type    string   `json:"type"`
	Tags    []string `json:"tags,omitempty"`
	Project string   `json:"project,omitempty"`
	Score   float64  `json:"score"`
	Source  string   `json:"source"`
}

// RunRecall embeds the query and searches memories. It is the shared core
// used by both `heimdall-mcp recall` and the MCP `toolRecall`.
func RunRecall(ctx context.Context, p RecallParams, embedder Embedder, store *MemoryStore) ([]RecallHit, error) {
	if strings.TrimSpace(p.Query) == "" {
		return nil, fmt.Errorf("query is required")
	}
	if store == nil {
		return nil, fmt.Errorf("memory store not initialized")
	}

	limit := p.Limit
	if limit <= 0 {
		limit = 5
	}
	if limit > 100 {
		limit = 100
	}

	queryVec, err := embedder.Embed(ctx, p.Query)
	if err != nil {
		return nil, fmt.Errorf("embedding error: %w", err)
	}

	filters := MemoryFilter{
		Tags:        p.Tags,
		Project:     p.Project,
		ContextPath: p.Scope,
	}
	if p.Type != "" {
		filters.Type = MemoryType(p.Type)
	}

	results := store.SearchMemories(queryVec, limit, filters)
	hits := make([]RecallHit, 0, len(results))
	for _, r := range results {
		hits = append(hits, RecallHit{
			ID:      r.Memory.ID,
			Content: r.Memory.Content,
			Type:    string(r.Memory.Type),
			Tags:    r.Memory.Tags,
			Project: r.Memory.Project,
			Score:   r.Similarity,
			Source:  string(r.Memory.Source),
		})
	}
	return hits, nil
}

// ReadLengthPrefixedBuffer parses the length-prefixed JSON buffer format
// documented in docs/plans/hooks/00-consolidated-plan.md §3.4:
//
//	[4-byte big-endian length][payload bytes]
//	[4-byte big-endian length][payload bytes]
//	...
//
// Each payload is an opaque chunk (typically a JSON turn record) that may
// contain newlines. The returned string concatenates all payloads with a
// newline separator, suitable for passing to IngestSession as a summary.
//
// EOF before a length prefix is terminal (returns accumulated payloads).
// EOF in the middle of a length prefix or payload is a hard error so
// truncated buffers fail loudly rather than silently losing data.
func ReadLengthPrefixedBuffer(r io.Reader) (string, error) {
	const maxPayload = 2 * 1024 * 1024 // 2 MB per chunk — matches buffer cap
	var parts []string
	var lenBuf [4]byte
	for {
		_, err := io.ReadFull(r, lenBuf[:])
		if err == io.EOF {
			break
		}
		if err == io.ErrUnexpectedEOF {
			return "", fmt.Errorf("truncated length prefix in buffer")
		}
		if err != nil {
			return "", fmt.Errorf("read length prefix: %w", err)
		}
		length := binary.BigEndian.Uint32(lenBuf[:])
		if length == 0 {
			continue
		}
		if length > maxPayload {
			return "", fmt.Errorf("payload too large: %d bytes (max %d)", length, maxPayload)
		}
		payload := make([]byte, length)
		if _, err := io.ReadFull(r, payload); err != nil {
			return "", fmt.Errorf("read payload: %w", err)
		}
		parts = append(parts, string(payload))
	}
	return strings.Join(parts, "\n"), nil
}

// IngestSessionSummary embeds and stores a session summary. It wraps
// IngestSession so CLI and MCP share one path.
func IngestSessionSummary(ctx context.Context, summary, project string, embedder Embedder, store *MemoryStore) (*IngestResult, error) {
	if strings.TrimSpace(summary) == "" {
		return nil, fmt.Errorf("summary is required")
	}
	if store == nil {
		return nil, fmt.Errorf("memory store not initialized")
	}
	cleaned := strings.ReplaceAll(summary, "\x00", "")
	return IngestSession(ctx, cleaned, project, embedder, store)
}
