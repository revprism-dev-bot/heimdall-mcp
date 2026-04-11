# Phase 3: Retrieval Debugging + Typed External Context — Implementation Plan

**Date:** 2026-04-11
**Status:** Draft
**Depends on:** Phase 1 (Rename openviking → heimdall) completed first
**Spec:** `docs/superpowers/specs/2026-04-11-heimdall-evolution-design.md` sections 3 + 4

---

## Overview

This phase adds two related feature sets to the renamed heimdall-mcp codebase:

1. **Retrieval Debugging** — Enriched search results with scores/source/chunkId/embeddingModel, plus a new `heimdall_explain` diagnostic tool
2. **Typed External Context** — Upgraded `heimdall_index_text` with type/metadata/relationships fields, filtered search, and relationship traversal in explain results

Both features share DB schema changes (new columns on `entries` table) and modifications to the search pipeline, so they are planned together.

---

## Part A: Retrieval Debugging

### A1. Enriched Search Results (`heimdall_search`)

**Goal:** Every search result includes `score`, `source`, `chunkId`, `embeddingModel` alongside existing fields.

#### Current State

The `toolSearch` method in `internal/mcp/tools.go` already returns `score` in its result struct (lines 81-97). The current result shape is:

```go
type result struct {
    File      string  `json:"file"`
    StartLine int     `json:"startLine"`
    EndLine   int     `json:"endLine"`
    Content   string  `json:"content"`
    Score     float64 `json:"score"`
}
```

**Missing fields:** `source`, `chunkId`, `embeddingModel`.

#### Changes Required

**File: `internal/mcp/types.go`**

Add a new `SearchResultEnriched` struct (or expand inline in tools.go):

```go
// SearchResultEnriched is the enriched result returned by heimdall_search.
type SearchResultEnriched struct {
    File           string  `json:"file"`
    StartLine      int     `json:"startLine"`
    EndLine        int     `json:"endLine"`
    Content        string  `json:"content"`
    Score          float64 `json:"score"`
    Source         string  `json:"source"`         // "code", "external", "memory"
    ChunkID        string  `json:"chunkId"`
    EmbeddingModel string  `json:"embeddingModel"`
}
```

**File: `internal/mcp/tools.go` — `toolSearch` method**

Replace the anonymous `result` struct with `SearchResultEnriched`. Derive fields:

- `Source`: Derive from `VectorRecord.Kind` field:
  - `kind == "external"` → `source = "external"`
  - `kind == "memory"` → `source = "memory"` (future-proofing for Phase 2 session memory)
  - everything else (`"file"`, `"paragraph"`, `"function"`, `"type"`) → `source = "code"`
- `ChunkID`: Already exists as `VectorRecord.ID` (format: `"relpath:startLine:endLine"` for code, `"ext:source:idx"` for external)
- `EmbeddingModel`: Read from `s.Cfg.Model` (the configured embedding model)

```go
for _, b := range blocks {
    source := "code"
    if b.Kind == "external" {
        source = "external"
    } else if b.Kind == "memory" {
        source = "memory"
    }
    results = append(results, SearchResultEnriched{
        File:           b.FilePath,
        StartLine:      b.StartLine,
        EndLine:        b.EndLine,
        Content:        b.Content,
        Score:          b.Score,
        Source:         source,
        ChunkID:        chunkID, // need to thread through from VectorRecord.ID
        EmbeddingModel: s.Cfg.Model,
    })
}
```

**File: `internal/heimdall/retriever.go` — `ContextBlock` struct**

The `ContextBlock` struct currently lacks the record ID. Add `ChunkID string` field so it can be propagated from `VectorRecord.ID` through the retriever:

```go
type ContextBlock struct {
    FilePath   string
    StartLine  int
    EndLine    int
    Content    string
    Kind       string
    Identifier string
    Score      float64
    ChunkID    string  // NEW: the VectorRecord.ID for this chunk
}
```

Update `Retrieve()` to populate `ChunkID` from `result.Record.ID`.

**File: `internal/mcp/server.go` — `handleToolsList`**

Update the `heimdall_search` tool description to mention the enriched fields.

#### searchInput Changes

The `searchInput` struct in `types.go` gains optional filter fields (see Part B below). For Part A, no changes needed to the input — only the output is enriched.

---

### A2. New Tool: `heimdall_explain`

**Goal:** Deep diagnostic for a query — returns embedding dimensions, total chunks searched, timing, score distribution, source counts, index stats, and (for Part B) relationship traversal.

#### New Types

**File: `internal/mcp/types.go`**

```go
type explainInput struct {
    Query   string `json:"query"`
    Limit   int    `json:"limit"`   // default 10
    Project string `json:"project"` // optional
}

// ExplainResult is the full diagnostic output.
type ExplainResult struct {
    Query             string              `json:"query"`
    QueryEmbeddingDim int                 `json:"queryEmbeddingDim"`
    TotalChunksSearched int              `json:"totalChunksSearched"`
    SearchTimeMs      int64               `json:"searchTimeMs"`
    Results           []ExplainResultItem `json:"results"`
    ScoreDistribution ScoreDistribution   `json:"scoreDistribution"`
    SourceCounts      SourceCounts        `json:"sourceCounts"`
    IndexStats        IndexStatsInfo      `json:"indexStats"`
}

type ExplainResultItem struct {
    Rank           int     `json:"rank"`
    ChunkID        string  `json:"chunkId"`
    File           string  `json:"file"`
    Score          float64 `json:"score"`
    Source         string  `json:"source"`
    ChunkSizeBytes int     `json:"chunkSizeBytes"`
    ChunkLines     int     `json:"chunkLines"`
    Related        []RelatedItem `json:"related,omitempty"` // Part B: relationship traversal
}

type ScoreDistribution struct {
    Above90 int `json:"above90"`
    Above70 int `json:"above70"`
    Above50 int `json:"above50"`
    Below50 int `json:"below50"`
}

type SourceCounts struct {
    Code     int `json:"code"`
    External int `json:"external"`
    Memory   int `json:"memory"`
}

type IndexStatsInfo struct {
    TotalChunks int    `json:"totalChunks"`
    TotalFiles  int    `json:"totalFiles"`
    LastIndexed string `json:"lastIndexed"`
}

// RelatedItem is a relationship-traversal result (Part B).
type RelatedItem struct {
    Source       string `json:"source"`
    Relationship string `json:"relationship"`
    Snippet      string `json:"snippet"`
}
```

#### New Store Method: `SearchAll`

The explain tool needs access to ALL search results (not just top-K) to compute score distribution and source counts. The current `Search()` method takes `topK` and truncates.

**File: `internal/heimdall/store.go`**

Add a new method that returns all results with scores, plus the total count:

```go
// SearchAll returns all records with their similarity scores, sorted descending.
// Unlike Search(), it does not truncate to topK — the caller decides.
func (s *VectorStore) SearchAll(query []float32) []SearchResult {
    s.mu.RLock()
    defer s.mu.RUnlock()

    rows, err := s.db.Query(`SELECT id, file_path, start_line, end_line, content, kind, identifier, vector, mod_time FROM entries`)
    if err != nil {
        return nil
    }
    defer rows.Close()

    var results []SearchResult
    for rows.Next() {
        var rec VectorRecord
        var vecBlob []byte
        var kind, identifier sql.NullString
        if err := rows.Scan(&rec.ID, &rec.FilePath, &rec.StartLine, &rec.EndLine, &rec.Content, &kind, &identifier, &vecBlob, &rec.ModTime); err != nil {
            continue
        }
        rec.Kind = kind.String
        rec.Identifier = identifier.String
        rec.Embedding = decodeFloat32Vec(vecBlob)
        sim := cosineSimilarity(query, rec.Embedding)
        results = append(results, SearchResult{Record: rec, Similarity: sim})
    }

    sort.Slice(results, func(i, j int) bool {
        return results[i].Similarity > results[j].Similarity
    })
    return results
}
```

**Alternative (preferred for performance):** Instead of adding `SearchAll`, modify the existing `Search` to accept `topK=0` meaning "return all". This avoids duplicating the scan logic. The explain tool calls `Search(queryVec, 0)` and gets everything.

**Recommendation:** Modify `Search` to treat `topK <= 0` as "return all results" — this is a one-line change (`if topK > 0 && len(results) > topK`... which is already the current behavior at line 123-125). The existing code already does this:

```go
if topK > 0 && len(results) > topK {
    results = results[:topK]
}
```

So calling `Search(queryVec, 0)` already returns all results. No new method needed. The explain tool simply calls `store.Search(queryVec, 0)`.

#### Tool Implementation

**File: `internal/mcp/tools.go`**

New method `toolExplain`:

```go
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

    // Resolve DB path (same pattern as toolSearch)
    dbDir := resolveDBDir(s, input.Project)
    if dbDir == "" {
        return ErrResult("No index found.")
    }

    store, err := heimdall.OpenStore(dbDir)
    if err != nil {
        return ErrResult("store error: " + err.Error())
    }
    defer store.Close()

    embedder := heimdall.NewOllamaEmbedder(client, s.Cfg.Model)

    // 1. Embed the query and measure time
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
        // Part B: relationship traversal (see below)
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
        Query:             input.Query,
        QueryEmbeddingDim: len(queryVec),
        TotalChunksSearched: len(allResults),
        SearchTimeMs:      searchMs,
        Results:           items,
        ScoreDistribution: dist,
        SourceCounts:      counts,
        IndexStats: IndexStatsInfo{
            TotalChunks: stats.TotalRecords,
            TotalFiles:  stats.TotalFiles,
            LastIndexed: lastIndexed,
        },
    }

    out, _ := json.MarshalIndent(result, "", "  ")
    return TextResult(string(out))
}
```

#### Helper: `classifySource`

```go
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
```

#### Helper: `resolveDBDir`

Extract the repeated DB-resolution logic from `toolSearch`, `toolIndexText`, and `toolExplain` into a shared helper:

```go
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
    dbDir := filepath.Join(cwd, ".heimdall_db")
    if _, err := os.Stat(dbDir); err == nil {
        return dbDir
    }
    return ""
}
```

#### Register in `handleToolsList` and `handleToolsCall`

**File: `internal/mcp/server.go`**

Add tool definition for `heimdall_explain`:

```go
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
```

Add case in `handleToolsCall`:

```go
case "heimdall_explain":
    result = s.toolExplain(params.Arguments)
```

---

## Part B: Typed External Context

### B1. DB Schema Changes

**Goal:** Add `source_type`, `metadata`, `relationships` columns to the `entries` table.

#### Migration Strategy

**File: `internal/heimdall/store.go` — `OpenStore` function**

Add ALTER TABLE statements after the existing `content_hash` migration (line 82-83 in current store.go). SQLite's `ALTER TABLE ADD COLUMN` is idempotent when wrapped in an error-ignoring call (existing pattern used for `content_hash`):

```go
// Migrate: add typed context columns if missing (existing DBs).
db.Exec(`ALTER TABLE entries ADD COLUMN source_type TEXT DEFAULT 'code'`)
db.Exec(`ALTER TABLE entries ADD COLUMN metadata TEXT DEFAULT '{}'`)
db.Exec(`ALTER TABLE entries ADD COLUMN relationships TEXT DEFAULT '[]'`)

// Index for filtered search by source_type
db.Exec(`CREATE INDEX IF NOT EXISTS idx_source_type ON entries(source_type)`)
```

This approach:
- Is safe on existing DBs (ALTER TABLE ADD COLUMN with DEFAULT works on populated tables)
- Is idempotent (ignores "duplicate column" errors, same pattern as `content_hash` migration)
- Requires no version tracking — each migration is self-contained
- New records get proper defaults; existing records get `'code'`, `'{}'`, `'[]'`

#### VectorRecord Changes

**File: `internal/heimdall/store.go`**

Add fields to `VectorRecord`:

```go
type VectorRecord struct {
    ID            string    `json:"id"`
    FilePath      string    `json:"filePath"`
    StartLine     int       `json:"startLine"`
    EndLine       int       `json:"endLine"`
    Content       string    `json:"content"`
    Kind          string    `json:"kind"`
    Identifier    string    `json:"identifier"`
    Embedding     []float32 `json:"embedding"`
    ModTime       int64     `json:"modTime"`
    ContentHash   string    `json:"contentHash"`
    SourceType    string    `json:"sourceType"`    // NEW: "code", "ticket", "doc", "pr", etc.
    Metadata      string    `json:"metadata"`      // NEW: JSON object string
    Relationships string    `json:"relationships"` // NEW: JSON array string
}
```

#### Update Upsert

Modify the `INSERT OR REPLACE` in `Upsert` to include the new columns:

```go
stmt, err := tx.Prepare(`
    INSERT OR REPLACE INTO entries
        (id, file_path, start_line, end_line, content, kind, identifier, vector, mod_time, content_hash, source_type, metadata, relationships)
    VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
`)
// ...
stmt.Exec(r.ID, r.FilePath, r.StartLine, r.EndLine, r.Content, r.Kind, r.Identifier,
    vecBlob, r.ModTime, r.ContentHash, r.SourceType, r.Metadata, r.Relationships)
```

#### Update Search/SearchAll Queries

Modify the SELECT in `Search` to include the new columns:

```go
rows, err := s.db.Query(`SELECT id, file_path, start_line, end_line, content, kind, identifier, vector, mod_time, source_type, metadata, relationships FROM entries`)
```

**Note (LOW-2 fix):** `store.go` needs `"encoding/json"` and `"bytes"` added to its imports for
the `matchesMetadata` helper that uses `json.Marshal` and `bytes.Equal`.

And scan them:

```go
var sourceType, metadata, relationships sql.NullString
if err := rows.Scan(&rec.ID, &rec.FilePath, &rec.StartLine, &rec.EndLine, &rec.Content,
    &kind, &identifier, &vecBlob, &rec.ModTime, &sourceType, &metadata, &relationships); err != nil {
    continue
}
rec.SourceType = sourceType.String
rec.Metadata = metadata.String
rec.Relationships = relationships.String
```

### B2. Enhanced `heimdall_index_text`

**Goal:** Accept `type`, `metadata`, `relationships` fields.

#### Input Type Changes

**File: `internal/mcp/types.go`**

```go
type indexTextInput struct {
    Content       string          `json:"content"`
    Source        string          `json:"source"`
    URL           string          `json:"url"`
    Project       string          `json:"project"`
    Type          string          `json:"type"`          // NEW: ticket, doc, pr, message, changelog, note, custom
    Metadata      json.RawMessage `json:"metadata"`      // NEW: arbitrary JSON object
    Relationships json.RawMessage `json:"relationships"` // NEW: array of {type, target}
}
```

#### Validation

Valid `type` values: `ticket`, `doc`, `pr`, `message`, `changelog`, `note`, `custom`. Default to `"custom"` if empty. Reject unknown values.

```go
var validSourceTypes = map[string]bool{
    "ticket": true, "doc": true, "pr": true, "message": true,
    "changelog": true, "note": true, "custom": true,
}

func validateSourceType(t string) string {
    if t == "" {
        return "custom"
    }
    if validSourceTypes[t] {
        return t
    }
    return "" // invalid
}
```

#### Tool Changes

**File: `internal/mcp/server.go` — `toolIndexText`**

After parsing input, validate and pass through the new fields:

```go
sourceType := validateSourceType(input.Type)
if sourceType == "" {
    return ErrResult("invalid type: must be one of ticket, doc, pr, message, changelog, note, custom")
}

metadataStr := "{}"
if len(input.Metadata) > 0 {
    // Validate it's valid JSON object
    var obj map[string]any
    if err := json.Unmarshal(input.Metadata, &obj); err != nil {
        return ErrResult("invalid metadata: must be a JSON object: " + err.Error())
    }
    metadataStr = string(input.Metadata)
}

relationshipsStr := "[]"
if len(input.Relationships) > 0 {
    // Validate it's a valid JSON array of {type, target}
    var rels []struct {
        Type   string `json:"type"`
        Target string `json:"target"`
    }
    if err := json.Unmarshal(input.Relationships, &rels); err != nil {
        return ErrResult("invalid relationships: must be an array of {type, target}: " + err.Error())
    }
    relationshipsStr = string(input.Relationships)
}
```

Then when building `VectorRecord` in the chunking loop:

```go
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
    SourceType:    sourceType,       // NEW
    Metadata:      metadataStr,      // NEW
    Relationships: relationshipsStr, // NEW
})
```

#### Updated Tool Schema

Add new properties to the `heimdall_index_text` tool definition:

```go
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
    "type": "array",
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
```

### B3. Filtered Search

**Goal:** `heimdall_search` gains optional `source_type` and `metadata_filter` params.

#### searchInput Changes

**File: `internal/mcp/types.go`**

```go
type searchInput struct {
    Query          string          `json:"query"`
    Limit          int             `json:"limit"`
    Project        string          `json:"project"`
    SourceType     string          `json:"source_type"`     // NEW: filter by source type
    MetadataFilter json.RawMessage `json:"metadata_filter"` // NEW: filter by metadata key-value pairs
}
```

#### Filtering Strategy

Two-phase approach: **SQL WHERE for source_type** + **post-filter for metadata**.

**Why not SQL for metadata?** The metadata column is a JSON string. While SQLite supports `json_extract()`, doing cosine similarity in Go means we're already iterating all rows. Adding a WHERE clause for `source_type` reduces the scan set, but metadata filtering is more efficiently done post-scan alongside the cosine computation.

**File: `internal/heimdall/store.go` — Refactored search (CORR-6 fix: eliminate duplication)**

**Architecture decision:** `SearchFiltered` becomes the single search implementation. The existing
`Search(query, topK)` becomes a convenience wrapper: `SearchFiltered(query, topK, "", nil)`. This
eliminates ~60 lines of duplicated scan/decode/sort logic.

```go
// Search is a convenience wrapper — delegates to SearchFiltered with no filters.
func (s *VectorStore) Search(query []float32, topK int) []SearchResult {
    return s.SearchFiltered(query, topK, "", nil)
}

// SearchFiltered is the single search implementation. Supports optional source_type
// pre-filter (SQL WHERE) and metadata post-filter (JSON comparison).
func (s *VectorStore) SearchFiltered(query []float32, topK int, sourceType string, metadataFilter map[string]any) []SearchResult {
    s.mu.RLock()
    defer s.mu.RUnlock()

    // Build query with optional source_type WHERE clause
    sqlQuery := `SELECT id, file_path, start_line, end_line, content, kind, identifier, vector, mod_time, source_type, metadata, relationships FROM entries`
    var args []any
    if sourceType != "" {
        sqlQuery += ` WHERE source_type = ?`
        args = append(args, sourceType)
    }

    rows, err := s.db.Query(sqlQuery, args...)
    if err != nil {
        return nil
    }
    defer rows.Close()

    var results []SearchResult
    for rows.Next() {
        var rec VectorRecord
        var vecBlob []byte
        var kind, identifier, srcType, meta, rels sql.NullString
        if err := rows.Scan(&rec.ID, &rec.FilePath, &rec.StartLine, &rec.EndLine, &rec.Content,
            &kind, &identifier, &vecBlob, &rec.ModTime, &srcType, &meta, &rels); err != nil {
            continue
        }
        rec.Kind = kind.String
        rec.Identifier = identifier.String
        rec.SourceType = srcType.String
        rec.Metadata = meta.String
        rec.Relationships = rels.String
        rec.Embedding = decodeFloat32Vec(vecBlob)

        // Post-filter: metadata match
        if len(metadataFilter) > 0 && !matchesMetadata(rec.Metadata, metadataFilter) {
            continue
        }

        sim := cosineSimilarity(query, rec.Embedding)
        results = append(results, SearchResult{Record: rec, Similarity: sim})
    }

    sort.Slice(results, func(i, j int) bool {
        return results[i].Similarity > results[j].Similarity
    })
    if topK > 0 && len(results) > topK {
        results = results[:topK]
    }
    return results
}

// matchesMetadata checks if the record's JSON metadata contains all key-value pairs in the filter.
// Uses JSON serialization for type-safe comparison (SEC-9 fix: no fmt.Sprintf("%v") coercion).
func matchesMetadata(metadataJSON string, filter map[string]any) bool {
    if metadataJSON == "" || metadataJSON == "{}" {
        return false
    }
    var meta map[string]any
    if err := json.Unmarshal([]byte(metadataJSON), &meta); err != nil {
        return false
    }
    for k, v := range filter {
        val, ok := meta[k]
        if !ok {
            return false
        }
        // Type-safe comparison via JSON round-trip (avoids "1" == 1 coercion)
        expected, _ := json.Marshal(v)
        actual, _ := json.Marshal(val)
        if !bytes.Equal(expected, actual) {
            return false
        }
    }
    return true
}
```

#### Update toolSearch

When `SourceType` or `MetadataFilter` is present, use `SearchFiltered` instead of the regular `Retrieve` path:

```go
func (s *Server) toolSearch(args json.RawMessage) MCPToolResult {
    // ... parse input ...

    // If filters are present, use filtered search path
    if input.SourceType != "" || len(input.MetadataFilter) > 0 {
        return s.toolSearchFiltered(input, dbDir)
    }

    // Otherwise, use existing retriever path (unchanged)
    // ...
}

func (s *Server) toolSearchFiltered(input searchInput, dbDir string) MCPToolResult {
    ctx := context.Background()
    client := heimdall.NewOllamaClient(s.Cfg.OllamaEndpoint)
    embedder := heimdall.NewOllamaEmbedder(client, s.Cfg.Model)

    store, err := heimdall.OpenStore(dbDir)
    if err != nil {
        return ErrResult("store error: " + err.Error())
    }
    defer store.Close()

    queryVec, err := embedder.Embed(ctx, input.Query)
    if err != nil {
        return ErrResult("embedding error: " + err.Error())
    }

    var metaFilter map[string]any
    if len(input.MetadataFilter) > 0 {
        json.Unmarshal(input.MetadataFilter, &metaFilter)
    }

    results := store.SearchFiltered(queryVec, input.Limit, input.SourceType, metaFilter)

    // Build enriched results
    var enriched []SearchResultEnriched
    for _, r := range results {
        enriched = append(enriched, SearchResultEnriched{
            File:           r.Record.FilePath,
            StartLine:      r.Record.StartLine,
            EndLine:        r.Record.EndLine,
            Content:        r.Record.Content,
            Score:          r.Similarity,
            Source:         classifySource(r.Record.Kind),
            ChunkID:        r.Record.ID,
            EmbeddingModel: s.Cfg.Model,
        })
    }

    out, _ := json.MarshalIndent(enriched, "", "  ")
    return TextResult(string(out))
}
```

#### Updated Tool Schema

Add new optional properties to `heimdall_search`:

```go
"source_type": map[string]any{
    "type":        "string",
    "description": "Filter by source type: code, ticket, doc, pr, message, changelog, note, custom",
},
"metadata_filter": map[string]any{
    "type":        "object",
    "description": "Filter by metadata key-value pairs (e.g. {\"status\": \"in_progress\"})",
},
```

### B4. Relationship Traversal in `heimdall_explain`

**Goal:** When explain results have relationships, include related items with snippets.

#### Implementation

**File: `internal/mcp/tools.go`**

New method on Server:

```go
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
        // Look up the target in the store by file_path (source identifier)
        snippet := store.SnippetBySource(rel.Target)
        items = append(items, RelatedItem{
            Source:       rel.Target,
            Relationship: rel.Type,
            Snippet:      snippet,
        })
    }
    return items
}
```

**File: `internal/heimdall/store.go`**

New method:

```go
// SnippetBySource returns the first 200 characters of content for entries matching the given file_path (source identifier).
func (s *VectorStore) SnippetBySource(source string) string {
    s.mu.RLock()
    defer s.mu.RUnlock()

    var content string
    s.db.QueryRow(`SELECT content FROM entries WHERE file_path = ? LIMIT 1`, source).Scan(&content)
    if len(content) > 200 {
        content = content[:200] + "..."
    }
    return content
}
```

---

## Part C: Shared Refactoring

### C1. Extract `resolveDBDir` Helper (ARCH-3 fix)

The DB directory resolution logic is duplicated across `toolSearch`, `toolIndexText`, `toolStatus`, and will be needed by `toolExplain`. Extract into a shared method.

**Design decision (ARCH-3 fix):** `resolveDBDir` always returns the fallback path — it does NOT
check `os.Stat`. The *caller* decides whether to verify existence:
- **Read-only tools** (search, explain, status): check `os.Stat` on the returned path and return
  an error if the DB doesn't exist.
- **Write tools** (index, index_text): use the path directly — `OpenStore` creates the directory
  via `os.MkdirAll`. This preserves the existing behavior where `toolIndexText` creates the DB
  on first use.

```go
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
```

Update `toolSearch` and `toolExplain` to use `resolveDBDirForRead`. Update `toolIndexText` to use `resolveDBDir`.

### C2. Ensure Existing Code Sets New Fields

When the indexer creates `VectorRecord` for code files (in `indexer.go`), set the new fields:

```go
records = append(records, VectorRecord{
    // ... existing fields ...
    SourceType:    "code",  // NEW: always "code" for indexed files
    Metadata:      "{}",    // NEW: empty object
    Relationships: "[]",    // NEW: empty array
})
```

When `toolIndexText` creates records for external content, set `SourceType` from the input `Type` field (or default to `"custom"`).

---

## Part D: Coordination with Other Phases

### Shared DB Schema with Session Memory (Phase 2)

Phase 2 (Session Memory) adds a **new `memories` table** — it does NOT modify the `entries` table. The two phases are independent at the DB level:

- **This phase (Phase 3):** Adds columns to `entries` table (`source_type`, `metadata`, `relationships`)
- **Phase 2:** Creates a new `memories` table

No conflicts. Both can be developed in parallel.

However, the `heimdall_explain` tool's `sourceCounts` should include memory counts. Since memory will be in a separate table, the explain tool should query both tables:

```go
// Count memories separately (table may not exist if Phase 2 hasn't run yet)
var memoryCount int
_ = store.db.QueryRow(`SELECT COUNT(*) FROM memories`).Scan(&memoryCount)
counts.Memory = memoryCount

// Optionally, for per-project memory counts:
// _ = store.db.QueryRow(`SELECT COUNT(*) FROM memories WHERE project = ?`, project).Scan(&memoryCount)
```

**Implementation note:** The error-ignore pattern (`_ =`) is sufficient since the `memories` table uses `CREATE TABLE IF NOT EXISTS` and simply won't exist until Phase 2 runs. Confirmed with orch-plan-memory that the `memories` table schema is stable and the columns we need (`COUNT(*)`, optional `WHERE project = ?`) are finalized (see `docs/plans/phase2-memory.md` section 13).

### Naming Coordination (Phase 1)

All file paths, package names, tool names, and DB directory names in this plan use the POST-rename names (`heimdall_*`, `internal/heimdall/`, `.heimdall_db/`). Implementation should happen after Phase 1 rename completes.

---

## Performance Considerations

### `heimdall_explain` Performance

1. **Full table scan required:** The explain tool calls `Search(queryVec, 0)` which scans all entries. For large indexes (5000+ chunks), this could take 50-100ms. This is acceptable for a diagnostic tool — it's not meant for real-time use.

2. **Timing measurement:** Use `time.Now()` before/after the `Search` call. Report in `searchTimeMs`. The embedding time is NOT included (it's a separate operation measured separately if needed).

3. **Memory:** Loading all results into memory for score distribution. For 10K chunks, each `SearchResult` is ~2KB → ~20MB. Acceptable.

### Filtered Search Performance

1. **source_type SQL filter:** Adding `WHERE source_type = ?` with the `idx_source_type` index means SQLite can skip non-matching rows entirely before loading the vector blob. This significantly reduces the scan set for filtered queries.

2. **metadata post-filter:** JSON parsing per row is unavoidable without SQLite JSON extension. For most queries this is fast (metadata objects are small). If performance becomes an issue, consider adding a `metadata_index` column with pre-computed filter keys.

### Relationship Traversal Performance

Each relationship lookup is a single-row query by `file_path`. With the existing `idx_file_path` index, this is O(1). A result with 3 relationships = 3 indexed queries = negligible overhead.

---

## Test Strategy

### Unit Tests

**File: `internal/heimdall/store_test.go` (new)**

1. **TestSearchFiltered_BySourceType** — Insert records with different source_types, verify filtering works
2. **TestSearchFiltered_ByMetadata** — Insert records with metadata, verify key-value matching
3. **TestSearchFiltered_Combined** — Both source_type and metadata filters together
4. **TestMatchesMetadata** — Edge cases: empty metadata, partial match, type coercion
5. **TestSnippetBySource** — Verify snippet lookup and truncation
6. **TestDBMigration_NewColumns** — Open store, verify new columns exist, insert/read records with new fields
7. **TestVectorRecordUpsert_WithNewFields** — Upsert records with source_type/metadata/relationships, read back

**File: `internal/mcp/tools_test.go` (new)**

1. **TestClassifySource** — Verify kind → source mapping
2. **TestValidateSourceType** — Valid and invalid type values
3. **TestResolveDBDir** — With project, with CWD, fallback

### Integration Tests

**File: `internal/mcp/integration_test.go` (new)**

These tests use `MockEmbedder` (already exists in `embedder.go`):

1. **TestToolSearch_EnrichedResults** — Call toolSearch, verify response includes score/source/chunkId/embeddingModel
2. **TestToolSearch_FilteredBySourceType** — Index code + external, search with source_type filter
3. **TestToolSearch_FilteredByMetadata** — Index typed external content with metadata, filter by status
4. **TestToolExplain_Basic** — Call explain, verify all fields in response
5. **TestToolExplain_ScoreDistribution** — Insert known vectors, verify distribution buckets
6. **TestToolExplain_SourceCounts** — Mix of code/external, verify counts
7. **TestToolExplain_RelationshipTraversal** — Index items with relationships, verify related items in explain output
8. **TestToolIndexText_TypedContent** — Index with type/metadata/relationships, verify stored correctly
9. **TestToolIndexText_ValidationErrors** — Invalid type, invalid metadata JSON, invalid relationships format

---

## Task Breakdown (Implementation Order)

### Task 1: DB Schema Migration (30 min)
- Add `source_type`, `metadata`, `relationships` columns to `OpenStore` migration
- Add `idx_source_type` index
- Update `VectorRecord` struct with new fields
- Update `Upsert` INSERT statement
- Update `Search` SELECT and scan

### Task 2: Indexer Default Values (15 min)
- Set `SourceType: "code"`, `Metadata: "{}"`, `Relationships: "[]"` in indexer record creation

### Task 3: Enhanced `heimdall_index_text` (45 min)
- Add `type`, `metadata`, `relationships` to `indexTextInput`
- Add validation logic
- Update record creation in `toolIndexText`
- Update tool schema in `handleToolsList`

### Task 4: Enriched Search Results (30 min)
- Add `ChunkID` to `ContextBlock`
- Update `Retrieve()` to populate it
- Replace anonymous result struct with `SearchResultEnriched`
- Add `source`, `chunkId`, `embeddingModel` to search output

### Task 5: Extract `resolveDBDir` Helper (15 min)
- Create shared helper
- Refactor `toolSearch`, `toolIndexText` to use it

### Task 6: Filtered Search (45 min)
- Add `source_type`, `metadata_filter` to `searchInput`
- Implement `SearchFiltered` on VectorStore
- Implement `matchesMetadata`
- Add filtered search path to `toolSearch`
- Update tool schema

### Task 7: `heimdall_explain` Tool (60 min)
- Add `explainInput` and all result types
- Implement `toolExplain` with timing, score distribution, source counts
- Implement `resolveRelationships` for Part B traversal
- Implement `SnippetBySource` on VectorStore
- Register tool in `handleToolsList` and `handleToolsCall`

### Task 8: Unit Tests (45 min)
- `store_test.go`: filtered search, metadata matching, snippet, migration
- `tools_test.go`: classifySource, validateSourceType, resolveDBDir

### Task 9: Integration Tests (60 min)
- Full tool-level tests with MockEmbedder
- Enriched results, filtered search, explain tool, typed content, validation

### Task 10: Documentation Update (15 min)
- Update README with new tool descriptions
- Update tool schema descriptions in code

**Total estimated implementation: ~6 hours**

---

## Files Changed Summary

| File (post-rename paths) | Changes |
|---|---|
| `internal/heimdall/store.go` | Add columns migration, new fields on VectorRecord, update Upsert/Search, add SearchFiltered, matchesMetadata, SnippetBySource |
| `internal/heimdall/retriever.go` | Add ChunkID to ContextBlock, populate in Retrieve() |
| `internal/heimdall/indexer.go` | Set default SourceType/Metadata/Relationships on code records |
| `internal/mcp/types.go` | Add SearchResultEnriched, explainInput, ExplainResult + sub-types, update searchInput + indexTextInput |
| `internal/mcp/server.go` | Register heimdall_explain tool, update heimdall_search + heimdall_index_text schemas, extract resolveDBDir, add toolExplain + toolSearchFiltered + classifySource + validateSourceType + resolveRelationships |
| `internal/mcp/tools.go` | Update toolSearch for enriched results + filtered path, update toolIndexText for typed content |
| `internal/heimdall/store_test.go` | NEW: unit tests for filtered search, metadata, migration |
| `internal/mcp/tools_test.go` | NEW: unit tests for helpers |
| `internal/mcp/integration_test.go` | NEW: integration tests for all new functionality |
