// Package mcp implements the JSON-RPC based MCP server for Heimdall.
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/caio-silva/heimdall-mcp/internal/config"
	"github.com/caio-silva/heimdall-mcp/internal/heimdall"
	"github.com/caio-silva/heimdall-mcp/internal/registry"
)

// Server holds runtime state for the MCP server.
type Server struct {
	Cfg         config.Config
	Registry    *registry.Registry
	Index       IndexState
	MemoryStore *heimdall.MemoryStore
}

// TextResult creates a successful text result.
func TextResult(text string) MCPToolResult {
	return MCPToolResult{Content: []MCPContent{{Type: "text", Text: text}}}
}

// ErrResult creates an error text result.
func ErrResult(msg string) MCPToolResult {
	return MCPToolResult{Content: []MCPContent{{Type: "text", Text: msg}}, IsError: true}
}

// Handle dispatches a JSON-RPC request to the appropriate handler.
func (s *Server) Handle(req JSONRPCRequest) *JSONRPCResponse {
	switch req.Method {
	case "initialize":
		return s.handleInitialize(req)
	case "notifications/initialized":
		return nil
	case "tools/list":
		return s.handleToolsList(req)
	case "tools/call":
		return s.handleToolsCall(req)
	case "ping":
		return &JSONRPCResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{}}
	default:
		return &JSONRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error:   &RPCError{Code: -32601, Message: "method not found: " + req.Method},
		}
	}
}

func (s *Server) handleInitialize(req JSONRPCRequest) *JSONRPCResponse {
	return &JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      req.ID,
		Result: map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities": map[string]any{
				"tools": map[string]any{},
			},
			"serverInfo": map[string]any{
				"name":    "heimdall-mcp",
				"version": "1.0.0",
			},
		},
	}
}

func (s *Server) handleToolsList(req JSONRPCRequest) *JSONRPCResponse {
	tools := []MCPToolInfo{
		{
			Name:        "heimdall_search",
			Description: "Search indexed project files for code relevant to a query. Returns enriched results with score, source type, chunk ID, and embedding model. Supports filtering by source_type and metadata.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{
						"type":        "string",
						"description": "Natural language query or code snippet to search for",
					},
					"limit": map[string]any{
						"type":        "integer",
						"description": "Max results to return (default 5)",
						"default":     5,
					},
					"project": map[string]any{
						"type":        "string",
						"description": "Project name or path (optional — auto-detected from CWD or registry)",
					},
					"source_type": map[string]any{
						"type":        "string",
						"description": "Filter by source type: code, ticket, doc, pr, message, changelog, note, custom",
					},
					"metadata_filter": map[string]any{
						"type":        "object",
						"description": "Filter by metadata key-value pairs (e.g. {\"status\": \"in_progress\"})",
					},
				},
				"required": []string{"query"},
			},
		},
		{
			Name:        "heimdall_index",
			Description: "Index a directory for semantic search. Runs in the background — call heimdall_status to check progress. Creates or updates the .heimdall_db/ vector store.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{
						"type":        "string",
						"description": "Absolute path to the directory to index",
					},
				},
				"required": []string{"path"},
			},
		},
		{
			Name:        "heimdall_status",
			Description: "Check the status of the Heimdall context engine: Ollama reachability, model availability, index statistics, and indexing progress.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
		{
			Name:        "heimdall_index_text",
			Description: "Index arbitrary text content for semantic search. Use this to store context from external sources (Jira tickets, Confluence pages, Slack messages, changelogs, etc.) so it can be found via heimdall_search alongside code. Supports typed content with metadata and relationships.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"content": map[string]any{
						"type":        "string",
						"description": "The text content to index",
					},
					"source": map[string]any{
						"type":        "string",
						"description": "Source identifier (e.g. JIRA-123, confluence:My Page, slack:#engineering)",
					},
					"url": map[string]any{
						"type":        "string",
						"description": "URL of the original source (optional — stored alongside content for reference)",
					},
					"project": map[string]any{
						"type":        "string",
						"description": "Project to store under (optional — uses CWD or auto-detects from registry)",
					},
					"type": map[string]any{
						"type":        "string",
						"description": "Content type: ticket, doc, pr, message, changelog, note, custom",
						"enum":        []string{"ticket", "doc", "pr", "message", "changelog", "note", "custom"},
					},
					"metadata": map[string]any{
						"type":        "object",
						"description": "Arbitrary metadata (status, priority, assignee, sprint, etc.)",
					},
					"relationships": map[string]any{
						"type":        "array",
						"description": "Relationships to other items",
						"items": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"type":   map[string]any{"type": "string", "description": "Relationship type (implements, blocks, relates, etc.)"},
								"target": map[string]any{"type": "string", "description": "Target identifier (e.g. JIRA-400)"},
							},
							"required": []string{"type", "target"},
						},
					},
				},
				"required": []string{"content", "source"},
			},
		},
		{
			Name:        "heimdall_projects",
			Description: "List all projects registered in the Heimdall index registry. Shows project names, paths, and database locations.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
		{
			Name:        "heimdall_remember",
			Description: "Store a memory for persistent recall across sessions. Memories are embedded and stored in a local SQLite database. WARNING: Do not store API keys, passwords, tokens, or other secrets — memories are stored in plaintext and returned in recall results.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"content": map[string]any{
						"type":        "string",
						"description": "The memory text to store",
					},
					"tags": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "Tags for categorization and filtering (optional)",
					},
					"type": map[string]any{
						"type":        "string",
						"enum":        []string{"preference", "decision", "fact", "context"},
						"description": "Memory type (default: fact)",
						"default":     "fact",
					},
					"project": map[string]any{
						"type":        "string",
						"description": "Scope memory to a specific project (optional)",
					},
				},
				"required": []string{"content"},
			},
		},
		{
			Name:        "heimdall_recall",
			Description: "Retrieve stored memories by semantic search. Returns memories ranked by relevance with similarity scores. Optionally filter by type, tags, or project.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{
						"type":        "string",
						"description": "Natural language query to search memories",
					},
					"tags": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "Filter to memories with any of these tags (optional)",
					},
					"type": map[string]any{
						"type":        "string",
						"enum":        []string{"preference", "decision", "fact", "context"},
						"description": "Filter to a specific memory type (optional)",
					},
					"limit": map[string]any{
						"type":        "integer",
						"description": "Max results to return (default: 5)",
						"default":     5,
					},
					"project": map[string]any{
						"type":        "string",
						"description": "Filter to memories scoped to this project (optional)",
					},
				},
				"required": []string{"query"},
			},
		},
		{
			Name:        "heimdall_ingest_session",
			Description: "Auto-extract and store memories from a conversation summary. Chunks the summary, classifies memory types, auto-generates tags, and deduplicates against existing memories (cosine similarity > 0.92 = update instead of insert).",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"summary": map[string]any{
						"type":        "string",
						"description": "Conversation summary text to extract memories from",
					},
					"project": map[string]any{
						"type":        "string",
						"description": "Scope extracted memories to this project (optional)",
					},
				},
				"required": []string{"summary"},
			},
		},
		{
			Name:        "heimdall_explain",
			Description: "Deep diagnostic for a search query. Returns embedding dimensions, total chunks searched, search timing, top-N results with scores and chunk details, score distribution, source counts, index stats, and related items from relationships.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{
						"type":        "string",
						"description": "The search query to analyze",
					},
					"limit": map[string]any{
						"type":        "integer",
						"description": "Number of top results to include (default 10)",
						"default":     10,
					},
					"project": map[string]any{
						"type":        "string",
						"description": "Project name or path (optional)",
					},
				},
				"required": []string{"query"},
			},
		},
	}

	return &JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      req.ID,
		Result:  map[string]any{"tools": tools},
	}
}

func (s *Server) handleToolsCall(req JSONRPCRequest) *JSONRPCResponse {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return &JSONRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error:   &RPCError{Code: -32602, Message: "invalid params: " + err.Error()},
		}
	}

	var result MCPToolResult
	switch params.Name {
	case "heimdall_search":
		result = s.toolSearch(params.Arguments)
	case "heimdall_index":
		result = s.toolIndex(params.Arguments)
	case "heimdall_status":
		result = s.toolStatus()
	case "heimdall_index_text":
		result = s.toolIndexText(params.Arguments)
	case "heimdall_projects":
		result = s.toolListProjects()
	case "heimdall_remember":
		result = s.toolRemember(params.Arguments)
	case "heimdall_recall":
		result = s.toolRecall(params.Arguments)
	case "heimdall_ingest_session":
		result = s.toolIngestSession(params.Arguments)
	case "heimdall_explain":
		result = s.toolExplain(params.Arguments)
	default:
		return &JSONRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error:   &RPCError{Code: -32602, Message: "unknown tool: " + params.Name},
		}
	}

	return &JSONRPCResponse{JSONRPC: "2.0", ID: req.ID, Result: result}
}

func (s *Server) toolIndexText(args json.RawMessage) MCPToolResult {
	var input indexTextInput
	if err := json.Unmarshal(args, &input); err != nil {
		return ErrResult("invalid arguments: " + err.Error())
	}
	if input.Content == "" {
		return ErrResult("content is required")
	}
	if input.Source == "" {
		return ErrResult("source is required")
	}

	// SEC-1: Validate source field
	if err := validateSource(input.Source); err != nil {
		return ErrResult("invalid source: " + err.Error())
	}

	// SEC-2: Validate content length
	if len(input.Content) > 100*1024 {
		return ErrResult("content too large (max 100KB)")
	}

	// Validate and normalize source type
	sourceType := validateSourceType(input.Type)
	if sourceType == "" {
		return ErrResult("invalid type: must be one of ticket, doc, pr, message, changelog, note, custom")
	}

	// Validate metadata
	metadataStr := "{}"
	if len(input.Metadata) > 0 {
		if len(input.Metadata) > 10*1024 {
			return ErrResult("metadata too large (max 10KB)")
		}
		var obj map[string]any
		if err := json.Unmarshal(input.Metadata, &obj); err != nil {
			return ErrResult("invalid metadata: must be a JSON object: " + err.Error())
		}
		metadataStr = string(input.Metadata)
	}

	// Validate relationships
	relationshipsStr := "[]"
	if len(input.Relationships) > 0 {
		if len(input.Relationships) > 10*1024 {
			return ErrResult("relationships too large (max 10KB)")
		}
		var rels []struct {
			Type   string `json:"type"`
			Target string `json:"target"`
		}
		if err := json.Unmarshal(input.Relationships, &rels); err != nil {
			return ErrResult("invalid relationships: must be an array of {type, target}: " + err.Error())
		}
		if len(rels) > 50 {
			return ErrResult("too many relationships (max 50)")
		}
		relationshipsStr = string(input.Relationships)
	}

	ctx := context.Background()
	client := heimdall.NewOllamaClient(s.Cfg.OllamaEndpoint)
	if err := client.Ping(ctx); err != nil {
		return ErrResult("Ollama not reachable: " + err.Error())
	}

	// Resolve DB path — write tools use resolveDBDir (creates if needed)
	dbDir := s.resolveDBDir(input.Project)

	store, err := heimdall.OpenStore(dbDir)
	if err != nil {
		return ErrResult("store error: " + err.Error())
	}
	defer store.Close()

	embedder := heimdall.NewOllamaEmbedder(client, s.Cfg.Model)

	// Prepend source metadata so it shows up in search results
	content := input.Content
	if input.URL != "" {
		content = fmt.Sprintf("[Source: %s](%s)\n\n%s", input.Source, input.URL, content)
	}

	// Chunk the text
	chunks := chunkText(content, input.Source, 1500)

	var records []heimdall.VectorRecord
	for i, chunk := range chunks {
		vec, err := embedder.Embed(ctx, chunk.Content)
		if err != nil {
			return ErrResult(fmt.Sprintf("embedding error on chunk %d: %v", i, err))
		}
		records = append(records, heimdall.VectorRecord{
			ID:            fmt.Sprintf("ext:%s:%d", input.Source, i),
			FilePath:      input.Source,
			StartLine:     chunk.Start,
			EndLine:       chunk.End,
			Content:       chunk.Content,
			Kind:          "external",
			Identifier:    input.Source,
			Embedding:     vec,
			ModTime:       time.Now().Unix(),
			ContentHash:   "",
			SourceType:    sourceType,
			Metadata:      metadataStr,
			Relationships: relationshipsStr,
		})
	}

	if err := store.Upsert(records); err != nil {
		return ErrResult("upsert error: " + err.Error())
	}
	if err := store.Save(); err != nil {
		log.Printf("store save warning: %v", err)
	}

	out, _ := json.MarshalIndent(map[string]any{
		"source":   input.Source,
		"type":     sourceType,
		"chunks":   len(records),
		"database": dbDir,
	}, "", "  ")
	return TextResult(string(out))
}

// chunkText splits text into chunks for embedding.
type textChunk struct {
	Content string
	Start   int
	End     int
}

func chunkText(text, source string, maxSize int) []textChunk {
	if len(text) <= maxSize {
		return []textChunk{{Content: text, Start: 1, End: 1}}
	}

	// Split on double newlines first (paragraph boundaries)
	paragraphs := splitParagraphs(text)
	var chunks []textChunk
	var buf string
	chunkIdx := 0

	for _, para := range paragraphs {
		if len(buf)+len(para)+2 > maxSize && buf != "" {
			chunkIdx++
			chunks = append(chunks, textChunk{Content: buf, Start: chunkIdx, End: chunkIdx})
			buf = ""
		}
		if buf != "" {
			buf += "\n\n"
		}
		buf += para
	}
	if buf != "" {
		chunkIdx++
		chunks = append(chunks, textChunk{Content: buf, Start: chunkIdx, End: chunkIdx})
	}

	return chunks
}

func splitParagraphs(text string) []string {
	var result []string
	var current string
	lines := splitLines(text)
	for _, line := range lines {
		if line == "" && current != "" {
			result = append(result, current)
			current = ""
		} else {
			if current != "" {
				current += "\n"
			}
			current += line
		}
	}
	if current != "" {
		result = append(result, current)
	}
	return result
}

func splitLines(text string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(text); i++ {
		if text[i] == '\n' {
			lines = append(lines, text[start:i])
			start = i + 1
		}
	}
	if start < len(text) {
		lines = append(lines, text[start:])
	}
	return lines
}

func (s *Server) toolListProjects() MCPToolResult {
	projects := s.Registry.All()
	if len(projects) == 0 {
		return TextResult("No projects registered. Index a project first with index_project.")
	}

	type projectInfo struct {
		Name   string `json:"name"`
		Path   string `json:"path"`
		DBPath string `json:"dbPath"`
	}
	var list []projectInfo
	for _, p := range projects {
		list = append(list, projectInfo{
			Name:   p.Name,
			Path:   p.Path,
			DBPath: p.DBPath,
		})
	}

	out, _ := json.MarshalIndent(list, "", "  ")
	return TextResult(fmt.Sprintf("Registered projects (%d):\n%s", len(list), string(out)))
}

// resolveDBDir returns the DB directory path for a project. Always returns a path
// (never empty string). Caller is responsible for checking existence if needed.
func (s *Server) resolveDBDir(project string) string {
	if project != "" {
		if p := s.Registry.Find(project); p != nil {
			return p.DBPath
		}
	}
	cwd, _ := os.Getwd()
	if p := s.Registry.FindByCWD(cwd); p != nil {
		return p.DBPath
	}
	return filepath.Join(cwd, ".heimdall_db")
}

// resolveDBDirForRead returns the DB directory, or empty string if it doesn't exist on disk.
// Use for read-only tools that should not create the directory.
func (s *Server) resolveDBDirForRead(project string) string {
	dbDir := s.resolveDBDir(project)
	if _, err := os.Stat(dbDir); err != nil {
		return ""
	}
	return dbDir
}

// classifySource maps a VectorRecord.Kind to a source category.
func classifySource(kind string) string {
	switch kind {
	case "external":
		return "external"
	case "memory":
		return "memory"
	default:
		return "code"
	}
}

// validSourceTypes is the set of accepted type values for index_text.
var validSourceTypes = map[string]bool{
	"ticket": true, "doc": true, "pr": true, "message": true,
	"changelog": true, "note": true, "custom": true,
}

// validateSourceType validates and normalizes the source type.
// Returns the validated type, or empty string if invalid.
func validateSourceType(t string) string {
	if t == "" {
		return "custom"
	}
	if validSourceTypes[t] {
		return t
	}
	return ""
}

// sourceFieldPattern validates the source identifier field.
var sourceFieldPattern = regexp.MustCompile(`^[a-zA-Z0-9_.:#/@\-\s]+$`)

// validateSource validates the source field for path traversal and other attacks.
func validateSource(source string) error {
	if len(source) > 500 {
		return fmt.Errorf("source too long (max 500 chars)")
	}
	if strings.Contains(source, "..") {
		return fmt.Errorf("source must not contain path traversal sequences")
	}
	if strings.ContainsAny(source, "\x00\n\r") {
		return fmt.Errorf("source must not contain null bytes or newlines")
	}
	if !sourceFieldPattern.MatchString(source) {
		return fmt.Errorf("source contains invalid characters")
	}
	return nil
}

func (s *Server) toolExplain(args json.RawMessage) MCPToolResult {
	var input explainInput
	if err := json.Unmarshal(args, &input); err != nil {
		return ErrResult("invalid arguments: " + err.Error())
	}
	if input.Query == "" {
		return ErrResult("query is required")
	}
	if input.Limit <= 0 {
		input.Limit = 10
	}

	ctx := context.Background()
	client := heimdall.NewOllamaClient(s.Cfg.OllamaEndpoint)
	if err := client.Ping(ctx); err != nil {
		return ErrResult("Ollama not reachable: " + err.Error())
	}

	// Resolve DB path (read-only)
	dbDir := s.resolveDBDirForRead(input.Project)
	if dbDir == "" {
		return ErrResult("No index found. Run index_project first.")
	}

	store, err := heimdall.OpenStore(dbDir)
	if err != nil {
		return ErrResult("store error: " + err.Error())
	}
	defer store.Close()

	embedder := heimdall.NewOllamaEmbedder(client, s.Cfg.Model)

	// 1. Embed the query
	queryVec, err := embedder.Embed(ctx, input.Query)
	if err != nil {
		return ErrResult("embedding error: " + err.Error())
	}

	// 2. Search ALL chunks (topK=0) and time it
	start := time.Now()
	allResults := store.Search(queryVec, 0)
	searchMs := time.Since(start).Milliseconds()

	// 3. Compute score distribution and source counts
	dist := ScoreDistribution{}
	counts := SourceCounts{}
	for _, r := range allResults {
		score := r.Similarity
		switch {
		case score >= 0.90:
			dist.Above90++
		case score >= 0.70:
			dist.Above70++
		case score >= 0.50:
			dist.Above50++
		default:
			dist.Below50++
		}

		source := classifySource(r.Record.Kind)
		switch source {
		case "code":
			counts.Code++
		case "external":
			counts.External++
		case "memory":
			counts.Memory++
		}
	}

	// 4. Build top-N result items
	topN := input.Limit
	if topN > len(allResults) {
		topN = len(allResults)
	}
	var items []ExplainResultItem
	for i := 0; i < topN; i++ {
		r := allResults[i]
		item := ExplainResultItem{
			Rank:           i + 1,
			ChunkID:        r.Record.ID,
			File:           r.Record.FilePath,
			Score:          r.Similarity,
			Source:         classifySource(r.Record.Kind),
			ChunkSizeBytes: len(r.Record.Content),
			ChunkLines:     r.Record.EndLine - r.Record.StartLine + 1,
		}
		// Relationship traversal
		item.Related = s.resolveRelationships(store, r.Record)
		items = append(items, item)
	}

	// 5. Index stats
	stats := store.Stats()
	lastIndexed := "never"
	if stats.LastModified > 0 {
		lastIndexed = time.Unix(stats.LastModified, 0).Format("2006-01-02 15:04:05")
	}

	result := ExplainResult{
		Query:               input.Query,
		QueryEmbeddingDim:   len(queryVec),
		TotalChunksSearched: len(allResults),
		SearchTimeMs:        searchMs,
		Results:             items,
		ScoreDistribution:   dist,
		SourceCounts:        counts,
		IndexStats: IndexStatsInfo{
			TotalChunks: stats.TotalRecords,
			TotalFiles:  stats.TotalFiles,
			LastIndexed: lastIndexed,
		},
	}

	explainOut, _ := json.MarshalIndent(result, "", "  ")
	return TextResult(string(explainOut))
}

// resolveRelationships looks up related entries for a record that has relationships.
func (s *Server) resolveRelationships(store *heimdall.VectorStore, rec heimdall.VectorRecord) []RelatedItem {
	if rec.Relationships == "" || rec.Relationships == "[]" {
		return nil
	}

	var rels []struct {
		Type   string `json:"type"`
		Target string `json:"target"`
	}
	if err := json.Unmarshal([]byte(rec.Relationships), &rels); err != nil {
		return nil
	}

	var items []RelatedItem
	for _, rel := range rels {
		snippet := store.SnippetBySource(rel.Target)
		items = append(items, RelatedItem{
			Source:       rel.Target,
			Relationship: rel.Type,
			Snippet:      snippet,
		})
	}
	return items
}
