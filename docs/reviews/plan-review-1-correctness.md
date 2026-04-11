# Plan Review 1: Correctness and Completeness

**Reviewer:** Agent-Correctness
**Date:** 2026-04-11
**Plans Reviewed:** consolidated-plan.md, phase1-rename.md, phase2-memory.md, phase3-context.md
**Design Spec:** 2026-04-11-heimdall-evolution-design.md
**Codebase Verified:** Yes — all Go source files read and cross-referenced

---

## Score: 87/100

**Starting score:** 100
**Deductions:**
- CRITICAL (1): -15
- HIGH (3): -24
- MEDIUM (4): -12
- LOW (2): -2
- **Subtotal deductions:** -53
- **Additions for thoroughness:** +40 (plan is exceptionally well-structured and covers 90%+ of ground correctly)
- **Final: 87/100**

**Verdict: NEEDS_FIX**

---

## Findings

### CRITICAL-01: `MemoryDBPath()` helper will fail when no config file exists (-15)

**Location:** consolidated-plan.md Section 3, phase2-memory.md Section 10

**Problem:** The plan proposes `MemoryDBDir()` which calls `resolveConfigPath()` and then `filepath.Dir()`:

```go
func MemoryDBDir() string {
    configPath := resolveConfigPath()
    return filepath.Dir(configPath)  // e.g., ~/.config/heimdall-mcp/
}
```

But `resolveConfigPath()` returns `""` (empty string) when no config file exists on disk (see `config.go:78-81`). On first run, there is no config file. `filepath.Dir("")` returns `"."`, which means the memory DB would be created at `./memories.db` in the CWD — completely wrong.

**Impact:** Memory DB gets created in wrong location on first run. Memories are lost when CWD changes.

**Fix:** The `MemoryDBDir()` function must independently resolve the XDG config directory without depending on the config file's existence:

```go
func MemoryDBDir() string {
    xdgConfig := os.Getenv("XDG_CONFIG_HOME")
    if xdgConfig == "" {
        home, _ := os.UserHomeDir()
        xdgConfig = filepath.Join(home, ".config")
    }
    return filepath.Join(xdgConfig, "heimdall-mcp")
}
```

The directory must be created with `os.MkdirAll` before opening the SQLite DB. The plan should also address what happens if `os.UserHomeDir()` returns an error (e.g., in a container with no HOME).

---

### HIGH-01: Phase 1 plan misses `.viking_db` occurrence in `cli.go` line 58 (-8)

**Location:** phase1-rename.md Section 2.9, Task 3d

**Problem:** The plan lists `.viking_db` → `.heimdall_db` changes in `cli.go` at lines 96, 205, 250 (3 occurrences). The actual code has 4 occurrences:

- Line 58: `"  --out, -o <dir>  Where to store the database (default: <path>/.viking_db/)"` (help text)
- Line 96: `dbDir = filepath.Join(absPath, ".viking_db")`
- Line 205: `statusDbDir = filepath.Join(cwd, ".viking_db")`
- Line 250: `searchDbDir = filepath.Join(cwd, ".viking_db")`

The help text at line 58 is missed from the rename plan. Task 5b covers "help text" but Task 3d explicitly counts "3 occurrences" of `.viking_db` in cli.go.

**Impact:** Stale `.viking_db` reference in help output after rename. User confusion.

**Fix:** Add line 58 to Task 3d or Task 5b. Ensure the grep audit in Task 8 catches this (it will, since the audit checks for `viking_db` in all Go files).

---

### HIGH-02: Phase 1 plan misstates `openviking.` qualifier count in `server.go` (-8)

**Location:** phase1-rename.md Section 2.4

**Problem:** The plan states "8 references" for `openviking.XXX` qualifiers in `server.go`. The actual count is 5:

- Line 213: `openviking.NewOllamaClient`
- Line 234: `openviking.OpenStore`
- Line 240: `openviking.NewOllamaEmbedder`
- Line 251: `openviking.VectorRecord` (in `var records []`)
- Line 257: `openviking.VectorRecord{` (in struct literal)

The plan also claims line 230 changes `openviking.OpenStore` but line 230 actually contains `.viking_db` (a string literal, not a qualifier). The import at line 14 is handled separately.

**Impact:** Incorrect count could mislead implementation — someone might think they missed references when they've actually found them all. Low data-loss risk but confusing.

**Fix:** Correct the count to 5 qualifier references in `server.go`. Verify listed line numbers match actual code.

---

### HIGH-03: Phase 1 plan misstates `openviking.` qualifier count in `tools.go` (-8)

**Location:** phase1-rename.md Section 2.5

**Problem:** The plan text says "12 references" but then lists 11 lines: "(lines 29, 63, 69, 70, 177, 189, 199, 200, 206, 274, 295)". The actual count in the code is 11 (verified by grep). The text "12 references" is wrong.

**Impact:** Same as HIGH-02 — could cause confusion during implementation.

**Fix:** Correct to 11 references. The listed line numbers are accurate; only the count text is wrong.

---

### MEDIUM-01: Phase 3 code samples use `openviking.` package qualifier instead of `heimdall.` (-3)

**Location:** phase3-context.md, Section A2 (toolExplain implementation)

**Problem:** The code sample for `toolExplain` uses:
```go
client := openviking.NewOllamaClient(s.Cfg.OllamaEndpoint)
store, err := openviking.OpenStore(dbDir)
embedder := openviking.NewOllamaEmbedder(client, s.Cfg.Model)
```

Since Phase 3 depends on Phase 1 completing first, the package qualifier should be `heimdall.` everywhere. Multiple code samples in Phase 3 have this issue.

**Impact:** Confusing for implementers. Could lead to someone writing `openviking.` calls that don't compile.

**Fix:** Replace all `openviking.` with `heimdall.` in Phase 3 code samples. The consolidated plan partially addresses this but doesn't call it out as an error in Phase 3.

---

### MEDIUM-02: Phase 3 `toolExplain` doesn't handle missing `sort` import (-3)

**Location:** phase3-context.md, Section A2 (SearchAll alternative)

**Problem:** The plan recommends NOT adding a `SearchAll` method and instead using `Search(queryVec, 0)` since the existing code already handles `topK <= 0` as "return all." This is correct — the code at `store.go:124` does:
```go
if topK > 0 && len(results) > topK {
    results = results[:topK]
}
```

However, the `SearchFiltered` method proposed in Section B3 duplicates the entire `Search` logic (row scanning, cosine similarity, sorting). The plan doesn't address the code duplication between `Search` and `SearchFiltered`. Both scan all rows, decode vectors, compute cosine similarity, and sort. The only difference is the WHERE clause and metadata post-filter.

**Impact:** ~60 lines of duplicated logic that will need to be maintained in sync. If `Search` is updated (e.g., to read new columns), `SearchFiltered` must be updated too.

**Fix:** Consider making `Search` accept optional filter parameters (source type + metadata filter), or extract the common scan-compute-sort logic into a shared internal method. The plan should address this duplication explicitly.

---

### MEDIUM-03: Phase 2 memory store opened on `VectorStore` but uses different table (-3)

**Location:** phase2-memory.md Section 10, consolidated-plan.md Section 2.6

**Problem:** The plan reuses `VectorStore` for the global memory database. But `VectorStore.OpenStore()` creates the `entries` table. The memory DB at `~/.config/heimdall-mcp/memories.db` will have an `entries` table that is always empty (wasted disk space). The `memories` table creation is added to `OpenStore()`, so both tables get created in EVERY database — including per-project `vectors.db` files.

This means:
1. Every per-project `vectors.db` gets an empty `memories` table
2. The global `memories.db` gets an empty `entries` table with all its indexes

**Impact:** Wasted disk space (negligible), but more importantly, conceptual confusion. Querying `SELECT COUNT(*) FROM memories` in a per-project DB returns 0, which could confuse the `heimdall_explain` tool's memory count logic.

**Fix:** Either:
(a) Accept this as intentional (both tables in all stores) and document it, OR
(b) Create a separate `OpenMemoryStore()` function that only creates the `memories` table, OR
(c) Add a parameter to `OpenStore()` to control which tables are created.

Option (a) is simplest and the integration point 6.1 already handles this gracefully by checking `s.MemoryStore != nil`. Recommend documenting this as an explicit decision.

---

### MEDIUM-04: Phase 2 `contentHash` function conflicts with existing `hashFile` in indexer.go (-3)

**Location:** phase2-memory.md Section 6 (Deduplication Algorithm)

**Problem:** Phase 2 defines a `contentHash` function that normalizes (lowercases, trims, collapses whitespace) and SHA-256 hashes content. The existing codebase has a `hashFile` function in `indexer.go` that directly SHA-256 hashes file bytes without normalization. Both are in the `heimdall` package (post-rename).

The names are different (`contentHash` vs `hashFile`) so there's no compile error, but the semantic similarity could cause confusion. Additionally, `contentHash` uses `hex.EncodeToString` (same as `hashFile`), and both are unexported.

**Impact:** Minor confusion. Two hash functions with slightly different semantics in the same package.

**Fix:** Name the memory content hash more distinctively, e.g., `normalizedContentHash` or `memoryContentHash`. Document why normalization is needed for memories but not for file content hashing.

---

### LOW-01: Comment at `tools.go:37` also references `.viking_db` (-1)

**Location:** phase1-rename.md Section 2.5

**Problem:** The plan lists line numbers for `.viking_db` in `tools.go` as lines 56, 188, 293 but the comment at line 37 (`// 3. Fallback: cwd/.viking_db/`) is not explicitly listed. Task 5 (comments) might catch this, but it's not explicitly called out.

**Impact:** Possible stale comment reference. The grep audit (Task 8) would catch this.

**Fix:** Add line 37 to the change manifest for `tools.go`, or explicitly note that Task 5 covers all comments.

---

### LOW-02: Phase 3 `SearchFiltered` needs `sort` import (-1)

**Location:** phase3-context.md Section B3

**Problem:** The `SearchFiltered` method uses `sort.Slice()` but `store.go` already imports `"sort"`. Not a problem in itself, but the code sample for `SearchFiltered` also uses `json.Unmarshal` in the `matchesMetadata` helper — and `store.go` does NOT currently import `"encoding/json"`.

**Impact:** Won't compile without adding `"encoding/json"` to store.go imports.

**Fix:** Add `"encoding/json"` to the import list for `store.go` when implementing Phase 3. Note this in the plan.

---

## Completeness Check: Design Spec Coverage

| Design Spec Feature | Plan Coverage | Notes |
|---|---|---|
| Rename: module, binary, dirs, tool names | Phase 1: Complete | All mappings verified against actual code |
| Rename: config + registry migration | Phase 1: Complete | Both auto-migrate on startup |
| Rename: DB dir migration | Phase 1: Complete | MigrateDBDir + registry path update |
| Session Memory: heimdall_remember | Phase 2: Complete | Schema, types, handler, tests |
| Session Memory: heimdall_recall | Phase 2: Complete | Schema, types, handler, tests |
| Session Memory: heimdall_ingest_session | Phase 2: Complete | Chunking, tagging, dedup, tests |
| Session Memory: dedup (hash + semantic) | Phase 2: Complete | Two-tier with priority rules |
| Retrieval Debugging: enriched search | Phase 3: Complete | score, source, chunkId, embeddingModel |
| Retrieval Debugging: heimdall_explain | Phase 3: Complete | Full diagnostic output |
| Typed External Context: type/metadata/relationships | Phase 3: Complete | Schema, validation, record creation |
| Typed External Context: filtered search | Phase 3: Complete | source_type + metadata_filter |
| Typed External Context: relationship traversal | Phase 3: Complete | In heimdall_explain results |
| Design Spec: 9 tools total | Plans: Verified | All 9 tools accounted for |

---

## Dependency Accuracy Check

| Dependency Claim | Verified? | Notes |
|---|---|---|
| Phase 1 must complete before Phase 2/3 | Correct | Import paths won't resolve otherwise |
| Phase 2 + Phase 3 can run in parallel | Correct | Touch different tables and different files (except server.go, types.go, store.go — all append-only) |
| P2-T4 parallel with P2-T2 | Correct | types.go input structs don't depend on memory.go types |
| P2-T8 parallel with P2-T3 | Correct | Tests for store ops can be written once store ops exist |
| P3-T5 parallel with P3-T1 | Correct | resolveDBDir extraction doesn't touch schema |
| Cross-phase X tasks after both P2+P3 | Correct | X-T1 needs memories table + explain tool |

---

## Edge Cases Reviewed

| Edge Case | Coverage | Notes |
|---|---|---|
| Empty DB (first run) | Partial | `CREATE TABLE IF NOT EXISTS` handles it. But CRITICAL-01 affects memory DB path. |
| Ollama offline | Covered | All tools check `client.Ping()` and return clean errors. |
| Corrupted DB | Not covered | SQLite handles most corruption internally. Plan doesn't address what happens if vectors.db is corrupted beyond repair. Acceptable for v1. |
| First run with no config dir | CRITICAL-01 | MemoryDBPath fails. |
| Both `.viking_db` and `.heimdall_db` exist | Covered | Migration skips if new dir exists. |
| Memory dedup with empty store | Covered | FindSimilarMemory returns nil. GetMemoryByHash returns nil. |
| Large session summary (>100KB) | Partially covered | chunkSummary splits on sentences with 500-char max. Very long summaries work but could produce hundreds of chunks, each needing an embedding call. No limit on chunk count. |
| Concurrent access to memory store | Covered | Uses existing sync.RWMutex pattern. |

---

## Summary of Required Fixes

1. **CRITICAL-01**: Fix `MemoryDBDir()` to not depend on `resolveConfigPath()` return value. Must independently resolve XDG config directory.
2. **HIGH-01**: Add `.viking_db` at cli.go line 58 to the rename manifest.
3. **HIGH-02**: Correct server.go qualifier count from 8 to 5.
4. **HIGH-03**: Correct tools.go qualifier count from 12 to 11.
5. **MEDIUM-01**: Fix Phase 3 code samples to use `heimdall.` not `openviking.`.
6. **MEDIUM-02**: Address Search/SearchFiltered code duplication.
7. **MEDIUM-03**: Document that OpenStore creates both tables in all databases.
8. **MEDIUM-04**: Rename memory `contentHash` to avoid confusion with `hashFile`.
9. **LOW-01**: Add tools.go line 37 comment to rename manifest.
10. **LOW-02**: Note that store.go needs `"encoding/json"` import for Phase 3.
