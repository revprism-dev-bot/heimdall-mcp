# Phase 2: Session Memory System — Implementation Plan

**Date:** 2026-04-11
**Status:** Draft
**Scope:** `heimdall_remember`, `heimdall_recall`, `heimdall_ingest_session` tools
**Depends on:** Phase 1 (rename openviking -> heimdall) must complete first

---

## 1. Overview

Add persistent session memory to Heimdall: three new MCP tools that let Claude (or any MCP client) store, retrieve, and auto-extract memories across sessions. Memories are stored in the same SQLite database as code vectors but in a separate `memories` table, using the same BGE-M3 embedding model via Ollama.

---

## 2. New Types and Structs

All new types go in a new file: `internal/heimdall/memory.go`

```go
// MemoryType constrains the allowed memory types.
type MemoryType string

const (
    MemoryTypePreference MemoryType = "preference"
    MemoryTypeDecision   MemoryType = "decision"
    MemoryTypeFact       MemoryType = "fact"
    MemoryTypeContext    MemoryType = "context"
)

// ValidMemoryTypes for validation.
var ValidMemoryTypes = map[MemoryType]bool{
    MemoryTypePreference: true,
    MemoryTypeDecision:   true,
    MemoryTypeFact:       true,
    MemoryTypeContext:    true,
}

// MemorySource indicates how a memory was created.
type MemorySource string

const (
    MemorySourceExplicit MemorySource = "explicit"
    MemorySourceSession  MemorySource = "session"
)

// Memory represents a single stored memory.
type Memory struct {
    ID          string       `json:"id"`
    Content     string       `json:"content"`
    Type        MemoryType   `json:"type"`
    Tags        []string     `json:"tags,omitempty"`
    Project     string       `json:"project,omitempty"`
    Vector      []float32    `json:"-"`              // not serialized to JSON responses
    CreatedAt   int64        `json:"createdAt"`
    UpdatedAt   int64        `json:"updatedAt"`
    Source      MemorySource `json:"source"`
    ContentHash string       `json:"-"`
}

// MemorySearchResult is a memory with its similarity score.
type MemorySearchResult struct {
    Memory     Memory  `json:"memory"`
    Similarity float64 `json:"score"`
}
```

### New input types in `internal/mcp/types.go`

```go
type rememberInput struct {
    Content string   `json:"content"`
    Tags    []string `json:"tags"`
    Type    string   `json:"type"`    // "preference", "decision", "fact", "context"
    Project string   `json:"project"`
}

type recallInput struct {
    Query   string   `json:"query"`
    Tags    []string `json:"tags"`
    Type    string   `json:"type"`
    Limit   int      `json:"limit"`
    Project string   `json:"project"`
}

type ingestSessionInput struct {
    Summary string `json:"summary"`
    Project string `json:"project"`
}
```

---

## 3. DB Schema — Migration Strategy

### New table: `memories`

Added to `VectorStore.OpenStore()` in `internal/heimdall/store.go` (post-rename from `internal/openviking/store.go`).

```sql
CREATE TABLE IF NOT EXISTS memories (
    id TEXT PRIMARY KEY,
    content TEXT NOT NULL,
    type TEXT NOT NULL DEFAULT 'fact',
    tags TEXT,              -- JSON array stored as TEXT
    project TEXT,
    vector BLOB NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    source TEXT DEFAULT 'explicit',
    content_hash TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_memories_type ON memories(type);
CREATE INDEX IF NOT EXISTS idx_memories_project ON memories(project);
CREATE INDEX IF NOT EXISTS idx_memories_source ON memories(source);
CREATE INDEX IF NOT EXISTS idx_memories_content_hash ON memories(content_hash);
```

### Migration approach (ARCH-1 fix)

The memories table is created by `OpenMemoryStore()` in the dedicated `MemoryStore`, NOT by
`OpenStore()` in `VectorStore`. This prevents schema pollution:

1. `OpenMemoryStore()` creates ONLY the `memories` table + indexes in `~/.config/heimdall-mcp/memories.db`
2. `OpenStore()` (VectorStore) continues to create ONLY the `entries` table in per-project DBs
3. No phantom tables in either database

```go
func OpenMemoryStore(dbPath string) (*MemoryStore, error) {
    // ... open DB ...
    db.Exec(`CREATE TABLE IF NOT EXISTS memories (...)`)
    // ... create indexes ...
}
```

No data migration required — memories table starts empty.

---

## 4. New Functions — File Locations

### `internal/heimdall/memory_store.go` (NEW FILE — ARCH-1 fix)

Dedicated `MemoryStore` struct, separate from `VectorStore`. This prevents schema pollution
(no phantom `entries` table in memory DB, no phantom `memories` table in project DBs).

```go
type MemoryStore struct {
    db *sql.DB
    mu sync.RWMutex
}

func OpenMemoryStore(dbPath string) (*MemoryStore, error) { ... }
func (s *MemoryStore) Close() error { ... }
```

### `internal/heimdall/vecmath.go` (NEW FILE — ARCH-1 fix)

Extracted shared helpers (no receiver dependency):

```go
func cosineSimilarity(a, b []float32) float64 { ... }
func encodeFloat32Vec(v []float32) []byte { ... }
func decodeFloat32Vec(b []byte) []float32 { ... }
```

### `internal/heimdall/memory.go` (NEW FILE)

Core memory operations on `MemoryStore` (NOT VectorStore). These are methods on the dedicated MemoryStore struct.

| Function | Signature | Purpose |
|---|---|---|
| `UpsertMemory` | `(s *MemoryStore) UpsertMemory(m Memory) error` | Insert or replace a memory by ID |
| `SearchMemories` | `(s *MemoryStore) SearchMemories(query []float32, topK int, filters MemoryFilter) []MemorySearchResult` | Cosine similarity search with post-filtering |
| `FindSimilarMemory` | `(s *MemoryStore) FindSimilarMemory(vector []float32, threshold float64) (*Memory, float64, error)` | Find the most similar existing memory above threshold (for dedup). Skips semantic dedup if memory count exceeds 10K (SEC-4 fix). |
| `GetMemoryByHash` | `(s *MemoryStore) GetMemoryByHash(hash string) (*Memory, error)` | Exact content-hash lookup (fast dedup pre-check) |
| `MemoryStats` | `(s *MemoryStore) MemoryStats() MemoryStoreStats` | Count of memories by type/source |
| `LoadAllMemoryVectors` | `(s *MemoryStore) LoadAllMemoryVectors() ([]memoryVectorEntry, error)` | Load all vectors once for batch dedup (SEC-4 fix) |
| `MemoryCount` | `(s *MemoryStore) MemoryCount() int` | Fast count for SEC-4 threshold check |

```go
// MemoryFilter holds optional filters for memory search.
type MemoryFilter struct {
    Type    MemoryType // empty = no filter
    Tags    []string   // empty = no filter; any-match semantics
    Project string     // empty = no filter
    Source  MemorySource // empty = no filter
}

// MemoryStoreStats holds memory-specific index statistics.
type MemoryStoreStats struct {
    TotalMemories  int
    ByType         map[string]int
    BySource       map[string]int
    ByProject      map[string]int
}
```

### `internal/heimdall/memory_ingest.go` (NEW FILE)

Session ingestion logic — chunking, auto-tagging, deduplication.

| Function | Signature | Purpose |
|---|---|---|
| `IngestSession` | `IngestSession(ctx context.Context, summary string, project string, embedder Embedder, store *VectorStore) (*IngestResult, error)` | Main entry point for session ingestion |
| `chunkSummary` | `chunkSummary(summary string, maxSize int) []string` | Split summary into embeddable chunks |
| `extractTags` | `extractTags(content string) []string` | Auto-extract tags from memory content |
| `classifyMemoryType` | `classifyMemoryType(content string) MemoryType` | Heuristic classification of memory type |
| `contentHash` | `contentHash(content string) string` | SHA-256 hash of normalized content |

```go
// IngestResult reports what happened during session ingestion.
type IngestResult struct {
    ChunksProcessed int      `json:"chunksProcessed"`
    MemoriesCreated int      `json:"memoriesCreated"`
    MemoriesUpdated int      `json:"memoriesUpdated"`
    Duplicates      int      `json:"duplicates"`
    Tags            []string `json:"tagsExtracted"`
}
```

### `internal/mcp/server.go` — Tool registration

Add three new tool definitions to `handleToolsList` and three new cases to `handleToolsCall`:

| Tool name | Handler method | File |
|---|---|---|
| `heimdall_remember` | `toolRemember(args)` | `internal/mcp/memory_tools.go` |
| `heimdall_recall` | `toolRecall(args)` | `internal/mcp/memory_tools.go` |
| `heimdall_ingest_session` | `toolIngestSession(args)` | `internal/mcp/memory_tools.go` |

### `internal/mcp/memory_tools.go` (NEW FILE)

Contains the three tool handler methods on `*Server`. Each:
1. Parses input JSON
2. Validates required fields
3. Connects to Ollama (for embedding)
4. Resolves the DB path (using the same registry/CWD logic as existing tools)
5. Opens the store, calls the appropriate `internal/heimdall` function
6. Returns a JSON result

---

## 5. Tool Schema Definitions (MCP JSON Schema)

### `heimdall_remember`

```json
{
  "name": "heimdall_remember",
  "description": "Store a memory for persistent recall across sessions. Memories are embedded and stored in the local SQLite database. WARNING: Do not store API keys, passwords, tokens, or other secrets — memories are stored in plaintext and returned in recall results.",
  "inputSchema": {
    "type": "object",
    "properties": {
      "content": {
        "type": "string",
        "description": "The memory text to store"
      },
      "tags": {
        "type": "array",
        "items": { "type": "string" },
        "description": "Tags for categorization and filtering (optional)"
      },
      "type": {
        "type": "string",
        "enum": ["preference", "decision", "fact", "context"],
        "description": "Memory type (default: fact)",
        "default": "fact"
      },
      "project": {
        "type": "string",
        "description": "Scope memory to a specific project (optional)"
      }
    },
    "required": ["content"]
  }
}
```

### `heimdall_recall`

```json
{
  "name": "heimdall_recall",
  "description": "Retrieve stored memories by semantic search. Returns memories ranked by relevance with similarity scores. Optionally filter by type, tags, or project.",
  "inputSchema": {
    "type": "object",
    "properties": {
      "query": {
        "type": "string",
        "description": "Natural language query to search memories"
      },
      "tags": {
        "type": "array",
        "items": { "type": "string" },
        "description": "Filter to memories with any of these tags (optional)"
      },
      "type": {
        "type": "string",
        "enum": ["preference", "decision", "fact", "context"],
        "description": "Filter to a specific memory type (optional)"
      },
      "limit": {
        "type": "integer",
        "description": "Max results to return (default: 5)",
        "default": 5
      },
      "project": {
        "type": "string",
        "description": "Filter to memories scoped to this project (optional)"
      }
    },
    "required": ["query"]
  }
}
```

### `heimdall_ingest_session`

```json
{
  "name": "heimdall_ingest_session",
  "description": "Auto-extract and store memories from a conversation summary. Chunks the summary, classifies memory types, auto-generates tags, and deduplicates against existing memories (cosine similarity > 0.92 = update instead of insert).",
  "inputSchema": {
    "type": "object",
    "properties": {
      "summary": {
        "type": "string",
        "description": "Conversation summary text to extract memories from"
      },
      "project": {
        "type": "string",
        "description": "Scope extracted memories to this project (optional)"
      }
    },
    "required": ["summary"]
  }
}
```

---

## 6. Deduplication Algorithm

### Two-tier deduplication (fast path + semantic path)

#### Tier 1: Content hash (exact match)

Before embedding, compute `SHA-256(normalize(content))` where `normalize` lowercases, trims whitespace, and collapses multiple spaces/newlines.

```go
func contentHash(content string) string {
    normalized := strings.ToLower(strings.TrimSpace(content))
    normalized = collapseWhitespace(normalized)
    h := sha256.Sum256([]byte(normalized))
    return hex.EncodeToString(h[:])
}
```

If a memory with the same `content_hash` already exists:
- **For `heimdall_remember`**: Update `updated_at`, `tags` (merge), and `type` if different. Skip re-embedding.
- **For `heimdall_ingest_session`**: Skip entirely (exact duplicate from a previous session).

#### Tier 2: Semantic similarity (near-duplicate)

After embedding the new content, scan all existing memory vectors for cosine similarity > 0.92:

```go
func (s *VectorStore) FindSimilarMemory(vector []float32, threshold float64) (*Memory, float64, error) {
    rows, err := s.db.Query(`SELECT id, content, type, tags, project, vector, 
        created_at, updated_at, source, content_hash FROM memories`)
    // ... iterate, compute cosine similarity
    // Return the MOST similar memory above threshold, or nil
}
```

If a near-duplicate is found (similarity > 0.92):
- **For `heimdall_remember`**: Update the existing memory's content, tags (merge), vector, and `updated_at`. Keep the original `created_at` and `id`.
- **For `heimdall_ingest_session`**: Update `updated_at` only. Do not overwrite content (the existing explicit memory is higher quality than an auto-extracted one).

#### Priority rules

| Existing source | New source | Action |
|---|---|---|
| `explicit` | `explicit` | Update content, merge tags, update vector |
| `explicit` | `session` | Update `updated_at` only (explicit wins) |
| `session` | `explicit` | Overwrite — explicit always takes precedence |
| `session` | `session` | Update content if newer, merge tags |

### Performance and Safety (SEC-4 fix)

Semantic dedup uses brute-force cosine similarity. To prevent memory exhaustion:

1. **Cache vectors per ingestion session:** `IngestSession` calls `store.LoadAllMemoryVectors()` once, then checks all chunks against the cached set. This reduces O(chunks * memories) DB queries to O(1) + O(chunks * memories) in-memory comparisons.

2. **Hard cap at 10K memories:** If `store.MemoryCount()` exceeds `MaxMemoriesForSemanticDedup` (10,000), skip semantic dedup entirely and rely only on content hash (Tier 1). This prevents loading 40MB+ of vector data.

3. **For individual `heimdall_remember` calls:** Still does a single `FindSimilarMemory` scan (acceptable — one call, not per-chunk).

```go
const MaxMemoriesForSemanticDedup = 10000

func (s *MemoryStore) FindSimilarMemory(vector []float32, threshold float64) (*Memory, float64, error) {
    if s.MemoryCount() > MaxMemoriesForSemanticDedup {
        return nil, 0, nil // fall back to hash-only dedup
    }
    // ... existing brute-force scan ...
}
```

---

## 7. Auto-Tag Extraction (`ingest_session`)

### Approach: Keyword + pattern extraction

No external NLP dependencies. Use a rule-based approach:

```go
func extractTags(content string) []string {
    var tags []string
    
    // 1. Technical terms: detect known patterns
    techPatterns := map[string]*regexp.Regexp{
        "database":    regexp.MustCompile(`(?i)\b(sql|sqlite|postgres|mysql|mongodb|redis|database|db)\b`),
        "testing":     regexp.MustCompile(`(?i)\b(test|tdd|unit test|integration test|e2e)\b`),
        "api":         regexp.MustCompile(`(?i)\b(api|rest|grpc|graphql|endpoint|webhook)\b`),
        "deployment":  regexp.MustCompile(`(?i)\b(deploy|kubernetes|docker|ci/cd|pipeline|flux)\b`),
        "security":    regexp.MustCompile(`(?i)\b(auth|security|token|credential|encrypt|ssl|tls)\b`),
        "performance": regexp.MustCompile(`(?i)\b(performance|latency|throughput|cache|optimization)\b`),
        "workflow":    regexp.MustCompile(`(?i)\b(workflow|process|pr|review|branch|commit|git)\b`),
        "architecture":regexp.MustCompile(`(?i)\b(architecture|design|pattern|refactor|migration)\b`),
        "frontend":    regexp.MustCompile(`(?i)\b(frontend|react|vue|angular|css|html|ui|ux)\b`),
        "backend":     regexp.MustCompile(`(?i)\b(backend|server|microservice|queue|worker)\b`),
    }
    
    for tag, pattern := range techPatterns {
        if pattern.MatchString(content) {
            tags = append(tags, tag)
        }
    }
    
    // 2. Project/tool names: capitalized words or CamelCase
    // Extract words that look like proper nouns or tool names
    namePattern := regexp.MustCompile(`\b([A-Z][a-z]+(?:[A-Z][a-z]+)+)\b`)  // CamelCase
    for _, match := range namePattern.FindAllString(content, -1) {
        tags = append(tags, strings.ToLower(match))
    }
    
    // 3. Deduplicate tags
    return uniqueStrings(tags)
}
```

### Memory type classification

```go
func classifyMemoryType(content string) MemoryType {
    lower := strings.ToLower(content)
    
    // Decision indicators
    if containsAny(lower, []string{"decided", "decision", "chose", "chosen", "we will", "agreed", "instead of"}) {
        return MemoryTypeDecision
    }
    
    // Preference indicators
    if containsAny(lower, []string{"prefers", "preference", "likes", "always use", "never use", "rather", "favorite"}) {
        return MemoryTypePreference
    }
    
    // Context indicators
    if containsAny(lower, []string{"currently", "working on", "in progress", "context", "background", "situation"}) {
        return MemoryTypeContext
    }
    
    // Default to fact
    return MemoryTypeFact
}
```

---

## 8. Chunking Strategy for `ingest_session`

Reuse the existing `chunkText` function from `internal/mcp/server.go` (which splits on paragraph boundaries), adapted for memory-sized chunks:

```go
func chunkSummary(summary string, maxSize int) []string {
    if maxSize <= 0 {
        maxSize = 500  // smaller chunks for memories (vs 1500 for code)
    }
    
    // Split on sentence boundaries within paragraphs
    sentences := splitSentences(summary)
    
    var chunks []string
    var buf strings.Builder
    
    for _, sentence := range sentences {
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
```

Key difference from code chunking: memories use **sentence-level** splitting with a smaller max size (500 chars vs 1500) to produce more focused, semantically coherent memories.

---

## 9. Memory ID Generation

Memory IDs use a deterministic format for explicit memories and UUID-based for session memories:

```go
// For heimdall_remember (explicit):
id = fmt.Sprintf("mem:explicit:%s", contentHash(content))

// For heimdall_ingest_session (session):  
id = fmt.Sprintf("mem:session:%s", contentHash(chunkContent))
```

This ensures:
- Same explicit content always maps to the same ID (natural dedup via UPSERT)
- Session chunks from the same content are idempotent across multiple ingestions
- IDs are human-readable and debuggable

---

## 10. DB Path Resolution for Memories

Memories should be **global** (not per-project), stored in the Heimdall config directory. The `project` field on a memory is a metadata filter, not a storage location.

**Memory DB location:** `~/.config/heimdall-mcp/memories.db` (separate from per-project `vectors.db`)

Rationale:
- Memories span projects (e.g., "user prefers TDD" applies everywhere)
- Project-scoped memories use the `project` filter field, not separate databases
- This avoids the problem of "which project DB do I search for memories?"
- Consistent with existing XDG convention (`~/.config/heimdall-mcp/` for all global state)

This uses a dedicated `MemoryStore` struct (ARCH-1 fix — NOT `VectorStore`):

```go
// In Server struct (internal/mcp/server.go):
type Server struct {
    Cfg         config.Config
    Registry    *registry.Registry
    Index       IndexState
    MemoryStore *heimdall.MemoryStore  // global memory database (dedicated struct)
}
```

Opened once at server startup in `main.go`, closed on shutdown.

**Path resolution (CORR-1 fix):** The memory DB path is resolved by `config.resolveMemoryDBPath()`,
which independently resolves `$XDG_CONFIG_HOME/heimdall-mcp/memories.db` without depending on
config file existence. This prevents the bug where `resolveConfigPath()` returns `""` on first
run, causing `filepath.Dir("")` = `"."` and the memory DB landing in CWD.

```go
// In main.go:
memDBPath := config.ResolveMemoryDBPath() // e.g., ~/.config/heimdall-mcp/memories.db
memStore, err := heimdall.OpenMemoryStore(memDBPath)
```

**Alternative considered and rejected:** Storing memories in each project's `vectors.db`. This fragments memories and makes cross-project recall impossible without searching multiple databases.

---

## 11. Test Strategy

### Unit Tests

File: `internal/heimdall/memory_test.go`

| Test | What it verifies |
|---|---|
| `TestUpsertMemory` | Basic insert and retrieve |
| `TestUpsertMemory_Update` | Insert then update same ID |
| `TestSearchMemories_Basic` | Cosine similarity ranking |
| `TestSearchMemories_FilterByType` | Type filter works |
| `TestSearchMemories_FilterByTags` | Tag any-match filter works |
| `TestSearchMemories_FilterByProject` | Project filter works |
| `TestSearchMemories_CombinedFilters` | Multiple filters AND together |
| `TestFindSimilarMemory_AboveThreshold` | Returns match above 0.92 |
| `TestFindSimilarMemory_BelowThreshold` | Returns nil below 0.92 |
| `TestGetMemoryByHash` | Exact hash lookup |
| `TestMemoryStats` | Correct counts by type/source |

File: `internal/heimdall/memory_ingest_test.go`

| Test | What it verifies |
|---|---|
| `TestChunkSummary_Short` | Short text = single chunk |
| `TestChunkSummary_Long` | Long text splits on sentences |
| `TestChunkSummary_Empty` | Empty string = no chunks |
| `TestExtractTags_Technical` | Detects database, testing, api etc. |
| `TestExtractTags_Empty` | No tags from empty/generic text |
| `TestClassifyMemoryType_Decision` | "We decided..." -> decision |
| `TestClassifyMemoryType_Preference` | "User prefers..." -> preference |
| `TestClassifyMemoryType_Default` | Generic text -> fact |
| `TestContentHash_Normalization` | Whitespace/case normalization |
| `TestIngestSession_Dedup` | Same summary twice = no new memories |
| `TestIngestSession_NearDuplicate` | Similar content updates existing |
| `TestIngestSession_ExplicitWins` | Session doesn't overwrite explicit |

File: `internal/mcp/memory_tools_test.go`

| Test | What it verifies |
|---|---|
| `TestToolRemember_Basic` | Happy path: store and return |
| `TestToolRemember_MissingContent` | Error on empty content |
| `TestToolRemember_InvalidType` | Error on bad type value |
| `TestToolRemember_Dedup` | Same content twice = update |
| `TestToolRecall_Basic` | Happy path: search and return |
| `TestToolRecall_MissingQuery` | Error on empty query |
| `TestToolRecall_WithFilters` | Filters reduce results |
| `TestToolRecall_EmptyResults` | No matches = friendly message |
| `TestToolIngestSession_Basic` | Happy path: chunk, store, return stats |
| `TestToolIngestSession_MissingSummary` | Error on empty summary |
| `TestToolIngestSession_Dedup` | Dedup works end-to-end |

### Integration Tests

All tests use the `MockEmbedder` (already exists in `internal/openviking/embedder.go`) with pre-computed vectors. This keeps tests fast and deterministic without requiring Ollama.

For the store tests, use `t.TempDir()` for ephemeral SQLite databases.

### Test Helpers

```go
// testMemoryStore creates a VectorStore in a temp dir with the memories table.
func testMemoryStore(t *testing.T) *VectorStore {
    t.Helper()
    dir := t.TempDir()
    store, err := OpenStore(dir)
    if err != nil {
        t.Fatal(err)
    }
    t.Cleanup(func() { store.Close() })
    return store
}
```

---

## 12. Task Breakdown

### Task 1: DB Schema (store.go changes)
- Add `CREATE TABLE IF NOT EXISTS memories (...)` to `OpenStore`
- Add indexes
- **Estimate:** Small change to existing function
- **File:** `internal/heimdall/store.go` (post-rename)

### Task 2: Memory types and store operations (memory.go)
- Define `Memory`, `MemoryType`, `MemorySource`, `MemoryFilter`, `MemorySearchResult` types
- Implement `UpsertMemory`, `SearchMemories`, `FindSimilarMemory`, `GetMemoryByHash`, `MemoryStats`
- **File:** `internal/heimdall/memory.go` (NEW)

### Task 3: Memory ingestion logic (memory_ingest.go)
- Implement `IngestSession`, `chunkSummary`, `extractTags`, `classifyMemoryType`, `contentHash`
- Deduplication logic (hash check + semantic similarity)
- **File:** `internal/heimdall/memory_ingest.go` (NEW)

### Task 4: MCP tool input types
- Add `rememberInput`, `recallInput`, `ingestSessionInput` structs
- **File:** `internal/mcp/types.go`

### Task 5: MCP tool handlers (memory_tools.go)
- Implement `toolRemember`, `toolRecall`, `toolIngestSession` methods on `*Server`
- **File:** `internal/mcp/memory_tools.go` (NEW)

### Task 6: Tool registration and routing
- Add tool schemas to `handleToolsList`
- Add cases to `handleToolsCall` switch
- **File:** `internal/mcp/server.go`

### Task 7: Global memory store initialization
- Open `~/.heimdall/memories.db` in `main.go`
- Pass to `mcp.Server` as `MemoryStore` field
- Close on shutdown
- **File:** `cmd/heimdall-mcp/main.go` (post-rename), `internal/mcp/server.go`

### Task 8: Unit tests — store operations
- Tests for all `VectorStore` memory methods
- **File:** `internal/heimdall/memory_test.go` (NEW)

### Task 9: Unit tests — ingestion logic
- Tests for chunking, tagging, classification, dedup
- **File:** `internal/heimdall/memory_ingest_test.go` (NEW)

### Task 10: Unit tests — MCP tool handlers
- Tests for all three tool handlers
- **File:** `internal/mcp/memory_tools_test.go` (NEW)

### Task 11: CLI memory commands (optional, lower priority)
- `heimdall-mcp remember <text>` — CLI equivalent
- `heimdall-mcp recall <query>` — CLI search
- `heimdall-mcp memories` — list all memories
- **File:** `internal/cli/cli.go`

### Dependency order

```
Task 1 (schema) 
    -> Task 2 (store ops) 
        -> Task 3 (ingestion) -> Task 9 (ingestion tests)
        -> Task 8 (store tests)
    -> Task 4 (input types) 
        -> Task 5 (tool handlers) 
            -> Task 6 (registration)
                -> Task 7 (startup)
                    -> Task 10 (tool tests)
Task 11 (CLI) depends on Task 2 + Task 3
```

Parallelizable: Tasks 2+4 can run in parallel. Tasks 8+9+10 can run in parallel once their dependencies complete.

---

## 13. Cross-Cutting Concerns

### Shared with Phase 2 — Typed External Context
- The `memories` table is separate from the `entries` table. No schema conflicts.
- The `SearchMemories` function is independent from code search. No interference.
- The `chunkSummary` function in `memory_ingest.go` is separate from `chunkText` in `server.go`.

### Shared with Phase 1 — Rename
- All file paths in this plan use post-rename names (`internal/heimdall/`, `~/.config/heimdall-mcp/`).
- Tool names use `heimdall_` prefix.
- Config directory uses `~/.config/heimdall-mcp/` (XDG convention, consistent with Phase 1).
- Phase 1 must complete before implementation starts.

### Shared with Phase 2 — Retrieval Debugging
- `heimdall_explain` will need to include memory source counts in `sourceCounts`.
- The `heimdall_search` enriched results will include `source: "memory"` for memory results.
- These are additive changes on top of the memory system, not blockers.

---

## 14. Open Questions / Decisions

1. **Memory DB location confirmed:** `~/.config/heimdall-mcp/memories.db` (global, not per-project, XDG-consistent). Project scoping is via metadata filter.

2. **Tag merging strategy:** When dedup updates an existing memory, merge tags with union (no duplicates). Do not remove existing tags. **Max 20 tags per memory, each max 100 chars (SEC-2 fix).**

3. **Memory limit:** Hard cap of 10K memories for semantic dedup (SEC-4 fix). Above 10K, falls back to hash-only dedup. No hard cap on total stored memories.

4. **Embedding model consistency:** Memories and code use the same BGE-M3 model. If the model changes, all memories would need re-embedding (same as code vectors). This is a known limitation shared with the existing code index.

5. **Concurrency:** The `VectorStore` already uses `sync.RWMutex`. Memory operations follow the same pattern — RLock for reads, Lock for writes. No additional concurrency primitives needed.
