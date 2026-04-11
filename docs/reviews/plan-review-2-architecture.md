# Plan Review 2: Architecture Quality

**Reviewer:** Architecture Review Agent
**Date:** 2026-04-11
**Scope:** Consolidated plan + Phase 1/2/3 plans + Design spec vs. actual codebase
**Method:** 5 independent review passes (YAGNI, Separation of Concerns, Data Model, Interface Design, Performance/Testability)

---

## Score: 88/100 — NEEDS_FIX

| Severity | Count | Deduction |
|---|---|---|
| CRITICAL | 0 | 0 |
| HIGH | 1 | -8 |
| MEDIUM | 1 | -3 |
| LOW | 1 | -1 |

---

## Findings

### ARCH-1 [HIGH] `VectorStore` is becoming a god object — memory operations should not be bolted onto it

**Location:** Phase 2 plan, `internal/heimdall/memory.go` — methods on `*VectorStore`

**Problem:** The plan adds `UpsertMemory`, `SearchMemories`, `FindSimilarMemory`, `GetMemoryByHash`, and `MemoryStats` directly onto `VectorStore`. This struct currently has a clear single responsibility: managing the `entries` table for code/external vector storage. Adding memory operations to it violates separation of concerns:

1. `VectorStore` operates on per-project databases (opened per-tool-call, closed immediately). The memory store is a **global singleton** opened once at startup and kept alive for the server lifetime. These are fundamentally different lifecycle models.
2. The `memories` table has a completely different schema, different query patterns, different dedup logic, and different ID generation than `entries`. Sharing the struct conflates two unrelated data models.
3. The plan acknowledges the memory DB is at `~/.config/heimdall-mcp/memories.db` while code DBs are at `<project>/.heimdall_db/vectors.db`. Despite this, it reuses `VectorStore` by opening it at a different path. This works mechanically (both are SQLite), but the `OpenStore` function will now create the `entries` table inside the memory DB file (it always runs `CREATE TABLE IF NOT EXISTS entries`), and the `memories` table inside every project DB. This is schema pollution — every project DB gets a phantom `memories` table, and the memory DB gets a phantom `entries` table.

**Suggestion:** Extract a dedicated `MemoryStore` struct in `internal/heimdall/memory_store.go`. It wraps `*sql.DB` with its own `Open`/`Close`, its own mutex, and its own schema creation (only the `memories` table). Share the low-level helpers (`encodeFloat32Vec`, `decodeFloat32Vec`, `cosineSimilarity`) by making them package-level functions (they already are). The `Server` struct holds `MemoryStore *heimdall.MemoryStore` instead of reusing `*heimdall.VectorStore`.

This costs ~30 extra lines of boilerplate but prevents schema pollution, keeps lifecycles clean, and makes each store independently testable.

**Deduction:** -8

---

### ARCH-2 [MEDIUM] Brute-force cosine similarity on `SearchAll`/`Search(0)` for `heimdall_explain` loads all embeddings into memory — no concern at current scale, but the plan should document the scaling ceiling

**Location:** Phase 3 plan, Part A2 — `heimdall_explain` calls `store.Search(queryVec, 0)`

**Problem:** The `Search` method (store.go:96-128) loads every row from `entries`, deserializes every embedding blob, computes cosine similarity, sorts, and returns all results. For `heimdall_explain`, this means:

- 10K chunks x ~4KB embedding (1024 float32s) = **~40MB** loaded into memory per explain call
- O(n) cosine computations + O(n log n) sort
- The plan says "50-100ms for 5K chunks" which is optimistic — the bottleneck is SQLite row scanning + blob deserialization, not the math

This is acceptable for a diagnostic tool at current scale. The plan acknowledges it. However, the plan does NOT document the scaling ceiling or when to switch strategies. The same brute-force pattern applies to `SearchMemories` (Phase 2) and `SearchFiltered` (Phase 3).

**Suggestion:** Add a brief "Scaling Notes" section to the consolidated plan documenting:
- Current approach is brute-force, acceptable for <10K entries / <5K memories
- If indexes grow beyond 50K entries, consider SQLite-backed ANN (e.g., `sqlite-vss` extension) or pre-filtering by `source_type` before cosine computation
- The `SearchFiltered` method already does source_type pre-filtering at the SQL level, which is the right first step

This is documentation, not implementation work. No code change needed now.

**Deduction:** -3

---

### ARCH-3 [LOW] `resolveDBDir` helper in Phase 3 has subtly different behavior than existing code in `toolSearch` and `toolIndexText`

**Location:** Phase 3 plan, Part C1 — `resolveDBDir` helper

**Problem:** The plan's `resolveDBDir` returns `""` when no DB is found (after checking project, CWD registry, and fallback path). But the existing `toolSearch` (tools.go:37-57) has a different fallback: it returns `filepath.Join(cwd, ".viking_db")` unconditionally as the fallback, then checks `os.Stat` on the result. The existing `toolIndexText` (server.go:219-232) does NOT check `os.Stat` — it creates the DB dir if it doesn't exist (via `OpenStore` which calls `os.MkdirAll`).

The plan's `resolveDBDir` adds an `os.Stat` check before returning the fallback path. This changes behavior for `toolIndexText`, which currently creates the DB directory on first use. After the refactor, `toolIndexText` would get `""` from `resolveDBDir` and return an error instead of creating the DB.

**Suggestion:** `resolveDBDir` should have two variants or a parameter: one for read-only tools (search, explain) that returns `""` if no DB exists, and one for write tools (index, index_text) that returns the fallback path without the existence check. Or simply: `resolveDBDir` returns the fallback path always, and the caller decides whether to check existence.

**Deduction:** -1

---

## Pass-by-Pass Analysis

### Pass 1: YAGNI

**Verdict: PASS.** The plan is well-scoped. No speculative abstractions detected.

- The `classifySource` helper includes a `"memory"` case that doesn't exist yet — this is explicitly noted as "future-proofing" and is a 2-line switch case, not an abstraction. Acceptable.
- Phase 2's `MemoryType` constants (preference, decision, fact, context) are all used in the classification and filtering logic. No unused types.
- The `RelatedItem` struct and `resolveRelationships` are only built when relationships actually exist on a record. No speculative traversal.
- CLI memory commands (Phase 2, Task 11) are correctly marked as "optional, lower priority." Good.

### Pass 2: DRY / Code Duplication

**Verdict: PASS with note.** The plan identifies and addresses the main DRY violations:

- DB path resolution duplication is extracted to `resolveDBDir` (Phase 3, Part C1). Good.
- The `chunkSummary` function (Phase 2) is separate from `chunkText` (existing) — these have different splitting strategies (sentence vs. paragraph) and different chunk sizes (500 vs. 1500). Not a DRY violation; they are genuinely different algorithms for different purposes.
- The `contentHash` function (Phase 2) and `hashFile` function (existing indexer.go) both compute SHA-256 but on different inputs (normalized text string vs. raw file bytes). Not a DRY violation.

**Note:** `encodeFloat32Vec`, `decodeFloat32Vec`, and `cosineSimilarity` are currently methods in `store.go`. If a `MemoryStore` is extracted per ARCH-1, these should become package-level functions to avoid duplication. They already have no receiver dependency.

### Pass 3: Separation of Concerns

**Verdict: NEEDS_FIX** — see ARCH-1 above.

Additional observations:
- Tool handlers (`internal/mcp/`) correctly delegate to domain logic (`internal/heimdall/`). The MCP layer handles JSON parsing and error formatting; the domain layer handles business logic. Good separation.
- The `Server` struct is a thin coordinator, not a god object. Adding `MemoryStore` to it is appropriate.
- Phase 2 puts memory types in `internal/heimdall/memory.go` and tool handlers in `internal/mcp/memory_tools.go`. This maintains the existing package boundary pattern.
- Phase 3 puts new store methods in `store.go` and new tool logic in `tools.go`. This keeps related code together.

### Pass 4: Interface Design / Data Model

**Verdict: PASS.**

- **MCP tool naming:** All new tools use the `heimdall_` prefix consistently. Tool names are descriptive: `remember`, `recall`, `ingest_session`, `explain`. Good.
- **Input schemas:** All required fields are marked as such. Optional fields have sensible defaults. The `type` field on `heimdall_remember` uses an enum constraint. Good.
- **DB schema:** The `memories` table is properly normalized. Tags as JSON array in TEXT is a pragmatic choice for SQLite (no separate tags table needed at this scale). The `content_hash` column enables fast dedup. Indexes are on the right columns (type, project, source, content_hash).
- **`entries` table extensions:** Adding `source_type`, `metadata`, `relationships` as TEXT columns with defaults is the right approach for SQLite. Avoids schema complexity while enabling the filtering use case.
- **Embedder interface:** Already well-defined with `MockEmbedder` for testing. All new features use the same interface. No changes needed.
- **Error handling:** Tool handlers consistently return `ErrResult()` for user-facing errors. Domain functions return `error`. The pattern is established and the plans follow it.

### Pass 5: Performance / Testability / Extensibility

**Verdict: PASS with note** — see ARCH-2 above.

**Testability:**
- All new store operations can be tested with `t.TempDir()` + `OpenStore()`. Good.
- `MockEmbedder` (already exists) enables deterministic embedding tests without Ollama. Good.
- The plan specifies 33+ unit tests and 10 integration tests. Coverage is thorough.
- The `IngestSession` function takes `Embedder` and `*VectorStore` as parameters — proper dependency injection. Good.

**Extensibility:**
- Adding new `MemoryType` values: add a constant + update `ValidMemoryTypes` map. Minimal change.
- Adding new source types for `index_text`: add to `validSourceTypes` map. Minimal change.
- Adding new relationship types: no code change needed — relationships are stored as JSON, types are strings.
- Adding a new MCP tool: add schema to `handleToolsList`, case to `handleToolsCall`, implement handler. The pattern is clear and repeatable.

---

## Summary

The plan is architecturally sound with one structural issue (ARCH-1) that should be fixed before implementation. The `VectorStore` reuse for memories creates schema pollution and conflates two different lifecycle models. Extracting a `MemoryStore` is straightforward and prevents technical debt.

The remaining findings (ARCH-2, ARCH-3) are minor: one is a documentation gap about scaling limits, the other is a subtle behavioral difference in a refactored helper that should be caught during implementation.

**Score: 88/100 — NEEDS_FIX (below 95 threshold)**

To reach 95+:
1. Fix ARCH-1: Extract `MemoryStore` from `VectorStore` (+8 points)
2. Fix ARCH-2: Add scaling notes to consolidated plan (+3 points)
3. Fix ARCH-3: Clarify `resolveDBDir` behavior for read vs. write tools (+1 point)
