package mcp

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/caio-silva/heimdall-mcp/internal/heimdall"
)

// --- JSON-RPC / MCP types ---

// JSONRPCRequest represents an incoming JSON-RPC request.
type JSONRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// JSONRPCResponse represents an outgoing JSON-RPC response.
type JSONRPCResponse struct {
	JSONRPC string    `json:"jsonrpc"`
	ID      any       `json:"id,omitempty"`
	Result  any       `json:"result,omitempty"`
	Error   *RPCError `json:"error,omitempty"`
}

// RPCError represents a JSON-RPC error.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// MCPToolInfo describes a tool exposed via MCP.
type MCPToolInfo struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// MCPToolResult is the result of a tool call.
type MCPToolResult struct {
	Content []MCPContent `json:"content"`
	IsError bool         `json:"isError,omitempty"`
}

// MCPContent is a single content block in a tool result.
type MCPContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// --- Tool input types ---

type searchInput struct {
	Query   string `json:"query"`
	Limit   int    `json:"limit"`
	Project string `json:"project"` // optional: project name or path
}

type indexInput struct {
	Path string `json:"path"`
}

type indexTextInput struct {
	Content string `json:"content"` // the text to index
	Source  string `json:"source"`  // identifier (e.g. "JIRA-123", "confluence:page-title", "slack:#channel")
	URL     string `json:"url"`     // optional link to the original source
	Project string `json:"project"` // which project to store it under (optional, uses CWD)
}

// --- Index state ---

// IndexState tracks the state of a background indexing operation.
type IndexState struct {
	Mu         sync.Mutex
	Running    bool
	Path       string
	Current    int
	Total      int
	StartedAt  time.Time
	LastUpdate time.Time
	Result     *heimdall.IndexResult
	Err        error
	Cancel     context.CancelFunc
}
