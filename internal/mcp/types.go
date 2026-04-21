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
	Query          string          `json:"query"`
	Limit          int             `json:"limit"`
	Project        string          `json:"project"`         // optional: project name or path
	SourceType     string          `json:"source_type"`     // optional: filter by source type
	SubProject     string          `json:"sub_project"`     // optional: filter by sub-project (sub-repo directory name)
	MetadataFilter json.RawMessage `json:"metadata_filter"` // optional: filter by metadata key-value pairs
	Detail         string          `json:"detail"`          // "summary", "snippet", "full" (default: "full")
	Scope          string          `json:"scope"`           // path prefix filter for context_path hierarchy
}

type expandInput struct {
	ChunkID string `json:"chunk_id"`
	Project string `json:"project"` // optional
}

type lsInput struct {
	Path    string `json:"path"`    // context path to list (empty = root)
	Project string `json:"project"` // optional
	// SubProject narrows the fanout on a wrapper project:
	//   "" (omitted)  → fan out across the anchor + every registered sub-repo
	//                   whose Path is under the anchor's Path.
	//   "__root__"    → anchor/wrapper only (same sentinel as the search
	//                   filter — see heimdall.SubProjectRoot).
	//   "<name>"      → restrict to the named registered sub-repo.
	// The wire contract matches the search tool's sub_project parameter so
	// callers use one mental model. When B-migration's SubProjectFilter
	// typed variant lands (docs/plans/2026-04-21-dispatch-plan/decisions.md
	// §Q4) this string field maps to that type at the MCP boundary without
	// breaking callers.
	SubProject string `json:"sub_project"`
}

// LsGroup aggregates one ls "section" contributed by a single registered
// project (the anchor wrapper, or one of its sub-repos). Emitted only when
// toolLs fans out across multiple registered projects — the single-project
// path keeps the pre-fanout flat `[]PathEntry` shape for backwards
// compatibility (see PR4 / Problem #4).
//
// Entries reuses heimdall.PathEntry directly so the per-row wire shape
// (name/path/chunkCount) is identical between the flat and grouped
// responses; callers that already parse flat ls output only need to learn
// one extra wrapper to parse the grouped form.
type LsGroup struct {
	SubProject  string                `json:"subProject"`  // "" for anchor/wrapper; sub-repo name otherwise
	ProjectName string                `json:"projectName"` // registry Name of the contributing project
	Entries     []heimdall.PathEntry  `json:"entries"`
}

type indexInput struct {
	Path string `json:"path"`
}

// statusInput carries the optional `path` parameter added in PR1 so
// callers can target a specific project without relying on the server's
// cwd. Resolution routes through s.resolveDBDir(path) (registry-first,
// cwd-last) — see server.go:resolveDBDir.
type statusInput struct {
	Path string `json:"path"`
}

type indexTextInput struct {
	Content       string          `json:"content"`       // the text to index
	Source        string          `json:"source"`        // identifier (e.g. "JIRA-123", "confluence:page-title")
	URL           string          `json:"url"`           // optional link to the original source
	Project       string          `json:"project"`       // which project to store it under (optional)
	Type          string          `json:"type"`          // content type: ticket, doc, pr, message, changelog, note, custom
	Metadata      json.RawMessage `json:"metadata"`      // arbitrary JSON object
	Relationships json.RawMessage `json:"relationships"` // array of {type, target}
}

type explainInput struct {
	Query   string `json:"query"`
	Limit   int    `json:"limit"`   // default 10
	Project string `json:"project"` // optional
}

// --- Enriched search result ---

// SearchResultEnriched is the enriched result returned by heimdall_search.
type SearchResultEnriched struct {
	File           string  `json:"file"`
	StartLine      int     `json:"startLine"`
	EndLine        int     `json:"endLine"`
	Content        string  `json:"content"`
	Score          float64 `json:"score"`
	Source         string  `json:"source"` // "code", "external", "memory"
	ChunkID        string  `json:"chunkId"`
	EmbeddingModel string  `json:"embeddingModel"`
	Summary        string  `json:"summary,omitempty"`
	ContextPath    string  `json:"contextPath,omitempty"`
}

// --- Explain result types ---

// ExplainResult is the full diagnostic output for heimdall_explain.
type ExplainResult struct {
	Query               string              `json:"query"`
	QueryEmbeddingDim   int                 `json:"queryEmbeddingDim"`
	TotalChunksSearched int                 `json:"totalChunksSearched"`
	SearchTimeMs        int64               `json:"searchTimeMs"`
	Results             []ExplainResultItem `json:"results"`
	ScoreDistribution   ScoreDistribution   `json:"scoreDistribution"`
	SourceCounts        SourceCounts        `json:"sourceCounts"`
	IndexStats          IndexStatsInfo      `json:"indexStats"`
}

// ExplainResultItem is a single result in the explain output.
type ExplainResultItem struct {
	Rank           int           `json:"rank"`
	ChunkID        string        `json:"chunkId"`
	File           string        `json:"file"`
	Score          float64       `json:"score"`
	Source         string        `json:"source"`
	ChunkSizeBytes int           `json:"chunkSizeBytes"`
	ChunkLines     int           `json:"chunkLines"`
	Related        []RelatedItem `json:"related,omitempty"`
}

// ScoreDistribution categorizes search results by score range.
type ScoreDistribution struct {
	Above90 int `json:"above90"`
	Above70 int `json:"above70"`
	Above50 int `json:"above50"`
	Below50 int `json:"below50"`
}

// SourceCounts tallies results by source type.
type SourceCounts struct {
	Code     int `json:"code"`
	External int `json:"external"`
	Memory   int `json:"memory"`
	Commit   int `json:"commit"`
}

// IndexStatsInfo holds high-level index statistics for explain output.
type IndexStatsInfo struct {
	TotalChunks int    `json:"totalChunks"`
	TotalFiles  int    `json:"totalFiles"`
	LastIndexed string `json:"lastIndexed"`
}

// RelatedItem is a relationship-traversal result.
type RelatedItem struct {
	Source       string `json:"source"`
	Relationship string `json:"relationship"`
	Snippet      string `json:"snippet"`
}

// --- Memory tool input types ---

type rememberInput struct {
	Content     string   `json:"content"`
	Tags        []string `json:"tags"`
	Type        string   `json:"type"` // "preference", "decision", "fact", "context", "skill"
	Project     string   `json:"project"`
	ContextPath string   `json:"context_path"` // optional slash-separated subpath within the project; auto-derived from CWD when empty
	// SkillName is the human-readable skill name. Used (when WriteFile
	// is true and Type=="skill") to derive the disk slug and the
	// SKILL.md frontmatter `name:` field. If empty, falls back to the
	// first line of Content with whitespace trimmed.
	SkillName string `json:"skill_name"`
	// SkillDescription is written into the frontmatter `description:`
	// field on outbound writes. Optional.
	SkillDescription string `json:"skill_description"`
	// WriteFile, when true and Type=="skill", additionally writes a
	// SKILL.md to <skills-dir>/<slug>/SKILL.md so the memory becomes an
	// always-loaded Claude Code skill. Default OFF — opt-in per call.
	// The skills dir resolves from $HEIMDALL_CLAUDE_SKILLS_DIR or
	// ~/.claude/skills.
	WriteFile bool `json:"write_file"`
	// WriteFileOverwrite, when true, replaces an existing SKILL.md at
	// the destination. Default false → tool returns an error noting the
	// file already exists. Has no effect unless WriteFile is also true.
	WriteFileOverwrite bool `json:"write_file_overwrite"`
}

type recallInput struct {
	Query   string   `json:"query"`
	Tags    []string `json:"tags"`
	Type    string   `json:"type"`
	Limit   int      `json:"limit"`
	Project string   `json:"project"`
	Scope   string   `json:"scope"` // prefix filter on Memory.ContextPath
}

type ingestSessionInput struct {
	Summary string `json:"summary"`
	Project string `json:"project"`
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
