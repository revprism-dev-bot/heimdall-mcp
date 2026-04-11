# Consolidated Implementation Plan: Heimdall Evolution

**Date:** 2026-04-11
**Status:** Plan
**Scope:** Rename openviking -> heimdall + Session Memory + Retrieval Debugging + Typed External Context
**Design Spec:** `docs/superpowers/specs/2026-04-11-heimdall-evolution-design.md`
**Input Plans:** phase1-rename.md, phase2-memory.md, phase3-context.md

---

## Table of Contents

1. [Conflict Resolution Decisions](#1-conflict-resolution-decisions)
2. [Unified Schema Definitions](#2-unified-schema-definitions)
3. [Config Directory Reconciliation](#3-config-directory-reconciliation)
4. [Execution Order and Parallelism](#4-execution-order-and-parallelism)
5. [Complete Task List with Dependencies](#5-complete-task-list-with-dependencies)
6. [Cross-Phase Integration Points](#6-cross-phase-integration-points)
7. [Tool Inventory Verification](#7-tool-inventory-verification)
8. [Integration Test Plan](#8-integration-test-plan)
9. [Risk Assessment](#9-risk-assessment)

---

## 1. Conflict Resolution Decisions

### 1.1 `store.go` — New Table (Phase 2) vs New Columns (Phase 3)

**Conflict:** Phase 2 adds `CREATE TABLE IF NOT EXISTS memories (...)` to `OpenStore()`. Phase 3 adds `ALTER TABLE entries ADD COLUMN source_type/metadata/relationships` to `OpenStore()`.

**Resolution: No conflict.** These are independent, additive changes:
- Phase 2 creates a **new table** (`memories`) — does not touch `entries`
- Phase 3 adds **columns to existing table** (`entries`) — does not touch `memories`
- Both follow the existing migration pattern (idempotent `CREATE TABLE IF NOT EXISTS` / error-ignoring `ALTER TABLE ADD COLUMN`)
- Both changes go in `OpenStore()` after the existing `content_hash` migration (line 82)
- **Order within `OpenStore()`:** Phase 3 column additions FIRST (they modify the existing `entries` table), then Phase 2 `memories` table creation. This keeps `entries`-related migrations together.

### 1.2 `types.go` — New Input Structs (Phase 2 + Phase 3)

**Conflict:** Both phases add new structs to `internal/mcp/types.go`.

**Resolution: No field name collisions.** Verified struct names:
- Phase 2 adds: `rememberInput`, `recallInput`, `ingestSessionInput`
- Phase 3 adds: `SearchResultEnriched`, `explainInput`, `ExplainResult`, `ExplainResultItem`, `ScoreDistribution`, `SourceCounts`, `IndexStatsInfo`, `RelatedItem`
- Phase 3 modifies: `searchInput` (adds `SourceType`, `MetadataFilter`), `indexTextInput` (adds `Type`, `Metadata`, `Relationships`)
- No name collisions between the two sets. Both can be added independently.

### 1.3 `server.go` — Tool Registration (Phase 2 + Phase 3)

**Conflict:** Both phases add tool definitions to `handleToolsList` and cases to `handleToolsCall`.

**Resolution: Append in any order.** Tool registration is order-independent (it's a slice of `MCPToolInfo` and a `switch` statement). Convention: add Phase 3 tools after existing tools (they extend existing functionality), then Phase 2 tools (they are entirely new tools).

**Merge order for `handleToolsList`:**
1. Existing 5 tools (renamed by Phase 1)
2. Phase 3: `heimdall_explain` (extends search functionality)
3. Phase 2: `heimdall_remember`, `heimdall_recall`, `heimdall_ingest_session`

**Merge order for `handleToolsCall`:**
Same order in the switch statement — cosmetic preference, no functional impact.

### 1.4 `tools.go` — New Tool Handlers (Phase 2 + Phase 3)

**Conflict:** Both phases add new tool handler methods.

**Resolution: No overlapping function names.**
- Phase 2 adds: `toolRemember`, `toolRecall`, `toolIngestSession` (in new file `memory_tools.go`)
- Phase 3 adds: `toolExplain`, `toolSearchFiltered`, `classifySource`, `validateSourceType`, `resolveRelationships` (in `tools.go`)
- Phase 3 also modifies `toolSearch` (enriched results + filtered path)
- Phase 2's tools are in a separate file (`memory_tools.go`), so no merge conflicts with `tools.go`

### 1.5 `VectorRecord` struct — Phase 3 Additions

**Question:** Does Phase 2 need the `SourceType`, `Metadata`, `Relationships` fields from Phase 3?

**Answer: No.** Phase 2's `memories` table is separate from `entries`. Memory records use their own `Memory` struct, not `VectorRecord`. The Phase 3 fields (`SourceType`, `Metadata`, `Relationships`) are only relevant to the `entries` table. The `VectorRecord` changes do not affect Phase 2.

### 1.6 `resolveDBDir` Helper — Shared by Phase 2 and Phase 3

**Question:** Phase 3 extracts `resolveDBDir` from duplicated logic. Should Phase 2 memory tools use it?

**Answer: No, with caveats.** Memory tools connect to the **global** memory store at `~/.config/heimdall-mcp/memories.db`, not to per-project stores. The `resolveDBDir` helper resolves per-project DB paths. Memory tools should only use `resolveDBDir` when they need to access the project index (e.g., for cross-referencing in `heimdall_explain`). The memory DB path resolution is a separate concern.

However, `heimdall_remember` and `heimdall_ingest_session` do not need project DB access. `heimdall_recall` searches memories only. The global memory store path is resolved differently (see Section 3).

**Note on `resolveDBDir` (ARCH-3 fix):** The `resolveDBDir` helper should return the fallback
path (`cwd/.heimdall_db`) unconditionally (without `os.Stat` check). The *caller* decides
whether to check existence. Read-only tools (search, explain) check `os.Stat` on the returned
path and return an error if missing. Write tools (index, index_text) use the path directly and
let `OpenStore` create the directory via `os.MkdirAll`. This preserves the existing behavior
where `toolIndexText` creates the DB directory on first use.

---

## 2. Unified Schema Definitions

### 2.1 `entries` Table (Phase 3 additions)

```sql
-- Existing schema (after Phase 1 rename):
CREATE TABLE IF NOT EXISTS entries (
    id TEXT PRIMARY KEY,
    file_path TEXT NOT NULL,
    start_line INTEGER,
    end_line INTEGER,
    content TEXT NOT NULL,
    kind TEXT,
    identifier TEXT,
    vector BLOB NOT NULL,
    mod_time INTEGER NOT NULL,
    content_hash TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_file_path ON entries(file_path);

-- Phase 3 migrations (idempotent):
ALTER TABLE entries ADD COLUMN source_type TEXT DEFAULT 'code';
ALTER TABLE entries ADD COLUMN metadata TEXT DEFAULT '{}';
ALTER TABLE entries ADD COLUMN relationships TEXT DEFAULT '[]';
CREATE INDEX IF NOT EXISTS idx_source_type ON entries(source_type);
```

### 2.2 `memories` Table (Phase 2 addition)

```sql
CREATE TABLE IF NOT EXISTS memories (
    id TEXT PRIMARY KEY,
    content TEXT NOT NULL,
    type TEXT NOT NULL DEFAULT 'fact',
    tags TEXT,
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

### 2.3 `VectorRecord` Struct (Unified)

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
    SourceType    string    `json:"sourceType"`    // Phase 3: "code", "ticket", "doc", "pr", etc.
    Metadata      string    `json:"metadata"`      // Phase 3: JSON object string
    Relationships string    `json:"relationships"` // Phase 3: JSON array string
}
```

### 2.4 `Memory` Struct (Phase 2 only — separate from VectorRecord)

```go
type Memory struct {
    ID          string       `json:"id"`
    Content     string       `json:"content"`
    Type        MemoryType   `json:"type"`
    Tags        []string     `json:"tags,omitempty"`
    Project     string       `json:"project,omitempty"`
    Vector      []float32    `json:"-"`
    CreatedAt   int64        `json:"createdAt"`
    UpdatedAt   int64        `json:"updatedAt"`
    Source      MemorySource `json:"source"`
    ContentHash string       `json:"-"`
}
```

### 2.5 `ContextBlock` Struct (Phase 3 addition)

```go
type ContextBlock struct {
    FilePath   string
    StartLine  int
    EndLine    int
    Content    string
    Kind       string
    Identifier string
    Score      float64
    ChunkID    string  // Phase 3: VectorRecord.ID for this chunk
}
```

### 2.6 Server Struct (Unified)

```go
type Server struct {
    Cfg         config.Config
    Registry    *registry.Registry
    Index       IndexState
    MemoryStore *heimdall.MemoryStore  // Phase 2: global memory database (dedicated struct, NOT VectorStore)
}
```

**Architecture Decision (ARCH-1 fix):** Memory operations use a dedicated `MemoryStore` struct
in `internal/heimdall/memory_store.go`, NOT the `VectorStore`. Reasons:

1. **Different lifecycles:** VectorStore is per-project, opened per-tool-call. MemoryStore is a global singleton opened at startup.
2. **No schema pollution:** VectorStore creates `entries` table; MemoryStore creates `memories` table. Each only creates what it needs.
3. **Independent testability:** Each store can be tested in isolation without phantom tables.

Shared helpers (`cosineSimilarity`, `encodeFloat32Vec`, `decodeFloat32Vec`) become package-level
functions in `internal/heimdall/vecmath.go` (they already have no receiver dependency).

```go
// internal/heimdall/memory_store.go
type MemoryStore struct {
    db *sql.DB
    mu sync.RWMutex
}

func OpenMemoryStore(dbPath string) (*MemoryStore, error) {
    dir := filepath.Dir(dbPath)
    if err := os.MkdirAll(dir, 0700); err != nil {
        return nil, err
    }
    db, err := sql.Open("sqlite3", dbPath+"?_journal_mode=WAL")
    if err != nil {
        return nil, err
    }
    // Only create the memories table — NO entries table
    _, err = db.Exec(`CREATE TABLE IF NOT EXISTS memories (
        id TEXT PRIMARY KEY,
        content TEXT NOT NULL,
        type TEXT NOT NULL DEFAULT 'fact',
        tags TEXT,
        project TEXT,
        vector BLOB NOT NULL,
        created_at INTEGER NOT NULL,
        updated_at INTEGER NOT NULL,
        source TEXT DEFAULT 'explicit',
        content_hash TEXT NOT NULL DEFAULT ''
    )`)
    if err != nil {
        db.Close()
        return nil, err
    }
    db.Exec(`CREATE INDEX IF NOT EXISTS idx_memories_type ON memories(type)`)
    db.Exec(`CREATE INDEX IF NOT EXISTS idx_memories_project ON memories(project)`)
    db.Exec(`CREATE INDEX IF NOT EXISTS idx_memories_source ON memories(source)`)
    db.Exec(`CREATE INDEX IF NOT EXISTS idx_memories_content_hash ON memories(content_hash)`)
    return &MemoryStore{db: db}, nil
}

func (s *MemoryStore) Close() error { return s.db.Close() }
```

### 2.7 All New Input/Output Types

**Phase 2 types (in `types.go`):**
- `rememberInput{Content, Tags, Type, Project}`
- `recallInput{Query, Tags, Type, Limit, Project}`
- `ingestSessionInput{Summary, Project}`

**Phase 3 types (in `types.go`):**
- `SearchResultEnriched{File, StartLine, EndLine, Content, Score, Source, ChunkID, EmbeddingModel}`
- `explainInput{Query, Limit, Project}`
- `ExplainResult{Query, QueryEmbeddingDim, TotalChunksSearched, SearchTimeMs, Results, ScoreDistribution, SourceCounts, IndexStats}`
- `ExplainResultItem{Rank, ChunkID, File, Score, Source, ChunkSizeBytes, ChunkLines, Related}`
- `ScoreDistribution{Above90, Above70, Above50, Below50}`
- `SourceCounts{Code, External, Memory}`
- `IndexStatsInfo{TotalChunks, TotalFiles, LastIndexed}`
- `RelatedItem{Source, Relationship, Snippet}`

**Phase 3 modified types:**
- `searchInput` gains: `SourceType string`, `MetadataFilter json.RawMessage`
- `indexTextInput` gains: `Type string`, `Metadata json.RawMessage`, `Relationships json.RawMessage`

---

## 3. Config Directory Reconciliation

### The Problem

Phase 1 (rename) establishes `~/.config/heimdall-mcp/` as the config directory (following the existing XDG convention from `~/.config/openviking-mcp/`). Phase 2 (memory) proposes `~/.heimdall/memories.db` as the global memory store location.

These are inconsistent: config in `~/.config/heimdall-mcp/`, but memories in `~/.heimdall/`.

### The Decision: Use `~/.config/heimdall-mcp/` for Everything

**All global Heimdall data goes in `~/.config/heimdall-mcp/`:**

| Asset | Path |
|---|---|
| Config file | `~/.config/heimdall-mcp/config.json` |
| Project registry | `~/.config/heimdall-mcp/projects.json` |
| Global memories DB | `~/.config/heimdall-mcp/memories.db` |

**Rationale:**
1. The XDG convention (`~/.config/<app>/`) is already established in the codebase and should not be broken.
2. Having two separate config directories (`~/.config/heimdall-mcp/` and `~/.heimdall/`) creates user confusion and complicates uninstall/cleanup.
3. The design spec's `~/.heimdall/` was a simplification that doesn't match the actual code's XDG pattern. Phase 1's plan already identified this discrepancy (Section 7: "Spec Discrepancies").
4. The memory DB is small (SQLite file, hundreds to low thousands of records) and belongs with other global app state.

**Impact on Phase 2:**
- Change memory DB path from `~/.heimdall/memories.db` to `~/.config/heimdall-mcp/memories.db`
- The config directory is already created by `config.LoadConfig()` (it calls `os.MkdirAll`), so the directory will exist
- The `MemoryStore` initialization in `main.go` resolves the path using the same `resolveConfigPath()` helper from `config.go`

**Impact on Phase 1:**
- No changes. Phase 1 already uses `~/.config/heimdall-mcp/`.

**No migration needed for `~/.heimdall/`** since it was never created by any released version.

### Memory DB Path Resolution

Add a standalone helper to `config.go` that independently resolves the XDG config directory
without depending on config file existence. This is critical because `resolveConfigPath()`
returns `""` when no config file exists on disk (first run), and `filepath.Dir("")` returns
`"."` which would incorrectly place the memory DB in the CWD.

```go
// resolveMemoryDBPath returns the path to the global memories database.
// It independently resolves the XDG config directory without depending
// on config file existence, so it works correctly on first run.
func resolveMemoryDBPath() string {
    dir := resolveConfigDir()
    if err := os.MkdirAll(dir, 0700); err != nil {
        log.Printf("heimdall: failed to create config dir %s: %v", dir, err)
    }
    return filepath.Join(dir, "memories.db")
}

// resolveConfigDir returns the Heimdall config directory path.
// Uses $XDG_CONFIG_HOME/heimdall-mcp/ or falls back to ~/.config/heimdall-mcp/.
// Does NOT depend on config file existence.
func resolveConfigDir() string {
    xdgConfig := os.Getenv("XDG_CONFIG_HOME")
    if xdgConfig == "" {
        home, err := os.UserHomeDir()
        if err != nil {
            // Fallback for containers with no HOME — use /tmp
            log.Printf("heimdall: cannot determine home directory: %v, using /tmp", err)
            return filepath.Join(os.TempDir(), "heimdall-mcp")
        }
        xdgConfig = filepath.Join(home, ".config")
    }
    return filepath.Join(xdgConfig, "heimdall-mcp")
}
```

**Note:** `resolveConfigDir()` is also useful for other callers (config migration, etc.) and
should be reused internally by `resolveConfigPath()` to avoid duplicating XDG resolution logic.

---

## 4. Execution Order and Parallelism

### Phase Dependencies

```
Phase 1 (Rename)  ──MUST COMPLETE FIRST──┬──> Phase 2 (Memory)
                                          └──> Phase 3 (Context)
                                                    │
                                 Phase 2 + Phase 3 run in PARALLEL
                                                    │
                                          Cross-phase integration
                                          (heimdall_explain memory counts,
                                           enriched search memory awareness)
```

### Phase 1: Rename (Sequential — ~8 tasks)

Phase 1 is strictly sequential. Each step depends on the previous.

| Step | Task | Parallel? | Depends On |
|---|---|---|---|
| 1.1 | `git mv internal/openviking internal/heimdall` + package declarations | No | - |
| 1.2 | Update `go.mod` module path | No | 1.1 |
| 1.3 | Update all import paths + package qualifiers (5 files) | No | 1.2 |
| 1.4 | `git mv cmd/openviking-mcp cmd/heimdall-mcp` | No | 1.3 |
| 1.5 | Update string literals (tool names, DB paths, config paths, env vars) | **Parallel across files** | 1.4 |
| 1.6 | Add migration logic (`migrate.go`, config migration) | No | 1.5 |
| 1.7 | Update non-Go files (Makefile, .gitignore, README) | **Parallel across files** | 1.4 |
| 1.8 | Verify: `go build`, `go vet`, `grep` audit, smoke test | No | 1.5, 1.6, 1.7 |

### Phase 2: Session Memory (After Phase 1 — ~11 tasks)

| Step | Task | Parallel? | Depends On |
|---|---|---|---|
| 2.1 | DB schema: `memories` table in `OpenStore()` | No | Phase 1 |
| 2.2 | Memory types + store operations (`memory.go`) | No | 2.1 |
| 2.3 | Memory ingestion logic (`memory_ingest.go`) | No | 2.2 |
| 2.4 | MCP tool input types in `types.go` | **Parallel with 2.2** | Phase 1 |
| 2.5 | MCP tool handlers (`memory_tools.go`) | No | 2.2, 2.3, 2.4 |
| 2.6 | Tool registration in `server.go` | No | 2.5 |
| 2.7 | Global memory store init in `main.go` | No | 2.6 |
| 2.8 | Unit tests: store operations (`memory_test.go`) | **Parallel with 2.3** | 2.2 |
| 2.9 | Unit tests: ingestion (`memory_ingest_test.go`) | **Parallel with 2.8** | 2.3 |
| 2.10 | Unit tests: tool handlers (`memory_tools_test.go`) | No | 2.5, 2.6, 2.7 |
| 2.11 | CLI memory commands (optional, lower priority) | No | 2.2, 2.3 |

### Phase 3: Retrieval Debugging + Typed Context (After Phase 1 — ~10 tasks)

| Step | Task | Parallel? | Depends On |
|---|---|---|---|
| 3.1 | DB schema: new columns on `entries` in `OpenStore()` | No | Phase 1 |
| 3.2 | Indexer default values (`SourceType: "code"`, etc.) | No | 3.1 |
| 3.3 | Enhanced `heimdall_index_text` (type/metadata/relationships) | No | 3.1 |
| 3.4 | Enriched search results (`ChunkID` in ContextBlock, `SearchResultEnriched`) | No | 3.1 |
| 3.5 | Extract `resolveDBDir` helper | **Parallel with 3.2** | Phase 1 |
| 3.6 | Filtered search (`SearchFiltered`, `matchesMetadata`) | No | 3.1, 3.5 |
| 3.7 | `heimdall_explain` tool (full implementation) | No | 3.1, 3.4, 3.5, 3.6 |
| 3.8 | Unit tests: store (`store_test.go`) | **Parallel with 3.7** | 3.1, 3.6 |
| 3.9 | Integration tests | No | 3.7 |
| 3.10 | Documentation update (README) | No | 3.7 |

### Cross-Phase Integration (After Phase 2 + Phase 3)

| Step | Task | Depends On |
|---|---|---|
| X.1 | `heimdall_explain` includes memory counts from `memories` table | Phase 2 (2.1), Phase 3 (3.7) |
| X.2 | `heimdall_search` enriched results recognize `source: "memory"` | Phase 2 (2.2), Phase 3 (3.4) |
| X.3 | Cross-feature integration tests | X.1, X.2 |

### Parallelism Map (Visual)

```
TIME ──────────────────────────────────────────────────────────────────>

Phase 1 (Sequential):
[1.1]─[1.2]─[1.3]─[1.4]─[1.5 + 1.7]─[1.6]─[1.8]
                                                │
                              ┌─────────────────┴─────────────────┐
                              │                                   │
Phase 2 (Memory):             │   Phase 3 (Context):              │
[2.1]─[2.2 + 2.4]─[2.3]      │   [3.1 + 3.5]─[3.2 + 3.3 + 3.4] │
      │   │         │        │          │              │          │
      │  [2.8]     [2.9]     │         [3.6]          [3.4]       │
      │   │         │        │          │              │          │
      └─[2.5]─[2.6]─[2.7]   │         [3.7]─[3.8]               │
              │              │          │                          │
            [2.10]           │        [3.9]                       │
              │              │          │                          │
              └──────────────┼──────────┘                          │
                             │                                     │
                     Cross-Phase Integration:                      │
                     [X.1]─[X.2]─[X.3]                            │
```

---

## 5. Complete Task List with Dependencies

### Phase 1: Rename (8 tasks — sequential, ~2 hours)

| ID | Task | Files | Dependencies | Parallelizable |
|---|---|---|---|---|
| P1-T1 | Directory + module rename (`git mv`, package decls, go.mod, imports, qualifiers) | go.mod, 6 files in internal/heimdall/, 5 importing files, cmd dir | None | No |
| P1-T2 | Tool names + server info | internal/mcp/server.go | P1-T1 | No |
| P1-T3 | String literals: DB paths (`.viking_db` -> `.heimdall_db`) | config.go, indexer.go, tools.go, cli.go, registry.go | P1-T1 | Yes (across files) |
| P1-T4 | String literals: config paths + env vars | config.go, registry.go | P1-T1 | Yes (with P1-T3) |
| P1-T5 | Comments + help text | cli.go, main.go, server.go, tools.go | P1-T1 | Yes (with P1-T3) |
| P1-T6 | Migration logic (`migrate.go`, config migration) | NEW: internal/heimdall/migrate.go, config.go, main.go, cli.go | P1-T2..T5 | No |
| P1-T7 | Non-Go files | Makefile, .gitignore, README.md | P1-T1 | Yes (with P1-T3) |
| P1-T8 | Verification (`go build`, `go vet`, grep audit, smoke test) | - | P1-T1..T7 | No |

### Phase 2: Session Memory (11 tasks — partially parallel, ~4 hours)

| ID | Task | Files | Dependencies | Parallelizable |
|---|---|---|---|---|
| P2-T1 | DB schema: `memories` table + indexes in `OpenStore()` | internal/heimdall/store.go | Phase 1 done | No |
| P2-T2 | Memory types + store operations | NEW: internal/heimdall/memory.go | P2-T1 | No |
| P2-T3 | Memory ingestion logic | NEW: internal/heimdall/memory_ingest.go | P2-T2 | No |
| P2-T4 | MCP tool input types | internal/mcp/types.go | Phase 1 done | Yes (with P2-T2) |
| P2-T5 | MCP tool handlers | NEW: internal/mcp/memory_tools.go | P2-T2, P2-T3, P2-T4 | No |
| P2-T6 | Tool registration + routing | internal/mcp/server.go | P2-T5 | No |
| P2-T7 | Global memory store init + `MemoryDBPath()` helper | cmd/heimdall-mcp/main.go, internal/config/config.go, internal/mcp/server.go | P2-T6 | No |
| P2-T8 | Unit tests: store operations | NEW: internal/heimdall/memory_test.go | P2-T2 | Yes (with P2-T3) |
| P2-T9 | Unit tests: ingestion logic | NEW: internal/heimdall/memory_ingest_test.go | P2-T3 | Yes (with P2-T8) |
| P2-T10 | Unit tests: tool handlers | NEW: internal/mcp/memory_tools_test.go | P2-T5, P2-T6, P2-T7 | No |
| P2-T11 | CLI memory commands (optional) | internal/cli/cli.go | P2-T2, P2-T3 | Yes (with P2-T5) |

### Phase 3: Retrieval Debugging + Typed Context (10 tasks — partially parallel, ~6 hours)

| ID | Task | Files | Dependencies | Parallelizable |
|---|---|---|---|---|
| P3-T1 | DB schema: `source_type`, `metadata`, `relationships` columns + `VectorRecord` update + `Upsert`/`Search` update | internal/heimdall/store.go | Phase 1 done | No |
| P3-T2 | Indexer default values for new fields | internal/heimdall/indexer.go | P3-T1 | Yes (with P3-T5) |
| P3-T3 | Enhanced `heimdall_index_text` (type/metadata/relationships input, validation, record creation, schema update) | internal/mcp/types.go, internal/mcp/server.go (toolIndexText in server.go) | P3-T1 | Yes (with P3-T2) |
| P3-T4 | Enriched search results (`ChunkID` in ContextBlock, `SearchResultEnriched`, update `toolSearch` output) | internal/heimdall/retriever.go, internal/mcp/types.go, internal/mcp/tools.go | P3-T1 | Yes (with P3-T2) |
| P3-T5 | Extract `resolveDBDir` helper | internal/mcp/tools.go | Phase 1 done | Yes (with P3-T1) |
| P3-T6 | Filtered search (`SearchFiltered`, `matchesMetadata`, `toolSearchFiltered`, `searchInput` update) | internal/heimdall/store.go, internal/mcp/types.go, internal/mcp/tools.go | P3-T1, P3-T5 | No |
| P3-T7 | `heimdall_explain` tool (types, implementation, `classifySource`, `resolveRelationships`, `SnippetBySource`, registration) | internal/mcp/types.go, internal/mcp/tools.go, internal/heimdall/store.go, internal/mcp/server.go | P3-T1, P3-T4, P3-T5, P3-T6 | No |
| P3-T8 | Unit tests: store + helpers | NEW: internal/heimdall/store_test.go, NEW: internal/mcp/tools_test.go | P3-T1, P3-T6 | Yes (with P3-T7) |
| P3-T9 | Integration tests | NEW: internal/mcp/integration_test.go | P3-T7 | No |
| P3-T10 | Documentation update (README) | README.md | P3-T7 | No |

### Cross-Phase Integration (3 tasks — after Phase 2 + Phase 3)

| ID | Task | Files | Dependencies | Parallelizable |
|---|---|---|---|---|
| X-T1 | `heimdall_explain` includes memory count from `memories` table | internal/mcp/tools.go (toolExplain) | P2-T1, P3-T7 | Yes (with X-T2) |
| X-T2 | `heimdall_search` enriched results recognize `source: "memory"` (future-proofing — `classifySource` already handles `kind == "memory"` from Phase 3) | Verify in internal/mcp/tools.go | P2-T2, P3-T4 | Yes (with X-T1) |
| X-T3 | Cross-feature integration tests (search across code + external + memory, explain with memory counts, ingest + recall cycle) | NEW or extend: internal/mcp/integration_test.go | X-T1, X-T2 | No |

---

## 6. Cross-Phase Integration Points

### 6.1 `heimdall_explain` Needs Memory Counts

**What:** The `sourceCounts` field in `ExplainResult` includes `Memory int`. Phase 3 defines the field, Phase 2 provides the data.

**How:** In `toolExplain` (Phase 3), after computing code/external counts from the `entries` table, query the `memories` table for total count:

```go
// Count memories (table may not exist if Phase 2 hasn't run yet)
var memoryCount int
_ = store.db.QueryRow(`SELECT COUNT(*) FROM memories`).Scan(&memoryCount)
counts.Memory = memoryCount
```

The error-ignore pattern is safe because `memories` uses `CREATE TABLE IF NOT EXISTS` and the query will fail harmlessly if the table doesn't exist yet.

**When:** This is a 2-line addition to `toolExplain`. It can be added during Phase 3 implementation (the query gracefully fails if Phase 2 hasn't run), or as a cross-phase integration task after both phases complete.

### 6.2 `heimdall_search` Memory-Aware Source Classification

**What:** Phase 3's `classifySource` helper already handles `kind == "memory"` -> `source = "memory"`. Phase 2's memory search (`heimdall_recall`) is a separate tool that doesn't go through `toolSearch`. However, if in the future memories are blended into regular search results, the classification is ready.

**How:** Already handled by Phase 3's `classifySource`:

```go
func classifySource(kind string) string {
    switch kind {
    case "external": return "external"
    case "memory":   return "memory"
    default:         return "code"
    }
}
```

**When:** No additional work needed. This is future-proofing that Phase 3 already includes.

### 6.3 Shared Embedding Model

**What:** Both memory embeddings (Phase 2) and code/external embeddings (existing + Phase 3) use the same BGE-M3 model via Ollama.

**How:** Memory tools use the same `s.Cfg.Model` and `NewOllamaEmbedder` as existing tools. No configuration changes needed.

**Risk:** If the embedding model changes, ALL vectors (code, external, memory) become stale simultaneously. This is a known limitation documented in Phase 2 (Section 14.4).

### 6.4 Server Struct Extension

**What:** Phase 2 adds `MemoryStore *heimdall.VectorStore` to the `Server` struct.

**Impact on Phase 3:** Phase 3's `toolExplain` needs to access the memory store for memory counts (Integration Point 6.1). It should use `s.MemoryStore` rather than trying to open a separate connection.

**How:** In `toolExplain`, check if `s.MemoryStore != nil` before querying memory counts:

```go
if s.MemoryStore != nil {
    counts.Memory = s.MemoryStore.MemoryStats().TotalMemories
}
```

---

## 7. Tool Inventory Verification

Post-implementation, we should have exactly 9 tools:

| # | Tool | Source | Phase |
|---|---|---|---|
| 1 | `heimdall_search` | Renamed from `search_context` + enriched results + filtered search | Phase 1 + 3 |
| 2 | `heimdall_index` | Renamed from `index_project` | Phase 1 |
| 3 | `heimdall_index_text` | Renamed from `index_text` + type/metadata/relationships | Phase 1 + 3 |
| 4 | `heimdall_status` | Renamed from `openviking_status` | Phase 1 |
| 5 | `heimdall_projects` | Renamed from `list_projects` | Phase 1 |
| 6 | `heimdall_remember` | New: store explicit memory | Phase 2 |
| 7 | `heimdall_recall` | New: search memories | Phase 2 |
| 8 | `heimdall_ingest_session` | New: auto-extract memories from summary | Phase 2 |
| 9 | `heimdall_explain` | New: retrieval diagnostics | Phase 3 |

**Verification command (post-implementation):**

```bash
echo '{"jsonrpc":"2.0","id":1,"method":"tools/list"}' | ./heimdall-mcp 2>/dev/null | jq '.result.tools[].name' | sort
```

Expected output:
```
"heimdall_explain"
"heimdall_index"
"heimdall_index_text"
"heimdall_ingest_session"
"heimdall_projects"
"heimdall_recall"
"heimdall_remember"
"heimdall_search"
"heimdall_status"
```

---

## 8. Integration Test Plan

### 8.1 Within-Phase Tests (covered by individual phase plans)

- Phase 1: Compilation, grep audit, MCP smoke test, migration tests
- Phase 2: Memory store operations, ingestion dedup, tool handlers
- Phase 3: Filtered search, enriched results, explain tool, typed content

### 8.2 Cross-Feature Integration Tests

These tests verify that features from different phases work together correctly.

| # | Test | Scenario | Validates |
|---|---|---|---|
| 1 | **Search across sources** | Index code files, index external text with type/metadata, store memories. Run `heimdall_search`. Verify results include all source types with correct `source` field. | Phase 2 + 3 integration |
| 2 | **Explain with memory counts** | Store 5 memories via `heimdall_remember`. Run `heimdall_explain` on a query. Verify `sourceCounts.memory == 5`. | Phase 2 + 3 integration |
| 3 | **Ingest + Recall + Explain cycle** | Ingest a session summary. Recall memories. Run explain. Verify memory counts include ingested memories. | Phase 2 + 3 integration |
| 4 | **Typed external + relationship traversal** | Index two related tickets (JIRA-1 implements JIRA-2). Run `heimdall_explain`. Verify relationship traversal returns related snippets. | Phase 3 internal |
| 5 | **Filtered search with typed content** | Index tickets with metadata `{status: "open"}` and docs. Search with `source_type: "ticket"` and `metadata_filter: {status: "open"}`. Verify only matching results returned. | Phase 3 internal |
| 6 | **Memory dedup across sessions** | Store a memory explicitly. Ingest a session summary containing the same content. Verify no duplicate (explicit wins). | Phase 2 internal |
| 7 | **Status includes memory stats** | Store memories. Call `heimdall_status`. Verify response includes memory statistics (if status is extended to show them). | Phase 2 optional |
| 8 | **Migration + new features** | Start with a `.viking_db/` directory. Run `heimdall_status` (triggers migration). Index text with new typed fields. Verify migration + new columns work on migrated DB. | Phase 1 + 3 integration |
| 9 | **Global memory + per-project index** | Register two projects. Store memories (global). Search in project A — memories are accessible. Search in project B — same memories accessible. | Phase 2 design validation |
| 10 | **Full 9-tool smoke test** | Call all 9 tools in sequence. Verify each returns valid JSON-RPC responses (no crashes, no unknown tool errors). | All phases |

### 8.3 Test Infrastructure

All integration tests use:
- `MockEmbedder` (already exists in `internal/openviking/embedder.go`) for deterministic embeddings
- `t.TempDir()` for ephemeral SQLite databases
- Direct method calls on `*Server` (no need for actual stdio JSON-RPC in unit tests)

---

## 9. Risk Assessment

### High Risk

| Risk | Impact | Mitigation |
|---|---|---|
| **Phase 1 rename breaks compilation** | Blocks all subsequent work | Phase 1 is a single atomic commit. Verification step (P1-T8) catches all issues before moving on. `go build` + `go vet` + grep audit. |
| **SQLite schema migration on existing DBs** | Phase 3's ALTER TABLE on populated DBs could fail | Uses same error-ignoring pattern as existing `content_hash` migration (proven pattern). ALTER TABLE ADD COLUMN with DEFAULT is safe on populated tables in SQLite. |

### Medium Risk

| Risk | Impact | Mitigation |
|---|---|---|
| **Phase 2 + Phase 3 edit `store.go` concurrently** | Git merge conflicts in `OpenStore()` | Both changes are at the bottom of `OpenStore()` (after existing migrations). Phase 3 adds column migrations, Phase 2 adds table creation. These are separate blocks of code — merge conflict is a simple append. |
| **Phase 2 + Phase 3 both edit `types.go`** | Git merge conflicts | All new types have unique names. Merge is straightforward append. If implementing in parallel branches, resolve at merge time. |
| **Phase 2 + Phase 3 both edit `server.go`** | Git merge conflicts in `handleToolsList` and `handleToolsCall` | Tool registration is append-only. Each phase adds to the end of the tools slice and switch statement. Simple merge. |
| **Memory DB path confusion** | Users look for memories in wrong location | Resolved by Section 3: all global state in `~/.config/heimdall-mcp/`. Single source of truth. |
| **Phase 1 migration logic** | Filesystem operations could fail | MigrateDBDir/migrateConfigDir are defensive: check existence first, skip if target exists, log errors without crashing. Idempotent by design. |

### Low Risk

| Risk | Impact | Mitigation |
|---|---|---|
| **Performance of `heimdall_explain` full scan** | Slow on large indexes (5000+ chunks) | Acceptable for diagnostic tool. ~50-100ms for 5K chunks. Not meant for real-time use. |
| **Performance of memory semantic dedup** | Slow with thousands of memories | Phase 2 plans O(n) brute-force scan. Acceptable for low thousands. Add `created_at` window filter if needed later. |
| **Embedding model consistency** | Model change invalidates all vectors | Known limitation, documented. Same risk exists pre-Heimdall. No new risk introduced. |

### No Risk

| Item | Reason |
|---|---|
| `go.sum` changes | Handled by `go mod tidy` automatically |
| `ollama-env.sh` | Contains no openviking/viking references |
| Git history | `git mv` preserves file history |
| Existing functionality | All changes are additive (new tables, new columns with defaults, new tools). No existing behavior is modified except tool names (Phase 1). |

---

## 10. Security Hardening (All Phases)

These security measures apply across all phases and must be implemented in every tool handler.

### 10.1 Input Validation — Source Field (SEC-1, CRITICAL)

The `source` field in `index_text`/`heimdall_index_text` is stored as `FilePath` with zero
validation. A malicious MCP client could inject path traversal sequences, control characters,
or extremely long strings.

**Validation function (add to `internal/mcp/server.go` or a shared `validate.go`):**

```go
var sourceValidator = regexp.MustCompile(`^[a-zA-Z0-9._:#@/\-\s]{1,500}$`)

func validateSource(source string) error {
    if len(source) > 500 {
        return fmt.Errorf("source too long: max 500 characters, got %d", len(source))
    }
    if strings.Contains(source, "..") {
        return fmt.Errorf("source must not contain '..'")
    }
    if strings.ContainsAny(source, "\x00\n\r") {
        return fmt.Errorf("source must not contain null bytes or newlines")
    }
    if !sourceValidator.MatchString(source) {
        return fmt.Errorf("source contains invalid characters: use alphanumeric, dots, hyphens, colons, hashes, at-signs, slashes, spaces")
    }
    return nil
}
```

**Apply in:** `toolIndexText` (current + Phase 3 enhanced), before any embedding or DB operations.

### 10.2 Input Length Limits (SEC-2, CRITICAL)

Every tool handler must validate input lengths before any expensive operations:

| Field | Max Length | Applies To |
|---|---|---|
| `content` (index_text, remember) | 100KB (102,400 bytes) | `heimdall_index_text`, `heimdall_remember` |
| `summary` (ingest_session) | 50KB (51,200 bytes) | `heimdall_ingest_session` |
| `query` | 10KB (10,240 bytes) | `heimdall_search`, `heimdall_recall`, `heimdall_explain` |
| `source` | 500 chars | `heimdall_index_text` |
| `tags` | Max 20 items, each max 100 chars | `heimdall_remember`, `heimdall_recall` |
| `metadata` (JSON) | 10KB | `heimdall_index_text` |
| `relationships` (JSON) | 10KB, max 50 entries | `heimdall_index_text` |

**Implementation pattern (add to start of each tool handler):**

```go
func (s *Server) toolIndexText(args json.RawMessage) MCPToolResult {
    // ... parse input ...
    if len(input.Content) > 102400 {
        return ErrResult("content too large: max 100KB")
    }
    if len(input.Source) > 500 {
        return ErrResult("source too long: max 500 characters")
    }
    // ... continue with validation and processing ...
}
```

### 10.3 Migration Race Condition Fix (SEC-3, HIGH)

The `MigrateDBDir()` and `migrateConfigDir()` functions use a check-then-act pattern vulnerable
to TOCTOU races when multiple server instances start simultaneously.

**Fix: Use lock file + re-check after lock:**

```go
func MigrateDBDir(parentDir string) {
    oldDir := filepath.Join(parentDir, ".viking_db")
    newDir := filepath.Join(parentDir, ".heimdall_db")

    // Quick pre-check (no lock needed for the common case: nothing to migrate)
    if _, err := os.Stat(oldDir); err != nil {
        return
    }

    // Acquire exclusive lock file
    lockPath := filepath.Join(parentDir, ".heimdall-migrate.lock")
    lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
    if err != nil {
        return // another process is migrating
    }
    defer os.Remove(lockPath)
    defer lock.Close()

    // Re-check after acquiring lock
    if _, err := os.Stat(oldDir); err != nil {
        return // another process already migrated
    }
    if _, err := os.Stat(newDir); err == nil {
        return // new dir already exists
    }

    if err := os.Rename(oldDir, newDir); err != nil {
        log.Printf("heimdall: failed to migrate %s -> %s: %v", oldDir, newDir, err)
        return
    }
    log.Printf("heimdall: migrated database directory %s -> %s", oldDir, newDir)
}
```

### 10.4 FindSimilarMemory Memory Exhaustion Fix (SEC-4, HIGH)

Loading all memory vectors per chunk during ingestion causes O(chunks * memories) memory
allocations. Fix with two measures:

1. **Cache vectors for ingestion session** — load once, reuse across all chunks:

```go
func IngestSession(ctx context.Context, summary string, project string,
    embedder Embedder, store *MemoryStore) (*IngestResult, error) {
    // Load all memory vectors ONCE for the entire session
    allMemories, err := store.LoadAllMemoryVectors()
    if err != nil {
        return nil, err
    }

    chunks := chunkSummary(summary, 500)
    for _, chunk := range chunks {
        // ... embed chunk ...
        // Use cached allMemories for similarity check instead of re-querying
        similar := findSimilarInCached(allMemories, chunkVec, 0.92)
        // ...
    }
}
```

2. **Configurable max memories limit** — default 10,000. If exceeded, skip semantic dedup
   and rely only on content hash (Tier 1):

```go
const MaxMemoriesForSemanticDedup = 10000

func (s *MemoryStore) FindSimilarMemory(vector []float32, threshold float64) (*Memory, float64, error) {
    count := s.MemoryCount()
    if count > MaxMemoriesForSemanticDedup {
        return nil, 0, nil // fall back to hash-only dedup
    }
    // ... existing logic ...
}
```

### 10.5 Error Message Sanitization (SEC-5, MEDIUM)

Error messages returned to MCP clients must not leak internal filesystem paths:

```go
// sanitizeError strips absolute paths from error messages for MCP client responses.
func sanitizeError(err error) string {
    msg := err.Error()
    // Replace absolute paths with relative basename
    re := regexp.MustCompile(`(/[^\s:]+)+`)
    return re.ReplaceAllStringFunc(msg, func(path string) string {
        return filepath.Base(path)
    })
}

// Usage in tool handlers:
return ErrResult("store error: " + sanitizeError(err))
```

Log full errors to stderr for debugging. Return sanitized messages to MCP client.

### 10.6 File Permissions (SEC-6, MEDIUM)

Registry and config files should use restrictive permissions:

| File | Current | Fixed |
|---|---|---|
| Registry file (`projects.json`) | `0644` | `0600` |
| Config directory | `0755` | `0700` |
| DB directory | `0755` | `0700` |
| Memory DB directory | (new) | `0700` |

**Fix in `registry.go`:** Change `os.WriteFile(r.filePath, data, 0644)` to `os.WriteFile(r.filePath, data, 0600)`.
**Fix in `registry.go`:** Change `os.MkdirAll(dir, 0755)` to `os.MkdirAll(dir, 0700)`.

### 10.7 Content Warning for heimdall_remember (SEC-8, MEDIUM)

Add a warning to the `heimdall_remember` tool description:

```
"description": "Store a memory for persistent recall across sessions. WARNING: Do not store
API keys, passwords, tokens, or other secrets — memories are stored in plaintext and returned
in recall results. Memories are embedded and stored in the local SQLite database."
```

Content filtering is the MCP client's responsibility, not the server's. The server should not
attempt to detect secrets (too many false positives), but the tool description should clearly
warn against storing sensitive data.

### 10.8 matchesMetadata Type Safety (SEC-9/LOW-1)

Replace `fmt.Sprintf("%v")` comparison with JSON-based comparison:

```go
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
        // Type-safe comparison via JSON serialization
        expected, _ := json.Marshal(v)
        actual, _ := json.Marshal(val)
        if !bytes.Equal(expected, actual) {
            return false
        }
    }
    return true
}
```

### 10.9 Null Byte Sanitization (SEC-10/LOW-2)

Strip null bytes and validate UTF-8 before storage:

```go
func sanitizeContent(content string) string {
    // Strip null bytes
    content = strings.ReplaceAll(content, "\x00", "")
    // Ensure valid UTF-8
    if !utf8.ValidString(content) {
        content = strings.ToValidUTF8(content, "\uFFFD")
    }
    return content
}
```

Apply in `toolIndexText`, `toolRemember`, and `IngestSession` before embedding.

---

## 11. Scaling Notes

### Current Approach: Brute-Force Cosine Similarity

All search operations (code search, memory search, filtered search, explain) load vectors
from SQLite and compute cosine similarity in Go. This is acceptable at current scale:

| Operation | Safe Up To | Memory Usage | Time |
|---|---|---|---|
| `heimdall_search` (top-K) | 10K chunks | ~40MB transient | <100ms |
| `heimdall_explain` (all results) | 10K chunks | ~40MB transient | <200ms |
| `SearchMemories` | 5K memories | ~20MB transient | <50ms |
| `FindSimilarMemory` (per-chunk dedup) | 5K memories (cached) | ~20MB cached | <10ms/chunk |
| `SearchFiltered` (with source_type) | 50K chunks | Reduced by SQL filter | <100ms |

### When to Scale

If indexes grow beyond these thresholds, consider:

1. **>10K entries per project:** Add pre-filtering by `source_type` at SQL level before cosine computation (already done by `SearchFiltered`)
2. **>50K entries per project:** Consider SQLite-backed ANN (e.g., `sqlite-vss` extension) or pre-computing a vector index in memory at store open
3. **>10K memories:** Already handled by `MaxMemoriesForSemanticDedup` cap — falls back to hash-only dedup
4. **>100K entries:** Requires architectural change — consider a dedicated vector database or sharding by project

No implementation work needed now. This is documentation for future reference.

---

## Appendix A: Complete New File Inventory

| # | File (post-rename path) | Phase | Purpose |
|---|---|---|---|
| 1 | `internal/heimdall/migrate.go` | Phase 1 | DB + config directory migration helpers (with lock file for SEC-3) |
| 2 | `internal/heimdall/memory_store.go` | Phase 2 | **Dedicated** MemoryStore struct with own schema (ARCH-1 fix) |
| 3 | `internal/heimdall/memory.go` | Phase 2 | Memory types + MemoryStore operations (UpsertMemory, SearchMemories, etc.) |
| 4 | `internal/heimdall/memory_ingest.go` | Phase 2 | Session ingestion: chunking, tagging, dedup (with vector caching for SEC-4) |
| 5 | `internal/heimdall/vecmath.go` | Phase 2 | Shared helpers: cosineSimilarity, encodeFloat32Vec, decodeFloat32Vec (extracted for ARCH-1) |
| 6 | `internal/mcp/memory_tools.go` | Phase 2 | MCP tool handlers for remember/recall/ingest |
| 7 | `internal/mcp/validate.go` | All | Input validation helpers: validateSource, sanitizeContent, sanitizeError, length checks (SEC-1, SEC-2, SEC-5, SEC-10) |
| 8 | `internal/heimdall/memory_test.go` | Phase 2 | Unit tests for memory store operations |
| 9 | `internal/heimdall/memory_ingest_test.go` | Phase 2 | Unit tests for ingestion logic |
| 10 | `internal/mcp/memory_tools_test.go` | Phase 2 | Unit tests for memory tool handlers |
| 11 | `internal/heimdall/store_test.go` | Phase 3 | Unit tests for filtered search, metadata, migration |
| 12 | `internal/mcp/tools_test.go` | Phase 3 | Unit tests for helpers (classifySource, validateSourceType, resolveDBDir) |
| 13 | `internal/mcp/integration_test.go` | Phase 3 + X | Integration tests for all new functionality |

**Total: 13 new files, 17 existing files modified (from Phase 1), additional modifications across phases.**

## Appendix B: Complete Modified File Inventory (All Phases)

| # | File (post-rename path) | Phase 1 | Phase 2 | Phase 3 | Cross |
|---|---|---|---|---|---|
| 1 | `go.mod` | Module path | - | - | - |
| 2 | `cmd/heimdall-mcp/main.go` | Move + imports + comments | MemoryStore init | - | - |
| 3 | `internal/heimdall/store.go` | Package name | - (memories table moved to memory_store.go per ARCH-1) | New columns, VectorRecord, Upsert, Search, SearchFiltered, matchesMetadata, SnippetBySource | - |
| 4 | `internal/heimdall/retriever.go` | Package name | - | ChunkID in ContextBlock | - |
| 5 | `internal/heimdall/indexer.go` | Package name + .viking_db | - | Default SourceType/Metadata/Relationships | - |
| 6 | `internal/heimdall/chunker.go` | Package name | - | - | - |
| 7 | `internal/heimdall/embedder.go` | Package name | - | - | - |
| 8 | `internal/heimdall/ollama.go` | Package name | - | - | - |
| 9 | `internal/mcp/server.go` | Imports, tool names, descriptions | Tool registration (3 tools), MemoryStore field | Tool registration (1 tool), schema updates | - |
| 10 | `internal/mcp/tools.go` | Imports, DB paths, qualifiers | - | Enriched results, filtered search, resolveDBDir, toolExplain, classifySource, resolveRelationships | Memory counts in explain |
| 11 | `internal/mcp/types.go` | Import, qualifier | 3 new input structs | 8+ new types, modified searchInput + indexTextInput | - |
| 12 | `internal/config/config.go` | Config paths, env var, migration | resolveMemoryDBPath() + resolveConfigDir() helpers (CORR-1 fix) | - | - |
| 13 | `internal/registry/registry.go` | Registry path, comment, file permissions 0644->0600 (SEC-6) | - | - | - |
| 14 | `internal/cli/cli.go` | Help text, imports, DB paths | (Optional: CLI memory commands) | - | - |
| 15 | `Makefile` | Binary name, cmd path | - | - | - |
| 16 | `.gitignore` | Binary name, DB dir | - | - | - |
| 17 | `README.md` | All references | - | Tool documentation | - |
