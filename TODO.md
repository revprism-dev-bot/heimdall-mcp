# heimdall-mcp — TODO

Tracking all outstanding work across the "steal ideas from OpenViking" roadmap.
Tackled progressively — see status per item.

## 1. Claude Code Hooks Integration (**WAVE 2 PHASE 2 SHIPPED** — 5 hooks live, next: dogfood + phase 3 guardrails)

**Goal:** make Claude actually use heimdall on every turn via Claude Code hooks,
not via hopeful tool exposure. Highest-leverage item by a wide margin.

**Wave 1 shipped** (merges `cdf890b`, `4a3c643`, `55ab7c3`, `8189180`):
- [x] T1 `heimdall-mcp recall` CLI + shared-core extraction
- [x] T2 `heimdall-mcp ingest-session` CLI + length-prefixed buffer parser
- [x] T3 `status --format=json`
- [x] T9 `VerifyHookIndex` + three sentinel errors, refuses fuzzy fallback
- [x] T10 `hook_cache` SQLite table + `store_metadata.index_version` bump + two-phase RLock/Lock for `HookCacheGet`
- [x] T11 Ollama `keep_alive: "10m"` on hook path only
- [x] T12 Tier B suppression store
- [x] T13 `hooks.log` file, rotation, redaction (POSIX + Windows)
- [x] T17 `heimdall-mcp hooks tail` with filter flags
- [x] T18 `heimdall-mcp hooks cache-clear` / `cache-stats` wired to real store methods
- [x] T22 `HooksDisabled` fast-path (env + marker file, <1µs warm)
- [x] Review gate: weighted 94.5/100, merged by judgment at structural ceiling

**Wave 2 phase 1a shipped** (merges `c7f7f6c`, `12cc751`, `fbcb4dc`):
- [x] T5 `heimdall-mcp hook session-start` — retrieval, VerifyHookIndex gate, Tier B suppression, EmbedForHook keep_alive, budget-ms context timeout, golden-file tested
- [x] T7 `heimdall-mcp hook post-edit` — flock debouncer, 2-s coalesce window, fork+setsid detached actor, deadletter JSONL with retry + N=10 hard-drop
- [x] T14 `heimdall-mcp install-hooks` — settings.json merge with atomic write, backup, `--dry-run`/`--merge`/`--force`/`--only`/`--scope`, dual marker detection (`"source": "heimdall"` + `--source=heimdall` command-string fallback)
- [x] T15 `heimdall-mcp uninstall-hooks` — symmetric, idempotent
- [x] T16 `heimdall-mcp hooks doctor` — 11-check pass/fail table, dry-fires each installed hook, skips internal `post-edit-actor`
- [x] OQ-1..OQ-5 locked in `docs/plans/hooks/06-decisions.md`
- [x] Review gate: weighted 97.8/100 (Quality 98, Security 99, Performance 97, Tests 96, Design 98). 110 tests in internal/cli, all green under `-race`.

**Dogfood ✅ 2026-04-16 (PR #9):**
- [x] `heimdall-mcp index .` — 1758 chunks, `nomic-embed-text`
- [x] `install-hooks --scope=project`
- [x] `hooks doctor` — 11/11 green, all three hooks dry-fire OK
- [x] Reopen Claude Code → `SessionStart` fires, `## Heimdall context` block in first turn
- [x] `Edit` tool call → `PostToolUse` → `fork+setsid` actor reached `files=1 msg=reindex_ok` (no deadletter, ~67 s first embed, Ollama cold)

**Wave 2 phase 1b shipped** (PR #10, merge `320c4bd`, impl `648a388`):
- [x] T4 `--format=hook-md` on `search` + `--budget-ms` — breaking `SearchFiltered` signature (ctx first arg), 30+ call sites updated, real ctx cancellation in the row loop
- [x] T4 side-effect: killed the `TestSearchFiltered_BudgetTimeout` skip stub, replaced with real pre-cancelled-ctx assertions + nil-ctx tolerance
- [x] T6 `hook user-prompt` command — 250 ms default budget, skip heuristic (`len < 8`), cache lookup keyed on `(normalized_prompt, index_version, project)`, Tier B suppression on Ollama down / model mismatch, cache store on miss
- [x] `install-hooks` template now installs `UserPromptSubmit` alongside `SessionStart` + `PostToolUse` (3 hooks total)
- [x] Unit tests: 16 new cases in `hook_user_prompt_test.go` — skip, disabled, Ollama down (Tier B + suppressed), no index, model mismatch, happy path caches result, cache hit short-circuits, cache invalidation on index_version bump, budget timeout, normalization, empty stdin → CWD fallback, `--prompt` flag override, malformed JSON, dispatcher routing
- [x] Version constants bumped to `wave2-phase1b` (doctor `--version` check + install envelope)
- [~] **T21 benchmark skipped for now** — original plan called for a microbenchmark; replaced with a same-task with-vs-without-hooks comparison after merge (per user direction)
- [~] **Soak gate killed** — wall-clock soak tests nothing for a solo local tool; phase 1b rides on the unit test suite + dogfood instead

**Wave 2 phase 1b follow-ups (post-merge):**
- [ ] Live dogfood `UserPromptSubmit` — watch `hooks tail --event=user-prompt` for cache-hit/miss ratio, Tier B paths, any stage=err lines
- [ ] With-vs-without comparison (T21 replacement): pick a real task, run twice — `HEIMDALL_HOOKS=0` control vs defaults — compare quality, token spend, tool-call count

**Wave 2 phase 2 ✅ shipped (session learning):**
- [x] T8 `hook stop` command — rolling buffer appends `last_assistant_message` per turn, keyed by `session_id`, capped at 2 MB per buffer file
- [x] T8b `hook session-end` command — triggers `ingest-session` from `transcript_path` JSONL, cleans up rolling buffer
- [x] Stop-event payload shape confirmed: `session_id`, `transcript_path`, `last_assistant_message`, `cwd`, `stop_hook_active`
- [x] SessionEnd-event payload confirmed: `session_id`, `transcript_path`, `cwd`, `reason` (clear/resume/logout/prompt_input_exit/other)
- [x] Session buffer stored at `<project>/.heimdall_db/hooks/sessions/<session_id>.jsonl`
- [x] Install template updated to 5 hooks: SessionStart + PostToolUse + UserPromptSubmit + Stop + SessionEnd
- [x] 12 unit tests in `hook_stop_test.go` — buffer append, multi-append, cleanup, empty/malformed, disabled, transcript summary extraction

**Wave 2 phase 3 (pending, guardrails):**
- [ ] `PreToolUse(Bash(rm *|git push --force*))` as `agent`-type hook — needs a destructive-op judgment primitive heimdall does not have today
- [ ] Phase 3 guardrails — design doc: docs/plans/hooks/08-destructive-op-primitive.md

**Carried over / not yet in scope:**
- [x] T20 Layer 2 integration tests (`os/exec` + fake Ollama) — `internal/cli/integration_test.go`, build-tag `integration`, 4 tests (session-start, user-prompt cache-hit, post-edit actor, stop→session-end). Run via `make test-integration`.
- [x] T23 Layer 3 opt-in e2e harness (`-tags e2e`, `HEIMDALL_E2E_CLAUDE=1`) — `internal/cli/e2e_test.go`, cleanly skips without env var or `claude` binary. Run via `make test-e2e`.
- [x] T24 Windows path redaction — verified correct, 12 test cases in `hooklog_test.go` cover all required patterns
- [x] PERF-002 `bumpIndexVersionTx` — single atomic `INSERT ... ON CONFLICT DO UPDATE` SQL statement
- [x] PERF-003 hook_cache eviction — incremental row-count tracking via lazy-init counter, no more O(n) COUNT per insert
- [x] First-run hint after `index` — prints "Tip: run `install-hooks`..." if hooks not detected, 6 tests in `install_test.go`

## 2. Tiered retrieval (L0/L1/L2) — **SHIPPED**

**Goal:** return one-line summaries first, expand to snippet or full chunk on demand. Biggest token/quality win once hooks are live.

- [x] Schema: `summary TEXT` column added to `entries` with migration
- [x] Index-time summary generation: heuristic `GenerateSummary()` — uses identifier+kind for named chunks, first non-comment line for paragraphs
- [x] `heimdall_search` gains `detail: summary|snippet|full` param — summary returns one-line, snippet 200 chars, full (default) unchanged
- [x] New `heimdall_expand(chunk_id)` MCP tool — returns full content for drill-down after summary search
- [x] `SearchResultEnriched` includes `summary` and `contextPath` fields
- [ ] Token-savings measurement before/after (follow-up)

## 3. Path-based context hierarchy — **SHIPPED**

**Goal:** replace flat `sub_project` with a real tree, so retrieval can scope by path prefix and walk the hierarchy.

- [x] Schema: `context_path TEXT` column added to `entries` with indexed migration
- [x] Auto-derived from file path via `deriveContextPath()` (directory hierarchy, slash-normalized)
- [x] `heimdall_search` gains `scope` path param — prefix filter on `context_path`
- [x] New `heimdall_ls(path)` MCP tool — lists child paths with chunk counts for filesystem-style navigation
- [x] `WithScope()` and `WithDetail()` functional options on `SearchFiltered`
- [ ] Auto-detect path for memories (follow-up)
- [ ] Update hook injection to respect scope when CWD is a subpath (follow-up)

## 4. Skills as indexable content — **SHIPPED**

**Goal:** store reusable procedures as first-class, semantically retrievable content.

- [x] Decision: Claude Code skills are static always-loaded instructions; Heimdall skills are semantically searchable, auto-surfaced by relevance. Complementary, not competing.
- [x] New `MemoryTypeSkill` constant + validation
- [x] `heimdall_remember --type=skill` accepted (validated in MCP tool schema + memory type map)
- [ ] Auto-surface via `SessionStart` (top-N skills for repo) (follow-up)
- [ ] Optional: two-way sync with `~/.claude/skills/` directory (follow-up)

## 5. Misc / carried over — **SHIPPED**

- [x] SEC-001 SQL-builder audit comment — SAFETY comments added to all dynamic SQL builders (SearchFiltered, SearchMemories, UpdateLastAccessed)
- [x] SEC-002 subProject length cap — 255-char validation in toolSearch
- [x] SEC-003 sanitized error strings — `sanitizeStoreError()` helper logs full error server-side, returns generic message to client
- [x] DES-006 config-driven EmbedBatchSize — `EmbedBatchSize` field added to Config with default 32
- [x] DES-010 ETA unit test — extracted `computeETA()` pure function + 6 tests in `eta_test.go`
- [x] TEST-013 mock rename — `MockEmbedder` → `StubEmbedder` across all source files (12+ files)
