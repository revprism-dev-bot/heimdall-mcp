# Plan Review 3: Security, Backwards Compatibility, and Migration Safety

**Date:** 2026-04-11
**Reviewer:** Security Review Orchestrator (5 independent reviewers)
**Scope:** Consolidated plan + Phase 1/2/3 plans + design spec + current codebase
**Focus:** SQL injection, path traversal, input validation, memory safety, migration safety, backwards compatibility, concurrency, resource exhaustion, filesystem safety, error leakage

---

## Score: 78/100 — NEEDS_FIX

**Deductions:**
- CRITICAL-1: Path traversal via `source` field in `index_text`/`heimdall_index_text` (-15)
- CRITICAL-2: No input length limits on any MCP tool input (-15)
- HIGH-1: Migration race condition — no filesystem locking on `os.Rename` (-8)
- HIGH-2: `FindSimilarMemory` full table scan loads all vectors into memory (-8)
- MEDIUM-1: Error messages leak internal filesystem paths (-3)
- MEDIUM-2: Registry file written with world-readable permissions (0644) (-3)
- MEDIUM-3: `ingest_session` summary has no size cap — unbounded embedding calls (-3)
- MEDIUM-4: `heimdall_remember` can store arbitrary content including secrets/tokens (-3)
- LOW-1: `matchesMetadata` uses `fmt.Sprintf("%v")` for comparison — type coercion bugs (-1)
- LOW-2: No content sanitization on `content` field before storage (-1)

---

## Finding Details

### CRITICAL-1: Path Traversal via `source` Field (Current Code + All Plans)

**Severity:** CRITICAL (-15)
**Affected code:** `internal/mcp/server.go:258` (current), Phase 3 plan (enhanced `heimdall_index_text`)
**Attack vector:** The `source` field from `indexTextInput` is used directly in the record ID (`fmt.Sprintf("ext:%s:%d", input.Source, i)`) and stored as `FilePath` in the database. The `source` field is also used by `SnippetBySource` (Phase 3) as a SQL query parameter. While SQL injection is prevented by parameterized queries, the `source` field has **no validation at all**.

A malicious MCP client could set `source` to:
- `"../../../../etc/passwd"` — stored as `FilePath`, returned in search results, potentially confusing downstream consumers
- Extremely long strings (no length limit) — stored in DB, returned in all search/explain results
- Strings containing newlines, null bytes, or control characters — corrupting JSON output

More critically, `SnippetBySource` (Phase 3 plan) does `SELECT content FROM entries WHERE file_path = ?` using the `source` as the lookup key. While this is parameterized (safe from SQL injection), a crafted `source` value could be used to probe what entries exist in the database.

**Current codebase evidence:**
```go
// server.go:258 — source used directly as FilePath
records = append(records, openviking.VectorRecord{
    ID:       fmt.Sprintf("ext:%s:%d", input.Source, i),
    FilePath: input.Source,  // NO VALIDATION
    ...
})
```

**Fix:**
1. Validate `source` field: max 500 characters, no path separators (`/`, `\`, `..`), no null bytes, no control characters
2. Or sanitize: strip path components, keep only the identifier portion
3. Add a dedicated `Source` field to `VectorRecord` separate from `FilePath` so external content identifiers don't masquerade as file paths

---

### CRITICAL-2: No Input Length Limits on Any MCP Tool Input (All Plans)

**Severity:** CRITICAL (-15)
**Affected code:** All tool handlers in `internal/mcp/tools.go` and `internal/mcp/server.go`; all planned tool handlers
**Attack vector:** None of the MCP tool inputs have maximum length validation:

| Field | Current Limit | Risk |
|---|---|---|
| `searchInput.Query` | None | Sent to Ollama for embedding — huge query = OOM or timeout |
| `indexTextInput.Content` | None | Chunked and embedded — 100MB content = thousands of Ollama calls, fills DB |
| `indexTextInput.Source` | None | Stored as FilePath — massive string in every row |
| `rememberInput.Content` (Phase 2) | None | Stored + embedded — unbounded |
| `ingestSessionInput.Summary` (Phase 2) | None | Chunked into N memories — 10MB summary = thousands of memories |
| `explainInput.Query` (Phase 3) | None | Embedded + full table scan |
| `indexTextInput.Metadata` (Phase 3) | None | Stored as JSON string — massive metadata per row |
| `indexTextInput.Relationships` (Phase 3) | None | Stored as JSON array — massive relationships per row |

**Impact:** A single malicious `heimdall_index_text` call with a 100MB content body would:
1. Generate hundreds of embedding API calls to Ollama (each taking seconds)
2. Store hundreds of large records in the SQLite database
3. Make all subsequent searches slow (full table scan over bloated records)

**Fix:**
1. Add explicit max lengths to all string inputs:
   - `query`: max 10,000 characters
   - `content`: max 1MB (or 100KB to match `MaxFileSize`)
   - `source`: max 500 characters
   - `summary`: max 100KB
   - `metadata`: max 10KB
   - `relationships`: max 10KB (or max 50 relationship entries)
   - `content` for `heimdall_remember`: max 10KB
2. Validate at the start of each tool handler before any expensive operations
3. Document limits in tool descriptions so MCP clients know the constraints

---

### HIGH-1: Migration Race Condition — No Filesystem Locking (Phase 1)

**Severity:** HIGH (-8)
**Affected code:** Phase 1 plan, `MigrateDBDir()` and `migrateConfigDir()`
**Attack vector:** The planned migration logic uses a check-then-act pattern without filesystem locking:

```go
// Phase 1 plan — MigrateDBDir
if _, err := os.Stat(oldDir); err != nil {
    return  // old dir doesn't exist
}
if _, err := os.Stat(newDir); err == nil {
    return  // new dir already exists
}
os.Rename(oldDir, newDir)  // TOCTOU race here
```

**Race scenario:**
1. Two MCP server instances start simultaneously (e.g., Claude Code + Claude Desktop both configured with heimdall-mcp)
2. Both check: old dir exists, new dir doesn't exist
3. Both call `os.Rename(oldDir, newDir)`
4. First rename succeeds
5. Second rename fails because `oldDir` no longer exists — **silent failure** (error is logged but not returned)

**Worse scenario:**
1. Instance A checks: old exists, new doesn't
2. Instance B creates the new dir (e.g., by running `heimdall_index`)
3. Instance A calls `os.Rename(oldDir, newDir)` — on Linux, `os.Rename` on directories when target exists either fails (good) or replaces (data loss, bad depending on filesystem)

**Impact:** Potential data loss if both old and new DB directories exist with different content and a race causes one to overwrite the other.

**Fix:**
1. Use `os.Rename` atomicity guarantees (on the same filesystem, rename is atomic) but add a lock file:
   ```go
   lockPath := filepath.Join(parentDir, ".heimdall_migrate.lock")
   lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL, 0600)
   if err != nil {
       return // another process is migrating
   }
   defer os.Remove(lockPath)
   defer lock.Close()
   ```
2. Re-check conditions after acquiring lock
3. Or simpler: accept the race and make the error handling more robust — if `os.Rename` fails, check if the new dir already exists (another process won) and if so, verify the old dir is gone

---

### HIGH-2: `FindSimilarMemory` Full Table Scan Into Memory (Phase 2)

**Severity:** HIGH (-8)
**Affected code:** Phase 2 plan, `FindSimilarMemory` in `memory.go`
**Attack vector:** The planned `FindSimilarMemory` loads ALL memory vectors into Go memory for cosine similarity comparison:

```go
rows, err := s.db.Query(`SELECT id, content, type, tags, project, vector,
    created_at, updated_at, source, content_hash FROM memories`)
// ... iterate ALL rows, decode ALL vectors, compute similarity
```

Each BGE-M3 vector is 1024 floats = 4KB. With 10,000 memories, this loads 40MB of vector data into memory **per dedup check**. Since `heimdall_ingest_session` calls `FindSimilarMemory` once per chunk, a 50-chunk summary would load 50 * 40MB = 2GB of transient memory allocations.

**Impact:** Memory exhaustion on systems with many stored memories. The plan acknowledges this ("Performance note: brute force is acceptable") but underestimates the multiplicative effect when called per-chunk during ingestion.

**Fix:**
1. **Tier 1 (immediate):** Add a `LIMIT` to the query — only check recent memories (e.g., last 1000 by `updated_at`)
2. **Tier 2 (better):** Cache the loaded vectors for the duration of an `IngestSession` call — load once, check N chunks against the same set
3. **Tier 3 (best):** Pre-compute and cache a vector index in memory when the memory store is opened, update incrementally on writes
4. At minimum, add a hard cap: if memory count exceeds 10,000, skip semantic dedup and rely only on content hash (Tier 1 dedup)

---

### MEDIUM-1: Error Messages Leak Internal Filesystem Paths (Current Code + All Plans)

**Severity:** MEDIUM (-3)
**Affected code:** Multiple locations in `tools.go`, `server.go`
**Evidence from current codebase:**

```go
// tools.go:113 — leaks absolute path
return ErrResult("path does not exist: " + absPath)

// tools.go:60 — leaks DB directory suggestion
return ErrResult("No index found. Run index_project first.")

// server.go:65 — leaks store error with DB path
return ErrResult("store error: " + err.Error())
```

The `store error` messages from SQLite can include full filesystem paths in the error text (e.g., "unable to open database file: /home/user/.config/heimdall-mcp/memories.db: permission denied").

**Impact:** An MCP client (or anyone reading tool results) learns internal filesystem layout, username, config directory structure. Low severity for a local-only tool, but relevant if results are shared or logged.

**Fix:**
1. Sanitize error messages before returning: strip absolute paths, replace with relative references
2. Log full errors to stderr (for debugging), return generic messages to MCP client
3. Example: `ErrResult("store error: unable to open database")` instead of passing through the raw SQLite error

---

### MEDIUM-2: Registry File Written with World-Readable Permissions (Current Code)

**Severity:** MEDIUM (-3)
**Affected code:** `internal/registry/registry.go:60`
**Evidence:**

```go
return os.WriteFile(r.filePath, data, 0644)
```

The registry file contains absolute filesystem paths to all indexed projects and their database locations. Permission 0644 means any user on the system can read it.

**Impact:** Information disclosure — any local user can enumerate all projects the heimdall user has indexed, including their absolute paths.

**Fix:**
1. Use `0600` instead of `0644` for the registry file
2. Similarly, the config directory is created with `0755` (`os.MkdirAll(dir, 0755)`) — consider `0700`
3. Apply same fix to the planned `memories.db` — the `OpenStore` already uses `0755` for the DB directory, which should be `0700`

---

### MEDIUM-3: `ingest_session` Summary Has No Size Cap (Phase 2)

**Severity:** MEDIUM (-3)
**Affected code:** Phase 2 plan, `heimdall_ingest_session` tool
**Attack vector:** The `chunkSummary` function splits the summary into 500-char chunks. A 1MB summary = 2,000 chunks. Each chunk:
1. Gets embedded (Ollama API call — ~100ms each = 200 seconds of embedding time)
2. Gets a `FindSimilarMemory` dedup check (full table scan per chunk)
3. Gets stored as a separate memory record

**Impact:** A single `heimdall_ingest_session` call with a large summary could:
- Take 10+ minutes to complete
- Create thousands of memory records
- Cause memory exhaustion (see HIGH-2)
- Block Ollama for other embedding requests

**Fix:**
1. Cap summary input at 50KB (or 100 chunks max)
2. Add a maximum chunks-per-ingestion limit in `chunkSummary`
3. Return an error if the summary exceeds the limit, rather than silently processing a massive input

---

### MEDIUM-4: `heimdall_remember` Can Store Secrets/Tokens (Phase 2)

**Severity:** MEDIUM (-3)
**Affected code:** Phase 2 plan, `heimdall_remember` tool
**Attack vector:** There is no content filtering on what gets stored as a memory. An MCP client (or the LLM itself) could store:
- API keys, tokens, passwords
- PII (personal identifiable information)
- Session tokens or auth credentials

These memories are:
1. Stored in plaintext in SQLite (no encryption at rest)
2. Returned in `heimdall_recall` results (no redaction)
3. Globally accessible (not scoped to a session or user)
4. Persistent across sessions (by design)

**Impact:** Credential leakage through memory recall. If the LLM stores a token as a "fact" memory, any future session can recall it.

**Fix:**
1. Add a content filter that rejects or warns on common secret patterns:
   - API key patterns (`sk-`, `ghp_`, `Bearer `, `eyJ`)
   - AWS keys (`AKIA`)
   - Generic password patterns
2. Add a `sensitive` flag to memory records and exclude them from default recall results
3. At minimum, document the risk in the tool description: "Do not store API keys, passwords, or other secrets as memories"
4. Consider encrypting the memories.db at rest

---

### LOW-1: `matchesMetadata` Uses `fmt.Sprintf("%v")` for Comparison (Phase 3)

**Severity:** LOW (-1)
**Affected code:** Phase 3 plan, `matchesMetadata` function
**Issue:** The plan uses `fmt.Sprintf("%v", val) != fmt.Sprintf("%v", v)` for comparing metadata values. This has type coercion issues:
- `"1"` (string) vs `1` (number from JSON) — both produce `"1"` via `%v`, so they match even though they're different types
- `true` (bool) vs `"true"` (string) — both produce `"true"` via `%v`
- Maps and slices produce Go-syntax strings, not JSON

**Fix:** Use `json.Marshal` for both values and compare the JSON bytes, or use `reflect.DeepEqual`.

---

### LOW-2: No Content Sanitization Before Storage (Current Code + All Plans)

**Severity:** LOW (-1)
**Affected code:** All tool handlers that store content
**Issue:** Content stored in the database is not sanitized for:
- Null bytes (can cause issues with SQLite TEXT columns and Go string handling)
- Extremely long single lines (can cause issues with downstream consumers)
- Invalid UTF-8 sequences

**Fix:** Add a `sanitizeContent` helper that strips null bytes and validates UTF-8 before storage.

---

## Additional Observations (Not Scored)

### SQL Injection: PASS
All current SQL queries use parameterized queries (`?` placeholders). The plans maintain this pattern. The `SearchFiltered` plan correctly builds the WHERE clause with `?` parameters. No string interpolation in SQL anywhere in the codebase or plans.

### Concurrent Access: MOSTLY PASS
The current `VectorStore` uses `sync.RWMutex` correctly (RLock for reads, Lock for writes). Phase 2's memory operations plan to follow the same pattern. However, the `MemoryStore` is a separate `*VectorStore` instance on the `Server` struct — concurrent tool calls accessing it are safe because each `VectorStore` has its own mutex. SQLite WAL mode (`_journal_mode=WAL`) allows concurrent readers with a single writer.

**Minor concern:** The `Server.Index` state uses a `sync.Mutex` (not RWMutex), meaning status checks (`toolStatus`) block during indexing state updates. This is correct but could be optimized to `sync.RWMutex` for read-heavy status checks.

### Backwards Compatibility: PASS WITH NOTES
- Old MCP clients using `search_context`, `index_project`, etc. will get "unknown tool" errors after the rename. This is a **breaking change** by design. The plan does not include backwards-compatible aliases. This is acceptable for a pre-1.0 tool but should be documented in release notes.
- Old `.viking_db/` directories are migrated automatically. The migration is one-way (no rollback).
- Old config at `~/.config/openviking-mcp/` is migrated automatically.
- SQLite schema migrations are additive (new columns with defaults, new tables) — existing data is preserved.

### Config File Parsing: MINOR CONCERN
`config.LoadConfig()` silently ignores JSON parse errors and returns defaults. If a user has a malformed config file, they get default behavior with no warning. Not a security issue, but a usability concern.

---

## Summary of Required Fixes

### Must Fix Before Implementation (Score < 95)

| # | Severity | Finding | Fix Effort |
|---|---|---|---|
| CRITICAL-1 | CRITICAL | Path traversal via `source` field | Small — add validation |
| CRITICAL-2 | CRITICAL | No input length limits | Medium — add to all handlers |
| HIGH-1 | HIGH | Migration TOCTOU race | Small — add lock file |
| HIGH-2 | HIGH | Memory exhaustion in `FindSimilarMemory` | Medium — cache vectors |
| MEDIUM-1 | MEDIUM | Error path leakage | Small — sanitize errors |
| MEDIUM-2 | MEDIUM | World-readable registry | Trivial — change to 0600 |
| MEDIUM-3 | MEDIUM | Unbounded `ingest_session` | Small — add size cap |
| MEDIUM-4 | MEDIUM | Secrets in memory storage | Medium — add content filter |
| LOW-1 | LOW | `matchesMetadata` type coercion | Trivial — use json.Marshal |
| LOW-2 | LOW | No content sanitization | Small — add sanitize helper |

### Verdict: NEEDS_FIX

Score 78/100. The plan needs fixes for the two CRITICAL findings (path traversal, input length limits) and the two HIGH findings (migration race, memory exhaustion) before implementation can proceed. The MEDIUM and LOW findings should be addressed during implementation.
