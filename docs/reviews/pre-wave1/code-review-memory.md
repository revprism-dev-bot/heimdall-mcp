# Code Review: Session Memory Implementation (Phase 2)

**Date:** 2026-04-11
**Reviewer:** Code Review Agent (5 independent review passes)
**Scope:** All Phase 2 files -- memory types, store, ingestion, MCP tools, validation, config, main wiring
**All tests pass:** Yes (go vet clean, go build clean, 71 tests PASS)

---

## Summary

The session memory implementation is well-structured, follows the project's existing patterns, and properly separates MemoryStore from VectorStore (ARCH-1). The code handles input validation, dedup at two tiers (hash + semantic), and has reasonable test coverage. A few findings remain below.

---

## Findings

### CRITICAL (0 found)

None.

### HIGH (-8 each)

**H-1: Duplicate `mergeTags` function across packages**
- Files: `internal/heimdall/memory_ingest.go:285` and `internal/mcp/memory_tools.go:254`
- Two independent implementations of `mergeTags` exist. The `memory_ingest.go` version uses `uniqueStrings` then truncates; the `memory_tools.go` version uses inline dedup with an early cap. They have subtly different semantics: the heimdall version keeps all unique strings then truncates (order-dependent truncation), while the mcp version stops adding new tags once the cap is reached (preserves existing tags over new). Both cap at 20 but the behavior differs. This risks confusion and silent divergence.
- **Recommendation:** Export one canonical implementation (e.g., `heimdall.MergeTags`) and use it in both places. Choose the mcp version's semantics (prefer existing tags) since that is the safer behavior.

**H-2: `SearchMemories` performs a full table scan for every search**
- File: `internal/heimdall/memory_store.go:91`
- The `SearchMemories` method loads ALL rows from the `memories` table, decodes every vector blob, and computes cosine similarity in Go. While acceptable at small scale (same as VectorStore), the `WHERE` clause filters (type, project, source, tags) could be pushed to SQL to reduce the amount of data decoded and processed. For 10K+ memories, this becomes a performance bottleneck.
- **Recommendation:** Add SQL WHERE clauses for type, project, and source filters before the scan. Tags still require Go-side filtering since they're stored as JSON, but the other three are indexed columns. This is a performance improvement, not a correctness bug.

### MEDIUM (-3 each)

**M-1: Ignored `json.Unmarshal` error when deserializing tags from DB**
- Files: `internal/heimdall/memory_store.go:205`, `:234`, `:346`
- `json.Unmarshal([]byte(tagsJSON), &m.Tags)` return value is silently discarded. If the stored JSON is corrupted, `m.Tags` will be nil and no error is reported. This creates a silent data corruption path.
- **Recommendation:** Log the error (at debug level) so corruption is observable. The current behavior of returning nil tags is acceptable as a fallback, but it should be logged.

**M-2: `toolRemember` and `toolRecall` create a new `OllamaClient` and `OllamaEmbedder` per call**
- Files: `internal/mcp/memory_tools.go:69-70`, `:159-160`, `:229-230`
- Each tool invocation creates a fresh HTTP client and pings Ollama. This adds latency to every call. The existing `toolSearch` and `toolExplain` do the same, so this is consistent, but it is still wasteful.
- **Recommendation:** Consider creating the OllamaClient once at server startup (or lazily on first use with caching). This is an optimization, not a bug. Low priority given consistency with existing code.

**M-3: `recallInput.Limit` has no upper bound validation**
- File: `internal/mcp/memory_tools.go:153`
- The `toolRecall` handler defaults `limit` to 5 if <= 0, but has no maximum. A client could request `limit: 999999`, causing all memories to be returned. Combined with H-2 (full table scan), this means the entire memory database is serialized to JSON.
- **Recommendation:** Cap `limit` at a reasonable maximum (e.g., 100) in `toolRecall`.

**M-4: `IngestSession` does not add new memories to the vector cache for semantic dedup**
- File: `internal/heimdall/memory_ingest.go:122-124`
- Wait, this IS handled at line 122-124. The code appends new memories to `vectorCache`. However, `vectorCache` is only populated when `store.MemoryCount() <= MaxMemoriesForSemanticDedup` at line 35. If the count is exactly at the boundary (e.g., 10000 memories), and ingestion adds more, the cache was populated but subsequent chunks within the same session still benefit from it. This is correct.
- **Retracted** -- on closer inspection, this is properly handled.

**M-4 (revised): `DecodeFloat32Vec` does not validate input length**
- File: `internal/heimdall/vecmath.go:35-42`
- If `b` has a length that is not a multiple of 4 (e.g., from DB corruption), the trailing bytes are silently dropped via integer division `len(b) / 4`. While this won't panic, it silently loses data.
- **Recommendation:** Either validate `len(b) % 4 == 0` or document the truncation behavior. Since this only processes data written by `EncodeFloat32Vec`, the risk is very low.

**M-5: `splitSentences` regex loses punctuation for some sentences**
- File: `internal/heimdall/memory_ingest.go:176-199`
- The sentence splitting logic splits on `([.!?])\s+` and tries to re-attach punctuation, but the logic at lines 190-196 has a gap: the comment at line 191 says "This part already lost its leading punctuation" but doesn't do anything about it. The `if i < len(delims)` at line 195 re-attaches punctuation to the current part, but this means the punctuation is attached to the wrong sentence. For example, "Hello. World. Foo" splits into parts ["Hello", "World", "Foo"] with delims [". ", ". "]. The code attaches "." to "Hello" and "." to "World", which is correct. However, for edge cases with "Hello?! World" the regex captures only "!" and loses "?".
- **Recommendation:** This is a minor text-processing edge case. The chunking still produces reasonable results. Document that the sentence splitting is approximate.

### LOW (-1 each)

**L-1: `tagsJSON, _ := json.Marshal(m.Tags)` ignores error in `UpsertMemory`**
- File: `internal/heimdall/memory_store.go:75`
- `json.Marshal` of a `[]string` cannot realistically fail, but ignoring the error is not idiomatic Go. A nil Tags slice marshals to `null` which is valid JSON.
- **Recommendation:** Accept as-is. `json.Marshal` for `[]string` is infallible in practice.

**L-2: Memory ID format uses content hash, making IDs non-random**
- Files: `internal/mcp/memory_tools.go:110` (`mem:explicit:<hash>`), `internal/heimdall/memory_ingest.go:102` (`mem:session:<hash>`)
- Memory IDs are deterministic based on content hash. This means if content changes but hash collides (astronomically unlikely with SHA-256), two memories would get the same ID. This also means the ID reveals the content source type.
- **Recommendation:** Accept as-is. SHA-256 collision probability is negligible. The source type in the ID is useful for debugging.

**L-3: No `DeleteMemory` method exists**
- Files: `internal/heimdall/memory_store.go`
- There is no way to delete a memory. Users can only create or update. This may be intentional for Phase 2 (MVP), but should be documented.
- **Recommendation:** Add a delete tool in a future phase, or document that deletion is not supported.

**L-4: `ValidMemoryTypes` is a public map that could be mutated**
- File: `internal/heimdall/memory.go:14-19`
- `ValidMemoryTypes` is an exported mutable map. Any caller could do `heimdall.ValidMemoryTypes["foo"] = true`.
- **Recommendation:** Accept for internal use. If the package becomes a public API, convert to a function `IsValidMemoryType(t MemoryType) bool`.

---

## Security Review

| Requirement | Status | Notes |
|---|---|---|
| SEC-1: Input validation | PASS | `validate.go` enforces size limits on content (100KB), summary (50KB), query (10KB), tags (20 max, 100 chars each) |
| SEC-2: Content size limits | PASS | Enforced before processing |
| SEC-4: Memory exhaustion (caching) | PASS | `MaxMemoriesForSemanticDedup = 10000` caps vector cache; SEC-4 batch dedup loads vectors once |
| SEC-10: Null byte handling | PASS | `toolRemember` and `toolIngestSession` strip `\x00` from content/summary |
| SQL injection | PASS | All queries use parameterized `?` placeholders |
| Path traversal | N/A | Memory store path is resolved from config, not user input |
| Secret storage warning | PASS | Tool description warns against storing secrets |

---

## Architecture Review

| Criterion | Status | Notes |
|---|---|---|
| ARCH-1: MemoryStore separate from VectorStore | PASS | Completely separate struct, DB file, and schema |
| vecmath.go shared correctly | PASS | `CosineSimilarity`, `EncodeFloat32Vec`, `DecodeFloat32Vec` used by both stores |
| DB schema appropriate | PASS | Indexed columns for type, project, source, content_hash |
| Dedup logic correct | PASS | Tier 1 (hash) + Tier 2 (semantic @ 0.92); explicit wins over session |
| Tag extraction reasonable | PASS | 10 tech patterns + CamelCase extraction |
| Memory classification reasonable | PASS | Keyword-based heuristics with fact as default |
| Tool registration | PASS | 3 tools registered in `handleToolsList`, routed in `handleToolsCall` |
| Config wiring | PASS | `ResolveMemoryDBPath` resolves to `~/.config/heimdall-mcp/memories.db` |
| Main.go wiring | PASS | MemoryStore opened at startup, closed on exit, passed to Server |

---

## Test Coverage Review

| File | Tests | Coverage Assessment |
|---|---|---|
| `memory_store.go` | 11 tests | Good -- covers CRUD, search with filters, similarity, stats, count, vector loading |
| `memory_ingest.go` | 8 tests | Good -- covers chunking, tagging, classification, hashing, ingestion, dedup, explicit-wins |
| `memory_tools.go` | 11 tests | Good -- covers validation errors, nil store, mergeTags |
| `validate.go` | Tested via memory_tools_test.go | All validators exercised |

**Missing test coverage:**
1. No test for `toolRecall` returning actual results (requires mock embedder wiring at the Server level -- would need an embedder interface on Server or a test Ollama)
2. No test for `toolRemember` actually storing a memory (same limitation -- requires Ollama)
3. No test for `FindSimilarMemoryFromCache` directly (tested indirectly via `IngestSession`)
4. No test for `MemoryStats` with project grouping showing "(global)" label
5. No test for `ChunkSummary` with `maxSize <= 0` (defaults to 500)

These are not blockers but represent gaps that should be filled.

---

## Score Calculation

| Severity | Count | Points Each | Total |
|---|---|---|---|
| CRITICAL | 0 | -15 | 0 |
| HIGH | 2 | -8 | -16 |
| MEDIUM | 4 | -3 | -12 |
| LOW | 4 | -1 | -4 |
| **Base** | | | **100** |
| **Final** | | | **68** |

---

## Verdict: FAIL (68/100)

The implementation is functionally correct and well-structured, but the duplicate `mergeTags` function (H-1) and the full-table-scan search without SQL-level filtering (H-2) are the primary deductions. The medium findings (ignored unmarshal errors, no limit cap, input validation gap) add up.

### Required fixes for 95+:
1. **H-1:** Consolidate `mergeTags` into one exported function
2. **H-2:** Push type/project/source filters to SQL WHERE clauses in `SearchMemories`
3. **M-1:** Log (or handle) `json.Unmarshal` errors on tag deserialization
4. **M-3:** Cap `recallInput.Limit` to a reasonable maximum

### Recommended but not required:
- M-2: Cache OllamaClient (consistent with existing code, so not blocking)
- M-4: Validate vector blob length in `DecodeFloat32Vec`
- M-5: Document approximate sentence splitting
- L-3: Plan `DeleteMemory` for Phase 3
