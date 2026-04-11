# Heimdall Evolution — Design Spec

**Date:** 2026-04-11
**Status:** Approved
**Scope:** Rename openviking-mcp → heimdall-mcp + add session memory, retrieval debugging, typed external context

---

## 1. Rename: openviking → heimdall

Pure mechanical rename. No logic changes.

### What changes:
- **Go module:** `github.com/caio-silva/openviking-mcp` → `github.com/caio-silva/heimdall-mcp`
- **Binary:** `openviking-mcp` → `heimdall-mcp`
- **Package dirs:** `internal/openviking/` → `internal/heimdall/`
- **Tool names:** `search_context` → `heimdall_search`, `index_project` → `heimdall_index`, `openviking_status` → `heimdall_status`, `index_text` → `heimdall_index_text`, `list_projects` → `heimdall_projects`
- **Config:** `~/.openviking/` → `~/.heimdall/`, env vars `OPENVIKING_*` → `HEIMDALL_*`
- **DB dir:** `.viking_db/` → `.heimdall_db/` (with migration: detect old dir, rename automatically)
- **Registry:** `~/.openviking/registry.json` → `~/.heimdall/registry.json` (auto-migrate)
- **Server info:** `openviking-mcp` → `heimdall-mcp` in MCP initialize response
- **All comments, docs, README, Makefile, go.mod**

### Migration:
- On startup, if `.viking_db/` exists but `.heimdall_db/` doesn't, rename it
- Same for `~/.openviking/` → `~/.heimdall/`
- Log a one-time migration message to stderr

---

## 2. Session Memory

New tools for persistent memory across sessions.

### New MCP Tools:

#### `heimdall_remember`
Store a memory explicitly.
```json
{
  "content": "User prefers TDD workflow",
  "tags": ["workflow", "testing"],
  "type": "preference",
  "project": "payments-analyzer"
}
```
- `content` (required): the memory text
- `tags` (optional): array of string tags for filtering
- `type` (optional): one of `preference`, `decision`, `fact`, `context` (default: `fact`)
- `project` (optional): scope to a project

#### `heimdall_recall`
Retrieve memories by semantic search + optional filters.
```json
{
  "query": "how does the user like to work",
  "tags": ["workflow"],
  "type": "preference",
  "limit": 5,
  "project": "payments-analyzer"
}
```
Returns memories ranked by relevance with scores.

#### `heimdall_ingest_session`
Auto-extract memories from a conversation summary.
```json
{
  "summary": "We decided to use PostgreSQL instead of SQLite for the payments service because of concurrent write requirements. User prefers PRs over direct commits. We added retry logic to the payment webhook handler.",
  "project": "payments-analyzer"
}
```
- Chunks the summary
- Embeds each chunk
- Stores with `type: "session"` and auto-generated tags
- Deduplicates against existing memories (cosine similarity > 0.92 = duplicate)

### DB Schema:
New `memories` table in the existing SQLite store:
```sql
CREATE TABLE memories (
    id TEXT PRIMARY KEY,
    content TEXT NOT NULL,
    type TEXT NOT NULL DEFAULT 'fact',
    tags TEXT,              -- JSON array
    project TEXT,
    vector BLOB NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    source TEXT DEFAULT 'explicit',  -- 'explicit' or 'session'
    content_hash TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_memories_type ON memories(type);
CREATE INDEX idx_memories_project ON memories(project);
```

### Memory Search:
- Cosine similarity on embeddings (same as code search)
- Post-filter by type, tags, project
- Deduplication: before storing, check if any existing memory has similarity > 0.92 — if so, update instead of insert

---

## 3. Retrieval Debugging

### 3a. Enriched Search Results
Every `heimdall_search` result now includes:
```json
{
  "file": "internal/store.go",
  "startLine": 45,
  "endLine": 78,
  "content": "...",
  "score": 0.847,
  "source": "code",
  "chunkId": "internal/store.go:45:78",
  "embeddingModel": "bge-m3"
}
```
New fields: `score`, `source` (code/external/memory), `chunkId`, `embeddingModel`.

### 3b. `heimdall_explain` Tool
Deep diagnostic for a query.
```json
{
  "query": "payment retry logic",
  "limit": 10,
  "project": "payments-analyzer"
}
```
Returns:
```json
{
  "query": "payment retry logic",
  "queryEmbeddingDim": 1024,
  "totalChunksSearched": 1204,
  "searchTimeMs": 45,
  "results": [
    {
      "rank": 1,
      "chunkId": "internal/payments/retry.go:1:45",
      "file": "internal/payments/retry.go",
      "score": 0.912,
      "source": "code",
      "chunkSizeBytes": 1230,
      "chunkLines": 45
    }
  ],
  "scoreDistribution": {
    "above90": 2,
    "above70": 8,
    "above50": 15,
    "below50": 1179
  },
  "sourceCounts": {
    "code": 1150,
    "external": 42,
    "memory": 12
  },
  "indexStats": {
    "totalChunks": 1204,
    "totalFiles": 89,
    "lastIndexed": "2026-04-11 14:30:00"
  }
}
```

---

## 4. Typed External Context

Upgrade `heimdall_index_text` (formerly `index_text`) with structured types.

### Enhanced Schema:
```json
{
  "content": "As a user I want to retry failed payments automatically",
  "source": "JIRA-456",
  "url": "https://jira.example.com/browse/JIRA-456",
  "type": "ticket",
  "metadata": {
    "status": "in_progress",
    "priority": "high",
    "assignee": "alice",
    "sprint": "2026-Q2-S3"
  },
  "relationships": [
    {"type": "implements", "target": "JIRA-400"},
    {"type": "blocks", "target": "JIRA-470"}
  ],
  "project": "payments-analyzer"
}
```

### Source Types:
`ticket`, `doc`, `pr`, `message`, `changelog`, `note`, `custom`

### DB Schema:
Add columns to `entries` table:
```sql
ALTER TABLE entries ADD COLUMN source_type TEXT DEFAULT 'code';
ALTER TABLE entries ADD COLUMN metadata TEXT DEFAULT '{}';    -- JSON
ALTER TABLE entries ADD COLUMN relationships TEXT DEFAULT '[]'; -- JSON array
```

### Filtered Search:
`heimdall_search` gains optional filters:
```json
{
  "query": "payment retry",
  "source_type": "ticket",
  "metadata_filter": {"status": "in_progress"},
  "limit": 5
}
```

### Relationship Traversal:
When a search result has relationships, `heimdall_explain` includes related items:
```json
{
  "related": [
    {"source": "JIRA-400", "relationship": "implements", "snippet": "..."},
    {"source": "JIRA-470", "relationship": "blocks", "snippet": "..."}
  ]
}
```

---

## 5. Implementation Phases

**Phase 1:** Rename (no logic changes, pure mechanical)
**Phase 2:** Session memory + Retrieval debugging + Typed external context (parallel, on renamed codebase)

### New Tool Summary (post-rename):

| Tool | Purpose |
|---|---|
| `heimdall_search` | Semantic search (enriched with scores, source type, filters) |
| `heimdall_index` | Index a directory (background, async) |
| `heimdall_index_text` | Index external content (typed, with metadata + relationships) |
| `heimdall_status` | Health + stats |
| `heimdall_projects` | List registered projects |
| `heimdall_remember` | Store explicit memory |
| `heimdall_recall` | Retrieve memories |
| `heimdall_ingest_session` | Auto-extract memories from conversation |
| `heimdall_explain` | Retrieval debugging / diagnostics |

**Total: 9 tools** (was 5)
