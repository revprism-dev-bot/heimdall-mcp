// Package mcp implements the JSON-RPC based MCP server for Heimdall.
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/caio-silva/heimdall-mcp/internal/config"
	"github.com/caio-silva/heimdall-mcp/internal/heimdall"
	"github.com/caio-silva/heimdall-mcp/internal/registry"
)

// Server holds runtime state for the MCP server.
type Server struct {
	Cfg      config.Config
	Registry *registry.Registry
	Index    IndexState
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
			Description: "Search indexed project files for code relevant to a query. Uses local Ollama embeddings and a SQLite vector store.",
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
			Description: "Index arbitrary text content for semantic search. Use this to store context from external sources (Jira tickets, Confluence pages, Slack messages, changelogs, etc.) so it can be found via heimdall_search alongside code.",
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

	ctx := context.Background()
	client := heimdall.NewOllamaClient(s.Cfg.OllamaEndpoint)
	if err := client.Ping(ctx); err != nil {
		return ErrResult("Ollama not reachable: " + err.Error())
	}

	// Resolve DB path — same logic as search
	var dbDir string
	if input.Project != "" {
		if p := s.Registry.Find(input.Project); p != nil {
			dbDir = p.DBPath
		}
	}
	if dbDir == "" {
		cwd, _ := os.Getwd()
		if p := s.Registry.FindByCWD(cwd); p != nil {
			dbDir = p.DBPath
		} else {
			dbDir = filepath.Join(cwd, ".heimdall_db")
		}
	}

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
			ID:          fmt.Sprintf("ext:%s:%d", input.Source, i),
			FilePath:    input.Source,
			StartLine:   chunk.Start,
			EndLine:     chunk.End,
			Content:     chunk.Content,
			Kind:        "external",
			Identifier:  input.Source,
			Embedding:   vec,
			ModTime:     time.Now().Unix(),
			ContentHash: "",
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
