# Code Review: Phase 3 — Retrieval Debugging + Typed External Context

**Reviewer:** Opus 4.6 deep-code-review (5 independent passes)
**Date:** 2026-04-11
**Scope:** store.go, retriever.go, indexer.go, types.go, server.go, tools.go + test files
**All tests:** PASS (verified via `go test ./internal/heimdall/... ./internal/mcp/...`)

---

## Overall Score: 96/100

| Category | Deductions | Details |
|----------|-----------|---------|
| Security | -0 | All SEC items addressed |
| Correctness | -1 (LOW) | Minor: `content_hash` not scanned in SearchFiltered |
| Performance | -1 (LOW) | explain tool full-table scan — acceptable for expected dataset sizes |
| Test Coverage | -1 (LOW) | toolExplain/toolSearch not end-to-end tested (Ollama dependency) |
| Design | -1 (LOW) | Score distribution buckets are mutually exclusive but names are overlapping |
| Migration | -0 | Idempotent, correct defaults |

**VERDICT: PASS (96/100)**

---

## 1. Security Review

### SEC-1: Source Field Validation — PASS
- `validateSource()` at `server.go:624-638` checks:
  - Length limit (500 chars)
  - Path traversal (`..` detection)
  - Null bytes and newlines
  - Character allowlist via `sourceFieldPattern` regex
- Test coverage in `tools_test.go:62-88` — tests path traversal, null bytes, newlines, length, special chars, backticks.
- **No findings.**

### SEC-2: Input Limits — PASS
- Content limit: 100KB at `server.go:361-363`
- Metadata limit: 10KB at `server.go:375`
- Relationships limit: 10KB at `server.go:388` + max 50 items at `server.go:399`
- Source length: 500 chars at `server.go:625`
- **No findings.**

### SEC-9: Metadata Type Comparison — PASS
- `matchesMetadata()` at `store.go:165-185` uses `json.Marshal` for both sides, comparing serialized bytes.
- This correctly prevents type coercion attacks (string "1" != number 1, string "true" != bool true).
- Test coverage in `store_test.go:192-217` including type-safety cases.
- **No findings.**

### SEC-10: Null Byte Sanitization — PASS
- `store.go:209` strips null bytes: `strings.ReplaceAll(r.Content, "\x00", "")`
- Test at `store_test.go:264-287` verifies null bytes are stripped.
- **No findings.**

### SEC-EXTRA: SnippetBySource Path Traversal — PASS
- `SnippetBySource()` at `store.go:293-303` uses parameterized SQL query (`WHERE file_path = ?`).
- No path concatenation or file system access — purely database lookup.
- The `source` field is validated before it reaches this point (via `validateSource`).
- **No findings.**

### SEC-EXTRA: SQL Injection — PASS
- All SQL queries use parameterized statements (`?` placeholders).
- `SearchFiltered` at `store.go:119` uses `args` slice, not string concatenation.
- **No findings.**

---

## 2. Correctness Review

### SearchFiltered SQL — PASS
- `store.go:112-161`: Correctly builds WHERE clause only when `sourceType != ""`.
- Uses `sql.NullString` for nullable columns — handles NULL correctly.
- Post-filter for metadata applied after SQL pre-filter — correct ordering.
- TopK applied after sort — correct.

### matchesMetadata Logic — PASS
- `store.go:165-185`: Correct AND semantics — all filter keys must match.
- Uses `json.Marshal` + `bytes.Equal` for type-safe comparison.
- Handles edge cases: empty metadata, `{}`, invalid JSON all return false.

### Score Distribution Math — CORRECT but naming is confusing (LOW)
- `server.go:688-697`: Buckets are `>=0.90`, `>=0.70`, `>=0.50`, `<0.50`.
- The field names `Above90`, `Above70`, `Above50` suggest "above X" but they're actually mutually exclusive ranges: `[0.90,1.0]`, `[0.70,0.90)`, `[0.50,0.70)`, `[0,0.50)`.
- This is functional but the names `Above70` could be misread as "everything above 0.70" (which would include Above90).
- **Finding: LOW** — Field naming is slightly misleading. Not a bug, but a consumer might misinterpret.

### Relationship Traversal — PASS
- `server.go:759-782`: Correctly parses relationships JSON, looks up snippets by target.
- Handles empty/`[]` relationships gracefully (returns nil).
- Test at `integration_test.go:205-238` covers the happy path.

### Enriched Results — PASS
- `tools.go:66-77`: Correctly maps ContextBlock fields to SearchResultEnriched.
- `classifySource()` at `server.go:591-600` properly maps Kind to source category.
- ChunkID propagated from `VectorRecord.ID` through `ContextBlock.ChunkID` to `SearchResultEnriched.ChunkID`.

### content_hash Not Read Back in SearchFiltered — LOW
- `store.go:116` SELECT does not include `content_hash` column.
- `VectorRecord` struct has `ContentHash` field but it's never populated from search results.
- This is not a bug since `ContentHash` is only used in the indexer's `isUpToDate` check (via `ContentHashForFile`), not in search results.
- **Finding: LOW** — `ContentHash` on `VectorRecord` is effectively dead data in search results. Not a correctness issue.

---

## 3. DB Migration Review

### ALTER TABLE Idempotency — PASS
- `store.go:86-93`: Uses `db.Exec` for ALTER TABLE, ignoring errors (which is correct — SQLite returns an error if column already exists).
- The error is intentionally swallowed. This is the standard pattern for SQLite migrations without a migration framework.
- `CREATE INDEX IF NOT EXISTS` at line 93 is properly idempotent.

### Default Values — PASS
- `source_type DEFAULT 'code'` — correct for existing code entries.
- `metadata DEFAULT '{}'` — correct empty JSON object.
- `relationships DEFAULT '[]'` — correct empty JSON array.
- Test at `store_test.go:319-367` verifies re-open migration is idempotent and old records retain correct defaults.

### Upsert Default Handling — PASS
- `store.go:211-221`: Explicitly sets defaults when fields are empty.
- This ensures new records created without typed context fields get proper defaults.

---

## 4. Performance Review

### Explain Tool Full Scan — ACCEPTABLE (LOW)
- `server.go:680`: `store.Search(queryVec, 0)` retrieves ALL records for score distribution.
- This is O(N) where N = total chunks. For the stated MaxFiles=5000 and typical chunking, this could be ~50K records.
- At 50K records, in-memory cosine similarity is fast (milliseconds).
- The search is timed and reported (`searchMs`), which is diagnostic-appropriate.
- **Finding: LOW** — Full scan is acceptable for diagnostic tool. Would need optimization if dataset grows beyond ~500K records.

### SearchFiltered Indexing — PASS
- `store.go:93`: `CREATE INDEX IF NOT EXISTS idx_source_type ON entries(source_type)` — correct.
- `store.go:79`: `idx_file_path` already existed for file_path lookups.
- Combined source_type + metadata filter: source_type is pre-filtered via SQL (uses index), metadata is post-filtered in Go.
- This is the correct approach — metadata is JSON and can't be efficiently indexed in SQLite without FTS.

### SnippetBySource for Relationship Traversal — PASS
- Each relationship does a separate DB query (`store.go:298`), but relationships are capped at 50 per record.
- These are simple indexed lookups. Acceptable.

---

## 5. Test Coverage Review

### store_test.go — GOOD
- `TestOpenStore_MigrationCreatesColumns` — migration + field roundtrip
- `TestOpenStore_DefaultValues` — default value handling
- `TestSearchFiltered_BySourceType` — source type pre-filter
- `TestSearchFiltered_ByMetadata` — metadata post-filter
- `TestSearchFiltered_Combined` — both filters together
- `TestMatchesMetadata` — 9 cases including type safety
- `TestSnippetBySource` — short, long (truncation), non-existent
- `TestUpsert_NullByteSanitization` — SEC-10
- `TestSearchFiltered_TopK` — boundary (topK=2, topK=0)
- `TestOpenStore_ExistingDB_Migration` — idempotent re-open
- **Coverage assessment: GOOD.** Key paths covered.

### tools_test.go — GOOD
- `TestClassifySource` — all Kind variants
- `TestValidateSourceType` — all valid types + invalid + case sensitivity
- `TestValidateSource` — 11 cases covering security vectors
- `TestResolveDBDir` / `TestResolveDBDirForRead` — registry fallback behavior
- **Coverage assessment: GOOD.**

### integration_test.go — GOOD
- `TestToolSearch_EnrichedResults` — enriched field availability
- `TestToolSearch_FilteredBySourceType` / `ByMetadata` — filter integration
- `TestToolExplain_ScoreDistribution` / `SourceCounts` — distribution logic
- `TestToolExplain_RelationshipTraversal` — end-to-end relationship resolution
- `TestToolIndexText_TypedContent` / `ValidationErrors` — input validation
- `TestResolveRelationships_EmptyRelationships` — edge case
- `TestSearchResultEnriched_JSON` / `ExplainResult_JSON` — serialization contracts
- `TestDBDir_WrittenOnFirstUse` — directory creation

### Missing Coverage — LOW
- No end-to-end test of `toolExplain()` or `toolSearch()` through the JSON-RPC dispatch (requires Ollama).
- No test for `toolSearchFiltered()` through the MCP server path (same reason).
- No negative test for `toolExplain` with empty index.
- No test for `chunkText` with very large input hitting paragraph boundaries.
- **Finding: LOW** — Integration with Ollama can't be unit tested. The component-level tests adequately cover the logic.

---

## 6. Design Review

### Type System — CLEAN
- `types.go` is well-structured. All new types (`SearchResultEnriched`, `ExplainResult`, etc.) use proper JSON tags.
- Separation between input types (`explainInput`) and output types (`ExplainResult`) is correct.
- `ScoreDistribution` and `SourceCounts` are value types — appropriate.

### Dual DB Resolution Pattern — CLEAN
- `resolveDBDir` (write) vs `resolveDBDirForRead` (read-only) at `server.go:567-588`.
- Write path always returns a path (falls back to CWD). Read path returns empty string if not found.
- This prevents accidental directory creation on read operations.

### classifySource — CLEAN
- Simple switch at `server.go:591-600`. Maps VectorRecord.Kind to a source category.
- The three-way classification (code/external/memory) is consistent across search and explain tools.

---

## 7. Summary of Findings

| ID | Severity | File | Line | Finding |
|----|----------|------|------|---------|
| CR-1 | LOW | server.go | 688-697 | ScoreDistribution field names suggest cumulative but are mutually exclusive |
| CR-2 | LOW | store.go | 116 | content_hash not selected in SearchFiltered — dead field in results |
| CR-3 | LOW | server.go | 680 | explain full scan is O(N) — acceptable for expected dataset sizes |
| CR-4 | LOW | integration_test.go | — | No end-to-end test of toolExplain through RPC (Ollama dep) |

No CRITICAL or HIGH findings. All LOW findings are acceptable.

---

## 8. Conclusion

Phase 3 implementation is **solid**. Security controls (SEC-1, SEC-2, SEC-9, SEC-10) are all properly implemented and tested. The migration is idempotent with correct defaults. The SearchFiltered implementation correctly combines SQL pre-filtering with Go post-filtering. The explain tool provides useful diagnostics. Test coverage is good for unit/component level, with the expected gap for integration tests requiring a running Ollama instance.

**Score: 96/100 -- PASS**
